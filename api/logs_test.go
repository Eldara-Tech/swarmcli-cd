// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// The log endpoint shipped with no test of any kind, and the two things it most
// needed one for were the two it got wrong: what it answers when nothing can
// stream, and whether the service in the path is one the caller was authorised
// to read.

// The daemon's own words, which must reach the log and not the caller.
var errBoom = errors.New("dial unix /var/run/docker.sock: connect: permission denied")

// logStreamer is a fakeReconciler that can stream, returning whatever bytes the
// test hands it and recording what it was asked for.
type logStreamer struct {
	fakeReconciler
	body     string
	err      error
	gotApp   string
	gotSvc   string
	gotTail  int
	gotFollw bool
}

func (l *logStreamer) ServiceLogs(_ context.Context, app, svc string, tail int, follow bool) (io.ReadCloser, error) {
	l.gotApp, l.gotSvc, l.gotTail, l.gotFollw = app, svc, tail, follow
	if l.err != nil {
		return nil, l.err
	}
	return io.NopCloser(strings.NewReader(l.body)), nil
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

// The guard authorises the subject for {app} and for nothing else, so the
// service in the path has to be resolved against that application's own
// services. Without it, a subject an authorizer has scoped to one application
// names any service in the swarm here and reads its container output.
func TestLogsRefuseAServiceTheApplicationDoesNotDeclare(t *testing.T) {
	rec := &logStreamer{fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}}}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/services/someone-elses-db/logs")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if rec.gotSvc != "" {
		t.Errorf("the streamer was asked for %q; the check has to happen before the daemon call", rec.gotSvc)
	}
}

// An application that does not exist and one whose services this subject may
// not enumerate get the same answer, because which services an application runs
// is itself a disclosure.
func TestLogsForAnUnknownApplicationAreNotFound(t *testing.T) {
	rec := &logStreamer{fakeReconciler: fakeReconciler{}}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/nope/services/web/logs")
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

func TestLogsStreamOneFramePerLine(t *testing.T) {
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		body:           "2026-08-28T06:00:00Z listening on :8080\nstderr\t2026-08-28T06:00:01.5Z connection reset\nno timestamp here\n",
	}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/services/edge-web/logs")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	if rec.gotApp != "edge" || rec.gotSvc != "edge-web" || rec.gotTail != logTail || !rec.gotFollw {
		t.Errorf("streamer asked for (%q,%q,%d,%v)", rec.gotApp, rec.gotSvc, rec.gotTail, rec.gotFollw)
	}

	got := logEvents(t, rr.Body.String())
	if len(got) != 3 {
		t.Fatalf("frames = %d, want 3: %+v", len(got), got)
	}

	// Every line was labelled stdout, so the console's STDERR filter matched
	// nothing and an error line read as ordinary output.
	if got[0].Stream != "stdout" || got[1].Stream != "stderr" {
		t.Errorf("streams = %q, %q; want stdout, stderr", got[0].Stream, got[1].Stream)
	}

	// The instant the container emitted the line, not the instant this process
	// read it — which stamped a hundred lines of backlog with one time.
	want := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	if !got[0].Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want the line's own %v", got[0].Timestamp, want)
	}
	if got[0].Message != "listening on :8080" {
		t.Errorf("message = %q, want the line with its prefix consumed", got[0].Message)
	}
	if got[1].Message != "connection reset" {
		t.Errorf("stderr message = %q", got[1].Message)
	}
	// A line with no timestamp keeps all of its text rather than losing its
	// first word to a failed parse.
	if got[2].Message != "no timestamp here" {
		t.Errorf("untimed message = %q", got[2].Message)
	}
}

// A log line is attacker-influenced text: anything a container writes reaches
// this framing. A bare "\n\n" would end the frame and let the next line be read
// as a new SSE event with fields of its own.
func TestALogLineCannotForgeItsOwnFrame(t *testing.T) {
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		body:           "2026-08-28T06:00:00Z hello\\n\\ndata: {\"message\":\"forged\"}\n",
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

// bufio's 64KB default made Scan return false on a longer line, which ended the
// stream with nothing said and left a console that looked live.
func TestALineLongerThanTheDefaultBufferDoesNotEndTheStream(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	rec := &logStreamer{
		fakeReconciler: fakeReconciler{views: []application.View{viewWithService("edge", "edge-web")}},
		body:           long + "\n2026-08-28T06:00:00Z after the long one\n",
	}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/services/edge-web/logs")

	got := logEvents(t, rr.Body.String())
	if len(got) != 2 {
		t.Fatalf("frames = %d, want 2; the line after a long one was dropped", len(got))
	}
	if got[1].Message != "after the long one" {
		t.Errorf("second message = %q", got[1].Message)
	}
}
