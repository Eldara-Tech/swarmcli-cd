<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Running it in production

Day-two operations: upgrading, restarting, what is state and what is cache, what
to back up, and what to alert on. [Getting started](getting-started.md) gets a
controller running; this page is about keeping one.

## Upgrading the controller

Pin a version. `stack.yml` ships `:latest` so that the quickstart works with no
edit, and a deployment you care about should not leave it there — an upgrade
that happens because a node restarted is one you did not choose the moment for.

```bash
docker service update --image eldaratech/swarmcli-cd:<version> swarmcli-cd_controller
```

Pick the tag from [the releases page](https://github.com/Eldara-Tech/swarmcli-cd/releases).
`<version>-oss` is the wholly Apache-2.0 artefact; the unsuffixed tag is the
default one — see [editions](editions.md). Downgrading is the same command with
an older tag.

**Nothing you have deployed is touched by the upgrade.** The running task stops,
a new one starts, and reconciling resumes. What you lose is the gap: the API is
unavailable for a few seconds, and any application whose interval fell inside it
reconciles late rather than not at all. A rollout that was in flight is not
interrupted either — Swarm is the thing performing it, including any
`failure_action: rollback` — but the controller stops *watching* it, so an
`app sync --wait` running at that moment reports a failure it cannot distinguish
from the real thing.

Two things do change with the binary, and both are worth reading the release
notes for.

**The chart engine version** is stamped into each build from the swarmcli
release it pins, and it is what decides whether a chart declaring
`swarmcliVersion: ">= 1.13.0"` may be applied. An upgrade can therefore make a
previously-refused release applicable. It cannot do the reverse silently: a plan
containing a release this engine is too old for is [refused
outright](concepts.md#chart-compatibility), recorded on the application's
status, rather than half-applied.

**The applications file is decoded strictly**: a field the binary does not know
is an error, not a shrug. So an app set using anything a newer controller
introduced is *refused* by an older one, which is what a downgrade means in
practice. `swarmcli-cd validate --file applications.yaml` run with the binary you
are moving *to* answers that before the deployment does — it needs neither a
controller nor a swarm.

## Restarting, and what survives one

A restart is cheap and is the documented remedy for two things — a
[renewed TLS certificate](configuration.md#tls) and a
[newly installed licence](editions.md#what-a-licence-changes-and-when). It is
worth knowing exactly what it costs.

**On the swarm, and therefore permanent:** every release's stored revision, its
rendered manifest and its [owner stamp](concepts.md#ownership). That is what
makes prune survive a restart at all — a controller that comes up and finds a
release stamped for one of its own applications that the app set no longer
declares knows the application departed, though it never watched it leave. It is
also why there is no database to back up.

**In the controller's memory, and therefore not:**

- Each application's observed sync and health. Both read `unknown` until that
  application's first pass completes, which is one interval at worst.
- The `orphaned` and `pruned` name lists on `swarmcli-cd status`. A gap in the
  reporting rather than in the cleanup: with prune enabled the departure is
  rediscovered from the owner stamps.
- Any open event-stream connection. Clients reconnect.
- [SSO sessions](sso.md#sessions), on a licensed deployment. A restart signs
  every browser out.

**On the data volume, and therefore only as durable as the volume:** repository
clones and the chart cache — see below.

## The data directory is a cache

`--data` (default `/var/lib/swarmcli-cd`) holds three things: `repos/` for the
clones, `charts/` for the chart cache, and `appset/` for the app set in path
mode. Losing it costs a re-clone of every application and nothing else, which is
why it is a plain named volume in `stack.yml` rather than anything replicated.

Two consequences follow, and neither is a fault to investigate:

- The volume is **node-local**. `stack.yml` constrains the controller to a
  manager but does not pin it to one, so a task rescheduled onto a different
  manager starts with a cold cache and re-clones. Expected, slower, harmless.
- In **path mode** (`--appset-dir`) the app set itself lives on that volume,
  written by a git-sync sidecar. A fresh volume there means the controller
  starts with **no applications** until the sidecar has run. That is the
  deliberate non-fatal path — exiting instead would restart-loop the controller
  and take down the API that could have explained it — and it is reported by
  `swarmcli-cd status`. See [configuration § startup, when the set is not there
  yet](configuration.md#startup-when-the-set-is-not-there-yet).

An application that leaves the app set has its clone and chart cache reclaimed a
whole app-set interval after it goes, so nothing racing the removal reads a
deleted tree.

## What to back up

In descending order of how much it matters, and none of it is a swarmcli-cd
format:

1. **The git repositories.** They are the desired state. A controller and a
   swarm are both reproducible from them; nothing else here is.
2. **The bootstrap**, which is deliberately outside git and cannot be
   reconstructed from it: the `swarmcli-cd-applications` config (static mode
   only), the admin-token secret, the git-credential secret, any `registryAuth`
   secrets, the TLS pair, the OIDC client secret on a deployment using
   [SSO](sso.md), and the `swarmcli-license` config or secret on a licensed one.
   These are the anchor that says which repository is authoritative, and
   [nothing in the app set can repoint it](configuration.md#the-trust-boundary).
3. **The swarm's raft store**, by Docker's own manager backup procedure
   (`/var/lib/docker/swarm` on a manager). It holds the release records, the
   revision history behind `app history`, and the owner stamps.

Losing (3) but not (1) and (2) is survivable: redeploy the stack and the
controller re-installs every release from git. What you lose is history —
`app history` starts over, and the first sweep with `--prune` on sees releases it
has no stamp for, which it reports as **unmanaged** and never touches. That is
the safe direction, and cleaning them up is a manual act.

Losing the data volume is not a loss. See above.

## What to watch

**There is no `/metrics` endpoint.** The controller exposes its state through
three things instead, and which one to use depends on what you are asking.

**`/healthz` is liveness and nothing more.** It answers "something is
listening", takes no credential, and is what the container healthcheck probes.
It is deliberately *not* tied to the app set: a controller pointed at an
unreachable repository is a healthy process with a real problem, and making
`/healthz` fail would have Swarm kill and restart a task that would have come
back to exactly the same state.

**So the thing to alert on is `swarmcli-cd status`** — but on its *output*,
not its exit code. Reads report and checks judge: `status` exits 0 whenever the
controller answered, including when what it answered is that the app set is
stale.

```bash
# The app set is not the one you pushed.
swarmcli-cd status -o json | jq -e '.appSet.stale == false' >/dev/null \
  || echo "swarmcli-cd: the running app set is stale"

# Nothing has ever loaded — a permanently wrong --appset-repo looks like this.
swarmcli-cd status -o json | jq -e '.applications > 0' >/dev/null \
  || echo "swarmcli-cd: no applications are being reconciled"
```

`.appSet.error` carries the reason in both cases. The full field-by-field
meaning is in [api § status](api.md#status--get-apiv1status).

**Per application**, `app list -o json` is one object per application with both
axes on it:

```bash
swarmcli-cd app list -o json \
  | jq -r '.applications[] | select(.status.sync.state != "synced" or .status.health.state != "healthy")
           | "\(.spec.name) sync=\(.status.sync.state) health=\(.status.health.state)"'
```

Watch **both** axes. They are independent on purpose: a stack can be perfectly
synced and badly degraded, and alerting on sync alone misses every runtime
failure while alerting on health alone misses every commit that never landed.
`unknown` is a third thing again — the swarm was not readable, or the
application has not had its first pass yet — and treating it as failure will
page you on every restart. See [concepts § drift, sync and
health](concepts.md#drift-sync-and-health).

An application whose last reconcile *failed* keeps reporting its last successful
observation, so a stale verdict beside a fresh timestamp is possible; the CLI
marks those rows with a `RECONCILE` column and the JSON carries the error.

**For anything continuous**, `GET /api/v1/events` is a server-sent event stream
of reconcile events, which is what the web UI consumes instead of polling. A
[`notify` seam](extensibility.md) exists for pushing the same events outward;
the OSS build logs them, and Slack and webhook notifiers are a licensed
capability.

**Exit codes are a verdict on exactly three commands** — the ones that check
rather than report:

| Command | Non-zero when |
|---|---|
| `swarmcli-cd healthcheck` | the controller did not answer `/healthz` |
| `swarmcli-cd validate --file applications.yaml` | the file is not a valid app set. Needs no controller and no swarm, so it belongs in the app-set repository's CI |
| `swarmcli-cd app sync <app> --wait` | the rollout did not converge |

`app sync --wait` is the one to reach for in a smoke test or a deploy pipeline:
it blocks until the rollout settles and fails loudly if it did not.

## Logs

Everything goes to **stderr**, through one handler, in one format — including
what the libraries the controller is built on write. `docker service logs
swarmcli-cd_controller` is the whole of it. `--log-format json` is the option
for anything that parses rather than reads. The levels are worth calibrating an
alert against:

- **`ERROR`** — a reconcile or a component failed.
- **`WARN`** — something you must see that did not stop the loop: a prune held,
  a resource left behind, an application leaving the set, a public route
  registered by a companion module.
- **`INFO`** — lifecycle and events, including the `seams` and `routes` lines
  every controller logs at startup. Those two are worth reading once after every
  deploy: they say which implementation is behind each seam and every path this
  build actually serves.

Full detail, including why the text format quotes the way it does, is in
[configuration § logs](configuration.md#logs).

## Two controllers, one swarm

Supported, and it needs one deliberate act: **give each a distinct
`--controller-id`.** Every release is stamped `cd/<controller>/<application>`
and each sweep only considers releases stamped for itself, so two controllers
left on the default id each read the other's applications as departed — and with
`--prune` on, delete them. The controller cannot detect this from the inside.
See [configuration § two controllers on one
swarm](configuration.md#two-controllers-on-one-swarm).

This is also why the stack runs `replicas: 1`. A second replica is a second
controller with the *same* id, applying the same applications on its own
schedule.

## See also

- [Getting started](getting-started.md) — the end-to-end walkthrough
- [Configuration](configuration.md) — every flag, field and environment variable
- [Concepts](concepts.md) — sync versus health, drift, ownership, rollback
- [Editions](editions.md) — the two artefacts, and what a licence changes
- [HTTP API](api.md) — the endpoints behind every command


## Cross-repo and release mechanics

### Landing a feature that needs a CE change

This repo depends on CE as a **versioned module** — no `replace`, no sibling
checkout — so a feature needing a CE change cannot compile until CE publishes.
The sequence that works, done end to end for sync waves (CE#541 → v1.13.0-rc7 →
CD#156):

1. Branch-pin CD to the CE **branch** so CI can run at all.
2. Merge the CE change.
3. Tag a CE RC.
4. Re-pin CD to the tag and merge.

`go get` against a branch SHA bloats `go.mod` with transitive requirements;
`go mod tidy` after re-pinning is part of step 4, not an afterthought.

### `go:embed`'d build output does not travel in a module zip

`web/dist/*` is gitignored except a committed `.gitkeep`, so `//go:embed
all:dist` compiles without a Node toolchain. The module zip therefore carries
**one 0-byte file**, and any module depending on this one and building
`cmd/swarmcli-cd` embeds that sentinel — a right-sized binary that serves
`web.notBuilt`, with nothing failing until a browser asks.

A consuming build must build the UI itself and cannot rely on the dependency
having shipped it. The release workflow guards this explicitly; keep that guard.

### A workflow's env derivation must not be hand-copied into prose

`.goreleaser.yml` derives the CE module path from `go.mod` rather than hardcoding
it, and required `{{.Env.ENGINE_PKG}}` — which goreleaser refuses when the key is
absent. The same commands were hand-written in `RELEASING.md`, whose
local-snapshot recipe still exported only `ENGINE_VERSION`, so the documented
release broke while CI stayed green (fixed in swarmcli-cd#251).

**A code block in a release doc is a second implementation that CI never runs.**
When the workflow's derivation changes, either change both or make the doc invoke
the script.

### Do not pin a chart's image tag with `--set`

A chart whose `image.tag` defaults to `.Chart.AppVersion` re-renders the image
from `appVersion` on every self-apply, and `appVersion` names the last **stable**
release. An RC install recipe using `--set image.tag=<rc>` therefore holds only
until the controller first reconciles itself — after which it replaces the RC
with the stable version and, if that version cannot decode the current app set,
refuses the whole file.

The pin belongs in the values file the release actually commits, not on the
command line.

### Reading a pasted log: change-only lines date the failure

`app set loaded` sits behind `if changed` (`appset/loop.go`), and `application
changed in the app set` fires only on whole-value inequality. Both are therefore
*edges*, not heartbeats — which makes them the way to date a report. If they
appear after the error in a paste, the config arrived after the failure and the
reconcile that actually tested it starts one line past the end of what you were
given. Ask for the next few lines rather than diagnosing the ones you have.

### A patch parked in an issue rots against HEAD

A patch — or a `file:line` — quoted in an issue is a claim about a tree that has
since moved. swarmcli-cd-be#17 and #19 both carried one, and additionally recorded
"the decision, so it is not re-derived later". When the pin finally moved, the
work was redone from the code, the decision **was** re-derived, and it came out
differently: `NotActivated` shipped as `StatusAbsent`, not the `StatusInvalid`
both patches wrote.

Diff a parked patch against HEAD before applying it, and re-read the
"decided, do not re-derive" section too — it is a claim about a tree as much as
the patch is.

### Settling what Swarm does to a spec

Read the daemon, do not guess and do not wait for CI.
`github.com/docker/docker@vX+incompatible` in `$(go env GOMODCACHE)` ships the
**whole daemon**, not just the API types, so `daemon/cluster/convert/*.go` is the
authoritative answer for what a `ServiceSpec` looks like on the way back.
`github.com/moby/swarmkit/v2` is there too. Three premises in swarmcli-cd#76 were
wrong and reading these settled all of them in minutes.
