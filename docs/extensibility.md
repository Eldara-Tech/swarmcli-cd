<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Extensibility: the seams a private companion replaces, and the one it adds to

swarmcli-cd is open core. This repository is the whole product — reconcile,
diff, health, API, CLI and, later, the web UI — and a private `swarmcli-cd-be`
companion replaces five specific behaviours with licensed ones and, through a
sixth seam, **adds** HTTP routes of its own rather than replacing anything. Per
D6 the companion arrives in Phase 3, but the seams ship from day one so that
nothing in the public tree has to move when it does.

The mechanism is the one `swarmcli-be` already uses against `swarmcli`: `init()`
self-registration and a blank import. **No build tags, and no stubbed files in
the public tree.** Every default here is a working implementation, not a panic
waiting for a licence.

## Why it works

Go initialises an imported package before the package that imports it. A seam
package registers its OSS default in its own `init()`; a companion package
registers its replacement in *its* `init()`, which necessarily runs later. The
companion always wins, and nothing has to coordinate the order.

**That argument covers four of the five defaults.** It holds because the
default lives in the very package the companion must import, which is what
makes "later" guaranteed. `swarms`' default does not — it is in `swarms/local`,
so that the seam does not drag the Docker applier in behind it — and two
unrelated sibling packages are initialised in import-path order, which neither
of them controls. Under that order `…/swarmcli-cd-be/swarms` runs *before*
`…/swarmcli-cd/swarms/local` (`-` sorts before `/`), so last-wins would have the
OSS default overwrite the companion, silently, with `swarms.Active()` reporting
`local` as the only evidence.

So `swarms/local` registers **only if nothing has**, which is order-independent
and makes "the companion always wins" true unconditionally. A default outside
its seam package must do the same; one inside it never needs to.

## The six seams

| Package | Interface | OSS default | What the companion brings |
|---|---|---|---|
| `swarms` | `Registry` | `local` — the swarm the controller runs in | multi-swarm through swarmcli-rbac-proxy |
| `authz` | `Authorizer` | `token` — one shared bearer token | SSO, projects and RBAC |
| `notify` | `Notifier` | `log` — a structured line per event | Slack, webhooks, e-mail |
| `secrets` | `Provider` | `plaintext` — passes material through, refuses what it cannot decrypt | SOPS decryption |
| `feature` | `Reporter` | `community` — nothing granted, no licence | what a verified licence grants, for the badge and the UI |
| `extension` | `Extension` | none — no route is the right OSS behaviour | SSO login and callback, `/api/v1/projects`, entitlement endpoints |

Four of them **replace**: there is exactly one answer to "which swarm registry
is in force". `notify` and `extension` **append**. `notify` does because a
companion adding Slack must not remove the log notifier — and because the HTTP
API's own event stream is itself a registered notifier, so replacing would
silently kill the UI's live updates. `extension` does because it has nothing to
replace: the core's endpoint set stays exactly where it is and a companion's
routes are served beside it, so several companions may each add their own and
appending is the only sensible composition.

`swarms`' default is the one that is not in its own package: it lives in
`swarms/local` and the binary blank-imports it, so that `swarms` stays a
contract and nothing importing it links the Docker applier. A build that takes
neither the default nor a companion registers `none`, which says so in the
startup line below and errors naming the missing import.

`feature` replaces for the same reason `authz` does: there is exactly one answer
to "what does this build grant", and the only composition rule available for a
set of booleans is OR — under which one module's report would turn on another
module's feature.

### `feature` is a report, never an enforcement point

Nothing in this repository reads a feature `Set` to decide whether to do
something, and nothing may. A `Reporter` is supplied by the very module a gate
would be constraining, so gating on one asks the licensed module for permission
to limit it. Entitlement is enforced from inside the implementation whose
entitlement lapsed — the same rule "When registration is over" states for a
`Slot` — and that is the only place holding both the licence and the operation.

`api` is the seam's one consumer, and a test in `feature` reads the repository's
source to keep it that way: the doc comment is what somebody reads once, and the
test is what fails the day somebody does not.

### Adding routes to the API

`extension` is the seam whose subject is the HTTP API rather than the reconcile
loop. SSO needs a login endpoint and a callback, projects need
`/api/v1/projects`, and the `licence` package sketched below needs somewhere to
answer about entitlements. None of them can be reached from outside the module:
`api` has no mux to hand out and `controller` exports only `Main()`, so before
this seam the only way to serve one of them was to fork both packages.

