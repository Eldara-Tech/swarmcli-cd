# Reconciler work lifecycle — design for #106 and #105

Against `main` @ `4bd6938`. Every claim below was re-verified against that tree,
not taken from the issue text.

## Contents

1. [Executive summary](#1-executive-summary)
2. [What is actually true on `main` today](#2-what-is-actually-true-on-main-today)
3. [The design: the entry is the owner](#3-the-design-the-entry-is-the-owner)
   - 3.1 [The lease](#31-the-lease)
   - 3.2 [The entry context](#32-the-entry-context)
   - 3.3 [Identity, not name](#33-identity-not-name)
   - 3.4 [Reaching the daemon with a context](#34-reaching-the-daemon-with-a-context)
   - 3.5 [The git working tree](#35-the-git-working-tree)
   - 3.6 [Panic isolation (#105)](#36-panic-isolation-105)
   - 3.7 [Shutdown](#37-shutdown)
4. [Decisions — settled 2026-07-31](#4-decisions--settled-2026-07-31)
5. [What this design does not fix](#5-what-this-design-does-not-fix)
6. [Verification](#6-verification)

---

## 1. Executive summary

**Nothing owns the lifecycle of work in this controller.** `appEntry` is a data
record with a mutex bolted to the side; the mutex serialises syncs but is not a
handle on them. Consequently four separate things each know a *different* subset
of what is running:

| knows about | loop goroutine | API detached sync | writes to status |
| --- | --- | --- | --- |
| `e.cancel`/`e.done` | yes | no | — |
| `r.wg` | yes | no | — |
| `e.syncing` | yes | yes | — |
| `r.apps[name]` | — | — | by name, not identity |

Every symptom in #106 is one of those gaps. `Remove` waits on the first column
and documents a guarantee that spans all four. `record` takes the global write
lock because the status map is the only thing that *is* owned. Membership is a
name lookup because there is no work handle to key on.

**The fix is one idea: the `appEntry` becomes the owner of every unit of work
done on its behalf, and the only way to do work is to take a lease from it.**

A lease is a channel semaphore plus the entry's own context. Taking one is
context-aware, so a shutdown never queues behind a stuck sync. Holding one is
proof the application is still a member. Returning one is what `Remove` and
shutdown wait for. The loop goroutine, the API's detached sync and any future
webhook trigger all go through the same door, so all four columns above collapse
into one.

Everything else in #106 — the interval floor, the `wg.Add`/`Wait` race, the
inverted shutdown order, the SSE drain — is independently small and does not
depend on this. They ship as their own PR (§4) so this diff stays one idea.

## 2. What is actually true on `main` today

Re-verified line by line; the issue text is mostly accurate but three things
have moved.

**Still true.**

- `record` holds `r.mu.Lock()` across a live daemon read
  (`reconcile/reconcile.go:2145` → `:2168` → `backend/backend.go:598`
  `docker.SnapshotWith(context.Background(), …)`), **once per release**. Each
  call is a whole-swarm snapshot: NodeList + ServiceList + TaskList + Info.
- `Remove` (`reconcile.go:384-387`) waits only for the loop goroutine.
  `api.detach` (`api/api.go:251-253`) spawns `context.WithoutCancel(…)` and is
  registered nowhere.
- Membership is a name lookup at `reconcile.go:1402` and `:2148`; nothing
  re-checks entry identity after `e.syncing` is taken.
- `git/git.go` has no mutex. One clone per application at `<root>/<app>`,
  mutated by `Checkout(Force)` + `Clean(Dir:true)`.
- `recover()` appears nowhere — confirmed zero hits across the tree.
- `config.go:155` rejects only a *negative* interval.
- Shutdown order is inverted (`controller/run.go:533-542`): the single `ctx`
  stops the reconciler, then the listener is drained.
- `Run` (`:271-272`) does not synchronise with `startLoopLocked`'s `wg.Add`.

**Moved since the issue was written.**

- **#125 landed** and added `stackServicesReader`/`ReadStackServices`
  (`reconcile.go:504-515`, `backend/backend.go:597`). This changed *what a failed
  read means* — it no longer reads as an empty swarm — but it did **not** move
  the call out of `r.mu`, and `ReadStackServices` still uses
  `context.Background()`. Item 1 of #106 is unchanged in substance.
- **#115 landed** the nil guard in `prepareUpdate` (`backend/service.go:356`),
  so the specific live panic path in #105 is closed. What remains of #105 is the
  blast-radius decision only, which its own commit message explicitly deferred.
- **`charts.Backend` is not entirely context-free.** Ten of its fourteen methods
  take a `ctx` and honour it. The four that do not are `DeployStack`,
  `RemoveStack`, `RefreshSnapshot` and `StackServices`
  (`charts/release.go:73-99`). CE's `waitReady` (`charts/release.go:926-943`)
  takes no context either and polls with `time.Sleep`. So cancellation today
  reaches the config/network/secret half of a deploy and stops dead at the
  service half — which is worth stating precisely, because it changes the fix.

**One thing the issue does not say.** `record` calls `stackServices` in a loop
over releases, and each call is a *full swarm snapshot*. An application with ten
releases costs ten NodeList+ServiceList+TaskList+Info round trips per reconcile.
Moving the read out of the lock fixes the global stall but leaves the N-snapshot
cost. That is a separate issue and I propose filing it rather than folding it in.

## 3. The design: the entry is the owner

### 3.1 The lease

`appEntry.syncing sync.Mutex` becomes a lease. The type is small:

```go
// held is the lease: capacity one, send to take it, receive to return it.
//
// A channel rather than a Mutex because acquiring has to be abandonable. A
// shutdown, or a Remove, must not queue behind a sync that is blocked in an
// unresponsive daemon — which is exactly the case both exist to handle.
// Holding it is also what proves the application is still a member.
held chan struct{}
```

`acquire` is context-aware and refuses a retired entry:

```go
func (e *appEntry) acquire(ctx context.Context) (func(), error) {
	if err := e.ctx.Err(); err != nil {
		return nil, err // the application has left the set, or we are shutting down
	}
	select {
	case e.held <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.ctx.Done():
		return nil, e.ctx.Err()
	}
	return func() { <-e.held }, nil
}
```

`drain` is what `Remove` and shutdown wait on — taking the lease *is* the proof
that nobody holds it:

```go
func (e *appEntry) drain(d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case e.held <- struct{}{}:
		<-e.held
		return nil
	case <-timer.C:
		return errStillSyncing
	}
}
```

### 3.2 The entry context

Each entry owns one context, created with it. Everything derived from it dies
with the application:

```go
e.ctx, e.cancel = context.WithCancel(context.Background())
```

`startLoopLocked` no longer derives the loop's context from `r.root`; it links
the two instead, so there is exactly one cancel to call:

```go
stop := context.AfterFunc(r.root, e.cancel) // controller shutdown retires every entry
```

and runs the loop on `e.ctx`. An in-flight sync started by the API — which today
severs itself from any context at all — is cancelled by the same `e.cancel`,
because `reconcile` links the caller's context to the entry's:

```go
ctx, cancel := context.WithCancel(ctx)
defer cancel()
defer context.AfterFunc(e.ctx, cancel)()
```

`context.AfterFunc` (Go 1.21+, this module is on 1.26) is the stdlib primitive
for exactly this and needs no hand-rolled watcher goroutine. Its returned `stop`
unregisters, so a completed sync leaves nothing behind.

`Remove` then cancels once and waits once:

```go
e.cancel()                 // the loop and every in-flight sync, whoever started it
if e.done != nil { <-e.done }
if err := e.drain(removeDrain); err != nil { … }
```

The wait is bounded (D3), so `Remove` cannot promise quiescence — it promises
removal. That makes "removed" and "still working" two questions, and the second
one gets its own method: an entry whose work outlives the wait goes on a
`retiring` list, and `Draining()` reports it and forgets it once it stops. The
app-set sweep consults `Draining()` beside its existing "has everything planned
once" gate, for the same reason that one exists — what it is about to delete has
to be something nobody is still writing.

`Remove` returning an error for this was the first shape and was wrong: the
removal *succeeded*, so a caller that retried would find nothing, and a caller
that treated it as a failure would skip recording the departure. One mechanism,
reported where it is actionable, beats two that half-agree.

`api.detach` keeps its `context.WithoutCancel` — severing from the *request* is
correct and deliberate — and gains nothing else. The reconciler is what supplies
the lifetime, which is the point: the API should not have to know.

One seam does change. D2's coalescing has to be decided *in the request*, because
the 202 goes out before the detached sync has done anything: a decision made
inside the goroutine would be made after the response already claimed a sync had
started. So `SyncNow` splits into `AcceptSync(app) (func(context.Context) error,
error)` — reserve synchronously, run detached — and `SyncNow` becomes the two
called in sequence, for tests and for any caller that wants to block. The queue
slot is returned when the sync *starts*, not when it ends: a request arriving
while one runs is asking about a repository that one has already read, so
coalescing it there would silently drop a fix somebody had just pushed.

### 3.3 Identity, not name

Every write helper takes the `*appEntry` it was given rather than re-looking-up
the name, and checks identity under `r.mu`:

```go
// current reports whether e is still the entry this name refers to. A name that
// left the set and returned has a new entry; the departed one's late write must
// not land on it.
func (r *Reconciler) current(e *appEntry) bool { return r.apps[e.name] == e }
```

`name` becomes an immutable field on the entry (a `Replace` cannot change it —
that is documented as a Remove plus an Add). The check replaces the
`e, ok := r.apps[app]` in `record`, `recordResult`, `setError` and
`recordPruneFailures`. It subsumes the existing "was it removed" check: a
departed name that has not returned fails the map lookup, a departed name that
*has* returned fails the pointer comparison.

### 3.4 Reaching the daemon with a context

`record`'s daemon read moves out of the lock. The read needs to know whether the
sync failed, which it takes from `result` and one `RLock`ed peek at the previous
`LastSync`; that peek is safe because the lease serialises every writer.

That fixes the *global stall*. It does not make the call cancellable, and the
`DeployStack` path is not reachable from here at all — CE's engine calls it, not
us. Two ways to close that; **D1 chose the second**, and the first is recorded
because it is what a reader will otherwise propose:

**Option A — bind the context to the backend, no CE change.** This repository
already builds a per-sync backend by decoration: `withRegistryAuth`,
`withForbiddenSecrets` and `withOutOfBandNotifier` each return a copy, and the
last one **already closes over the sync's `ctx`** (`reconcile.go:465-488`). A
fourth decorator in the same shape:

```go
// WithContext returns a copy of the backend whose context-free charts.Backend
// methods run under ctx.
func (b *Backend) WithContext(ctx context.Context) charts.Backend
```

`DeployStack`, `RemoveStack` and `ReadStackServices` then use `b.ctx` instead of
`context.Background()`, and CE's engine becomes cancellable without CE knowing.

- Storing a context in a struct is normally wrong; here the struct *is* the
  per-call value and the file already has three of them. It is the idiom this
  repo chose for the same problem three times.
- Residual: CE's `waitReady` polls `StackServices` with `time.Sleep`. Once
  cancelled, `StackServices` returns nil, `charts.Rollup(nil)` is
  `PhaseProgressing`, so `waitReady` spins to *its own* timeout (`syncPolicy.timeout`,
  default 5 min) rather than returning promptly. Bounded, but not prompt — which
  is why the drain in §3.7 must have a deadline regardless.

**Option B — widen CE's `charts.Backend` (chosen).** Correct at the root, and
the only option that fixes `waitReady`. Cross-repo, but cheaper than it looks:
`swarmcli-be` does not implement the interface, CE is pre-`v1.13.0`-stable, and
this module pins a pseudo-version rather than a tag, so no CE release is needed.
It also reaches below the interface — `DeployStackInContext` and
`RemoveStackCLIInContext` shell out through `exec.Command`, so this is what makes
a hung `docker stack deploy` killable.

The cost is ordering. `*backend.Backend` must satisfy the interface, so the pin
bump cannot be separated from `backend/`, which #103 is in flight in. Hence PR-C
in §4: the lifecycle work does not wait for it, and until the pin moves the four
CE methods stay exactly as uncancellable as they are today — with none of the
stalls, because the lease and the lock-free `record` are what those depended on.

### 3.5 The git working tree

`git.Sourcer` keeps one clone per application at `<root>/<app>` and mutates it
with `Checkout(Force)` + `Clean(Dir:true)`. Two concurrent `Fetch` calls for one
name interleave a checkout with another's render, and the deployed manifest can
be a mixture of two commits that never existed in git.

The lease makes that unreachable on the normal path — but only while `Remove`'s
drain succeeds. Under D3's bounded wait it can time out, and then the old
application's sync is still running when its successor starts.

So the lock belongs in `git.Sourcer`, keyed by application name, not in the
reconciler. It is the package's own invariant ("one clone per application", its
words) and it holds however the caller behaves. Fifteen lines:
a `map[string]*sync.Mutex` guarded by a mutex, taken for the whole of `Fetch`.

### 3.6 Panic isolation (#105)

The nil guard is merged. What remains is one decision and one small mechanism.

A panic becomes an ordinary reconcile failure, in one place — inside
`reconcile()`, which is the single funnel both `Sync` (the loop) and `SyncNow`
(the API) pass through:

```go
defer func() {
	if p := recover(); p != nil {
		err = fmt.Errorf("panic reconciling %q: %v", app, p)
		r.log.Error("panic reconciling an application", "application", app,
			"panic", p, "stack", string(debug.Stack()))
	}
}()
```

The existing machinery then does the rest with no new state: `setError` records
it on that application, the loop counts it as a failure, and backoff doubles to
30 minutes. A deterministic panic therefore degrades to one stack trace every 30
minutes on one application, and self-heals when the cause is removed — rather
than restart-looping the process.

Whether `appset.Loop.Run` and the three peer goroutines in `controller/run.go`
get the same treatment is decision **D4**.

### 3.7 Shutdown

Three changes, all in `controller/run.go` and `api/`:

1. **Order.** Drain the listener *before* cancelling the reconciler, so a
   `POST /sync` cannot arrive in the gap and spawn work after the reconciler has
   provably stopped. Any peer exiting still stops the others; only the order of
   the two shutdown phases changes.
2. **Bound the reconciler drain.** `Run` currently does an unbounded
   `r.wg.Wait()`. It gains a deadline and, past it, logs which applications are
   still syncing and returns. Swarm SIGKILLs 10s after SIGTERM
   (`stack.yml` sets no `stop_grace_period`), so the two phases share that
   budget: **2s** for the HTTP drain and **5s** for the reconciler, leaving
   headroom. The HTTP phase can drop from 5s to 2s precisely because of (3).
3. **SSE.** `httpSrv.RegisterOnShutdown` closes every subscriber channel, so the
   stream handlers return and `Shutdown` finishes at once. Today an SSE
   connection never goes idle, so every shutdown with a subscriber burns the
   full timeout and then logs a false "the API did not shut down cleanly".

And the `wg.Add`/`Wait` race: `Run` takes and releases `r.mu` between
`<-ctx.Done()` and `r.wg.Wait()`. That is the happens-before edge — an `Add`
already past the guard has completed its `wg.Add` before releasing `mu`, and one
that has not will find `r.root.Err() != nil` and refuse.

## 4. Decisions — settled 2026-07-31

Recorded as answered, with the reasoning kept so a later reader sees what was
traded away rather than only what was chosen.

- **D1 — cancellation reaches the daemon by widening CE's `charts.Backend`**,
  not by a decorator here. Blast radius measured: `swarmcli-be` does **not**
  implement the interface (its `DeployStack` is `docker.DeployStack`, a
  different function), so it is CE's `charts/` package plus this repository.
  CE is pre-`v1.13.0`-stable and this module already pins a *pseudo-version*
  rather than a tag, so no CE release is needed. `DeployStackInContext` and
  `RemoveStackCLIInContext` shell out through `exec.Command`, so this also makes
  a hung `docker stack deploy` killable for the first time.
- **D2 — `POST /sync` collapses to one pending.** Still 202; at most one queued.
- **D3 — `Remove` waits with a bound**, and a timeout is reported as a held
  prune rather than a stall. The git working-tree lock (§3.5) is what makes the
  bound safe.
- **D4 — panic isolation covers the per-application funnel and
  `appset.Loop.Run`.** The three peers in `controller/run.go` keep dying: a
  controller that cannot serve or cannot reconcile should restart rather than
  keep answering `/healthz` while lying about it.
- **D5 — the interval floor is 10s, rejected by `validate` and clamped at
  runtime.** A bad interval must not stop the whole app set from updating.
- **D6 — four PRs, none blocked on another.**

| PR | repo | files | gate |
| --- | --- | --- | --- |
| A | `Eldara-Tech/swarmcli` | `charts/`, `docker/` | — |
| B | `swarmcli-cd` | `reconcile/`, `git/`, `api/api.go`, `appset/` | — |
| C | `swarmcli-cd` | `go.mod`, `backend/` | after #103 merges **and** A lands |
| D | `swarmcli-cd` | `controller/run.go`, `api/stream.go`, `config/` | — |

B is useful without A: the lease is what makes cancellability reach anything at
all, and until C moves the pin the four CE methods simply stay uncancellable —
exactly today's behaviour, with none of today's stalls.

### The reasoning, kept

**D1 — how far cancellation reaches.** §3.4. A (decorator here, ship now,
`waitReady` residual), B (widen CE's interface, cross-repo), or A now and B as a
CE issue. *My recommendation: A plus a CE issue.* A is sufficient for every
failure mode #106 names and uses an idiom already present three times in this
file; B is right at the root but is a release-train decision, not a code one.

**D2 — `POST /sync` while a sync is running.** Today: unbounded, undeduplicated,
all N return 202 and serialise. Options: 409 Conflict (honest, changes the
contract — a UI's sync button starts failing); collapse to one pending (keeps
202, at most one queued, "your request will be honoured" stays true); leave it.
*My recommendation: collapse to one pending.*

**D3 — `Remove` when a sync will not stop.** Unbounded wait is correct and can
stall the whole app-set pass for minutes behind CE's uncancellable `waitReady`.
Bounded wait converts that into a *held prune* — a state `appset.Loop` already
models and reports (`pruneHeldBy`) — at the cost of `Remove` returning an error
the loop must handle. *My recommendation: bounded, because the prune gate makes
it safe and the alternative stalls every other application's membership change.*

**D4 — how wide panic isolation goes.** The per-application funnel is not in
question. The question is `appset.Loop.Run` (recovering keeps the last-good set
reconciling, which matches this loop's stated philosophy) and the three peers in
`run.go` (recovering there means a controller that has lost a limb keeps
answering `/healthz`, which may be worse than dying). *My recommendation:
recover in `appset.Loop.Run`; let the `run.go` peers die, because a controller
that cannot serve or cannot reconcile should restart rather than lie.*

**D5 — the interval floor, and how it is enforced.** Reject at config load
(loud, but a bad interval then blocks the *whole set* from updating); clamp with
a warning at use (never refuses a set); or reject in the offline `validate`
command and clamp at runtime. *My recommendation: the third — the validator is
where an operator finds out, and the running controller never refuses a set over
it.* The floor value is also yours; I propose **10s**.

**D6 — scope and PR shape.** Everything above is one coherent change to
`reconcile` plus four independent small ones (shutdown order, SSE drain,
`wg` race, interval floor). CI only runs on PRs based on `main` and stacked PRs
get none. *My recommendation: two PRs, both based on `main` and touching
disjoint files — (1) `reconcile`/`git`/`api` lifecycle + panic isolation,
(2) `controller/run.go` + `api/stream.go` + `config` shutdown and bounds.*

## 5. What this design does not fix

Stated so it is not read as covered:

- **CE's `waitReady` still polls uncancellably.** Under D1-A a cancelled sync
  returns when its own timeout expires, not when it is cancelled. The bounded
  drain is what keeps that off the shutdown path.
- **N full swarm snapshots per reconcile** (§2). Moving the read out of the lock
  removes the global stall, not the cost. Proposed as its own issue.
- **`View`/`Views` alias their backing array and pointers** (#106 item 5). Real,
  currently harmless, and a one-line `slices.Clone` — but it belongs with a
  consumer that needs it rather than with this.
- **Nothing here was run against a Docker daemon.** There is none in this
  environment; `go vet -tags integration ./integration-tests/` compiles the suite
  and never runs it. Anything daemon-facing is first-run-in-CI.

## 6. Verification

Each fix is proved load-bearing by mutation: revert the hunk, confirm the named
test fails, restore. A test that does not fail under its own mutation has not
done its job.

| fix | test | mutation that must break it |
| --- | --- | --- |
| lease tracks API syncs | `Remove` returns only after a detached `SyncNow` has returned | drop the `drain` call |
| entry context reaches an API sync | a detached sync sees its context cancelled by `Remove` | drop the `AfterFunc` link |
| identity not name | a departed entry's late `record` does not overwrite a returned name's status | restore the name lookup |
| `record` outside `r.mu` | a `Views()` call completes while a release read is blocked | restore the read inside the lock |
| git tree lock | two concurrent `Fetch` calls for one app serialise (`-race`) | remove the lock |
| panic isolation | a panicking fetcher fails one application; the others keep reconciling | remove the `recover` |
| manual syncs collapse | a third request returns `ErrSyncPending` | let the slot overflow |
| shutdown drain is bounded | `Run` returns despite an uncancellable sync | make the wait unbounded |
| the sweep waits for a departure | a still-syncing departed app holds the prune | stop consulting `Draining` |
| app-set panic isolation | a panic fails the pass, not the process | remove the `recover` |

Results, PR-B (each mutation applied, the named test observed failing, then
restored — full output in the PR):

- **Remove does not drain** → *"Remove returned while a sync it did not start was still running"*, and `Draining = []` where `[edge]` was wanted.
- **`acquire` does not link the entry context** → *"Remove did not cancel the context of a sync started through the API"* after 5s.
- **identity reverted to a name lookup** → *"state = \"synced\", want unknown: the departed application's reading was written onto its successor"*.
- **`record` back inside the exclusive lock** → *"a status read blocked behind the swarm read record was doing"* after 5s.
- **no `recover` per application** → the test process dies on the panic.
- **no sync dedup** → *"AcceptSync = <nil>, want ErrSyncPending"*.
- **unbounded drain** → *"Run did not return: the drain waited for a sync that cannot be cancelled"*.
- **no git tree lock** → *"a second Fetch for one application ran while its working tree was being rewritten"*; and with **one global lock instead of one per application** → *"a second application's fetch queued behind the first application's working tree"*.
- **sweep stops consulting `Draining`** → *"swept while core was still syncing (1 calls); that deletes what it is deploying"*.
- **no `recover` in the app-set pass** → the test process dies on the panic.
- **coalescing not reported** → the coalesced request 404s instead of 202.

One fix is **not** mutation-proven and is called out as such: the `wg.Add`/`Wait`
race in `Run`. It is a race with no deterministic trigger — the failure is a
runtime panic in a window of a few instructions — so what stands behind it is the
argument in the code comment and a clean `-race` run, not a test.

The remaining rows belong to PR-D:

| fix | test | mutation that must break it |
| --- | --- | --- |
| shutdown order | a `POST /sync` after shutdown begins is refused, not spawned | restore the original order |
| SSE drain | shutdown with a subscriber returns well under the timeout | drop `RegisterOnShutdown` |
| interval floor | `interval: 1ms` is clamped and warned about | remove the clamp |
