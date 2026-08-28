// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
)

// logAPI serves one scripted log stream.
//
// client.APIClient embedded nil for the reason fakeAPI gives: a daemon call
// this reader starts making and the fake does not answer panics naming the
// method, rather than returning a zero value the assertions then pass on.
type logAPI struct {
	client.APIClient

	body       io.Reader
	tty        bool
	logsErr    error
	inspectErr error

	// serviceID is what ServiceInspectWithRaw reports, and what TaskList is
	// expected to be filtered by. Empty means the daemon named no id, which is
	// the one case where the reader must not ask for tasks at all.
	serviceID string
	// tasks and nodes are what the two label lookups find. A nil slice with the
	// matching error set is a lookup that failed, which must cost its own label
	// and nothing else.
	tasks    []swarm.Task
	nodes    []swarm.Node
	tasksErr error
	nodesErr error
	// onTaskList lets a test answer the nth call differently, which is how the
	// refresh — a second lookup finding what the first could not — is exercised.
	// A nil result falls through to tasks.
	onTaskList func(call int) []swarm.Task

	gotService string
	gotOpts    container.LogsOptions
	// taskFilters records what each TaskList call was filtered by, so a test can
	// assert both the filter and how many times the throttle let one through.
	taskFilters []filters.Args
	closed      chan struct{}
}

func (a *logAPI) ServiceInspectWithRaw(_ context.Context, service string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	if a.inspectErr != nil {
		return swarm.Service{}, nil, a.inspectErr
	}
	return swarm.Service{
		ID: a.serviceID,
		Spec: swarm.ServiceSpec{
			Annotations:  swarm.Annotations{Name: service},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{TTY: a.tty}},
		},
	}, nil, nil
}

func (a *logAPI) TaskList(_ context.Context, opts swarm.TaskListOptions) ([]swarm.Task, error) {
	a.taskFilters = append(a.taskFilters, opts.Filters)
	if a.tasksErr != nil {
		return nil, a.tasksErr
	}
	if a.onTaskList != nil {
		if out := a.onTaskList(len(a.taskFilters)); out != nil {
			return out, nil
		}
	}
	return a.tasks, nil
}

func (a *logAPI) NodeList(_ context.Context, _ swarm.NodeListOptions) ([]swarm.Node, error) {
	if a.nodesErr != nil {
		return nil, a.nodesErr
	}
	return a.nodes, nil
}

func (a *logAPI) ServiceLogs(_ context.Context, service string, opts container.LogsOptions) (io.ReadCloser, error) {
	a.gotService, a.gotOpts = service, opts
	if a.logsErr != nil {
		return nil, a.logsErr
	}
	a.closed = make(chan struct{})
	return &closeOnce{Reader: a.body, closed: a.closed}, nil
}

type closeOnce struct {
	io.Reader
	closed chan struct{}
	done   bool
}

func (c *closeOnce) Close() error {
	if !c.done {
		c.done = true
		close(c.closed)
	}
	return nil
}

// frame wraps a payload the way the daemon's multiplexer does: an 8-byte
// header, the stream in the first byte, a big-endian length in the last four.
func frame(stream byte, payload string) string {
	var h [8]byte
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return string(h[:]) + payload
}

// swarmLine is one line as the daemon writes it with timestamps and details on:
// the instant, the attributes, the message.
func swarmLine(ts, node, task, message string) string {
	return ts + " com.docker.swarm.node.id=" + node +
		",com.docker.swarm.service.id=svc-1,com.docker.swarm.task.id=" + task +
		" " + message + "\n"
}

func drain(t *testing.T, ch <-chan application.ServiceLogEvent) []application.ServiceLogEvent {
	t.Helper()
	var out []application.ServiceLogEvent
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-time.After(5 * time.Second):
			t.Fatal("the stream never closed")
		}
	}
}

func tailFor(t *testing.T, api *logAPI) []application.ServiceLogEvent {
	t.Helper()
	b := testBackend(t, api, nil)
	ch, err := b.ServiceLogs(context.Background(), capability.ServiceLogRequest{Service: "edge_web", Tail: 100, Follow: true})
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}
	return drain(t, ch)
}

