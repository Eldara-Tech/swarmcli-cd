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
	s.subs[id] = ch
	return id, ch
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
	Type        string `json:"type"`
	Revision    string `json:"revision,omitempty"`
	Message     string `json:"message,omitempty"`
	At          string `json:"at"`
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
