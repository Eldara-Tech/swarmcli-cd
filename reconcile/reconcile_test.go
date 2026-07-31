// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/compose"
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

func (f fakeRegistry) Backend(context.Context, swarms.Target) (charts.Backend, error) {
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
	// removed records the stacks prune tore down. Prune removes the deployed
	// resources through the backend and only then lets the engine delete the
	// release records, so a test that asserts on the engine alone would not see
	// half of what happened.
	removed *[]string
}

func (s stubBackend) StackServices(_ context.Context, name string) []charts.ServiceState {
	return s.states[name]
}

func (s stubBackend) RemoveStack(_ context.Context, name string) error {
	if s.removed != nil {
		*s.removed = append(*s.removed, name)
	}
	return nil
}

// No volumes in these fakes: the reconciler's prune passes the application's
// pruneVolumes through, and what that does to real volumes is prune's own test.
func (s stubBackend) StackVolumes(context.Context, string) ([]string, error) { return nil, nil }
func (s stubBackend) RemoveVolume(context.Context, string) error             { return nil }

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

func (b recordingBackend) StackServices(context.Context, string) []charts.ServiceState { return nil }

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

	// uninstalled records what prune removed, in order, as "<release>" or
	// "<release>+volumes" so one field carries both the call and its argument.
	uninstalled []string
	// uninstAtPlan is how many PlanApply calls had happened when each uninstall
	// arrived. Prune runs between the apply and the confirming re-plan, so a 1
	// here is what proves the ordering rather than merely that both happened.
	uninstAtPlan []int
	// uninstAtApply is the same for Apply, which is what distinguishes the two
	// prune orderings: 0 means nothing had been installed yet.
	uninstAtApply []int
	uninstErr     map[string]error
	leftNets      map[string][]string
}

func (e *fakeEngine) Uninstall(_ context.Context, release string, purgeVolumes bool) (*charts.UninstallResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	call := release
	if purgeVolumes {
		call += "+volumes"
	}
	e.uninstalled = append(e.uninstalled, call)
	e.uninstAtPlan = append(e.uninstAtPlan, e.planCalls)
	e.uninstAtApply = append(e.uninstAtApply, e.applied)
	if err := e.uninstErr[release]; err != nil {
		return nil, err
	}
	return &charts.UninstallResult{OrphanedNetworks: e.leftNets[release]}, nil
}

func (e *fakeEngine) pruned() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.uninstalled)
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

func (e *fakeEngine) planCallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.planCalls
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
	return newTestWith(t, apps, engine, fetcher, fakeRegistry{})
}