A route is **declared, not registered**. The extension says what it wants
served and the core decides what wraps it, because what wraps it is an
authorisation decision on a process holding write access to the swarm:

```go
type Handler func(w http.ResponseWriter, r *http.Request, subject authz.Subject)

type Route struct {
    Pattern string       // "GET /api/v1/projects" — a ServeMux pattern, any path
    Action  authz.Action // what the core authorises this route with
    Handler Handler
}

type Extension interface {
    Routes() []Route       // served behind the same guard as every core route
    PublicRoutes() []Route // no authentication, no authorisation
}

func Register(name string, e Extension) // call it from an init()
func All() []Extension
func Active() []string
```

`Handler` is `api`'s own guarded-handler shape. The subject is a parameter
rather than something read out of the request context, for the reason the core
handlers give: a context lookup that came back empty hands a handler a zero
`Subject`, an authorizer given one fails closed, and the result looks like a
caller with no permissions rather than like the wiring mistake it is.

`Action` has no default. Empty is a startup error, because there is no action
that is obviously right for a route this repository has never seen and guessing
one would be guessing at a permission. `authz.Action` is a string type with
additive constants, so an extension may declare an action of its own — the
authorizer it ships with is the only thing that has to recognise it, and an
action an authorizer does not recognise is refused. Each route is authorised
with `r.PathValue("app")` as the scope, exactly as the core routes are, so a
pattern with no `{app}` wildcard authorises with an empty application: "not
scoped to one application", which is already what `GET /api/v1/status` does.

That rule cuts both ways, and the core's own `authz.ActionController` is the
worked example. It was added after the actions before it, so an authorizer
compiled against the older set refuses it — correctly. The two endpoints that
ask therefore treat a refusal as **less to disclose** rather than as a `403`:
both stay reachable with `read` and answer a document with the controller-wide
fields removed. An action added to a *read* is safe to add for exactly that
reason; one added to a route is not, which is why a route's action is fixed at
registration and a companion's own action is the companion's to recognise.

The registration is the core's, and that is the whole of the design:

```go
mux.Handle(rt.Pattern, s.guard(rt.Action, rt.Handler))
```

Handing the companion a mux instead would give away three things that cannot be
got back — the guarantee that a route is authorised, the ability to name every
route in the startup log, and the chance to catch a collision before the
standard library panics on it.

`extension` is also the one seam with no OSS default and no `init()` of its own.
The other four register a working implementation so that there is something in
place before a companion arrives; here no route is the correct free-build
behaviour, and a `seam.List`'s zero value already is exactly that.

The companion side is an `init()` and two methods:

```go
// swarmcli-cd-be/licence/register.go
package licence

import (
    "net/http"

    "github.com/Eldara-Tech/swarmcli-cd/authz"
    "github.com/Eldara-Tech/swarmcli-cd/extension"
)

func init() { extension.Register("licence", routes{}) }

type routes struct{}

func (routes) Routes() []extension.Route {
    return []extension.Route{{
        Pattern: "GET /api/v1/licence",
        Action:  authz.ActionRead,
        Handler: func(w http.ResponseWriter, r *http.Request, subject authz.Subject) {
            // The subject is the one the core authenticated, so an entitlement
            // this module gates per user is decided here, inside the
            // implementation — not by swapping something out of a Slot.
        },
    }}
}

// Nothing unauthenticated to serve. The method still has to be written, which
// is the point of it being a method.
func (routes) PublicRoutes() []extension.Route { return nil }
```

**A collision refuses to start.** Before it touches the mux the core checks
every extension pattern against the core's own and against every other
extension's; a duplicate produces an error naming the pattern and both
extensions, `serve` fails with it, and the controller does not come up. That is
why `api.Server.Handler()` returns `(http.Handler, error)`.

The check is a conservative exact-string one, not `net/http`'s conflict
analysis. Two patterns `ServeMux` would consider conflicting without being equal
— `GET /a/{x}` against `GET /a/b` — still reach the mux and still panic there,
so a companion should not register a wildcard pattern overlapping a core path.
What it gets if it does is the standard library's panic, which at least names
both patterns and both registration sites. Reimplementing `ServeMux`'s conflict
rules here would be a second copy of standard-library logic, drifting.

**Every route is in the startup log**, guarded ones once at `INFO` as a list.
**Every public route is logged individually at `WARN`**, by pattern and by the
name its extension registered under.

