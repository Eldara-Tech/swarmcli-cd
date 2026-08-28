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
- **Interface:** `LogStreamer` seam (`ServiceLogs(ctx, app, svc, tail, follow) (io.ReadCloser, error)`)
- **Unimplemented in this repository.** No reconciler here satisfies `LogStreamer`, so an
  open-source build answers **501** and the console says the controller does not stream logs.
  It must not answer anything else: the first version of this endpoint emitted a synthetic
  line every three seconds — `service X task healthy - 0 active errors` — as container output,
  which is a health claim about a service nothing had contacted.
- **Authorisation is per application, and `{svc}` is not a second grant.** The guard authorises
  the subject for `{app}`; the handler then resolves `{svc}` against that application's own
  reported services and answers 404 otherwise, so a subject scoped to one application cannot
  name a service belonging to another. An implementer must not look `svc` up swarm-wide.
- **What an implementer owes:** one already-demultiplexed log line per line, no 8-byte Docker
  frame header, an optional `stderr\t` prefix where the stream is known, and Docker's RFC3339
  timestamp at the head of the line where `--timestamps` was requested.
- **Event Wire Format:**
  ```json
  {
    "service": "whoami_web",
    "stream": "stdout",
    "message": "Starting server on :80",
    "timestamp": "2026-08-27T14:35:08.922Z"
  }
  ```

#### 2. Swarm Node Health Roster (`api/nodes.go`)
- **Route:** `GET /api/v1/nodes`
- **Guarded Action:** `ActionNodes`
- **Interface:** `NodeLister` seam (`Nodes(ctx) (application.NodesResponse, error)`)
- **Unimplemented in this repository.** No reconciler here satisfies `NodeLister`, so an
  open-source build answers **501** and the Diagnostics matrix says so. The example below is
  the *shape* a response takes, not something this build produces — the first version returned
  exactly that object as a literal, and the console drew a manager called `swarm-manager-01`
  at `127.0.0.1` running engine `27.5.1` on every deployment that had never been asked about.
  501 rather than an empty list, because a swarm with no nodes is not a thing and a UI cannot
  tell an empty roster from one it failed to read.
- **Payload Wire Format:**
  ```json
  {
    "swarm": "local",
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
- **Dual Stream Modes:** Segmented pill toggle between **Controller Reconcile Events** and **Service Container Logs**.
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
- **Three states, not one.** A build with no `NodeLister` (501), a roster that could not be read, and a swarm reporting no nodes are drawn differently; none of them is an empty grid.

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
