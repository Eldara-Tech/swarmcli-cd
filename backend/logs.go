// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
)

// The daemon's multiplexed framing: an 8-byte header whose first byte names the
// stream and whose last four are a big-endian payload length.
//
// Named here rather than taken from pkg/stdcopy, whose constants are unexported
// apart from the stream types. stdcopy.StdCopy itself is not what this needs:
// it writes stdout and stderr into two separate writers, which loses the
// interleaving that makes a log readable, and reaching a channel through it
// would cost two pipes and two more goroutines.
const (
	logHeaderLen  = 8
	logStreamByte = 0
	logSizeOffset = 4
)

// The stream each frame belongs to, as the daemon's header names it.
const (
	frameStdout    = 1
	frameStderr    = 2
	frameSystemerr = 3
)

// logChunk is how much of one frame is read at a time.
//
// Frames are not read whole. A frame's declared length is the daemon's number,
// and allocating it would let one line decide how much memory this process
// takes; the sink is fed in pieces instead and caps the line itself.
const logChunk = 32 * 1024

// maxLogLine caps one log line.
//
// A container is entitled to emit a line longer than any buffer — a stack
// trace, a serialised request, one line of JSON — and this used to live in the
// API, where bufio's 64KB default made Scan return false on one and ended the
// whole stream silently. Capped rather than unbounded because the producer is
// not ours; past it the line is delivered truncated and marked, which is a
// worse line but not a lost stream.
const maxLogLine = 1 << 20

// logSlotRefresh is the shortest interval between two task lookups on one
// stream.
//
// The map is built once at open and a rollout starts tasks it does not know —
// which is exactly when an operator is watching — so an unseen task id asks the
// daemon again. Throttled because a service crash-looping through tasks would
// otherwise turn one log stream into a TaskList loop against the manager, and
// the lookup runs on the goroutine draining the daemon connection.
const logSlotRefresh = 30 * time.Second

// logBacklogGrace is how long a read from the daemon must stall before the
// backlog is judged to be over.
//
// See logSink.send for why the phase matters at all. The signal is the *read*
// and not the gap between two events: a slow consumer makes the sink block, and
// the time spent blocked would show up in an event-to-event gap and end the
// backlog phase in the middle of it. A read that stalls means the daemon has
// nothing more to hand over, which is the thing being asked.
//
// Backlog lines arrive back to back because they are a file being read. Two
// seconds is far longer than that and far shorter than the quiet a live service
// falls into. A var so a test does not have to spend it.
var logBacklogGrace = 2 * time.Second

// logBuffer is how many events a client may fall behind by before lines start
// being dropped.
//
// Dropping is the correct failure, for the reason api/stream.go's
// subscriberBuffer gives: a container in a crash loop outruns a browser, and
// the alternative to dropping is buffering the difference for as long as the
// tab stays open. Larger than that one because these are lines rather than
// reconcile events, and a burst of a few hundred is ordinary.
const logBuffer = 256

// The two attributes kept out of the daemon's details prefix.
//
// Everything else in it is discarded rather than folded into the message.
// `details=1` prefixes each line with every attribute the message carries,
// which includes whatever the operator configured through `--log-opt labels=`
// and `--log-opt env=` — that is, selected environment variable *values* — and
// the console must show the text the container wrote and not that.
const (
	attrNodeID = "com.docker.swarm.node.id"
	attrTaskID = "com.docker.swarm.task.id"
)

