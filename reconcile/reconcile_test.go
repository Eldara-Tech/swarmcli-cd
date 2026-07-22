// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
	"github.com/Eldara-Tech/swarmcli-cd/source"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// ---------------------------------------------------------------- fakes

type fakeFetcher struct {
	mu       sync.Mutex
	revision string
	err      error
	calls    int
}

func (f *fakeFetcher) Fetch(_ context.Context, app string, _ application.Source) (git.Checkout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return git.Checkout{}, f.err
	}
	return git.Checkout{Dir: "/tmp/" + app, Revision: f.revision}, nil
}

func (f *fakeFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeBuilder struct{ err error }

func (f *fakeBuilder) Build(context.Context, string, application.Source, git.Checkout) (*source.Built, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &source.Built{
		ReleaseFile: &charts.ReleaseFile{},
		ReadFile:    func(string) ([]byte, error) { return nil, nil },
	}, nil
}

type fakeRegistry struct {
	err     error
	backend charts.Backend
}

func (f fakeRegistry) Backend(context.Context, string) (charts.Backend, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.backend != nil {
		return f.backend, nil
	}
	return stubBackend{}, nil
}

// stubBackend is a charts.Backend that only answers the one question the
// reconcile loop asks of it directly: which services a release has. charts.Backend
// is embedded nil, so anything else panics naming the method rather than
// returning a zero value the loop would then reason about.
type stubBackend struct {
	charts.Backend
	states map[string][]charts.ServiceState
}

func (s stubBackend) StackServices(name string) []charts.ServiceState { return s.states[name] }

// fakeEngine returns a scripted sequence of plans, so a test can say "out of
// sync, then synced after the apply".
type fakeEngine struct {
	mu        sync.Mutex
	plans     []*charts.Plan
	planErr   error
	planErrAt int // 1-based call number at which PlanApply starts failing
	applyErr  error
	applied   int
	planCalls int
	opts      []charts.InstallOptions
	planOpts  []charts.PlanOptions
}

func (e *fakeEngine) PlanApply(_ context.Context, _ *charts.ReleaseFile, _ charts.ChartSource, opts charts.PlanOptions) (*charts.Plan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.planCalls++
	e.planOpts = append(e.planOpts, opts)
	if e.planErr != nil && e.planCalls >= max(e.planErrAt, 1) {
		return nil, e.planErr
	}
	plan := e.plans[min(e.planCalls-1, len(e.plans)-1)]
	return plan, nil
}

func (e *fakeEngine) Apply(_ context.Context, plan *charts.Plan, opts charts.InstallOptions) ([]charts.ApplyResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applied++
	e.opts = append(e.opts, opts)
	if e.applyErr != nil {
		return nil, e.applyErr
	}
	out := make([]charts.ApplyResult, 0, len(plan.Releases))
	for _, rp := range plan.Releases {
		out = append(out, charts.ApplyResult{Name: rp.Name, Action: rp.Action, Revision: 1})
	}
	return out, nil
}

func (e *fakeEngine) applyCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applied
}

type recorder struct {
	mu  sync.Mutex
	got []notify.Event
}

func (r *recorder) Notify(_ context.Context, e notify.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, e)
}

func (r *recorder) types() []notify.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]notify.EventType, 0, len(r.got))
	for _, e := range r.got {
		out = append(out, e.Type)
	}
	return out
}

func listen(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	notify.Register("test", rec)
	return rec
}

// ---------------------------------------------------------------- helpers

func outOfSync() *charts.Plan {
	return &charts.Plan{Releases: []charts.ReleasePlan{
		{Name: "whoami", Ref: "repo/whoami", Action: charts.ActionUpgrade, ToVersion: "0.1.8",
			CurrentManifest: "replicas: 1\n", Manifest: "replicas: 3\n"},
	}}
}

func synced() *charts.Plan {
	return &charts.Plan{Releases: []charts.ReleasePlan{
		{Name: "whoami", Ref: "repo/whoami", Action: charts.ActionUnchanged, ToVersion: "0.1.8"},
	}}
}

func spec(name string, automated bool) application.Spec {
	return application.Spec{
		Name:           name,
		Source:         application.Source{RepoURL: "https://example.com/x.git", Revision: "main"},
		SyncPolicy:     application.SyncPolicy{Automated: automated},
		DriftDetection: application.DriftManifest,
	}
}