func newTestWith(t *testing.T, apps []application.Spec, engine Engine, fetcher Fetcher, swarms swarms.Registry) *Reconciler {
	t.Helper()
	if fetcher == nil {
		fetcher = &fakeFetcher{revision: strings.Repeat("a", 40)}
	}
	return New(apps, Options{
		Fetcher:   fetcher,
		Builder:   &fakeBuilder{},
		Swarms:    swarms,
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
	planned.Owner = "cd/default/edge" // what PlanApply returns for the owner it was given
	engine := &fakeEngine{plans: []*charts.Plan{planned, synced()}}
	historyMax := 20
	r := newTest(t, []application.Spec{{
		Name:           "edge",
		Source:         application.Source{RepoURL: "https://example.com/x.git", Revision: "main"},
		SyncPolicy:     application.SyncPolicy{Automated: true, Wait: true, Timeout: application.Duration(90 * time.Second), HistoryMax: &historyMax},
		DriftDetection: application.DriftManifest,
	}}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if got := engine.planOpts[0].Owner; got != "cd/default/edge" {
		t.Errorf("plan owner = %q, want cd/default/edge", got)
	}

	got := engine.opts[0]
	// From the plan, not recomputed: stamping an id the plan did not classify
	// against would install under one owner and hunt for orphans under another.
	if got.Owner != "cd/default/edge" {
		t.Errorf("owner = %q, want cd/default/edge", got.Owner)
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

func (c *countingBackend) StackServices(context.Context, string) []charts.ServiceState {
	c.calls++
	return nil
}

// unreadableBackend answers the honest read with a failure, the way
// *backend.Backend does when the daemon is unreachable. It still satisfies the
// plain StackServices, because charts.Backend requires it and the point of the
// upgrade is that the two answer differently.
type unreadableBackend struct{ stubBackend }

func (unreadableBackend) ReadStackServices(context.Context, string) ([]charts.ServiceState, error) {
	return nil, errors.New("daemon unreachable")
}

// A daemon that could not be asked is not a stack with nothing in it. Reading
// the second out of the first turned one slow daemon into every release of the
// application reporting Missing — the loudest state the rollup has, and the one
// alerting is watching for — while the stack ran perfectly (#107).
func TestAnUnreadableSwarmMakesHealthUnknownRatherThanMissing(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{spec("edge", false)}, engine, nil,
		fakeRegistry{backend: unreadableBackend{}})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Health.State != application.HealthUnknown {
		t.Errorf("health = %q, want unknown", view.Status.Health.State)
	}
	if len(view.Status.Releases) != 1 || view.Status.Releases[0].Health.State != application.HealthUnknown {
		t.Errorf("releases = %+v, want the release unknown too", view.Status.Releases)
	}
}

// The other half, and the reason the distinction had to be made at the backend
// rather than by treating every empty answer as suspect: a stack that really has
// gone still reads Missing.
func TestAnEmptyStackIsStillMissing(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{spec("edge", false)}, engine, nil,
		fakeRegistry{backend: stubBackend{}})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Health.State != application.HealthMissing {
		t.Errorf("health = %q, want missing", view.Status.Health.State)
	}
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

func (s swarmRouter) Backend(_ context.Context, t swarms.Target) (charts.Backend, error) {
	if b, ok := s[t.Swarm]; ok {
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

// An application that joins the set at runtime brings a credential the map
// built at startup has never seen, and one that stops declaring registryAuth
// must stop having it sent. Both go through the setter, and what proves it is
// the backend the engine is handed.
func TestSetRegistryAuthTakesEffectOnTheNextReconcile(t *testing.T) {
	var deployed charts.Backend
	resolver := regauth.Resolver(func(string) (string, error) { return "auth", nil })

	r := New(nil, Options{
		Fetcher: &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder: &fakeBuilder{},
		Swarms:  fakeRegistry{backend: recordingBackend{}},
		NewEngine: func(b charts.Backend) Engine {
			deployed = b
			return &fakeEngine{plans: []*charts.Plan{synced(), synced()}}
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Unix(0, 0).UTC() },
	})

	// The joining application: its credential is installed before it is added,
	// so its very first reconcile already pulls with it.
	r.SetRegistryAuth("edge", resolver)
	if err := r.Add(spec("edge", true)); err != nil {
		t.Fatalf("Add = %v, want nil", err)
	}
	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if rb, ok := deployed.(recordingBackend); !ok || rb.auth == nil {
		t.Error("the credential installed for a runtime-added application never reached the backend")
	}

	// It stops declaring one. The credential must go with it, or the controller
	// keeps sending a secret the application no longer asks for.
	r.SetRegistryAuth("edge", nil)
	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if rb, ok := deployed.(recordingBackend); !ok || rb.auth != nil {
		t.Error("a cleared credential was still applied to the backend")
	}
}

// A name that returns to the set later is a new application, and must not
// inherit the credential of the one that left.
func TestRemoveDropsTheCredential(t *testing.T) {
	r := New([]application.Spec{spec("edge", true)}, Options{
		Fetcher: &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder: &fakeBuilder{},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r.SetRegistryAuth("edge", func(string) (string, error) { return "auth", nil })

	if err := r.Remove("edge"); err != nil {
		t.Fatalf("Remove = %v, want nil", err)
	}
	if r.registryAuth("edge") != nil {
		t.Error("the departed application's credential survived it")
	}
}

// ---------------------------------------------------------------- prune

// orphaning returns an out-of-sync plan that also reports releases this
// application used to declare — what charts classifies as Orphaned once the
// release file stops naming them.
func orphaning(orphaned ...string) *charts.Plan {
	p := outOfSync()
	p.Orphaned = orphaned
	return p
}

func pruningSpec(name string, volumes bool) application.Spec {
	s := spec(name, true)
	s.SyncPolicy.Prune = true
	s.SyncPolicy.PruneVolumes = volumes
	return s
}

func TestPruneRemovesReleasesTheApplicationNoLongerDeclares(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{orphaning("old", "older"), synced()}}
	r := newTest(t, []application.Spec{pruningSpec("edge", false)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if want := []string{"old", "older"}; !slices.Equal(engine.pruned(), want) {
		t.Errorf("uninstalled %v, want %v", engine.pruned(), want)
	}
}

// The D-e default. An orphan is reported and left running unless the
// application's own sync policy asks for it to go.
func TestOrphansSurviveWhenPruneIsOff(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{orphaning("old"), synced()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := engine.pruned(); len(got) != 0 {
		t.Errorf("uninstalled %v with prune off, want nothing", got)
	}
}

// The deployed resources come down through the backend and only then does the
// engine delete the release records — never the other way round, because those
// records carry the owner stamp a later sweep would need if this failed
// halfway. Uninstall is therefore always asked to purge no volumes: prune has
// already dealt with them.
func TestPruneRemovesTheStackBeforeTheRecords(t *testing.T) {
	var removed []string
	engine := &fakeEngine{plans: []*charts.Plan{orphaning("old"), synced()}}
	r := newTestWith(t, []application.Spec{pruningSpec("edge", true)}, engine,
		nil, fakeRegistry{backend: stubBackend{removed: &removed}})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if want := []string{"old"}; !slices.Equal(removed, want) {
		t.Errorf("removed stacks %v, want %v", removed, want)
	}
	if want := []string{"old"}; !slices.Equal(engine.pruned(), want) {
		t.Errorf("uninstalled %v, want %v — volumes are prune's own business now", engine.pruned(), want)
	}
}

// The deploy landed; only the tidy-up did not. Reporting the sync as failed
// would say the wrong thing about what is running, so the result stands and the
// error is the reconcile's — the same split the re-plan failure already makes.
func TestPruneFailureDoesNotFailTheSync(t *testing.T) {
	engine := &fakeEngine{
		plans:     []*charts.Plan{orphaning("old"), synced()},
		uninstErr: map[string]error{"old": errors.New("network still attached")},
	}
	r := newTest(t, []application.Spec{pruningSpec("edge", false)}, engine, nil)

	err := r.Sync(context.Background(), "edge")
	if err == nil || !strings.Contains(err.Error(), "network still attached") {
		t.Fatalf("Sync = %v, want the prune failure surfaced", err)
	}

	view, _ := r.View("edge")
	if view.Status.Sync.LastSync == nil || !view.Status.Sync.LastSync.Succeeded {
		t.Errorf("lastSync = %+v, want the apply still recorded as successful", view.Status.Sync.LastSync)
	}
}

// A clean shutdown is not a failed sync. Two routine triggers: every rolling
// restart of the controller cancels each application's context mid-apply — with
// syncPolicy.wait the apply blocks for as long as the rollout takes, so a
// `docker service update` of the controller is near-certain to catch one — and
// Remove cancels the application it is retiring. Both used to dispatch
// sync-failed carrying "context canceled" through every notifier the deployment
// has, and to record a failed sync and an application-level error with it (#107).
//
// The cancellation arrives wrapped in whatever the call was doing at the time,
// which is why the test wraps it too.
func TestACancelledSyncIsNotDispatchedAsAFailure(t *testing.T) {
	rec := listen(t)
	engine := &fakeEngine{
		plans:    []*charts.Plan{outOfSync(), synced()},
		applyErr: fmt.Errorf("deploying release %q: %w", "whoami", context.Canceled),
	}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Sync(ctx, "edge"); err == nil {
		t.Fatal("Sync = nil, want the cancellation returned to the loop")
	}

	if got := rec.types(); slices.Contains(got, notify.SyncFailed) {
		t.Errorf("events = %v, want no sync-failed for a clean shutdown", got)
	}
	view, _ := r.View("edge")
	if last := view.Status.Sync.LastSync; last != nil {
		t.Errorf("lastSync = %+v, want none — the sync reached no outcome", last)
	}
	if view.Status.Error != "" {
		t.Errorf("status error = %q, want none: a shutdown is not a broken application", view.Status.Error)
	}
}

// The prune paths build their errors out of ctx.Err() too — prune.purgeVolumes
// literally returns it — so a shutdown during a teardown raised prune-failed for
// something that had not failed.
//
// Both orderings, because they report a sweep failure differently and a
// cancellation is not a sweep failure under either: the pruneFirst one also
// dispatches sync-failed and writes a result, and must not do that for a
// controller that is merely stopping.
func TestACancelledPruneIsNotDispatchedAsAFailure(t *testing.T) {
	for _, pruneFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("pruneFirst=%v", pruneFirst), func(t *testing.T) {
			rec := listen(t)
			engine := &fakeEngine{
				plans:     []*charts.Plan{orphaning("old"), synced()},
				uninstErr: map[string]error{"old": fmt.Errorf("removing volume %q: %w", "old_data", context.Canceled)},
			}
			spec := pruningSpec("edge", false)
			spec.SyncPolicy.PruneFirst = pruneFirst
			r := newTest(t, []application.Spec{spec}, engine, nil)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := r.Sync(ctx, "edge"); err == nil {
				t.Fatal("Sync = nil, want the cancellation returned to the loop")
			}

			for _, unwanted := range []notify.EventType{notify.PruneFailed, notify.SyncFailed} {
				if slices.Contains(rec.types(), unwanted) {
					t.Errorf("events = %v, want no %s for a clean shutdown", rec.types(), unwanted)
				}
			}
			// Not "no result": under the default ordering the apply has already
			// landed and recorded a successful one by the time the sweep is
			// cancelled, which is right. What must never appear is a *failed*
			// result, under either ordering.
			view, _ := r.View("edge")
			if last := view.Status.Sync.LastSync; last != nil && !last.Succeeded {
				t.Errorf("lastSync = %+v, want no failure written down for a clean shutdown", last)
			}
		})
	}
}

// Under pruneFirst the sweep runs before the apply and its failure stops it, so
// nothing was deployed and the sync delivered nothing at all. It used to return
// bare: the event stream carried a sync-started that never ended, and the status
// went on reporting the previous sync — so the one ordering that guarantees
// nothing is running was also the one that showed the last time something was
// (#107).
//
// The other ordering is the opposite case and keeps the opposite answer, which
// TestPruneFailureDoesNotFailTheSync pins. If that one starts failing, this
// change has leaked across the branch.
func TestPruneFirstFailureIsReportedAsAFailedSync(t *testing.T) {
	rec := listen(t)
	engine := &fakeEngine{
		plans:     []*charts.Plan{orphaning("old"), synced()},
		uninstErr: map[string]error{"old": errors.New("network still attached")},
	}
	spec := pruningSpec("edge", false)
	spec.SyncPolicy.PruneFirst = true
	r := newTest(t, []application.Spec{spec}, engine, nil)

	// A sync that worked, so that "the status still shows the last one" is a
	// thing this test can actually catch rather than infer from a nil.
	previous := &application.SyncResult{Revision: strings.Repeat("b", 40), Succeeded: true}
	e, err := r.entry("edge")
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	r.recordResult(e, previous)

	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Fatal("Sync = nil, want the sweep failure")
	}

	// The premise: nothing was deployed. Without this the assertions below would
	// pass just as well on a sync that had in fact landed.
	if engine.applyCount() != 0 {
		t.Fatalf("applied %d times, want 0 — the sweep stops the apply, which is what makes this a failed sync", engine.applyCount())
	}
	view, _ := r.View("edge")
	last := view.Status.Sync.LastSync
	switch {
	case last == nil || last == previous:
		t.Errorf("lastSync = %+v, want this sync's outcome rather than the one before it", last)
	case last.Succeeded:
		t.Errorf("lastSync = %+v, want a failure — none of what the repository declares was deployed", last)
	case !strings.Contains(last.Error, "network still attached"):
		t.Errorf("lastSync.Error = %q, want the sweep failure named", last.Error)
	}
	want := []notify.EventType{notify.DriftDetected, notify.SyncStarted, notify.PruneFailed, notify.SyncFailed}
	if got := rec.types(); !equal(got, want) {
		t.Errorf("events = %v, want %v — a started sync is terminated", got, want)
	}
}

// Prune runs after the apply and before the confirming re-plan. Were it after,
// the re-plan would report the releases prune is about to delete straight back
// as orphans.
func TestPruneRunsBetweenTheApplyAndTheReplan(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{orphaning("old"), synced()}}
	r := newTest(t, []application.Spec{pruningSpec("edge", false)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if engine.applyCount() != 1 {
		t.Fatalf("applied %d times, want 1", engine.applyCount())
	}
	if want := []int{1}; !slices.Equal(engine.uninstAtPlan, want) {
		t.Errorf("uninstall happened after %v PlanApply calls, want %v — prune must precede the re-plan", engine.uninstAtPlan, want)
	}
	if engine.planCallCount() != 2 {
		t.Errorf("PlanApply called %d times, want 2 (plan then confirm)", engine.planCallCount())
	}
}

// Prune is gated on the apply having happened at all: a synced application has
// nothing to deploy, so it never reaches the prune step even with orphans
// declared and prune enabled.
func TestSyncedApplicationDoesNotPrune(t *testing.T) {
	plan := synced()
	plan.Orphaned = []string{"old"}
	engine := &fakeEngine{plans: []*charts.Plan{plan}}
	r := newTest(t, []application.Spec{pruningSpec("edge", false)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := engine.pruned(); len(got) != 0 {
		t.Errorf("uninstalled %v, want nothing — there was nothing to apply", got)
	}
}

// PruneFirst is for a workload where two instances running at once is worse
// than none — a validator that would double-sign, a runner that would process a
// queue twice. The default order installs the replacement and only then deletes
// what it replaced, which briefly runs both; this inverts that.
func TestPruneFirstDeletesBeforeItInstalls(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{orphaning("old"), synced()}}
	spec := pruningSpec("edge", false)
	spec.SyncPolicy.PruneFirst = true
	r := newTest(t, []application.Spec{spec}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if want := []string{"old"}; !slices.Equal(engine.pruned(), want) {
		t.Fatalf("uninstalled %v, want %v", engine.pruned(), want)
	}
	// One PlanApply had happened and no Apply had: the deletion landed before
	// anything was installed, which is the guarantee.
	if want := []int{1}; !slices.Equal(engine.uninstAtPlan, want) {
		t.Errorf("uninstall after %v PlanApply calls, want %v", engine.uninstAtPlan, want)
	}
	if want := []int{0}; !slices.Equal(engine.uninstAtApply, want) {
		t.Errorf("uninstall after %v Apply calls, want %v — nothing may be installed first", engine.uninstAtApply, want)
	}
}

// The default is the other way round, and this is what pins it: the apply has
// already happened when prune runs, so a failed apply leaves the old release
// running rather than deleted.
func TestTheDefaultOrderInstallsBeforeItDeletes(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{orphaning("old"), synced()}}
	r := newTest(t, []application.Spec{pruningSpec("edge", false)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if want := []int{1}; !slices.Equal(engine.uninstAtApply, want) {
		t.Errorf("uninstall after %v Apply calls, want %v", engine.uninstAtApply, want)
	}
}

// With PruneFirst the deletion gates the install: if it fails, installing
// anyway would create exactly the overlap the option exists to prevent.
func TestPruneFirstFailureStopsTheApply(t *testing.T) {
	engine := &fakeEngine{
		plans:     []*charts.Plan{orphaning("old"), synced()},
		uninstErr: map[string]error{"old": errors.New("network still attached")},
	}
	spec := pruningSpec("edge", false)
	spec.SyncPolicy.PruneFirst = true
	r := newTest(t, []application.Spec{spec}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Fatal("Sync = nil, want the prune failure to stop the sync")
	}
	if engine.applyCount() != 0 {
		t.Errorf("applied %d times, want 0 — the old release is still deployed", engine.applyCount())
	}
}

// ------------------------------------------------- driftDetection: live

// driftBackend is a stubBackend that also answers the two live-drift reads and
// records what was redeployed to correct one.
//
// desired and live are keyed by release. A release absent from both compares
// equal, which is the ordinary case and keeps every existing test unaffected.
type driftBackend struct {
	stubBackend
	desired map[string]*compose.Stack
	live    map[string]map[string]swarm.Service
	readErr error
	// byManifest answers for one specific manifest rather than for whatever a
	// release currently renders to. The service sweep converts stored revisions,
	// so a fake that answered the same thing for every manifest could not tell
	// "the chart used to declare this" from "the chart declares it now".
	byManifest map[string]*compose.Stack

	mu sync.Mutex
	// deployed records each converge as "<release>|<manifest>", so one field
	// carries both that a correction happened and that it wrote the right thing.
	deployed  []string
	deployErr error
	// manifestReads records every manifest converted, in order, which is how a
	// test sees that the history walk stopped early.
	manifestReads []string
	// unresolvableErr fails the *resolving* conversion of one manifest, keyed by
	// it — a stored revision that mounted a config a previous sweep has since
	// deleted, which is the real daemon's "config not found". DeclaredResources
	// answers it anyway, because a name needs nothing to exist.
	unresolvableErr map[string]error
	// removedServices records the ids the sweep deleted.
	removedServices []string
	removeErr       error
	// appliedAt reports how many applies had happened, sampled at each removal.
	// It is what distinguishes the two prune orderings.
	appliedAt      func() int
	removedAtApply []int

	// The swarm's networks by id, as the networkNamer seam returns them, and the
	// failure of that listing. Nil names with no error is a swarm with no
	// networks, which is not the same as a backend that could not look.
	networkNames map[string]string
	namesErr     error

	// The other three kinds, by release and then scoped name to id — the shape
	// the resourceLister seam returns.
	liveNetworks map[string]map[string]string
	liveConfigs  map[string]map[string]string
	liveSecrets  map[string]map[string]string
	// listErr fails the resource listers only, so a test can have a backend that
	// reads services and not the rest.
	listErr error
	// removedResources records what the resource sweep deleted, as "<kind>:<id>",
	// so one field carries both what went and that the right call was made.
	removedResources []string
	// attemptedResources records every removal that was *tried*, refusals
	// included, which is the only thing that shows a sweep has stopped trying.
	attemptedResources []string
	// resourceRemoveErr fails a removal by "<kind>:<id>", modelling the daemon
	// refusing to remove something still in use.
	resourceRemoveErr map[string]error
	// resourcesAtApply is removedAtApply for the other three kinds. They are
	// sampled separately because they are ordered separately: pruneFirst moves
	// the services and deliberately leaves these where they are.
	resourcesAtApply []int
}

func (b *driftBackend) DesiredServices(_ context.Context, manifest, stack string) (*compose.Stack, error) {
	if b.readErr != nil {
		return nil, b.readErr
	}
	if err := b.unresolvableErr[manifest]; err != nil {
		return nil, err
	}
	return b.answer(manifest, stack), nil
}

// DeclaredResources answers the same manifests as DesiredServices but is not
// subject to unresolvableErr, which is the asymmetry the real pair has: resolving
// a reference needs the resource to exist, and reading a name does not.
//
// readErr still applies. That one models a backend that cannot read at all, and a
// fake that answered anyway would make the no-backend degradation untestable.
func (b *driftBackend) DeclaredResources(_ context.Context, manifest, stack string) (*compose.Stack, error) {
	if b.readErr != nil {
		return nil, b.readErr
	}
	return b.answer(manifest, stack), nil
}

// answer is what both conversions have in common: record the read, then hand back
// this manifest's stack if the test named one and the release's otherwise.
func (b *driftBackend) answer(manifest, stack string) *compose.Stack {
	b.mu.Lock()
	b.manifestReads = append(b.manifestReads, manifest)
	b.mu.Unlock()
	if s, ok := b.byManifest[manifest]; ok {
		return s
	}
	return b.desired[stack]
}

// RemoveService deletes from the fake swarm as well as recording, so that the
// reconcile after a sweep sees what the sweep did rather than reporting the same
// orphan for ever.
func (b *driftBackend) RemoveService(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.removeErr != nil {
		return b.removeErr
	}
	b.removedServices = append(b.removedServices, id)
	if b.appliedAt != nil {
		b.removedAtApply = append(b.removedAtApply, b.appliedAt())
	}
	for _, stack := range b.live {
		for name, svc := range stack {
			if svc.ID == id {
				delete(stack, name)
			}
		}
	}
	return nil
}

func (b *driftBackend) pruned() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.removedServices)
}

func (b *driftBackend) converted() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.manifestReads)
}

func (b *driftBackend) LiveServices(_ context.Context, stack string) (map[string]swarm.Service, error) {
	if b.readErr != nil {
		return nil, b.readErr
	}
	return b.live[stack], nil
}

func (b *driftBackend) LiveNetworkNames(context.Context) (map[string]string, error) {
	if b.namesErr != nil {
		return nil, b.namesErr
	}
	return b.networkNames, nil
}

func (b *driftBackend) LiveNetworks(_ context.Context, stack string) (map[string]string, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.liveNetworks[stack], nil
}

func (b *driftBackend) LiveConfigs(_ context.Context, stack string) (map[string]string, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.liveConfigs[stack], nil
}

func (b *driftBackend) LiveSecrets(_ context.Context, stack string) (map[string]string, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.liveSecrets[stack], nil
}

// The three resource removals delete from the fake swarm as RemoveService does,
// so a reconcile after a sweep sees what the sweep did.
func (b *driftBackend) RemoveNetwork(_ context.Context, id string) error {
	return b.removeResource("network", id, b.liveNetworks)
}

func (b *driftBackend) RemoveConfig(_ context.Context, id string) error {
	return b.removeResource("config", id, b.liveConfigs)
}

func (b *driftBackend) RemoveSecret(_ context.Context, id string) error {
	return b.removeResource("secret", id, b.liveSecrets)
}

func (b *driftBackend) removeResource(kind, id string, from map[string]map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := kind + ":" + id
	b.attemptedResources = append(b.attemptedResources, key)
	if err := b.resourceRemoveErr[key]; err != nil {
		return err
	}
	b.removedResources = append(b.removedResources, key)
	if b.appliedAt != nil {
		b.resourcesAtApply = append(b.resourcesAtApply, b.appliedAt())
	}
	for _, stack := range from {
		for name, have := range stack {
			if have == id {
				delete(stack, name)
			}
		}
	}
	return nil
}

func (b *driftBackend) prunedResources() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.removedResources)
}

func (b *driftBackend) attempted() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.attemptedResources)
}

func (b *driftBackend) DeployStack(_ context.Context, name, manifest, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deployed = append(b.deployed, name+"|"+manifest)
	return b.deployErr
}

func (b *driftBackend) converged() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.deployed)
}

func serviceSpec(name string, replicas uint64) swarm.ServiceSpec {
	return swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: name},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "busybox:1.36"}},
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
	}
}

