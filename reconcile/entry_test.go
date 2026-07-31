// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/git"
)

// settle is how long a test waits before concluding that something which should
// not have happened has not happened. Short, because every case below is a
// handoff between two goroutines rather than any real work.
const settle = 100 * time.Millisecond

// respond is how long a test waits for something that should happen. Generous,
// because failing here means a genuine hang and the cost of the margin is only
// paid when the test is already failing.
const respond = 5 * time.Second

// gateFetcher holds a reconcile inside Fetch until the test lets it out, and
// hands back the context that reconcile was given.
//
// stopsOnCancel is the whole reason this exists in two modes. With it set the
// fetch behaves like the cancellable half of a sync and is the way to observe
// that a context really was cancelled. Without it the fetch stands in for the
// half that cannot be cancelled at all — charts.Backend declares DeployStack,
// RemoveStack and StackServices with no context, and CE's waitReady polls with
// time.Sleep — which is the case every bound in this file exists for. A fake
// that always stopped on cancel could not exercise a single one of them.
type gateFetcher struct {
	entered       chan context.Context
	release       chan struct{}
	stopsOnCancel bool
}

func newGate(stopsOnCancel bool) *gateFetcher {
	return &gateFetcher{
		entered:       make(chan context.Context, 16),
		release:       make(chan struct{}),
		stopsOnCancel: stopsOnCancel,
	}
}

func (g *gateFetcher) Fetch(ctx context.Context, app string, _ application.Source) (git.Checkout, error) {
	select {
	case g.entered <- ctx:
	default:
	}

	if g.stopsOnCancel {
		select {
		case <-g.release:
		case <-ctx.Done():
			return git.Checkout{}, ctx.Err()
		}
	} else {
		<-g.release
	}
	return git.Checkout{Dir: "/tmp/" + app, Revision: strings.Repeat("a", 40)}, nil
}

// await returns the context of the next reconcile to reach the gate.
func (g *gateFetcher) await(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-g.entered:
		return ctx
	case <-time.After(respond):
		t.Fatal("no reconcile reached the fetcher")
		return nil
	}
}

func (g *gateFetcher) let() { close(g.release) }

// blockingBackend holds the swarm read record makes until the test lets it out.
// It is the honest reader, so it is the one record actually calls.
type blockingBackend struct {
	charts.Backend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingBackend) StackServices(string) []charts.ServiceState { return nil }

func (b *blockingBackend) ReadStackServices(string) ([]charts.ServiceState, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return nil, nil
}

func syncing(t *testing.T, r *Reconciler, app string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.SyncNow(context.Background(), app) }()
	return done
}

// TestRemoveWaitsForASyncItDidNotStart is the guarantee Remove's comment has
// always made and could not keep: the API's sync runs detached from the request
// and was registered with nothing, so Remove waited on the loop goroutine and
// reported an application quiescent while a deploy for it was still in flight.
// appset.Loop is the caller that relies on it, and with prune enabled the
// interleaving recreated a stack whose volumes were being deleted.
func TestRemoveWaitsForASyncItDidNotStart(t *testing.T) {
	gate := newGate(false)
	r := newTest(t, []application.Spec{spec("edge", true)}, &fakeEngine{plans: []*charts.Plan{synced()}}, gate)

	inflight := syncing(t, r, "edge")
	gate.await(t)

	removed := make(chan error, 1)
	go func() { removed <- r.Remove("edge") }()

	select {
	case <-removed:
		t.Fatal("Remove returned while a sync it did not start was still running")
	case <-time.After(settle):
	}

	gate.let()

	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("Remove = %v, want nil", err)
		}
	case <-time.After(respond):
		t.Fatal("Remove did not return once the sync finished")
	}
	<-inflight
}

// TestRemoveCancelsASyncItDidNotStart is the other half. Waiting is only
// tolerable because the wait is short, and it is short because the work is
// cancelled first — through the entry's own context, which a sync started
// through the API is linked to precisely so that it can be reached here.
func TestRemoveCancelsASyncItDidNotStart(t *testing.T) {
	gate := newGate(true)
	r := newTest(t, []application.Spec{spec("edge", true)}, &fakeEngine{plans: []*charts.Plan{synced()}}, gate)

	inflight := syncing(t, r, "edge")
	ctx := gate.await(t)

	removed := make(chan error, 1)
	go func() { removed <- r.Remove("edge") }()

	select {
	case <-ctx.Done():
	case <-time.After(respond):
		t.Fatal("Remove did not cancel the context of a sync started through the API")
	}
	<-inflight
	<-removed
}

