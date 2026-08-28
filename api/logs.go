// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

// LogStreamer is the optional interface a Reconciler implements when it can
// stream live Docker Swarm service container logs.
//
// Optional in the seam sense: nothing in this repository implements it, and a
// build whose reconciler does not is expected rather than broken.
//
// # What an implementer owes the caller
//
// The reader yields **one log line per line**, already demultiplexed, with no
// 8-byte Docker stream header left on it — the API's own framing is line-based
// and cannot recover a length prefix. Where the implementer knows which of the
// two streams a line came from it labels it; where it does not, stdout is the
// honest default, and a line whose origin is not known must not be reported as
// stderr.
//
// `app` and `svc` have already been authorised and, more importantly, `svc` has
// already been checked to be one of `app`'s own services — see serviceLogs.
// An implementer must not treat `svc` as a swarm-wide service name it may look
// up on its own, because the guard authorised the subject for `app` and for
// nothing else.
type LogStreamer interface {
	ServiceLogs(ctx context.Context, app, svc string, tail int, follow bool) (io.ReadCloser, error)
}

// How much scrollback a newly attached client is given.
const logTail = 100

// A container is entitled to emit a line longer than bufio's 64KB default — a
// stack trace, a serialised request, one line of JSON — and the default makes
// Scan return false on it, which ended the whole stream silently and left a
// console that looked live. Capped rather than unbounded because the producer
// is not ours.
const maxLogLine = 1 << 20

// serviceLogs streams live stdout/stderr log lines for the given application
// service over SSE.
//
// The headers go out only once there is something to stream, so a service this
// application does not declare is a 404 and an unimplemented seam is a 501,
// rather than a 200 whose first frame carries the bad news. A client that has
// already been told "200, text/event-stream" cannot go back and report a
// failure as one.
func (s *Server) serviceLogs(w http.ResponseWriter, r *http.Request, _ authz.Subject) {
	app := r.PathValue("app")
	svc := r.PathValue("svc")

	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// The guard authorised this subject for `app`. It did not authorise them
	// for `svc`, and nothing downstream re-derives the link — so without this,
	// a subject an authorizer scoped to one application could read the logs of
	// any service in the swarm by naming it here. Resolving the name against
	// the application's own reported services is what makes the path value a
	// selection within what was granted rather than a second, ungranted one.
	view, found := s.rec.View(app)
	if !found {
		fail(w, http.StatusNotFound, "no such application")
		return
	}
	if !declaresService(view, svc) {
		// Deliberately the same answer as an application that does not exist:
		// which services an application a subject may not see happens to run is
		// itself a disclosure.
		fail(w, http.StatusNotFound, "no such service for this application")
		return
	}

	streamer, ok := s.rec.(LogStreamer)
	if !ok {
		fail(w, http.StatusNotImplemented, "this controller does not stream service logs")
		return
	}

	rc, err := streamer.ServiceLogs(r.Context(), app, svc, logTail, true)
	if err != nil {
		// Logged in full, reported as a sentence. The error from a daemon call
		// names socket paths and internal state, and this endpoint is reachable
		// by every subject an authorizer grants ActionLogs — the same reasoning
		// api.go applies to the app set's error.
		s.log.Error("failed to open service log stream", "app", app, "service", svc, "error", err)
		fail(w, http.StatusBadGateway, "could not open the service log stream; ask an administrator")
		return
	}
	// Closed the way every other reader in this repository is: errcheck is on,
	// and a bare `defer rc.Close()` is the one call in the tree that ignored it.
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(e application.ServiceLogEvent) bool {
		data, err := json.Marshal(e)
		if err != nil {
			return false
		}
		// One JSON object on one `data:` line. json.Marshal escapes every
		// newline in the message, so a log line containing "\n\ndata: {...}"
		// cannot close this frame and inject the next one.
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogLine)
	for scanner.Scan() {
		if r.Context().Err() != nil {
			return
		}
		stream, message, at := splitLine(scanner.Text(), func() time.Time { return time.Now().UTC() })
		if !send(application.ServiceLogEvent{
			Service:   svc,
			Stream:    stream,
			Message:   message,
			Timestamp: at,
		}) {
			return
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		s.log.Error("error scanning service logs", "app", app, "service", svc, "error", err)
	}
}

// declaresService reports whether svc is one of the services this application's
// own status reports. An application the controller has not reconciled yet
// declares none, and answers no to everything.
func declaresService(view application.View, svc string) bool {
	for _, release := range view.Status.Releases {
		if slices.ContainsFunc(release.Services, func(s application.ServiceStatus) bool { return s.Name == svc }) {
			return true
		}
	}
	return false
}

// splitLine reads one line from the streamer into the three things the wire
// type carries.
//
// # The stream label
//
// The wire type has carried a `stream` field since it was written and every
// line was labelled "stdout" regardless, so the UI's STDERR filter matched
// nothing and an error line was reported as ordinary output. A "stderr\t"
// prefix is the cheapest thing a demultiplexing implementer can put on a line
// that this side can read back; an unprefixed line is stdout, which is what an
// implementer that cannot tell the two apart should produce.
//
// # The timestamp
//
// Docker puts an RFC3339 instant at the head of each line when asked for one,
// and it is when the *container* emitted the line. Stamping time.Now() instead
// — which is what this did — gives a hundred lines of backlog a single instant
// and reads, in an incident, as a hundred things happening at once. A line
// carrying no parseable instant falls back to now, because a log line with no
// time at all is worse than one with an approximate one.
func splitLine(line string, now func() time.Time) (stream, message string, at time.Time) {
	stream = "stdout"
	for _, p := range [...]struct{ prefix, name string }{{"stderr\t", "stderr"}, {"stdout\t", "stdout"}} {
		if rest, ok := strings.CutPrefix(line, p.prefix); ok {
			stream, line = p.name, rest
			break
		}
	}

	head, rest, found := strings.Cut(line, " ")
	if !found {
		return stream, line, now()
	}
	parsed, err := time.Parse(time.RFC3339Nano, head)
	if err != nil {
		// Not a timestamp, so it is the first word of the message.
		return stream, line, now()
	}
	return stream, rest, parsed
}