// The daemon interleaves the two streams in one connection, and reading them
// into two writers — which is what stdcopy.StdCopy does — loses the order that
// makes a log readable.
func TestBothStreamsArriveLabelledAndInOrder(t *testing.T) {
	api := &logAPI{body: strings.NewReader(
		frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "listening on :8080")) +
			frame(frameStderr, swarmLine("2026-08-28T06:00:01.5Z", "node-b", "task-2", "connection reset")) +
			frame(frameStdout, swarmLine("2026-08-28T06:00:02Z", "node-a", "task-1", "served /")),
	)}

	got := tailFor(t, api)

	if len(got) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(got), got)
	}
	if got[0].Stream != "stdout" || got[1].Stream != "stderr" || got[2].Stream != "stdout" {
		t.Errorf("streams = %q %q %q", got[0].Stream, got[1].Stream, got[2].Stream)
	}
	if got[1].Message != "connection reset" {
		t.Errorf("message = %q", got[1].Message)
	}
	// The container's instant, not the read time: a hundred lines of backlog
	// stamped now is a hundred things happening at once.
	want := time.Date(2026, 8, 28, 6, 0, 1, 500000000, time.UTC)
	if !got[1].Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want %v", got[1].Timestamp, want)
	}
	// The two labels the byte-oriented seam could not carry, and the reason a
	// crash loop on one node used to look like a broken service.
	if got[1].TaskID != "task-2" || got[1].NodeID != "node-b" {
		t.Errorf("task/node = %q/%q, want task-2/node-b", got[1].TaskID, got[1].NodeID)
	}
	if got[0].Service != "edge_web" {
		t.Errorf("service = %q", got[0].Service)
	}
}

// Both are asked for on every open. Timestamps because the alternative is
// stamping the read time; details because it is the only place the daemon says
// which task a line came from.
func TestTheOpenAsksForTimestampsAndDetails(t *testing.T) {
	api := &logAPI{body: strings.NewReader("")}
	b := testBackend(t, api, nil)

	ch, err := b.ServiceLogs(context.Background(), capability.ServiceLogRequest{Service: "edge_web", Tail: 42, Follow: true})
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}
	drain(t, ch)

	if api.gotService != "edge_web" {
		t.Errorf("service = %q", api.gotService)
	}
	if !api.gotOpts.Timestamps || !api.gotOpts.Details {
		t.Errorf("opts = %+v, want timestamps and details", api.gotOpts)
	}
	if !api.gotOpts.ShowStdout || !api.gotOpts.ShowStderr {
		t.Errorf("opts = %+v, want both streams", api.gotOpts)
	}
	if api.gotOpts.Tail != "42" || !api.gotOpts.Follow {
		t.Errorf("tail/follow = %q/%v", api.gotOpts.Tail, api.gotOpts.Follow)
	}
}

// A frame is a write, not a line. The daemon usually sends one message per
// frame, and a reader that assumes it splits a long line in two.
func TestALineSplitAcrossFramesIsOneEvent(t *testing.T) {
	line := swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "a message in two halves")
	api := &logAPI{body: strings.NewReader(
		frame(frameStdout, line[:20]) + frame(frameStdout, line[20:]),
	)}

	got := tailFor(t, api)

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(got), got)
	}
	if got[0].Message != "a message in two halves" {
		t.Errorf("message = %q", got[0].Message)
	}
}

// The details prefix carries whatever the operator configured with
// `--log-opt labels=` and `--log-opt env=`, which is selected environment
// variable *values*. The console must show the text the container wrote.
func TestOnlyTheSwarmContextSurvivesTheDetailsPrefix(t *testing.T) {
	payload := "2026-08-28T06:00:00Z com.docker.swarm.node.id=node-a," +
		"com.docker.swarm.task.id=task-1,env=DB_PASSWORD%3Dhunter2 the real message\n"
	api := &logAPI{body: strings.NewReader(frame(frameStdout, payload))}

	got := tailFor(t, api)

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(got), got)
	}
	if got[0].Message != "the real message" {
		t.Errorf("message = %q, want the container's own text", got[0].Message)
	}
	if strings.Contains(got[0].Message, "hunter2") {
		t.Error("an operator-configured log attribute reached the console")
	}
	if got[0].TaskID != "task-1" || got[0].NodeID != "node-a" {
		t.Errorf("task/node = %q/%q", got[0].TaskID, got[0].NodeID)
	}
}