// TestALateWriteDoesNotLandOnTheSuccessorOfItsName is the identity check.
//
// Membership was a name lookup, and a name that leaves the set and returns gets
// a fresh entry. The departed application's sync — cancelled, but not yet
// finished — then passed "is this name present", and overwrote the new
// application's status and plan with the old one's readings.
func TestALateWriteDoesNotLandOnTheSuccessorOfItsName(t *testing.T) {
	gate := newGate(false)
	r := newTest(t, []application.Spec{spec("edge", true)}, &fakeEngine{plans: []*charts.Plan{synced()}}, gate)

	departing := syncing(t, r, "edge")
	gate.await(t)

	removed := make(chan error, 1)
	go func() { removed <- r.Remove("edge") }()

	// The name returns while the departed application's sync is still running.
	// Remove has released the map lock by now — it is waiting, not holding — so
	// this is the interleaving, not a contrivance around one.
	successor := spec("edge", true)
	successor.Source.RepoURL = "https://example.com/successor.git"
	waitFor(t, "the name to return to the set", func() bool { return r.Add(successor) == nil })

	gate.let()
	<-departing
	<-removed

	view, ok := r.View("edge")
	if !ok {
		t.Fatal("the successor is not in the set")
	}
	if view.Spec.Source.RepoURL != successor.Source.RepoURL {
		t.Fatalf("spec = %q, want the successor's — the wrong entry is under this name", view.Spec.Source.RepoURL)
	}
	if view.Status.Sync.State != application.SyncUnknown {
		t.Fatalf("state = %q, want unknown: the departed application's reading was written onto its successor",
			view.Status.Sync.State)
	}
	if view.Status.Sync.Revision != "" {
		t.Fatalf("revision = %q, want empty: the successor has not synced", view.Status.Sync.Revision)
	}
}

// TestRecordDoesNotHoldTheGlobalLockAcrossTheSwarmRead is item 1 of #106.
//
// record took the exclusive lock and then read the swarm once per release, each
// read a whole-swarm snapshot on an uncancellable context. One application whose
// daemon was slow therefore blocked every API read and every other
// application's reconcile, and because Go parks readers behind a waiting writer
// the starvation spread. /healthz touches none of it and kept answering, so
// nothing restarted the container.
func TestRecordDoesNotHoldTheGlobalLockAcrossTheSwarmRead(t *testing.T) {
	backend := &blockingBackend{entered: make(chan struct{}, 1), release: make(chan struct{})}
	r := newTestWith(t, []application.Spec{spec("edge", true)},
		&fakeEngine{plans: []*charts.Plan{synced()}}, nil, fakeRegistry{backend: backend})

	inflight := syncing(t, r, "edge")

	select {
	case <-backend.entered:
	case <-time.After(respond):
		t.Fatal("record never reached the swarm read")
	}

	// The reads the API serves, while the daemon read is still outstanding.
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		r.Views()
		r.View("edge")
	}()

	select {
	case <-answered:
	case <-time.After(respond):
		t.Fatal("a status read blocked behind the swarm read record was doing")
	}

	close(backend.release)
	<-inflight
}

// TestAPanicFailsOneApplicationRatherThanTheProcess is #105's remaining half.
//
// recover() appeared nowhere in this repository, so a panic in one application's
// loop killed every other application's reconcile, the app-set loop, and the API
// that would have explained it — the exact blast radius this package's doc
// comment rejects for an unreachable repository.
func TestAPanicFailsOneApplicationRatherThanTheProcess(t *testing.T) {
	r := newTest(t, []application.Spec{spec("bad", true), spec("good", true)},
		&fakeEngine{plans: []*charts.Plan{synced(), synced()}}, panicFetcher{on: "bad"})

	err := r.Sync(context.Background(), "bad")
	if err == nil {
		t.Fatal("Sync = nil, want the recovered panic reported as a failure")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("error = %q, want it to say a panic was recovered", err)
	}

	view, _ := r.View("bad")
	if !strings.Contains(view.Status.Error, "panic") {
		t.Fatalf("status error = %q, want the panic recorded against this application", view.Status.Error)
	}

	// The point of recovering at all: the others still reconcile.
	if err := r.Sync(context.Background(), "good"); err != nil {
		t.Fatalf("the second application failed too: %v", err)
	}
	if view, _ := r.View("good"); view.Status.Sync.State != application.SyncSynced {
		t.Fatalf("state = %q, want synced", view.Status.Sync.State)
	}
}

