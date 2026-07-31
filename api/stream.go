// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/Eldara-Tech/swarmcli-cd/notify"
)

// notifyEvent is notify.Event under a local name, so the exported Notify method
// reads without the package qualifier repeating itself.
type notifyEvent = notify.Event

// subscriberBuffer is how many events a slow client may fall behind by before
// its events start being dropped.
//
// Dropping is the correct failure. notify.Notifier must not block — a browser
// that stopped reading must never be able to stall a reconcile — and the events
// are a live feed rather than a log: a client that missed some re-reads the
// status endpoint, which is authoritative. Sized for a burst from several
// applications reconciling at once, not for a client to go away and come back.
const subscriberBuffer = 64

// stream fans notifications out to the connected event-stream clients.
type stream struct {
	log *slog.Logger

	mu   sync.Mutex
	next int
	subs map[int]chan notifyEvent
	// closed is set once the streams have been ended for shutdown, so a request
	// already past the listener cannot subscribe to a feed nothing will ever
	// publish to and then hold the drain open waiting for it.
	closed bool
}

func newStream(log *slog.Logger) *stream {
	return &stream{log: log, subs: map[int]chan notifyEvent{}}
}

func (s *stream) subscribe() (int, <-chan notifyEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	ch := make(chan notifyEvent, subscriberBuffer)
	if s.closed {
		// Already draining. A closed channel ends the handler's loop at once,
		// which is the same answer it would get a moment later anyway.
		close(ch)
		return id, ch
	}
	s.subs[id] = ch
	return id, ch
}

// closeAll ends every connected stream.
//
// http.Server.Shutdown waits for connections to go idle and does not cancel
// in-flight request contexts, and an event stream never goes idle — so every
// shutdown with a subscriber attached spent the entire timeout achieving
// nothing and then logged that the API had not shut down cleanly. Swarm sends
// SIGKILL ten seconds after SIGTERM, so that was half the budget.
func (s *stream) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
}

func (s *stream) unsubscribe(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subs[id]; ok {
		delete(s.subs, id)
		close(ch)
	}
}

// publish delivers to every subscriber, never blocking on any of them.
func (s *stream) publish(_ context.Context, e notifyEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subs {
		select {
		case ch <- e:
		default:
			s.log.Warn("event stream subscriber is not keeping up; dropping an event",
				"subscriber", id, "application", e.Application, "event", e.Type)
		}
	}
}

func (s *stream) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// wire is one event as it goes down the stream. notify.Event's own fields are
// not tagged for JSON — it is an internal type — so the wire shape is stated
// here rather than leaking whatever Go field names happen to be.
type wire struct {
	Application string `json:"application"`
	// Swarm is deliberately not omitempty, where Revision and Message are.
	//
	// Those two are omitted because they do not always apply: a sync-succeeded
	// has nothing to say, a resources-pruned resolved no commit, and a key
	// holding "" there would assert an answer where the event has none. A
	// destination is not like that — every event has one, and notify.Event.Swarm
	// says so explicitly: empty *is* the answer, "the swarm the controller runs
	// in", rather than an absence. Omitting it would tell a consumer the
	// opposite of what is true.
	//
	// The cost is one always-empty key in a single-swarm deployment, which is
	// every Apache-2.0 one. That buys a shape readable off a single frame: with
	// omitempty, a client written against this controller would never see the
	// field exist and would meet it for the first time pointed at a multi-swarm
	// build — which is precisely the client that must not be surprised by it.
	//
	// notify's logNotifier omits the same field when it is empty. That is the
	// deliberate opposite, not an inconsistency: a log line is read by a human
	// who already knows which controller's log they are in, and it has no
	// contract for a field to be missing from.
	Swarm    string `json:"swarm"`
	Type     string `json:"type"`
	Revision string `json:"revision,omitempty"`
	Message  string `json:"message,omitempty"`
	At       string `json:"at"`
}

// stream serves server-sent events.
//
// SSE rather than a websocket because nothing here flows upstream: events go
// controller to client and never back. It is plain HTTP, so the TUI reads it
// with an ordinary client and no second protocol, browsers reconnect on their
// own through EventSource, and the Phase 3 rbac-proxy forwards it without
// needing to handle an upgrade.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Nothing in the standard server does this, but a middleware that
		// wrapped the writer without preserving Flush would otherwise produce a
		// stream that silently never arrives.
		fail(w, http.StatusInternalServerError, "this server cannot stream")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Proxies that buffer a response defeat the entire point of a stream; this
	// is the header nginx and friends read to turn that off.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id, events := s.events.subscribe()
	defer s.events.unsubscribe(id)

	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(wire{
				Application: e.Application,
				Swarm:       e.Swarm,
				Type:        string(e.Type),
				Revision:    e.Revision,
				Message:     e.Message,
				At:          e.At.UTC().Format("2006-01-02T15:04:05Z07:00"),
			})
			if err != nil {
				s.log.Warn("could not encode an event", "error", err)
				continue
			}
			// The event name is what an EventSource listener binds to, so it
			// carries the type as well as the payload.
			if _, err := w.Write([]byte("event: " + string(e.Type) + "\ndata: " + string(payload) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
