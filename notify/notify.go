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
	// left the app set, a release an application stopped declaring, or a
	// service, network, config or secret its chart stopped declaring. Message
	// names what went.
	//
	// It is the loudest thing this controller does, and the only event that
	// reports something destroyed rather than converged.
	ResourcesPruned EventType = "resources-pruned"
	// PruneFailed reports that the deletion did not complete. What it names is
	// still deployed and still unmanaged; the next reconcile tries again.
	PruneFailed EventType = "prune-failed"
	// SelfUpdateIssued reports that the controller has asked the swarm to
	// replace it with the revision it just applied. Message names the release.
	//
	// It is the last thing this controller says. Swarm's update order is
	// stop-first, so the task that dispatched this is stopped as the rollout
	// starts, and the sync it belongs to therefore has no recorded outcome —
	// the process that would have recorded one is gone. This event is what
	// stands in its place, and the release record written a moment earlier is
	// what the replacement reads.
	//
	// Which is also why a notifier must not treat it as a completion. What it
	// reports is a write accepted by the daemon, not a controller that came
	// back: if the new task fails its healthcheck the swarm rolls the service
	// back, and the recovered controller reports that as live drift rather than
	// as anything here.
	SelfUpdateIssued EventType = "self-update-issued"
)

// Event is one thing that happened to one application.
type Event struct {
	Application string
	Type        EventType
	Revision    string // the resolved commit, where one applies
	Message     string
	At          time.Time
	// Swarm is the destination the event concerns, as
	// application.Spec.Destination names it. Empty is the swarm the controller
	// runs in.
	//
	// It is here because routing is what a Business Edition notifier does —
	// production alerts to one channel, staging to another — and without it the
	// only thing to route on is Message, which is prose written for a human.
	// Deciding where an alert goes by matching on a sentence is a notifier that
	// breaks when somebody improves the wording.
	//
	// Empty is the ordinary value in an Apache-2.0 build and means the swarm the
	// controller runs in, because that is the only destination swarms.Registry's
	// OSS default resolves (D2) and the only one an application can name without
	// a companion loaded. It is not "unknown": the reconciler stamps every event
	// it raises with the application's own destination, whatever that is (#131).
	Swarm string
	// Actor is who asked for the work this event came out of: the authenticated
	// subject's name, as the API's guard resolved it. Empty means the controller
	// acted on its own — a tick of the reconcile loop, a drift correction, a
	// sweep nobody triggered.
	//
	// Everything one request set off carries it, not only that request's own
	// sync-started and sync-succeeded: a prune or a drift correction performed
	// during a manual sync was started by the person who pressed the button, and
	// an audit log saying otherwise would name nobody for the one event that
	// reports something deleted.
	//
	// Empty is a *genuine absence*, and that is what decides the wire shape:
	// api's wire tags it omitempty, alongside Revision and Message, whose keys
	// are dropped because the event has no answer to give. Swarm is the exact
	// opposite on the same struct — empty is itself the answer, "the swarm the
	// controller runs in" — which is why it is stated on every frame. The
	// inconsistency between those two tags is the point rather than something to
	// tidy up; see api's wire for the long version.
	Actor string
}

// actorKey is what an actor travels under. Unexported, and of an unexported
// type, so nothing outside this package can put a value there.
type actorKey struct{}

// WithActor marks ctx as work one identified caller asked for. Every event
// raised under it names them; see Event.Actor.
//
// A context value rather than a parameter because the events are raised deep
// inside a reconcile, and the only other route is api.Reconciler — an interface
// stated so that an alternative reconciler can implement it, which would then
// have to carry an attribution label through a method signature for something it
// need not understand. A reconciler that ignores this raises events with no
// actor, which is exactly what an unattributed sync should look like.
//
// It carries the name and not the authz.Subject, deliberately. api.guard hands
// its subject to handlers as a parameter precisely so that no authorisation
// decision is ever taken on a context lookup that may have come back empty; a
// bare display string cannot be mistaken for one, and the only thing that ever
// reads it is a label on an event.
func WithActor(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, actorKey{}, name)
}

// ActorFrom returns the actor ctx was marked with, or "" for work the controller
// started itself — which is what Event.Actor documents empty to mean.
func ActorFrom(ctx context.Context) string {
	name, _ := ctx.Value(actorKey{}).(string)
	return name
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
	// Only when there is one, where the API's wire shape states swarm on every
	// frame whether or not it is empty. The two are the same field treated
	// oppositely on purpose, and the reason is who is reading.
	//
	// That shape is parsed by a program against a documented contract, so the
	// key has to be there always: omit it and a client written against this
	// build never sees the field exist, meeting it for the first time pointed at
	// a controller that fills it. This line is read by a human who already knows
	// which controller's log they are in, and swarm="" on every line of a
	// single-swarm deployment says nothing while crowding out what does — the
	// same reason revision and message are conditional here.
	if e.Swarm != "" {
		attrs = append(attrs, slog.String("swarm", e.Swarm))
	}
	if e.Revision != "" {
		attrs = append(attrs, slog.String("revision", e.Revision))
	}
	if e.Message != "" {
		attrs = append(attrs, slog.String("message", e.Message))
	}
	// With revision and message rather than with swarm, and the wire agrees for
	// once: an absent actor is nobody having pressed anything, which is most
	// lines in this log. actor="" on all of them would read as a request whose
	// caller went unrecorded — the opposite of what happened.
	if e.Actor != "" {
		attrs = append(attrs, slog.String("actor", e.Actor))
	}

	level := slog.LevelInfo
	switch e.Type {
	case SyncFailed:
		level = slog.LevelError
	// Warn, not Error, and this is the case CLAUDE.md names to define the level:
	// "a prune held, a resource left behind". A prune that did not complete does
	// not fail the sync — the deploy landed, and what is left is retried next
	// interval — and for the network, config and secret sweep it is the expected
	// answer while the services that referenced them are still draining. Error
	// therefore fired every interval, beside a Warn line the reconciler had just
	// written about the same fact, for something working as intended. Where a
	// sweep does fail the reconcile, loop logs that at Error itself; this line
	// would only be a second copy of it.
	case PruneFailed:
		level = slog.LevelWarn
	// Warn, not Info: a stack was deleted. It is the line an operator goes
	// looking for when something they expected to be running is not.
	case ResourcesPruned:
		level = slog.LevelWarn
	// Warn for the same reason, from the other direction: somebody changed a
	// running service outside git, and the controller either has or has not put
	// it back. Both are lines an operator needs to find afterwards.
	case LiveDriftDetected, DriftConverged:
		level = slog.LevelWarn
	// Warn, for the reason ResourcesPruned is: the controller is about to stop.
	// It is the line an operator goes looking for to explain why the log ends
	// here and the API stopped answering for a minute.
	case SelfUpdateIssued:
		level = slog.LevelWarn
	}
	slog.Default().Log(ctx, level, "reconcile", attrs...)
}
