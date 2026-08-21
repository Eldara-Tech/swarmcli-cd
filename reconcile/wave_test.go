// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/compose"
)

// ------------------------------------------------------- waves on the converge path
//
// The apply path needs nothing here: Engine.Apply groups and waits, and its
// signature did not change, so the reconciler calls it exactly as it did. What
// this repository owns is the correction path, which cannot go through the engine
// at all — the engine skips a release it planned as unchanged, and every
// live-drifted release is unchanged by construction.

// wavePlan is two releases in two waves, both of which the live comparison will
// find drifted. Written with wave 1 first so that a converge walking the slice
// as it stands would get the order wrong; PlanApply sorts, and these fixtures
// stand in for what it returns.
func wavePlan() *charts.Plan {
	return &charts.Plan{Releases: []charts.ReleasePlan{
		{Name: "db", Ref: "repo/db", Action: charts.ActionUnchanged, ToVersion: "1", Manifest: "db-manifest", Wave: 0},
		{Name: "api", Ref: "repo/api", Action: charts.ActionUnchanged, ToVersion: "1", Manifest: "api-manifest", Wave: 1},
	}}
}

// oneWavePlan is the same two releases with no wave declared between them, which
// is what every release file written before waves existed produces.
func oneWavePlan() *charts.Plan {
	p := wavePlan()
	p.Releases[1].Wave = 0
	return p
}

// bothDrifted is a backend on which both releases are running three replicas
// where their manifests ask for one.
func bothDrifted() *driftBackend {
	want, got := serviceSpec("db_app", 1), serviceSpec("db_app", 3)
	apiWant, apiGot := serviceSpec("api_app", 1), serviceSpec("api_app", 3)
	return &driftBackend{
		desired: map[string]*compose.Stack{
			"db":  {Services: []compose.Service{{Name: "app", Spec: want}}},
			"api": {Services: []compose.Service{{Name: "app", Spec: apiWant}}},
		},
		live: map[string]map[string]swarm.Service{
			"db":  {"db_app": {ID: "db-id", Spec: got}},
			"api": {"api_app": {ID: "api-id", Spec: apiGot}},
		},
	}
}

// waveBackend answers convergence per release rather than from one script, which
// is what a wave test needs: the point is that one release holds up another, and
// a backend that replied the same thing for both could not express it.
//
// It keeps ONE ordered log of both deploys and convergence reads, and that is the
// point rather than a convenience. StackServices is not only what a barrier
// calls: record reads it for every deployed release to compute health, on every
// reconcile, waves or no waves. So "did the swarm get asked about convergence" is
// not a question a read count can answer — the honest question is whether
// anything was asked *between* two corrections, which is exactly what a barrier
// is and what a read count cannot see.
type waveBackend struct {
	*driftBackend

	mu sync.Mutex
	// states is the convergence answer per release. A release absent from it is
	// converged, which keeps the fixtures short.
	states map[string][]charts.ServiceState
	// events is "deploy:<release>" and "read:<release>", in order.
	events []string
}

func newWaveBackend(states map[string][]charts.ServiceState) *waveBackend {
	return &waveBackend{driftBackend: bothDrifted(), states: states}
}

func (b *waveBackend) StackServices(_ context.Context, release string) []charts.ServiceState {
	b.mu.Lock()
	b.events = append(b.events, "read:"+release)
	states, ok := b.states[release]
	b.mu.Unlock()

	if ok {
		return states
	}
	return []charts.ServiceState{{Name: release + "_app", Running: 1, Desired: 1, NewestTaskAge: time.Hour}}
}

func (b *waveBackend) DeployStack(ctx context.Context, req charts.DeployRequest) error {
	b.mu.Lock()
	b.events = append(b.events, "deploy:"+req.Name)
	b.mu.Unlock()
	return b.driftBackend.DeployStack(ctx, req)
}

func (b *waveBackend) log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.events...)
}

