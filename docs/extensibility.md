<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Extensibility: how the private companion replaces a seam

swarmcli-cd is open core. This repository is the whole product — reconcile,
diff, health, API, CLI and, later, the web UI — and a private `swarmcli-cd-be`
companion replaces four specific behaviours with licensed ones. Per D6 the
companion arrives in Phase 3, but the seams ship from day one so that nothing
in the public tree has to move when it does.

The mechanism is the one `swarmcli-be` already uses against `swarmcli`: `init()`
self-registration and a blank import. **No build tags, and no stubbed files in
the public tree.** Every default here is a working implementation, not a panic
waiting for a licence.

## Why it works

Go initialises an imported package before the package that imports it. A seam
package registers its OSS default in its own `init()`; a companion package
registers its replacement in *its* `init()`, which necessarily runs later. The
companion always wins, and nothing has to coordinate the order.

**That argument covers three of the four.** It holds because the default lives
in the very package the companion must import, which is what makes "later"
guaranteed. `swarms`' default does not — it is in `swarms/local`, so that the
seam does not drag the Docker applier in behind it — and two unrelated sibling
packages are initialised in import-path order, which neither of them controls.
Under that order `…/swarmcli-cd-be/swarms` runs *before*
`…/swarmcli-cd/swarms/local` (`-` sorts before `/`), so last-wins would have the
OSS default overwrite the companion, silently, with `swarms.Active()` reporting
`local` as the only evidence.

So `swarms/local` registers **only if nothing has**, which is order-independent
and makes "the companion always wins" true unconditionally. A default outside
its seam package must do the same; one inside it never needs to.

## The four seams

| Package | Interface | OSS default | Replaced by |
|---|---|---|---|
| `swarms` | `Registry` | `local` — the swarm the controller runs in | multi-swarm through swarmcli-rbac-proxy |
| `authz` | `Authorizer` | `token` — one shared bearer token | SSO, projects and RBAC |
| `notify` | `Notifier` | `log` — a structured line per event | Slack, webhooks, e-mail |
| `secrets` | `Provider` | `plaintext` — passes material through, refuses what it cannot decrypt | SOPS decryption |

Three of them **replace**: there is exactly one answer to "which swarm registry
is in force". `notify` **appends**, because a companion adding Slack must not
remove the log notifier — and because the HTTP API's own event stream is itself
a registered notifier, so replacing would silently kill the UI's live updates.

`swarms`' default is the one that is not in its own package: it lives in
`swarms/local` and the binary blank-imports it, so that `swarms` stays a
contract and nothing importing it links the Docker applier. A build that takes
neither the default nor a companion registers `none`, which says so in the
startup line below and errors naming the missing import.

### Optional interfaces beside a seam

A companion does not have to answer everything at once. Where a capability may
genuinely be absent, the seam states it as a separate interface and the caller
type-asserts for it — the same shape the reconciler uses for backends that may
or may not be able to read a `ServiceSpec`. Implement it and the feature works;
do not, and the caller degrades rather than failing.

| Interface | What it adds | Why the OSS default does not implement it |
|---|---|---|
| `swarms.Lister` | enumerate the swarms this registry resolves | there is one, and the caller is on it |
| `swarms.NodeReach` | a handle to each *node's* own daemon | a Swarm node does not expose its daemon; this controller holds one socket |