type panicFetcher struct{ on string }

func (p panicFetcher) Fetch(_ context.Context, app string, _ application.Source) (git.Checkout, error) {
	if app == p.on {
		panic("the daemon returned a service with no container spec")
	}
	return git.Checkout{Dir: "/tmp/" + app, Revision: strings.Repeat("a", 40)}, nil
}

// TestManualSyncsCollapseOntoTheOneAlreadyQueued is #106's undeduplicated sync.
// Twenty requests each returned 202 and then serialised, redeploying the swarm
// twenty times with the scheduled tick queued behind all of them.
func TestManualSyncsCollapseOntoTheOneAlreadyQueued(t *testing.T) {
	gate := newGate(false)
	r := newTest(t, []application.Spec{spec("edge", true)}, &fakeEngine{plans: []*charts.Plan{synced()}}, gate)

	running := syncing(t, r, "edge")
	gate.await(t)

	// One queued behind it. It reserves the slot and then waits for the lease.
	queued := syncing(t, r, "edge")
	waitFor(t, "the second request to be queued", func() bool { return len(r.mustEntry(t, "edge").pending) == 1 })

	if _, err := r.AcceptSync("edge"); !errors.Is(err, ErrSyncPending) {
		t.Fatalf("AcceptSync = %v, want ErrSyncPending: a third request should collapse onto the queued one", err)
	}

	gate.let()
	<-running
	<-queued
}

// TestDrainingHoldsUntilADepartedApplicationStops is what makes the bounded wait
// safe. Remove gives up after removeDrain, so "removed" and "quiescent" are two
// questions; deleting a departed application's resources while its sync is still
// writing them is the same interleaving Remove exists to prevent, one pass later.
func TestDrainingHoldsUntilADepartedApplicationStops(t *testing.T) {
	shorten(t, &removeDrain, 20*time.Millisecond)

	gate := newGate(false)
	r := newTest(t, []application.Spec{spec("edge", true)}, &fakeEngine{plans: []*charts.Plan{synced()}}, gate)

	inflight := syncing(t, r, "edge")
	gate.await(t)

	if err := r.Remove("edge"); err != nil {
		t.Fatalf("Remove = %v, want nil: the removal succeeded even though the work has not stopped", err)
	}
	if got := r.Draining(); len(got) != 1 || got[0] != "edge" {
		t.Fatalf("Draining = %v, want [edge]: its sync is still running", got)
	}

	gate.let()
	<-inflight

	waitFor(t, "the departed application to stop", func() bool { return len(r.Draining()) == 0 })
}

// TestRunDrainsWithABound is the shutdown half. The drain used to be an
// unbounded wg.Wait that did not count the API's syncs at all, so a sync blocked
// in an unresponsive daemon meant Run never returned and the process was
// SIGKILLed with nothing said about it.
func TestRunDrainsWithABound(t *testing.T) {
	shorten(t, &drainTimeout, 50*time.Millisecond)

	gate := newGate(false)
	r := newTest(t, []application.Spec{spec("edge", false)}, &fakeEngine{plans: []*charts.Plan{synced()}}, gate)

	ctx, cancel := context.WithCancel(context.Background())
	run := make(chan error, 1)
	go func() { run <- r.Run(ctx) }()
	gate.await(t)

	cancel()

	select {
	case err := <-run:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(respond):
		t.Fatal("Run did not return: the drain waited for a sync that cannot be cancelled")
	}

	gate.let()
}

// mustEntry is the entry behind a name, for the tests that need to look at the
// lease rather than at what it protects.
func (r *Reconciler) mustEntry(t *testing.T, app string) *appEntry {
	t.Helper()
	e, err := r.entry(app)
	if err != nil {
		t.Fatalf("entry(%q): %v", app, err)
	}
	return e
}

// shorten swaps a package-level bound for the duration of one test. The bounds
// are variables for exactly this: a test should not have to spend thirty seconds
// to observe what happens after thirty seconds.
func shorten(t *testing.T, d *time.Duration, to time.Duration) {
	t.Helper()
	was := *d
	*d = to
	t.Cleanup(func() { *d = was })
}
