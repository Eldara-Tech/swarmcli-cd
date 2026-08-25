# swarmcli-cd #234 — the controller updates itself

Plan for making `swarmcli-cd` manage its own stack from the app set, as
requested in
[swarmcli-cd#234](https://github.com/Eldara-Tech/swarmcli-cd/issues/234).

Status: **implemented and released** 2026-08-19. Written against
`swarmcli-cd@3fd4a47` (v1.2.0) and `swarmcli-charts` main; shipped in
`swarmcli-cd v1.3.0-rc1` and chart `swarmcli-cd/v0.2.4`.

The five PRs are #236, #237, #238, #239 and #240 under epic #235, and the chart
half is swarmcli-charts#175. Three things the plan did not anticipate, each found
by a test rather than by reading: a **fourth** guard — `compose.checkBindSources`
refuses the docker socket during conversion, before any of the three in scope
(#238); a renamed controller service as a second route to two controllers, which
became the fourth preflight refusal (#239); and `capability.ErrOwnStack` rather
than a sentinel in `backend`, so a companion backend need not import the OSS
applier to be skipped by the sweep (#240). The TLS/SSO gap in the chart is
unclosed and is recorded in swarmcli-charts#175.

---

## Contents

1. [Executive summary](#1-executive-summary)
2. [What #234 actually is](#2-what-234-actually-is)
3. [Decisions taken](#3-decisions-taken)
4. [Design](#4-design)
   - [4.1 The `self` field](#41-the-self-field)
   - [4.2 What the exemption permits, and what it still refuses](#42-what-the-exemption-permits-and-what-it-still-refuses)
   - [4.3 Adopting the stack that is already there](#43-adopting-the-stack-that-is-already-there)
   - [4.4 Apply ordering: the controller writes itself last](#44-apply-ordering-the-controller-writes-itself-last)
   - [4.5 When the new controller does not come up](#45-when-the-new-controller-does-not-come-up)
   - [4.6 The three refusals that keep it recoverable](#46-the-three-refusals-that-keep-it-recoverable)
   - [4.7 Prune, uninstall and the sweep](#47-prune-uninstall-and-the-sweep)
   - [4.8 What the operator sees](#48-what-the-operator-sees)
5. [What has to change in swarmcli-charts](#5-what-has-to-change-in-swarmcli-charts)
6. [Work breakdown](#6-work-breakdown)
7. [Test plan](#7-test-plan)
8. [Risks, and what this deliberately does not solve](#8-risks-and-what-this-deliberately-does-not-solve)
9. [Rollout](#9-rollout)

---

## 1. Executive summary

An operator adds the `swarmcli-cd` chart to `applications.yaml`, marks that one
entry `self: true`, and from then on the controller upgrades itself the way it
upgrades everything else: a commit changes the chart version or the values, and
the next reconcile applies it.

Four things stand in the way today, and all four are deliberate. The stack
mounts the controller's own secrets, which `rejectForbiddenResources` refuses
unconditionally. The release name has to be the controller's own stack
namespace, which `rejectOwnNamespace` refuses. The stack that is already running
has no release record, so `rejectForeignNamespace` refuses to adopt it. And the
chart engine writes the release record *after* the deploy, so Swarm's stop-first
rollout kills the controller before the record is written — leaving a stack with
no record, which the third refusal then blocks for ever.

The design turns each refusal into a rule that has a *self* case rather than
deleting it, and solves the fourth by ordering: `ApplyServices` skips the
controller's own service and hands the caller a closure, which the reconciler
runs only after `engine.Apply` has returned and the revision is recorded. **The
controller kills itself last, after the whole pass has been written down.** No
change to `swarmcli` (CE) is needed, and no new release-record status.

Two properties fall out of machinery that already exists. A self-update that
Swarm rolls back cannot loop, because swarmcli-cd#90 already refuses to
re-push a spec the platform reverted (`drift/live.go:147`, `convergeable` at
`reconcile/reconcile.go:1406`). And a self-update that was never issued heals
itself, because live drift sees the image differ and converges.

Three preflight refusals are added, covering the ways a self-apply could make the
controller unable to fix itself: dropping the docker socket, dropping the
admin-token secret, or dropping the app-set source. Anything else the manifest
drops is a loud warning, not a refusal, because the controller keeps running and
the operator can commit a correction.

Scope: five PRs in `swarmcli-cd`, one in `swarmcli-charts`, no CE change.

---

## 2. What #234 actually is

The reported error is the first refusal, reached because the reporter's app set
pointed an application at the `swarmcli-cd` chart:

```
release 'cd': deploy failed: service 'controller' mounts secret
'swarmcli-cd-git-token', which is a credential belonging to the controller;
a reconciled stack may not mount it
```

`rejectForbiddenResources` (`backend/backend.go:225`, refusal at `:257`) reads
what Swarm has mounted into this controller off its own service spec
(`backend/selfmounts.go`) and refuses any reconciled stack that mounts one. The
`swarmcli-cd` chart declares `auth.adminTokenSecret` and `git.tokenSecret` as
`external: true` and mounts both, so it is refused by construction. That guard
landed in `adfd2e8` for swarmcli-cd#46: without it, any chart could mount the
admin token, which is root-equivalent on the swarm.

Behind it sit three more, none of which the reporter reached:

| guard | where | why it fires |
|---|---|---|
| own namespace | `backend/backend.go:519` | a release named `swarmcli-cd` writes the chart's services over the controller's own |
| foreign namespace | `backend/service.go:208` | the running stack was put there by `docker stack deploy` and has no release record |
| record after deploy | `swarmcli/charts/release.go:363` then `:384` | the rollout kills the controller before `record()` runs |

The reporter's release was named `cd`, not `swarmcli-cd`, so even with the
secret guard lifted the deploy would not have updated anything — it would have
stood up a **second** controller stack beside the first, and the chart pins
`replicas: 1` precisely because two controllers reconciling one app set apply
over each other every interval.

---

## 3. Decisions taken

Confirmed with the maintainer before this was written.

**Shape — full chart-driven self-management.** The app set declares the
controller's own chart; the controller adopts its stack as a release and applies
it. The alternatives considered were an image-only self-update (much smaller,
but leaves the controller's configuration outside GitOps) and an out-of-band
one-shot updater service (no self-kill, but a second published artefact and the
controller's credentials handed to a task, which is the thing every guard here
exists to prevent).

**Opt-in — `self: true` in the app set alone**, with no second controller flag.
This is consistent with the existing trust model: `allow.hostPaths` can already
grant the docker socket, so whoever writes the app set is already
root-equivalent, and `docs/configuration.md` says so.

**Recording — defer the controller's own service, CD-only.** No change to
`swarmcli`, no cross-repo release ordering, no new record status.

**Preflight — refuse only the unrecoverable.** The socket, the admin token and
the app-set source. Everything else dropped is a warning.

---

## 4. Design

### 4.1 The `self` field

`application.Spec` gains one field:

```go
// Self marks the one application that deploys the stack this controller
// itself runs as.
Self bool `json:"self,omitempty" yaml:"self,omitempty"`
```

Validated in `config` (`validate`, `validateApplication`), which has no daemon
and so checks only what is knowable from the file:

- at most one application may set it;
- it requires `source.chart` — a `releaseFile` application is several releases
  and cannot be one of them;
- it implies `driftDetection: live`, defaulted in `applyDefaults` beside the
  release name so an operator need not know to ask for it, and an explicit
  `manifest` is refused with the reason. Without live drift a rolled-back
  self-update is invisible: the plan reads *unchanged* because the record
  already names the new revision, and nothing else compares the running spec to
  the manifest.

The release name is **not** defaulted specially. `ChartSource.Release` already
defaults to the application's name, so an application called `swarmcli-cd` gets
the right name for free; anything else is caught below with a message naming
both values.

The namespace check belongs where the namespace is known, which is the backend:
a self release whose name is not `selfMounts.namespace` is refused, naming what
the controller is actually deployed as. This also gives the right answer for a
`destination` that resolves to another swarm — that backend inspects a daemon
that has never heard of this container, gets an empty namespace, and the refusal
says the controller is not deployed on that swarm.

### 4.2 What the exemption permits, and what it still refuses

Only for the self release, and only for what the controller has *already* got:

- `mine.secrets` and `mine.configs` become permitted references and permitted
  declarations. They already *are* the release's own, because the release
  namespace **is** the controller's namespace — what hides that is only that a
  cluster-global secret or config carries no namespace prefix for
  `scopedUnder` to recognise.
- `mine.volumes` likewise, and the controller's own stack networks
  (`inControllersStack`) become joinable.

Everything else stands, unchanged and for the same reasons:

- **the engine's release records** are still refused. Nothing in the CD chart
  mounts one, and a manifest that wants to is not a self-update.
- **`allow`** still governs everything outside the controller's own set — the
  Traefik network in `exposure.mode: traefik` goes through `allow.networks` as
  it does for any other application.
- **every guard for every other application** is untouched. The exemption is
  carried on the per-application backend copy, the same seam `allow` uses
  (`WithAllowedReferences`), so it cannot leak to a second application: a
  backend nobody marked self refuses exactly what it refuses today.

The seam is a new optional interface in `capability`, matching the four already
there:

```go
// SelfRelease is the optional interface a backend implements to deploy the
// stack this controller itself runs as.
type SelfRelease interface {
	WithSelfRelease(hold DeferSelf) charts.Backend
}

// DeferSelf receives the one write that replaces this controller, so the
// caller can issue it after everything else in the pass has been recorded.
type DeferSelf func(apply func(context.Context) error)
```

### 4.3 Adopting the stack that is already there

`rejectForeignNamespace` refuses a namespace whose services this controller has
no release record for, because those services were put there by something else
and are not ours to write over. For the self release the answer to "whose are
they" is known without a record: they are this controller's own, identified by
the service carrying our own service id. So the refusal gains a self case, and
the first self reconcile plans as an **install** and adopts.

This is the one place the design accepts something the surrounding code
otherwise refuses, and it is worth being explicit that the operator asked for it
in the file that already defines every deployment on the swarm.

### 4.4 Apply ordering: the controller writes itself last

`ApplyServices` (`backend/service.go:125`) walks the stack's services in order.
For the self release it skips the one whose live service id equals the
controller's own — already read in `readSelfMounts`, which needs only to keep it
— and hands the caller a closure that re-reads that service and applies the same
spec.

Nothing else about the deploy changes. Networks, secrets, configs and the
sidecar are applied as usual, `DeployStack` returns success, and the chart
engine writes the release record. The reconciler then runs the deferred closure
**at the end of the whole pass** — after `apply`, after `converge`, after the
prunes and the confirming re-plan. The controller is replaced with everything
about the pass already durable.

That last placement matters for a second reason: `converge` writes through
`Backend.DeployStack` directly rather than through the engine
(`reconcile/reconcile.go:2199`), so a deferral taken inside a drift correction
would otherwise never be run and the controller's own drift would never be
corrected — silently. Holding one deferral for the pass and running it once at
the end covers both writers.

`syncPolicy.wait` cannot cover the controller's own rollout, by construction:
the process doing the waiting is the one being replaced. Say so in the docs
rather than pretending otherwise.

Every other application's in-flight sync is cancelled when the task stops, which
needs nothing new: swarmcli-cd#107 already established that a cancelled apply is
not an outcome and is neither dispatched nor recorded as a failed sync — it was
written for exactly this, "a clean `docker service update` of the controller".

### 4.5 When the new controller does not come up

Two failure modes, and the existing machinery already answers both.

**Swarm rolls the update back.** `drift.rolledBack` (`drift/live.go:147`) reads
`UpdateStatus`, and `convergeable` (`reconcile/reconcile.go:1406`) lets a
rolled-back service veto correction of the whole release — swarmcli-cd#90,
written for exactly this shape of loop. The recovered controller therefore
reports live drift, carries the daemon's own reason on the service, and does
**not** re-push the spec Swarm just rejected. The remedy is a commit, which
arrives as an upgrade and clears the state with it. This is why a self
application gets `driftDetection: live` whether or not it asked for one.

**The deferred write never landed** — an API error, or the pass was cancelled.
The record names revision N and the running spec is N-1, live drift sees the
image differ with no rollback state, and the next reconcile converges. Self-heals.

The dangerous third case is the one the chart has to fix rather than the
controller: with `replicas: 1`, stop-first ordering and Swarm's default
`failure_action: pause`, a controller image that fails its healthcheck leaves
**zero** controllers running and nothing left to correct it. See §5.

### 4.6 The three refusals that keep it recoverable

Before the self service is written, the rendered spec is compared with the live
one and the deploy is refused if it would drop any of:

1. **the `/var/run/docker.sock` bind** — without it the controller can reconcile
   nothing, including itself;
2. **the secret named by `SWARMCLI_CD_ADMIN_TOKEN_FILE`** in the live spec —
   without it the API and the healthcheck stop answering, and Swarm restarts a
   controller that is working;
3. **the app-set source** — whichever of `--config`, `--appset-dir` or
   `--appset-repo` the live command carries must still be present, and for the
   first two the backing config or volume must still be mounted.

Each refusal names the field and what would be lost. Everything else the
manifest drops — TLS, SSO, a registry credential — is logged at `Warn` and
dispatched as an event: the controller keeps running, so a wrong value is a
commit away from being fixed, and refusing it would block an operator
legitimately turning SSO off.

### 4.7 Prune, uninstall and the sweep

`RemoveStack` keeps refusing the controller's own namespace, self or not: there
is no correct way for a controller to delete itself, and `--prune-volumes` would
take the volume holding every application's clone with it.

The sweep needs one explicit skip. If the self application leaves the app set,
its release is stamped for a departed application and `prune.Release` would call
`RemoveStack`, which refuses — every sweep, for ever. So the sweep skips the
controller's own namespace and says once, at `Warn`, that the controller's stack
is no longer declared and is being left alone.

### 4.8 What the operator sees

The self application appears in `app list`, the API and the UI like any other,
with `self: true` on the wire so the UI can mark it. Two additions:

- a `self-update-issued` notify event, dispatched immediately before the
  deferred write, so the event stream carries the last thing this controller did;
- a matching `Info` line naming the revision and the image, which is the last
  line in the log before the task is replaced.

The sync that performs a self-update has no recorded *outcome* — the process
recording it is gone. The record it does leave is the release revision, written
before the kill, which is what the next controller reads.

---

## 5. What has to change in swarmcli-charts

Two things, and the first is not optional.

**`update_config.failure_action: rollback`** on the controller service. The
chart already renders `monitor` when the healthcheck is on; without
`failure_action` Swarm's default is `pause`, and §4.5's third case is an outage
that only a human at a shell can end. With rollback, a bad self-update reverts
and the recovered controller reports drift instead.

**TLS and OIDC values.** `charts/swarmcli-cd/values.yaml` has no TLS listener
and no SSO settings, while `stack.yml` documents both. A deployment using either
that adopts self-management gets §4.6's warning and loses them on the first
self-apply. The chart should be able to express what the shipped bootstrap
expresses before self-update is announced. The data volume is already fine:
`persistence.volumeName: swarmcli-cd-data` scopes to the same
`swarmcli-cd_swarmcli-cd-data` the bootstrap stack uses, so the clone and chart
cache survive adoption.

Both land in one PR against `swarmcli-charts`, released by a
`swarmcli-cd/vX.Y.Z` tag — `Chart.yaml`'s `version` is a placeholder the tag
sets, so the PR edits the template and `appVersion` only. CI there runs only on
PRs based on `main`.

---

## 6. Work breakdown

An epic issue on `swarmcli-cd` tracking five PRs, linked from #234. Each PR is
independently reviewable and none of them turns the feature on until the last.

**PR 1 — the field.** `application.Spec.Self`, validated in `config`: one at
most, chart source only, live drift implied. Wire contract only; nothing reads
it yet. *(`application/`, `config/`)*

**PR 2 — the backend knows what it is.** `selfMounts` keeps the service id it
already reads; `capability.SelfRelease` and `DeferSelf`; `WithSelfRelease` on
`*backend.Backend`; the release-name-equals-namespace refusal. Still nothing
calls it. *(`backend/`, `capability/`)*

**PR 3 — the exemptions.** The self cases in `rejectForbiddenResources`,
`rejectOwnNamespace` and `rejectForeignNamespace`, each with the test that shows
a *non*-self backend still refuses. This is the security-carrying PR and wants
the most careful review. *(`backend/`)*

**PR 4 — ordering and the refusals.** `ApplyServices` defers the controller's
own service; the three preflight refusals and the drop warnings. *(`backend/`)*

**PR 5 — the reconciler, the sweep and the docs.** Thread `Self` through
`reconcileHeld`, run the deferral at the end of the pass, the notify event, the
prune skip, and the documentation: a new section in `docs/configuration.md`, the
release-name section of `docs/concepts.md`, and the chart README.
*(`reconcile/`, `prune/`, `docs/`)*

**Chart PR** — `swarmcli-charts`, §5. Independent of all of the above and can
land first.

Each PR carries one label from each of A/B/C in a single REST call. PR 3 is
`A3-technical` / `B2-high-priority` / `C0-breaks-nothing`.

---

## 7. Test plan

**Unit** — the repository's guards are all unit-tested against a faked
`ContainerInspect` / `ServiceInspectWithRaw` (`backend/backend_test.go`), which
is exactly the shape the self path needs. Every exemption gets a pair: the self
backend permits, a backend not marked self still refuses. swarmcli-cd#109 is the
precedent — a guard nothing exercises is a guard that can be deleted by
accident.

**The ordering test that matters**: a fake API asserting that the controller's
own service is not written by `DeployStack`, and *is* written by the closure —
and that the closure is run after the release record exists. That is the whole
of the fourth blocker and it must fail if the ordering is reversed.

**Integration** — the current harness runs the reconciler **in-process**
(`integration-tests/harness_test.go`), not as a swarm service, so `selfMounts`
comes back empty and every self path is inert there. A real end-to-end test
means deploying the built image as a stack in the DinD swarm and watching it
replace itself. That is worth doing and is its own step; the fallback, if the
harness fight is disproportionate, is a `mount_guard_test.go`-style test that
asserts a *non*-self application still cannot mount the controller's secrets —
which is the regression that would actually hurt.

**Manual** — on a scratch swarm: bootstrap from `stack.yml`, add the self entry,
commit an image bump, watch it land; then commit a deliberately broken image and
confirm rollback plus a drift report rather than a loop.

---

## 8. Risks, and what this deliberately does not solve

**The first self-apply rewrites the controller from the chart.** Anything the
chart cannot express is dropped. §4.6 refuses the three that are unrecoverable
and warns about the rest; §5 closes the two known gaps. It stays true in general
that adopting self-management means the chart is now the definition.

**The app set becomes the only thing standing between a chart author and the
admin token** for the one application marked self. That is not a new boundary —
`allow.hostPaths: ["/var/run"]` already grants the socket — but it is a
sharper one, and the docs should say plainly that `self: true` is the highest
privilege in the file.

**Not solved: two controllers.** Nothing here changes `replicas: 1` or makes a
self-update non-disruptive. The API is unavailable for the length of the
rollout.

**Not solved: reverting a self-update from the controller.** Swarm's own
rollback is the only mechanism, and the remedy for a bad revision is a commit.
This repository has no release-rollback command at all today, and giving one to
the self release would mean deploying through this same path — out of scope
here.

**Not solved: bootstrapping.** The controller still has to be deployed by
`docker stack deploy` once. Self-update maintains it; it does not install it.

---

## 9. Rollout

The ordering is not optional and belongs at the top of the documentation.

`config/config.go:103` decodes the app set with `KnownFields(true)`. A
controller older than the release that adds `self:` therefore refuses the
**entire** app set the moment the field appears — it keeps its last-good set and
stops following git, which is a frozen deployment plus a loud error rather than
an outage, but it is not what an operator expects from adding one line.

So: **upgrade the controller to the release carrying this, by hand, and only
then add the entry.** The chart PR from §5 should be tagged and released ahead
of that, so the very first self-managed revision already carries
`failure_action: rollback`.