// barriered reports whether anything asked the swarm about convergence between
// the two named deploys — a barrier, as opposed to the health reads that bracket
// every reconcile.
func (b *waveBackend) barriered(first, second string) bool {
	events := b.log()
	from, to := -1, -1
	for i, e := range events {
		switch e {
		case "deploy:" + first:
			from = i
		case "deploy:" + second:
			if from >= 0 && to < 0 {
				to = i
			}
		}
	}
	if from < 0 || to < 0 {
		return false
	}
	for _, e := range events[from:to] {
		if strings.HasPrefix(e, "read:") {
			return true
		}
	}
	return false
}

// waveSpec is a live-drift application that has NOT asked for syncPolicy.wait.
// That is the interesting configuration: the wave barrier has to hold anyway, or
// declaring an ordering in the release file would silently do nothing for
// anybody who had not also set an unrelated policy in a different file.
func waveSpec(name string) application.Spec {
	return liveSpec(name, true)
}

// newTestWithAdvancingClock is newTestWith with a clock that moves.
//
// newTestWith freezes time at the epoch, which is right for almost everything
// here and fatal for anything that has to reach a deadline: a wait polling a
// frozen clock never times out, so a test that expects one hangs instead of
// failing. Every test below that stages a release which never converges needs
// this, including the ones asserting a wait does NOT happen — a barrier that
// wrongly fired would otherwise stall the suite rather than turn it red.
func newTestWithAdvancingClock(t *testing.T, spec application.Spec, engine Engine, backend charts.Backend) *Reconciler {
	t.Helper()
	var ticks int
	return New([]application.Spec{spec}, Options{
		Fetcher:   &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{backend: backend},
		NewEngine: func(charts.Backend) Engine { return engine },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time {
			ticks++
			return time.Unix(0, 0).UTC().Add(time.Duration(ticks) * time.Minute)
		},
	})
}

// Corrections happen in wave order, and the barrier between waves holds even
// though the application never asked for syncPolicy.wait.
func TestConvergeCorrectsInWaveOrder(t *testing.T) {
	fastPolling(t)
	backend := newWaveBackend(nil)
	engine := &fakeEngine{plans: []*charts.Plan{wavePlan()}}
	r := newTestWith(t, []application.Spec{waveSpec("edge")}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	got := backend.converged()
	if len(got) != 2 || !strings.HasPrefix(got[0], "db|") || !strings.HasPrefix(got[1], "api|") {
		t.Fatalf("corrected %v, want db before api — the wave order, not the plan's slice order", got)
	}
	// And the barrier really ran: the swarm was asked about convergence between
	// the two corrections. Without it the writes would be back to back and the
	// ordering above would be a coincidence rather than a guarantee.
	if !backend.barriered("db", "api") {
		t.Errorf("no convergence read between the two corrections: %v", backend.log())
	}
}

// A wave that does not settle stops the corrections after it.
//
// This is the converge-path half of the acceptance criterion: the same
// dependency that must stop an install from running ahead of its migration must
// stop a correction from doing it.
func TestConvergeStopsAtAWaveThatDoesNotSettle(t *testing.T) {
	fastPolling(t)
	backend := newWaveBackend(map[string][]charts.ServiceState{
		"db": {{Name: "db_app", Running: 0, Desired: 1}}, // never comes up
	})
	engine := &fakeEngine{plans: []*charts.Plan{wavePlan()}}
	spec := waveSpec("edge")
	spec.SyncPolicy.Timeout = application.Duration(3 * time.Minute)
	r := newTestWithAdvancingClock(t, spec, engine, backend)

	err := r.Sync(context.Background(), "edge")
	if err == nil {
		t.Fatal("Sync = nil, want the failed wave to fail the sync")
	}
	if !strings.Contains(err.Error(), "wave 0") {
		t.Errorf("error = %v, want it to name the wave that did not settle", err)
	}

	got := backend.converged()
	if len(got) != 1 || !strings.HasPrefix(got[0], "db|") {
		t.Fatalf("corrected %v, want only wave 0 — api is in the wave after the one that stalled", got)
	}
}

// With one wave there is no boundary, so nothing waits — for an application that
// did not ask for syncPolicy.wait, a correction returns as soon as the daemon
// accepts the write, exactly as it did before waves existed.
//
// The guard is on the absence of the new behaviour. Without it, waves would have
// added a convergence wait to every application whose release file says nothing
// about them, which is all of them.
func TestConvergeOfASingleWaveNeverWaits(t *testing.T) {
	fastPolling(t)
	// Both releases are staged as never converging. A barrier that fired would
	// therefore time out and fail the sync rather than hang, which is what makes
	// this fail cleanly instead of stalling the suite.
	backend := newWaveBackend(map[string][]charts.ServiceState{
		"db":  {{Name: "db_app", Running: 0, Desired: 1}},
		"api": {{Name: "api_app", Running: 0, Desired: 1}},
	})
	engine := &fakeEngine{plans: []*charts.Plan{oneWavePlan()}}
	spec := waveSpec("edge")
	spec.SyncPolicy.Timeout = application.Duration(3 * time.Minute)
	r := newTestWithAdvancingClock(t, spec, engine, backend)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil — one wave is no boundary", err)
	}
	if got := len(backend.converged()); got != 2 {
		t.Errorf("corrected %d releases, want both", got)
	}
	if backend.barriered("db", "api") {
		t.Errorf("waited between two releases in the same wave: %v", backend.log())
	}
}

