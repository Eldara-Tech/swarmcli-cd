# Sync waves — design for #78

Against `swarmcli-cd` `main` @ `194c95f` and `Eldara-Tech/swarmcli` `v1.13.0-rc6`
(`4f0c266`, which is CE `main`). Every claim below was re-verified against those
two trees. The `/claude-go/swarmcli` working tree is **not** one of them — it sits
on `fix/stack-deploy-feedback`, its `main` is at `812fdf5`, and it pre-dates
`charts.DeployRequest` and the context-aware `Backend`; anything read from it
about the applier or the wait path is wrong.

## Contents

1. [Executive summary](#1-executive-summary)
2. [What is actually true today](#2-what-is-actually-true-today)
   - 2.1 [Ordering](#21-ordering)
   - 2.2 [Waiting](#22-waiting)
   - 2.3 [What the issue's sketch gets wrong](#23-what-the-issues-sketch-gets-wrong)
3. [Decisions — settled 2026-08-01](#3-decisions--settled-2026-08-01)
4. [The design](#4-the-design)
   - 4.1 [The field: `releases[].wave`](#41-the-field-releaseswave)
   - 4.2 [Ordering: `PlanApply` sorts, once](#42-ordering-planapply-sorts-once)
   - 4.3 [The barrier: `Engine.Apply` groups and waits](#43-the-barrier-engineapply-groups-and-waits)
   - 4.4 [`waitWave` subsumes `waitReady`](#44-waitwave-subsumes-waitready)
   - 4.5 [Waves and `wait` / `--wait`](#45-waves-and-wait----wait)
   - 4.6 [The converge path](#46-the-converge-path)
   - 4.7 [Reporting](#47-reporting)
5. [What this deliberately does not do](#5-what-this-deliberately-does-not-do)
6. [The changes, file by file](#6-the-changes-file-by-file)
   - 6.1 [CE — `Eldara-Tech/swarmcli`](#61-ce--eldara-techswarmcli)
   - 6.2 [CD — `Eldara-Tech/swarmcli-cd`](#62-cd--eldara-techswarmcli-cd)
7. [Tests](#7-tests)
   - 7.1 [The acceptance criterion](#71-the-acceptance-criterion)
   - 7.2 [CE unit tests](#72-ce-unit-tests)
   - 7.3 [CE integration tests](#73-ce-integration-tests)
   - 7.4 [CD unit tests](#74-cd-unit-tests)
   - 7.5 [CD integration tests](#75-cd-integration-tests)
   - 7.6 [Harness work the tests need first](#76-harness-work-the-tests-need-first)
   - 7.7 [Wall-clock budget](#77-wall-clock-budget)
8. [Sequencing and PR shape](#8-sequencing-and-pr-shape)
9. [Risks and how each is bounded](#9-risks-and-how-each-is-bounded)

---

## 1. Executive summary

A release file gains one integer key, `releases[].wave`. Releases sort by it and
apply in groups: every release in wave *N* is written, the whole wave is waited
for, then wave *N+1* starts. A wave that does not converge stops every wave after
it, so a failed migration can never let the application that depends on it start.

The whole ordering-and-barrier semantic lives in CE, in `charts`:

- `ReleaseSpec.Wave` is parsed in `charts/releasefile.go`. There is nothing to
  validate — see §4.1.
- `ReleasePlan.Wave` carries it onto the plan, because `Engine.Apply` never sees
  the `ReleaseFile` — only the `Plan`.
- `PlanApply` sorts `plan.Releases` stably by `(wave, file index)`, so plan order
  becomes execution order for every consumer at once.
- `Engine.Apply` walks contiguous wave groups and calls a new `waitWave` between
  them.

`swarmcli-cd`'s apply path therefore needs **no change at all** — `Engine.Apply`'s
signature is untouched, and the `Engine` interface in `reconcile.go:118` still
matches. What CD adds is the same barrier on the one path that cannot go through
the engine: `converge`, which writes through `Backend.DeployStack` because a
live-drifted release is `ActionUnchanged` by construction. Plus the pin bump, the
`wave` on the status contract, and the tests.

Three properties make this safe to ship:

- **A single-wave plan is byte-for-byte today's behaviour.** No wave declared
  means every release is in wave 0, one group, no barrier, no extra swarm read.
  Every release file that exists today is a single-wave plan.
- **A barrier waits only for what its wave actually deployed.** A release the plan
  called unchanged is not waited for. The alternative lets one unrelated,
  already-deployed, degraded release block every later wave on every reconcile for
  ever — the trap `convergeable` and `maxPruneAttempts` were both written to
  avoid.
- **The barrier is unconditional, so `wait: false` + waves is not a
  contradiction** — it is the recommended pairing, and the only one that delivers
  "db, then migrate, then the three *together*". `wait` keeps its exact current
  meaning: per-release blocking, inside a wave.

The gate is the real-swarm integration job, per the #54, #62 and #69
retrospectives: a fixture whose second wave never converges, asserting the third
was never deployed — proved by the absence of a release record, not only by the
absence of services.

---

## 2. What is actually true today

### 2.1 Ordering

`Engine.Apply` iterates `plan.Releases` and stops at the first failure
(`charts/apply.go:307-331` @ rc6). `PlanApply` appends in file order
(`charts/apply.go:159`), and the field says so: `// Releases, in file order.`
(`charts/apply.go:69`). `Apply`'s own doc repeats it: `// Apply converges the
swarm to a plan, in file order.` (`charts/apply.go:283`).

`docs/configuration.md:410-411` is the promise built on that:

> Releases are applied in the order the release file lists them; use `wait: true`
> if a later release needs an earlier one live first.

and `docs/configuration.md:463-465` extends it to drift correction. **Neither is
tested anywhere** — there is no ordering assertion in any of the 37 integration
tests, and no helper exposes apply order.

`charts.ReleaseSpec` is `{Name, Chart, Version, Values}`
(`charts/releasefile.go:60-65`) and `ParseReleaseFile` sets `dec.KnownFields(true)`
(`charts/releasefile.go:83`), so a `wave:` key is rejected outright today.

### 2.2 Waiting

`InstallOptions.Wait` (`charts/release.go:152`) is consumed in `deployAndRecord`
(`charts/release.go:386`), which calls `waitReady(ctx, rel.Name, opts.Timeout)`
— **after** the revision is recorded, per release. `waitReady`
(`charts/release.go:1107`) is a twelve-line loop around the exported
`charts.Rollup`, is context-aware at rc6, returns immediately on
`PhaseWedged` rather than waiting out the deadline, and defaults a zero timeout to
5 minutes.

On the CD side `Reconciler.awaitConverged` (`reconcile/reconcile.go:2184`) is the
same loop, written independently in #77 for the converge path — deliberately, and
its docstring says why: the judgement is CE's own exported rollup rather than a
second copy of the rules, and `waitReady` is unexported. It early-returns unless
`spec.SyncPolicy.Wait`, polls every `convergePollInterval` (2s,
`reconcile.go:2135`), defaults to `defaultConvergeTimeout` (5m,
`reconcile.go:2139`), and scopes the rollup to the services the correction
actually wrote (`scopedTo`, `reconcile.go:2222`) — the #130 lesson.

### 2.3 What the issue's sketch gets wrong

#78 says:

> a wave is one `Apply` call per group over a sub-plan …
> No engine change, no CE change

Both halves fail on inspection.

**`Engine.Apply` never sees the `ReleaseFile`.** Its signature is
`Apply(ctx, plan *Plan, opts InstallOptions)`. A wave declared on `ReleaseSpec`
must be plumbed onto `ReleasePlan` before any caller — CD or CE — can group by it.
That is a CE change whoever does the grouping.

**A `wave:` CE parses and ignores is a lie in CE.** CE is the thing that parses
the release file. If the key is accepted there, `swarmcli charts apply` must
honour it, or an operator who declares an ordering and runs `charts apply` gets
silence. That is also the whole of the "independently useful to `swarmcli charts`
itself" argument #1 uses for Phase 0 — CE has exactly the same coarse ordering and
benefits identically.

One thing the issue's mechanism section gets right and is worth keeping: nothing
here needs `Plan.Owner`, `Orphaned` or `Unmanaged`, and the waiting machinery
already exists. It just lives one repo further down than the sketch assumed.

---

## 3. Decisions — settled 2026-08-01

| # | Decision |
|---|---|
| **W1** | **The wave is declared in CE's release file**, as `releases[].wave: <int>`. Ordering is a property of the workload, so it lives beside the releases it orders, in the application's own repository, changeable by the team that owns the chart. Not `applications.yaml`: that puts it in a different file under a different owner, and is dead for `source.chart`, which has no release list. Not the chart: a chart cannot know about its siblings. |
| **W2** | **CE owns grouping and the barrier.** `PlanApply` sorts; `Engine.Apply` groups and waits. `swarmcli-cd`'s apply path is unchanged; CD adds the barrier only on the converge path, which cannot go through the engine. |
| **W3** | **The barrier is unconditional.** A wave boundary always waits, whatever `wait` says. `wait` keeps its current meaning — per-release blocking inside a wave. Neither combination is refused, and no cross-file, cross-repo config error is introduced. |
| **W4** | **A barrier waits only for the releases its wave deployed.** A release the plan called unchanged is skipped by `Apply` and is not waited for. |
| **W5** | **Default wave 0, ascending, stable within a wave.** Negative waves are legal. An undeclared release is wave 0, so a file that declares nothing is one wave and behaves exactly as it does today. |
| **W6** | **Converge respects waves**, with the same barrier. **Prune does not** — see §5. Concurrency within a wave is out, and is not designed out. |
| **W7** | **The wave is surfaced** on `application.ReleaseStatus`, in the API payload, and as a `WAVE` column in `app get` and `charts apply`'s plan table — rendered only when the plan spans more than one wave. |

W1–W4 and W6–W7 were confirmed by the maintainer before any code was written. W5
follows Argo CD, which is the reflex an operator arrives with.

---

## 4. The design

### 4.1 The field: `releases[].wave`

```yaml
apiVersion: v1
releases:
  - name: db
    chart: ./charts/postgres
  - name: migrate
    chart: ./charts/migrate
    wave: 1
  - name: api
    chart: ./charts/api
    wave: 2
  - name: worker
    chart: ./charts/worker
    wave: 2
  - name: web
    chart: ./charts/web
    wave: 2
```

`db` declares nothing and is wave 0. `api`, `worker` and `web` share wave 2, so
the order between them is not meaningful and they go out together.

```go
// ReleaseSpec is one desired release.
type ReleaseSpec struct {
	Name    string   `yaml:"name"`
	Chart   string   `yaml:"chart"`
	Version string   `yaml:"version,omitempty"`
	Values  []string `yaml:"values,omitempty"`
	// Wave groups releases that may be applied together. Every release in one
	// wave is written, the whole wave is waited for, and only then does the
	// next start — so a migration that does not converge stops the application
	// that depends on it from ever starting.
	//
	// Ascending, and 0 by default, which is Argo CD's sync-wave and the reading
	// that keeps a file declaring nothing behaving exactly as it did: one wave,
	// no barrier. Negative is legal and is how something is put before an
	// existing set without renumbering it. Within a wave, order is the file's.
	Wave int `yaml:"wave,omitempty"`
}
```

`omitempty` matters in one place beyond tidiness: `swarmcli-cd`'s
`source.synthesise` marshals a `charts.ReleaseFile` to YAML and re-parses it with
CE's own parser (`source/source.go:163-180`), so without it every `source.chart`
application would round-trip a meaningless `wave: 0`.

No validation beyond the YAML type. There is no invalid wave — a gap in the
numbering is not an error, a negative one is deliberate, and a file whose every
release shares a wave is the common case.

### 4.2 Ordering: `PlanApply` sorts, once

```go
// After the release loop in PlanApply:
slices.SortStableFunc(plan.Releases, func(a, b ReleasePlan) int {
	return cmp.Compare(a.Wave, b.Wave)
})
```

Sorting in `PlanApply` rather than grouping at each consumer is the choice that
carries the whole feature. Plan order becomes execution order, so:

- `Engine.Apply` needs only to notice where a contiguous run of equal waves ends.
- `cli/apply.go`'s plan table renders the order things will actually happen in.
- `swarmcli-cd`'s `converge` walks `plan.Releases` (`reconcile.go:2098`, via
  `driftedReleases` at `reconcile.go:1319`) and gets wave order for free.
- `drift.FromPlan` (`drift/drift.go:57-60`) builds `Status.Releases` in the same
  order, so the API and `app get` list releases in the order they are applied.

Two doc comments become wrong and must be corrected in the same change:
`charts/apply.go:69` (`// Releases, in file order.`) and `charts/apply.go:283`
(`// Apply converges the swarm to a plan, in file order.`). Both become "in wave
order, then file order" — which, for every file that declares no wave, *is* file
order.

Nothing in either repository depends on file order semantically. The readers are
`Counts` (order-insensitive), the compat gate (order-insensitive), the two
renderers above, and CD's `observe`/`departed`/`record` walks, which key by
release name.

### 4.3 The barrier: `Engine.Apply` groups and waits

```go
func (e *Engine) Apply(ctx context.Context, plan *Plan, opts InstallOptions) ([]ApplyResult, error) {
	var results []ApplyResult
	waves := waveGroups(plan.Releases)
	for i, wave := range waves {
		// wrote is what this wave actually deployed, which is not the wave: a
		// release the plan called unchanged is skipped, and waiting for one
		// would let an unrelated degraded release that nothing here touched
		// stop every wave after it, on every apply, for ever.
		var wrote []string
		for _, rp := range wave {
			if err := ctx.Err(); err != nil {
				return results, err
			}
			if rp.Action == ActionUnchanged {
				results = append(results, ApplyResult{Name: rp.Name, Action: ActionUnchanged})
				continue
			}
			o := opts
			o.Requirements = rp.Requirements
			o.Files = rp.Files
			o.Install = true
			rel, err := e.Upgrade(ctx, rp.Name, rp.Chart, rp.Values, rp.Manifest, o)
			if err != nil {
				return results, fmt.Errorf("release %q: %w", rp.Name, err)
			}
			results = append(results, ApplyResult{Name: rp.Name, Action: rp.Action, Revision: rel.Revision})
			wrote = append(wrote, rp.Name)
		}
		// Between waves only. The last wave is not waited for here — under
		// opts.Wait every release in it has already been waited for
		// individually, and without it the caller asked not to block.
		if i == len(waves)-1 || len(wrote) == 0 {
			continue
		}
		if err := e.waitWave(ctx, wrote, opts.Timeout); err != nil {
			return results, err
		}
	}
	return results, nil
}
```

`waveGroups` is a walk over a sorted slice returning contiguous runs of equal
`Wave`. A plan with one distinct wave produces one group, so the `i ==
len(waves)-1` guard skips the barrier entirely and the loop is the loop that is
there today.

A wave that does not converge returns from `waitWave`, `Apply` returns the results
so far alongside the error, and no release in any later wave is written — no
service, no revision record, nothing. That is the failure semantic the feature is
for, and it is the same shape as the existing first-failure behaviour, one scope
up.

Under `opts.Wait` the barrier is partly redundant: each release in the wave was
already waited for by `deployAndRecord`. It is kept rather than skipped so that "a
wave boundary always waits" is literally true, and it costs one `StackServices`
read per release of a wave that has by then converged.

### 4.4 `waitWave` subsumes `waitReady`

One polling loop, not two. `waitReady` becomes the single-release case:

```go
// waitWave polls until every named release has converged, or the timeout
// elapses. One deadline for the whole wave rather than one per release: the
// releases in a wave were deployed together and roll out concurrently, so
// waiting for them serially would charge the wave the sum of what it actually
// spends in parallel.
//
// It returns early on the first wedged release, for the reason waitReady always
// has: swarm never rolls back a rollback, so waiting out the deadline only
// delays the same answer, and the release that wedged is the one to name.
func (e *Engine) waitWave(ctx context.Context, releases []string, timeout time.Duration) error

func (e *Engine) waitReady(ctx context.Context, release string, timeout time.Duration) error {
	return e.waitWave(ctx, []string{release}, timeout)
}
```

The messages are unchanged for the single-release case, so no existing test moves:

- wedged: `release %q: %s` — names the offending release either way.
- timeout, one release: `timed out after %s waiting for release %q to converge`.
- timeout, several: `timed out after %s waiting for releases %s to converge`,
  naming only the ones that had not converged.

`timeout` is per wave, as it is per release today — not per apply. An N-wave plan
with `wait: false` can therefore block for up to `(N-1) × timeout`. That is
already true of an N-release apply with `wait: true`, and is documented rather
than changed.

### 4.5 Waves and `wait` / `--wait`

| waves | `wait` | behaviour |
|---|---|---|
| no | `false` | unchanged — exactly today |
| no | `true` | unchanged — per-release wait |
| yes | `false` | barrier between waves; the last wave is not waited for; the releases inside a wave go out together |
| yes | `true` | the above, plus each release inside a wave waits, so a wave is serialised — and the last wave is covered by its own per-release waits |

The row that matters is the third: **`wait: false` plus declared waves is the
recommended pairing**, and the only one that delivers "the three together". The
open question in #78 — whether `wait: false` plus waves is a config that
contradicts itself, to be refused — dissolves, because the barrier does not depend
on `wait`.

Refusing it was rejected for a second reason: under W1 the two fields live in
different files, in different repositories, under different owners. A chart team's
commit would fail an operator's application over a field the operator controls
elsewhere, surfacing at plan time inside the controller rather than at commit time.

### 4.6 The converge path

Drift correction cannot go through `Engine.Apply` — the engine skips a release it
planned as unchanged, and a live-drifted release is unchanged by construction
(`reconcile.go:2068-2071`). So `converge` writes through `Backend.DeployStack`
directly, and needs the barrier of its own.

It gets wave *order* free: `driftedReleases` walks `plan.Releases`
(`reconcile.go:1319`), which is now wave-sorted. What is added is the barrier:

```go
for i, wave := range waves {          // the same grouping, over plan.Releases
    var corrected []string
    for _, release := range drifted-in-this-wave {
        DeployStack(...)
        if err := r.awaitConverged(...); err != nil { … }   // unchanged: gated on spec.SyncPolicy.Wait
        corrected = append(corrected, release)
    }
    if i == len(waves)-1 || len(corrected) == 0 { continue }
    if err := r.awaitSettled(ctx, backend, correctedScopes, timeout); err != nil {
        // Later waves are not corrected. Warn naming what was skipped — an
        // operator has to know that wave 2's correction never ran because
        // wave 1 did not settle, and the status alone says only that it is
        // still drifted.
        break
    }
}
```

`awaitSettled` is the ungated extraction of `awaitConverged`'s loop, taking a
release-to-written-services map so the rollup stays scoped per release
(`scopedTo`, and the #130 reason for it). `awaitConverged` keeps its exact current
shape: the `spec.SyncPolicy.Wait` gate plus a call to `awaitSettled` with one
release. The `TestConvergeDoesNotWaitWithoutThePolicy` guard on the *absence* of
per-release waiting still holds.

The per-release wait inside a wave stays gated on `wait`, and the wave boundary is
unconditional — the same rule as the apply path, so `wait` means one thing across
both.

### 4.7 Reporting

`application.ReleaseStatus` gains ``Wave int `json:"wave,omitempty"` ``, filled by
`drift.releaseStatus` (`drift/drift.go:64-80`) from `rp.Wave`. `Status.Clone`
needs no change and the clone test needs none either — it fills by reflection
precisely so a field added later is covered (`application/clone_test.go:14-21`,
the #143 lesson).

`app get`'s release table (`controller/app.go:351-359`) and `charts apply`'s plan
table (`cli/apply.go:154-163`) gain a `WAVE` column, **rendered only when the
releases span more than one distinct wave**. That keeps every existing output
byte-identical for files that declare nothing — including
`cli/charts_test.go:361-377`, whose exact-table regexp is otherwise the one test
that breaks mechanically.

`omitempty` on the JSON means wave 0 is absent from the payload. That is correct
rather than lossy: absent and zero are the same thing here, because 0 is the
default and there is no "unset".

No new notification type. A wave that does not converge fails the sync, and
`SyncFailed` already carries the error, which names the release.

One thing an operator should be told rather than discover: **while a sync is
blocked at a wave barrier, the status is the one `record` published before the
apply** (`reconcile.go:1703`), so `app get` shows the plan rather than how far
through it the controller is. That is already true of every `wait: true`
application today and is not made worse here — it is simply now reachable with
`wait: false`, which is the pairing the docs will recommend. A per-wave progress
axis is a separate change and is not in this one.

---

## 5. What this deliberately does not do

**Prune does not reverse waves.** Argo CD deletes in reverse wave order so a
dependency outlives its dependants. That is not implementable here without new
storage: `plan.Orphaned` is a `[]string` of releases the file has *stopped*
declaring, so by definition there is no `wave:` to read for any of them. The wave
a departed release used to be in is recoverable only from a stored revision, which
means stamping the wave onto every revision record — a separate change with its
own cost in the manager's raft log. `pruneFirst` is unaffected: it orders the
sweep against the apply as a whole, which is a different sequencing question and
still works exactly as documented. The within-release resource sweep
(`pruneResources`) has no wave either — it acts inside one release.

**Releases in a wave are not deployed concurrently.** `Engine.Apply` writes them
back to back. With `wait: false` that is already effectively concurrent — each
`DeployStack` returns as soon as the daemon accepts it, and the rollouts overlap
in the swarm — which is what makes the wave barrier meaningful. True concurrency
would mean not using `Apply`'s sequential loop, and it is explicitly not designed
out: the grouping is the thing that would make it possible, and it is a local
change inside one loop when somebody wants it.

**Applications are not ordered relative to each other.** #47's app set has the same
question one level up. Out of scope, and the vocabulary here — `wave`, an integer,
default 0, ascending — is deliberately reusable there without changing.

**An unchanged release is not waited for.** See W4 and the comment in §4.3. The
consequence is honest and worth stating in the docs: a wave whose releases are all
already deployed is not a health check on them, and if wave 1's database is live
but broken, wave 2 will still start. Health is a separate axis that already reports
that, and the alternative is an application that can never sync again.

---

## 6. The changes, file by file

### 6.1 CE — `Eldara-Tech/swarmcli`

| File | Change |
|---|---|
| `charts/releasefile.go:60-65` | ``ReleaseSpec.Wave int `yaml:"wave,omitempty"` `` with the doc comment from §4.1. |
| `charts/releasefile.go:15-31` | The `ReleaseFile` doc comment names the Renovate `helmfile` manager and the exact keys it reads. Add a line: `wave` is a swarmcli extension, not a Helmfile key, and Renovate ignores it. |
| `charts/apply.go:26` | ``ReleasePlan.Wave int `json:"wave,omitempty"` ``. |
| `charts/apply.go:69` | `// Releases, in file order.` → wave order, then file order. |
| `charts/apply.go:127-172` | `PlanApply`: `slices.SortStableFunc` right after the release loop (which ends at `:160`). The orphan/unmanaged loop below it walks `current`, not `plan.Releases`, so the placement does not touch it. |
| `charts/apply.go:244-252` | `planRelease`: `Wave: spec.Wave` in the `ReleasePlan` literal. |
| `charts/apply.go:283` | `Apply`'s doc: wave order, the barrier, and what a non-converging wave does to the ones after it. |
| `charts/apply.go:307-331` | `Apply`: the wave loop from §4.3, plus `waveGroups`. |
| `charts/release.go:1107` | `waitWave`; `waitReady` delegates to it. |
| `charts/release.go:152` | `InstallOptions.Timeout` doc: it now also bounds a wave. |
| `cli/apply.go:154-174` | `printPlan`: conditional `WAVE` column. |
| `cli/apply.go:121-149` | `rejectUnsupported` — no change, but confirm no flag is added. There is deliberately **no `--wave` flag**: the file is the only source of truth, which is the rule this function exists to enforce. |
| `charts/README.md:130-132` | Replace *"Releases are applied in file order; add `--wait` if a later release needs an earlier one live"* with the wave section. |
| `charts/README.md:140-162` | Helmfile-compatibility section: note `wave` as a swarmcli extension. |
| `cli/charts.go:58-66` | The third copy of the release-file example. Per CE's `CLAUDE.md`, all three copies drift if one is missed. |
| `README.md:102-111` | The one-paragraph summary. |

### 6.2 CD — `Eldara-Tech/swarmcli-cd`

| File | Change |
|---|---|
| `go.mod` | Pin bump to the CE commit carrying the above. PR #155 is already in flight pinning `v1.13.0-rc6`; this lands on top of it. |
| `reconcile/reconcile.go:2091-2131` | `converge`: walk waves, barrier between them, warn naming the waves whose correction was skipped. |
| `reconcile/reconcile.go:2184-2213` | Extract `awaitSettled` (ungated, several releases, one deadline); `awaitConverged` keeps the `Wait` gate and calls it. |
| `application/application.go:451-467` | ``ReleaseStatus.Wave int `json:"wave,omitempty"` ``. |
| `drift/drift.go:64-80` | `releaseStatus`: `Wave: rp.Wave`. |
| `controller/app.go:351-359` | Conditional `WAVE` column. |
| `docs/configuration.md:399` | The `timeout` row currently reads "how long `wait` waits for a rollout before giving up". It now also bounds a wave barrier, which happens **whether or not `wait` is set** — so the row is wrong the moment waves exist, not merely incomplete. |
| `docs/configuration.md:410-411` | Replace the file-order sentence with waves; keep `wait` documented as the per-release tool it still is. |
| `docs/configuration.md:460-465` | The converge ordering paragraph: waves order corrections too, and the boundary waits regardless of `wait`. |
| `docs/concepts.md` | A short "sync waves" entry alongside drift/sync/health, if that file has the right altitude for it. |
| `CLAUDE.md` | Nothing — no new package. |

Nothing in `config/`, `appset/`, `api/`, `client/`, `prune/`, `backend/` or
`compose/` moves. In particular `reconcile.Engine` (`reconcile.go:114-124`) is
unchanged: `Apply`'s signature is the same, so `Reconciler.apply`
(`reconcile.go:1898`) is untouched.

---

## 7. Tests

The suite already has 37 integration tests, ~15 minutes against a 25-minute `go
test` timeout and a 30-minute job cap. Three things it does **not** have, all of
which this feature needs first: a multi-release release file, an ordering
assertion, and anything that deliberately fails to converge. §7.6 covers those.

### 7.1 The acceptance criterion

From #78, and the gate for anything in this area per the #54, #62 and #69
retrospectives:

> a fixture whose second wave fails to converge, asserting the third never
> deployed.

`TestAWaveThatDoesNotConvergeStopsTheWavesAfterIt` (§7.5) is that test, and it
asserts the third wave's absence by **release record**, not only by services:
`releaseRecordCount(t, cli, third) == 0` (`harness_test.go:381`) proves
`Engine.Apply` never reached it, whereas an empty service list is also what a
deploy that was accepted and then failed looks like.

### 7.2 CE unit tests

`charts/releasefile_test.go` — extend the existing map-based table at `:95-112`
and the parse helper at `:13-16`:

- `wave:` parses onto `ReleaseSpec`; an absent one is 0.
- A negative wave parses.
- A non-integer wave is a parse error naming the key.
- `wave` survives the marshal/parse round trip `source.synthesise` performs, and
  `wave: 0` is **omitted** by the marshal — the direct guard on `omitempty`, which
  is what keeps every `source.chart` application from growing a meaningless key.

**`fakeBackend` needs two probes first**, and the first is not optional: ordering is
**unobservable** with the fake as it stands. `fakeBackend.deployed` is a
`map[string]string` (`charts/release_test.go:26`), so nothing records the sequence
— which is why `TestApplyFailsFastAndReportsPartialProgress` has to infer order
from `require.NotContains(t, fb.deployed, "second")` rather than assert it. Add:

- `deployOrder []string`, appended in `DeployStack` (`charts/release_test.go:72`).
  The alternative — asserting on the returned `[]ApplyResult`, which `Apply` builds
  in iteration order — is cheaper but proves only what `Apply` *says* it did, not
  what reached the backend. Ordering is the whole feature, so it is asserted at the
  backend.
- `stackServiceCalls int`, incremented in `StackServices`
  (`charts/release_test.go:127`), so "this wave triggered no wait at all" is a thing
  a test can state rather than infer from elapsed time.

`fakeBackend.services` (`charts/release_test.go:32`) already lets a test stage
non-converging `ServiceState`s, and `waitPollInterval` (`charts/release.go:1085`)
is a `var` so the poll can be shrunk — between them a wave that never converges
costs a unit test milliseconds.

`charts/apply_test.go` — using `applyEnv(t, body, versions...)` (`:47`) with a
multi-release body:

- `PlanApply` sorts by wave, ascending.
- The sort is **stable**: two releases in one wave keep file order.
- Negative waves sort first.
- A file declaring no wave produces exactly file order (the backwards-compat
  guard).
- `Apply` deploys in wave order, asserted on `deployOrder`.
- `Apply` waits between waves and **not** after the last one.
- A wave whose release never converges stops the ones after it: nothing from a
  later wave appears in `deployOrder`, and `Apply` returns the earlier results
  alongside the error.
- A wave in which every release is `ActionUnchanged` triggers no wait at all — the
  W4 guard, asserted on `stackServiceCalls`.
- A single-wave plan performs no wait when `opts.Wait` is false — the guard on the
  *absence* of the new behaviour, in the shape #77 used for
  `TestConvergeDoesNotWaitWithoutThePolicy`.
- `waitWave` on one release produces byte-identical messages to today's
  `waitReady`, for both the wedged and the timeout case.

`charts/json_test.go:48-78` — `wave` appears in the plan JSON when non-zero and is
absent when zero, alongside the existing `TestPlanJSONKeys` assertions.

`cli/charts_test.go:361-377` — `TestPrintPlan`. Its
`require.Regexp(t, "hello\\s+r/whoami\\s+-\\s+0\\.1\\.8\\s+install", o)` is the one
assertion in either repository that a new column breaks mechanically, and the
conditional rendering in §4.7 is what keeps it passing untouched. Add a sibling
asserting the column *is* there for a multi-wave plan.

### 7.3 CE integration tests

In `integration-tests/charts/`, which already drives both the engine and
`cli.Dispatch` against a real swarm:

- `TestApplyRespectsWaves` — three releases in three waves; assert deploy order.
- `TestApplyStopsAtAWaveThatDoesNotConverge` — the CE mirror of §7.1, run through
  `charts apply -f` so the CLI's exit code and output are covered too.

These matter beyond symmetry: they are what makes W1's "independently useful to
`swarmcli charts` itself" true rather than asserted.

They are also cheaper than they look. `charts_wait_test.go` is **in the same
package** and already carries both fixtures these need — `unschedulableStack`
(never converges: a placement constraint no node satisfies, so the task stays
`pending` and nothing is ever pulled) and `slowStartStack` with `slowStartDelay` /
`slowStartGrace` (converges, but only after 15s). Neither has to be written, and
`TestWaitTimesOutOnAServiceThatCanNeverConverge` is the shape
`TestApplyStopsAtAWaveThatDoesNotConverge` follows, one scope up. The same two
fixtures are what §7.6 ports into CD's chart-based harness.

### 7.4 CD unit tests

`reconcile/reconcile_test.go`:

- `converge` corrects in wave order.
- `converge` waits at a wave boundary even with `syncPolicy.wait: false` — the
  unconditional-barrier rule.
- A wave whose correction does not settle stops the correction of later waves, and
  the releases in them are left reported-but-uncorrected.
- `awaitConverged`'s existing four tests from #77 still pass **unmodified**:
  `TestConvergeWaitsForTheCorrectionToSettle`,
  `TestConvergeThatWedgesFailsTheSync`, `TestConvergeWaitTimesOut` and
  `TestConvergeDoesNotWaitWithoutThePolicy`. That is the check that the
  `awaitSettled` extraction changed nothing; if any of them needs editing, the
  extraction moved behaviour it was not supposed to.
- A single-wave plan adds no wait — the absence guard again.

`drift/drift_test.go` — `FromPlan` carries `Wave` onto `ReleaseStatus`, and the
status list is in wave order.

`controller/app_test.go` — the `WAVE` column appears for a multi-wave view and is
absent for a single-wave one.

`source/source_test.go` — a synthesised release file for a `source.chart`
application still round-trips with no `wave` key.

### 7.5 CD integration tests

Four tests. Each registers `t.Cleanup(func() { removeStack(t, r); removeVolumes(t,
cli, r) })` for every release before the first sync, uses unique `e2e-…` release
names, and gets its controller identity free from `controllerID(t)`.

**1. `TestWavesDeployInWaveOrder`** — three releases, waves 0/1/2, one chart each,
`wait: false`. Assert every stack converges and that
`serviceOf(t, cli, r+"_app").Meta.CreatedAt` is strictly increasing across the
three. `CreatedAt` is daemon-assigned with sub-second precision and is exactly the
right primitive for an install; no test in the suite reads it today, which is why
§7.6 adds the accessor. Also assert `rec.View(app)` reports the three releases in
wave order with the right `Wave` values — folding §7.4's reporting check into a
test that has a real plan.

**2. `TestAWaveThatDoesNotConvergeStopsTheWavesAfterIt`** — the acceptance test.
Three releases in waves 0/1/2; wave 1's chart is `stallChartFiles`, whose service
is pinned by a placement constraint no node satisfies and so never leaves
`pending`. The application carries `wait: false` and
`SyncPolicy.Timeout: 25s`, rather than `releaseApp`'s `Wait: true` and 3 minutes —
the default is what would cost this test three minutes of wall clock, and
`wait: false` is what puts the failure on the wave barrier rather than on the
per-release wait, which is the thing under test. Assert:

- wave 0 converged: `waitForRunning(t, cli, first, 1)`;
- `SyncNow` returned an error mentioning the timeout and wave 1's release name;
- wave 1's stack exists and **has a release record** — `releaseRecordCount == 1`.
  `deployAndRecord` records the revision *before* it waits, so a wave that deployed
  and then failed to converge leaves a record behind. Asserting this is what makes
  the next line mean something;
- **wave 2 has no services and no release record** — `serviceNamesOf` empty and
  `releaseRecordCount == 0`. The contrast with the line above is the whole proof:
  wave 1 was reached and wave 2 was not;
- `rec.View` reports the sync as failed, with wave 2 still out of sync.

Two things about this test are deliberate and would otherwise be got wrong.

**An install, not an upgrade, so the failure is a timeout.** A brand-new service
has no `UpdateStatus`, so it can never report `paused` and can never reach
`PhaseWedged`. The only honest way for a *first* deploy to fail to converge is
therefore the deadline — which is also what an operator actually hits — and that is
why the 25-second timeout is load-bearing rather than a tuning detail.

**A placement constraint, not a failing healthcheck.** The obvious way to make a
service never converge is a check that never passes, and it is wrong here:
`harness_test.go:639-642` records that a failing healthcheck does not fail a
fixture, it hangs it and then gets the container killed, "which reads as something
else entirely". A task no node can schedule stays `pending` with nothing pulled and
nothing started, which is unambiguous.

**3. `TestASingleWaveNeitherWaitsNorSerialises`** — two releases, no `wave:` key,
`wait: false`, one of them `slowChartFiles` (§7.6.4: healthy only after ~15s).
Assert that `SyncNow` returns well before the slow release converges, and that both
stacks were created within a second of each
other. This is two guards in one test: the backwards-compat rule (a file declaring
nothing gets no barrier and nothing that did not block before starts blocking) and
the "within a wave, together" property that W3 exists to deliver.

It should be written **before** the CE change lands and shown to pass on `main` as
it stands. That is what makes it a regression guard rather than a description of
the new code: a compatibility test that has only ever run against the new
behaviour proves nothing about the old.

**4. `TestWavesOrderADriftCorrection`** — two releases in waves 0 and 1. The spec is
`waveApp` with `DriftDetection` set to `application.DriftLive` and
`SyncPolicy.Automated` to true: `waveApp` follows `releaseApp` in defaulting to
manifest mode, and manifest mode never asks the question this test is about.
Deploy, converge, then `updateOutOfBand` (`harness_test.go:826`) both services — a
replica change on each. Sync, then assert both were put back and that
`serviceOf(...).Meta.UpdatedAt` for wave 0's release precedes wave 1's.
`UpdatedAt` rather than `CreatedAt`, because a correction rewrites an existing
service.

Note that this test exercises the wave barrier with `wait: false`, which is the
whole of W3 on the converge path: without the unconditional barrier the two
corrections would be issued back to back and the assertion would be a coin toss.

### 7.6 Harness work the tests need first

Six additions to `integration-tests/harness_test.go`, each with the SPDX header
and `//go:build integration` the directory requires:

1. **`waveApp(name, repoDir string, timeout time.Duration) application.Spec`** —
   `releaseApp` (`harness_test.go:177`) hard-codes `Wait: true, Timeout: 3m`, which
   is wrong for all four tests here: `wait: true` moves the failure off the wave
   barrier onto the per-release wait, and three minutes is the wall clock test 2
   would spend. This is `releaseApp` with `Wait: false` and a caller-chosen timeout.
   `releaseApp` itself is left alone — every existing test depends on its defaults.

2. **`multiReleaseChartFiles(releases []waveRelease) map[string]string`** — the
   missing piece #78 names. `chartFiles` (`harness_test.go:194`) is the only writer
   of `swarmcli-release.yaml` in the suite and emits exactly one entry; this writes
   N entries with a `wave:` each, plus one chart directory per release. It takes a
   small `{release string; wave int; chart string}` so a test can mix the stall and
   slow charts into a wave.

3. **`stallChartFiles(release string)`** — a release that never converges. **Do not
   invent one**: CE's integration suite already has the right construction, proved
   in CI, in `integration-tests/charts/charts_wait_test.go`'s `unschedulableStack` —
   a `deploy.placement.constraints` entry naming a node label no node carries, so
   the task stays `pending` for ever. Port it to a chart. It is strictly better than
   the obvious alternative (a command that exits, restarting for ever): nothing is
   pulled, nothing is started, no container churn accumulates for the 25 seconds the
   test spends, and the failure mode cannot be confused with a crash-loop. The
   comment must say what it is for and why it is a separate fixture, in the shape
   `jobChartFiles`' comment already uses (`harness_test.go:734-738`) — a fixture
   that never converges is exactly the thing that made a shared fixture time out at
   three minutes once already.

4. **`slowChartFiles(release string)`** — converges, but only after a known delay,
   so "did the sync return before this was live" is a question with a stable
   answer. Again, **do not invent it**: CE's `slowStartStack` in the same file is
   this fixture, and its comment records the two rules that make it work and are
   easy to get wrong.

   A healthchecked task stays in task state `starting` until the first healthy
   event — swarmkit's container controller blocks in `Start()` waiting for one — so
   gating the check on a file the container creates after a delay keeps the task out
   of `running` for exactly that long. That is the lever, and it is the same swarm
   behaviour `richChartFiles` deliberately steers around at
   `harness_test.go:639-642`.

   And **`start_period` must comfortably exceed the delay**. Outside it, `retries`
   consecutive failures mark the container unhealthy and swarmkit shuts it down — so
   a fixture that gets this wrong restart-loops instead of coming up slowly, and the
   test asserts something else entirely. CE uses `3 × delay` and says why. Inside
   `start_period` a failing check costs nothing while a passing one still promotes
   immediately, which is what makes the delay the only thing being measured.

   One deviation from CE's copy: use `busybox:1.36` rather than `alpine:latest`.
   busybox has `sh`, `touch` and `test -f`, and it is what CD's workflow already
   pre-pulls.

5. **`createdAtOf(t, cli, name) time.Time`** and **`updatedAtOf(...)`** — the
   ordering primitive. `serviceOf` (`harness_test.go:807`) already finds the
   service; these read `Meta.CreatedAt` / `Meta.UpdatedAt` off it.

6. **`releaseOf(t, rec, app, release) application.ReleaseStatus`** — a sibling of
   `driftOf` (`harness_test.go:837`), which hard-fails on anything but exactly one
   release (`:844`) and so is unusable from every test here. `driftOf` is left
   alone; the twelve live-drift tests depend on its assertion.

The CI workflow's pre-pull step needs no change — every fixture stays on
`busybox:1.36`, which is already pulled.

### 7.7 Wall-clock budget

The workflow's own comment is the constraint: ~15 minutes used, a 25-minute `go
test` timeout, a 30-minute job cap, and the headroom is deliberate because a
timeout there reads as a defect in whatever was running when the alarm fired.

| Test | Estimate |
|---|---|
| `TestWavesDeployInWaveOrder` | ~45s — three converges plus teardown |
| `TestAWaveThatDoesNotConvergeStopsTheWavesAfterIt` | ~50s — one converge, a 25s timeout, teardown |
| `TestASingleWaveNeitherWaitsNorSerialises` | ~35s |
| `TestWavesOrderADriftCorrection` | ~60s — deploy, converge, mutate, correct |
| **Total** | **~3.5 minutes** |

That takes the suite to roughly 19 minutes against the 25-minute timeout, leaving
about 6 minutes of headroom. **Recommendation: leave the workflow's timeouts
alone** and note the new figure in the existing comment, which already tracks the
suite's growth (11 → 15 minutes over #76). If the measured cost comes in above
about 4 minutes, raise `-timeout` to 30m and `timeout-minutes` to 35 in the same
PR rather than shipping a suite that fits only on a fast runner.

Teardown is the hidden cost: `sleep` ignores SIGTERM, so every stack removal waits
out the stop-grace period. `stallChartFiles` is free there — its task never
schedules, so there is no container to stop — but every ordinary fixture pays it,
and each test above deploys two or three of them.

---

## 8. Sequencing and PR shape

The dependency is one-way and unavoidable: CD cannot compile against a `wave` that
CE has not published.

1. **CE PR — "sync waves"**. Everything in §6.1, with §7.2 and §7.3. Labels
   `A1-feature`, `B0-low-priority`, `C0-breaks-nothing`. Ships as one PR rather
   than split: the field, the sort and the barrier are unusable apart, and CE's own
   integration tests are what prove the feature rather than only the plumbing.
2. **CE tag or pseudo-version.** A pseudo-version off `main` is the normal state
   between CE releases and builds fine; a tag is only needed if this is going out
   with an RC.
3. **CD PR #155 lands first** — it is already open, pinning `v1.13.0-rc6`. The wave
   PR rebases on top of it so the pin bump in this change is one line moving from
   rc6 to the wave commit, rather than a bump carrying four unrelated CE commits.
4. **CD PR — "sync waves"**. §6.2, §7.4 and §7.5. Same three labels.
5. Close #78 from the CD PR; reference the CE PR from both.

Two process notes from `CLAUDE.md` that apply directly. Add all three labels in one
REST call, and expect that an early `check_labels` run can still evaluate an
incomplete set and *fail* rather than be cancelled — the fix is to re-fire the
event by toggling a label, since the PAT cannot re-run jobs. And the CE
integration job is a DinD multi-node swarm driven by `test-setup/testenv.sh`, not
the single-node runner swarm CD uses; the CE tests in §7.3 belong to that harness
and its conventions.

The obvious worry about §4.2 — that CE is a library and this changes an order a
doc comment has promised since the type existed — was checked rather than
assumed. **`swarmcli-be` does not import `github.com/Eldara-Tech/swarmcli/charts`
at all**: zero hits across its 177 Go files. `swarmcli-cd` is the only external
consumer of `Plan.Releases`, and every one of its walks is covered in §6.2.

Before opening the CE PR: `go test ./...` plus `golangci-lint run ./...
--build-tags=integration`, and `./scripts/check-spdx.sh` in CD for any new file.

## 9. Risks and how each is bounded

**A release file with `wave:` fails to parse on an older binary.** `KnownFields(true)`
means the key is a hard error — `field wave not found in type charts.ReleaseSpec` —
on any swarmcli or swarmcli-cd built before this lands, and `apiVersion: v1`
offers no graceful degradation. This is true of every key ever added to the file,
`owner:` included, and the mitigation is documentation: the release-file reference
states the minimum swarmcli version for `wave`. It is not a `C1-breaking-change` —
nothing that works today stops working — but it is worth a line in the CE PR body
so the release notes carry it.

**Plan order changes for wave-declaring files.** Mitigated by W5: a file that
declares nothing sorts to exactly file order, so every existing plan, payload and
rendered table is byte-identical. The order only moves where somebody asked for it
to.

**A wave barrier blocks a caller that passed `Wait: false`.** By design, and
bounded by `Timeout` (5 minutes by default, per wave). The exposure is a reconcile
loop that takes longer per tick for an application that declared waves; the
per-application lease means it cannot stall any other application, and a cancelled
context unwinds it because `waitWave` selects on `ctx.Done()`.

**The two barriers could drift apart.** CE's `waitWave` and CD's `awaitSettled` are
separate implementations of the same idea, and that is deliberate — #77 established
that the judgement is CE's exported `Rollup` rather than a second copy of the
rules, and both call it. What could diverge is the scoping and the deadline, so
both are stated in the same terms here and both are tested for the wedged, the
timeout and the converged case.

**The timing-sensitive integration test.** `TestASingleWaveNeitherWaitsNorSerialises`
is the one test here that asserts something did *not* take time. It is bounded by
using a `start_period` well above the assertion window rather than a bare race, but
it is the first candidate to loosen if it flakes on a slow runner — and it should
be loosened rather than deleted, because it is the only guard on the
backwards-compat rule that the whole design rests on.
