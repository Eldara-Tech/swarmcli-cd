// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	"github.com/Eldara-Tech/swarmcli-cd/authz"
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

// recentLimit is how many published events the controller remembers, and so the
// most GET /api/v1/events/recent can ever return.
//
// The stream carries no `id:` and replays nothing, which is deliberate and
// documented — but it left the console's terminal empty on every load, and a
// converged controller raises nothing to fill it with, so the screen looked
// broken on precisely the fleet that was healthiest (#292). This is where the
// history it seeds from comes from.
//
// Four times the 250 the web UI keeps, because the document is not only the web
// UI's: a curl or the future TUI view asks for the ring and gets it, while the
// console asks for the 250 it can hold. It is memory rather than a record —
// roughly 150KB at this depth, gone on restart, and the history endpoint is
// still the durable account of what was deployed.
const recentLimit = 1000

// stream fans notifications out to the connected event-stream clients.
type stream struct {
	log *slog.Logger

	mu   sync.Mutex
	next int
	subs map[int]chan notifyEvent
	// recent is the last recentLimit events published, oldest first, under the
	// same mutex as subs so that publishing stays one critical section.
	//
	// Appended before the fan-out rather than after it, and appended
	// unconditionally: an event dropped for a subscriber that was not keeping up
	// is still something this controller did, so the document is strictly more
	// complete than any one connection was.
	recent []notifyEvent
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

	// Trimmed from the front by one, which keeps the slice's length at the cap
	// rather than letting the backing array grow without bound behind it.
	if len(s.recent) == recentLimit {
		s.recent = append(s.recent[:0], s.recent[1:]...)
	}
	s.recent = append(s.recent, e)

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

// snapshot returns the newest n published events, oldest first.
//
// A copy, so the caller can authorise and marshal them without holding the lock
// that every publish takes — the fan-out must never wait on a request.
func (s *stream) snapshot(n int) []notifyEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.recent) {
		n = len(s.recent)
	}
	out := make([]notifyEvent, n)
	copy(out, s.recent[len(s.recent)-n:])
	return out
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
	// Actor is omitempty, where Swarm above deliberately is not. That is the
	// opposite call on the same struct, and it is the point rather than an
	// inconsistency to tidy up: the question is never "is the string empty", it
	// is "does empty mean something".
	//
	// For Swarm, empty is the answer — "the swarm the controller runs in" — so
	// dropping the key would tell a consumer the event has no destination when
	// every event has one. An absent actor is a genuine absence: the controller
	// acted on its own, on its own tick, correcting drift or sweeping something
	// git stopped declaring, and nobody pressed anything. That puts it with
	// Revision and Message, whose keys are omitted because the event has no
	// answer to give — and "actor":"" on every controller-raised frame would
	// assert that somebody triggered it and declined to say who.
	//
	// The argument that keeps Swarm present does not reach here either. A client
	// meets this field the first time an operator syncs from the UI, which is a
	// thing that happens in every build including this one, so there is no
	// licensed shape it could be surprised by.
	Actor string `json:"actor,omitempty"`
	At    string `json:"at"`
}

// toWire is the one place a notify.Event becomes the JSON a client reads.
//
// Shared by the stream and by the recent-events document so that the two cannot
// drift into two shapes: a frame and a row of history describe the same event,
// and a consumer seeding a terminal from one and then tailing the other has to
// be able to render them with the same code.
func toWire(e notifyEvent) wire {
	return wire{
		Application: e.Application,
		Swarm:       e.Swarm,
		Type:        string(e.Type),
		Revision:    e.Revision,
		Message:     e.Message,
		Actor:       e.Actor,
		At:          e.At.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// stream serves server-sent events.
//
// SSE rather than a websocket because nothing here flows upstream: events go
// controller to client and never back. It is plain HTTP, so the TUI reads it
// with an ordinary client and no second protocol, a browser needs no upgrade
// handshake, and the Phase 3 rbac-proxy forwards it without having to handle
// one.
//
// The wire conforms to SSE and an EventSource could read it — the `event:` name
// below is there for exactly that — but the web UI does not use one: EventSource
// cannot set an Authorization header, so the console reads this with fetch and
// reconnects itself. This comment claimed browsers reconnect "on their own
// through EventSource" until #292; they do reconnect, and nothing here is what
// makes them.
//
// Every event is authorised against the subject that opened the stream. The
// guard's one decision was about the endpoint, so without this a tenant with
// read access to one application watched every application's syncs, drift and
// failures go past — the same disclosure the list view had, arriving live rather
// than on request.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, subject authz.Subject) {
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
			// Authorize rather than Visible: here the question really is about
			// one application, because that is what an event is about. Asking
			// per event rather than once at subscribe time also means a subject
			// whose access is withdrawn stops being sent things without the
			// connection having to be noticed and dropped — the decision is
			// taken afresh, against the authorizer in force, for as long as the
			// stream lives.
			if err := s.authz.Authorize(r.Context(), subject, authz.ActionRead, e.Application); err != nil {
				s.log.Debug("not delivering an event to a subscriber that may not see it",
					"subscriber", id, "application", e.Application, "event", e.Type)
				continue
			}
			payload, err := json.Marshal(toWire(e))
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

// parseRecentLimit reads how many events the caller wants, newest first.
//
// Absent means the whole ring, which is what a curl or the TUI view asks for. A
// value above it is clamped rather than refused, where api/logs.go's `tail`
// refuses: `tail` over its maximum asks the daemon for work it should not do,
// while a limit over the ring asks for events that do not exist — which is
// exactly what omitting the parameter already means, so answering with
// everything there is says the truth rather than an error about it.
func parseRecentLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return recentLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, errors.New("limit must be a positive whole number of events")
	}
	return min(n, recentLimit), nil
}

// recentEvents serves what this controller has published, so a client opening a
// stream has something to open with.
//
// The stream itself is unchanged and still replays nothing: it carries no `id:`,
// a reconnect is a refetch rather than a resume, and a frame remains a hint that
// a document should be re-read. This is the document. What it adds is that the
// hint channel is no longer the only account of what happened — before it, a
// console on a converged fleet drew an empty terminal for ever, because a
// controller with nothing to correct raises nothing (#292).
//
// Authorised per event, exactly as the stream is and for the same reason. The
// guard's one decision was about the endpoint; without this, a subject scoped to
// one application reads every application's syncs, drift and failures out of the
// ring — the same disclosure the live path refuses, arriving on request instead.
func (s *Server) recentEvents(w http.ResponseWriter, r *http.Request, subject authz.Subject) {
	limit, err := parseRecentLimit(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	events := s.events.snapshot(limit)

	// Never nil: a client that has to tell "no events" from "the key is missing"
	// would otherwise be reading the difference between two encodings of the
	// same empty answer.
	out := make([]wire, 0, len(events))
	for _, e := range events {
		if err := s.authz.Authorize(r.Context(), subject, authz.ActionRead, e.Application); err != nil {
			continue
		}
		out = append(out, toWire(e))
	}

	write(w, http.StatusOK, map[string]any{"events": out})
}
