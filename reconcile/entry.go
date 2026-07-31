// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli/charts"
)

// removeDrain bounds how long Remove waits for work it has just cancelled.
//
// It cannot be unbounded. Cancellation does not reach every daemon call —
// charts.Backend declares DeployStack, RemoveStack and StackServices without a
// context, and CE's waitReady polls with time.Sleep — so a sync inside a rollout
// can legitimately take minutes to notice it was cancelled. Removals run first
// in an app-set pass, so an unbounded wait there would hold up every other
// application's membership change behind one departing application.
//
// Past it Remove reports ErrStillSyncing rather than pretending. The app-set
// loop turns that into a held prune, which is a condition it already models and
// reports, so nothing is deleted while something is still writing it. A variable
// so a test does not have to spend it.
var removeDrain = 30 * time.Second

// drainTimeout bounds how long shutdown waits for in-flight syncs.
//
// Swarm sends SIGKILL 10s after SIGTERM — stack.yml sets no stop_grace_period —
// and the HTTP drain has to come out of the same budget, so this is the larger
// half of it rather than the whole. Past it the syncs are named in a warning and
// abandoned: they hold nothing another process needs, and the alternative is
// being killed mid-write with nothing said about it.
var drainTimeout = 5 * time.Second

// errStillSyncing reports that work for an application was cancelled but had not
// stopped before the caller gave up waiting.
//
// It never leaves this package. Removal itself does not fail because of it — the
// application really has left the set — so reporting it as a failed Remove would
// make callers retry something that already happened. What a caller needs is the
// separate question "has its work stopped", and Draining answers that one.
var errStillSyncing = errors.New("still syncing")

// ErrSyncPending reports that a manual sync was not started because one is
// already running with another already queued behind it.
//
// Not an error the caller can do anything about, and deliberately not a failure:
// the queued sync will read the same repository and deploy the same state, so
// the request has been honoured by the time it matters. It exists so the API can
// say which of the two happened rather than claiming to have started something
// it did not.
var ErrSyncPending = errors.New("a sync is already queued for this application")

// appEntry is one application's mutable record: the spec it reconciles against,
// what was last observed of it, and — through the lease below — every piece of
// work being done on its behalf.
//
// It is the unit of ownership. Before this existed as one thing, four separate
// mechanisms each knew a different subset of what was running: cancel and done
// knew the loop goroutine, Reconciler.wg knew the loop goroutine, the sync mutex
// knew the loop and the API's detached sync but was not a handle on either, and
// the status map was keyed by name so it could not tell one entry from its
// successor. Remove documented a guarantee that spanned all four and waited on
// one of them.
//
// Every field except name, ctx, cancel and the lease channels is guarded by
// Reconciler.mu.
type appEntry struct {
	// name is the application this entry is, fixed for its whole life: a
	// Replace keys on the name and cannot change it, and a change of name is a
	// Remove and an Add, which the caller does. That makes it safe to read
	// without the lock, and it is what current compares against.
	name string

	// ctx is this entry's lifetime, and cancel ends it. Everything done on the
	// application's behalf derives from it — the loop goroutine runs on it, and
	// a sync the API started is linked to it by acquire — so one call stops
	// the application's work whoever started it. The controller's own shutdown
	// reaches it through startLoopLocked's link to the run context.
	ctx    context.Context
	cancel context.CancelFunc

	// held is the lease: capacity one, send to take it, receive to return it.
	//
	// A channel rather than a Mutex because acquiring has to be abandonable. A
	// shutdown, or a Remove, must not queue behind a sync that is blocked in an
	// unresponsive daemon — which is exactly the case both exist to handle.
	// Holding it is also what proves the application is still a member, and
	// taking it is how Remove and shutdown prove nobody else is working.
	held chan struct{}

	// pending is the one queued manual sync a request may leave behind while
	// another is running. Capacity one, so N simultaneous requests collapse to
	// the one already waiting rather than each redeploying the swarm in turn.
	pending chan struct{}

	spec   application.Spec
	status application.Status
	// plan is the last plan for this application so the diff endpoint can be
	// served without re-rendering. A plan carries whole manifests, which is why
	// it is kept per application rather than per revision.
	plan *charts.Plan

	// pruneFailures counts consecutive failed deletions of one resource, keyed
	// by pruneKey, and is the whole of what makes maxPruneAttempts possible: a
	// sweep re-derives its candidates from the swarm on every pass and so
	// cannot otherwise tell the first refusal from the hundredth.
	//
	// Kept in memory rather than on the swarm deliberately. It is a judgement
	// about how long this controller has been trying, not a fact about the
	// deployment, and a restart is a reasonable moment to try again — the
	// operator may well have restarted it *because* they cleared whatever was
	// holding the resource.
	pruneFailures map[string]int

	// done is closed when this application's loop goroutine has returned. It is
	// nil until the loop is started — before Run, or for an application added to
	// a not-yet-running reconciler — and starting is what it gates on, since
	// cancel is now set from construction.
	done chan struct{}
}

