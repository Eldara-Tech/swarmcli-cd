// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

// The log endpoint shipped with no test of any kind, and the two things it most
// needed one for were the two it got wrong: what it answers when nothing can
// stream, and whether the service in the path is one the caller was authorised
// to read.

// The daemon's own words, which must reach the log and not the caller.
var errBoom = errors.New("dial unix /var/run/docker.sock: connect: permission denied")

// logStreamer is a fakeReconciler that can stream, returning whatever events
// the test hands it and recording what it was asked for.
//
// With hold set the channel stays open until the context it was given ends,
// which is what a following stream does and what every test about the life of
// an attached console needs. Without it the channel is closed at once, which is
// what a finished tail does.
type logStreamer struct {
	fakeReconciler
	events []application.ServiceLogEvent
	err    error
	hold   bool

	mu       sync.Mutex
	gotApp   string
	gotSvc   string
	gotTail  int
	gotFollw bool
	opened   int
	// ended is closed when the producer notices its context was cancelled, so
	// a test can assert the seam was actually stopped rather than sleeping.
	ended chan struct{}
}

func (l *logStreamer) ServiceLogs(ctx context.Context, app, svc string, tail int, follow bool) (<-chan application.ServiceLogEvent, error) {
	l.mu.Lock()
	l.gotApp, l.gotSvc, l.gotTail, l.gotFollw = app, svc, tail, follow
	l.opened++
	if l.ended == nil {
		l.ended = make(chan struct{})
	}
	ended := l.ended
	l.mu.Unlock()

	if l.err != nil {
		return nil, l.err
	}
	ch := make(chan application.ServiceLogEvent, len(l.events)+1)
	for _, e := range l.events {
		ch <- e
	}
	if !l.hold {
		close(ch)
		return ch, nil
	}
	go func() {
		<-ctx.Done()
		close(ch)
		select {
		case <-ended:
		default:
			close(ended)
		}
	}()
	return ch, nil
}

func (l *logStreamer) asked() (app, svc string, tail int, follow bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gotApp, l.gotSvc, l.gotTail, l.gotFollw
}

// line is one log event as a test writes it, so the fixtures read as lines.
func line(stream, message string) application.ServiceLogEvent {
	return application.ServiceLogEvent{
		Service:   "edge-web",
		Stream:    stream,
		Message:   message,
		Timestamp: time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC),
	}
}

// viewWithService is a reconciled application declaring exactly one service.
func viewWithService(app, svc string) application.View {
	v := view(app)
	v.Status.Releases = []application.ReleaseStatus{{
		Name:     "rel",
		Services: []application.ServiceStatus{{Name: svc}},
	}}
	return v
}

func logEvents(t *testing.T, body string) []application.ServiceLogEvent {
	t.Helper()
	var out []application.ServiceLogEvent
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var e application.ServiceLogEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		out = append(out, e)
	}
	return out
}

// streamLogs attaches to the log endpoint of a real listener and hands back the
// response and a cancel. A recorder cannot be used for a stream that outlives
// the call: the handler writes to it from the same goroutine the test would
// have to read from.
func streamLogs(t *testing.T, h http.Handler, path string) (*http.Response, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, cancel
}

// The guard authorises the subject for {app} and for nothing else, so the
// service in the path has to be resolved against that application's own
// services. Without it, a subject an authorizer has scoped to one application
// names any service in the swarm here and reads its container output.
func TestLogsRefuseAServiceTheApplicationDoesNotDeclare(t *testing.T) {
	rec := &logStreamer{fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}}}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/services/other-app-db/logs")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if app, svc, _, _ := rec.asked(); app != "" || svc != "" {
		t.Errorf("the streamer was asked for (%q, %q) for a service the application does not declare", app, svc)
	}
}

func TestLogsForAnUnknownApplicationAreNotFound(t *testing.T) {
	rec := &logStreamer{fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}}}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/nope/services/edge-web/logs")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// It used to answer 200 and then emit a made-up line every three seconds —