func uint64p(v uint64) *uint64 { return &v }

// driftedBackend is a backend on which the release "whoami" is running three
// replicas where the manifest asks for one.
func driftedBackend() *driftBackend {
	want := serviceSpec("whoami_app", 1)
	got := serviceSpec("whoami_app", 3)
	return &driftBackend{
		desired: map[string]*compose.Stack{
			"whoami": {Services: []compose.Service{{Name: "app", Spec: want}}},
		},
		live: map[string]map[string]swarm.Service{
			"whoami": {"whoami_app": {ID: "id", Spec: got}},
		},
	}
}

// matchingBackend is the same stack with nothing changed behind the controller.
func matchingBackend() *driftBackend {
	spec := serviceSpec("whoami_app", 1)
	return &driftBackend{
		desired: map[string]*compose.Stack{
			"whoami": {Services: []compose.Service{{Name: "app", Spec: spec}}},
		},
		live: map[string]map[string]swarm.Service{
			"whoami": {"whoami_app": {ID: "id", Spec: spec}},
		},
	}
}

func liveSpec(name string, automated bool) application.Spec {
	s := spec(name, automated)
	s.DriftDetection = application.DriftLive
	return s
}

// attachedTo is the "whoami" release with its service attached to one network in
// the manifest, and to whatever the daemon reports live.
//
// The two sides are deliberately written in different currencies, because that is
// the whole problem the seam solves: the manifest names a network and the daemon
// stores an id, so a comparison that skipped the naming step would report a
// detachment on every service ever deployed.
func attachedTo(liveIDs ...string) *driftBackend {
	want := serviceSpec("whoami_app", 1)
	want.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "whoami_internal"}}

	got := serviceSpec("whoami_app", 1)
	for _, id := range liveIDs {
		got.TaskTemplate.Networks = append(got.TaskTemplate.Networks,
			swarm.NetworkAttachmentConfig{Target: id})
	}

	return &driftBackend{
		desired: map[string]*compose.Stack{
			"whoami": {Services: []compose.Service{{Name: "app", Spec: want}}},
		},
		live: map[string]map[string]swarm.Service{
			"whoami": {"whoami_app": {ID: "id", Spec: got}},
		},
		networkNames: map[string]string{"net-internal": "whoami_internal"},
	}
}

// The attachment comparison end to end: the id the daemon stored is named back
// to what the manifest declared, and the two agree.
func TestAnAttachmentIsNamedBeforeItIsCompared(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil,
		fakeRegistry{backend: attachedTo("net-internal")})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	view, _ := r.View("edge")
	if view.Status.Drift == nil || view.Status.Drift.State != application.DriftStateNone {
		t.Errorf("drift = %+v, want none — the id names the network the manifest declared", view.Status.Drift)
	}
}

func TestADetachedNetworkIsReported(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", false)}, engine, nil,
		fakeRegistry{backend: attachedTo()})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	rel := releaseDriftOf(t, r, "edge")
	if len(rel.Services) != 1 {
		t.Fatalf("services = %+v, want the one detached service", rel.Services)
	}
	got := rel.Services[0].Fields
	want := []application.FieldDrift{{Field: "networks", Desired: "whoami_internal", Live: ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fields = %+v, want %+v", got, want)
	}
}

// A listing this controller could not make proves nothing about the attachments,
// so they are not compared — and that must cost the attachments only. Losing the
// release's other fields, or calling it unknown, would turn one unreadable list
// into a blind spot over everything.
func TestANamingFailureCostsTheAttachmentsAndNothingElse(t *testing.T) {
	backend := attachedTo()
	backend.namesErr = errors.New("swarm unreachable")
	// A difference the comparison can still see without naming anything.
	live := backend.live["whoami"]["whoami_app"]
	live.Spec.Mode.Replicated.Replicas = uint64p(4)
	backend.live["whoami"]["whoami_app"] = live

	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", false)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	rel := releaseDriftOf(t, r, "edge")
	if rel.State != application.DriftStateDetected {
		t.Fatalf("state = %q, want detected: the replica difference is still legible", rel.State)
	}
	want := []application.FieldDrift{{Field: "replicas", Desired: "1", Live: "4"}}
	if got := rel.Services[0].Fields; !reflect.DeepEqual(got, want) {
		t.Errorf("fields = %+v, want %+v — the detachment is not reported on a listing that failed", got, want)
	}
}

// A backend that does not implement the seam at all, which is the Phase 3 remote
// one: the same degradation, decided one step earlier.
func TestABackendThatCannotNameNetworksComparesEverythingElse(t *testing.T) {
	ldb := oldSeamBackend{}
	if _, ok := any(ldb).(networkNamer); ok {
		t.Fatal("oldSeamBackend implements networkNamer; it is meant to be the backend that does not")
	}

	r := newTest(t, []application.Spec{liveSpec("edge", false)}, &fakeEngine{plans: []*charts.Plan{synced()}}, nil)
	if got := r.networkNames(context.Background(), liveSpec("edge", false), ldb, true); got != nil {
		t.Errorf("names = %v, want nil so that attachments are simply not compared", got)
	}
}

// releaseDriftOf returns the drift detail of an application's only release.
func releaseDriftOf(t *testing.T, r *Reconciler, app string) *application.ReleaseDrift {
	t.Helper()
	view, ok := r.View(app)
	if !ok {
		t.Fatalf("no view for %q", app)
	}
	if len(view.Status.Releases) != 1 {
		t.Fatalf("got %d releases, want 1", len(view.Status.Releases))
	}
	if view.Status.Releases[0].Drift == nil {
		t.Fatal("no drift detail on the release")
	}
	return view.Status.Releases[0].Drift
}

// rolledBackBackend is driftedBackend after Swarm reverted the spec this
// controller wrote: the release is running one replica where the manifest asks
// for three, and the service says why.
//
// The manifest asking for what the *rollback undid* is the whole point — that is
// what makes the difference look identical to somebody having scaled the service
// by hand, which is the write this cannot otherwise be told apart from.
func rolledBackBackend() *driftBackend {
	want := serviceSpec("whoami_app", 3)
	running := serviceSpec("whoami_app", 1)
	return &driftBackend{
		desired: map[string]*compose.Stack{
			"whoami": {Services: []compose.Service{{Name: "app", Spec: want}}},
		},
		live: map[string]map[string]swarm.Service{
			"whoami": {"whoami_app": {
				ID:   "id",
				Spec: running,
				UpdateStatus: &swarm.UpdateStatus{
					State:   swarm.UpdateStateRollbackCompleted,
					Message: "update paused due to failure or early termination of task 9xk2",
				},
			}},
		},
	}
}

// The flap #90 is about. An automated application would read the rolled-back spec
// as drift, push the rejected spec again, be rolled back again, and find the same
// drift next interval — for ever, because a deploy without wait reports success,
// so the failure backoff never engages.
//
// Reported and not corrected. The state clears when a commit changes the plan to
// an upgrade, which is the only thing that can actually fix it.
func TestARolledBackReleaseIsReportedAndNotRedeployed(t *testing.T) {
	backend := rolledBackBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	got := view.Status.Releases[0].Drift
	if got == nil || got.State != application.DriftStateDetected {
		t.Fatalf("drift = %+v, want it still reported", got)
	}
	if len(got.Services) != 1 || got.Services[0].Reason != application.DriftRolledBack {
		t.Fatalf("services = %+v, want one rolled-back service", got.Services)
	}
	if !strings.Contains(got.Services[0].Message, "update paused") {
		t.Errorf("message = %q, want the daemon's own account", got.Services[0].Message)
	}
	if len(backend.converged()) != 0 {
		t.Errorf("redeployed %v, want nothing: Swarm has already rejected that spec", backend.converged())
	}
}

// A manual sync is not an escape hatch for it either. `force` means "do not wait
// for the schedule", not "push a spec the platform has judged" — and re-applying
// an unchanged manifest is the one thing that cannot fix this.
func TestAManualSyncDoesNotRetryARolledBackRelease(t *testing.T) {
	backend := rolledBackBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", false)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.SyncNow(context.Background(), "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	if len(backend.converged()) != 0 {
		t.Errorf("redeployed %v, want nothing", backend.converged())
	}
}

