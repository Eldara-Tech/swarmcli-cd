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
	// DriftDetected reports that the repository moved: a commit changed what
	// the application declares and the swarm has not caught up.
	DriftDetected EventType = "drift-detected"
	// LiveDriftDetected reports that the *swarm* moved: the running services of
	// a release the repository has not touched no longer match it, which under
	// driftDetection: live is the only way an out-of-band change is ever seen.
	//
	// Deliberately not DriftDetected with a different message. The two need
	// different responses — one is a commit to review, the other is a change
	// nobody recorded — and a notifier that routes on the type must be able to
	// tell them apart without parsing prose.
	LiveDriftDetected EventType = "live-drift-detected"
	// DriftConverged reports that the controller corrected one. Message names
	// the releases it redeployed.
	//
	// It exists because the correction is otherwise invisible: no chart
	// revision is written for it — the desired state did not change — so
	// without this the only trace of a service being rewritten under an
	// operator is a status field that has already gone back to none.
	DriftConverged EventType = "drift-converged"
	// ResourcesPruned reports that the controller deleted the deployed
	// resources of something git no longer declares — a whole application that
	// left the app set, a release an application stopped declaring, or a service
	// its chart stopped declaring. Message names what went.
	//
	// It is the loudest thing this controller does, and the only event that
	// reports something destroyed rather than converged.
	ResourcesPruned EventType = "resources-pruned"
	// PruneFailed reports that the deletion did not complete. What it names is
	// still deployed and still unmanaged; the next reconcile tries again.
	PruneFailed EventType = "prune-failed"
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
	switch e.Type {
	case SyncFailed, PruneFailed:
		level = slog.LevelError
	// Warn, not Info: a stack was deleted. It is the line an operator goes
	// looking for when something they expected to be running is not.
	case ResourcesPruned:
		level = slog.LevelWarn
	// Warn for the same reason, from the other direction: somebody changed a
	// running service outside git, and the controller either has or has not put
	// it back. Both are lines an operator needs to find afterwards.
	case LiveDriftDetected, DriftConverged:
		level = slog.LevelWarn
	}
	slog.Default().Log(ctx, level, "reconcile", attrs...)
}
