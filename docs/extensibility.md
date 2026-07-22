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

## The four seams

| Package | Interface | OSS default | Replaced by |
|---|---|---|---|
| `swarms` | `Registry` | `local` — the swarm the controller runs in | multi-swarm through swarmcli-rbac-proxy |
| `authz` | `Authorizer` | `token` — one shared bearer token | SSO, projects and RBAC |
| `notify` | `Notifier` | `log` — a structured line per event | Slack, webhooks, e-mail |
| `secrets` | `Provider` | `plaintext` — material passes through | SOPS decryption |

Three of them **replace**: there is exactly one answer to "which swarm registry
is in force". `notify` **appends**, because a companion adding Slack must not
remove the log notifier — and because the HTTP API's own event stream is itself
a registered notifier, so replacing would silently kill the UI's live updates.

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

func (r *registry) Backend(ctx context.Context, swarm string) (charts.Backend, error) {
    // Resolve `swarm` to a named Docker context and hand back a backend for
    // it. charts.NewDockerBackend(name) carries no process-global state — not
    // the client singleton, not the context lookup, not the snapshot cache —
    // which is what makes one process able to serve several swarms at once.
}
```

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

That entry point is the top-level `controller` package, and `cmd/swarmcli-cd` is
already the one-line `main` above without the blank imports — a `main` package
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

require github.com/Eldara-Tech/swarmcli-cd v0.1.0
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
slog.Info("seams",
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