// ServiceLogs tails one service's container output.
//
// Timestamps and details are always requested. The first is what makes a
// hundred lines of backlog a hundred instants rather than one; the second is
// the only place the daemon states which task and which node a line came from,
// which for a replicated service is the difference between "one node is
// crash-looping" and "this service is broken".
func (b *Backend) ServiceLogs(ctx context.Context, req capability.ServiceLogRequest) (<-chan application.ServiceLogEvent, error) {
	if req.Service == "" {
		return nil, errors.New("no service was named")
	}

	// Before the stream, because it decides how to read it. A service whose
	// tasks have a TTY is served unframed — the daemon multiplexes only when no
	// selected task has one — and reading that as frames yields nothing but
	// header errors. The id comes back from the same call, so naming the tasks
	// below costs no second inspect.
	id, tty, err := b.inspectForLogs(ctx, req.Service)
	if err != nil {
		return nil, err
	}

	rc, err := b.api.ServiceLogs(ctx, req.Service, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Details:    true,
		Follow:     req.Follow,
		Tail:       strconv.Itoa(req.Tail),
		// The daemon's own spelling of an instant on this path — the format
		// daemon/cluster/executor/container/adapter.go writes when it passes a
		// swarm log subscription down to a node's container reader. RFC3339
		// would also parse, and matching the daemon costs nothing.
		Since: sinceParam(req.Since),
	})
	if err != nil {
		return nil, fmt.Errorf("opening the log stream for service '%s': %w", req.Service, err)
	}

	out := make(chan application.ServiceLogEvent, logBuffer)
	go b.pumpLogs(ctx, rc, req.Service, id, tty, req.Tail, out)
	return out, nil
}

// sinceParam renders an instant the way the daemon renders it, and the zero
// time as no bound at all.
func sinceParam(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d.%09d", at.Unix(), at.Nanosecond())
}

// inspectForLogs reports the service's id and whether its tasks are given a
// terminal.
func (b *Backend) inspectForLogs(ctx context.Context, service string) (id string, tty bool, err error) {
	svc, _, err := b.api.ServiceInspectWithRaw(ctx, service, swarm.ServiceInspectOptions{})
	if err != nil {
		return "", false, fmt.Errorf("inspecting service '%s': %w", service, err)
	}
	spec := svc.Spec.TaskTemplate.ContainerSpec
	return svc.ID, spec != nil && spec.TTY, nil
}

// pumpLogs owns the response body and the channel for the life of the stream.
//
// Both are released here and nowhere else, on every exit path, which is what
// makes cancelling the caller's context sufficient to end the stream: the
// daemon request is made with that context, so a cancel fails the read, which
// ends the loop, which closes both.
func (b *Backend) pumpLogs(ctx context.Context, rc io.ReadCloser, service, serviceID string, tty bool, tail int, out chan<- application.ServiceLogEvent) {
	defer close(out)
	defer func() { _ = rc.Close() }()

	sink := &logSink{
		ctx:     ctx,
		out:     out,
		service: service,
		now:     b.now,
		labels:  b.logLabels(ctx, serviceID),
		// The backlog is what the daemon had before it started following, and
		// it is at most `tail` lines. Delivering it is the whole point of a
		// history read, so the sink blocks rather than drops until it is
		// through — see send.
		backlog: tail,
	}

	// The reader reports a stalled read so the sink can stop blocking. Passed
	// as a function rather than read from the reader afterwards because the
	// judgement has to be made while the stream is still running.
	timed := &timedReader{r: rc, now: b.now, stalled: sink.caughtUp, after: logBacklogGrace}

	var err error
	if tty {
		err = readRawLog(timed, sink)
	} else {
		err = readFramedLog(timed, sink)
	}
	sink.flush()

	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// The daemon closed, which is what a --no-follow tail and a removed
		// service both look like.
	case ctx.Err() != nil:
		// The caller went away. Not a failure and not worth a line.
	default:
		// Logged and not sent. The daemon's words name socket paths and
		// internal state, and this stream is reachable by every subject an
		// authorizer grants ActionLogs — the same reasoning api.go applies to
		// the error from a failed open.
		b.log.Error("the service log stream ended with an error", "service", service, "error", err)
	}
}

// timedReader watches how long each read takes and reports a stall.
//
// The stall is how the backlog phase ends. It is measured on the read rather
// than on the interval between two events because the sink blocks while the
// backlog is draining, and blocked time sits between two events but not inside
// a read: the bytes are already on the connection, so a read that returns
// slowly is the daemon having nothing left to hand over.
//
// It reports once and then stops timing, so a live service that goes quiet does
// not pay for a clock read per frame for the rest of the stream.
type timedReader struct {
	r       io.Reader
	now     func() time.Time
	stalled func()
	after   time.Duration
	done    bool
}

func (t *timedReader) Read(p []byte) (int, error) {
	if t.done {
		return t.r.Read(p)
	}
	start := t.now()
	n, err := t.r.Read(p)
	if t.now().Sub(start) >= t.after {
		t.done = true
		t.stalled()
	}
	return n, err
}