// A line the daemon did not stamp keeps all of its text rather than losing its
// first two words to a parse that had nothing to parse.
func TestALineWithNoTimestampKeepsItsText(t *testing.T) {
	api := &logAPI{body: strings.NewReader(frame(frameStdout, "no timestamp here\n"))}

	got := tailFor(t, api)

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(got), got)
	}
	if got[0].Message != "no timestamp here" {
		t.Errorf("message = %q", got[0].Message)
	}
	if got[0].Timestamp.IsZero() {
		t.Error("an unstamped line arrived with no time at all")
	}
}

// The console's Copy and Export write these bytes verbatim to the clipboard and
// to a file an operator may open in a terminal.
func TestControlCharactersDoNotSurviveALine(t *testing.T) {
	payload := swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "clean\x1b]0;pwned\x07\x1b[31mred\x1b[0m\tkept")
	api := &logAPI{body: strings.NewReader(frame(frameStdout, payload))}

	got := tailFor(t, api)

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(got), got)
	}
	if strings.ContainsRune(got[0].Message, 0x1b) || strings.ContainsRune(got[0].Message, 0x07) {
		t.Errorf("message = %q, which still carries terminal control", got[0].Message)
	}
	// Tab survives: log output is full of it, and stripping it would reformat
	// every line that uses one as a separator.
	if !strings.Contains(got[0].Message, "\tkept") {
		t.Errorf("message = %q, want the tab kept", got[0].Message)
	}
}

// A container is entitled to emit a line longer than any buffer. Truncating it
// with a notice is a worse line; ending the stream, which is what bufio's
// default did in the API, is a console that looks live and receives nothing.
func TestAnEnormousLineIsTruncatedAndSaidSo(t *testing.T) {
	huge := strings.Repeat("x", maxLogLine+4096)
	api := &logAPI{body: strings.NewReader(
		frame(frameStdout, "2026-08-28T06:00:00Z attrs=none "+huge+"\n") +
			frame(frameStdout, swarmLine("2026-08-28T06:00:01Z", "node-a", "task-1", "after the long one")),
	)}

	got := tailFor(t, api)

	if len(got) != 3 {
		t.Fatalf("events = %d, want the truncated line, its notice and the next line: %+v", len(got), summarise(got))
	}
	if len(got[0].Message) > maxLogLine {
		t.Errorf("the long line arrived at %d bytes, uncapped", len(got[0].Message))
	}
	if !got[1].Notice {
		t.Errorf("second event = %+v, want the truncation notice", got[1])
	}
	// The tail of the over-long line is discarded rather than delivered as a
	// second line the container never wrote.
	if got[2].Message != "after the long one" {
		t.Errorf("third message = %q", got[2].Message)
	}
}

// A TTY service is served unframed — the daemon multiplexes only when no
// selected task has a terminal — and reading that as frames yields nothing but
// header errors.
func TestATTYServiceIsReadUnframed(t *testing.T) {
	api := &logAPI{
		tty: true,
		body: strings.NewReader(
			swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "first") +
				swarmLine("2026-08-28T06:00:01Z", "node-a", "task-1", "second"),
		),
	}

	got := tailFor(t, api)

	if len(got) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(got), summarise(got))
	}
	for _, e := range got {
		// Nothing in the bytes says otherwise, and stdout is the honest default
		// for a reader that cannot tell the two apart.
		if e.Stream != "stdout" {
			t.Errorf("stream = %q, want stdout", e.Stream)
		}
	}
	// The labels survive: details is a separate flag from framing.
	if got[0].TaskID != "task-1" {
		t.Errorf("task = %q, want it kept on the unframed path", got[0].TaskID)
	}
}

// The daemon reports a failure inside a stream it has already begun as a
// Systemerr frame. Delivered as a log line it reads as something the container
// wrote.
func TestADaemonErrorMidStreamIsNotALogLine(t *testing.T) {
	api := &logAPI{body: strings.NewReader(
		frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "up")) +
			frame(frameSystemerr, "Error grabbing logs: node is down\n"),
	)}

	got := tailFor(t, api)

	for _, e := range got {
		if strings.Contains(e.Message, "Error grabbing logs") {
			t.Errorf("the daemon's own error arrived as a log line: %q", e.Message)
		}
	}
	if len(got) != 1 {
		t.Errorf("events = %d, want only the line that really was one: %+v", len(got), summarise(got))
	}
}

