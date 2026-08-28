<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright © 2026 Eldara Tech -->

# Phase 2.0: Swarm Engine Extensions & Advanced Operator Console

Phase 2.0 expands SwarmCLI CD from GitOps declarative reconciliation into a full-featured Docker Swarm cluster control plane, introducing live per-service container log streaming over SSE, Swarm Node topology inspection, unified cluster diagnostics, and high-performance operator console UI screens.

---

## 1. Architecture Overview

```
                      ┌────────────────────────────────────────┐
                      │          SwarmCLI CD Web UI            │
                      │  (React 19 / Vite / Tailwind / CSS)    │
                      └───────┬───────────────┬────────────────┘
                              │               │
     GET /api/v1/nodes        │               │ GET /api/v1/.../logs (SSE)
     GET /api/v1/diagnostics  │               │ GET /api/v1/events   (SSE)
                              ▼               ▼
                      ┌────────────────────────────────────────┐
                      │          SwarmCLI CD Server            │
                      │       (Go 1.24 HTTP REST / SSE)        │
                      └───────┬───────────────┬────────────────┘
                              │               │
               NodeLister / Diagnostics       │ LogStreamer Seam
                              ▼               ▼
                      ┌────────────────────────────────────────┐
                      │         Docker Swarm Engine            │
                      │   (Node Tasks, Service Log Pipes)      │
                      └────────────────────────────────────────┘
```

---

## 2. Go Backend Extensions

### 2.1 RBAC Actions (`authz/authz.go`)
Added two new fine-grained permission verbs:
- `ActionLogs` (`"logs"`): Permits opening live streaming stdout/stderr container logs for application services.
- `ActionNodes` (`"nodes"`): Permits querying physical and virtual Swarm manager/worker node topology and capacity.

### 2.2 Endpoints

#### 1. Live Container Log Streamer (`api/logs.go`)
- **Route:** `GET /api/v1/applications/{app}/services/{svc}/logs`
- **Guarded Action:** `ActionLogs`
- **Protocol:** Server-Sent Events (`text/event-stream; charset=utf-8`)
- **Interface:** `LogStreamer` seam
  (`ServiceLogs(ctx, app, svc, application.ServiceLogRequest) (<-chan application.ServiceLogEvent, error)`)