// logLabels is what an operator calls the task and the node behind a line.
//
// Two maps and not one, because the two ids arrive independently: the daemon's
// details prefix carries both the task id and the node id per line, so the
// hostname needs only a node lookup and the task is not on the path to it. Only
// the slot needs the task.
//
// That split is what makes the degraded cases partial rather than total. A task
// listing that fails costs the slot and leaves the hostname; a node listing that
// fails costs the hostname and leaves the slot. Neither costs the line, which is
// the product — a label is a convenience, and a stream that refused to open
// because the manager was busy would be a worse answer than an unnamed replica.
type logLabels struct {
	// slots maps a task id to the replica number swarm gave it. Absent means
	// no listing has named this task; zero means a listing named it and it has
	// no replica number, which is what every task of a global-mode service is.
	// Those are two different questions and only the first is worth asking
	// again.
	slots map[string]int
	// hosts maps a node id to its hostname.
	hosts map[string]string

	// refresh re-reads slots and reports whether asking again could ever
	// answer differently. Nil once it could not.
	refresh func(context.Context) (map[string]int, bool)
	// asked is when refresh last ran, so an unseen task id cannot ask more
	// often than logSlotRefresh.
	asked time.Time
	now   func() time.Time
}

// logLabels builds the maps for one stream.
//
// Both lookups are targeted rather than a whole-swarm snapshot: CE's
// docker.SnapshotWith is four unfiltered reads — nodes, services, tasks and
// info — and this needs two, one of which the daemon can filter for us.
func (b *Backend) logLabels(ctx context.Context, serviceID string) *logLabels {
	l := &logLabels{now: b.now, hosts: map[string]string{}}

	if serviceID != "" {
		l.refresh = func(ctx context.Context) (map[string]int, bool) { return b.taskSlots(ctx, serviceID) }
		l.reread(ctx)
	}
	if l.slots == nil {
		l.slots = map[string]int{}
	}
	// asked is deliberately left at the zero time rather than set to now. The
	// throttle exists to bound *repeated* asking, and the first task id this
	// map cannot name is the signal that it is already stale — making the very
	// first refresh wait out the interval would leave a rollout's new replica
	// unnamed for up to half a minute, which is the window an operator is most
	// likely to be watching.

	nodes, err := b.api.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		// Logged and survived. Every line then carries the node id it already
		// carried, which is what the console showed before hostnames existed.
		b.log.Warn("could not name the nodes behind a log stream; lines will carry node ids", "error", err)
		return l
	}
	for _, n := range nodes {
		if n.Description.Hostname != "" {
			l.hosts[n.ID] = n.Description.Hostname
		}
	}
	return l
}

// reread runs the lookup and keeps what it answered.
//
// A failure that asking again cannot fix drops the refresh, so one console
// session does not spend a TaskList and a warning line every thirty seconds on
// a question that has already been answered no.
func (l *logLabels) reread(ctx context.Context) {
	fresh, again := l.refresh(ctx)
	if fresh != nil {
		l.slots = fresh
	}
	if !again {
		l.refresh = nil
	}
}

// taskSlots maps each of the service's task ids to its replica number, and
// reports whether a failure is worth asking about again.
//
// A nil map means the lookup failed, which is deliberately distinct from an
// empty one: empty is a service with no tasks at all, and nil is a question
// that was not answered.
func (b *Backend) taskSlots(ctx context.Context, serviceID string) (map[string]int, bool) {
	tasks, err := b.api.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", serviceID)),
	})
	if err != nil {
		b.log.Warn("could not name the tasks behind a log stream; lines will carry task ids", "service", serviceID, "error", err)
		return nil, !refusedForGood(err)
	}
	out := make(map[string]int, len(tasks))
	for _, t := range tasks {
		// Slot 0 is stored rather than dropped, and the zero is the point. It
		// is what a global-mode service reports — the daemon treats a zero slot
		// as the global case itself — so recording it is how a task known to
		// have no replica number is told apart from a task no listing has
		// named yet. The console is unaffected: 0 is the absent slot it
		// already renders as the node's hostname, and the wire field is
		// omitempty (#271).
		out[t.ID] = t.Slot
	}
	return out, true
}