// "service X task healthy - 0 active errors" — as container output, on every
// build, because nothing implements the seam.
func TestLogsWithoutAStreamerSaySoRatherThanInventingOutput(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}}, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/services/edge-web/logs")

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
	for _, invented := range []string{"task healthy", "0 active errors", "attached to live log stream"} {
		if strings.Contains(rr.Body.String(), invented) {
			t.Errorf("the response still carries %q, which no container emitted", invented)
		}
	}
}

// A reconciler that has the method but meets a destination with no reader
// behind it is the same fact, from the caller's side, as a reconciler that
// never had the method — and must not be a 502 telling an operator the read
// failed when nothing was ever going to read it.
func TestADestinationThatCannotStreamIsTheSame501(t *testing.T) {
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		err:            application.ErrUnsupported,
	}
	_, h := testServer(t, rec, nil)

	unwired := do(t, h, "GET", "/api/v1/applications/edge/services/edge-web/logs")
	_, plain := testServer(t, &fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}}, nil)
	none := do(t, plain, "GET", "/api/v1/applications/edge/services/edge-web/logs")

	if unwired.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", unwired.Code)
	}
	if unwired.Body.String() != none.Body.String() {
		t.Errorf("an unsupported destination says %q where an unwired build says %q",
			unwired.Body.String(), none.Body.String())
	}
}

// A failure to open the stream is a failure, not a 200 whose first frame
// carries the bad news — and the daemon's own words stay in the log.
func TestAFailedOpenIsAStatusAndNotAStreamedError(t *testing.T) {
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		err:            errBoom,
	}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/services/edge-web/logs")

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
	if strings.Contains(rr.Body.String(), errBoom.Error()) {
		t.Errorf("body = %q, which hands the caller the daemon's own error text", rr.Body.String())
	}
}

func TestLogsStreamOneFramePerEvent(t *testing.T) {
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		events: []application.ServiceLogEvent{
			line("stdout", "listening on :8080"),
			func() application.ServiceLogEvent {
				e := line("stderr", "connection reset")
				e.TaskID, e.NodeID = "task-1", "node-a"
				return e
			}(),
		},
	}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/services/edge-web/logs")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	// api/stream.go has set this since it was written and this endpoint did
	// not, so a proxied console sat silent while nginx accumulated.
	if buffering := rr.Header().Get("X-Accel-Buffering"); buffering != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", buffering)
	}
	if app, svc, tail, follow := rec.asked(); app != "edge" || svc != "edge-web" || tail != logTail || !follow {
		t.Errorf("streamer asked for (%q,%q,%d,%v)", app, svc, tail, follow)
	}

	got := logEvents(t, rr.Body.String())
	if len(got) != 2 {
		t.Fatalf("frames = %d, want 2: %+v", len(got), got)
	}
	// Every line was labelled stdout once, so the console's STDERR filter
	// matched nothing and an error line read as ordinary output.
	if got[0].Stream != "stdout" || got[1].Stream != "stderr" {
		t.Errorf("streams = %q, %q; want stdout, stderr", got[0].Stream, got[1].Stream)
	}
	// The instant the container emitted the line, not the instant this process
	// read it — which stamped a hundred lines of backlog with one time.
	want := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	if !got[0].Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want the line's own %v", got[0].Timestamp, want)
	}
	// The two fields the byte-oriented seam could never carry, which is the
	// reason it stopped being one.
	if got[1].TaskID != "task-1" || got[1].NodeID != "node-a" {
		t.Errorf("task/node = %q/%q, want task-1/node-a", got[1].TaskID, got[1].NodeID)
	}
}

// A log line is attacker-influenced text: anything a container writes reaches
// this framing. A bare "\n\n" would end the frame and let the next line be read
// as a new SSE event with fields of its own.
func TestALogLineCannotForgeItsOwnFrame(t *testing.T) {
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		events:         []application.ServiceLogEvent{line("stdout", "hello\n\ndata: {\"message\":\"forged\"}")},
	}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/services/edge-web/logs")

	got := logEvents(t, rr.Body.String())
	if len(got) != 1 {
		t.Fatalf("frames = %d, want 1 — a line closed its own frame: %+v", len(got), got)
	}
	if got[0].Message == "forged" {
		t.Errorf("the injected frame was read as an event")
	}
}