func newTest(t *testing.T, apps []application.Spec, engine Engine, fetcher Fetcher) *Reconciler {
	t.Helper()
	if fetcher == nil {
		fetcher = &fakeFetcher{revision: strings.Repeat("a", 40)}
	}
	return New(apps, Options{
		Fetcher:   fetcher,
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{},
		NewEngine: func(charts.Backend) Engine { return engine },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})
}

// ---------------------------------------------------------------- tests

func TestSyncedApplicationDeploysNothing(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if engine.applyCount() != 0 {
		t.Error("a synced application was deployed")
	}
	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("state = %q, want synced", view.Status.Sync.State)
	}
	if view.Status.Sync.Revision != strings.Repeat("a", 40) {
		t.Errorf("revision = %q, want the fetched commit", view.Status.Sync.Revision)
	}
}

// An automated application applies, and the status afterwards comes from a
// fresh plan rather than from an assumption that the apply worked.
func TestAutomatedApplicationAppliesAndConfirms(t *testing.T) {
	rec := listen(t)
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync(), synced()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if engine.applyCount() != 1 {
		t.Errorf("applied %d times, want 1", engine.applyCount())
	}
	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("state = %q, want synced after a confirmed apply", view.Status.Sync.State)
	}
	if view.Status.Sync.LastSync == nil || !view.Status.Sync.LastSync.Succeeded {
		t.Errorf("lastSync = %+v, want a successful result", view.Status.Sync.LastSync)
	}
	want := []notify.EventType{notify.DriftDetected, notify.SyncStarted, notify.SyncSucceeded}
	if got := rec.types(); !equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// A chart whose render is not deterministic would deploy on every tick
// forever. Re-planning after the apply surfaces it on the first sync.
func TestApplyThatDoesNotConvergeStaysOutOfSync(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync()}} // never becomes unchanged
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Errorf("state = %q, want out-of-sync: the re-plan still wanted to change something", view.Status.Sync.State)
	}
}

// A manual policy means "do not deploy on a schedule", not "never deploy".
func TestManualPolicyReportsDriftButDoesNotDeploy(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync()}}
	r := newTest(t, []application.Spec{spec("edge", false)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if engine.applyCount() != 0 {
		t.Fatal("a manual application was deployed by a scheduled reconcile")
	}
	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Errorf("state = %q, want out-of-sync", view.Status.Sync.State)
	}

	engine.plans = []*charts.Plan{outOfSync(), synced()}
	engine.planCalls = 0
	if err := r.SyncNow(context.Background(), "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	if engine.applyCount() != 1 {
		t.Errorf("applied %d times, want the forced sync to deploy", engine.applyCount())
	}
}