// refusedForGood reports whether a failed daemon call is one that asking again
// cannot fix.
//
// Only the answers that are about the caller rather than about the moment: an
// authorizer in front of the daemon that does not grant this call, a
// connection that is not authenticated, an endpoint the other side does not
// serve, a request it will not parse. Everything else — a busy manager, a lost
// connection, a leader election — is transient by default, because asking again
// costs one call per interval and refusing to costs the label for the life of
// the stream, and a rollout is both when the label matters and when a manager
// is busy.
func refusedForGood(err error) bool {
	return errdefs.IsPermissionDenied(err) || errdefs.IsUnauthorized(err) ||
		errdefs.IsNotImplemented(err) || errdefs.IsInvalidArgument(err)
}

// slot reports the replica number for a task id, and whether it is known.
//
// An id the map has never seen asks the daemon again, at most once per
// logSlotRefresh: a rollout starts tasks after the stream opened, and a rollout
// is when an operator is reading. The first such id is answered at once and the
// rest of its burst is not, which is the whole of the throttle's job — a service
// crash-looping through tasks would otherwise turn one log stream into a
// TaskList loop against the manager, on the goroutine draining the connection.
//
// A task the listing has already named has its answer even when that answer is
// that it has no slot, because the zero is recorded rather than dropped. So a
// stream tailing a global-mode service reads the task list at open and not
// again, where it used to read it once per interval for as long as the console
// stayed open (#271).
func (l *logLabels) slot(ctx context.Context, taskID string) int {
	// A nil receiver is "nothing is known", not a wiring mistake. Every label
	// is optional by design — the line is the product and the name is the
	// convenience — so a sink assembled without them labels nothing and still
	// delivers everything, which is also what the two failed lookups produce.
	if l == nil || taskID == "" {
		return 0
	}
	if n, ok := l.slots[taskID]; ok {
		return n
	}
	if l.refresh == nil || l.now().Sub(l.asked) < logSlotRefresh {
		return 0
	}
	l.asked = l.now()
	l.reread(ctx)
	return l.slots[taskID]
}

// host reports the hostname for a node id, or the empty string.
//
// Not refreshed, unlike slots. Nodes do not join a swarm on a rollout's
// timescale, and one that does during a single console session is rare enough
// to be worth the reopen.
// A nil receiver answers nothing, for the reason slot gives.
func (l *logLabels) host(nodeID string) string {
	if l == nil {
		return ""
	}
	return l.hosts[nodeID]
}

// readFramedLog reads the daemon's multiplexed stream.
//
// A Systemerr frame is the daemon reporting a failure inside a stream it has
// already begun — "Error grabbing logs: …" — and ends the stream with that text
// as the error rather than delivering it as a log line, which is what it would
// look like to an operator otherwise.
func readFramedLog(r io.Reader, sink *logSink) error {
	var header [logHeaderLen]byte
	buf := make([]byte, logChunk)
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return err
		}
		size := int(binary.BigEndian.Uint32(header[logSizeOffset:]))

		var stream string
		switch header[logStreamByte] {
		case frameStdout:
			stream = "stdout"
		case frameStderr:
			stream = "stderr"
		case frameSystemerr:
			text, err := readN(r, buf, size)
			if err != nil {
				return err
			}
			return fmt.Errorf("the daemon reported an error in the log stream: %s", strings.TrimSpace(text))
		default:
			return fmt.Errorf("unrecognised log frame header %d", header[logStreamByte])
		}

		for size > 0 {
			n := min(size, len(buf))
			if _, err := io.ReadFull(r, buf[:n]); err != nil {
				return err
			}
			sink.write(stream, buf[:n])
			size -= n
		}
	}
}

// readRawLog reads a TTY service's unframed stream.
//
// Every line is stdout, because there is nothing in the bytes to say otherwise:
// the daemon multiplexes only when no selected task has a terminal, and with
// one the two sources were joined before they reached the socket. That is a
// property of the daemon rather than a choice made here, and stdout is the
// honest default for a reader that cannot tell them apart.
func readRawLog(r io.Reader, sink *logSink) error {
	buf := make([]byte, logChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sink.write("stdout", buf[:n])
		}
		if err != nil {
			return err
		}
	}
}