// A failed open is the caller's to report as a status. It must not become a
// stream that opens and immediately ends, which a console draws as a service
// that has said nothing.
func TestAFailedOpenIsAnError(t *testing.T) {
	b := testBackend(t, &logAPI{logsErr: errBoomLogs}, nil)

	if _, err := b.ServiceLogs(context.Background(), capability.ServiceLogRequest{Service: "edge_web", Tail: 10, Follow: true}); err == nil {
		t.Fatal("a daemon that refused the stream produced no error")
	}
}

// Cancelling the caller's context is the whole of how a stream is stopped, so
// it has to reach both the daemon connection and the channel.
func TestCancellingTheContextClosesTheBodyAndTheChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	api := &logAPI{body: blockingReader{ctx: ctx}}
	b := testBackend(t, api, nil)

	ch, err := b.ServiceLogs(ctx, capability.ServiceLogRequest{Service: "edge_web", Tail: 10, Follow: true})
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}
	cancel()

	// The reader unblocks on cancel the way an http body does; draining is what
	// proves the channel was closed rather than left for the caller to wait on.
	drain(t, ch)
	select {
	case <-api.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon connection was never closed")
	}
}

// A container in a crash loop outruns a browser. Dropping is the correct
// failure — the alternative buffers the difference for as long as the tab is
// open — and a console showing a gap has to be told it is one.
func TestAFullChannelDropsAndSaysSo(t *testing.T) {
	out := make(chan application.ServiceLogEvent, 2)
	sink := &logSink{
		ctx:     context.Background(),
		out:     out,
		service: "edge_web",
		now:     func() time.Time { return time.Unix(0, 0).UTC() },
	}

	for i := range 10 {
		sink.write("stdout", []byte("2026-08-28T06:00:00Z attrs=none line "+string(rune('0'+i))+"\n"))
	}
	if sink.dropped == 0 {
		t.Fatal("ten lines into a channel of two dropped nothing")
	}

	// Read one, freeing a slot, and the next send reports the gap before it
	// delivers anything else.
	<-out
	dropped := sink.dropped
	sink.write("stdout", []byte("2026-08-28T06:00:00Z attrs=none once there is room\n"))

	var notice application.ServiceLogEvent
	for range len(out) {
		e := <-out
		if e.Notice {
			notice = e
		}
	}
	if !notice.Notice {
		t.Fatal("the gap was never reported")
	}
	if notice.Stream != "stderr" {
		t.Errorf("notice stream = %q, want stderr so the console's filter does not hide it", notice.Stream)
	}
	if !strings.Contains(notice.Message, "dropped") {
		t.Errorf("notice = %q", notice.Message)
	}
	if !strings.Contains(notice.Message, itoa(dropped)) {
		t.Errorf("notice = %q, want the count it was holding (%d)", notice.Message, dropped)
	}
}

// itoa avoids dragging strconv into a test fixture for one small integer.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// summarise keeps a failure message readable when a stream produced more than
// a couple of events.
func summarise(events []application.ServiceLogEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		msg := e.Message
		if len(msg) > 40 {
			msg = msg[:40] + "…"
		}
		out = append(out, e.Stream+":"+msg)
	}
	return out
}

var errBoomLogs = errors.New("dial unix /var/run/docker.sock: connect: permission denied")

// blockingReader is a quiet container: it returns nothing until the context the
// request was made with ends, which is how an http body behaves on cancel.
type blockingReader struct{ ctx context.Context }

func (b blockingReader) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

// ---------------------------------------------------------------------------
// #266 — naming the task and the node behind a line
// ---------------------------------------------------------------------------

// labelled is a fake whose service has an id, two replicas on two nodes, and a
// roster naming both. The shape every test below starts from.
func labelled(body string) *logAPI {
	return &logAPI{
		body:      strings.NewReader(body),
		serviceID: "svc-1",
		tasks: []swarm.Task{
			{ID: "task-1", Slot: 1, NodeID: "node-a"},
			{ID: "task-2", Slot: 2, NodeID: "node-b"},
		},
		nodes: []swarm.Node{
			{ID: "node-a", Description: swarm.NodeDescription{Hostname: "swarm-w1"}},
			{ID: "node-b", Description: swarm.NodeDescription{Hostname: "swarm-w2"}},
		},
	}
}