`NodeReach` is what makes a volume purge complete. A stack's ordinary named
volumes live on whichever nodes ran its tasks and the daemon's volume list
answers from the node's own store, so without it `prune` deletes what the
controller's node can see and says plainly that that was not all of them
([#108](https://github.com/Eldara-Tech/swarmcli-cd/issues/108)). Reaching the
other nodes is a deployment capability — operator-provisioned endpoints, a
per-node agent, a privileged global-mode service — before it is a code path,
which is why it is the companion's to solve and not a function the free build
is missing.

Its `NodeBackend` is deliberately not a `charts.Backend`: a worker node's
daemon cannot deploy or remove a stack, so handing back a deploy-capable handle
would promise what the node cannot do.

### The backend contract

A `charts.Backend` is the smallest thing that can deploy a stack. Everything the
reconciler and the sweep ask of a backend beyond that — read a `ServiceSpec`,
list what a stored revision declared, scope an image pull to one application's
credential, count the swarm's nodes — is an optional interface they type-assert
for, and all twelve of them are named in
[`capability`](../capability/capability.go). A Phase 3 remote backend implements
whichever it can answer; each one it leaves out costs that one feature and
nothing else.

They are exported, and in a package of their own, because a companion has to be
able to name them. Go interfaces are structural, so a backend in another module
*can* satisfy an interface it cannot name — but it cannot be compile-checked
against it, and since every one of these is an upgrade that falls back silently
when the assertion fails, the first evidence of a changed signature would be a
feature that had quietly stopped working. `backend.Backend` asserts all twelve at
compile time for exactly that reason, so the OSS backend and a companion one are
held to the contract the same way.

### When registration is over

A `Slot` is settled by the end of `init()`. Every replacement arrives by blank
import, so by the time `main` runs nothing is left to register and a consumer
may read `Get` once and keep the result — `api.New`, `reconcile.New` and
`prune.New` all do.

A `List` is not. The API server registers itself as a notifier during wiring,
long after every `init()`, so `notify.Dispatch` re-reads `All` on every event.

**Entitlement gating therefore belongs inside an implementation, not in
swapping a `Slot` at runtime.** A licensed authorizer whose entitlement lapses
must start refusing from the inside. Replacing it in the slot would be seen by
the consumers that re-read and not by the ones holding the old value — and the
ones that did see it would see it mid-flight, which for `swarms` means handing
a half-finished sync a different daemon.

## Writing a companion package

```go
// swarmcli-cd-be/swarms/register.go
package swarms

import (
    cdswarms "github.com/Eldara-Tech/swarmcli-cd/swarms"
    "github.com/Eldara-Tech/swarmcli/charts"
)

func init() { cdswarms.Register("rbac-proxy", &registry{}) }

type registry struct{ /* … */ }

func (r *registry) Backend(ctx context.Context, t cdswarms.Target) (charts.Backend, error) {
    // Resolve t.Swarm to a named Docker context and hand back a backend for
    // it. charts.NewDockerBackend(name) carries no process-global state — not
    // the client singleton, not the context lookup, not the snapshot cache —
    // which is what makes one process able to serve several swarms at once.
}
```

`Target` is a struct rather than a `swarm string` so that the seam can grow
without breaking this file. Every parameter list a companion implements is
frozen the day the companion ships, so each one is a struct: `Target`, `Node`,
`secrets.Request`, `authz.Subject`, `notify.Event`. Add fields freely; do not
widen a parameter list.

## The companion's main.go

The only file that differs between the two builds — blank imports and nothing
else:

```go
package main

import (
    "github.com/Eldara-Tech/swarmcli-cd/controller" // the entry point, see below

    _ "github.com/Eldara-Tech/swarmcli-cd-be/authz"    // SSO + RBAC
    _ "github.com/Eldara-Tech/swarmcli-cd-be/licence"  // entitlement gating
    _ "github.com/Eldara-Tech/swarmcli-cd-be/notify"   // Slack, webhooks
    _ "github.com/Eldara-Tech/swarmcli-cd-be/secrets"  // SOPS
    _ "github.com/Eldara-Tech/swarmcli-cd-be/swarms"   // multi-swarm
)

func main() { controller.Main() }
```

No `swarmcli-cd/swarms/local` here: the companion registers its own registry,
and importing the default beside it would only mean building a Docker client
nothing asks for. `cmd/swarmcli-cd` does take it, which is the one line beyond
the call that its `main` has.

That entry point is the top-level `controller` package, and `cmd/swarmcli-cd` is
otherwise the one-line `main` above without the blank imports — a `main` package
cannot be imported, so anything else would leave the companion with nothing to
call. The binary's own version is stamped into that package rather than into
`main`, so the companion stamps the same symbol with its own tag:

```
-X github.com/Eldara-Tech/swarmcli-cd/controller.version=<version>
```

## The companion's go.mod

```
module github.com/Eldara-Tech/swarmcli-cd-be

go 1.26

require github.com/Eldara-Tech/swarmcli-cd v1.0.0-rc2
```

**Nothing here may live under `internal/`.** Go's internal rule is per-module,
so a separate `swarmcli-cd-be` module could not import a single one of these
packages — which is the whole point of them. This is why `Eldara-Tech/swarmcli`
has no `internal/` directory either. Issue #1's sketch of an `internal/` tree
should be read as a list of packages, not of paths.

## Seeing which implementations are live

Each seam reports what registered, and the controller logs it at startup, so an
operator can tell from the logs whether the companion actually loaded rather
than inferring it from behaviour:

```go
log.Info("seams",
    "swarms", swarms.Active(),
    "authz", authz.Active(),
    "notify", notify.Active(),   // a list: every notifier is live
    "secrets", secrets.Active(),
)
```

## Adding a seam

Only when a behaviour genuinely has to differ between the editions. Each seam
is an interface a companion must keep implementing across releases, so the bar
is a licensed feature that cannot be expressed any other way — not a
configuration knob, and not somewhere a future feature *might* go.

1. Add a package under the repository root with the interface, the OSS default,
   and `Register` / `Get` / `Active` wrapping a `seam.Slot` (or a `seam.List`
   when several implementations should all run).
2. Register the default from the package's own `init()`.
3. Add it to the table above.

Take every parameter a companion implements as a struct, from the first commit.
The day the companion ships, every signature in this document is frozen.

## What a companion still cannot do

Recorded so that it is a known gap rather than a discovery.

**Add a route.** SSO needs a login and a callback endpoint, projects need
`/api/v1/projects`, and the `licence` package sketched above has no
registration point at all. `api.Options` has no mux and `controller` exports
only `Main()`, so none of them can be added without forking both. The design is
a fifth registration point rather than a fourth option field — a `seam.List` of
extensions, each route declaring the `authz.Action` the core guards it with,
with an explicit opt-out for the one shape that must be unauthenticated (an SSO
callback arrives without a credential this controller issued) and every
registered route logged at startup by name. A private module adding an
unauthenticated endpoint to a controller holding the Docker socket is the most
dangerous thing this mechanism permits, and it has to be visible in a running
controller's log rather than only in the companion's source.

It is the last open item of
[#111](https://github.com/Eldara-Tech/swarmcli-cd/issues/111), and deliberately
not landed with the rest of it: this is a new registration mechanism, and one
whose worst case is the paragraph above deserves a review of its own rather than
a corner of a larger change.