// readN reads exactly n bytes, capped, discarding the remainder. For a frame
// whose content is an error message rather than a log line.
func readN(r io.Reader, buf []byte, n int) (string, error) {
	var b strings.Builder
	for n > 0 {
		c := min(n, len(buf))
		if _, err := io.ReadFull(r, buf[:c]); err != nil {
			return b.String(), err
		}
		if b.Len() < maxLogLine {
			b.Write(buf[:c])
		}
		n -= c
	}
	return b.String(), nil
}

// logSink turns the bytes of one stream into events.
//
// It holds one partial line per stream, because a frame is not guaranteed to
// end on a newline and two frames of different streams can interleave around
// one. Everything about a line — the cap, the parse, the send — happens here so
// that the framed and unframed readers above stay readers.
type logSink struct {
	ctx     context.Context
	out     chan<- application.ServiceLogEvent
	service string
	now     func() time.Time
	labels  *logLabels

	// backlog counts down the lines the sink will block to deliver rather than
	// drop. See send.
	backlog int

	// partial is what has arrived of a line that has not ended yet, per stream.
	partial map[string][]byte
	// overrun marks a stream whose current line passed the cap and was already
	// delivered truncated: the rest of it is discarded up to the next newline,
	// rather than arriving as a second line that was never written as one.
	overrun map[string]bool
	// dropped counts the lines the channel had no room for. Reported to the
	// caller as a notice the moment there is room to report it, so a console
	// showing a gap says that it is one.
	dropped int
}

func (s *logSink) write(stream string, p []byte) {
	if s.partial == nil {
		s.partial = map[string][]byte{}
		s.overrun = map[string]bool{}
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.hold(stream, p)
			return
		}
		s.hold(stream, p[:i])
		if s.overrun[stream] {
			s.overrun[stream] = false
		} else {
			s.emit(stream, string(s.partial[stream]))
		}
		s.partial[stream] = s.partial[stream][:0]
		p = p[i+1:]
	}
}

// hold appends to the line being assembled, delivering it truncated and marked
// if it passes the cap rather than growing to whatever the container writes.
func (s *logSink) hold(stream string, p []byte) {
	if s.overrun[stream] {
		return
	}
	s.partial[stream] = append(s.partial[stream], p...)
	if len(s.partial[stream]) <= maxLogLine {
		return
	}
	s.emit(stream, string(s.partial[stream][:maxLogLine]))
	s.notice("a log line longer than 1MiB was truncated")
	s.partial[stream] = s.partial[stream][:0]
	s.overrun[stream] = true
}

// flush delivers whatever a stream had assembled when the daemon closed. A
// final line with no newline after it is still a line.
func (s *logSink) flush() {
	for stream, line := range s.partial {
		if len(line) > 0 && !s.overrun[stream] {
			s.emit(stream, string(line))
		}
		s.partial[stream] = nil
	}
}

// emit parses one line, names what wrote it, and delivers it.
func (s *logSink) emit(stream, line string) {
	at, taskID, nodeID, message := parseLogLine(line, s.now)
	s.send(application.ServiceLogEvent{
		Service:      s.service,
		TaskID:       taskID,
		NodeID:       nodeID,
		NodeHostname: s.labels.host(nodeID),
		Slot:         s.labels.slot(s.ctx, taskID),
		Stream:       stream,
		Message:      message,
		Timestamp:    at,
	})
}

// caughtUp ends the backlog phase.
//
// Called by the reader when a read from the daemon stalls, which is what the
// end of a backlog looks like from here. Also reached by the count in send, for
// the case where the daemon keeps producing and the stall never comes.
func (s *logSink) caughtUp() { s.backlog = 0 }

// notice delivers a line the controller wrote rather than the container.
func (s *logSink) notice(message string) {
	s.send(application.ServiceLogEvent{
		Service: s.service,
		// stderr so the console's stream filter does not hide it from the
		// operator looking for trouble; Notice so nobody reads it as output.
		Stream:    "stderr",
		Message:   message,
		Timestamp: s.now().UTC(),
		Notice:    true,
	})
}