// The daemon states which task and which node wrote each line and says both in
// ids. An operator reads `docker service ps`, which says `.1` and a hostname,
// so a console that showed the ids would still be asking them to correlate by
// eye against another window.
func TestALineIsNamedByItsReplicaAndItsNode(t *testing.T) {
	api := labelled(
		frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "up")) +
			frame(frameStderr, swarmLine("2026-08-28T06:00:01Z", "node-b", "task-2", "refused")),
	)

	got := tailFor(t, api)

	if len(got) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(got), got)
	}
	if got[0].Slot != 1 || got[0].NodeHostname != "swarm-w1" {
		t.Errorf("first line = slot %d on %q, want slot 1 on swarm-w1", got[0].Slot, got[0].NodeHostname)
	}
	if got[1].Slot != 2 || got[1].NodeHostname != "swarm-w2" {
		t.Errorf("second line = slot %d on %q, want slot 2 on swarm-w2", got[1].Slot, got[1].NodeHostname)
	}
	// The ids stay. They are what a support conversation quotes, and the label
	// is the convenience laid over them rather than a replacement.
	if got[0].TaskID != "task-1" || got[0].NodeID != "node-a" {
		t.Errorf("the ids were dropped: %+v", got[0])
	}
}

// The tasks are asked for by the service's own id, not its name. A name is a
// filter the daemon resolves and this one is already resolved, and asking
// swarm-wide is exactly what the seam's contract forbids.
func TestTheTasksAreAskedForByTheServiceTheStreamIsFor(t *testing.T) {
	api := labelled(frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "up")))

	tailFor(t, api)

	if len(api.taskFilters) != 1 {
		t.Fatalf("TaskList calls = %d, want 1", len(api.taskFilters))
	}
	if got := api.taskFilters[0].Get("service"); len(got) != 1 || got[0] != "svc-1" {
		t.Errorf("tasks filtered by service %v, want [svc-1]", got)
	}
}

// A global service's tasks have no slot — the daemon reports 0 and treats that
// as the global case itself. Rendering it as replica zero would name a replica
// that does not exist, so the slot stays absent and the node is the label.
func TestAGlobalServiceIsNamedByItsNodeAndNotByReplicaZero(t *testing.T) {
	api := labelled(frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-g", "up")))
	api.tasks = []swarm.Task{{ID: "task-g", Slot: 0, NodeID: "node-a"}}

	got := tailFor(t, api)

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Slot != 0 {
		t.Errorf("slot = %d, want 0 — a global task has no replica number", got[0].Slot)
	}
	if got[0].NodeHostname != "swarm-w1" {
		t.Errorf("hostname = %q, want swarm-w1 — the node is what names a global task", got[0].NodeHostname)
	}
}

// The two lookups are separate calls filling separate maps, which is what makes
// a failure partial. Folding them into one snapshot read would make either
// failure cost both labels.
func TestAFailedTaskLookupStillNamesTheNode(t *testing.T) {
	api := labelled(frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "up")))
	api.tasksErr = errors.New("manager busy")

	got := tailFor(t, api)

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Slot != 0 {
		t.Errorf("slot = %d, want 0 — the task lookup failed", got[0].Slot)
	}
	if got[0].NodeHostname != "swarm-w1" {
		t.Errorf("hostname = %q, want swarm-w1 — the node lookup succeeded", got[0].NodeHostname)
	}
	if got[0].Message != "up" {
		t.Errorf("message = %q — a label that could not be read must not cost the line", got[0].Message)
	}
}

func TestAFailedNodeLookupStillNamesTheReplica(t *testing.T) {
	api := labelled(frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "up")))
	api.nodesErr = errors.New("manager busy")

	got := tailFor(t, api)

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Slot != 1 {
		t.Errorf("slot = %d, want 1 — the task lookup succeeded", got[0].Slot)
	}
	if got[0].NodeHostname != "" {
		t.Errorf("hostname = %q, want empty — the node lookup failed", got[0].NodeHostname)
	}
	if got[0].Message != "up" {
		t.Errorf("message = %q — a label that could not be read must not cost the line", got[0].Message)
	}
}

