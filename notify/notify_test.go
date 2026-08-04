// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package notify

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type recorder struct{ got []Event }

func (r *recorder) Notify(_ context.Context, e Event) { r.got = append(r.got, e) }

func TestDefaultIsRegistered(t *testing.T) {
	names := Active()
	if len(names) != 1 || names[0] != "log" {
		t.Errorf("Active = %v, want [log]", names)
	}
}

// The asymmetry that matters: registering appends. A companion adding Slack
// must not remove the log notifier or the API's event stream.
func TestRegisterAppendsAndDispatchReachesAll(t *testing.T) {
	stream, slack := &recorder{}, &recorder{}
	Register("stream", stream)
	Register("slack", slack)

	e := Event{Application: "edge", Type: SyncSucceeded, Revision: "9f3c1ab", At: time.Now()}
	Dispatch(context.Background(), e)

	for name, r := range map[string]*recorder{"stream": stream, "slack": slack} {
		if len(r.got) != 1 || r.got[0].Application != "edge" {
			t.Errorf("%s received %v, want one event for edge", name, r.got)
		}
	}
	if names := Active(); len(names) != 3 || names[0] != "log" {
		t.Errorf("Active = %v, want the default plus both registrations", names)
	}
	if got := All(); len(got) != 3 {
		t.Errorf("All returned %d notifiers, want the default plus both registrations", len(got))
	}
}

// The actor travels on the context so that api.Reconciler does not have to
// carry it: an implementation that never heard of this raises events with an
// empty actor, which is exactly what an unattributed sync should look like.
func TestActorTravelsOnTheContext(t *testing.T) {
	if got := ActorFrom(context.Background()); got != "" {
		t.Errorf("ActorFrom on an unmarked context = %q, want empty — the controller acted on its own", got)
	}
	if got := ActorFrom(WithActor(context.Background(), "alice")); got != "alice" {
		t.Errorf("ActorFrom = %q, want alice", got)
	}
}

func TestLogNotifierWritesTheEvent(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	logNotifier{}.Notify(context.Background(), Event{
		Application: "edge",
		Type:        SyncFailed,
		Revision:    "9f3c1ab",
		Message:     "chart digest mismatch",
		Actor:       "alice",
	})

	got := buf.String()
	for _, want := range []string{"application=edge", "event=sync-failed", "revision=9f3c1ab", "level=ERROR", "chart digest mismatch", "actor=alice"} {
		if !strings.Contains(got, want) {
			t.Errorf("log line is missing %q: %s", want, got)
		}
	}
}

// The level is the contract, not decoration: CLAUDE.md defines Warn as
// something an operator must see but that did not stop the loop — a prune held,
// a resource left behind — and Error as a reconcile or a component failing. Each
// of these was assigned deliberately and none of them was covered, so demoting
// resources-pruned to Info left the suite green while the one line an operator
// greps for after a stack disappears went quiet.
func TestLogNotifierLevelsFollowTheContract(t *testing.T) {
	for _, tc := range []struct {
		event EventType
		want  string
		why   string
	}{
		{SyncStarted, "INFO", "lifecycle"},
		{SyncSucceeded, "INFO", "lifecycle"},
		{DriftDetected, "INFO", "a commit moved; the next sync applies it"},
		{SyncFailed, "ERROR", "a reconcile failed"},
		{PruneFailed, "WARN", "a prune held is the case the contract names for Warn, and it does not fail the sync"},
		{ResourcesPruned, "WARN", "the controller deleted somebody's running stack"},
		{LiveDriftDetected, "WARN", "somebody changed a running service outside git"},
		{DriftConverged, "WARN", "the controller overwrote that change"},
	} {
		t.Run(string(tc.event), func(t *testing.T) {
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(restore) })

			logNotifier{}.Notify(context.Background(), Event{Application: "edge", Type: tc.event})

			if want := "level=" + tc.want; !strings.Contains(buf.String(), want) {
				t.Errorf("logged %q, want %s — %s", buf.String(), want, tc.why)
			}
		})
	}
}

// Empty optional fields are left out rather than logged as empty attributes.
func TestLogNotifierOmitsEmptyFields(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	logNotifier{}.Notify(context.Background(), Event{Application: "edge", Type: SyncStarted})

	got := buf.String()
	// actor is on this list and not on the swarm test's, because it is the one
	// field both the log and the wire omit: an event the controller raised on its
	// own has nobody to name.
	for _, unwanted := range []string{"revision=", "message=", "actor="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("log line contains %q for an empty field: %s", unwanted, got)
		}
	}
	if !strings.Contains(got, "level=INFO") {
		t.Errorf("a started event should not be an error: %s", got)
	}
}

// Event.Swarm has two consumers and both must carry it: the API's event stream,
// and this line. The stream states it on every frame because a program parses a
// contract; this omits it when empty because a human reads a log and swarm="" on
// every line of a single-swarm deployment is noise. Both directions are asserted
// here, because the omission is the half that looks like the bug it is not.
func TestLogNotifierNamesTheDestinationOnlyWhenThereIsOne(t *testing.T) {
	for _, tc := range []struct {
		name  string
		swarm string
		want  bool
	}{
		{"a named destination", "production", true},
		{"the swarm the controller runs in", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(restore) })

			logNotifier{}.Notify(context.Background(), Event{
				Application: "edge",
				Type:        ResourcesPruned,
				Swarm:       tc.swarm,
				Message:     "pruned legacy-api",
			})

			got := buf.String()
			// The attribute, not the value: a router keys on swarm=, and an
			// empty one is the attribute being there.
			if has := strings.Contains(got, "swarm="); has != tc.want {
				t.Fatalf("log line %q: swarm attribute present = %v, want %v", got, has, tc.want)
			}
			if tc.want && !strings.Contains(got, "swarm="+tc.swarm) {
				t.Errorf("log line %q does not name the destination %q", got, tc.swarm)
			}
		})
	}
}