// send delivers an event, blocking through the backlog and dropping once live.
//
// # Why there are two rules and not one
//
// The live rule came first and is unchanged: a browser that has stopped reading
// must not be able to stall the goroutine draining the daemon connection, so a
// full channel drops the line and counts it, and the count is reported as its
// own notice as soon as there is room. That is what keeps a gap in a console
// visible as a gap.
//
// Applied to a backlog it is wrong, and wrong at exactly the thing a history
// read is for. Twenty thousand lines arrive from the daemon far faster than
// they can be written to a browser, so most of a history read would be dropped
// and the operator would be handed a notice saying so. The justification for
// dropping is that a live stream is unbounded; a backlog is `tail` lines and no
// more, so blocking on it delays a bounded amount of work, and cancelling the
// context still ends it.
//
// The phase ends on whichever comes first: `tail` events delivered, or a read
// from the daemon that stalled (logBacklogGrace). Both are needed. The count
// alone blocks on live output whenever the service has less history than `tail`
// — twenty thousand asked for, fifty in the file, and events fifty-one onward
// are live. The stall alone never fires against a service producing steadily.
func (s *logSink) send(e application.ServiceLogEvent) {
	if s.dropped > 0 && !e.Notice {
		select {
		case s.out <- application.ServiceLogEvent{
			Service:   s.service,
			Stream:    "stderr",
			Message:   fmt.Sprintf("%d log lines were dropped because this client could not keep up", s.dropped),
			Timestamp: s.now().UTC(),
			Notice:    true,
		}:
			s.dropped = 0
		default:
			// Still no room. The count keeps rising and the notice goes out
			// with the true total whenever one line finally fits.
		}
	}
	if s.ctx.Err() != nil {
		return
	}
	if s.backlog > 0 {
		select {
		case s.out <- e:
			s.backlog--
		case <-s.ctx.Done():
		}
		return
	}
	select {
	case s.out <- e:
	default:
		s.dropped++
	}
}

// parseLogLine splits one line of the daemon's output into the four things the
// wire type carries.
//
// The layout is positional and comes from httputils.WriteLogStream: with
// Details the attributes and a space are prefixed, and with Timestamps the
// instant and a space go in front of those. So it is timestamp, attributes,
// message — and cutting twice is safe *only* while the head parses as an
// instant, which is why a line that does not start with one is taken as a
// message in its entirety rather than having its first two words eaten.
func parseLogLine(line string, now func() time.Time) (at time.Time, taskID, nodeID, message string) {
	head, rest, found := strings.Cut(line, " ")
	if !found {
		return now().UTC(), "", "", clean(line)
	}
	at, err := time.Parse(time.RFC3339Nano, head)
	if err != nil {
		return now().UTC(), "", "", clean(line)
	}

	attrs, msg, found := strings.Cut(rest, " ")
	if !found || !strings.Contains(attrs, "=") {
		// Details produced nothing to parse. The daemon always supplies the
		// three swarm context attributes for a service, so this is the shape a
		// stream opened without them has — and the whole remainder is the
		// message rather than a first word that happened to be eaten.
		return at, "", "", clean(rest)
	}
	taskID, nodeID = swarmContext(attrs)
	return at, taskID, nodeID, clean(msg)
}

// swarmContext picks the task and the node out of the daemon's attribute
// prefix, and drops everything else in it.
func swarmContext(attrs string) (taskID, nodeID string) {
	for _, pair := range strings.Split(attrs, ",") {
		k, v, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		key, err := url.QueryUnescape(k)
		if err != nil {
			continue
		}
		if key != attrTaskID && key != attrNodeID {
			continue
		}
		value, err := url.QueryUnescape(v)
		if err != nil {
			continue
		}
		if key == attrTaskID {
			taskID = value
		} else {
			nodeID = value
		}
	}
	return taskID, nodeID
}

// clean removes the control characters a container can put in a log line.
//
// Not for the browser's sake: React escapes a text child, so the console is
// safe either way. It is for the console's Copy and Export, which write these
// bytes verbatim to the clipboard and to a .txt file — and an operator who then
// opens that file in a terminal gets whatever cursor control, colour and title
// sequences the container chose to emit. Tab survives because log output is
// full of it. DEL and C1 are left alone, deliberately: this is a hardening pass
// on one text field and not a sanitiser, and saying so is better than implying
// a guarantee it does not make.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return -1
		}
		return r
	}, s)
}
