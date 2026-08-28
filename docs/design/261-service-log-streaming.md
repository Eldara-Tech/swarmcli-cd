# Service log streaming — design for #261

Against `swarmcli-cd` `main` @ `c18d1d4` (2026-08-28) and the pinned daemon
module `github.com/docker/docker v28.5.2+incompatible`. Every claim below was
re-verified against those two trees rather than taken from the issue text. Two
things the issue says are wrong and are corrected here: there is no
`api/events.go` — the event stream is `api/stream.go` — and the seam is not the
only thing missing, because the wire type carries two fields no byte-oriented
seam can ever populate.

## Contents

1. [Executive summary](#1-executive-summary)
2. [What is actually true today](#2-what-is-actually-true-today)
   - 2.1 [The handler, and what it already guarantees](#21-the-handler-and-what-it-already-guarantees)
   - 2.2 [The seam, and the two dead fields](#22-the-seam-and-the-two-dead-fields)
   - 2.3 [What the daemon actually hands back](#23-what-the-daemon-actually-hands-back)
   - 2.4 [What the console does with it](#24-what-the-console-does-with-it)
3. [The design](#3-the-design)
   - 3.1 [The seam becomes structured](#31-the-seam-becomes-structured)
   - 3.2 [Where the reader lives](#32-where-the-reader-lives)
   - 3.3 [Reading one service](#33-reading-one-service)
   - 3.4 [Backpressure: drop, and say so](#34-backpressure-drop-and-say-so)
   - 3.5 [The concurrency bound](#35-the-concurrency-bound)
   - 3.6 [Keepalive, and why this stream diverges](#36-keepalive-and-why-this-stream-diverges)
   - 3.7 [Authorisation while the stream is open](#37-authorisation-while-the-stream-is-open)
   - 3.8 [Capability reporting and the compile-time assertion](#38-capability-reporting-and-the-compile-time-assertion)
4. [Threat model](#4-threat-model)
5. [Decisions](#5-decisions)
6. [What this does not fix](#6-what-this-does-not-fix)
7. [Verification](#7-verification)
8. [Delivery](#8-delivery)

---

## 1. Executive summary

`GET /api/v1/applications/{app}/services/{svc}/logs` is written, guarded and
tested. It answers 501 because nothing implements `api.LogStreamer`. This design
implements the reader, and changes the seam while doing it.

The seam changes because the byte-oriented contract cannot carry what the wire
type declares. `application.ServiceLogEvent` has `taskID` and `nodeID`; a
reader handing back an `io.ReadCloser` of `stderr\t<timestamp> <message>` lines
has nowhere to put them, and the handler has nothing to parse them out of. The
daemon supplies both, per line, in a form that is thrown away the moment the
reader flattens its work back into bytes. So `LogStreamer` becomes:

```go
type LogStreamer interface {
    ServiceLogs(ctx context.Context, app, svc string, tail int, follow bool) (<-chan application.ServiceLogEvent, error)
}
```

That removes an encode-and-reparse round trip through an `io.Pipe`, removes the
`bufio.Scanner` and `splitLine` from the handler, and moves the one piece of
work with rules rather than plumbing — demultiplexing Docker's framing — to the
one place that has the daemon in front of it.

Everything else follows the shape #262 used for the node roster: a new optional
interface in `capability`, implemented by the moby `backend`, reached by
`reconcile.Reconciler` through the `swarms` registry, asserted at compile time
in `controller/run.go`, and reported by the capability document because the
report is computed from the same type assertions the handlers make.

The four operational questions the issue raised are answered as: a global cap of
16 concurrent streams in the handler, a bounded channel that drops and says so
rather than buffering, a 20-second keepalive comment on this stream only, and a
re-authorisation on each keepalive tick.

## 2. What is actually true today

### 2.1 The handler, and what it already guarantees

`api/logs.go` `serviceLogs` does, in order: resolve `{app}` through
`Reconciler.View`, 404 if absent; check `{svc}` against that application's own
reported services with `declaresService`, 404 with a deliberately identical
sentence if absent; assert `LogStreamer`, 501 if the reconciler is not one; call
the seam, 502 with a fixed sentence if the open fails, the daemon's own words
going to the log only. Headers are written after all of that, so no failure ever
arrives as the first frame of a 200.

The guard on the route is `authz.ActionLogs`, and `api/api.go`'s `guard`
authorises the subject for `r.PathValue("app")` — for the application, and for
nothing else. `declaresService` is therefore load-bearing: it is what makes
`{svc}` a selection inside a grant rather than a second, ungranted one. The
existing tests pin it.

Two more things the handler already gets right and that must survive: one JSON
object per `data:` line, so a log line cannot forge its own frame — `json.Marshal`
escapes every newline, and `TestALogLineCannotForgeItsOwnFrame` pins it — and a
1 MiB line cap, because `bufio.Scanner`'s 64 KiB default returned false on a long
stack trace and ended the whole stream silently.

### 2.2 The seam, and the two dead fields

`application.ServiceLogEvent` (`application/status.go:123`) declares `service`,
`taskID`, `nodeID`, `stream`, `message`, `timestamp`. The handler populates four
of the six. `web/ui/src/api/types.ts:392` mirrors all six, and
`ServiceLogViewer.tsx` renders four.

`taskID` and `nodeID` are not decoration. A replicated service is N tasks on N
nodes and the daemon merges them into one stream; without a per-line label, a
crash loop on one node is indistinguishable from a service that is broken
everywhere. The daemon hands the labels over. The byte contract drops them.

No reconciler in this repository implements `LogStreamer`, and neither does the
private companion — a search of `swarmcli-cd-be` for the interface name and for
a `ServiceLogs` method returns nothing. So the interface is exported, part of a
released v1 module, and implemented by nobody. Changing it is a breaking change
on paper that breaks nothing in practice, which is the only window in which it
can be done at all.

### 2.3 What the daemon actually hands back

Verified in the pinned module, not from memory.

**Framing.** `api/server/router/swarm/helpers.go:71` calls
`httputils.WriteLogStream(ctx, w, msgs, logsConfig, !tty)`, where `tty` is true
if *any* selected service or task has `ContainerSpec.TTY`. With `mux` true each
write is wrapped by `stdcopy.NewStdWriter`: an 8-byte header, byte 0 the stream
(`Stdout` 1, `Stderr` 2, `Systemerr` 3), bytes 4–8 a big-endian payload length
(`pkg/stdcopy/stdcopy.go:18-27`). With `mux` false the stream is raw and the two
sources are indistinguishable.

**A `Systemerr` frame is a daemon error mid-stream**, carrying
`Error grabbing logs: …`. `stdcopy.StdCopy` turns it into a returned error
(`stdcopy.go:169`).

**Timestamps.** With `Timestamps: true` the line is prefixed with
`msg.Timestamp.Format("2006-01-02T15:04:05.000000000Z07:00")` and a space
(`write_log_stream.go:58`). Fixed width, nanosecond padded.

**Details.** With `Details: true` the message's attributes and a space are
prefixed first, and the timestamp then goes in front of those
(`write_log_stream.go:54`), which is the order the example below shows. `attrsByteSlice`
sorts by key and emits `k=v` pairs, comma separated, both halves
`url.QueryEscape`d. `daemon/cluster/services.go:538-556` appends exactly three
attributes for a swarm service, overriding any the container set with the same
keys: `com.docker.swarm.node.id`, `com.docker.swarm.service.id`,
`com.docker.swarm.task.id`, where `contextPrefix` is `"com.docker.swarm"`
(`daemon/cluster/cluster.go:75`). It also passes through whatever the operator
configured with `--log-opt labels=` or `env=`.

So the payload of one multiplexed frame, with both options on, is:

```
2026-08-28T12:06:47.000000000Z com.docker.swarm.node.id=abc,com.docker.swarm.service.id=def,com.docker.swarm.task.id=ghi hello world
```

Positional: timestamp, attributes, message. Cutting twice on a space is safe
because the attributes field is always present when `Details` is set — swarmkit
always supplies the three context keys.

**The client.** `client.ServiceLogs(ctx, serviceID, container.LogsOptions)`
returns the response body and nothing else (`client/service_logs.go:56`), so the
`Content-Type` that would have said whether the stream is multiplexed is not
available. TTY has to be established by inspecting the service.
`ServiceInspectWithRaw` is on the same `client.APIClient` the backend already
holds (`client/client_interfaces.go:180`).

### 2.4 What the console does with it

`ServiceLogViewer.tsx` opens the endpoint with `fetch` and reads
`response.body`. It is **not** an `EventSource`. It splits on a blank line, keeps
lines starting with `data: `, and ignores everything else — so an SSE comment
frame costs it nothing and needs no UI change. When the body ends it sets
`ENDED` and stops. There is no reconnect.

That last sentence is the whole argument in §3.6.

## 3. The design

### 3.1 The seam becomes structured

```go
// api/logs.go
type LogStreamer interface {
    ServiceLogs(ctx context.Context, app, svc string, tail int, follow bool) (<-chan application.ServiceLogEvent, error)
}
```

Same parameters, different return. The producer owns the channel and closes it
when the stream ends; cancelling `ctx` is how the handler stops it, which is why
there is no `Close` to forget — the `io.ReadCloser` version needed a `defer` with
an `_ =` on it purely to satisfy `errcheck`.

What an implementer now owes is smaller and checkable: events in order, `Stream`
one of `stdout` or `stderr`, `Timestamp` the container's instant, `Message` with
no trailing newline, and the channel closed exactly once. What it no longer owes
is a private line encoding.

The handler loses `bufio`, `maxLogLine` and `splitLine`, and gains a `select`
over the channel, the keepalive ticker and `r.Context().Done()`.

### 3.2 Where the reader lives

Three layers, each the same shape as the node roster added in #262:

- **`capability.ServiceLogReader`** — the optional interface a `charts.Backend`
  implements. It takes a *swarm service name*, because a backend knows nothing
  about applications, and it takes it in a struct because that is the rule the
  package states for itself: a capability that needs to grow grows by taking a
  struct rather than by widening a parameter list, and the growth this one can
  already see coming is `since`.

  ```go
  type ServiceLogRequest struct {
      Service string
      Tail    int
      Follow  bool
  }

  type ServiceLogReader interface {
      ServiceLogs(ctx context.Context, req ServiceLogRequest) (<-chan application.ServiceLogEvent, error)
  }
  ```

- **`backend.Backend.ServiceLogs`** — the moby implementation, §3.3.

- **`reconcile.Reconciler.ServiceLogs`** — resolves the application's own
  destination (`swarms.Target{Swarm: spec.Destination.Swarm}`, not the empty
  target `Nodes` uses, because logs are per application and an application can
  name a swarm), asserts `capability.ServiceLogReader`, and returns
  `application.ErrUnsupported` when the backend behind this destination has no
  reader — which the handler answers with the same 501 and the same sentence as
  a reconciler that never had the method.

The reconciler **re-derives the service set from its own view and refuses a name
that is not in it**, duplicating the handler's `declaresService` check. That is
deliberate duplication: the handler's copy is what protects the HTTP surface,
and this one is what makes the guarantee a property of the reconciler rather
than of one caller of it. It is a map lookup on a value already in memory.

### 3.3 Reading one service

```go
b.api.ServiceLogs(ctx, service, container.LogsOptions{
    ShowStdout: true, ShowStderr: true,
    Timestamps: true, Details: true,
    Follow: follow, Tail: strconv.Itoa(tail),
})
```

Then, in one goroutine per stream, owning the channel and the response body:

1. **Establish TTY** before opening the stream, with `ServiceInspectWithRaw`. A
   TTY service's output is unframed and its two sources are indistinguishable, so
   every line is labelled `stdout` — which is what `api/logs.go` already
   documents as the honest default for a reader that cannot tell them apart.
   Only the stream label is lost: `Details` is a separate flag from framing, so
   the attributes prefix is still there and a TTY service's lines still carry
   their task and node.
2. **Read frames** rather than calling `stdcopy.StdCopy`. `StdCopy` writes into
   two separate writers, which loses the interleaving that makes a log readable,
   and would need two pipes and two more goroutines to reach a channel. A frame
   reader is a header, a length, a payload, and a per-stream remainder buffer for
   the case where a frame is not line-aligned. A `Systemerr` frame ends the
   stream with its payload as the error.
3. **Split lines** out of each frame's payload, keeping a remainder per stream.
   A remainder above 1 MiB — the cap `api/logs.go` holds today, moved here — is
   emitted truncated with the notice of §3.4 rather than grown.
4. **Parse positionally**: cut once for the timestamp, once for the attributes,
   the rest is the message. `time.Parse(time.RFC3339Nano, …)`, falling back to
   the read time when it does not parse, exactly as `splitLine` does now.
5. **Keep two attributes**, `com.docker.swarm.node.id` and
   `com.docker.swarm.task.id`, after `url.QueryUnescape`. Everything else in the
   attributes field is dropped rather than folded into the message — see §4,
   information disclosure. `service.id` is dropped too: the caller named the
   service, so echoing its id back adds nothing.
6. **Strip C0 control characters except tab** from the message. §4, tampering.

### 3.4 Backpressure: drop, and say so

The channel is buffered at 256 events. The producer sends non-blocking; on a full
buffer it drops the line and counts it. When a slot next frees it emits one
notice event naming the count, and resets. A crash-looping container therefore
cannot grow the controller's heap, and a browser that has stopped reading cannot
stall the goroutine draining the daemon connection.

This is the same choice `api/stream.go`'s `publish` already makes for events, for
the same reason, and it is why the buffer is a number in one place with a comment
rather than an option.

A notice is a line the container did not write, and must not be mistakable for
one. `application.ServiceLogEvent` gains a `Notice` field, tagged
`notice,omitempty`.
A notice carries `stream: "stderr"`, because an operator filtering to stderr is
looking for trouble and dropped output is trouble, and `notice: true` so the
console can mark it. Widening the `stream` union to a third value was the
alternative and was rejected: the TypeScript type is `'stdout' | 'stderr'`, the
filter has three buttons, and a notice visible only under "All" is a notice the
operator debugging a failure does not see.

### 3.5 The concurrency bound

`Server` gains `logSlots chan struct{}`, buffered at 16, filled in `New`. The
handler takes a slot non-blocking after the 501 assertion and before calling the
seam, releases it on return, and answers 503 with
`too many log streams are open on this controller; try again shortly` when the
pool is empty.

In the handler rather than in the backend, so the bound binds every implementer
of the seam, including a companion's. After the 501 branch, so a build with no
streamer keeps saying so rather than reporting a full pool it does not have.

16 is a per-controller bound on browser tabs and TUI sessions, each of which
holds a goroutine, a channel and one daemon connection. It is a constant, not a
flag: a deployment that needs a different number needs a reason first, and no
such reason exists yet.

### 3.6 Keepalive, and why this stream diverges

The handler emits `: keepalive\n\n` every 20 seconds and flushes.

Every 20 seconds, not 20 seconds after the last line: the tick is also when the
authorisation is re-decided (§3.7), and that has to happen on a busy stream as
well as a quiet one. A comment frame on a chatty console costs 13 bytes a
minute.

`api/stream.go` deliberately has no idle timer, and this stream deliberately
does. The difference is not the traffic pattern — a quiet container and a quiet
controller are both normal — it is the client. A browser opens `/events` with
`EventSource`, which reconnects on its own, so a proxy closing an idle event
stream is invisible. `ServiceLogViewer` uses `fetch` and a reader, does not
reconnect, and renders the close as `ENDED` until the operator changes tab and
comes back. nginx's `proxy_read_timeout` defaults to 60 seconds, so without this
every log console watching a service that logs hourly dies a minute after it is
opened.

20 seconds is a third of that default, so one lost frame still leaves 20 seconds
of margin. The comment frame is ignored by the console's parser as written —
`ServiceLogViewer.tsx` keeps only lines starting with `data: ` — so this needs no
UI change, and the same is true of any conforming SSE client.

One more header belongs with this. `api/stream.go` sets `X-Accel-Buffering: no`,
which is what nginx reads to stop buffering a response, and `api/logs.go` does
not — so a proxied console can sit silent while the proxy accumulates. The log
stream sets it too.

### 3.7 Authorisation while the stream is open

On each keepalive tick, before writing the comment, the handler re-runs
`Authenticate` and `Authorize(ActionLogs, app)` and ends the stream if either
now fails.

A log stream is the longest-lived authorised thing this API serves. Checked only
at open, a withdrawn grant or a rotated admin token has no effect on a console
already attached — it keeps reading container output for as long as the tab is
open. `api/stream.go` already re-decides per event for the same reason, and says
so: "a subject whose access is withdrawn stops being sent things without the
connection having to be noticed and dropped".

Per tick rather than per line, because the authorizer is in-process but a
crash-looping container is thousands of lines a second, and 20 seconds of
residual access is the same window the keepalive already establishes.

### 3.8 Capability reporting and the compile-time assertion

`controller/run.go` gains `var _ api.LogStreamer = rec` beside the existing
`var _ api.NodeLister = rec`, for the reason stated there: the type assertion
inside `api` falls back silently, so a renamed or re-signed method would turn the
console off rather than fail the build.

`capabilityReport` needs no change — it is computed from the same assertion — but
`capabilities.logs` becomes `true` for the Apache-2.0 build, which makes the
console draw the control it currently omits (#259). `docs/api.md`'s sample
document says `"logs": false` and has to change; `docs/PHASE-2.0.md` §2.2 says
"Unimplemented in this repository" and must be rewritten the way the node roster
entry was after #260, keeping the note about the synthetic health line the first
version emitted.

## 4. Threat model

This is a new path into a process holding the Docker socket, streaming
attacker-influenced bytes to a browser. STRIDE, against the design above.

**Spoofing.** Unchanged at open: bearer credential through `guard`. The new
exposure is duration — an authorised stream outliving the authorisation. §3.7 is
the mitigation and bounds the residual window at 20 seconds. Not mitigated: a
credential revoked between two ticks is honoured until the next one.

**Tampering.** Log content is written by whatever the application runs, and
reaches a browser. Three surfaces:

- *SSE frame injection* — impossible and already tested. One JSON object per
  `data:` line, and `json.Marshal` escapes newlines, so a line containing
  `\n\ndata: {…}` cannot close its frame.
- *HTML injection* — React escapes text children, and the message is rendered as
  one.
- *Terminal escape sequences* — the console's Copy and Export write the message
  bytes verbatim to the clipboard and to a `.txt`, and an operator who `cat`s that
  file gets whatever the container emitted, including cursor control and title
  setting. Mitigated by stripping C0 control characters except tab in the reader
  (§3.3 step 6). Tab survives because log output is full of it; DEL and C1 are
  left alone as a deliberate limit — this is a hardening pass on a text field,
  not a sanitiser.

**Repudiation.** Controller-generated lines are marked `notice: true` (§3.4), so
the one class of line in the stream that the container did not write cannot be
read back as evidence that it did.

**Information disclosure.** Four:

- *Another application's logs.* The guard authorises `{app}`; `{svc}` is resolved
  against that application's own reported services in the handler, and again in
  the reconciler (§3.2). The value that reaches the daemon is the name matched
  out of the view, not the raw path value, so nothing the caller wrote is
  concatenated into a daemon request path.
- *Log-driver attributes.* `Details: true` makes the daemon prefix each line with
  every attribute the message carries, which includes whatever the operator
  configured through `--log-opt labels=` and `--log-opt env=` — that is, selected
  environment variable *values*. The reader keeps two keys and drops the rest, so
  the console shows the same message text it would have shown without `Details`.
- *Daemon error text.* The existing 502 branch already logs in full and answers a
  fixed sentence; the same discipline applies to a `Systemerr` frame arriving
  mid-stream, which is logged and ends the stream rather than being emitted as a
  line.
- *Service names.* A subject scoped to one application learns that application's
  swarm service names — which the status endpoint already tells them.

**Denial of service.** The category the issue was right to hold this for:

- *Stream count* — 16, §3.5. Each stream is one goroutine, one channel and one
  daemon connection.
- *Unbounded buffering* — 256 events, drop and count, §3.4.
- *A single enormous line* — 1 MiB, truncate and notice, §3.3 step 3.
- *A client that stops reading* — cannot block the producer, §3.4.
- *Goroutine and connection leak* — the producer selects on `ctx.Done()`, closes
  the response body and closes the channel on every exit path; the handler
  cancels through the request context. Pinned by a test that opens, disconnects,
  and asserts the body was closed.
- *A service with no readable log driver* — `none`, or a driver with no read
  support, makes the daemon fail the open, which is the existing 502.

**Elevation of privilege.** The reader calls exactly two daemon methods,
`ServiceInspectWithRaw` and `ServiceLogs`, both reads. It accepts no service id
and no task id from the caller — only a name that the application's own status
reported — so there is no path from this endpoint to naming an arbitrary object
in the swarm.

## 5. Decisions

Settled 2026-08-28, before code. Recorded with the reasoning kept, so a later
reader sees what was traded away rather than only what was chosen.

- **D1 — the seam becomes a channel of `application.ServiceLogEvent`.** The
  byte contract cannot carry `taskID`/`nodeID`, and re-encoding structured data
  into lines for the handler to re-parse is a round trip with no beneficiary.
  Breaking change to an exported v1 interface; nothing implements it, here or in
  the companion, so the blast radius is documentation. `C1-breaking-change`.
- **D2 — global cap of 16 concurrent streams, enforced in the handler**, 503 when
  full, taken after the 501 branch so an unwired build still says it is unwired.
  Per-subject caps were the alternative and were rejected for now: they need a
  key off `authz.Subject` and a map with its own lifecycle, to solve a fairness
  problem no deployment has reported.
- **D3 — a 20-second keepalive comment on this stream only.** §3.6. The
  divergence from `api/stream.go` is written down there and here, and its cause
  is the client, not the traffic.
- **D4 — backpressure drops with a marked notice**, 256-event buffer, matching
  what `publish` already does for events.

- **D5 — the reconciler re-checks `svc` against its own view.** Duplicates the
  handler's check on purpose, so the guarantee belongs to the reconciler rather
  than to one caller.
- **D6 — re-authorise on each keepalive tick** (§3.7). The alternative is to
  authorise once at open and document that a withdrawn grant does not reach an
  attached console.
- **D7 — `ServiceLogEvent` gains `notice`,** and a notice carries
  `stream: "stderr"`. The alternative was a third `stream` value, rejected
  because the console's filter would hide it from the operator most likely to
  need it.
- **D8 — strip C0 control characters except tab** (§4, tampering). The
  alternative is to pass container bytes through verbatim, as CE's TUI does, and
  accept that Export writes escape sequences into a file an operator may `cat`.
- **D9 — the console marks a notice line.** One class on the existing row plus a
  test; without it a notice renders as an ordinary red stderr line whose text
  happens to be ours. Small enough to keep in this change rather than defer.
- **D10 — this document lands first, the implementation is one PR after it.**
  The code half does not split further: the seam, the three layers, the handler
  changes, the wire field, the UI mark and the docs move together, because the
  capability document flips to `true` the moment the reconciler gains the method
  and a split would ship a console control with nothing behind it.

## 6. What this does not fix

- **Per-task selection.** The daemon merges a replicated service's tasks and this
  design labels each line with its task and node; it does not let the operator
  ask for one task. That is a UI feature with an API question behind it, and the
  labels are its precondition.
- **`since` / `until` / history.** `tail` is 100 and `follow` is true, as today.
  Reading further back is a different endpoint shape.
- **TTY services get no stderr.** Unframed output cannot be split, so every line
  is labelled `stdout`. The task and node labels survive. This is a daemon
  property, not a choice.
- **Multi-swarm.** The reconciler resolves the application's destination, so a
  companion registry serving a second swarm works as soon as its backend
  implements `capability.ServiceLogReader`. The OSS registry serves one swarm and
  that does not change.
- **A revoked credential between ticks** (§4, spoofing).
- **Scrollback.** The console keeps 1000 lines in memory, and this does not
  change it.

## 7. Verification

**Unit, `backend`** — a fake `client.APIClient` returning hand-built frames:
stdout and stderr interleaved in one stream come out in order and correctly
labelled; a frame that is not line-aligned joins with the next; a line above the
cap is truncated once with a notice rather than growing; the attributes prefix is
parsed into `taskID`/`nodeID` and everything else in it is dropped, including an
operator label carrying a secret-looking value; a `Systemerr` frame ends the
stream with its text in the error and not in a line; a TTY service takes the
unframed path and every line is `stdout`; a full channel drops and emits exactly
one notice naming the count; cancelling the context closes the body and the
channel.

The fake follows this package's existing convention: `client.APIClient`
embedded nil, so a method the reader starts calling and the fake does not answer
panics rather than returning a zero value the assertions then pass on.

**Unit, `api`** — the existing suite, ported to the channel seam: the 404s, the
501, the 502, one frame per event, the forged-frame test, and the long-line test
in its new home. New: the 17th stream gets 503 and the 16th does not; a stream
with no output writes a keepalive comment and nothing else; a subject whose
authorisation is withdrawn has its stream ended at the next tick; a disconnected
client does not leak the producer.

**Unit, `reconcile`** — an application whose destination backend has no reader
returns `ErrUnsupported`; a service name the view does not declare is refused
even though the handler would have caught it first.

**Integration, `integration-tests/logs_test.go`** (build tag `integration`, first
run in CI by definition — there is no daemon here). A chart whose service runs
`sh -c 'echo out; echo err >&2; sleep 3600'`, deployed by the real reconciler
against the real swarm, then tailed: both streams appear, each labelled
correctly, each timestamp is the container's rather than the read time — asserted
as *earlier than* the read, which is the only form of that assertion that can
fail — and `taskID` and `nodeID` are non-empty and match the task the daemon
reports for that service.

**Manual** — the console against a live swarm: a chatty service, a quiet one left
open past a minute through a proxy, and a crash loop, which is the only way to
see a drop notice.

## 8. Delivery

This document is the first PR. The table below is the second, and nothing in it
is written until the decisions above are signed off.

| File | Change |
| --- | --- |
| `api/logs.go` | seam signature, `select` loop, keepalive, `X-Accel-Buffering`, re-auth, slot acquire; `splitLine`, `bufio`, `maxLogLine` removed |
| `api/api.go` | `logSlots` on `Server`, filled in `New` |
| `api/logs_test.go` | ported, plus cap, keepalive, withdrawal, leak |
| `application/status.go` | `Notice` on `ServiceLogEvent` |
| `capability/capability.go` | `ServiceLogReader` |
| `backend/logs.go`, `backend/logs_test.go` | the reader |
| `reconcile/reconcile.go` | `ServiceLogs`, beside `Nodes` |
| `controller/run.go` | `var _ api.LogStreamer = rec` |
| `backend/backend.go` | the compile-time capability assertion |
| `reconcile/logs_test.go` | the destination and the second declared-service check |
| `web/ui/src/api/types.ts`, `ServiceLogViewer.tsx`, `index.css` | `notice`, and its mark |
| `web/ui/src/screens/Monitor.test.tsx` | the notice is labelled, and survives the stderr filter |
| `docs/api.md`, `docs/PHASE-2.0.md` | `capabilities.logs`, and the seam's new shape |
| `integration-tests/logs_test.go` | the live-swarm tail |

Labels: `A1-feature`, `B0-low-priority`, `C1-breaking-change`.
