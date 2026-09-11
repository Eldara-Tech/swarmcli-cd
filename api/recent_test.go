// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/notify"
)

// recentBody is the document GET /api/v1/events/recent answers with.
type recentBody struct {
	Events []wire `json:"events"`
}

// raise publishes n events for one application, numbered in their revision so
// that a test can say which of them survived and in what order.
func raise(t *testing.T, s *Server, app string, n int) {
	t.Helper()
	for i := range n {
		s.Notify(context.Background(), notify.Event{
			Application: app,
			Type:        notify.SyncSucceeded,
			Revision:    fmt.Sprintf("r%d", i),
			At:          time.Unix(int64(i), 0).UTC(),
		})
	}
}

func recent(t *testing.T, h http.Handler, query string) recentBody {
	t.Helper()
	rr := do(t, h, "GET", "/api/v1/events/recent"+query)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	return decode[recentBody](t, rr)
}

// The whole point of the endpoint: a console that has just loaded, against a
// controller that has been running for a while, opens with what happened rather
// than with an empty terminal. Oldest first, because that is the order a
// terminal renders and the order the stream would have delivered them in.
func TestRecentEventsAreOldestFirst(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, nil)
	raise(t, s, "edge", 3)

	got := recent(t, h, "")

	if len(got.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(got.Events))
	}
	for i, e := range got.Events {
		if want := fmt.Sprintf("r%d", i); e.Revision != want {
			t.Errorf("event %d is %q, want %q: the document is not oldest first", i, e.Revision, want)
		}
	}
}

// The ring is memory, not a record. What it must not do is grow without bound
// on a controller that has been up for a month.
func TestRecentEventsCapAtTheRingAndDropTheOldest(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, nil)
	raise(t, s, "edge", recentLimit+5)

	got := recent(t, h, "")

	if len(got.Events) != recentLimit {
		t.Fatalf("got %d events, want the ring's %d", len(got.Events), recentLimit)
	}
	if want := fmt.Sprintf("r%d", 5); got.Events[0].Revision != want {
		t.Errorf("oldest kept is %q, want %q: the wrong end was trimmed", got.Events[0].Revision, want)
	}
	if want := fmt.Sprintf("r%d", recentLimit+4); got.Events[len(got.Events)-1].Revision != want {
		t.Errorf("newest kept is %q, want %q", got.Events[len(got.Events)-1].Revision, want)
	}
}

// The console holds 250 and the ring holds four times that, so the parameter is
// what stops a page load — and every reconnect — carrying four times what the
// client can use.
func TestRecentEventsHonourALimit(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, nil)
	raise(t, s, "edge", 10)

	got := recent(t, h, "?limit=3")

	if len(got.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(got.Events))
	}
	// The newest three, still oldest first: a client asking for fewer wants the
	// most recent ones, not the start of the controller's history.
	for i, e := range got.Events {
		if want := fmt.Sprintf("r%d", 7+i); e.Revision != want {
			t.Errorf("event %d is %q, want %q: a limit took the wrong end", i, e.Revision, want)
		}
	}
}

// Clamped rather than refused, where api/logs.go's `tail` refuses: a limit above
// the ring asks for events that do not exist, which is what omitting the
// parameter already means.
func TestRecentEventsClampALimitAboveTheRing(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, nil)
	raise(t, s, "edge", 4)

	got := recent(t, h, fmt.Sprintf("?limit=%d", recentLimit*10))

	if len(got.Events) != 4 {
		t.Fatalf("got %d events, want the 4 that exist", len(got.Events))
	}
}

func TestRecentEventsRefuseALimitThatIsNotAPositiveNumber(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{}, nil)

	for _, raw := range []string{"0", "-1", "all", "2.5"} {
		rr := do(t, h, "GET", "/api/v1/events/recent?limit="+raw)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("limit=%s: status = %d, want 400", raw, rr.Code)
		}
	}
}

// An empty ring is an empty array. A client that has to tell "no events" from
// "the key is missing" is reading the difference between two encodings of the
// same answer.
func TestRecentEventsAreAnArrayWhenThereAreNone(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{}, nil)

	rr := do(t, h, "GET", "/api/v1/events/recent")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, `"events":[]`) {
		t.Errorf("body = %s, want an empty array rather than null", body)
	}
}

// The same disclosure the live stream refuses, arriving on request instead. The
// guard's one decision was about the endpoint; the events are about
// applications, and a subject scoped to one may not read another's out of the
// ring.
func TestRecentEventsOnlyCarryWhatTheSubjectMaySee(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, projectAuthorizer{visible: "edge"})
	raise(t, s, "prod", 2)
	raise(t, s, "edge", 2)

	got := recent(t, h, "")

	if len(got.Events) != 2 {
		t.Fatalf("got %d events, want only the 2 for the application the subject may see", len(got.Events))
	}
	for _, e := range got.Events {
		if e.Application != "edge" {
			t.Errorf("event for %q reached a subject scoped to 'edge'", e.Application)
		}
	}
}

// The ring is appended before the fan-out and never conditionally, so it holds
// what a subscriber that was not keeping up never received. That is what makes
// the document worth seeding from rather than a second copy of what the
// connection already had.
func TestRecentEventsHoldWhatASlowSubscriberMissed(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, nil)

	// A subscriber that never reads. Its buffer fills and publish starts
	// dropping, which is the documented behaviour being relied on here.
	id, ch := s.events.subscribe()
	defer s.events.unsubscribe(id)

	raise(t, s, "edge", subscriberBuffer+10)

	if len(ch) != subscriberBuffer {
		t.Fatalf("the subscriber's buffer holds %d, want it full at %d", len(ch), subscriberBuffer)
	}

	got := recent(t, h, "")
	if len(got.Events) != subscriberBuffer+10 {
		t.Errorf("the ring holds %d events, want all %d published: it dropped what the subscriber did",
			len(got.Events), subscriberBuffer+10)
	}
}

// The document and the frame describe the same event, and a console seeding
// from one before tailing the other renders both with the same code. toWire is
// the single place that shape is decided; this is the assertion that keeps it
// single.
func TestTheDocumentCarriesTheSameFieldsAsAFrame(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, nil)
	s.Notify(context.Background(), notify.Event{
		Application: "edge",
		Type:        notify.ResourcesPruned,
		Message:     "pruned legacy-api",
		Actor:       "alice",
		At:          time.Unix(0, 0).UTC(),
	})

	got := recent(t, h, "")
	if len(got.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(got.Events))
	}

	want := toWire(notify.Event{
		Application: "edge",
		Type:        notify.ResourcesPruned,
		Message:     "pruned legacy-api",
		Actor:       "alice",
		At:          time.Unix(0, 0).UTC(),
	})
	if got.Events[0] != want {
		t.Errorf("document row %+v, want %+v", got.Events[0], want)
	}
}
