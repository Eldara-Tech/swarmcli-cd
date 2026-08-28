// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
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

	gotService string
	gotOpts    container.LogsOptions
	closed     chan struct{}
}

func (a *logAPI) ServiceInspectWithRaw(_ context.Context, service string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	if a.inspectErr != nil {
		return swarm.Service{}, nil, a.inspectErr
	}
	return swarm.Service{Spec: swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: service},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{TTY: a.tty}},
	}}, nil, nil
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