// The veto is release-wide, and that is the granularity talking rather than a
// choice: an apply writes every service the release declares, so there is no way
// to rewrite the drifting sibling without re-writing the rejected spec beside it.
func TestARolledBackServiceVetoesItsReleasesCorrection(t *testing.T) {
	backend := rolledBackBackend()
	// A second service in the same release, drifting in the ordinary way — the
	// one a correction would otherwise be right to make.
	sibling := serviceSpec("whoami_side", 2)
	backend.desired["whoami"].Services = append(backend.desired["whoami"].Services,
		compose.Service{Name: "side", Spec: sibling})
	backend.live["whoami"]["whoami_side"] = swarm.Service{ID: "id-side", Spec: serviceSpec("whoami_side", 5)}

	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if n := len(view.Status.Releases[0].Drift.Services); n != 2 {
		t.Errorf("reported %d services, want both the rollback and the sibling", n)
	}
	if len(backend.converged()) != 0 {
		t.Errorf("redeployed %v, want nothing: correcting the sibling re-pushes the rejected spec too",
			backend.converged())
	}
}

// The default mode must be exactly what it was. Every deployment runs it, and a
// nil axis is what says the question was not asked rather than answered "none".
func TestManifestModeNeverComparesAgainstTheSwarm(t *testing.T) {
	backend := driftedBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{spec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Drift != nil {
		t.Errorf("drift = %+v, want nil for a manifest-mode application", view.Status.Drift)
	}
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("state = %q, want synced", view.Status.Sync.State)
	}
	if len(backend.converged()) != 0 {
		t.Errorf("redeployed %v, want nothing", backend.converged())
	}
}

// A backend that cannot read service specs — a remote one reached through the
// same seam — must leave the axis absent rather than fail the application.
func TestLiveModeDegradesOnABackendWithoutTheSeam(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTest(t, []application.Spec{liveSpec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if view, _ := r.View("edge"); view.Status.Drift != nil {
		t.Errorf("drift = %+v, want nil when the backend cannot answer", view.Status.Drift)
	}
}

func TestLiveModeReportsNothingOnAnUntouchedStack(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil, fakeRegistry{backend: matchingBackend()})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Drift == nil || view.Status.Drift.State != application.DriftStateNone {
		t.Errorf("drift = %+v, want none", view.Status.Drift)
	}
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("state = %q, want synced", view.Status.Sync.State)
	}
}

// The folding decision: drift makes the application out of sync, so the sync
// button and anything already alerting on that field work without knowing this
// mode exists. What moved is the axis beside it.
func TestLiveDriftMakesTheApplicationOutOfSync(t *testing.T) {
	rec := listen(t)
	backend := driftedBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", false)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Errorf("state = %q, want out-of-sync", view.Status.Sync.State)
	}
	if view.Status.Sync.Summary.Drifted != 1 {
		t.Errorf("summary.drifted = %d, want 1", view.Status.Sync.Summary.Drifted)
	}
	// The plan itself still says the release is unchanged. Summary describes the
	// plan; State describes the verdict.
	if view.Status.Sync.Summary.Unchanged != 1 {
		t.Errorf("summary.unchanged = %d, want the plan still reported honestly", view.Status.Sync.Summary.Unchanged)
	}
	if view.Status.Drift == nil || view.Status.Drift.State != application.DriftStateDetected {
		t.Fatalf("drift = %+v, want detected", view.Status.Drift)
	}
	if view.Status.Drift.Services != 1 {
		t.Errorf("drift services = %d, want 1", view.Status.Drift.Services)
	}
	if rel := view.Status.Releases[0]; rel.Sync != application.SyncOutOfSync || rel.Drift == nil {
		t.Errorf("release = %+v, want it out of sync with its own drift detail", rel)
	}

	// The swarm moving is a different event from git moving: they need different
	// responses, so a notifier must be able to route on the type.
	if got := rec.types(); !slices.Contains(got, notify.LiveDriftDetected) {
		t.Errorf("events = %v, want a live-drift-detected", got)
	}
	if slices.Contains(rec.types(), notify.DriftDetected) {
		t.Error("a live drift also raised the git-drift event; one change must not report as two")
	}

	// Manual policy: reported, not corrected.
	if len(backend.converged()) != 0 {
		t.Errorf("redeployed %v, want a manual application left alone", backend.converged())
	}
}

// The notify contract the git-drift event already has: an application sits
// drifted until someone acts, and an event on every tick trains an operator to
// ignore the one that matters.
func TestLiveDriftNotifiesOnlyOnTheTransition(t *testing.T) {
	rec := listen(t)
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", false)}, engine, nil, fakeRegistry{backend: driftedBackend()})

	for range 3 {
		if err := r.Sync(context.Background(), "edge"); err != nil {
			t.Fatalf("Sync = %v, want nil", err)
		}
	}

	var n int
	for _, e := range rec.types() {
		if e == notify.LiveDriftDetected {
			n++
		}
	}
	if n != 1 {
		t.Errorf("raised %d live-drift events over three reconciles, want 1", n)
	}
}

func TestLiveDriftConvergesAnAutomatedApplication(t *testing.T) {
	rec := listen(t)
	backend := driftedBackend()
	// Drifted on the first plan, matching on the confirming re-plan — which is
	// what the correction is supposed to produce.
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	// Redeployed through the backend rather than the engine: Engine.Apply skips
	// a release it planned as unchanged, which every drifted release is.
	if got := backend.converged(); len(got) != 1 || !strings.HasPrefix(got[0], "whoami|") {
		t.Errorf("redeployed %v, want the drifted release's manifest", got)
	}
	// And no chart revision was written for it. The desired state did not change.
	if engine.applyCount() != 1 {
		t.Errorf("engine applied %d times, want the one no-op pass over an unchanged plan", engine.applyCount())
	}
	if got := rec.types(); !slices.Contains(got, notify.DriftConverged) {
		t.Errorf("events = %v, want a drift-converged", got)
	}
}

// Manual means "not on a schedule", not "never" — the same rule the manifest
// mode already follows.
func TestManualApplicationConvergesOnAnExplicitSync(t *testing.T) {
	backend := driftedBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", false)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.SyncNow(context.Background(), "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	if len(backend.converged()) != 1 {
		t.Errorf("redeployed %v, want the explicit sync to correct it", backend.converged())
	}
}

// A correction that did not happen is a write that was asked for and refused,
// so it fails the sync rather than being logged and forgotten.
func TestFailedConvergeFailsTheSync(t *testing.T) {
	backend := driftedBackend()
	backend.deployErr = errors.New("swarm refused")
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	err := r.Sync(context.Background(), "edge")
	if err == nil {
		t.Fatal("Sync = nil, want the failed correction surfaced")
	}
	if !strings.Contains(err.Error(), "whoami") {
		t.Errorf("error %q does not name the release", err)
	}
	view, _ := r.View("edge")
	if last := view.Status.Sync.LastSync; last == nil || last.Succeeded {
		t.Errorf("lastSync = %+v, want a failure recorded", last)
	}
}

// The rule that keeps the mode safe: a read that failed says Unknown, and the
// controller does not rewrite a service on the strength of it.
func TestUnreadableDriftIsNotConverged(t *testing.T) {
	backend := driftedBackend()
	backend.readErr = errors.New("daemon unreachable")
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want the reconcile to survive a failed drift read", err)
	}

	view, _ := r.View("edge")
	if view.Status.Drift == nil || view.Status.Drift.State != application.DriftStateUnknown {
		t.Fatalf("drift = %+v, want unknown", view.Status.Drift)
	}
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("state = %q, want the plan's own verdict when drift could not be read", view.Status.Sync.State)
	}
	if len(backend.converged()) != 0 {
		t.Errorf("redeployed %v, want nothing written on the strength of a read that failed", backend.converged())
	}
}

// A release the plan would change is not compared: its spec is about to be
// overwritten, so a spec-level difference is noise against a manifest the sync
// axis already reports.
func TestOnlyUnchangedReleasesAreCompared(t *testing.T) {
	backend := driftedBackend()
	engine := &fakeEngine{plans: []*charts.Plan{{Releases: []charts.ReleasePlan{
		{Name: "whoami", Ref: "repo/whoami", Action: charts.ActionUpgrade, ToVersion: "0.1.8", Manifest: "replicas: 1\n"},
	}}}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", false)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Releases[0].Drift != nil {
		t.Errorf("drift = %+v, want none for a release the plan would upgrade", view.Status.Releases[0].Drift)
	}
	if view.Status.Drift != nil {
		t.Errorf("application drift = %+v, want nil when nothing was compared", view.Status.Drift)
	}
}

// conflictingBackend accepts the out-of-band notifier and fires it, which is
// what the real backend does when a mutation loses its compare-and-swap.
type conflictingBackend struct {
	stubBackend
	fires  int
	notify func(string)
}

func (b *conflictingBackend) WithOutOfBandNotifier(fn func(string)) charts.Backend {
	c := *b
	c.notify = fn
	return &c
}

func (b *conflictingBackend) DeployStack(context.Context, string, string, string) error {
	for range b.fires {
		b.notify("edge_web")
	}
	return nil
}

// Swarm's one signal that something else is writing was being detected and
// thrown away: swarms/local builds the backend with an empty Options, so the
// callback was the no-op default and production never set it.
func TestOutOfBandWriteIsReported(t *testing.T) {
	rec := listen(t)
	// Three fires, as the retry loop produces for one conflict. One event: three
	// identical notifications describe one thing that happened.
	backend := &conflictingBackend{fires: 3}
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync(), synced()}}
	r := newTestWith(t, []application.Spec{spec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	// The engine is a fake, so drive the notifier the way a real apply would.
	b, err := fakeRegistry{backend: backend}.Backend(context.Background(), swarms.Target{})
	if err != nil {
		t.Fatal(err)
	}
	scoped := r.withOutOfBandNotifier(context.Background(), b, "edge")
	if err := scoped.DeployStack(t.Context(), "edge", "", ""); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}

	var n int
	var message string
	for _, e := range rec.got {
		if e.Type == notify.LiveDriftDetected {
			n++
			message = e.Message
		}
	}
	if n != 1 {
		t.Fatalf("raised %d events for one conflict, want 1", n)
	}
	if !strings.Contains(message, "edge_web") {
		t.Errorf("message %q does not name the service", message)
	}
}

// A service running under the stack's namespace that the manifest does not
// declare cannot be corrected by a redeploy — applying deletes nothing — so
// treating it as convergeable would redeploy the whole stack on every tick
// forever, raising a correction event each time and never removing the thing it
// was reacting to.
//
// Reported, and left alone. The application stays out of sync, which is true.
func TestAnUnexpectedServiceIsReportedButNotConverged(t *testing.T) {
	rec := listen(t)
	spec := serviceSpec("whoami_app", 1)
	stray := serviceSpec("whoami_leftover", 1)
	backend := &driftBackend{
		desired: map[string]*compose.Stack{
			"whoami": {Services: []compose.Service{{Name: "app", Spec: spec}}},
		},
		live: map[string]map[string]swarm.Service{
			"whoami": {"whoami_app": {ID: "a", Spec: spec}, "whoami_leftover": {ID: "b", Spec: stray}},
		},
	}
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	if view.Status.Drift == nil || view.Status.Drift.State != application.DriftStateDetected {
		t.Fatalf("drift = %+v, want it reported", view.Status.Drift)
	}
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Errorf("state = %q, want out-of-sync — the swarm really does not match", view.Status.Sync.State)
	}
	// The operator is told, because nothing else is going to deal with it.
	if got := rec.types(); !slices.Contains(got, notify.LiveDriftDetected) {
		t.Errorf("events = %v, want the operator told about it", got)
	}
	// And nothing was written, on this tick or any other.
	if got := backend.converged(); len(got) != 0 {
		t.Errorf("redeployed %v, want nothing a redeploy could not fix", got)
	}
	if engine.applyCount() != 0 {
		t.Errorf("engine applied %d times, want the apply cycle skipped entirely", engine.applyCount())
	}
}