`PublicRoutes` is a separate method rather than a `Public bool` on `Route`, and
the reason is the one `api` gives for passing the subject as a parameter: the
dangerous state must not be the zero value. An unset bool reads like every other
unset field in a struct literal, while a second method has to be written, named
and returned from. It exists for one shape — a callback arriving with a
credential this controller never issued, which therefore cannot be
authenticated by the authorizer that guards everything else. A route returned
from it must leave `Action` empty, since a public route naming an action is a
contradiction whose likeliest cause is an author who thought the action would
be enforced; and its handler is passed the zero `Subject`, so it must not
consult one.

A private module adding an unauthenticated endpoint to a controller holding the
Docker socket is the most dangerous thing this mechanism permits, and it has to
be visible in a running controller's log rather than only in the companion's
source. The `WARN` line is what discharges that: every unauthenticated path the
process serves, and the module that put it there, in the controller's own
startup output — readable by an operator who has neither the companion's source
nor a reason to trust its documentation.

### Reserved paths

The web UI is in this repository; part of what it draws is not. A licensed
build's login button navigates to `/auth/login` and its project switcher fetches
`/api/v1/projects`, neither of which exists in this tree — so the paths are
fixed here, in the open half, before anything serves them:

| Path | | Serves |
|---|---|---|
| `GET /auth/login` | public | where an SSO login starts |
| `GET /auth/callback` | public | where the identity provider comes back to |
| `GET /auth/logout` | public | where a session ends: expire the credential and redirect to `/` |
| `GET /api/v1/licence` | guarded | the whole licence record, beyond the summary `/api/v1/capabilities` carries |
| `GET /api/v1/projects` | guarded | the projects a subject may see, and their membership |
| `GET /api/v1/audit` | guarded | the audit log, paged |
| `GET /api/v1/notifications` | guarded | the notification destinations configured |

