# swarmcli-cd

GitOps controller for Docker Swarm — "Argo CD for Swarm". Public, Apache-2.0.
The entry point is the top-level `controller` package; `cmd/swarmcli-cd` is a
one-line `main` so that the private companion can build the same binary from a
main.go differing only by its blank imports.

**Read [issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1) before
designing anything here.** It is the source of truth for positioning, decisions
D1–D6, the Swarm constraints that shape the design, and the phase plan. Do not
re-derive them.

## Quick reference

```bash
go build -o swarmcli-cd ./cmd/swarmcli-cd
go test ./...
golangci-lint run ./...
./scripts/check-spdx.sh          # CI enforces this on every .go and .sh
```

## Conventions

- **Module path** is `github.com/Eldara-Tech/swarmcli-cd`. It depends on CE as a
  normal versioned module — **no `replace`, no sibling checkout**. That is
  deliberate: swarmcli-be pays the sibling-checkout tax, and renaming CE's
  module (Eldara-Tech/swarmcli#476) existed precisely so this repo would not
  have to. Ask `go.mod` what the pin is rather than trusting a version written
  down anywhere, including here:
  ```bash
  go list -m -f '{{.Version}}' github.com/Eldara-Tech/swarmcli
  ```
  A pseudo-version is the normal state between CE releases and builds fine;
  what it costs at release time is in [RELEASING.md](RELEASING.md).
- **SPDX header** on every `.go` and `.sh` file, or `licence.yml` fails:
  ```go
  // SPDX-License-Identifier: Apache-2.0
  // Copyright © 2026 Eldara Tech
  ```
  `goheader` in `.golangci.yml` enforces the same thing at lint time.
- **PRs need one label from each of A/B/C** (`check_labels.yml`), mirroring the
  other code repos. Add all three in **one** REST call:
  ```
  gh api -X POST repos/Eldara-Tech/swarmcli-cd/issues/<n>/labels \
    -f 'labels[]=A3-technical' -f 'labels[]=B0-low-priority' -f 'labels[]=C0-breaks-nothing'
  ```
  Even so, an early run can evaluate an incomplete label set and **fail** (not
  merely get cancelled), stranding the PR at `mergeable_state: unstable`. The PAT
  cannot re-run jobs (403); re-fire the event by toggling one label off and on.
- **Push via `originToken`**, never `origin` (SSH, broken). Commit as
  `eldara-cruncher <hello@eldara.io>`, matching the other code repos.
- `gofmt` is the only formatter; import grouping is not linted.

## Logging

Everything goes through **one** `log/slog` handler on stderr, built in
`runController` from `--log-level` (debug|info|warn|error, default info) and
`--log-format` (text|json, default text), and installed with
`slog.SetDefault`. Components take a `Log` option; `SetDefault` is what covers
the ones that cannot — `notify`'s log notifier registers from an `init()` where
no logger exists yet, and any dependency reaching for `slog.Default` or the
standard `log` package lands in the same stream rather than printing a second
format alongside it. That was issue #70; do not reintroduce a second handler.

Flags, not environment variables: only credentials come from the environment
here, for the reason `controllerUsage` gives.

`text` is logfmt, which quotes any value containing a space and escapes the
quotes inside it. **So a message names things in `'single quotes'`, not with
`%q`** — every error and status string here ends up as a logfmt value sooner or
later, and `%q` puts the one character the encoding has to escape into the
middle of it. `\"` all through a line an operator is reading is the format
working, but it is our `"` it is escaping; single quotes cost the encoding
nothing and read the same. That is issue #157, and it is why this repository
diverges from CE, which uses `%q` throughout.

This does not make the log backslash-free, and should not be sold as if it did:
a wrapped error keeps its author's quoting, and `net/http` renders a URL as
`Get "…"`, so an unreachable repository still arrives escaped. What the rule
buys is that `\"` around something *this* controller is reporting is now a bug
rather than the normal state.

`%q` also escaped control characters, and dropping it does not reopen anything:
on the log path slog escapes them whatever the message did, and on the terminal
path the surrounding text was never escaped anyway — `app.go` prints a status
`Message` with `%s`, and those messages already embed Docker task text
(`health.go`) and daemon reasons (`reconcile.go`) verbatim. Quoting the name
while interpolating the daemon's sentence raw was not a defence.

`--log-format json` is still the answer for anything that parses rather than
reads; note it escapes `"` exactly as logfmt does, so it was never the fix for
this.

Levels: `Error` is a reconcile or a component failing, `Warn` is something an
operator must see but that did not stop the loop (a prune held, a resource left
behind, an application leaving the set), `Info` is lifecycle and events.

The CE packages this repository imports log through CE's own zap-based
`swarmlog`, which is a no-op until initialised. `runController` hands it the
same handler via `swarmlog.InitSlog`, so their lines arrive in this format on
this stream rather than being discarded (issue #72) or opening a rotating file
in the container. If the controller starts importing a CE package that logs
more heavily, that is the knob — not a second handler.

The other repos meet the same contract with different libraries, deliberately:
swarmcli-agent uses slog configured by `AGENT_LOG_*` environment variables (it
parses no flags), and swarmcli-rbac-proxy uses zap. CE and BE stay on zap with
lumberjack file rotation, because a TUI cannot log to the terminal it owns.

## Layout

```
cmd/swarmcli-cd/   one-line main; the entry point is controller/
controller/        entry point, command dispatch, daemon wiring, CLI rendering
client/            HTTP client for the API; the CLI goes through it, not the
                   reconciler, so a UI can do everything the CLI can (D3)
api/               the HTTP server: the endpoint set and the SSE event stream
application/       Application spec + status: the wire contract
config/            reads and validates the applications file
appset/            where that file comes from — mounted, git, or a directory
                   something else keeps current — and the loop re-reading it

reconcile/         the pull loop: one goroutine per application, fetch → render
                   → plan → apply
git/               fetches a repository and pins it to a commit, on go-git
                   rather than a git binary
source/            a checked-out tree → what the chart engine's PlanApply takes
compose/           a rendered manifest → Swarm specs; the half of
                   charts.Backend that needs no daemon
backend/           applies those specs through the moby client — the half that
                   needs one
capability/        interfaces only — what a backend may optionally implement
                   beyond charts.Backend, exported so a Phase 3 remote one can
                   be written and compile-checked against them
health/            whether what is running actually works — the axis sync does
                   not answer
drift/             whether the swarm matches git: drift.go is manifest mode,
                   live.go the ServiceSpec comparison
prune/             deletes what git no longer declares, and the ownership rules
                   deciding what may be deleted at all
reclaim/           deletes the on-disk caches — clone and chart cache — of an
                   application that has left the set, a whole interval after it
                   did, so nothing racing the removal reads a deleted tree
regauth/           per-application registry credentials, from Docker secrets

seam/              the init()-registration mechanism the five seams share (D6)
swarms/            seam — which swarm a destination resolves to
swarms/local/      its OSS default, blank-imported by cmd/ so that the seam
                   stays a contract and importing it does not link backend/
authz/             seam — who is calling the API and what they may do
notify/            seam — reconcile events out; appends rather than replaces
secrets/           seam — secret material read from an application's own tree
extension/         seam — HTTP routes a companion adds to the API; appends, and
                   the only seam with no OSS default, because no route is the
                   right behaviour for a build with no companion loaded

Dockerfile         alpine, no docker binary; stamps both version ldflags
stack.yml          the in-swarm deploy: manager node, docker.sock, config+secret
examples/          a commented applications.yaml, a quickstart repo, an app-set
                   repo
integration-tests/ `-tags integration`, against a real swarm
scripts/           check-spdx.sh
docs/
.github/workflows/ ci.yml, check_labels.yml, licence.yml, integration-tests.yml,
                   release.yml
```

Packages land with the code that needs them rather than as empty directories,
so this grows one issue at a time — and this list is the thing that goes stale
when one lands, so add it here in the same PR.

**Do not put them under `internal/`.** Issue #1 sketches an `internal/` tree
(`app`, `git`, `render`, `reconcile`, `health`, `swarms`, `store`, `api`,
`audit`), but that is incompatible with D6: the private `swarmcli-cd-be`
companion is a separate *module*, and Go's internal rule is per-module, not
per-repository — it could not import any of it. This is exactly why
`Eldara-Tech/swarmcli` has no `internal/` directory anywhere, and why
swarmcli-be imports `app`, `views`, `registry` and `docker` as top-level
packages. Anything the companion touches — the seams, the core types, the
reconciler, the API — is top-level here too.

## Design constraints worth not rediscovering

Both are established, with reproductions, in issue #1:

- **`docker stack deploy` cannot be the applier.** Its `--prune` removes services
  only and swallows its own list error, networks are silently never updated,
  there is no dry-run, `--detach=true` returns before convergence, and update
  order is Go map iteration. The applier is built on docker/cli's *exported*
  `cli/compose/{loader,schema,convert}` plus `github.com/moby/moby/client`.
- **Live drift is diffed at `ServiceSpec` level, not YAML.** `compose →
  ServiceSpec` is lossy and one-way, so diffing against
  `docker.ReconstructStackCompose` manufactures differences that do not exist.
  That function is for human-readable display only.

## Open-core seams

Per D6 the private `swarmcli-cd-be` companion is deferred to Phase 3, but the
**seams ship from day one**: `SwarmRegistry`, `Authorizer`, `Notifier`,
`SecretProvider` and `Extension`, each replaced via Go `init()` self-registration
(the mechanism swarmcli-be already uses — see its `docs/extensibility.md`). No
build tags, no stubbed files in the public tree.

The first four have a working OSS default. `Extension` does not, and that is the
one place the rule bends rather than breaks: it *adds* routes rather than
replacing a behaviour, so the empty list is already the correct OSS build, and a
default would be a route nobody asked for. A seam that replaces something must
still ship a default — the step exists so a `Slot` never hands its consumer a nil
implementation, and a `List` has no such value to hand out.

The companion does not exist yet, and that is the whole of the deadline: every
parameter list a companion implements is frozen the day it ships, and every
struct field stays free forever. **So take parameters as structs from the first
commit** — `swarms.Target`, `swarms.Node`, `secrets.Request`, `authz.Subject`,
`notify.Event` all exist for that reason. Where a capability may genuinely be
absent, state it as an optional interface the caller type-asserts for rather
than as a method on the seam, so a companion implementing one thing does not
have to stub four (`swarms.Lister`, `swarms.NodeReach`). `docs/extensibility.md`
is the fuller version, including what a companion still cannot do.