// The other half: a missing service is convergeable, because a redeploy creates
// what is not there.
func TestAMissingServiceIsConverged(t *testing.T) {
	spec := serviceSpec("whoami_app", 1)
	backend := &driftBackend{
		desired: map[string]*compose.Stack{
			"whoami": {Services: []compose.Service{{Name: "app", Spec: spec}}},
		},
		live: map[string]map[string]swarm.Service{"whoami": {}},
	}
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := backend.converged(); len(got) != 1 {
		t.Errorf("redeployed %v, want the release with the missing service", got)
	}
}

// ------------------------------------------- converge honours syncPolicy.wait

// convergingBackend replays a scripted sequence of convergence states, so a test
// can watch the wait poll rather than merely observe its result. The last entry
// repeats once the script runs out.
type convergingBackend struct {
	*driftBackend

	mu     sync.Mutex
	script [][]charts.ServiceState
	calls  int
}

func (b *convergingBackend) StackServices(context.Context, string) []charts.ServiceState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.script[min(b.calls-1, len(b.script)-1)]
}

func (b *convergingBackend) reads() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func converging(script ...[]charts.ServiceState) *convergingBackend {
	return &convergingBackend{driftBackend: driftedBackend(), script: script}
}

// NewestTaskAge past the stability window: parity alone is not convergence, and
// a fixture that ignored the window would report converged where a real swarm
// would still be waiting.
func convergedStates() []charts.ServiceState {
	return []charts.ServiceState{{Name: "whoami_app", Running: 1, Desired: 1, NewestTaskAge: time.Hour}}
}

func progressingStates() []charts.ServiceState {
	return []charts.ServiceState{{Name: "whoami_app", Running: 0, Desired: 1}}
}

// Swarm never rolls back a rollback, so a paused rollout needs a human — the
// engine calls it wedged and waiting it out only delays the report.
func wedgedStates() []charts.ServiceState {
	return []charts.ServiceState{{Name: "whoami_app", Running: 1, Desired: 1, UpdateState: "paused"}}
}

// A rollout Swarm undid: at parity and past the stability window, on the
// previous spec. NewestTaskAge is the point of the fixture — it is what made the
// engine's parity check answer converged before Eldara-Tech/swarmcli#530, so a
// state without it would prove nothing about the switch.
func rolledBackStates() []charts.ServiceState {
	return []charts.ServiceState{{
		Name: "whoami_app", Running: 1, Desired: 1,
		UpdateState: "rollback_completed", NewestTaskAge: time.Hour,
	}}
}

func waitingSpec(name string, automated bool) application.Spec {
	s := liveSpec(name, automated)
	s.SyncPolicy.Wait = true
	return s
}

// fastPolling shrinks the converge poll so a test does not spend it.
func fastPolling(t *testing.T) {
	t.Helper()
	prev := convergePollInterval
	convergePollInterval = time.Millisecond
	t.Cleanup(func() { convergePollInterval = prev })
}

// A correction is a deploy, so syncPolicy.wait has to mean the same thing for it
// as for any other. It did not: converging writes through DeployStack rather
// than Engine.Apply, and the engine is where the wait lives.
func TestConvergeWaitsForTheCorrectionToSettle(t *testing.T) {
	fastPolling(t)
	backend := converging(progressingStates(), progressingStates(), convergedStates())
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{waitingSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	// It polled rather than accepting the first answer, which is the whole point:
	// the daemon accepts the write long before the rollout has landed.
	if got := backend.reads(); got < 3 {
		t.Errorf("read the swarm %d times, want it to have polled until converged", got)
	}
	if got := backend.converged(); len(got) != 1 {
		t.Errorf("redeployed %v, want the drifted release", got)
	}
}

// A correction that deploys and then wedges recorded a successful sync. Because
// health only turns a slow rollout into a broken one on the back of a failed
// sync, the release then read as progressing indefinitely instead of degraded —
// the exact failure the wait policy exists to surface.
func TestConvergeThatWedgesFailsTheSync(t *testing.T) {
	fastPolling(t)
	rec := listen(t)
	backend := converging(wedgedStates())
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{waitingSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	err := r.Sync(context.Background(), "edge")
	if err == nil {
		t.Fatal("Sync = nil, want the wedged correction surfaced")
	}
	if !strings.Contains(err.Error(), "whoami") {
		t.Errorf("error %q does not name the release", err)
	}

	view, _ := r.View("edge")
	if last := view.Status.Sync.LastSync; last == nil || last.Succeeded {
		t.Errorf("lastSync = %+v, want a failure recorded", last)
	}
	// Deployed but not settled, so it is not announced as corrected.
	if slices.Contains(rec.types(), notify.DriftConverged) {
		t.Error("announced a correction that never converged")
	}
}

// The other half of the same failure, and it could not be closed in this
// repository: awaitConverged's judgement is charts.Rollup's on purpose rather
// than a second copy of the predicate, and the predicate let rollback_completed
// fall through to the parity check. So a correction Swarm undid reached parity
// on the spec the correction was trying to remove, outlived the stability
// window, and reported converged — a successful sync, a DriftConverged event,
// and a release reading healthy. #104 item 4; fixed in Eldara-Tech/swarmcli#530
// and reaching this repository through the pin.
func TestConvergeThatSwarmRolledBackFailsTheSync(t *testing.T) {
	fastPolling(t)
	rec := listen(t)
	backend := converging(rolledBackStates())
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{waitingSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	err := r.Sync(context.Background(), "edge")
	if err == nil {
		t.Fatal("Sync = nil, want the rolled-back correction surfaced")
	}
	if !strings.Contains(err.Error(), "whoami") || !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error %q does not name the release and why it did not converge", err)
	}
	view, _ := r.View("edge")
	if last := view.Status.Sync.LastSync; last == nil || last.Succeeded {
		t.Errorf("lastSync = %+v, want a failure recorded", last)
	}
	if slices.Contains(rec.types(), notify.DriftConverged) {
		t.Error("announced a correction Swarm had already undone")
	}
}

// An application that did not ask to wait must not start: the policy is opt-in,
// and waiting by default would make every correction as slow as its rollout.
func TestConvergeDoesNotWaitWithoutThePolicy(t *testing.T) {
	// Deliberately no fastPolling: if this waited at all it would poll a
	// backend that never converges, and the test would hang rather than fail.
	backend := converging(progressingStates())
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := backend.converged(); len(got) != 1 {
		t.Errorf("redeployed %v, want the correction to have gone ahead unwaited", got)
	}
}

// A rollout that never lands must end, or the reconcile loop for this
// application never ticks again.
func TestConvergeWaitTimesOut(t *testing.T) {
	fastPolling(t)
	backend := converging(progressingStates())
	engine := &fakeEngine{plans: []*charts.Plan{synced()}}

	// A clock that advances a minute per reading, so the deadline is reached in
	// a handful of polls rather than in real time.
	var ticks int
	clock := func() time.Time {
		ticks++
		return time.Unix(0, 0).UTC().Add(time.Duration(ticks) * time.Minute)
	}

	spec := waitingSpec("edge", true)
	spec.SyncPolicy.Timeout = application.Duration(3 * time.Minute)
	r := New([]application.Spec{spec}, Options{
		Fetcher:   &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{backend: backend},
		NewEngine: func(charts.Backend) Engine { return engine },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       clock,
	})

	err := r.Sync(context.Background(), "edge")
	if err == nil {
		t.Fatal("Sync = nil, want a timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q does not report a timeout", err)
	}
}

// ----------------------------------------- syncPolicy.pruneResources (#75, #80)

// The stack as the swarm holds it: two services running, a chart that has
// stopped declaring one of them, and a stored revision that declared both.
//
// That last part is the whole point. The namespace label says whoami_sidecar
// belongs to this release and anything could have set it; the revision says this
// controller installed it, and only that is a basis for deleting.
func sweepBackend() *driftBackend {
	app := serviceSpec("whoami_app", 1)
	sidecar := serviceSpec("whoami_sidecar", 1)
	return &driftBackend{
		desired: map[string]*compose.Stack{
			"whoami": {Services: []compose.Service{{Name: "app", Spec: app}}},
		},
		byManifest: map[string]*compose.Stack{
			"had-a-sidecar": {Services: []compose.Service{
				{Name: "app", Spec: app},
				{Name: "sidecar", Spec: sidecar},
			}},
		},
		live: map[string]map[string]swarm.Service{
			"whoami": {
				"whoami_app":     {ID: "id-app", Spec: app},
				"whoami_sidecar": {ID: "id-sidecar", Spec: sidecar},
			},
		},
	}
}

// revisions of "whoami" stamped for app, newest last, each naming the manifest
// the backend above will convert for it.
func sweepHistory(app string, manifests ...string) map[string][]charts.Release {
	stamp := charts.OwnerRef{
		ID:   application.OwnerID("", app),
		Kind: charts.OwnerKindRelease,
		Name: "whoami",
	}.String()

	revs := make([]charts.Release, 0, len(manifests))
	for i, m := range manifests {
		revs = append(revs, charts.Release{
			Name: "whoami", Revision: i + 1, Manifest: m, Owner: stamp,
		})
	}
	return map[string][]charts.Release{"whoami": revs}
}

func sweepingSpec(name string, drift application.DriftDetection) application.Spec {
	s := spec(name, true)
	s.SyncPolicy.PruneResources = true
	s.DriftDetection = drift
	return s
}

func orphanedServices(t *testing.T, r *Reconciler, app string) []string {
	t.Helper()
	view, _ := r.View(app)
	var out []string
	for _, rel := range view.Status.Releases {
		if rel.Drift == nil {
			continue
		}
		for _, svc := range rel.Drift.Services {
			if svc.Orphaned {
				out = append(out, svc.Name)
			}
		}
	}
	return out
}

// The case in the issue, in the default drift mode. A service dropped from a
// template is removed, and the one the chart still declares is untouched.
//
// Deliberately manifest mode: the feature is about git moving, not about the
// swarm moving, and hanging it off driftDetection: live would make it a silent
// no-op for every application running the default.
func TestAServiceDroppedFromTheChartIsPrunedInManifestMode(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if want := []string{"id-sidecar"}; !slices.Equal(backend.pruned(), want) {
		t.Errorf("removed %v, want %v — only the service the chart stopped declaring", backend.pruned(), want)
	}
}

// A revision proves ownership by what it declared, and that is a question about
// the past — so it must not depend on any of it still existing (#87).
//
// The stored revision here mounted something a previous sweep has already
// deleted, so the resolving conversion of it fails the way a real daemon fails
// it: "config not found". Before this the sweep lost that revision's claims and
// left the service behind, warning about a manifest it could not read; now it
// reads the names and deletes what the revision proves is this application's.
func TestAStoredRevisionStillClaimsWhatItDeclaredAfterASweep(t *testing.T) {
	backend := sweepBackend()
	backend.unresolvableErr = map[string]error{
		"had-a-sidecar": errors.New("converting services: service sidecar: config not found: whoami_swept"),
	}
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if want := []string{"id-sidecar"}; !slices.Equal(backend.pruned(), want) {
		t.Errorf("removed %v, want %v — the revision declared it, whether or not what it mounted survives",
			backend.pruned(), want)
	}
}

// The same defect one call earlier, and the one that made it reachable without any
// history at all: observe runs *before* the apply, so an upgraded release's
// manifest is the new one — and the upgrade that first adds a config a service
// mounts cannot be resolved yet, because this deploy is what creates it.
//
// Asking the resolving conversion there cost that release its whole sweep for the
// interval. The sweep wants names, so it no longer asks for ids.
func TestAnUpgradeThatAddsAMountedConfigStillSweeps(t *testing.T) {
	backend := sweepBackend()
	backend.unresolvableErr = map[string]error{
		"adds-a-config": errors.New("converting services: service app: config not found: whoami_new"),
	}
	engine := &fakeEngine{
		plans: []*charts.Plan{{Releases: []charts.ReleasePlan{{
			Name: "whoami", Ref: "repo/whoami", Action: charts.ActionUpgrade,
			ToVersion: "0.1.9", CurrentManifest: "had-a-sidecar", Manifest: "adds-a-config",
		}}}},
		history: sweepHistory("edge", "had-a-sidecar"),
	}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if want := []string{"id-sidecar"}; !slices.Equal(backend.pruned(), want) {
		t.Errorf("removed %v, want %v — the upgrade declares what it declares, "+
			"whether or not the daemon can resolve what it mounts yet", backend.pruned(), want)
	}
}

// A settled release whose mounted config somebody deleted by hand. The comparison
// is genuinely impossible — there is no spec to diff against — so live drift says
// so; but what the release *declares* is as legible as ever, so the sweep is
// unaffected. One reading failing must not cost the other.
func TestAnUnresolvableSettledReleaseLosesItsComparisonAndNotItsSweep(t *testing.T) {
	backend := sweepBackend()
	backend.unresolvableErr = map[string]error{
		"current": errors.New("converting services: service app: config not found: whoami_gone"),
	}
	engine := &fakeEngine{
		plans: []*charts.Plan{{Releases: []charts.ReleasePlan{{
			Name: "whoami", Ref: "repo/whoami", Action: charts.ActionUnchanged,
			ToVersion: "0.1.8", Manifest: "current",
		}}}},
		history: sweepHistory("edge", "had-a-sidecar"),
	}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftLive)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := r.View("edge")
	got := view.Status.Releases[0].Drift
	if got == nil || got.State != application.DriftStateUnknown {
		t.Fatalf("drift = %+v, want it reported unknown rather than compared", got)
	}
	if !strings.Contains(got.Message, "config not found") {
		t.Errorf("message = %q, want it to say why nothing could be compared", got.Message)
	}
	if want := []string{"id-sidecar"}; !slices.Equal(backend.pruned(), want) {
		t.Errorf("removed %v, want %v — a comparison it cannot make is not a sweep it cannot make",
			backend.pruned(), want)
	}
}

// oldSeamBackend implements liveDriftBackend and nothing else: a backend written
// against the seam as it stood before #87, or a Phase 3 remote one that answers
// what it can. It is the fallback path in claimed, which must be the behaviour
// that preceded declaredLister rather than no history walk at all.
type oldSeamBackend struct{ stack *compose.Stack }

func (o oldSeamBackend) DesiredServices(context.Context, string, string) (*compose.Stack, error) {
	return o.stack, nil
}

func (oldSeamBackend) LiveServices(context.Context, string) (map[string]swarm.Service, error) {
	return nil, nil
}

// A backend that cannot answer the names-only question loses nothing it had. The
// sweep falls back to the resolving conversion, and a revision that converts
// still claims what it declared.
func TestAnOlderBackendStillProvesOwnershipThroughTheResolvingConversion(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: sweepBackend()})
	ldb := oldSeamBackend{stack: &compose.Stack{Services: []compose.Service{
		{Name: "sidecar", Spec: serviceSpec("whoami_sidecar", 1)},
	}}}
	if _, ok := any(ldb).(declaredLister); ok {
		t.Fatal("oldSeamBackend implements declaredLister; it is meant to be the backend that does not")
	}

	got := r.claimed(context.Background(), sweepingSpec("edge", application.DriftManifest), ldb, engine,
		"whoami", resourceNames{services: []string{"whoami_sidecar"}})

	if want := []string{"whoami_sidecar"}; !slices.Equal(got.services, want) {
		t.Errorf("claimed %v, want %v through the older seam", got.services, want)
	}
}