// The controller stamps its own namespace so a release file applied by hand and
// an application reconciled here can never claim each other's releases — and it
// plans against that same id, or its own releases would come back Unmanaged.
func TestApplyStampsTheControllersOwner(t *testing.T) {
	planned := outOfSync()
	planned.Owner = "cd/edge" // what PlanApply returns for the owner it was given
	engine := &fakeEngine{plans: []*charts.Plan{planned, synced()}}
	r := newTest(t, []application.Spec{{
		Name:           "edge",
		Source:         application.Source{RepoURL: "https://example.com/x.git", Revision: "main"},
		SyncPolicy:     application.SyncPolicy{Automated: true, Wait: true, Timeout: application.Duration(90 * time.Second), HistoryMax: 20},
		DriftDetection: application.DriftManifest,
	}}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if got := engine.planOpts[0].Owner; got != "cd/edge" {
		t.Errorf("plan owner = %q, want cd/edge", got)
	}

	got := engine.opts[0]
	// From the plan, not recomputed: stamping an id the plan did not classify
	// against would install under one owner and hunt for orphans under another.
	if got.Owner != "cd/edge" {
		t.Errorf("owner = %q, want cd/edge", got.Owner)
	}
	if !got.Wait || got.Timeout != 90*time.Second || got.HistoryMax != 20 {
		t.Errorf("options = %+v, want the sync policy carried through", got)
	}
}

// The values reader is what carries repository content through the
// SecretProvider seam, so a plan built without it would read values files
// straight off disk and silently bypass the seam.
func TestPlanIsGivenTheValuesReader(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTest(t, []application.Spec{spec("edge", false)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if engine.planOpts[0].ReadFile == nil {
		t.Error("PlanApply was given no ReadFile; values would bypass the secrets seam")
	}
}

func TestFailedApplyRecordsTheResultAndTheError(t *testing.T) {
	rec := listen(t)
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync()}, applyErr: errors.New("swarm said no")}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	err := r.Sync(context.Background(), "edge")
	if err == nil {
		t.Fatal("Sync = nil, want the apply error")
	}
	if !strings.Contains(err.Error(), "swarm said no") {
		t.Errorf("error %q does not carry the reason", err)
	}

	view, _ := r.View("edge")
	if view.Status.Sync.LastSync == nil || view.Status.Sync.LastSync.Succeeded {
		t.Errorf("lastSync = %+v, want a failed result", view.Status.Sync.LastSync)
	}
	if view.Status.Error == "" {
		t.Error("status carries no error")
	}
	if got := rec.types(); got[len(got)-1] != notify.SyncFailed {
		t.Errorf("last event = %q, want sync-failed", got[len(got)-1])
	}
}

// A repository that cannot be reached does not make the swarm's state unknown.
// It makes it unverified, which is what the error says while the last observed
// status stands.
func TestFetchFailureKeepsTheLastKnownStatus(t *testing.T) {
	fetcher := &fakeFetcher{revision: strings.Repeat("a", 40)}
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, fetcher)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	fetcher.err = errors.New("host is down")

	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Fatal("Sync = nil, want the fetch error")
	}

	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("state = %q, want the last known state to stand", view.Status.Sync.State)
	}
	if !strings.Contains(view.Status.Error, "host is down") {
		t.Errorf("error = %q, want the fetch failure", view.Status.Error)
	}
}

