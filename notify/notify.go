// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package notify carries reconcile events to whoever is listening.
//
// This seam is additive where the others replace, and the asymmetry is
// deliberate. A Business Edition notifier posting to Slack must not remove the
// log notifier — and, the case that actually forces it, the HTTP API's event
// stream is itself a notifier. If registering replaced, loading the companion
// would silently kill the UI's live updates.
package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/seam"
)

// EventType names what happened.
type EventType string

const (
	SyncStarted   EventType = "sync-started"
	SyncSucceeded EventType = "sync-succeeded"
	SyncFailed    EventType = "sync-failed"
	DriftDetected EventType = "drift-detected"
)

// Event is one thing that happened to one application.
type Event struct {
	Application string
	Type        EventType
	Revision    string // the resolved commit, where one applies
	Message     string
	At          time.Time
}

// Notifier receives events.
//
// Notify returns nothing and must not block. A notification that cannot be
// delivered is not a reason to fail a sync, so implementations report their own
// delivery failures; an error return would only make every caller write the
// same log-and-continue.
type Notifier interface {
	Notify(ctx context.Context, e Event)
}

var list seam.List[Notifier]

// Register appends a notifier. It removes nothing. Call it from an init().
func Register(name string, n Notifier) { list.Register(name, n) }

// All returns every registered notifier.
func All() []Notifier { return list.All() }

// Active names every registered notifier, for startup logging.
func Active() []string { return list.Names() }

// Dispatch delivers e to every registered notifier.
func Dispatch(ctx context.Context, e Event) {
	for _, n := range list.All() {
		n.Notify(ctx, e)
	}
}

func init() { Register("log", logNotifier{}) }

// logNotifier writes one structured line per event. It is what makes a
// controller with no companion loaded still auditable.
type logNotifier struct{}

// Notify implements Notifier.
func (logNotifier) Notify(ctx context.Context, e Event) {
	attrs := []any{
		slog.String("application", e.Application),
		slog.String("event", string(e.Type)),
	}
	if e.Revision != "" {
		attrs = append(attrs, slog.String("revision", e.Revision))
	}
	if e.Message != "" {
		attrs = append(attrs, slog.String("message", e.Message))
	}

	level := slog.LevelInfo
	if e.Type == SyncFailed {
		level = slog.LevelError
	}
	slog.Default().Log(ctx, level, "reconcile", attrs...)
}
