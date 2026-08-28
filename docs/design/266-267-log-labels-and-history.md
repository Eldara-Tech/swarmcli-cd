# Per-task labels and log history — design for #266 and #267

Against `swarmcli-cd` `main` @ `d74ef1c` (2026-08-28), the pinned daemon module
`github.com/docker/docker v28.5.2+incompatible`, `github.com/moby/swarmkit/v2
v2.1.2` and CE `github.com/Eldara-Tech/swarmcli/v2 v2.0.0-rc2`. Every claim was
re-verified against those trees. Four claims in the two issues are wrong and are
corrected in §2.

## Contents

1. [Executive summary](#1-executive-summary)
2. [Corrections to the issues](#2-corrections-to-the-issues)
3. [What is true today](#3-what-is-true-today)
4. [Decisions taken](#4-decisions-taken)
5. [The design](#5-the-design)
   - 5.1 [The seam becomes a request struct](#51-the-seam-becomes-a-request-struct)
   - 5.2 [Resolving task and node labels](#52-resolving-task-and-node-labels)
   - 5.3 [The backlog phase blocks; the live phase drops](#53-the-backlog-phase-blocks-the-live-phase-drops)
   - 5.4 [The console row](#54-the-console-row)
   - 5.5 [The scrollback control](#55-the-scrollback-control)
   - 5.6 [Rendering 20,000 rows](#56-rendering-20000-rows)
6. [Verification](#6-verification)
7. [Delivery](#7-delivery)

---

## 1. Executive summary

Two issues, one console, one seam. They are planned together because #267 forces
a seam change that #266's new field would otherwise have to make separately, and
because both land in the same seven files: the four the seam passes through,
`application/status.go` where the wire type lives, and the two in the console.

**#266** — the daemon already tells us which task and which node wrote each line,
`backend/logs.go` already parses both out, and the console renders neither. The
fix adds a replica-slot column (`.3`, as `docker service ps` spells it) with the
node hostname on hover, plus a per-task colour and a task filter. The hostname is
resolved in the backend rather than fetched by the browser, because
`/api/v1/nodes` sits behind a different authz action and its own capability flag.

**#267** — the console reads the last 100 lines and cannot look further back. The
fix adds relative scrollback presets (100 lines, 15m, 1h, 6h), each sending a
`since` and a matching `tail`. It is not "add `since`": `tail` is applied *before*
`since` in the daemon, so `since` alone changes nothing (§2.2). The two must move
as one request, which is what makes the seam a struct.

The seam change is the reason to do these together and now.
`api.LogStreamer` takes `(app, svc, tail, follow)` and would need widening for
`since`, for the second time in two changes. Nothing outside this repository
implements it — verified by grep across `swarmcli-cd-be` — so it becomes a
request struct, mirroring the `capability.ServiceLogRequest` that already exists
one layer down. Breaking change on paper, blast radius is documentation.

Cost: roughly 350 lines of Go, 250 lines of TypeScript, and no new test files —
`backend/logs_test.go`, `api/logs_test.go`, `screens/Monitor.test.tsx` and
`integration-tests/logs_test.go` all exist and all get extended. One
`C1-breaking-change` PR after a design-doc PR, per #261's D10 precedent.

## 2. Corrections to the issues

Four load-bearing premises are wrong. Each changes what has to be built.

### 2.1 `until` does not exist for swarm service logs

#267 says "the daemon supports `since` and `until` on the same call
(`container.LogsOptions`)". The struct has an `Until` field; the swarm path never
reads it.

- `client/service_logs.go` builds its query from `ShowStdout`, `ShowStderr`,
  `Since`, `Timestamps`, `Details`, `Follow`, `Tail`. There is no `until`.
- The daemon's route, `api/server/router/swarm/helpers.go:30-40`, builds
  `container.LogsOptions` from `follow`, `timestamps`, `since`, `tail`, `stdout`,
  `stderr`, `details`. There is no `until`.
- `client/container_logs.go:59` *does* send it. That is the container endpoint,
  not the service one.

**Consequence.** An absolute range is not available. Only a start bound is. An end
bound could only be a client-side trim of lines already streamed, which is a
different feature with a different cost, and it is out of scope here.

### 2.2 `tail` is applied before `since`, so `since` alone does nothing

This is the correction that changes the shape of the feature.

The worker's log reader selects the last N lines first, then filters by time:

- `daemon/logger/loggerutils/logfile.go:404-431` — `tailFiles` collects the last
  `config.Tail` lines from the rotated file set.
- `logfile.go:855+` — `forwarder.Do` then drops each message whose timestamp is
  before `since`.

So `since = now-1h` with the current `tail = 100` returns the same 100 lines the
console shows today, minus any older than an hour. Adding a `since` field and
leaving `tail` at 100 ships a control that appears to do nothing.

**Consequence.** `since` and `tail` are one intent and must travel as one value.
That is why the presets in §5.5 carry both numbers, and why the seam takes a
struct rather than growing a parameter.

### 2.3 The read happens on the worker, not on the manager

#267 says a far-back `since` "reads the whole json-file log for every task of the
service, on a manager holding the raft store".

The manager relays. `daemon/cluster/services.go:428` converts the selector and
subscribes through swarmkit's log broker; the broker dispatches to the agent on
each node holding a task, and
`daemon/cluster/executor/container/adapter.go:507-533` performs the actual
`ContainerLogs` read there, translating `Since` and `Tail` into the local log
driver's options.

**Consequence.** The disk cost is spread across the worker nodes holding tasks.
The manager's cost is gRPC relay bandwidth and the goroutines behind it. Still
worth bounding, but it is not the raft store under load, and the bound belongs on
line volume rather than on manager I/O.

### 2.4 The console cannot simply fetch `/api/v1/nodes`

#266 says the node-to-hostname mapping is available and "the console does not
fetch it here", framing it as a second query on the Monitor screen.

`api/api.go:245-246`:

```go
mux.Handle("GET /api/v1/applications/{app}/services/{svc}/logs", s.guard(authz.ActionLogs, s.serviceLogs))
mux.Handle("GET /api/v1/nodes",                                  s.guard(authz.ActionNodes, s.nodes))
```

`ActionLogs` and `ActionNodes` are separate actions (`authz/authz.go:113,115`).
The roster is also behind its own capability key, `CapabilityNodes`
(`api/capability.go:32`), which is computed from a different type assertion than
`CapabilityLogs`.

**Consequence.** A subject granted logs may be refused the roster with a 403, and
a build wired for logs may report `nodes: false`. A console that fetched it would
need to render correctly for in-flight, forbidden, unsupported and empty — four
states for a label. Resolving in the backend removes all four.

### 2.5 One more thing the issues do not mention

`swarm.Task.Slot` is `omitempty` and is **0 for a global-mode service**. The
daemon itself treats that as the global case
(`daemon/cluster/executor/container/container.go:125-126`, where a zero slot
falls back to the node id for the container name). A replica-slot label must
therefore fall back to the hostname for a global service rather than rendering
`.0` for every task.

## 3. What is true today

**The seam, three layers.**

```
api.LogStreamer          ServiceLogs(ctx, app, svc string, tail int, follow bool)
  └─ reconcile.Reconciler  reconcile/reconcile.go:3210 — re-checks svc, resolves the destination
       └─ capability.ServiceLogReader  ServiceLogs(ctx, capability.ServiceLogRequest)
            └─ backend.Backend         backend/logs.go:92 — the daemon call
```

`capability.ServiceLogRequest` (`capability/capability.go:296`) is already a
struct and its doc comment already names `since` as "the growth it can see coming".
`api.LogStreamer` is the one that is not.

**Who implements `api.LogStreamer`.** `reconcile.Reconciler`, asserted at
`controller/run.go:473`. Nothing else. `swarmcli-cd-be` — which pins
`swarmcli-cd v1.4.0-rc1` — contains no reference to `LogStreamer`, `ServiceLogs`
or `ServiceLogReader`.

**What the backend already extracts.** `backend/logs.go` requests
`Timestamps: true, Details: true`, demultiplexes the daemon's framing, and
`swarmContext` picks `com.docker.swarm.task.id` and `com.docker.swarm.node.id`
out of the attribute prefix while discarding everything else in it — including
operator-configured env values. Both ids reach `application.ServiceLogEvent`
and both reach the browser (`web/ui/src/api/types.ts:392`).

**What the console draws.** `ServiceLogViewer.tsx:349-352` — a line number, a
time, a stream tag, the message. `logBufferLimit = 1000`. The stream filter is
three buttons over `all | stdout | stderr`. There is no `ServiceLogViewer.test.tsx`;
the console's tests live in `screens/Monitor.test.tsx`, which already covers the
notice class, pausing, and the 501 path.

**Two defects in the row, in code #266 has to touch anyway.**
`ServiceLogViewer.tsx:346,349` call `logs.indexOf(log)` inside the render map —
once for the React key and once for the line number. That is O(n²) per paint at a
1000-line buffer, and it is about to be a larger buffer. The key is also not
unique: two lines with the same timestamp and a stale index collide.

## 4. Decisions taken

Confirmed 2026-08-28 before code.

- **D1 — `api.LogStreamer` takes a request struct.** `C1-breaking-change` on an
  exported v1 interface with no implementer outside this repository. Rejected:
  adding `since` as a sixth parameter, which is the growth #267 names as the way
  the decision gets taken by accident.
  The struct is declared in `application`, not in `api`. `api` declares the
  Reconciler interface so that something other than the OSS applier can serve
  these endpoints, and `TestTheApiDoesNotImportTheReconciler` pins that — a
  request type declared in `api` would make the reconciler import the HTTP layer
  to satisfy it, which is why both sentinel errors already live in
  `application`.
- **D2 — the backend resolves the node hostname** and adds `nodeHostname` to
  `application.ServiceLogEvent`. Additive wire field, non-breaking. Rejected: a
  browser fetch of `/api/v1/nodes`, for §2.4; and showing the raw node id, which
  half-answers the issue.
- **D3 — the console shows the replica slot** (`.3`) with the hostname on hover
  and a colour per task. Rejected: a 12-character task id column, which is opaque
  and correlates by eye; and colour-only, which is invisible to Copy and Export.
- **D4 — relative scrollback presets** carrying a `since` and a matching `tail`.
  Rejected: an absolute `since` picker, which needs the operator to know the time
  and caps nothing; and an incident-driven jump, which couples the console to the
  event stream's history and is a larger change.
- **D5 — a historical read still follows.** The backlog is delivered and the
  stream stays open, as today. Rejected: a finite read, which would need its own
  end state — the console renders a closed stream as `ENDED`, which would be
  wrong for a completed range.

- **D6 — the slot map is refreshed when an unseen task id arrives**, throttled to
  one refresh per 30 seconds — but the *initial* build does not count against the
  throttle. The throttle bounds repeated asking, and the first id the map cannot
  name is the signal that it is already stale; making that first refresh wait out
  the interval would leave a rollout's new replica unnamed for up to half a
  minute, which is the window an operator is most likely to be watching. A rollout starts tasks the map does not know, and a
  rollout is when an operator is watching. Rejected: no refresh, which leaves a
  task started after the console opened unnamed for the life of the stream. The
  node map is not refreshed: nodes do not join a swarm on a rollout's timescale.
- **D7 — the console buffer rises to the largest preset's tail, and the render is
  capped at 2,000 rows** (§5.6). Rejected: capping the *presets* near 2,000 lines,
  which would ship a 6-hour button returning twenty minutes of a busy service.
  Also rejected, and it was the first choice: true windowing. It needs a fixed row
  height, so `.line-content` would stop wrapping long lines, and the spacer
  heights cannot be expressed as a class — `style-src 'self'` carries no
  `unsafe-inline`, which this repository's own lint rule states on the `style`
  prop. Capping the render keeps the row as it is, needs no scroll arithmetic,
  and costs only scrolling by hand past 2,000 rows; the search, the task filter,
  Copy and Export still work on all 50,000, which is how an operator finds
  something in six hours of output. The gap is stated above the rows rather than
  hidden.

  **Superseded 2026-08-28 by #270.** The cap is now a starting size rather than a
  ceiling: reaching the top of the drawn rows draws 2,000 more, and the window
  collapses back when the operator returns to the bottom. The reasoning above
  held except for one premise, and it was the load-bearing one — that the
  alternative had to be *true windowing*, with its fixed row height and its
  spacer heights. It does not. `content-visibility: auto` with
  `contain-intrinsic-size: auto` is a class in `index.css`; the browser skips
  layout and paint for an off-screen row and remembers each row's own measured
  height once it has been seen, so wrapped lines keep wrapping and nothing has a
  pixel value to express. See §5.6.
- **D8 — `maxLogStreams` stays at 16.** A history read holds a slot for its
  backlog and then for as long as the tab is open, which is heavier than a tail,
  but a separate bound for history reads needs a second counter and a fairness
  rule for a problem no deployment has reported. Recorded rather than changed.
- **D9 — `maxLogTail` is 50,000 and `maxLogSince` is 24 hours, and both are
  refused rather than clamped.** The tail matches the largest preset, so the
  console cannot ask for something the API refuses. Refusing rather than clamping
  because a client handed the last fifty thousand lines when it asked for a
  million has been answered with something other than what it asked for, with
  nothing in the reply to say so. `maxLogSince` is the companion bound the first
  draft of this document did not have: the tail caps how many lines cross the
  wire, and this caps how much of each node's rotated log set has to be walked to
  find them.
- **D10 — the task colour palette is its own categorical set**, not the existing
  stream and severity colours. Reusing those would make the second task look like
  an error.
- **D11 — this document lands first, the implementation is one PR after it**, as
  #261's D10 did. The seam struct, the two wire fields and the console row cannot
  be split without shipping a control with nothing behind it.

## 5. The design

### 5.1 The seam becomes a request struct

`api/logs.go`:

```go
// ServiceLogRequest names one read of one service's container output.
//
// A struct rather than a parameter list, for the reason capability's own
// request type gives: this interface has now grown twice, and a parameter list
// makes each growth a breaking change on an exported v1 interface. Nothing
// outside this module implements LogStreamer, which is the only window in which
// this can be done at all.
type ServiceLogRequest struct {
	// Tail is how many lines of scrollback to deliver before following.
	Tail int
	// Since bounds how far back those lines may reach. Zero means no bound.
	//
	// It does not stand alone. The daemon selects the last Tail lines *first*
	// and only then drops the ones older than Since, so a Since with the
	// default Tail returns the default Tail — see docs/design/266-267.
	// The caller sets both or neither.
	Since time.Time
	// Follow keeps the stream open for new lines.
	Follow bool
}

type LogStreamer interface {
	ServiceLogs(ctx context.Context, app, svc string, req ServiceLogRequest) (<-chan application.ServiceLogEvent, error)
}
```

`capability.ServiceLogRequest` gains the same `Since` field with the same comment.
`reconcile.Reconciler.ServiceLogs` changes signature and maps one struct to the
other; its `declaredService` re-check is untouched.

The handler reads the window from the query string:

- `?since=15m` — a duration, parsed with `time.ParseDuration`, resolved against
  the server's clock. Not an absolute instant: the browser's clock is not the
  daemon's, and a skewed laptop would silently ask for the wrong window.
- `?tail=N` — an integer, refused outside `[1, maxLogTail]`.
- Absent means today's behaviour: `tail=100`, no `since`.

`maxLogTail`, with `maxLogSince` beside it, bounds what a caller can ask the
swarm for.
It is enforced in the handler rather than behind the seam, for the reason
`maxLogStreams` is: the bound then covers every implementer, including a
companion's.

A rejected alternative worth recording: accepting a preset *name* (`15m`, `1h`)
rather than two numbers. It would keep the two values from ever disagreeing, but
it puts a UI vocabulary into an HTTP API, and a client that wanted 45 minutes
would have to be told no.

### 5.2 Resolving task and node labels

`backend/logs.go` gains one lookup at stream open, before the log call:

```go
// slots maps a task id to the replica number an operator reads in
// `docker service ps`. hosts maps a node id to its hostname.
slots map[string]int
hosts map[string]string
```

Two maps and not one, because the two ids arrive independently. The details
prefix already carries *both* the task id and the node id per line
(`backend/logs.go` `swarmContext`), so the hostname needs only a node lookup —
the task is not on the path to it. Only the slot needs the task.

Built from two targeted calls, not from `docker.SnapshotWith`. The snapshot is
four unfiltered whole-swarm reads — `NodeList`, `ServiceList`, `TaskList`, `Info`
(CE `docker/snapshot.go:123-162`) — and this needs two of them, one of which can
be filtered:

- `TaskList(ctx, swarm.TaskListOptions{Filters: service=<id>})` fills `slots`.
  The service id comes from the `ServiceInspectWithRaw` the reader already
  performs for the TTY check, so there is no second inspect.
- `NodeList(ctx, swarm.NodeListOptions{})` fills `hosts`. Unfiltered, because a
  node id is only resolvable against the whole roster and the roster is small.

That split matters for the degraded cases. A `TaskList` that fails costs the slot
and leaves the hostname; a `NodeList` that fails costs the hostname and leaves the
slot. Neither costs the line.

**A failed lookup is not a failed stream.** A call that errors is logged and
leaves its map empty; the affected label falls back to the id the line already
carries, which is exactly the state before this change. A hostname is a
convenience; the log line is the product.

**Tasks that appear after the maps are built.** A rollout starts tasks `slots`
does not know, and a rollout is when an operator is watching (D6). `slots` is refreshed
when an unseen task id arrives, throttled to at most one refresh per 30 seconds so
a service churning through tasks cannot turn a log stream into a `TaskList` loop.
`hosts` is not refreshed: nodes do not join a swarm on a rollout's timescale, and
one that does is rare enough to be worth a reopened console.
Between the arrival and the refresh, the line carries the short task id — the
honest fallback, and the same thing a lookup failure produces (D6).

**Amended for #271.** As shipped, `slots` recorded only tasks with a slot above
zero, so a global-mode service — whose tasks have no slot at all — presented
every line as a task the map had never seen, and each open console re-read that
service's task list once every 30 seconds for as long as the tab stayed open.
The refresh was answering a question already settled at open. Two changes close
it, both in `backend/logs.go`:

- The zero is recorded rather than dropped. `slots` now says *absent means no
  listing has named this task* and *zero means a listing named it and it has no
  replica number*, which is the distinction the refresh needed and did not have.
  Nothing downstream can tell: `slot` returned 0 for a global task either way,
  the wire field is `omitempty`, and the console already renders an absent slot
  as the node's hostname (§5.4).
- A lookup that failed in a way asking again cannot fix — the manager refused
  the call, the other side does not serve it, it will not parse the request —
  drops the refresh for that stream, which is what this section's `refresh`
  field always claimed and never did.

Everything else here stands: a task that genuinely appears later still triggers
one throttled refresh and is still named by it.

**What goes on the wire.** `application.ServiceLogEvent` gains two fields:

```go
NodeHostname string `json:"nodeHostname,omitempty"`
Slot         int    `json:"slot,omitempty"`
```

Both additive, both `omitempty`, so a client written against the current document
is unaffected. `Slot` is the number and not the rendered label, because `.3` is a
presentation choice and the console is where presentation lives — and because a
global service's `Slot` is 0, which the console must render as the hostname
rather than as `.0` (§2.5).

### 5.3 The backlog phase blocks; the live phase drops

This is the one place where #267 conflicts with a rule #261 settled.

`logSink.send` drops when the channel is full and reports the count as a notice.
That is right for a live stream: a crash-looping container outruns a browser and
the alternative is unbounded buffering. It is wrong for a backlog. A 20,000-line
history read arrives from the daemon far faster than SSE writes it to a browser,
so under the current rule most of a history read would be dropped and the operator
would get a notice saying so — a feature that reliably fails at the thing it was
added for.

The backlog is finite and the live stream is not, and that difference is the whole
justification for the drop rule. So the sink blocks while delivering the backlog
and drops once it is live:

```go
// Blocking is safe here and only here. The backlog is Tail lines and no more,
// so a slow consumer delays a bounded amount of work rather than an unbounded
// one, and cancelling ctx still ends it. The live phase keeps dropping, for
// the reason it always did.
```

**Where the switch happens** is the part with a trap in it. Docker's stream does
not mark the boundary, and the obvious rules are both wrong. A timestamp rule
compares the controller's clock against a worker's and breaks on node clock skew.
A pure count rule — block for the first `Tail` events — is wrong whenever the
service has *less* history than `Tail`: at `tail=20000` against a service with 50
lines, events 51 onward are live and would be blocked on, which is precisely the
stall the drop rule exists to prevent.

So it is a count *and* an idle timer, whichever comes first: the sink blocks
until it has delivered `Tail` events, or until no event has arrived from the
daemon for `logBacklogGrace` — two seconds. Backlog lines arrive back to back
because they are a file being read; live lines do not. The grace period is what
distinguishes "still draining the backlog" from "caught up and idling", and it
needs no clock but the controller's own monotonic one.

A stream held open by a slow client during its backlog is already covered by
`maxLogStreams`, which counts it as one of sixteen.

A rejected alternative: keep dropping, and cap the presets low enough that drops
are unlikely. That trades a correct feature for a smaller diff and leaves the
failure in place at the top of the range.

### 5.4 The console row

The row becomes five spans:

```
001  14:02:11  .3   STDOUT  starting up
002  14:02:11  .1   STDERR  connect refused
003  14:02:12  .3   STDOUT  listening on :8080
```

- **The slot cell** falls back in three steps: `.<slot>` when the slot is known,
  the hostname when it is not — which covers both a global service, whose slot is
  0 by design, and a `TaskList` that failed while `NodeList` succeeded — and the
  first 12 characters of the task id when neither map answered. Its `title`
  carries the full task id and the hostname, so hover gives the long form without
  spending width on it.
- **Colour per task**, assigned by stable order of first appearance from a fixed
  palette in the existing stylesheet, so the same task keeps its colour for the
  life of the console. Colour is the second cue and never the only one — the slot
  cell carries the same information as text, which is what keeps the row readable
  for a colour-blind operator and in Copy and Export.
- **A notice keeps its existing row class** and gets no slot: the controller wrote
  it, no task did.

Copy and Export gain the slot in the same position they already give the tag:

```
[14:02:11] [.3] [STDOUT] starting up
```

**The task filter** is a fourth control next to the stream segment: a select whose
options are the tasks seen so far, plus "all tasks". A select rather than a button
segment because the set is dynamic and unbounded — a rollout adds members while
the operator is reading — and a segment that reflows as tasks come and go is a
control that moves under the cursor. Its empty state is the existing "No lines
match active filter", which already covers a task that has stopped writing.

**The two defects in §3 are fixed while the row is open.** The `indexOf` calls go;
each event gets a monotonic sequence number when it is appended, which serves as
both the React key and the line number, and is stable when the buffer scrolls.

### 5.5 The scrollback control

A segmented control in the toolbar, next to the stream filter:

| Preset | `since` | `tail` |
|---|---|---|
| Tail (default) | none | 100 |
| 15 minutes | 15m | 5,000 |
| 1 hour | 1h | 20,000 |
| 6 hours | 6h | 50,000 |

`tail` is the safety cap and `since` is the intent. A service quiet enough that an
hour fits in 20,000 lines gets the whole hour; a chatty one gets its most recent
20,000 lines within that hour, which is the honest truncation and is what the
daemon would do anyway.

Changing the preset reopens the stream, which empties the buffer — the same thing
changing service already does. That is deliberate and must be said in the UI: a
window change is a new read, not an extension of the current one.

**The numbers are coupled to D7 and to §5.6.** 50,000 lines
at a typical 200 bytes is 10 MB in the browser per open console, and up to sixteen
consoles can be open against one controller. The upper preset may be worth
trimming; that decision is coupled to §5.6.

### 5.6 Rendering 20,000 rows

`ServiceLogViewer` renders every filtered line:

```tsx
filteredLogs.map((log) => <div className="console-line">…</div>)
```

At 1,000 rows that is fine. At 20,000 it is not: the browser holds 20,000 DOM
nodes of five spans each and re-lays them out on every append. The console would
hang on the preset it was added for.

`logBufferLimit` must rise to at least the largest preset's `tail` — otherwise a
20,000-line read is discarded down to 1,000 the moment it arrives, which is the
same failure as not asking for it. So the buffer grows and the *render* has to
stop growing with it.

**True windowing was the first answer and is not the one taken.** Placing every
row by multiplying a fixed row height needs the row to be a fixed height, which
means `.line-content` stops wrapping and a long line scrolls sideways instead —
a visible change to every line the console has ever drawn, made for the benefit
of a preset. And the spacer heights above and below the window are arbitrary
pixel values, which cannot be a class; this UI is served under `style-src 'self'`
with no `unsafe-inline`, and the repository's own lint rule states exactly that
on the `style` prop.

**So the render is capped instead.** The newest `logRenderLimit` (2,000) of the
matching lines are drawn, and the console says so above them:

> Showing the newest 2,000 of 18,432 matching lines. Narrow with the search or
> the task filter to reach the rest; Copy and Export write all of them.

That is a bound on the rendering and on nothing else. The search, the task
filter, Copy and Export all work on the whole buffer, which is the property that
makes the cap acceptable: an operator reading six hours of a service is looking
for something, and the way to find it in twenty thousand lines was never to
scroll. What it costs is scrolling by hand past 2,000 rows, and the banner is
what keeps that a stated limit rather than a silent one.

**Amended 2026-08-28 by #270: the cap is a starting size, not a ceiling.**

The paragraph above rejects windowing on a premise that does not hold. It is
true that placing rows by arithmetic needs a fixed row height, and true that the
spacer heights could not be a class under `style-src 'self'`. What is false is
that those were the only way to stop drawing what nobody is looking at.

`content-visibility: auto` on `.console-line`, with `contain-intrinsic-size:
auto 1.4rem`, hands the whole problem to the browser: an off-screen row is
skipped for layout and paint and stands in at the placeholder height, and once a
row has been on screen its own measured height is remembered — so a line that
wraps to three rows goes on costing three rows' worth of scrollbar. Variable
heights, no arithmetic, no spacers, no pixel value anywhere but in a stylesheet
this repository serves itself. It has been Baseline since September 2024.

That leaves one real cost, and it is not the DOM. React reconciles the drawn
rows on every render, and this console re-renders on every *controller* event as
well as on every arriving chunk, because `useEventStream` holds its state in the
Shell. Fifty thousand children diffed on each of those is a console that stutters
while it streams. So the window is bounded by the operator instead of by a
number:

- at rest, and while following, it draws the newest 2,000 — unchanged;
- leaving the bottom pins the window where it is, so arriving lines are added
  below rather than traded for the oldest drawn row under somebody's eye;
- reaching the top of the drawn rows draws 2,000 more, correcting `scrollTop` by
  what was inserted above so the rows do not jump;
- returning to the bottom collapses it back to 2,000.

The live path — the one that repaints constantly — therefore never draws more
than the original cap, however far back somebody read a minute ago. The growth
happens only while the stream is not the thing being watched.

The anchor is a sequence number rather than an index, because both things it has
to survive move indices under it: the buffer drops its oldest line at 50,000, and
a search narrows what is being indexed into.

The banner stays, reworded. It sits inside the scrolling viewport above the
oldest drawn row, which is exactly where an operator scrolling back arrives —
and where its own sentence is about to stop being true, because reaching it is
what draws more. The footer carries the same counts for anyone who never leaves
the bottom, which is what the banner's placement had always meant it could not
do.

`.line-num` was `min-width: 2rem`, from the thousand-line buffer. Five digits do
not fit, and it is the column every other column is positioned after, so one row
whose number overflowed moved the message on that row alone.

**One more thing the larger buffer broke.** The reader appended one line per
state update, and each append copies the buffer — O(n) per line, which is
quadratic across a 50,000-line history read and would hang the browser on the
one request this feature exists to make. It was survivable at 1,000 lines. Lines
are now batched per `reader.read()` chunk, which is one update per chunk rather
than one per line.

## 6. Verification

**Unit, `backend`** — extending `backend/logs_test.go` (12 tests today). The
existing `fakeAPI` embeds `client.APIClient` nil on purpose, so the new
`TaskList`/`NodeList` calls panic until the fake answers them; that is the
convention working and every existing log test needs the fake extended. New:
a task id in the details prefix is resolved to its slot and the line's node id to
its hostname; a global-mode task (slot 0) carries the hostname and no slot; a
`TaskList` error costs the slot and keeps the hostname, and a `NodeList` error the
reverse, neither failing the stream; an unseen task id triggers at most one
refresh inside the throttle window; `Since` reaches `container.LogsOptions`
alongside `Tail`; the backlog phase blocks rather than dropping; a service with
fewer lines than `Tail` leaves the backlog phase on the grace timer rather than
blocking on live output; and the live phase still drops with its notice.

**Unit, `api`** — extending `api/logs_test.go` (11 tests today), ported to the
request struct. New: `?since=15m&tail=5000` reaches the seam as a resolved
instant and a bounded integer; `?tail=999999` is refused rather than served
quietly reduced; `?since=` without a `?tail=` is refused, because the daemon
would have bounded the window to the default tail; a malformed `since` is a 400
before any header is written, which the handler's existing headers-last
discipline already allows; no query parameters reproduces today's request
exactly.

**Unit, `reconcile`** — the struct is passed through unchanged and
`declaredService` still refuses a service the view does not declare.

**Unit, `web/ui`** — in `screens/Monitor.test.tsx`, beside the existing notice and
pause suites. New: a line carrying a slot renders `.3` and its title carries the
task id and hostname; a line with no slot and no hostname renders the truncated
task id; two tasks get two colours and the same task keeps its colour after the
buffer scrolls; the task filter narrows to one task and back; Copy writes the slot;
selecting a preset reopens the stream with the right query and empties the buffer.
And, for the render cap (D7): a buffer of 20,000 draws a bounded number of rows,
says how many it is holding, draws the *newest* of them, and still finds an
undrawn line through the search and writes it to the clipboard.

**Integration**, `integration-tests/logs_test.go` (build tag `integration`, first
run in CI by definition — there is no daemon here). Extend the existing case: a
service at two replicas, tailed; the lines carry two distinct slots and the
hostnames the daemon reports for those tasks; a `since` narrower than the service's
age returns fewer lines than an unbounded read of the same service.

**Manual** — a rollout with the console open, which is the only way to see the
label refresh; a 6-hour read against a service with hours of output, which is the
only way to see the render cost; a slow client during a large backlog, which is
the only way to see the block-then-drop transition.

## 7. Delivery

This document is the first PR — labels `A3-technical`, `B0-low-priority`,
`C0-breaks-nothing`. The table below is the second, and nothing in it is written
until the decisions above are signed off.

| File | Change |
| --- | --- |
| `api/logs.go` | `ServiceLogRequest`, the seam signature, `since`/`tail` query parsing, `maxLogTail` |
| `api/logs_test.go` | ported to the struct, plus the clamp, the malformed `since`, and the unchanged default |
| `application/status.go` | `NodeHostname` and `Slot` on `ServiceLogEvent` |
| `capability/capability.go` | `Since` on `ServiceLogRequest` |
| `reconcile/reconcile.go` | `ServiceLogs` takes the struct and passes it through |
| `reconcile/logs_test.go` | the struct reaches the reader unchanged |
| `backend/logs.go` | the slot and host maps, the throttled refresh, the block-then-drop sink |
| `backend/logs_test.go` | the fake answers `TaskList` and `NodeList`; labels, degraded lookups, the backlog phase |
| `web/ui/src/api/types.ts` | `nodeHostname`, `slot` |
| `web/ui/src/components/ServiceLogViewer.tsx` | the slot cell, the task filter, the scrollback presets, the render cap, the `indexOf` and per-line-append removals |
| `web/ui/src/index.css` | the slot cell, the task palette, the truncation banner |
| `web/ui/src/screens/Monitor.test.tsx` | the labels, the filter, the presets, the capped render |
| `docs/api.md` | the two query parameters and the two event fields |
| `docs/web-ui.md` | the scrollback control and the task filter |
| `docs/extensibility.md` | the seam's new shape, for an implementer outside this module |
| `docs/PHASE-2.0.md` | the window, the two new event fields, the backlog phase |
| `api/capability_test.go`, `web/ui/src/test/fakeApi.ts` | the fakes the two above need |
| `integration-tests/logs_test.go` | two replicas carry two slots and their hostnames; a narrow `since` reads less |

Labels: `A1-feature`, `B0-low-priority`, `C1-breaking-change`. Both issues close
on it.

`swarmcli-cd` CI runs `ci`, `licence` and `check_labels` only for PRs based on
`main`, so both PRs target `main` directly rather than stacking.
