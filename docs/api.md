<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# HTTP API

The controller serves one HTTP API, and everything else is a client of it: the
`swarmcli-cd app` commands go through it, and so does the
[web UI](web-ui.md) the same process serves. It is
designed UI-first (decision D1): every screen a UI has is one request, and every
action a user can take is one endpoint. That makes it equally the interface to
script against.

The CLI's `-o json` prints the controller's response for a read **unmodified** —
so the quickest way to see any shape below is to run the matching command with
`-o json`.

## Endpoints

| Method | Path | |
|---|---|---|
| `GET` | `/healthz` | unauthenticated liveness; says nothing but "something is listening" |
| `GET` | `/ui/bootstrap.json` | unauthenticated: how a browser may log in, and nothing else |
| `GET` | `/api/v1/status` | the controller's own state — where the app set came from and whether it is loading |
| `GET` | `/api/v1/applications` | the list view — one row's worth of state per application |
| `GET` | `/api/v1/applications/{app}` | the detail view — releases and their services |
| `GET` | `/api/v1/applications/{app}/diff` | what a sync would change |
| `GET` | `/api/v1/applications/{app}/history` | each release's revisions |
| `POST` | `/api/v1/applications/{app}/sync` | trigger a reconcile-and-apply |
| `GET` | `/api/v1/applications/{app}/services/{svc}/logs` | a live container log stream for one of that application's services |
| `GET` | `/api/v1/nodes` | the swarm's node roster, with each node's task counts |
| `GET` | `/api/v1/diagnostics` | the cluster integrity score, its checks and its open risks |
| `GET` | `/api/v1/events` | a live event stream, so a UI never polls |
| `GET` | `/api/v1/capabilities` | what this build is, what it grants, what it is wired to answer, and which seam implementations are live |
| `GET` | `/` | the web UI, and the fallback for its client-side routes |
| `GET` | `/assets/{path...}` | the UI's hashed build output |

Two of those are served behind an optional interface a reconciler either
implements or does not, and a build whose reconciler does not answers **501**
rather than inventing a reply — which is what both of them used to do. Both are
implemented here, so both answer in this build; the 501 branch stays reachable
for a reconciler reached through the same interface that has neither, and for a
destination whose backend cannot serve one. Which of the two a build can serve
is reported by `capabilities` in the document below, so the console leaves out a
control it would only be able to apologise for.

The log stream is the one endpoint with a bound on how many of it a controller
serves at once: sixteen, after which it answers **503** rather than taking a
seventeenth goroutine and daemon connection. It writes an SSE comment frame
every twenty seconds so that a proxy does not close a stream watching a quiet
container, and re-checks the caller's authorisation on each of those, so a
withdrawn grant reaches a console that is already attached.