// Nothing bounded the number of open streams: each holds a goroutine, a channel
// and a connection to the daemon, and a browser with enough tabs took as many
// as it asked for.
func TestOpenLogStreamsAreBounded(t *testing.T) {
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		hold:           true,
	}
	_, h := testServer(t, rec, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Held open for the whole test: an attached stream is one that has taken a
	// slot, and closing the body is what gives it back.
	var attached []*http.Response
	defer func() {
		for _, resp := range attached {
			_ = resp.Body.Close()
		}
	}()
	for range maxLogStreams {
		req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/applications/edge/services/edge-web/logs", nil)
		if err != nil {
			t.Fatal(err)
		}
		// Closed by the deferred sweep above, which the analyser cannot follow
		// through the slice.
		resp, err := http.DefaultClient.Do(req) //nolint:bodyclose
		if err != nil {
			t.Fatal(err)
		}
		attached = append(attached, resp) //nolint:bodyclose
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("an attached stream got %d", resp.StatusCode)
		}
	}

	over, err := http.Get(srv.URL + "/api/v1/applications/edge/services/edge-web/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = over.Body.Close() }()
	if over.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("stream %d got %d, want 503", maxLogStreams+1, over.StatusCode)
	}
}

// A quiet container is normal, and an idle stream through a proxy is a stream
// that gets closed. The event stream deliberately has no idle timer; this one
// does, because the console reads it with fetch and does not reconnect.
func TestAQuietStreamKeepsItselfOpen(t *testing.T) {
	restore := logKeepalive
	logKeepalive = 5 * time.Millisecond
	t.Cleanup(func() { logKeepalive = restore })

	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		hold:           true,
	}
	_, h := testServer(t, rec, nil)
	resp, cancel := streamLogs(t, h, "/api/v1/applications/edge/services/edge-web/logs")
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	// A comment frame, which is what an SSE client ignores and a proxy counts
	// as traffic. Read as a line rather than to EOF: the stream is following.
	got, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the first frame: %v", err)
	}
	if !strings.HasPrefix(got, ":") {
		t.Errorf("first frame = %q, want a comment", got)
	}
}

// A log stream is the longest-lived authorised thing this API serves. Checked
// only at open, a withdrawn grant has no effect on a console already attached.
func TestWithdrawingAccessEndsAnAttachedStream(t *testing.T) {
	restore := logKeepalive
	logKeepalive = 5 * time.Millisecond
	t.Cleanup(func() { logKeepalive = restore })

	auth := &revocable{}
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		hold:           true,
	}
	_, h := testServer(t, rec, auth)
	resp, cancel := streamLogs(t, h, "/api/v1/applications/edge/services/edge-web/logs")
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	auth.revoke()

	// The body ends because the handler returned, not because the client went
	// away — which is the whole point, and is why this reads to EOF rather than
	// cancelling first.
	done := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 4096))
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived the authorisation that opened it")
	}
}

// The producer holds a goroutine and a daemon connection, so a client that
// disconnects has to end it rather than leaving it reading into a channel
// nobody drains.
func TestADisconnectedClientEndsTheProducer(t *testing.T) {
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		hold:           true,
	}
	_, h := testServer(t, rec, nil)
	resp, cancel := streamLogs(t, h, "/api/v1/applications/edge/services/edge-web/logs")
	defer func() { _ = resp.Body.Close() }()

	cancel()

	rec.mu.Lock()
	ended := rec.ended
	rec.mu.Unlock()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the seam was never told the client had gone")
	}
}

// revocable grants until it is revoked, and then refuses — the shape of a
// credential rotated or a grant withdrawn while a console is attached.
type revocable struct {
	gone atomic.Bool
}

func (*revocable) Ready() error { return nil }

func (r *revocable) Authenticate(*http.Request) (authz.Subject, error) {
	return authz.Subject{Name: "tester"}, nil
}

func (r *revocable) Authorize(_ context.Context, _ authz.Subject, _ authz.Action, _ string) error {
	if r.gone.Load() {
		return errors.New("not any more")
	}
	return nil
}

func (r *revocable) Visible(_ context.Context, _ authz.Subject, _ authz.Action, apps []string) ([]string, error) {
	return apps, nil
}

func (r *revocable) revoke() { r.gone.Store(true) }