// Manifest mode has no drift axis at all, so without this the controller would
// delete a service and leave no record of it anywhere but a log line.
func TestAPrunedServiceIsReportedInManifestModeBeforeItGoes(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	// Manual, so the report can be read at the point where nothing has acted on
	// it yet — which is also the only moment an operator could object.
	spec := sweepingSpec("edge", application.DriftManifest)
	spec.SyncPolicy.Automated = false
	r := newTestWith(t, []application.Spec{spec}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if len(backend.pruned()) != 0 {
		t.Errorf("removed %v on a manual policy, want nothing", backend.pruned())
	}
	if want := []string{"whoami_sidecar"}; !slices.Equal(orphanedServices(t, r, "edge"), want) {
		t.Errorf("orphaned = %v, want %v", orphanedServices(t, r, "edge"), want)
	}
	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Errorf("state = %q, want out-of-sync — a leftover service is the swarm not matching git", view.Status.Sync.State)
	}

	// And manual means "not on a schedule" rather than "never".
	if err := r.SyncNow(context.Background(), "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	if want := []string{"id-sidecar"}; !slices.Equal(backend.pruned(), want) {
		t.Errorf("removed %v after an explicit sync, want %v", backend.pruned(), want)
	}
}

// The pre-#75 behaviour, asserted so it cannot regress into a deletion nobody
// asked for. Live mode, because that is where an unexpected service is reported
// at all.
func TestAnUnexpectedServiceSurvivesWhenPruneResourcesIsOff(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{liveSpec("edge", true)}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if len(backend.pruned()) != 0 {
		t.Errorf("removed %v with pruneResources off, want nothing", backend.pruned())
	}
	// Still reported, and without the marker: no proof is computed for an
	// application that did not ask for one.
	if got := orphanedServices(t, r, "edge"); len(got) != 0 {
		t.Errorf("orphaned = %v, want none", got)
	}
	view, _ := r.View("edge")
	if got := view.Status.Drift; got == nil || got.State != application.DriftStateDetected {
		t.Errorf("drift = %+v, want detected — it is still unexpected", got)
	}
}

// The #62 lesson one scope down. A service under the stack's namespace that no
// revision of ours ever declared is not ours to delete, whatever the label says.
func TestAServiceNoRevisionOfOursDeclaredIsNeverPruned(t *testing.T) {
	backend := sweepBackend()
	// The history is real and ours, and it never mentioned a sidecar.
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftLive)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if len(backend.pruned()) != 0 {
		t.Errorf("removed %v, want nothing — the namespace label is not evidence", backend.pruned())
	}
	if got := orphanedServices(t, r, "edge"); len(got) != 0 {
		t.Errorf("orphaned = %v, want none — it was never proved ours", got)
	}
}

// The same, when the revision that declared it belongs to a sibling application.
// Its stamp names another owner, so it is that application's business.
func TestAServiceDeclaredOnlyByAnotherApplicationsRevisionIsNeverPruned(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("somebody-else", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftLive)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if len(backend.pruned()) != 0 {
		t.Errorf("removed %v, want nothing — the stamp names another application", backend.pruned())
	}
}

func TestAReleaseWhoseHistoryCannotBeReadPrunesNothing(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, histErr: errors.New("swarm unreachable")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftLive)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil — an unreadable history is not a failed deploy", err)
	}
	if len(backend.pruned()) != 0 {
		t.Errorf("removed %v, want nothing — nothing could be proved", backend.pruned())
	}
}

func TestAReleaseWhoseServicesCannotBeReadPrunesNothing(t *testing.T) {
	backend := sweepBackend()
	backend.readErr = errors.New("daemon hiccup")
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftLive)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if len(backend.pruned()) != 0 {
		t.Errorf("removed %v, want nothing — no service is deleted on a read that failed", backend.pruned())
	}
}

// The running state is not yet the declared one, so nothing about it is settled
// enough to delete from.
func TestNothingIsPrunedWhenTheApplyFailed(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{
		plans:    []*charts.Plan{synced()},
		history:  sweepHistory("edge", "had-a-sidecar"),
		applyErr: errors.New("no such image"),
	}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftLive)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Fatal("Sync = nil, want the apply failure")
	}
	if len(backend.pruned()) != 0 {
		t.Errorf("removed %v after a failed apply, want nothing", backend.pruned())
	}
}

// The pass that drops a service from a template is an upgrade, and that is the
// case worth putting right at once rather than an interval later.
func TestAnUpgradedReleaseIsSweptInTheSamePass(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync(), synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if want := []string{"id-sidecar"}; !slices.Equal(backend.pruned(), want) {
		t.Errorf("removed %v, want %v", backend.pruned(), want)
	}
}

// A release the plan would install has nothing of ours deployed under that name,
// so there is nothing that could be proved ours and nothing to read.
func TestAnInstalledReleaseIsNeverSwept(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{
		plans: []*charts.Plan{{Releases: []charts.ReleasePlan{
			{Name: "whoami", Ref: "repo/whoami", Action: charts.ActionInstall, ToVersion: "0.1.8"},
		}}},
		history: sweepHistory("edge", "had-a-sidecar"),
	}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftLive)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if len(backend.pruned()) != 0 {
		t.Errorf("removed %v, want nothing", backend.pruned())
	}
	if len(backend.converted()) != 0 {
		t.Errorf("converted %v, want nothing read at all", backend.converted())
	}
}

// pruneFirst is about not overlapping, and a service renamed within a template
// overlaps exactly as a renamed release does.
func TestPruneFirstRemovesTheServiceBeforeTheApply(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync(), synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	backend.appliedAt = engine.applyCount

	spec := sweepingSpec("edge", application.DriftManifest)
	spec.SyncPolicy.PruneFirst = true
	r := newTestWith(t, []application.Spec{spec}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if want := []int{0}; !slices.Equal(backend.removedAtApply, want) {
		t.Errorf("removed after %v applies, want %v — pruneFirst deletes before deploying", backend.removedAtApply, want)
	}
}

// The default order, and the reason for it: a failed apply must leave the old
// service running rather than already deleted.
func TestTheDefaultOrderRemovesTheServiceAfterTheApply(t *testing.T) {
	backend := sweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync(), synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	backend.appliedAt = engine.applyCount

	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if want := []int{1}; !slices.Equal(backend.removedAtApply, want) {
		t.Errorf("removed after %v applies, want %v", backend.removedAtApply, want)
	}
}

// The walk stops as soon as nothing is unaccounted for, so a release upgraded a
// thousand times does not convert a thousand manifests to prove one service.
func TestTheHistoryWalkStopsAtTheNewestRevisionThatDeclaredIt(t *testing.T) {
	backend := sweepBackend()
	backend.byManifest["older-with-sidecar"] = backend.byManifest["had-a-sidecar"]
	engine := &fakeEngine{
		plans:   []*charts.Plan{synced()},
		history: sweepHistory("edge", "older-with-sidecar", "had-a-sidecar"),
	}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if slices.Contains(backend.converted(), "older-with-sidecar") {
		t.Errorf("converted %v, want the walk to stop at the newest revision that claimed it", backend.converted())
	}
}

// A sweep that could not delete does not fail the deploy that did land, but the
// reconcile still says it did not do everything it set out to.
func TestAFailedRemovalIsReportedWithoutFailingTheDeploy(t *testing.T) {
	backend := sweepBackend()
	backend.removeErr = errors.New("service is updating")
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Fatal("Sync = nil, want the removal failure reported")
	}
	view, _ := r.View("edge")
	if last := view.Status.Sync.LastSync; last == nil || !last.Succeeded {
		t.Errorf("last sync = %+v, want a success — the deploy landed, only the sweep did not", last)
	}
}

// -------------------------------------- networks, configs and secrets (#80)

