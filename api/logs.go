// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

// LogStreamer is the optional interface a Reconciler implements when it can
// stream live Docker Swarm service container logs.
//
// Optional in the seam sense: a build whose reconciler does not implement it is
// expected rather than broken, and says so with a 501.
//
// # What an implementer owes the caller
//
// Events, in order, on a channel the implementer owns and closes exactly once.
// Cancelling ctx is how the caller stops it, so there is no Close to forget.
// `Stream` is "stdout" or "stderr" — a line whose origin is not known is
// stdout, which is the honest default, and must never be reported as stderr.
// `Timestamp` is when the *container* emitted the line rather than when it was
// read: a hundred lines of backlog stamped with the read time is a hundred
// things happening at once, which in an incident is a lie about the sequence.
// `Message` carries no trailing newline.
//
// It must not block on a caller that has stopped reading. A container in a
// crash loop outruns a browser, and the answer is to drop and say so with
// application.ServiceLogEvent.Notice, not to buffer the difference.
//
// This used to hand back an io.ReadCloser of lines with an optional "stderr\t"
// prefix, which the handler split and parsed. It carries events instead because
// the wire type has two fields that shape could not reach: the daemon states
// which task and which node each line came from, and a byte stream has nowhere to
// put them — so for a replicated service, one node crash-looping and the whole
// service being broken looked identical.
//
// `app` and `svc` have already been authorised and, more importantly, `svc` has
// already been checked to be one of `app`'s own services — see serviceLogs.
// An implementer must not treat `svc` as a swarm-wide service name it may look
// up on its own, because the guard authorised the subject for `app` and for
// nothing else.
type LogStreamer interface {
	ServiceLogs(ctx context.Context, app, svc string, tail int, follow bool) (<-chan application.ServiceLogEvent, error)
}

// How much scrollback a newly attached client is given.
const logTail = 100

// maxLogStreams bounds how many log streams this controller serves at once.
//
// Each one holds a goroutine, a channel and a connection to the daemon, and
// nothing else bounded them: a browser with several tabs open, or an authorised
// client behaving badly, took as many as it asked for. Enforced here rather
// than behind the seam so that the bound covers every implementer of it,
// including a companion's.
const maxLogStreams = 16

// logKeepalive is how often an idle log stream writes a comment frame, and how
// often it re-checks that the subject may still read it.
//
// # Why this stream has an idle timer when the event stream deliberately does not
//
// api/stream.go has none because a quiet controller is normal. A quiet
// container is normal too, so the traffic is not the difference — the client
// is. A browser opens /events with EventSource, which reconnects on its own, so
// a proxy closing an idle event stream is invisible. The log console uses fetch
// and a reader, does not reconnect, and renders the close as ENDED until the
// operator changes tab and comes back. nginx's proxy_read_timeout is 60 seconds
// by default, so without this every console watching a service that logs hourly
// dies a minute after it was opened.
//
// A third of that default, so one lost frame still leaves 20 seconds of margin.
// A var so a test does not have to spend it.
var logKeepalive = 20 * time.Second

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

	// After the 501, so a build with no streamer keeps saying so rather than
	// reporting a full pool it does not have.
	select {
	case s.logSlots <- struct{}{}:
		defer func() { <-s.logSlots }()
	default:
		fail(w, http.StatusServiceUnavailable, "too many log streams are open on this controller; try again shortly")
		return
	}

	// Derived rather than the request's own, so that every path out of this
	// handler ends the producer — including the ones the request context knows
	// nothing about, such as the authorisation being withdrawn below.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	events, err := streamer.ServiceLogs(ctx, app, svc, logTail, true)
	switch {
	case errors.Is(err, application.ErrUnsupported):
		// The same answer, and deliberately the same sentence, as a reconciler
		// that is not a LogStreamer at all. Whether a build cannot stream
		// because its reconciler has no method or because the backend behind
		// this application's destination has no reader is a distinction inside
		// the controller; from the caller's side it is one fact.
		fail(w, http.StatusNotImplemented, "this controller does not stream service logs")
		return
	case err != nil:
		// Logged in full, reported as a sentence. The error from a daemon call
		// names socket paths and internal state, and this endpoint is reachable
		// by every subject an authorizer grants ActionLogs — the same reasoning
		// api.go applies to the app set's error.
		s.log.Error("failed to open service log stream", "app", app, "service", svc, "error", err)
		fail(w, http.StatusBadGateway, "could not open the service log stream; ask an administrator")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Proxies that buffer a response defeat the entire point of a stream; this
	// is the header nginx and friends read to turn that off. api/stream.go has
	// set it since it was written and this endpoint did not, so a proxied
	// console could sit silent while the proxy accumulated.
	h.Set("X-Accel-Buffering", "no")
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

	ticker := time.NewTicker(logKeepalive)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-decided rather than settled at open, for the reason
			// api/stream.go re-decides per event: this is the longest-lived
			// authorised thing the API serves, and checked only once a
			// withdrawn grant or a rotated token has no effect on a console
			// that is already attached. Per tick rather than per line because
			// a crash-looping container is thousands of lines a second.
			if !s.mayStillRead(r, app) {
				return
			}
			// A comment frame: ignored by every conforming SSE client, and by
			// the console's own parser, which keeps only "data: " lines.
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-events:
			if !ok {
				return
			}
			if !send(e) {
				return
			}
		}
	}
}

// mayStillRead reports whether the credential on the request still authorises
// reading this application's logs.
func (s *Server) mayStillRead(r *http.Request, app string) bool {
	subject, err := s.authz.Authenticate(r)
	if err != nil {
		return false
	}
	return s.authz.Authorize(r.Context(), subject, authz.ActionLogs, app) == nil
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