- **Implemented (#261).** `*reconcile.Reconciler` satisfies `LogStreamer`, resolving the
  *application's own* destination through the `swarms` seam — unlike the node roster, which
  asks the swarm this controller runs in — and reading behind `capability.ServiceLogReader`.
  `controller` asserts the wiring at compile time, because the seam's type assertion falls
  back silently and a renamed method would turn the console off rather than fail the build.
  It must not answer anything else: the first version of this endpoint emitted a synthetic
  line every three seconds — `service X task healthy - 0 active errors` — as container output,
  which is a health claim about a service nothing had contacted.
- **The 501 branch stays, and is still reachable.** A reconciler reached through the same
  interface may not implement the method, and one that does may still meet a backend with no
  reader behind this application's destination — resolved per request. That second case is
  reported with `application.ErrUnsupported` and answered identically: same status, same
  sentence.
- **The seam carries events rather than bytes, and that is why.** It used to hand back an
  `io.ReadCloser` of lines with an optional `stderr\t` prefix. `ServiceLogEvent` declares
  `taskID` and `nodeID`, the daemon states both per line in its `details=1` prefix, and a byte
  stream has nowhere to put them — so on a replicated service, one node crash-looping and the
  whole service being broken looked identical. Demultiplexing Docker's 8-byte framing now
  happens once, in the backend, instead of being re-encoded into lines for the handler to
  parse back.
- **Only two attributes survive the details prefix.** `details=1` prefixes every line with the
  message's attributes, which includes whatever the operator configured through
  `--log-opt labels=` and `--log-opt env=` — that is, selected environment variable *values*.
  The reader keeps `com.docker.swarm.node.id` and `com.docker.swarm.task.id` and drops the
  rest, so the console shows the text the container wrote.
- **A TTY service loses the stream label and nothing else.** The daemon multiplexes only when
  no selected task has a terminal, so with one there is nothing in the bytes to tell stdout
  from stderr and every line is labelled `stdout`. The task and node labels survive.
- **Authorisation is per application, and `{svc}` is not a second grant.** The guard authorises
  the subject for `{app}`; the handler then resolves `{svc}` against that application's own
  reported services and answers 404 otherwise, so a subject scoped to one application cannot
  name a service belonging to another. The reconciler repeats that check rather than trusting
  it, so the rule belongs to the reconciler and not to one HTTP handler.
- **It is bounded, it keeps itself open, and it re-checks.** Sixteen concurrent streams per
  controller, enforced in the handler so the bound covers every implementer of the seam, and a
  **503** past it. An SSE comment frame every twenty seconds, because the console reads this
  with `fetch` and does not reconnect where `/events` uses `EventSource` and does — and each of
  those ticks re-runs authentication and authorisation, so a withdrawn grant reaches a console
  that is already attached. A 256-event buffer that drops rather than blocking, matching what
  `api/stream.go`'s `publish` does for events.
- **A line the controller wrote is marked.** Dropped output and a truncated line are reported
  in the stream with `notice: true`, carrying `stream: "stderr"` so the console's filter does
  not hide them from the operator looking for trouble, and rendered with their own label so
  nobody reads them as container output.
- **The window is two parameters and they travel together (#267).** `?tail=` and `?since=`,
  and `since` without `tail` is a **400**. The daemon selects the last `tail` lines *first*
  and only then drops the ones older than `since`, so a lone `since` asks for six hours and is
  answered with a hundred lines — a control that silently does nothing. `tail` is capped at
  50,000 and `since` at 24 hours, both **refused** rather than clamped. There is no `until`:
  the daemon's swarm log route does not read one, only the container route does.
- **The replica and the node are named, not just identified (#266).** `ServiceLogEvent` also
  carries `nodeHostname` and `slot`, resolved by the reader from a `TaskList` filtered to the
  service and an unfiltered `NodeList` — two lookups filling two maps, so one failing costs
  one label rather than both, and neither ever costs the line. `slot` is absent for a
  global-mode service, whose tasks have no slot at all; the console falls back
  `slot` → `nodeHostname` → truncated `taskID`. An unseen task id re-reads the task listing at
  most once per 30 seconds, because a rollout starts tasks after the stream opened and that is
  when an operator is watching.
- **Event Wire Format:**
  ```json
  {
    "service": "whoami_web",
    "taskID": "qz4k2h8x1n0p",
    "nodeID": "8t3v1m9c7b2d",
    "nodeHostname": "swarm-w1",
    "slot": 3,
    "stream": "stdout",
    "message": "Starting server on :80",
    "timestamp": "2026-08-27T14:35:08.922Z"
  }
  ```

#### 2. Swarm Node Health Roster (`api/nodes.go`)
- **Route:** `GET /api/v1/nodes`
- **Guarded Action:** `ActionNodes`
- **Interface:** `NodeLister` seam (`Nodes(ctx) (application.NodesResponse, error)`)
- **Implemented (#260).** `*reconcile.Reconciler` satisfies `NodeLister`, resolving the local
  swarm through the `swarms` seam and reading the roster behind `capability.NodeRoster` — the
  same whole-swarm snapshot the reconcile loop already takes, so nodes and their task counts
  arrive in one round trip. `controller` asserts the wiring at compile time, because the seam's
  type assertion falls back silently and a renamed method would turn the matrix off rather than
  fail the build.
- **The 501 branch stays, and is still reachable.** A reconciler reached through the same
  interface may not implement the method, and one that does may still meet a backend with no
  roster behind it — resolved per request, not at construction. That second case is reported
  with `application.ErrUnsupported` and answered identically: same status, same sentence, since
  from the caller's side both are "this build does not report that".
- **An empty roster is an error, not an answer.** 501 rather than an empty list because a swarm
  with no nodes is not a thing and a UI cannot tell an empty roster from one it failed to read.
  A locked swarm is the case that makes this load-bearing: `docker.SnapshotWith` answers one
  with an empty snapshot and a *nil* error, so passing it through would draw a locked cluster
  as an empty one.
- **The counts are per node, and are not record counts.** The swarm keeps stopped task records
  — that is how `docker service ps` shows a crash history — so `tasksDesired` counts the tasks
  the orchestrator still wants on that node and `tasksRunning` those that are up. Counting
  every record on a node instead gives a number that only ever grows.
- The first version of this endpoint returned the example below as a literal, and the console
  drew a manager called `swarm-manager-01` at `127.0.0.1` running engine `27.5.1` on every
  deployment that had never been asked about. It is now the shape of a real response.
- **Payload Wire Format:**
  ```json
  {
    "swarm": "",
    "nodes": [
      {
        "id": "node-mgr-01",
        "hostname": "swarm-manager-01",
        "role": "manager",
        "availability": "active",
        "status": "ready",
        "engineVersion": "27.5.1",
        "addr": "127.0.0.1",
        "leader": true,
        "tasksRunning": 1,
        "tasksDesired": 1
      }
    ]
  }
  ```

#### 3. Cluster Diagnostics (`api/diagnostics.go`)
- **Route:** `GET /api/v1/diagnostics`
- **Guarded Action:** `ActionRead`
- **Aggregates:**
  - Cluster integrity score (`0–100`) and operational tone (`ok`, `warn`, `bad`).
  - Clear count vs total check count.
  - Active risk findings with severity and actionable remedies.
  - Health & invariance audit checklist (GitOps manifest convergence, task/service health, runtime configuration drift, and controller state).

---

## 3. Web UI & Operator Console

### 3.1 Monitor 2.0 (`web/ui/src/screens/Monitor.tsx`)
- **Dual Stream Modes:** Segmented pill toggle between **Controller Reconcile Events** and **Service Container Logs**. The log half appears only on a build reporting `capabilities.logs` (#259); everywhere else Monitor is the controller stream, which every build serves.
- **Unified Header Toolbar:** Embedded Application & Service selectors with pulsing `LIVE` / `PAUSED` stream badge.
- **High-Performance Log Viewer:**
  - Tabular column alignment: Line numbers (`001`, `002`) → Gutter timestamps → `STDOUT`/`STDERR` chips → Log messages.
  - Line numbers count the buffer, not the filtered view, so grepping does not renumber the log.
  - Pause holds the view; it does not drop the connection or empty the buffer.
  - Regex and text grep filter with live match count.
  - Stream level filters (`All`, `Stdout`, `Stderr`).
  - Pause / Resume toggle and Clear buffer action.
  - "Jump to Bottom" floating button on scroll-up.
  - "Copy to Clipboard" and "Export (.txt)" tools.
  - Footer telemetry bar: Target, line count, autoscroll status, protocol badge.

### 3.2 Diagnostics 2.0 (`web/ui/src/screens/Diagnostics.tsx`)
- **Integrity Gauge:** Dynamic SVG score ring with status tone.
- **Audit Checklist:** Operational verification matrix with pass/warn chips.
- **Swarm Node Matrix:** Pro node cards displaying server icons, live status dots (`● READY`), 4-column metric grids (Engine version, IP address, Availability, and Tasks Allocated with visual progress tracks). The task gauge is an `<svg>` rect rather than a styled `<div>`: a data-driven width is an inline `style` attribute, which `style-src 'self'` refuses — the same constraint the integrity ring beside it is drawn under.
- **Gated on the capability (#259).** The card is absent altogether on a build reporting `capabilities.nodes: false`, and the roster is not fetched at all there — a card that could only ever be an apology is not a card.
- **Three states, not one,** for a build that does report one. A destination that cannot answer (501), a roster that could not be read, and a swarm reporting no nodes are drawn differently; none of them is an empty grid. The third is unreachable from this build's own backend, which refuses an empty roster rather than serving one, but the state stays: it is what a reconciler reached through the same interface may still produce.

### 3.3 Application Detail & Release Polish (`web/ui/src/screens/ApplicationDetail.tsx`)
- **Balanced Spec Bento Grid:** Equal-height layout for `GitOps Specification` and `Swarm Runtime & Reconcile` with drift state indicator.
- **Release Banner Cards:** Structured headers with package icon, bold release title, chart reference badge, revision chip, and right-aligned status chips.
- **Dense Service Table:** Clean monospace font for service names, replica badges (`2/2`), mode chips, and update status.

### 3.4 Official Brand Mark (`web/ui/src/Shell.tsx`)
- Standardized product wordmark: `>_ SwarmCLI` with elevated `CD` badge.

---

## 4. Verification & Testing
- **Go Backend:** 63 tests passing (`go test -v ./api`).
- **Frontend (Vitest):** 23 test suites and 168 tests passing (`npm test`).
- **TypeScript & ESLint:** 0 errors, 0 warnings (`npm run typecheck`, `npm run lint`).
- **Production Build:** Vite bundle built with zero CSP violations.
