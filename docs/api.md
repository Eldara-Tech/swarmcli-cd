<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# HTTP API

The controller serves one HTTP API, and everything else is a client of it. The
`swarmcli-cd app` commands go through it; a TUI view and a web UI will too. It is
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
| `GET` | `/api/v1/applications` | the list view — one row's worth of state per application |
| `GET` | `/api/v1/applications/{app}` | the detail view — releases and their services |
| `GET` | `/api/v1/applications/{app}/diff` | what a sync would change |
| `GET` | `/api/v1/applications/{app}/history` | each release's revisions |
| `POST` | `/api/v1/applications/{app}/sync` | trigger a reconcile-and-apply |
| `GET` | `/api/v1/events` | a live event stream, so a UI never polls |

The paths are nouns so that writable applications can be added later without any
of them moving. Applications are read-only in Phase 1 — they come from the
mounted applications file, and there is no hot reload.

## Authentication

Every `/api/v1/…` endpoint requires the admin token as a bearer credential:

```bash
curl -H "Authorization: Bearer $SWARMCLI_CD_ADMIN_TOKEN" \
     http://127.0.0.1:8080/api/v1/applications
```

The default authorizer checks one shared token and grants two actions: `read`
(every `GET`) and `sync` (the `POST`). A missing or wrong token is `401`. The
controller refuses to start with no token configured at all, so a `401` always
means the token was presented and rejected, never that auth was off. SSO, per-user
identity and RBAC are a licensed capability; see
[extensibility.md](extensibility.md).

`/healthz` takes no credential on purpose: a container healthcheck runs beside
the process and cannot carry one without putting it in the stack file and in
`docker inspect` output. What it discloses — that something is listening — a TCP
connect already tells you.

## Response shapes

The wire types are defined in the [`application`](../application/application.go)
package; this is the map, not a substitute for it. Enums marshal as their
lowercase name and decode an unrecognised name to `"unknown"`, so a newer
controller never breaks an older client.

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

`planned` distinguishes "nothing would change" (`planned: true`, empty
`releases`) from "not reconciled yet, nothing to compare" (`planned: false`). The
`diff` is a manifest-level text diff, carried here and nowhere else because a
list view must not drag whole manifests along.

### History — `GET /api/v1/applications/{app}/history`

```json
{ "releases": [ { "name": "traefik", "revisions": [ { "revision": 4, "chart": "...", "version": "0.1.1", "status": "deployed", "owner": "cd/edge" } ] } ] }
```

A release declared but never deployed has an empty `revisions` rather than being
absent — a real state, and a different one from "no such release".

### Sync — `POST /api/v1/applications/{app}/sync`

Triggers a reconcile that applies whatever the plan contains, whether or not the
policy is automated. It returns as soon as the sync is accepted; the sync itself
runs detached. Follow it by polling the detail view, or watch `/events`. This is
the one endpoint guarded by the `sync` action rather than `read`.

## Event stream — `GET /api/v1/events`

Server-Sent Events, so a UI gets live updates without polling. Each event is an
SSE frame whose `data` is a small JSON object:

```
event: sync
data: {"application":"edge","type":"sync","revision":"9f3c1ab","message":"...","at":"2026-07-22T09:41:10Z"}

```

The stream is fed by the same notifier seam that writes the controller's log,
which is why a companion adding Slack *appends* a notifier rather than replacing
one — replacing would silently kill the UI's live updates.

## See also

- [Getting started](getting-started.md) · [Configuration](configuration.md) · [Concepts](concepts.md)