// A rollout starts tasks after the stream opened, and a rollout is when an
// operator is reading. One unseen id asks again; the rest of the burst does
// not, or a crash-looping service turns one log stream into a TaskList loop.
func TestAnUnseenTaskAsksAgainOnceAndNotPerLine(t *testing.T) {
	var body strings.Builder
	for i := range 5 {
		body.WriteString(frame(frameStdout, swarmLine("2026-08-28T06:00:0"+strconv.Itoa(i)+"Z", "node-a", "task-new", "line")))
	}
	api := labelled(body.String())
	// The task the lines came from is not in the map the stream opened with.
	api.tasks = []swarm.Task{{ID: "task-1", Slot: 1, NodeID: "node-a"}}

	got := tailFor(t, api)

	if len(got) != 5 {
		t.Fatalf("events = %d, want 5", len(got))
	}
	// One at open, and exactly one more for the whole burst of unseen lines.
	if len(api.taskFilters) != 2 {
		t.Errorf("TaskList calls = %d, want 2 — one at open and one refresh for the burst", len(api.taskFilters))
	}
}

// The refresh is worth having only if it finds what it went looking for.
func TestARefreshNamesATaskThatStartedAfterTheStreamDid(t *testing.T) {
	api := labelled(frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-b", "task-late", "up")))
	api.tasks = []swarm.Task{{ID: "task-1", Slot: 1, NodeID: "node-a"}}
	// The second lookup — the refresh — finds the rollout's new task.
	api.onTaskList = func(n int) []swarm.Task {
		if n > 1 {
			return []swarm.Task{{ID: "task-late", Slot: 3, NodeID: "node-b"}}
		}
		return nil
	}

	got := tailFor(t, api)

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Slot != 3 {
		t.Errorf("slot = %d, want 3 — the refresh should have found the new task", got[0].Slot)
	}
}

// A daemon that names no service id is the one case where asking for tasks
// would be asking swarm-wide, which is what the seam forbids.
func TestNoServiceIdMeansNoTaskLookupAtAll(t *testing.T) {
	api := labelled(frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "up")))
	api.serviceID = ""

	got := tailFor(t, api)

	if len(api.taskFilters) != 0 {
		t.Errorf("TaskList calls = %d, want 0 — there was no service id to filter by", len(api.taskFilters))
	}
	if len(got) != 1 || got[0].NodeHostname != "swarm-w1" {
		t.Errorf("the node label should survive: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// #267 — reading further back than the tail
// ---------------------------------------------------------------------------

// The window is one intent in two options and the daemon applies them in one
// order: the last Tail lines are selected first, and only then are the ones
// older than Since dropped. So a request carrying one without the other is a
// window bounded by the wrong thing, and both have to reach the daemon.
func TestTheWindowReachesTheDaemonAsBothOptions(t *testing.T) {
	api := labelled(frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "up")))
	b := testBackend(t, api, nil)

	since := time.Date(2026, 8, 28, 5, 0, 0, 123456789, time.UTC)
	ch, err := b.ServiceLogs(context.Background(), capability.ServiceLogRequest{
		Service: "edge_web",
		Tail:    5000,
		Since:   since,
		Follow:  true,
	})
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}
	drain(t, ch)

	if api.gotOpts.Tail != "5000" {
		t.Errorf("tail = %q, want 5000", api.gotOpts.Tail)
	}
	// The daemon's own spelling on this path, seconds and nanoseconds — what
	// its executor writes when it passes a swarm subscription to a node.
	if want := "1787893200.123456789"; api.gotOpts.Since != want {
		t.Errorf("since = %q, want %q", api.gotOpts.Since, want)
	}
}

// No window asked for is the state every caller was in before there was one,
// and it must still reach the daemon as no bound rather than as the epoch —
// which would be a request for every line the node has ever retained.
func TestNoWindowAsksTheDaemonForNoBound(t *testing.T) {
	api := labelled(frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "up")))

	tailFor(t, api)

	if api.gotOpts.Since != "" {
		t.Errorf("since = %q, want empty — the zero time is no bound, not the epoch", api.gotOpts.Since)
	}
}