// newEntry returns an entry for spec, alive but not yet reconciling.
//
// The context is rooted at Background rather than at the run context because an
// entry outlives neither: an application can be added before Run is called, and
// Run links the two when it starts the loop. Nothing derives from this context
// until then, so an entry that is never started leaks nothing but the cancel
// func Remove would have called.
func newEntry(spec application.Spec) *appEntry {
	ctx, cancel := context.WithCancel(context.Background())
	return &appEntry{
		name:    spec.Name,
		ctx:     ctx,
		cancel:  cancel,
		held:    make(chan struct{}, 1),
		pending: make(chan struct{}, 1),
		spec:    spec,
		status:  application.Status{Sync: application.Sync{State: application.SyncUnknown}},
	}
}

// acquire takes the application's exclusive right to be reconciled.
//
// It returns the context the work runs under and the function that returns the
// lease. The context is the caller's, additionally cancelled when the entry is —
// which is how Remove and shutdown reach a sync started through the API, whose
// own context is deliberately severed from the request that asked for it.
//
// A retired entry is refused rather than queued. Starting work on an application
// that has left the set is the interleaving Remove's drain exists to prevent, and
// the check is repeated after the wait because the wait is where it happens.
func (e *appEntry) acquire(ctx context.Context) (context.Context, func(), error) {
	if err := e.ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("reconciling %q: %w", e.name, err)
	}

	select {
	case e.held <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-e.ctx.Done():
		return nil, nil, fmt.Errorf("reconciling %q: %w", e.name, e.ctx.Err())
	}

	if err := e.ctx.Err(); err != nil {
		<-e.held
		return nil, nil, fmt.Errorf("reconciling %q: %w", e.name, err)
	}

	work, cancel := context.WithCancel(ctx)
	// AfterFunc rather than a watcher goroutine, and its stop is called on
	// release so a completed sync unregisters instead of holding the entry alive
	// until it is retired.
	stop := context.AfterFunc(e.ctx, cancel)
	return work, func() {
		stop()
		cancel()
		<-e.held
	}, nil
}

// drain waits until no work is being done for this application, or until d has
// passed. Taking the lease is the proof: nobody else can hold it while this
// call does.
//
// The caller cancels first. Draining without cancelling would wait out whatever
// the sync was doing rather than stopping it.
func (e *appEntry) drain(d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case e.held <- struct{}{}:
		<-e.held
		return nil
	case <-timer.C:
		return fmt.Errorf("%q was cancelled but is %w after %s", e.name, errStillSyncing, d)
	}
}

// busy reports whether work is in flight right now.
//
// Taking the lease and giving it straight back is the test: nothing else can
// hold it in between, because it has capacity one and this call holds it. The
// answer is a snapshot — work can start again immediately afterwards — which is
// exactly right for the two callers, both of which ask about an application that
// has already been cancelled and so cannot start anything new.
func (e *appEntry) busy() bool {
	select {
	case e.held <- struct{}{}:
		<-e.held
		return false
	default:
		return true
	}
}
