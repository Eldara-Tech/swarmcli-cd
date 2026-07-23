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
	"github.com/Eldara-Tech/swarmcli-cd/regauth"
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

// recordingBackend records the scoping the reconciler applies before handing the
// backend to the engine, through the same optional-interface upgrades
// *backend.Backend implements. It is how a test proves the forbidden-secret
// check and the registry resolver are actually wired onto the backend a deploy
// runs through, not merely that each mechanism works in isolation.
type recordingBackend struct {
	charts.Backend
	forbidden map[string]struct{}
	auth      regauth.Resolver
}

func (b recordingBackend) StackServices(string) []charts.ServiceState { return nil }

func (b recordingBackend) WithForbiddenSecrets(names map[string]struct{}) charts.Backend {
	b.forbidden = names
	return b
}

func (b recordingBackend) WithRegistryAuth(auth regauth.Resolver) charts.Backend {
	b.auth = auth
	return b
}

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
	history   map[string][]charts.Release
	histErr   error
}

func (e *fakeEngine) History(_ context.Context, release string) ([]charts.Release, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.histErr != nil {
		return nil, e.histErr
	}
	revs, ok := e.history[release]
	if !ok {
		return nil, errors.New("release not found")
	}
	return revs, nil
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

// The reconciler must apply both the controller-wide forbidden-secret check and
// the application's registry resolver to the backend the engine deploys through.
// Every other test injects an engine that ignores the backend it is handed, so
// only this one would catch a regression that dropped the wiring — which would
// silently reopen the controller-secret exfiltration (#46) or the private-registry
// pull failure (#30) while the rest of the suite stayed green.
func TestReconcileScopesTheBackendItDeploysWith(t *testing.T) {
	var deployed charts.Backend
	forbidden := map[string]struct{}{"swarmcli-cd-token": {}}
	resolver := regauth.Resolver(func(string) (string, error) { return "auth", nil })

	r := New([]application.Spec{spec("edge", true)}, Options{
		Fetcher:               &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder:               &fakeBuilder{},
		Swarms:                fakeRegistry{backend: recordingBackend{}},
		NewEngine:             func(b charts.Backend) Engine { deployed = b; return &fakeEngine{plans: []*charts.Plan{synced()}} },
		RegistryAuth:          map[string]regauth.Resolver{"edge": resolver},
		ForbiddenSecretMounts: forbidden,
		Log:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:                   func() time.Time { return time.Unix(0, 0).UTC() },
	})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	rb, ok := deployed.(recordingBackend)
	if !ok {
		t.Fatalf("engine was handed %T, want a recordingBackend carrying the scoping", deployed)
	}
	if rb.forbidden == nil {
		t.Error("the forbidden-secret set was not applied to the backend the engine deploys with")
	}
	if rb.auth == nil {
		t.Error("the application's registry resolver was not applied to the backend the engine deploys with")
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

// A destination the swarm registry cannot resolve fails only its own
// application, surfaced on that application's status. It is no longer fatal to
// Run: a sibling with a good destination keeps reconciling and the controller
// stays up, rather than a single typo taking the whole loop down.
func TestBadDestinationFailsOnlyItsOwnApplication(t *testing.T) {
	broken := spec("broken", true)
	broken.Destination.Swarm = "nope"
	healthy := spec("healthy", true)
	healthy.Destination.Swarm = "local"

	fetcher := &fakeFetcher{revision: strings.Repeat("a", 40)}
	r := New([]application.Spec{broken, healthy}, Options{
		Fetcher:   fetcher,
		Builder:   &fakeBuilder{},
		Swarms:    swarmRouter{"local": stubBackend{}},
		NewEngine: func(charts.Backend) Engine { return &fakeEngine{plans: []*charts.Plan{synced()}} },
		Interval:  time.Millisecond,
		Log:       discardLog(),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// The bad-destination application surfaces its own error...
	waitFor(t, "the broken application to record its destination error", func() bool {
		v, ok := r.View("broken")
		return ok && strings.Contains(v.Status.Error, "destination")
	})
	// ...while Run has not returned and the sibling reconciles to synced.
	select {
	case err := <-done:
		t.Fatalf("Run returned %v; a bad destination must not stop the controller", err)
	default:
	}
	waitFor(t, "the healthy sibling to reconcile despite the failure", func() bool {
		v, ok := r.View("healthy")
		return ok && v.Status.Sync.State == application.SyncSynced
	})
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

// History reads the swarm rather than the status cache: the engine keeps one
// Docker Config per revision in Raft, which is what makes history survive a
// restart with no database here — and serving it from memory would make it
// vanish on exactly the restart it is most useful after.
func TestHistoryIsNewestFirstAndCoversEveryRelease(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{
		{Name: "whoami", Action: charts.ActionUnchanged},
		{Name: "redis", Action: charts.ActionUnchanged},
	}}
	engine := &fakeEngine{plans: []*charts.Plan{plan}, history: map[string][]charts.Release{
		// The engine returns them ascending, as written.
		"whoami": {
			{Name: "whoami", Revision: 1, Status: "superseded", Chart: charts.ReleaseChart{Name: "whoami", Version: "0.1.7"}},
			{Name: "whoami", Revision: 2, Status: "deployed", Chart: charts.ReleaseChart{Name: "whoami", Version: "0.1.8"}, Owner: "cd/edge:release/whoami"},
		},
		"redis": {{Name: "redis", Revision: 1, Status: "deployed"}},
	}}
	r := newTest(t, []application.Spec{spec("edge", false)}, engine, nil)
	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	got, err := r.History(context.Background(), "edge")
	if err != nil {
		t.Fatalf("History = %v, want nil", err)
	}
	if len(got.Releases) != 2 {
		t.Fatalf("got %d releases, want both", len(got.Releases))
	}
	revs := got.Releases[0].Revisions
	if len(revs) != 2 || revs[0].Revision != 2 || revs[1].Revision != 1 {
		t.Fatalf("revisions = %+v, want newest first", revs)
	}
	if revs[0].Version != "0.1.8" || revs[0].Status != "deployed" || revs[0].Owner != "cd/edge:release/whoami" {
		t.Errorf("revision = %+v, want the record's own detail", revs[0])
	}
}

// A release the plan would install has no history, and the engine says so with
// an error. That is the one case where an error is the expected answer.
func TestHistoryOfAnUninstalledReleaseIsEmpty(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{{Name: "whoami", Action: charts.ActionInstall}}}
	engine := &fakeEngine{plans: []*charts.Plan{plan}}
	r := newTest(t, []application.Spec{spec("edge", false)}, engine, nil)
	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	got, err := r.History(context.Background(), "edge")
	if err != nil {
		t.Fatalf("History = %v, want nil", err)
	}
	if len(got.Releases) != 1 || len(got.Releases[0].Revisions) != 0 {
		t.Errorf("history = %+v, want the release listed with nothing under it", got)
	}
}

// A deployed release whose history cannot be read is a real failure, not an
// empty table: reporting no revisions for a release that has them would say the
// swarm forgot them.
func TestHistoryFailureForADeployedReleaseIsAnError(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{{Name: "whoami", Action: charts.ActionUnchanged}}}
	engine := &fakeEngine{plans: []*charts.Plan{plan}, histErr: errors.New("swarm unreachable")}
	r := newTest(t, []application.Spec{spec("edge", false)}, engine, nil)
	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if _, err := r.History(context.Background(), "edge"); err == nil {
		t.Fatal("History = nil, want the failure surfaced")
	}
}

func TestHistoryBeforeAnyReconcile(t *testing.T) {
	r := newTest(t, []application.Spec{spec("edge", false)}, &fakeEngine{}, nil)
	if _, err := r.History(context.Background(), "edge"); !errors.Is(err, ErrNotPlanned) {
		t.Errorf("err = %v, want ErrNotPlanned", err)
	}
	if _, err := r.History(context.Background(), "absent"); err == nil {
		t.Error("History of an unknown application = nil, want an error")
	}
}

// An unattended reconciler has no operator to ask, so a chart declaring an
// engine floor this build does not meet is refused rather than deployed. The
// alternative is a failure minutes later, inside Render, naming whatever
// feature happened to be missing.
func TestIncompatibleChartIsRefusedBeforeApplying(t *testing.T) {
	rec := listen(t)
	engine := &fakeEngine{plans: []*charts.Plan{incompatible()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	err := r.Sync(context.Background(), "edge")
	if err == nil {
		t.Fatal("Sync = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), ">= 1.14.0") {
		t.Errorf("Sync = %v, want the error to name what the chart requires", err)
	}
	if engine.applyCount() != 0 {
		t.Errorf("applied %d times, want 0 — the plan was gated before converging any of it", engine.applyCount())
	}
	// The plan is still recorded: an operator needs to see the drift that is
	// not being applied, and why.
	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Errorf("state = %q, want out-of-sync", view.Status.Sync.State)
	}
	if got := rec.types(); len(got) != 1 || got[0] != notify.DriftDetected {
		t.Errorf("events = %v, want drift only — nothing was synced", got)
	}
}

// The gate is per release and skips the ones apply would not touch: a release
// already deployed is not made unrunnable by a constraint it never satisfied.
func TestUnchangedIncompatibleReleaseDoesNotBlockTheRest(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{
		{Name: "old", Action: charts.ActionUnchanged, Compat: charts.CompatFinding{
			Status: charts.CompatIncompatible, Chart: "old 1.0.0", Required: ">= 1.14.0", Engine: "1.13.0",
		}},
		{Name: "whoami", Ref: "repo/whoami", Action: charts.ActionUpgrade, ToVersion: "0.1.8",
			CurrentManifest: "replicas: 1\n", Manifest: "replicas: 3\n"},
	}}
	engine := &fakeEngine{plans: []*charts.Plan{plan, synced()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if engine.applyCount() != 1 {
		t.Errorf("applied %d times, want 1", engine.applyCount())
	}
}

func incompatible() *charts.Plan {
	plan := outOfSync()
	plan.Releases[0].Compat = charts.CompatFinding{
		Status:   charts.CompatIncompatible,
		Chart:    "whoami 0.1.8",
		Required: ">= 1.14.0",
		Engine:   "1.13.0",
	}
	return plan
}

// The image stamps the chart engine with the swarmcli version go.mod pins, and
// that pin is a pseudo-version whenever this module tracks a commit rather than
// a tag. SemVer constraints exclude prereleases, so ">= 1.13.0" would reject
// "v1.13.0-rc4.0.20260722094010-8b65cf951c7e" — and every chart declaring a
// floor would be refused by the gate above for a cosmetic reason.
//
// charts.coreVersion drops the prerelease, which is what makes the stamped
// pseudo-version usable. This is a contract test against that behaviour: if it
// ever changes, the controller starts refusing every chart that declares a
// version, and it would otherwise be found in production.
func TestPseudoVersionPinSatisfiesAChartsFloor(t *testing.T) {
	pinned := "v1.13.0-rc4.0.20260722094010-8b65cf951c7e"

	got := charts.CheckCompatAgainst(charts.Chartfile{
		Name:            "whoami",
		Version:         "0.1.8",
		SwarmcliVersion: ">= 1.13.0",
	}, pinned)

	if got.Status != charts.CompatOK {
		t.Errorf("CheckCompatAgainst(%q) = %v (%s), want CompatOK", pinned, got.Status, got.Reason)
	}
}

// ---------------------------------------------------------------- dynamic set

// An application added while the controller runs starts reconciling under the
// same supervision as the ones it started with.
func TestAddStartsReconcilingAtRuntime(t *testing.T) {
	fetcher := &fakeFetcher{revision: strings.Repeat("a", 40)}
	r := New(nil, Options{
		Fetcher:   fetcher,
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{},
		NewEngine: func(charts.Backend) Engine { return &fakeEngine{plans: []*charts.Plan{synced()}} },
		Interval:  time.Millisecond,
		Log:       discardLog(),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	if err := r.Add(spec("edge", true)); err != nil {
		t.Fatalf("Add = %v", err)
	}
	waitFor(t, "the added application to reconcile to synced", func() bool {
		v, ok := r.View("edge")
		return ok && v.Status.Sync.State == application.SyncSynced
	})
	if fetcher.count() == 0 {
		t.Error("the added application never fetched")
	}
}

// Remove stops the loop before it returns and drops what was observed, so a
// removed application no longer reconciles and is gone from the set.
func TestRemoveCancelsTheLoopAndDropsTheApp(t *testing.T) {
	fetcher := &fakeFetcher{revision: strings.Repeat("a", 40)}
	r := New([]application.Spec{spec("edge", true)}, Options{
		Fetcher:   fetcher,
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{},
		NewEngine: func(charts.Backend) Engine { return &fakeEngine{plans: []*charts.Plan{synced()}} },
		Interval:  time.Millisecond,
		Log:       discardLog(),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	waitFor(t, "the loop to reconcile at least twice", func() bool { return fetcher.count() >= 2 })

	if err := r.Remove("edge"); err != nil {
		t.Fatalf("Remove = %v", err)
	}

	// Remove drained the loop synchronously: no further reconcile may happen
	// once it has returned, which is what makes the absence of a leak testable.
	after := fetcher.count()
	time.Sleep(20 * time.Millisecond)
	if got := fetcher.count(); got != after {
		t.Errorf("the loop reconciled %d more times after Remove; it was not stopped", got-after)
	}
	if _, ok := r.View("edge"); ok {
		t.Error("a removed application is still in the set")
	}
	if got := r.Views(); len(got) != 0 {
		t.Errorf("Views lists %d applications after removing the only one", len(got))
	}
	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Error("Sync of a removed application = nil, want an error")
	}
}

// Replace swaps the spec a running loop reconciles against and keeps the
// recorded status: the next tick reads the new spec, and the loop is retuned
// rather than restarted (a remove+add would reset the status to unknown).
func TestReplaceIsObservedNextTickWithoutResettingStatus(t *testing.T) {
	fetcher := &specFetcher{}
	r := New([]application.Spec{spec("edge", false)}, Options{
		Fetcher:   fetcher,
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{},
		NewEngine: func(charts.Backend) Engine { return &fakeEngine{plans: []*charts.Plan{synced()}} },
		Interval:  time.Millisecond,
		Log:       discardLog(),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	waitFor(t, "the first reconcile against the original source", func() bool {
		return fetcher.lastURL() == "https://example.com/x.git"
	})
	waitFor(t, "a recorded status", func() bool {
		v, ok := r.View("edge")
		return ok && v.Status.Sync.State != application.SyncUnknown
	})

	next := spec("edge", false)
	next.Source.RepoURL = "https://example.com/replaced.git"
	if err := r.Replace(next); err != nil {
		t.Fatalf("Replace = %v", err)
	}

	// The swap kept the observed status rather than resetting it to unknown,
	// which is what a remove+add would have done.
	if v, _ := r.View("edge"); v.Status.Sync.State == application.SyncUnknown {
		t.Error("Replace reset the observed status; it should swap the spec in place")
	}

	waitFor(t, "the loop to reconcile against the replaced source", func() bool {
		return fetcher.lastURL() == "https://example.com/replaced.git"
	})
	if v, _ := r.View("edge"); v.Spec.Source.RepoURL != "https://example.com/replaced.git" {
		t.Error("View does not show the replaced spec")
	}
}

// The lifecycle operations are strict: the caller driving them from a git diff
// tells add from replace itself, so an add of an existing application or a
// replace/remove of an absent one is a mistake worth reporting. Order is kept.
func TestLifecycleErrorsOnWrongPrecondition(t *testing.T) {
	r := newTest(t, []application.Spec{spec("edge", true)}, &fakeEngine{plans: []*charts.Plan{synced()}}, nil)

	if err := r.Add(spec("edge", true)); err == nil {
		t.Error("Add of an existing application = nil, want an error")
	}
	if err := r.Replace(spec("ghost", true)); err == nil {
		t.Error("Replace of an absent application = nil, want an error")
	}
	if err := r.Remove("ghost"); err == nil {
		t.Error("Remove of an absent application = nil, want an error")
	}

	if err := r.Add(spec("edge2", true)); err != nil {
		t.Fatalf("Add = %v", err)
	}
	if got := r.Views(); len(got) != 2 || got[0].Spec.Name != "edge" || got[1].Spec.Name != "edge2" {
		t.Errorf("views = %+v, want [edge edge2] in declaration order", got)
	}

	if err := r.Remove("edge"); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	if got := r.Views(); len(got) != 1 || got[0].Spec.Name != "edge2" {
		t.Errorf("views = %+v, want [edge2] after removing edge", got)
	}
}

// The set mutates while it is read and synced from other goroutines. This is
// the race the dynamic reconciler exists to make safe; it earns its keep under
// the race detector (CI runs go test -race).
func TestConcurrentMutationAndReadsAreRaceFree(t *testing.T) {
	r := New([]application.Spec{spec("seed", true)}, Options{
		Fetcher:   &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{},
		NewEngine: func(charts.Backend) Engine { return &fakeEngine{plans: []*charts.Plan{synced()}} },
		Interval:  time.Millisecond,
		Log:       discardLog(),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = r.Run(ctx); close(runDone) }()

	// Each goroutine owns a distinct name, so its own add/replace/remove
	// sequence is well-formed while it races the others' reads and the seed
	// application's loop.
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for range 40 {
				_ = r.Add(spec(name, true))
				_ = r.Sync(context.Background(), name)
				_ = r.Views()
				_, _ = r.View(name)
				_ = r.Replace(spec(name, false))
				_, _ = r.Diffs(name)
				_ = r.Remove(name)
			}
		}(name)
	}
	wg.Wait()

	cancel()
	<-runDone
}

// ---------------------------------------------------------------- dynamic-set fakes

// swarmRouter resolves a backend per swarm name and fails any it does not know,
// so a test can give one application a good destination and another a bad one.
type swarmRouter map[string]charts.Backend

func (s swarmRouter) Backend(_ context.Context, name string) (charts.Backend, error) {
	if b, ok := s[name]; ok {
		return b, nil
	}
	return nil, errors.New("unknown swarm")
}

// specFetcher records the source URL of the most recent fetch, so a test can
// observe which spec a running loop is reconciling against.
type specFetcher struct {
	mu   sync.Mutex
	last string
}

func (f *specFetcher) Fetch(_ context.Context, _ string, src application.Source) (git.Checkout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = src.RepoURL
	return git.Checkout{Revision: strings.Repeat("a", 40)}, nil
}

func (f *specFetcher) lastURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// waitFor polls cond until it holds or two seconds pass, so a test can wait on a
// running loop without a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