// The same stack one scope wider: the chart has stopped declaring a network, a
// config and a secret as well as a service, and still declares one of each.
//
// The "keep" half is what makes this a test rather than a demonstration. A sweep
// that deleted everything under the namespace would pass a test that only
// asserted the departed ones were gone.
func resourceSweepBackend() *driftBackend {
	b := sweepBackend()
	b.desired["whoami"].Networks = []compose.Network{{Name: "whoami_keep"}}
	b.desired["whoami"].Configs = []swarm.ConfigSpec{{Annotations: swarm.Annotations{Name: "whoami_cfg-keep"}}}
	b.desired["whoami"].Secrets = []swarm.SecretSpec{{Annotations: swarm.Annotations{Name: "whoami_sec-keep"}}}

	had := b.byManifest["had-a-sidecar"]
	had.Networks = []compose.Network{{Name: "whoami_keep"}, {Name: "whoami_gone"}}
	had.Configs = []swarm.ConfigSpec{
		{Annotations: swarm.Annotations{Name: "whoami_cfg-keep"}},
		{Annotations: swarm.Annotations{Name: "whoami_cfg-old"}},
	}
	had.Secrets = []swarm.SecretSpec{
		{Annotations: swarm.Annotations{Name: "whoami_sec-keep"}},
		{Annotations: swarm.Annotations{Name: "whoami_sec-old"}},
	}

	b.liveNetworks = map[string]map[string]string{
		"whoami": {"whoami_keep": "n-keep", "whoami_gone": "n-gone"},
	}
	b.liveConfigs = map[string]map[string]string{
		"whoami": {"whoami_cfg-keep": "c-keep", "whoami_cfg-old": "c-old"},
	}
	b.liveSecrets = map[string]map[string]string{
		"whoami": {"whoami_sec-keep": "k-keep", "whoami_sec-old": "k-old"},
	}
	return b
}

// The issue's case for the other three kinds. Each one the chart stopped
// declaring goes; each one it still declares stays.
//
// The removal order is asserted because it is the order they stop being
// referenced in — the same order RemoveStack uses, and the reason a config is
// not attempted after the network it might have been reachable through.
func TestResourcesDroppedFromTheChartArePruned(t *testing.T) {
	backend := resourceSweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	want := []string{"config:c-old", "secret:k-old", "network:n-gone"}
	if got := backend.prunedResources(); !slices.Equal(got, want) {
		t.Errorf("pruned %v, want %v — configs, then secrets, then networks", got, want)
	}
	// The service sweep still works, and both halves came out of one proof.
	if want := []string{"id-sidecar"}; !slices.Equal(backend.pruned(), want) {
		t.Errorf("pruned services %v, want %v", backend.pruned(), want)
	}
	// One revision converted, answering all four kinds. Sweeping four costs what
	// sweeping one did; a walk per kind would show four reads of it.
	if got := slices.Collect(func(yield func(string) bool) {
		for _, m := range backend.converted() {
			if m == "had-a-sidecar" && !yield(m) {
				return
			}
		}
	}); len(got) != 1 {
		t.Errorf("converted the claiming revision %d times, want once for all four kinds", len(got))
	}
}

// The #62 lesson, one kind further out. A network carrying the namespace label
// that no revision of ours ever declared is not ours to delete.
func TestAResourceNoRevisionOfOursDeclaredIsNeverPruned(t *testing.T) {
	backend := resourceSweepBackend()
	// A network somebody else attached to this stack, and a history that is ours
	// and never mentioned it.
	backend.liveNetworks["whoami"]["whoami_stranger"] = "n-stranger"
	backend.byManifest["had-a-sidecar"].Networks = []compose.Network{{Name: "whoami_keep"}}
	delete(backend.liveNetworks["whoami"], "whoami_gone")

	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if slices.Contains(backend.prunedResources(), "network:n-stranger") {
		t.Errorf("pruned %v, want the stranger left alone — the namespace label is not evidence",
			backend.prunedResources())
	}
}

// Swarm scopes all four kinds into one namespace of names, so "whoami_sidecar"
// can be both a service and a config. The revision declared the service by that
// name and never a config, so the config is unproved and must survive.
func TestAConfigIsNotClaimedByASameNamedService(t *testing.T) {
	backend := resourceSweepBackend()
	backend.liveConfigs["whoami"]["whoami_sidecar"] = "c-namesake"

	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if slices.Contains(backend.prunedResources(), "config:c-namesake") {
		t.Errorf("pruned %v, want the config spared — only the service of that name was declared",
			backend.prunedResources())
	}
	// The service of the same name still goes, so this is not the sweep failing
	// wholesale.
	if want := []string{"id-sidecar"}; !slices.Equal(backend.pruned(), want) {
		t.Errorf("pruned services %v, want %v", backend.pruned(), want)
	}
}

// Swarm refuses to remove anything still in use, and a config whose service was
// removed moments ago is still in use while its tasks drain. That is the
// ordinary case, not a fault: failing the reconcile on it would report an error
// every interval for something working exactly as intended.
func TestAResourceStillInUseIsReportedAndTheReconcileSucceeds(t *testing.T) {
	backend := resourceSweepBackend()
	backend.resourceRemoveErr = map[string]error{
		"config:c-old": errors.New("config is in use by service whoami_sidecar"),
	}
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil — a refusal is what the next interval is for", err)
	}
	// The refusal does not stop the rest of the sweep.
	want := []string{"secret:k-old", "network:n-gone"}
	if got := backend.prunedResources(); !slices.Equal(got, want) {
		t.Errorf("pruned %v, want %v — one refusal does not abandon the others", got, want)
	}
	view, _ := r.View("edge")
	if last := view.Status.Sync.LastSync; last == nil || !last.Succeeded {
		t.Errorf("last sync = %+v, want a success", last)
	}
}

// The two sweeps are independent, and the comment above pruneResources claimed
// it ran "always here, under both orderings" while sitting six lines below a
// return that skipped it. So one release whose service teardown was stuck
// silenced the config, secret and network sweep for every release in the plan —
// and a config left behind is exactly what nobody goes looking for afterwards
// (#107).
func TestAFailedServiceSweepDoesNotSilenceTheResourceSweep(t *testing.T) {
	backend := resourceSweepBackend()
	backend.removeErr = errors.New("service is updating")
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Fatal("Sync = nil, want the service removal failure reported")
	}
	want := []string{"config:c-old", "secret:k-old", "network:n-gone"}
	if got := backend.prunedResources(); !slices.Equal(got, want) {
		t.Errorf("pruned %v, want %v — a stuck service is no reason to leave the rest behind", got, want)
	}
}

// pruneFirst is about a workload not overlapping itself. A network or a config
// has no such hazard and cannot go before the service holding it has, so the
// flag moves the services and deliberately leaves these where they are — which
// under pruneFirst is the better order anyway, the services being gone already.
func TestPruneFirstMovesTheServicesAndNotTheResources(t *testing.T) {
	backend := resourceSweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	backend.appliedAt = engine.applyCount

	spec := sweepingSpec("edge", application.DriftManifest)
	spec.SyncPolicy.PruneFirst = true
	r := newTestWith(t, []application.Spec{spec}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if want := []int{0}; !slices.Equal(backend.removedAtApply, want) {
		t.Errorf("services removed after %v applies, want %v — pruneFirst deletes before deploying",
			backend.removedAtApply, want)
	}
	if want := []int{1, 1, 1}; !slices.Equal(backend.resourcesAtApply, want) {
		t.Errorf("resources removed after %v applies, want %v — always after the apply",
			backend.resourcesAtApply, want)
	}
}

// Deleting with no record of it anywhere but a log line would be worse than not
// deleting. Manifest mode has no drift axis of its own, so the sweep builds one
// carrying the orphans and nothing else.
func TestDepartedResourcesAreReportedOnTheDriftAxis(t *testing.T) {
	backend := resourceSweepBackend()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	// A manual policy reports and does not act, which is what makes this the
	// operator's warning rather than a receipt.
	spec := sweepingSpec("edge", application.DriftManifest)
	spec.SyncPolicy.Automated = false
	r := newTestWith(t, []application.Spec{spec}, engine, nil, fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := backend.prunedResources(); len(got) != 0 {
		t.Fatalf("pruned %v on a manual policy, want nothing", got)
	}

	view, _ := r.View("edge")
	var got []string
	for _, rel := range view.Status.Releases {
		if rel.Drift == nil {
			continue
		}
		for _, res := range rel.Drift.Resources {
			got = append(got, string(res.Kind)+" "+res.Name)
		}
	}
	slices.Sort(got)
	want := []string{"config whoami_cfg-old", "network whoami_gone", "secret whoami_sec-old"}
	if !slices.Equal(got, want) {
		t.Errorf("reported %v, want %v", got, want)
	}
	if d := view.Status.Drift; d == nil || d.Resources != 3 {
		t.Errorf("drift = %+v, want 3 departed resources counted", d)
	}
}

// A backend that can read services but not the other three kinds loses only the
// half it cannot answer. The same degradation live drift already has.
func TestABackendThatCannotListResourcesStillSweepsServices(t *testing.T) {
	backend := resourceSweepBackend()
	backend.listErr = errors.New("not implemented here")
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := backend.prunedResources(); len(got) != 0 {
		t.Errorf("pruned %v, want nothing on a read that failed", got)
	}
	if want := []string{"id-sidecar"}; !slices.Equal(backend.pruned(), want) {
		t.Errorf("pruned services %v, want %v — the readable half still works", backend.pruned(), want)
	}
}

// ------------------------------------------- history and reproducible renders

// An application that says nothing about historyMax gets the bound rather than
// the chart engine's keep-everything. Each revision is a Docker Config in raft,
// written by every deploy that changes anything, so "keep all" is a slow leak on
// the manager node with nothing to stop it.
func TestHistoryIsBoundedByDefault(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync(), synced()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := engine.opts[0].HistoryMax; got != application.DefaultHistoryMax {
		t.Errorf("HistoryMax = %d, want the default %d", got, application.DefaultHistoryMax)
	}
}

// And an operator who writes 0 down still gets keep-everything, because that is
// what the number has always meant to the engine.
func TestAnExplicitZeroHistoryMaxStillKeepsEverything(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync(), synced()}}
	keepAll := 0
	app := spec("edge", true)
	app.SyncPolicy.HistoryMax = &keepAll
	r := newTest(t, []application.Spec{app}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := engine.opts[0].HistoryMax; got != 0 {
		t.Errorf("HistoryMax = %d, want 0 — an explicit zero means keep all", got)
	}
}

// A chart whose render is not reproducible deploys, is immediately planned as
// out of date again, and would deploy again for ever: every service rewritten,
// every task rolled and another revision stored, on every interval. The
// confirming re-plan sees it; this is it acting on what it saw.
func TestANonReproducibleRenderIsDeployedOnceAndThenHeld(t *testing.T) {
	// Always out of date, however often it is planned — which is exactly what
	// {{ now }} in a label produces.
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("first Sync = %v, want nil", err)
	}
	if engine.applyCount() != 1 {
		t.Fatalf("applied %d times on the first sync, want 1", engine.applyCount())
	}

	err := r.Sync(context.Background(), "edge")
	if err == nil {
		t.Fatal("second Sync = nil, want it refused: the chart does not render the same twice")
	}
	if !strings.Contains(err.Error(), "does not render the same twice") {
		t.Errorf("error = %v, want it to name the cause", err)
	}
	if engine.applyCount() != 1 {
		t.Errorf("applied %d times, want the redeploy loop stopped after the first", engine.applyCount())
	}
	// Reported, not hidden: the reconcile really did not converge.
	view, _ := r.View("edge")
	if !strings.Contains(view.Status.Error, "does not render the same twice") {
		t.Errorf("status error = %q, want the hold explained on the status", view.Status.Error)
	}
}