func TestStepFailuresAreNamed(t *testing.T) {
	for name, tc := range map[string]struct {
		options func(*Options)
		want    string
	}{
		"build": {func(o *Options) { o.Builder = &fakeBuilder{err: errors.New("boom")} }, "reading source"},
		"swarm": {func(o *Options) { o.Swarms = fakeRegistry{err: errors.New("boom")} }, "resolving destination"},
		"planning": {func(o *Options) {
			o.NewEngine = func(charts.Backend) Engine { return &fakeEngine{planErr: errors.New("boom")} }
		}, "planning"},
	} {
		t.Run(name, func(t *testing.T) {
			o := Options{
				Fetcher:   &fakeFetcher{revision: "abc"},
				Builder:   &fakeBuilder{},
				Swarms:    fakeRegistry{},
				NewEngine: func(charts.Backend) Engine { return &fakeEngine{plans: []*charts.Plan{synced()}} },
				Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			tc.options(&o)

			err := New([]application.Spec{spec("edge", true)}, o).Sync(context.Background(), "edge")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// Drift is announced on the transition, not on every tick: a manual-policy
// application sits out of sync by design, and notifying each time would train
// an operator to ignore the one that matters.
func TestDriftIsAnnouncedOnce(t *testing.T) {
	rec := listen(t)
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync()}}
	r := newTest(t, []application.Spec{spec("edge", false)}, engine, nil)

	for range 3 {
		if err := r.Sync(context.Background(), "edge"); err != nil {
			t.Fatalf("Sync = %v, want nil", err)
		}
	}

	var drifts int
	for _, e := range rec.types() {
		if e == notify.DriftDetected {
			drifts++
		}
	}
	if drifts != 1 {
		t.Errorf("announced drift %d times, want once", drifts)
	}
}

// The deploy landed and only the confirmation failed, so the sync result
// stands as a success while the reconcile itself reports the error.
func TestReplanFailureAfterASuccessfulApply(t *testing.T) {
	engine := &fakeEngine{
		plans:     []*charts.Plan{outOfSync()},
		planErr:   errors.New("daemon went away"),
		planErrAt: 2,
	}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	err := r.Sync(context.Background(), "edge")
	if err == nil || !strings.Contains(err.Error(), "re-planning after apply") {
		t.Fatalf("Sync = %v, want a re-plan error", err)
	}
	if engine.applyCount() != 1 {
		t.Errorf("applied %d times, want 1", engine.applyCount())
	}

	view, _ := r.View("edge")
	if view.Status.Sync.LastSync == nil || !view.Status.Sync.LastSync.Succeeded {
		t.Errorf("lastSync = %+v, want the deploy recorded as successful", view.Status.Sync.LastSync)
	}
	if !strings.Contains(view.Status.Error, "re-planning") {
		t.Errorf("status error = %q, want the confirmation failure", view.Status.Error)
	}
}

func TestUnknownApplication(t *testing.T) {
	r := newTest(t, []application.Spec{spec("edge", true)}, &fakeEngine{plans: []*charts.Plan{synced()}}, nil)

	if err := r.Sync(context.Background(), "absent"); err == nil {
		t.Error("Sync = nil, want an error for an unknown application")
	}
	if _, ok := r.View("absent"); ok {
		t.Error("View reported an unknown application")
	}
	if _, err := r.Diffs("absent"); err == nil {
		t.Error("Diffs = nil, want an error for an unknown application")
	}
}

func TestDiffsComeFromTheLastPlan(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync()}}
	r := newTest(t, []application.Spec{spec("edge", false)}, engine, nil)

	if _, err := r.Diffs("edge"); !errors.Is(err, ErrNotPlanned) {
		t.Errorf("Diffs before any reconcile = %v, want ErrNotPlanned", err)
	}

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	got, err := r.Diffs("edge")
	if err != nil {
		t.Fatalf("Diffs = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Release != "whoami" {
		t.Fatalf("diffs = %+v, want the one changed release", got)
	}
	if !strings.Contains(got[0].Diff, "+ replicas: 3") {
		t.Errorf("diff does not show the change:\n%s", got[0].Diff)
	}
}

func TestViewsAreInDeclarationOrder(t *testing.T) {
	r := newTest(t, []application.Spec{spec("beta", true), spec("alpha", true)}, &fakeEngine{plans: []*charts.Plan{synced()}}, nil)

	got := r.Views()
	if len(got) != 2 || got[0].Spec.Name != "beta" || got[1].Spec.Name != "alpha" {
		t.Errorf("views = %v, want declaration order", []string{got[0].Spec.Name, got[1].Spec.Name})
	}
	// Before any reconcile the state is unknown rather than optimistically
	// synced.
	if got[0].Status.Sync.State != application.SyncUnknown {
		t.Errorf("state = %q, want unknown before the first reconcile", got[0].Status.Sync.State)
	}
}

// A destination the registry cannot resolve is a startup error, not a
// per-application failure discovered three minutes later.
func TestRunRejectsAnUnresolvableDestination(t *testing.T) {
	r := New([]application.Spec{spec("edge", true)}, Options{
		Fetcher:   &fakeFetcher{},
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{err: errors.New("unknown swarm")},
		NewEngine: func(charts.Backend) Engine { return &fakeEngine{plans: []*charts.Plan{synced()}} },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "destination") {
		t.Errorf("Run = %v, want a destination error", err)
	}
	if !strings.Contains(err.Error(), "edge") {
		t.Errorf("error %q does not name the application", err)
	}
}

func TestRunReconcilesRepeatedlyAndStops(t *testing.T) {
	fetcher := &fakeFetcher{revision: strings.Repeat("a", 40)}
	r := New([]application.Spec{spec("edge", true)}, Options{
		Fetcher:   fetcher,
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{},
		NewEngine: func(charts.Backend) Engine { return &fakeEngine{plans: []*charts.Plan{synced()}} },
		Interval:  time.Millisecond,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for fetcher.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d reconciles in two seconds", fetcher.count())
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}
}

// One application whose repository is unreachable must not stall the others.
func TestOneBrokenApplicationDoesNotStallTheOthers(t *testing.T) {
	healthy := &fakeFetcher{revision: strings.Repeat("a", 40)}
	broken := &fakeFetcher{err: errors.New("host is down")}

	r := New([]application.Spec{spec("broken", true), spec("healthy", true)}, Options{
		Fetcher:   routingFetcher{"broken": broken, "healthy": healthy},
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{},
		NewEngine: func(charts.Backend) Engine { return &fakeEngine{plans: []*charts.Plan{synced()}} },
		Interval:  time.Millisecond,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for healthy.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("the healthy application reconciled only %d times while another was failing", healthy.count())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

type routingFetcher map[string]*fakeFetcher

func (f routingFetcher) Fetch(ctx context.Context, app string, src application.Source) (git.Checkout, error) {
	return f[app].Fetch(ctx, app, src)
}

func TestBackoff(t *testing.T) {
	const interval = time.Minute

	if got := backoff(interval, 0); got != interval {
		t.Errorf("no failures = %v, want the interval", got)
	}
	if got := backoff(interval, 1); got != 2*interval {
		t.Errorf("one failure = %v, want double", got)
	}
	if got := backoff(interval, 3); got != 8*interval {
		t.Errorf("three failures = %v, want eight times", got)
	}
	if got := backoff(interval, 40); got != maxBackoff {
		t.Errorf("many failures = %v, want the cap", got)
	}
	// A long interval must not overflow into a negative duration.
	if got := backoff(24*time.Hour, 20); got != maxBackoff {
		t.Errorf("long interval = %v, want the cap", got)
	}
}

func TestIntervalOverride(t *testing.T) {
	r := newTest(t, nil, &fakeEngine{}, nil)

	if got := r.intervalFor(spec("edge", true)); got != DefaultInterval {
		t.Errorf("interval = %v, want the default", got)
	}
	custom := spec("edge", true)
	custom.SyncPolicy.Interval = application.Duration(30 * time.Second)
	if got := r.intervalFor(custom); got != 30*time.Second {
		t.Errorf("interval = %v, want the application's own", got)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	r := New(nil, Options{Fetcher: &fakeFetcher{}, Builder: &fakeBuilder{}})

	if r.interval != DefaultInterval {
		t.Errorf("interval = %v, want %v", r.interval, DefaultInterval)
	}
	if r.swarms == nil || r.newEngine == nil || r.log == nil || r.now == nil {
		t.Error("New left a dependency nil")
	}
	if r.swarms != swarms.Get() {
		t.Error("New did not default to the registered swarm registry")
	}
}

func equal(a, b []notify.EventType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Health is a separate axis from sync, and the loop has to fill it: a status
// left at unknown is what a UI would render as "no idea", which is worse than
// either answer.
func TestHealthIsRecordedAlongsideSync(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := New([]application.Spec{spec("edge", false)}, Options{
		Fetcher: &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder: &fakeBuilder{},
		Swarms: fakeRegistry{backend: stubBackend{states: map[string][]charts.ServiceState{
			"whoami": {{Name: "whoami", Running: 2, Desired: 2, NewestTaskAge: time.Hour}},
		}}},
		NewEngine: func(charts.Backend) Engine { return engine },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Health.State != application.HealthHealthy {
		t.Errorf("health = %q, want healthy", view.Status.Health.State)
	}
	if view.Status.Health.Services != (application.ServiceCounts{Healthy: 1, Total: 1}) {
		t.Errorf("counts = %+v, want 1/1", view.Status.Health.Services)
	}
	if len(view.Status.Releases) != 1 || len(view.Status.Releases[0].Services) != 1 {
		t.Fatalf("releases = %+v, want per-service detail", view.Status.Releases)
	}
	if view.Status.Releases[0].Services[0].Name != "whoami" {
		t.Errorf("service = %+v", view.Status.Releases[0].Services[0])
	}
}

// A release the plan would install has never been deployed, so the swarm is not
// asked about it at all — and it reads Missing rather than Degraded.
func TestUninstalledReleaseIsMissingAndNotQueried(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{
		{Name: "whoami", Ref: "repo/whoami", Action: charts.ActionInstall, ToVersion: "0.1.0"},
	}}
	engine := &fakeEngine{plans: []*charts.Plan{plan}}
	backend := &countingBackend{}
	r := New([]application.Spec{spec("edge", false)}, Options{
		Fetcher:   &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{backend: backend},
		NewEngine: func(charts.Backend) Engine { return engine },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Health.State != application.HealthMissing {
		t.Errorf("health = %q, want missing", view.Status.Health.State)
	}
	if backend.calls != 0 {
		t.Errorf("asked the swarm %d times about a release that was never deployed", backend.calls)
	}
}

type countingBackend struct {
	charts.Backend
	calls int
}

func (c *countingBackend) StackServices(string) []charts.ServiceState {
	c.calls++
	return nil
}