// syncPolicy.wait still means what it always did underneath: each individual
// release blocks until it has settled. Waves changed the ordering, not this.
func TestWaitStillBlocksEachReleaseInsideAWave(t *testing.T) {
	fastPolling(t)
	backend := newWaveBackend(nil)
	engine := &fakeEngine{plans: []*charts.Plan{oneWavePlan()}}
	spec := waveSpec("edge")
	spec.SyncPolicy.Wait = true
	r := newTestWith(t, []application.Spec{spec}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	// One wave, so no wave barrier — but wait is set, so each release was waited
	// for on its own, which shows up as a read between the two corrections just
	// as a barrier would. That is the point: wait still means what it meant.
	if !backend.barriered("db", "api") {
		t.Errorf("no wait between the corrections under syncPolicy.wait: %v", backend.log())
	}
}

// waveGroups splits on the wave boundary and nowhere else. It is this
// repository's own copy of the engine's grouping, because the correction path
// cannot go through Engine.Apply, so it gets its own test.
func TestWaveGroups(t *testing.T) {
	if got := waveGroups(nil); got != nil {
		t.Errorf("waveGroups(nil) = %v, want nil", got)
	}

	got := waveGroups([]charts.ReleasePlan{
		{Name: "a", Wave: 0}, {Name: "b", Wave: 0},
		{Name: "c", Wave: 1},
		{Name: "d", Wave: 7}, {Name: "e", Wave: 7},
	})
	if len(got) != 3 {
		t.Fatalf("got %d groups, want 3", len(got))
	}
	if len(got[0]) != 2 || len(got[1]) != 1 || len(got[2]) != 2 {
		t.Errorf("group sizes %d/%d/%d, want 2/1/2", len(got[0]), len(got[1]), len(got[2]))
	}
}

// releaseList is what a timed-out wait names, and one release must read exactly
// as it did before the wait could take more than one — the integration suite
// matches on that text.
func TestReleaseListReadsTheSameForOneRelease(t *testing.T) {
	if got := releaseList([]string{"solo"}); got != `release 'solo'` {
		t.Errorf("releaseList = %q, want the phrase a single-release wait always produced", got)
	}
	if got := releaseList([]string{"a", "b"}); got != `releases 'a', 'b'` {
		t.Errorf("releaseList = %q, want both named", got)
	}
}