The paths are nouns so that writable applications can be added later without any
of them moving. Applications are read-only over the API in both directions of the
two-tier model: with the set mounted as a Docker config there is nothing to write
into, and with the set in git the way to change it is a commit, which is the
whole point. See [configuration § where the app set lives](configuration.md#where-the-app-set-lives).

That table is the **core's** route set, and a companion module may add to it
through the `extension` seam. What it adds is authorised the same way: the core,
not the companion, registers each route behind the same guard as everything
above, with the `authz.Action` the route declares. The one exception is a route
the companion declares public on purpose, which is served with no credential at
all. A running controller logs every route it serves at startup —
the guarded ones as a list, each public one on a `WARN` line naming the pattern
and the module that registered it — so what a build actually exposes is readable
from its own log rather than from the companion's source. See
[extensibility.md](extensibility.md).

## Authentication

Every `/api/v1/…` endpoint requires the admin token as a bearer credential:

```bash
curl -H "Authorization: Bearer $SWARMCLI_CD_ADMIN_TOKEN" \
     http://127.0.0.1:8080/api/v1/applications
```

The default authorizer checks one shared token and grants every action there is:
`read` (the status, list, detail and event-stream endpoints), `diff`, `history`,
`sync` (the `POST`) and `controller`. Those are five rather than two so that a
licensed authorizer can grant one and not another — a diff is the rendered
manifest and a history is a walk of the swarm's stored revisions, which are a
different disclosure from a list row. With one shared token there is one subject
and the distinction makes no difference. A missing or wrong token is `401`. The
controller refuses to start with no token configured at all, so a `401` always
means the token was presented and rejected, never that auth was off. SSO, per-user
identity and RBAC are a licensed capability; see
[extensibility.md](extensibility.md).

The endpoints that answer about every application are **narrowed rather than
gated**: `GET /api/v1/applications` returns only the applications the caller may
see, `/api/v1/events` delivers only their events, and `GET /api/v1/status`
reports only the caller's own applications in its three name lists. With one
shared token that is all of them; an authorizer implementing projects is what
makes it fewer, and an authorizer that cannot answer refuses the request rather
than serving the whole collection.

`controller` is the fifth action and the odd one out: it is not about any
application, so there is nothing for the narrowing above to narrow. It covers
the facts that belong to the controller and the build rather than to what they
reconcile — where the app set is sourced from, how many applications there are
in total, why a load was refused, the version, the licence, and which
implementation is loaded behind each seam. `GET /api/v1/status` and
`GET /api/v1/capabilities` are the two endpoints that carry any of it, and both
stay reachable with `read`: a subject the authorizer will not grant `controller`
gets the same document with those fields removed, not a `403`. That is
deliberate — a browser reads both on every load, and an authorizer written
before this action existed refuses it, as the contract requires.

`/healthz` takes no credential on purpose: a container healthcheck runs beside
the process and cannot carry one without putting it in the stack file and in
`docker inspect` output. What it discloses — that something is listening — a TCP
connect already tells you.

`/ui/bootstrap.json` is unauthenticated because a login screen has to know
whether to draw a token box or an SSO button *before* anyone has a credential to
present. It is under `/ui/` rather than `/api/v1/` precisely so that the sentence
at the top of this section stays true without exceptions: a public route under
the API prefix would make an operator check each one before believing it. It
carries no version string — an unauthenticated caller does not need the build
number — and is served `Cache-Control: no-store`, because installing a licence
or loading an SSO module changes what it says.

The two UI routes are unauthenticated for a related reason: a browser has no
credential until the login screen it is asking for has loaded, and what they
serve is the build's own bytes, which say nothing about the swarm. `--ui=false`
answers both with `404`; it does not remove them.

## What an unmatched path answers

`GET /` matches every path no more specific route claimed, because every such
path is a route belonging to the router in the browser. Two consequences, both
deliberate:

- **A mistyped endpoint under `/api/` is still a JSON `404`**, not the UI's
  index. Without that, a typo would answer `200 text/html` and become a parse
  error a long way from the mistake. The check is case-insensitive and takes the
  bare `/api` as well, which is wider than the routes behind it — the router
  matches case-sensitively, so `/API/v1/status` reaches no handler — because the
  client most in need of being told is the one that got the spelling wrong.
- **A wrong method is now a `405` or a `404`, not the old `405` in both cases.**
  `POST /api/v1/typo` is `405` with `Allow: GET, HEAD`, because with `GET /`
  registered every path matches under `GET`; and `GET …/sync`, which used to be
  `405 Allow: POST`, is the JSON `404` above. Accepted rather than solved:
  additionally registering a methodless `/api/` pattern would make the first a
  JSON `404` too, but it cannot bring the second back — a pattern matching every
  method takes the request before the `POST` route can report itself.

## Response shapes

The wire types are defined in the [`application`](../application/application.go)
package; this is the map, not a substitute for it. Enums marshal as their
lowercase name and decode an unrecognised name to `"unknown"`, so a newer
controller never breaks an older client.

### Status — `GET /api/v1/status`

The controller itself, as distinct from what it reconciles:

```json
{
  "appSet": {
    "mode": "git",
    "source": "https://github.com/your-org/apps.git @ main (apps/applications.yaml)",
    "revision": "a1b2c3d4e5f6...",
    "loadedAt": "2026-07-27T09:12:04Z",
    "error": "apps/applications.yaml@d4e5f6: applications[1]: duplicate application name \"edge\"",
    "stale": true,
    "orphaned": ["legacy-api"],
    "pruned": ["old-edge"],
    "pruneHeldBy": ["edge-eu"]
  },
  "applications": 4
}
```

`mode` is how the set is sourced — `static` for the applications file mounted at
deploy time, `git` for a repository the controller pulls, `path` for a directory
something else keeps current (see
[configuration § where the app set lives](configuration.md#where-the-app-set-lives)).
`source` is what the bootstrap was pointed at, which `mode` alone cannot tell
you: it is the anchor nothing in git can repoint, so it is worth being able to
read back. `revision` is the commit the running set came from, absent when the
set does not come from a repository, and `loadedAt` is when it last loaded
**successfully**, not when it was last checked.

`stale` is the field to watch: it means the running set is the last one that
validated and a newer version is being refused, with `error` saying why. An
`error` with `stale` false is one of two other things — a set that loaded but
could not be applied in full (one application that would not start, say, while
the rest of the change landed), or, with `applications` at zero and no
`loadedAt`, a controller that has never managed to load a set at all. That last
one is the loudest of the three and is what a controller pointed at an
unreachable repository looks like. All of them clear on the next attempt that
succeeds.

`orphaned` names applications that have left the set. Their stacks are still
deployed and nobody reconciles them any more: app-of-apps reports the orphan
rather than removing it, unless
[prune](configuration.md#prune) is enabled. The list lives in memory, so a
restart forgets it — a gap in the reporting rather than in the cleanup, since a
controller with prune enabled rediscovers a departed application from the owner
stamps on its releases and removes it anyway.

`pruned` names applications whose resources this controller has deleted, most
recent last, and is absent on the default report-only configuration. It exists
because prune otherwise leaves no trace: the application is gone from the set,
gone from `orphaned` and gone from the swarm, so "did it actually go" would only
be answerable from the logs. Like `orphaned` it lives in memory and reports this
process's own deletions rather than an audit log.

`pruneHeldBy` names the applications the sweep is waiting on, and is absent
whenever it is waiting on nothing — which, once a controller has settled, is
always. Prune deletes what no application declares, so it cannot run while an
application has not yet reconciled and said what it declares; it would read that
silence as a departure. This is the field that distinguishes "prune is enabled
and correctly doing nothing" from "prune is broken", and it is what a rename
looks like for the first pass or two (see
[configuration § renaming an application](configuration.md#renaming-an-application)).

`applications` counts what is actually being reconciled, which is not always what
the last loaded file declares.

All three name lists are narrowed to the applications the caller may see, so a
tenant scoped to one project does not read the names of applications in another.
The rest of the document above — `source`, `revision`, `error` and
`applications` — is the `controller` action's. A subject without it reads `mode`,
`loadedAt`, `stale` and the narrowed lists, an `applications` count of what it
can see, and, when the set really has an error, a sentence saying so without the
names. Nothing changes for the default authorizer, which grants everything.

### List — `GET /api/v1/applications`

An object wrapping the array, so the response stays an object if fields are ever
added beside it:

```json
{
  "applications": [
    {
      "spec":   { "name": "edge", "source": { ... }, "syncPolicy": { ... } },
      "status": {
        "sync":   { "state": "out-of-sync", "revision": "9f3c1ab",
                    "summary": { "install": 0, "upgrade": 1, "unchanged": 3 },
                    "lastSync": { "revision": "4b7e02d", "succeeded": true, ... } },
        "health": { "state": "healthy", "services": { "healthy": 7, "total": 7 } },
        "observedAt": "2026-07-22T09:41:10Z"
      }
    }
  ]
}
```

Each element is a `View`: the declared `spec` beside the last observed `status`.
The list omits per-release detail — a row renders from `status` alone — which is
what keeps a twenty-application list small.

### Detail — `GET /api/v1/applications/{app}`

The same `View`, with `status.releases` populated: each release's chart, version,
revision, planned action, sync state, health, and its services (name, mode,
`running`/`desired`, health, update state). Absent releases means "not requested"
here rather than "none" — the engine rejects a release file declaring none.

### Diff — `GET /api/v1/applications/{app}/diff`

```json
{ "planned": true, "releases": [ { "release": "traefik", "action": "upgrade", "diff": "..." } ] }
```

`planned` distinguishes "nothing would change" from "not reconciled yet, nothing
to compare", and the two carry **different empties**, which is worth reading
twice because the useful one is the null:

- `{"planned": true, "releases": null}` — reconciled, and a sync would change
  nothing. Only the releases a sync would touch are listed, so a converged
  application lists none.
- `{"planned": false, "releases": []}` — nothing has been reconciled, so there is
  no plan to compare against.

The `diff` is a manifest-level text diff, carried here and nowhere else because a
list view must not drag whole manifests along. It is line-oriented with `-`, `+`
and space prefixes and has no hunk headers, so each one carries the whole
rendered manifest as context.

### History — `GET /api/v1/applications/{app}/history`

```json
{ "releases": [ { "name": "traefik", "revisions": [ { "revision": 4, "chart": "...", "version": "0.1.1", "status": "deployed", "owner": "cd/default/edge:release/traefik" } ] } ] }
```

A release declared but never deployed has an empty `revisions` rather than being
absent — a real state, and a different one from "no such release".

`owner` is the stamp the chart engine stored: this controller's owner id,
`cd/<controller-id>/<application>` — so `cd/default/edge` for application `edge`
on a controller given no `--controller-id` — followed by the release it was
written for. A revision carrying anything else was installed by something that
is not this controller, which is what stops prune touching it; see
[concepts § ownership](concepts.md#ownership).

### Sync — `POST /api/v1/applications/{app}/sync`

Triggers a reconcile that applies whatever the plan contains, whether or not the
policy is automated. It returns as soon as the sync is accepted; the sync itself
runs detached. Follow it by polling the detail view, or watch `/events`. This is
the only endpoint that writes, and the only one guarded by the `sync` action.

It answers `202` with what it did, and there are two answers:

```json
{ "application": "edge", "accepted": true }
{ "application": "edge", "accepted": true, "coalesced": true }
```

`coalesced` means a sync was already running and another was already queued
behind it, so this request joined that one rather than adding a third. It is
still a `202` and it is not a no-op: the queued sync has not read the repository
yet, so the state you asked to have reconciled is the state it will reconcile.
A client that drew it as an error, or as a press that did nothing, would be
wrong in both directions.

It is also the only one with a caller to record: the subject it authenticated is
carried as the `actor` of every event that sync raises. Nothing else names one,
because nothing else starts work.

### Discovery — `GET /ui/bootstrap.json` and `GET /api/v1/capabilities`

Two documents, deliberately asymmetric: what a login screen needs before anyone
is authenticated, and what an operator holding the token may read about the
build. The first is as small as it can be.

```json
{ "login": [ { "id": "token", "label": "Admin token" } ] }
```

`start` appears only on a method the browser has to navigate to — an SSO button
carries `"start": "/auth/login"`, a typed-in credential carries nothing. The
list comes from the authorizer, so a deployment that has moved to SSO stops
offering a box for a credential it no longer issues.

The capability document is reachable with `read` and split at `controller` —
`version`, `licence` and `seams` are the latter's, and are absent or empty for a
subject that is not granted it:

```json
{
  "version": "1.2.0",
  "edition": "community",
  "features": { "multi-swarm": false, "sso": false, "projects": false,
                "audit": false, "notifications": false },
  "capabilities": { "logs": true, "nodes": true },
  "licence": null,
  "seams": { "swarms": "local", "authz": "token", "notify": ["log", "api"],
             "secrets": "plaintext", "feature": "community", "extension": [] }
}
```

`features` always carries every key the build knows about, whatever the reporter
put in its own map, so a UI hiding a control on `features["sso"]` can tell
`false` from absent.

`capabilities` answers a different question and must not be confused with it:
`features` is what a **licence** grants, and `capabilities` is what this build's
reconciler is **wired** to answer. The two key sets do not overlap, and a test
enforces that. It is read off the same type assertions the endpoints make, so
there is no second place to keep in step, and it carries every declared key for
the same reason `features` does. What it does *not* promise is the outcome of
any one request: whether a reconciler implements a seam is settled at compile
time, but which backend serves a destination is settled per request, so an
endpoint can still answer 501 for a capability reported `true`. The document
says what the build is wired for; the endpoint stays the authority. `licence` is `null` in a build with no licensed module
linked — which is a different thing from a licensed build with no licence
installed, which reports a status of `absent`; the other four statuses are
`valid`, `grace` (expired, still granting, renew before it stops), `expired` and
`invalid`. `seams` is the startup `seams` log line, readable by an operator who
has the token but not the logs — which is also why it is behind `controller`: it
names every implementation loaded into a process holding the docker socket,
companion modules included, and `version` is the build number that
`bootstrap.json` deliberately withholds from an unauthenticated caller. What a
subject without `controller` keeps is `edition`, `features` and `capabilities`,
because that is what a UI decides what to draw from — a control present for one
subject and absent for another would be a dead control.

## Event stream — `GET /api/v1/events`

Server-Sent Events, so a UI gets live updates without polling. Each event is an
SSE frame whose `data` is a small JSON object:

```
event: sync-succeeded
data: {"application":"edge","swarm":"","type":"sync-succeeded","revision":"9f3c1ab","at":"2026-07-22T09:41:10Z"}

```

`application`, `swarm`, `type` and `at` are always present. `revision` and
`message` appear only on the events they apply to: a `sync-succeeded` resolved a
commit and has nothing to say, a `resources-pruned` names what went and has no
revision of its own. `actor` appears only when there was one.

`actor` names whoever asked for the work the event came out of — the
authenticated subject of the request that called `POST
/api/v1/applications/{app}/sync`:

```
event: resources-pruned
data: {"application":"edge","swarm":"","type":"resources-pruned","message":"pruned legacy-api","actor":"alice","at":"2026-07-22T09:41:12Z"}

```

It appears on **every** event that sync raised, not only its `sync-started` and
`sync-succeeded`: a prune or a drift correction performed during a manual sync
was started by the person who pressed the button.

It is **absent** when the controller acted on its own — a tick of the reconcile
loop, drift converging, a sweep of something git stopped declaring — because
then nobody pressed anything. That is a genuine absence, which is why the key
goes rather than being sent empty, exactly as `revision` and `message` do.
`swarm` on the same frame is deliberately the opposite: see below.

`swarm` is the destination the event concerns, as the application's
[`destination.swarm`](configuration.md#destination-optional) names it, and is
**empty for the swarm the controller runs in**. In this build that is every
event: no other destination resolves, so an application naming one fails to
reconcile rather than raising events from somewhere else. A licensed multi-swarm
build fills it, which is what lets a client tell production events from staging
ones without parsing prose.

It is present even when empty, where `revision`, `message` and `actor` are not,
for two reasons. An absent key would say the event has no destination, and every
event has one — empty *is* the answer here, rather than an absence. And a client
written against this build would otherwise never see the field at all — it would
meet it for the first time pointed at a controller that fills it, which is
exactly the client that must not be surprised.

The event name is the `type` verbatim, because that is what an `EventSource`
listener binds to. There are nine of them and no others: `sync-started`,
`sync-succeeded`, `sync-failed`, `drift-detected`, `live-drift-detected`,
`drift-converged`, `resources-pruned`, `prune-failed` and `self-update-issued`.
Live drift is its own type rather than a `drift-detected` with a different
message — one is a commit to review and the other a change nobody recorded, and
a client routing on the name must be able to tell them apart without reading
prose.

`self-update-issued` is the last event a controller sends. It reports that the
write replacing it has been accepted by the daemon, which is not the same as a
controller that came back: the task is stopped as the rollout starts, and if the
new one fails its healthcheck the swarm reverts the service and the recovered
controller reports that as live drift. A client must not read it as a
completion, and should expect the stream to end immediately after it.

The stream is fed by the same notifier seam that writes the controller's log,
which is why a companion adding Slack *appends* a notifier rather than replacing
one — replacing would silently kill the UI's live updates.

## Service log stream — `GET /api/v1/applications/{app}/services/{svc}/logs`

Server-Sent Events, one JSON object per `data:` line, one line of container
output per object:

```
data: {"service":"edge_web","taskID":"k9r2m1x8p4qz…","nodeID":"n3f8…","nodeHostname":"swarm-w1","slot":3,"stream":"stdout","message":"listening on :8080","timestamp":"2026-08-28T06:00:00.412Z"}

```

`service`, `stream`, `message` and `timestamp` are always present. `stream` is
`stdout` or `stderr`; a line whose origin the daemon does not state is `stdout`,
which is the honest default and is what every line of a TTY service is, because
the daemon does not multiplex a stream it gave a terminal. `timestamp` is when
the *container* wrote the line, not when the controller read it.

`taskID` and `nodeID` are the daemon's own ids for the replica and the machine.
`nodeHostname` and `slot` are what an operator calls them, resolved by the
controller against the live swarm:

- **`slot`** is the replica number, as `docker service ps` shows it. It is
  **absent for a global-mode service**, whose tasks have no slot — the daemon
  reports 0 and means it, and there is no replica zero — and absent for a task
  that started after the stream opened and has not yet been re-read.
- **`nodeHostname`** is absent if the roster could not be read. The two are
  separate lookups on purpose, so one failing costs one label rather than both.

Neither is ever the only thing identifying a line: the ids are always there.
A client that needs a label should fall back `slot` → `nodeHostname` → `taskID`,
which is the order the console uses.

`notice` marks the one class of line the container did not write — the
controller reporting that it dropped output, or truncated a line that would not
end. It carries `stream: "stderr"`, so an operator filtered to stderr sees the
reason a gap appeared, and it carries no `taskID`.

### Choosing a window

| Parameter | Meaning | Default |
| --- | --- | --- |
| `tail` | how many lines of scrollback to deliver before following | `100` |
| `since` | how far back those lines may reach, as a Go duration (`15m`, `6h`) | none |

**`since` may not be given without `tail`, and a request that does is a 400.**
That is not fussiness. The daemon selects the last `tail` lines *first* and only
then drops the ones older than `since`, so `?since=6h` on its own asks for six
hours and is answered with a hundred lines — a window that silently does
nothing. The two are one intent and the API requires both.

`tail` must be between 1 and 50,000, and `since` at most 24 hours. Both are
**refused** rather than clamped: a client handed the last fifty thousand lines
when it asked for a million has been answered with something other than what it
asked for, with nothing in the reply to say so.

`since` is a duration and not an instant deliberately. It is resolved against
the controller's clock, which is far closer to the daemon's than a browser's is,
so a client whose clock is a few minutes fast cannot ask for a window that has
not happened yet.

There is no `until`. The daemon's swarm log route does not read one — only the
container endpoint does — so this endpoint cannot offer an end bound and does
not pretend to.

### What it costs

A `since` reaching far back is read on **each node holding a task**, by that
node's own log driver; the manager relays. So the disk cost is spread across the
swarm rather than landing on the raft store, and `tail` is what bounds how much
crosses the wire.

The controller serves **16 log streams at once** across all clients and
applications, and answers **503** when they are all taken. A history read holds
one for as long as its tab stays open.

### Statuses

| Status | Meaning |
| --- | --- |
| `400` | the window could not be read — see above |
| `404` | no such application, or no such service *for this application* |
| `501` | this build's reconciler cannot stream logs, or the destination's backend cannot |
| `503` | all 16 log streams are in use |
| `502` | the stream could not be opened; the daemon's own words go to the controller log only |

Every one of them is decided before the response headers are written, so a
failure is never the first frame of a `200`.

An idle stream writes a `: keepalive` comment frame every 20 seconds, which
conforming SSE clients ignore. The event stream above deliberately has no such
timer; this one does because its client is `fetch` rather than `EventSource` and
does not reconnect, so a proxy closing an idle connection would be permanent.
The controller re-checks the caller's authorisation on the same tick, so a
withdrawn grant ends an attached console rather than outliving it.

## See also

- [Getting started](getting-started.md) · [Configuration](configuration.md) · [Concepts](concepts.md)