**Reserved means documented, and nothing else.** The core registers none of
them, nothing refuses a companion that serves something different, and on a
build with no companion they are exactly what any other unclaimed path is — a
JSON `404` under `/api/`, and whatever `GET /` serves everywhere else (see
[api § what an unmatched path answers](api.md#what-an-unmatched-path-answers)).
The list is as strong as the seam contracts on this page already are, and in the
same way: a document the companion is written against, and the public UI with
it.

**One of them is an obligation rather than an offer.** An authorizer whose
`Authenticate` honours something the browser cannot see — a session cookie,
which is what SSO issues and what `HttpOnly` is for — **must** serve
`GET /auth/logout`. The UI's sign-out otherwise clears a credential that was
never in the tab and leaves the operator signed in until the cookie expires on
its own. It is the same shape of contract as the other five and enforced the
same way, which is to say not at all; what makes it different is that a
companion can meet every other one and still ship a button that does nothing.

The corresponding half in this repository is `/ui/bootstrap.json`, which reports
`session` when the request it arrives on already authenticates. That is how a
tab holding a cookie discovers it is signed in, and it is why a companion that
authenticates cookies gets a working UI without contributing a line of it.

**A reserved path must never be added to `coreRoutes`.** That reads backwards
until the mechanism is said out loud, so here it is: the collision check above
refuses an extension pattern equal to a core route, so listing
`/api/v1/projects` there would make the core reject the one module that exists
to serve it. Reserving a path is not registering it. Nothing belongs in
`coreRoutes` that this repository does not itself answer.

Reserving now is cheap and reserving later is not, because that same check is an
exact-string one. The day a companion ships `/api/v1/audit-log`, that spelling
is frozen, and the public UI has to be changed to match a private module's
choice of name.

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
| `authz.LoginMethods` | how a browser may authenticate, for the public login document | it does implement it — it names the token box, and that same value is the fallback for an authorizer that names none |

`authz.LoginMethods` is the one whose absence fails *harmlessly*, which is why
it is beside the seam rather than on it: an authorizer that does not implement
it is presented as a token box, which is what every authorizer before it was.
`authz.Visible` is on the interface for exactly the opposite reason — the
fallback for a missing narrowing is to return everything, and authorisation may
only degrade closed.

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

`extension` is a `List` and reads it exactly once, when `api.Handler()` builds
the mux — which is also during wiring, after every `init()` — because a route
registered after that has nowhere to go: the mux is already serving. So an
extension may only register from an `init()`, where every companion's does.

**Entitlement gating therefore belongs inside an implementation, not in
swapping a `Slot` at runtime.** A licensed authorizer whose entitlement lapses
must start refusing from the inside. Replacing it in the slot would be seen by
the consumers that re-read and not by the ones holding the old value — and the
ones that did see it would see it mid-flight, which for `swarms` means handing
a half-finished sync a different daemon.

For `extension` that has one shape worth naming outright: **a companion may
declare no routes at all, and an unlicensed build that does is working
correctly.** `Routes()` and `PublicRoutes()` both returning nil is how
entitlement reaches the mux — the only way it can, since the mux is built once
and no route can be withdrawn from it afterwards. What that buys is something an
operator can check: the same binary, unlicensed, serves the route set a build
with no companion linked serves, logs the same `routes` line, and emits no
public-route `WARN`. The cost is the other half of the same fact — installing a
licence needs a restart before its endpoints exist.

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

    _ "github.com/Eldara-Tech/swarmcli-cd-be/authz"    // SSO + RBAC, login and callback
    _ "github.com/Eldara-Tech/swarmcli-cd-be/licence"  // entitlement gating + endpoints
    _ "github.com/Eldara-Tech/swarmcli-cd-be/notify"   // Slack, webhooks
    _ "github.com/Eldara-Tech/swarmcli-cd-be/secrets"  // SOPS
    _ "github.com/Eldara-Tech/swarmcli-cd-be/swarms"   // multi-swarm
)

func main() { controller.Main() }
```

Routes add no line to this file. `authz` and `licence` register theirs through
`extension` from the same `init()` that registers everything else they bring, so
the endpoints the companion serves are wired by the import that was already
there — which is what keeps this the only file that differs between the builds.

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
    "notify", notify.Active(),        // a list: every notifier is live
    "secrets", secrets.Active(),
    "feature", feature.Active(),
    "extension", extension.Active(),  // a list too: every extension is served
)
```

The same names are served by `GET /api/v1/capabilities`, for an operator who has
the admin token but not the logs, and the two must not disagree. `feature` there
is not redundant with the edition it reports beside: a licensed reporter that is
linked but has no valid licence names itself here while the edition still reads
`community`, and that pair is what tells "the module is not loaded" apart from
"the licence is missing".

`extension` reports which modules registered routes, not which routes. What is
actually served is a second line, logged after the mux is built — which is a
different moment, because building it is the step that can fail on a collision:

```go
log.Info("routes", "guarded", guarded)  // every guarded pattern, core and extension
for _, r := range public {
    log.Warn("public route", "route", r)  // one line each: pattern and extension name
}
```

Two levels rather than one. A guarded route is unremarkable, so the whole list
is one `INFO` line. An unauthenticated route on a process holding the Docker
socket is not, and a line per route at `WARN` is what makes it as easy to find
afterwards as it is at startup.

## Adding a seam

Only when a behaviour genuinely has to differ between the editions. Each seam
is an interface a companion must keep implementing across releases, so the bar
is a licensed feature that cannot be expressed any other way — not a
configuration knob, and not somewhere a future feature *might* go.

1. Add a package under the repository root with the interface, its OSS default
   where it has one, and `Register` / `Get` / `Active` wrapping a `seam.Slot`
   (or a `seam.List` when several implementations should all run).
2. Register the default from the package's own `init()` — unless the zero value
   is already the behaviour a build with no companion should have. `extension`
   is the one seam where it is: no route is the right answer for a controller
   with nothing loaded, and an empty `seam.List` is exactly no routes, so the
   step would only add an implementation returning nothing. The step exists to
   stop a `Slot` handing a consumer a nil implementation to call, and a `List`
   has none to hand out. A seam that *replaces* a behaviour always needs its
   default.
3. Add it to the table above.

Take every parameter a companion implements as a struct, from the first commit.
The day the companion ships, every signature in this document is frozen.

## What a companion still cannot do

Recorded so that it is a known gap rather than a discovery.

**The list is currently empty.** Adding a route was the last entry and it is now
the `extension` seam above. The heading stays because an empty list is worth
stating — a companion author reading this should be able to tell "nothing is
missing" from "nobody has written it down".

One loose end, which is a gap in the wiring rather than in the contract:
`swarms.Lister` is landed and nothing consumes it, so a companion implementing
it changes no behaviour yet. The sweep that would use it, `prune.Departed`,
takes bare application names, and swarm-qualifying what the app-set loop hands
it cannot be tested against a build whose registry resolves one swarm. Neither
`prune.Departed` nor `appset`'s pruner is implemented outside this repository,
so unlike `swarms.Registry.Backend` there is no now-or-never here: the
signatures can change the day the companion exists. It is the one item
[#111](https://github.com/Eldara-Tech/swarmcli-cd/issues/111) stays open on.
