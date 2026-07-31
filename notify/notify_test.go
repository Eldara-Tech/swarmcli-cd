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
	})

	got := buf.String()
	for _, want := range []string{"application=edge", "event=sync-failed", "revision=9f3c1ab", "level=ERROR", "chart digest mismatch"} {
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
	for _, unwanted := range []string{"revision=", "message="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("log line contains %q for an empty field: %s", unwanted, got)
		}
	}
	if !strings.Contains(got, "level=INFO") {
		t.Errorf("a started event should not be an error: %s", got)
	}
}