// The hold is on that revision. A commit is the remedy, and it must not need a
// manual sync to be picked up.
func TestANewRevisionReleasesTheHold(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync()}}
	fetcher := &fakeFetcher{revision: strings.Repeat("a", 40)}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, fetcher)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Fatal("Sync = nil, want the application held at the revision it did not settle at")
	}

	fetcher.mu.Lock()
	fetcher.revision = strings.Repeat("b", 40)
	fetcher.mu.Unlock()

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync after a new commit = %v, want the hold released", err)
	}
	if engine.applyCount() != 2 {
		t.Errorf("applied %d times, want the new revision deployed", engine.applyCount())
	}
}

// An operator asking explicitly is the way past it. The hold exists to stop an
// unattended loop redeploying for ever, not to refuse a person.
func TestAForcedSyncIgnoresTheHold(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Fatal("Sync = nil, want it held")
	}

	if err := r.SyncNow(context.Background(), "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want a forced sync to deploy anyway", err)
	}
	if engine.applyCount() != 2 {
		t.Errorf("applied %d times, want the forced sync to have deployed", engine.applyCount())
	}
}

// The ordinary case must not be caught by any of this: a re-plan that comes back
// settled leaves nothing held, so the next tick deploys the next commit.
func TestASettledRePlanHoldsNothing(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync(), synced(), outOfSync(), synced()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	for i := range 2 {
		if err := r.Sync(context.Background(), "edge"); err != nil {
			t.Fatalf("Sync %d = %v, want nil", i, err)
		}
	}
	if engine.applyCount() != 2 {
		t.Errorf("applied %d times, want a chart that settles deployed on both passes", engine.applyCount())
	}
}

// Editing the application clears the hold too. A new spec is a different chart
// path or a different values file, so whatever the old one could not render
// reproducibly is no longer evidence about this one — and an edit made to fix it
// must not then need a manual sync to take effect.
func TestReplaceClearsTheHold(t *testing.T) {
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if err := r.Sync(context.Background(), "edge"); err == nil {
		t.Fatal("Sync = nil, want it held")
	}

	edited := spec("edge", true)
	edited.Source.ReleaseFile = "swarm/other.yaml"
	if err := r.Replace(edited); err != nil {
		t.Fatalf("Replace = %v, want nil", err)
	}

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync after an edit = %v, want the hold cleared", err)
	}
	if engine.applyCount() != 2 {
		t.Errorf("applied %d times, want the edited application deployed", engine.applyCount())
	}
}

// A render that is unstable only sometimes — a date that rolls over at midnight
// is the honest version of it — settles by itself, and must not stay held once
// it has. The hold means "the last render at this revision did not match what is
// stored", so a plan that does match clears it whether or not anything is
// deployed.
func TestAPlanThatMatchesClearsTheHold(t *testing.T) {
	// Out of sync, still out of sync after the apply (held), then a render that
	// matches, then out of sync again — which must deploy rather than refuse.
	engine := &fakeEngine{plans: []*charts.Plan{outOfSync(), outOfSync(), synced(), outOfSync(), synced()}}
	r := newTest(t, []application.Spec{spec("edge", true)}, engine, nil)

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("first Sync = %v, want nil", err)
	}
	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync over a settled plan = %v, want nil", err)
	}
	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync after the render settled = %v, want the hold cleared", err)
	}
	if engine.applyCount() != 2 {
		t.Errorf("applied %d times, want the second deploy to have gone ahead", engine.applyCount())
	}
}

// ------------------------------ a refusal that never clears (#108)

// refusingSweep is the case that used to run for ever: a release whose departed
// resources Swarm will refuse on this reconcile and on every one after it —
// a network another stack is still attached to gets swarmkit's
// FailedPrecondition, rendered as HTTP 400, permanently.
func refusingSweep() *driftBackend {
	b := resourceSweepBackend()
	b.resourceRemoveErr = map[string]error{
		"config:c-old":   errors.New("config is in use by service other_web"),
		"secret:k-old":   errors.New("secret is in use by service other_web"),
		"network:n-gone": errors.New("network n-gone is in use by service other_web"),
	}
	return b
}

// Retrying for ever is not the same as costing nothing. Each pass was a full
// observe with a history walk, an apply that deploys nothing, a warning, a
// PruneFailed event and an application reporting out of sync — for ever, since
// nothing about the evidence ever changes and the history is never trimmed past
// the revision that proves the claim.
func TestAResourceThatWillNeverGoStopsBeingRetried(t *testing.T) {
	restore := maxPruneAttempts
	maxPruneAttempts = 2
	t.Cleanup(func() { maxPruneAttempts = restore })

	backend := refusingSweep()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	for i := range 4 {
		if err := r.Sync(context.Background(), "edge"); err != nil {
			t.Fatalf("Sync %d = %v, want nil — a refusal is not a failed sync", i+1, err)
		}
	}

	// Three resources, two attempts each, and then nothing.
	if got := len(backend.attempted()); got != 6 {
		t.Errorf("attempted %d removals over four syncs (%v), want 6 — two passes each and then no more",
			got, backend.attempted())
	}
	// The apply gate is opened by something to delete, so it closes with them.
	if got := engine.applyCount(); got != 2 {
		t.Errorf("applied %d times, want 2 — a retained resource must not keep deploying an unchanged plan", got)
	}
}

// The consequence an operator sees. A resource this controller has stopped
// trying to delete is not "the next sync puts this right", and reporting it as
// drift folds through to SyncOutOfSync — which is why the application could
// never report Synced again however healthy it was.
func TestAnApplicationIsSyncedOnceItHasGivenUpOnAResource(t *testing.T) {
	restore := maxPruneAttempts
	maxPruneAttempts = 2
	t.Cleanup(func() { maxPruneAttempts = restore })

	backend := refusingSweep()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Fatalf("state after the first refusal = %q, want out of sync — it is still going to try", view.Status.Sync.State)
	}

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	view, _ = r.View("edge")
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("state = %q, want synced — nothing here is waiting on a sync any more", view.Status.Sync.State)
	}
	for _, rel := range view.Status.Releases {
		if rel.Drift != nil && len(rel.Drift.Resources) > 0 {
			t.Errorf("release %q still reports %v as drift a sync will remove", rel.Name, rel.Drift.Resources)
		}
	}
}

// Given up on is not forgotten. There is nowhere else for this to be said — the
// status reports what a sync will do, and this is no longer one of those things
// — so it is warned about on every pass that still finds it, the same choice
// appset.Loop makes for a held prune.
func TestARetainedResourceIsWarnedAboutOnEveryPass(t *testing.T) {
	restore := maxPruneAttempts
	maxPruneAttempts = 1
	t.Cleanup(func() { maxPruneAttempts = restore })

	var log bytes.Buffer
	backend := refusingSweep()
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := New([]application.Spec{sweepingSpec("edge", application.DriftManifest)}, Options{
		Fetcher:   &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{backend: backend},
		NewEngine: func(charts.Backend) Engine { return engine },
		Log:       slog.New(slog.NewTextHandler(&log, nil)),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})

	for range 3 {
		if err := r.Sync(context.Background(), "edge"); err != nil {
			t.Fatalf("Sync = %v, want nil", err)
		}
	}

	if n := strings.Count(log.String(), "will not be tried again"); n < 2 {
		t.Errorf("warned %d times over the two passes after giving up, want one per pass: %s", n, log.String())
	}
	if !strings.Contains(log.String(), "network whoami_gone") {
		t.Errorf("the warning does not name the resource to go and look at: %s", log.String())
	}
}

// The count is per resource and is cleared by a removal that works, so a slow
// drain that took three passes does not leave a name pre-condemned if it ever
// comes back.
func TestASuccessfulRemovalClearsTheFailureCount(t *testing.T) {
	restore := maxPruneAttempts
	maxPruneAttempts = 2
	t.Cleanup(func() { maxPruneAttempts = restore })

	backend := resourceSweepBackend()
	// Refused once, as a config whose service is still draining is, and then
	// allowed — the ordinary case the retry exists for.
	backend.resourceRemoveErr = map[string]error{"config:c-old": errors.New("config is in use")}
	engine := &fakeEngine{plans: []*charts.Plan{synced()}, history: sweepHistory("edge", "had-a-sidecar")}
	r := newTestWith(t, []application.Spec{sweepingSpec("edge", application.DriftManifest)}, engine, nil,
		fakeRegistry{backend: backend})

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	backend.mu.Lock()
	backend.resourceRemoveErr = nil
	backend.mu.Unlock()

	if err := r.Sync(context.Background(), "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if !slices.Contains(backend.prunedResources(), "config:c-old") {
		t.Errorf("pruned %v, want the config gone on the pass that was allowed", backend.prunedResources())
	}
	view, _ := r.View("edge")
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("state = %q, want synced once everything went", view.Status.Sync.State)
	}
}

// What a pruned service was mounting, which is reported and never deleted (#75).
//
// The loop had never run: every fixture in this file builds services without
// mounts, so swapping mount.TypeVolume for mount.TypeTmpfs here changed nothing
// the suite could see. What that silence costs is in the warning's own comment —
// naming the volumes left behind is the difference between a cleanup an operator
// can finish and one they believe is complete, and a filter matching the wrong
// type names none of them while still logging nothing at all.
func TestMountedVolumesNamesOnlyTheNamedVolumes(t *testing.T) {
	svc := swarm.Service{Spec: swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{
		ContainerSpec: &swarm.ContainerSpec{Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: "uploads", Target: "/srv/uploads"},
			// A bind is a path on the node, not a resource anything could
			// delete, and reporting one as a leftover volume sends an operator
			// looking for something that was never there.
			{Type: mount.TypeBind, Source: "/etc/hosts", Target: "/etc/hosts"},
			{Type: mount.TypeTmpfs, Target: "/tmp"},
			// An anonymous volume: Swarm names it, the manifest does not, and
			// there is no name to report.
			{Type: mount.TypeVolume, Target: "/cache"},
			{Type: mount.TypeVolume, Source: "data", Target: "/var/lib/data"},
		}},
	}}}

	// Sorted, because the report is read by a human and the mount order is the
	// manifest's.
	if got, want := mountedVolumes(svc), []string{"data", "uploads"}; !slices.Equal(got, want) {
		t.Errorf("mountedVolumes = %v, want %v", got, want)
	}
}

// A service that is not a container service at all — a plugin or a network
// attachment — has no mounts to read, and reading them panicked the controller
// once already.
func TestMountedVolumesToleratesAServiceWithNoContainerSpec(t *testing.T) {
	if got := mountedVolumes(swarm.Service{}); got != nil {
		t.Errorf("mountedVolumes = %v, want nil", got)
	}
}

// The runtime half of the floor. An app set is reconciled from git, so
// syncPolicy.interval is chosen by whoever can commit to that repository, and
// `interval: 1ms` made one application monopolise the reconciler and starve
// every other one.
//
// Clamped and not refused: refusing would stop the whole file from being
// applied. `validate` is where it is refused, with somebody present to fix it.
func TestAnIntervalBelowTheFloorIsClamped(t *testing.T) {
	r := newTest(t, nil, &fakeEngine{}, nil)

	fast := spec("edge", true)
	fast.SyncPolicy.Interval = application.Duration(time.Millisecond)
	if got := r.intervalFor(fast); got != application.MinInterval {
		t.Errorf("intervalFor(1ms) = %s, want the %s floor", got, application.MinInterval)
	}

	// Above the floor is left exactly as declared: this is a floor, not a
	// rounding.
	slow := spec("edge", true)
	slow.SyncPolicy.Interval = application.Duration(90 * time.Second)
	if got := r.intervalFor(slow); got != 90*time.Second {
		t.Errorf("intervalFor(90s) = %s, want it untouched", got)
	}

	// And the controller-wide interval is not floored, because it is not the
	// untrusted one — it is set by whoever runs the controller.
	r.interval = time.Millisecond
	if got := r.intervalFor(spec("edge", true)); got != time.Millisecond {
		t.Errorf("intervalFor(no policy) = %s, want the controller-wide interval untouched", got)
	}
}