// The drop rule is right for a live stream and wrong for a backlog. Twenty
// thousand lines arrive from the daemon far faster than they are written to a
// browser, so under the live rule most of a history read would be dropped and
// the operator handed a notice saying so — the feature failing at the one thing
// it was added for. A backlog is bounded by Tail, so blocking on it is bounded.
func TestTheBacklogIsDeliveredWholeToAConsumerThatIsBehind(t *testing.T) {
	const lines = logBuffer * 3

	var body strings.Builder
	for i := range lines {
		body.WriteString(frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "line "+strconv.Itoa(i))))
	}
	api := labelled(body.String())
	b := testBackend(t, api, nil)

	ch, err := b.ServiceLogs(context.Background(), capability.ServiceLogRequest{
		Service: "edge_web",
		Tail:    lines,
		Follow:  true,
	})
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}

	// Read only after the producer has had every chance to fill and overrun the
	// channel, which is what a browser that has not painted yet looks like.
	got := drain(t, ch)

	if len(got) != lines {
		t.Fatalf("events = %d, want %d — a backlog larger than the buffer was dropped", len(got), lines)
	}
	for _, e := range got {
		if e.Notice {
			t.Fatalf("a backlog read reported dropped output: %q", e.Message)
		}
	}
	if got[0].Message != "line 0" || got[lines-1].Message != "line "+strconv.Itoa(lines-1) {
		t.Errorf("the backlog is not whole: first %q, last %q", got[0].Message, got[lines-1].Message)
	}
}

// The count alone is not enough to end the backlog phase. A console asking for
// twenty thousand lines against a service that has written fifty is asking for
// a backlog of fifty, and events fifty-one onward are live — blocking on those
// is exactly the stall the drop rule exists to prevent. The reader reports a
// stalled read and the phase ends on that, whichever comes first.
func TestAShortBacklogDoesNotLeaveTheStreamBlockingOnLiveOutput(t *testing.T) {
	// A body that yields two lines and then stalls longer than the grace before
	// producing more, which is what a caught-up follow looks like.
	// Shortened so the test does not spend the real grace period; the rule it
	// exercises is the same at any value.
	restore := logBacklogGrace
	logBacklogGrace = 30 * time.Millisecond
	t.Cleanup(func() { logBacklogGrace = restore })

	backlog := frame(frameStdout, swarmLine("2026-08-28T06:00:00Z", "node-a", "task-1", "backlog"))
	live := frame(frameStdout, swarmLine("2026-08-28T06:00:01Z", "node-a", "task-1", "live"))
	api := labelled("")
	api.body = &stallingReader{
		data:    backlog + live,
		stallAt: len(backlog),
		stall:   logBacklogGrace,
	}
	b := testBackend(t, api, nil)

	// Far more than the stream will ever produce, so the count can never end
	// the phase and only the stall can.
	ch, err := b.ServiceLogs(context.Background(), capability.ServiceLogRequest{
		Service: "edge_web",
		Tail:    100000,
		Follow:  true,
	})
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}
	got := drain(t, ch)

	if len(got) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(got), got)
	}
	if !api.body.(*stallingReader).stalled {
		t.Error("the read never stalled, so the phase change was never exercised")
	}
	if got[0].Message != "backlog" || got[1].Message != "live" {
		t.Errorf("lines = %q, %q", got[0].Message, got[1].Message)
	}
}

// stallingReader is a byte stream that pauses once, at a fixed offset, before
// handing over the rest.
//
// A stream and not a chunk queue: the framed reader uses io.ReadFull for an
// 8-byte header, so a Read that returns a whole frame per call and then forgets
// the remainder would corrupt the framing rather than test the timing. The
// stall is inside a Read, which is the signal the backlog phase ends on — the
// gap between two *events* also contains any time the sink spent blocked, and
// would end the phase in the middle of a backlog being delivered slowly.
type stallingReader struct {
	data    string
	stallAt int
	stall   time.Duration
	stalled bool
	pos     int
}

func (s *stallingReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	if !s.stalled && s.pos >= s.stallAt {
		s.stalled = true
		time.Sleep(s.stall)
	}
	// Never past the stall point in one read, or the pause never happens where
	// the test placed it.
	end := len(s.data)
	if !s.stalled && end > s.stallAt {
		end = s.stallAt
	}
	n := copy(p, s.data[s.pos:end])
	s.pos += n
	return n, nil
}
