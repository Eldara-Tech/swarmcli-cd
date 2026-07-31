// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package appset

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/regauth"
)

// fakeReconciler records what the loop asked for, and answers Views with what it
// currently holds — so a test asserts against the same "what is running" the
// loop diffs against, rather than against a script.
//
// It is locked because the loop calls it from its own goroutine while a test
// reads Status from another, which is exactly how the real reconciler is used;
// that one guards the same state with its own mutex.
type fakeReconciler struct {
	mu    sync.Mutex
	apps  []application.Spec
	creds map[string]bool // whether a credential is currently installed
	// unreconciled names applications that have not planned yet, which Views
	// reports as SyncUnknown. Add puts an application in it and reconciled
	// takes it out, mirroring the real reconciler: one that joins the set is
	// unknown until its first plan succeeds, and one the fake was constructed
	// with has been running all along.
	unreconciled map[string]bool
	// releases names what each application declares, as its status reports it
	// once it has planned. Only the tests about prune set it.
	releases map[string][]string

	added, replaced, removed []string
	// ops is every call in the order it arrived, which is the only place the
	// interleaving between kinds can be read from.
	ops []string

	// failAdd names an application whose Add fails, for the best-effort case,
	// and failRemove one whose Remove does — which leaves it in the running set.
	failAdd    string
	failRemove string
}

func newFakeReconciler(apps ...application.Spec) *fakeReconciler {
	return &fakeReconciler{
		apps: apps, creds: map[string]bool{},
		unreconciled: map[string]bool{}, releases: map[string][]string{},
	}
}

func (f *fakeReconciler) Views() []application.View {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]application.View, 0, len(f.apps))
	for _, spec := range f.apps {
		state := application.SyncSynced
		if f.unreconciled[spec.Name] {
			state = application.SyncUnknown
		}
		status := application.Status{Sync: application.Sync{State: state}}
		for _, name := range f.releases[spec.Name] {
			status.Releases = append(status.Releases, application.ReleaseStatus{Name: name})
		}
		out = append(out, application.View{Spec: spec, Status: status})
	}
	return out
}

// reconciled marks an application as having completed its first plan, which is
// what releases the prune gate. holdBack is the reverse, for the state every
// application is in immediately after a controller restart.
func (f *fakeReconciler) reconciled(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.unreconciled, name)
}

// declares records the releases an application reports once it has planned.
func (f *fakeReconciler) declares(app string, releases ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases[app] = releases
}

func (f *fakeReconciler) holdBack(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, name := range names {
		f.unreconciled[name] = true
	}
}

func (f *fakeReconciler) Add(spec application.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if spec.Name == f.failAdd {
		return errors.New("cannot start it")
	}
	f.added = append(f.added, spec.Name)
	f.ops = append(f.ops, "add:"+spec.Name)
	f.apps = append(f.apps, spec)
	// As the real one does: the entry is seeded SyncUnknown and its loop has to
	// fetch and render before that changes.
	f.unreconciled[spec.Name] = true
	return nil
}

func (f *fakeReconciler) Replace(spec application.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.apps {
		if f.apps[i].Name == spec.Name {
			f.apps[i] = spec
			f.replaced = append(f.replaced, spec.Name)
			f.ops = append(f.ops, "replace:"+spec.Name)
			return nil
		}
	}
	return errors.New("no such application")
}

func (f *fakeReconciler) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == f.failRemove {
		// Left in the set, which is what a real Remove that failed leaves behind
		// and what the cache reclaim has to read rather than reading the file.
		return errors.New("cannot stop it")
	}
	i := slices.IndexFunc(f.apps, func(s application.Spec) bool { return s.Name == name })
	if i < 0 {
		return errors.New("no such application")
	}
	f.apps = slices.Delete(f.apps, i, i+1)
	f.removed = append(f.removed, name)
	f.ops = append(f.ops, "remove:"+name)
	return nil
}

func (f *fakeReconciler) SetRegistryAuth(app string, resolver regauth.Resolver) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creds[app] = resolver != nil
}

func (f *fakeReconciler) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.apps))
	for _, spec := range f.apps {
		out = append(out, spec.Name)
	}
	return out
}

// spec returns what the fake currently holds for an application, so a retune can
// be asserted on the spec that was actually swapped in.
func (f *fakeReconciler) spec(name string) application.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.apps, func(s application.Spec) bool { return s.Name == name })
	if i < 0 {
		return application.Spec{}
	}
	return f.apps[i]
}

func (f *fakeReconciler) adds() []string       { return f.calls(&f.added) }
func (f *fakeReconciler) replaces() []string   { return f.calls(&f.replaced) }
func (f *fakeReconciler) removes() []string    { return f.calls(&f.removed) }
func (f *fakeReconciler) operations() []string { return f.calls(&f.ops) }

func (f *fakeReconciler) calls(of *[]string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(*of)
}

// hasCred reports whether a credential is installed for an application, and
// sawCred whether one was ever offered — which is how "cleared" is told from
// "never resolved".
func (f *fakeReconciler) hasCred(app string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creds[app]
}

func (f *fakeReconciler) sawCred(app string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.creds[app]
	return ok
}

func (f *fakeReconciler) setFailAdd(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAdd = name
}

func (f *fakeReconciler) setFailRemove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failRemove = name
}

// noCredentials is the injected credential resolver for tests that are not about
// credentials: every application resolves to none, which is what an application
// declaring no registryAuth gets in production.
func noCredentials(application.Spec) (regauth.Resolver, error) { return nil, nil }

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// testSource is the description a bootstrap would hand in. It is opaque to the
// loop, which reports it and nothing else.
const testSource = "/var/lib/appset (applications.yaml)"

// newLoop builds a loop over a path-mode source seeded with initial, and returns
// it with the fake it drives and the publisher that moves the set.
func newLoop(t *testing.T, initial string, apps ...application.Spec) (*Loop, *fakeReconciler, func(string)) {
	t.Helper()
	m := pathMode(t, initial)
	rec := newFakeReconciler(apps...)
	loop := NewLoop(m.loader, rec, LoopOptions{
		Mode: "path", Source: testSource, Log: discard(), Credentials: noCredentials,
	})
	return loop, rec, m.publish
}

// spec is the application oneApp and twoApps declare, as it comes out of the
// parser — including the drift-detection default, which is what makes a
// re-parsed identical file compare equal.
func spec(name, releaseFile string) application.Spec {
	return application.Spec{
		Name: name,
		Source: application.Source{
			RepoURL:     "https://example.com/infra.git",
			Revision:    "main",
			ReleaseFile: releaseFile,
		},
		DriftDetection: application.DriftManifest,
	}
}

// A set that has not moved must produce no calls at all. The loop runs every few
// minutes for the life of the controller; a diff that fired on every tick would
// restart applications for ever.
func TestOnceOnAnUnchangedSetDoesNothing(t *testing.T) {
	loop, rec, _ := newLoop(t, oneApp, spec("edge", "releases/edge.yaml"))

	for range 2 {
		if err := loop.Once(context.Background()); err != nil {
			t.Fatalf("Once = %v, want nil", err)
		}
	}
	if len(rec.adds())+len(rec.replaces())+len(rec.removes()) != 0 {
		t.Errorf("added=%v replaced=%v removed=%v, want none", rec.adds(), rec.replaces(), rec.removes())
	}
}

func TestOnceAppliesTheDiff(t *testing.T) {
	edge := spec("edge", "releases/edge.yaml")

	t.Run("added", func(t *testing.T) {
		loop, rec, publish := newLoop(t, oneApp, edge)
		publish(twoApps)
		if err := loop.Once(context.Background()); err != nil {
			t.Fatalf("Once = %v, want nil", err)
		}
		if !slices.Equal(rec.adds(), []string{"core"}) {
			t.Errorf("added = %v, want [core]", rec.adds())
		}
		if len(rec.replaces())+len(rec.removes()) != 0 {
			t.Errorf("an addition also replaced %v and removed %v", rec.replaces(), rec.removes())
		}
	})

	t.Run("removed", func(t *testing.T) {
		loop, rec, publish := newLoop(t, twoApps, edge, spec("core", "releases/core.yaml"))
		publish(oneApp)
		if err := loop.Once(context.Background()); err != nil {
			t.Fatalf("Once = %v, want nil", err)
		}
		if !slices.Equal(rec.removes(), []string{"core"}) {
			t.Errorf("removed = %v, want [core]", rec.removes())
		}
		if !slices.Equal(rec.names(), []string{"edge"}) {
			t.Errorf("running set = %v, want [edge]", rec.names())
		}
	})

	t.Run("retuned", func(t *testing.T) {
		loop, rec, publish := newLoop(t, oneApp, edge)
		publish(strings.Replace(oneApp, "      releaseFile: releases/edge.yaml\n",
			"      releaseFile: releases/edge.yaml\n    syncPolicy:\n      automated: true\n", 1))
		if err := loop.Once(context.Background()); err != nil {
			t.Fatalf("Once = %v, want nil", err)
		}
		if !slices.Equal(rec.replaces(), []string{"edge"}) {
			t.Errorf("replaced = %v, want [edge]: a retune keeps the loop and its status", rec.replaces())
		}
		if len(rec.adds())+len(rec.removes()) != 0 {
			t.Error("a retune restarted the application instead of replacing its spec")
		}
		if !rec.spec("edge").SyncPolicy.Automated {
			t.Error("the replaced spec is not the published one")
		}
	})

	// A rename is two applications, not one changed: they report separately,
	// carry separate status, and own separate releases.
	t.Run("renamed", func(t *testing.T) {
		loop, rec, publish := newLoop(t, oneApp, edge)
		publish(strings.Replace(oneApp, "name: edge", "name: perimeter", 1))
		if err := loop.Once(context.Background()); err != nil {
			t.Fatalf("Once = %v, want nil", err)
		}
		if !slices.Equal(rec.adds(), []string{"perimeter"}) || !slices.Equal(rec.removes(), []string{"edge"}) {
			t.Errorf("added=%v removed=%v, want a rename to be both", rec.adds(), rec.removes())
		}
		if len(rec.replaces()) != 0 {
			t.Error("a rename was treated as a replace")
		}
	})
}

// A set that swaps one application for another shrinks before it grows.
func TestApplyRemovesBeforeItAdds(t *testing.T) {
	loop, rec, publish := newLoop(t, oneApp, spec("edge", "releases/edge.yaml"))

	// A set holding only "core": edge leaves as core arrives.
	publish(strings.Replace(twoApps, `  - name: edge
    source:
      repoURL: https://example.com/infra.git
      revision: main
      releaseFile: releases/edge.yaml
`, "", 1))
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}

	if got := rec.operations(); !slices.Equal(got, []string{"remove:edge", "add:core"}) {
		t.Errorf("operations = %v, want the departing application gone before the arriving one starts", got)
	}
}

// One application that cannot be started is no reason to leave a departed one
// reconciling, and no reason to skip the rest of the file.
func TestApplyIsBestEffort(t *testing.T) {
	loop, rec, publish := newLoop(t, oneApp, spec("edge", "releases/edge.yaml"))
	rec.setFailAdd("core")

	publish(twoApps)
	err := loop.Once(context.Background())
	if err == nil {
		t.Fatal("Once = nil, want the failed add reported")
	}
	if !strings.Contains(err.Error(), "core") {
		t.Errorf("error %q does not name the application that failed", err)
	}
	if got := loop.Status().AppSet.Error; !strings.Contains(got, "core") {
		t.Errorf("status error = %q, want the failure reported", got)
	}
	if loop.Status().AppSet.Stale {
		t.Error("stale is set, but the set loaded fine; only applying it failed")
	}

	// Diffing against what is running, not against the last file, is what makes
	// the retry free: the application is still missing, so it is tried again.
	rec.setFailAdd("")
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("second Once = %v, want the retry to succeed", err)
	}
	if !slices.Equal(rec.names(), []string{"edge", "core"}) {
		t.Errorf("running set = %v, want the retried application present", rec.names())
	}
}

func TestInvalidSetIsReportedAndChangesNothing(t *testing.T) {
	loop, rec, publish := newLoop(t, oneApp, spec("edge", "releases/edge.yaml"))
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}

	publish(invalidSets["duplicate name"])
	if err := loop.Once(context.Background()); err == nil {
		t.Fatal("Once = nil, want the validation error")
	}
	if len(rec.adds())+len(rec.replaces())+len(rec.removes()) != 0 {
		t.Error("a set that did not validate reached the reconciler")
	}

	status := loop.Status()
	if !status.AppSet.Stale {
		t.Error("stale is not set, but what is running is a last-good set")
	}
	if !strings.Contains(status.AppSet.Error, "duplicate application name") {
		t.Errorf("status error = %q, want the validation failure", status.AppSet.Error)
	}

	// The fix lands and the report clears.
	publish(twoApps)
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil once the set is valid again", err)
	}
	status = loop.Status()
	if status.AppSet.Error != "" || status.AppSet.Stale {
		t.Errorf("status still reports error=%q stale=%v after a good load", status.AppSet.Error, status.AppSet.Stale)
	}
	if !slices.Equal(rec.adds(), []string{"core"}) {
		t.Errorf("added = %v, want the fixed set applied", rec.adds())
	}
}

// A load that succeeds clears a previous failure even when the set is unchanged:
// being readable again is the news.
func TestRecoveryWithoutAChangeClearsTheError(t *testing.T) {
	loop, _, publish := newLoop(t, oneApp, spec("edge", "releases/edge.yaml"))
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}

	publish(invalidSets["unknown key"])
	if err := loop.Once(context.Background()); err == nil {
		t.Fatal("Once = nil, want the validation error")
	}
	publish(oneApp) // back to exactly what is already running
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if status := loop.Status(); status.AppSet.Error != "" || status.AppSet.Stale {
		t.Errorf("status reports error=%q stale=%v, want both cleared", status.AppSet.Error, status.AppSet.Stale)
	}
}

// A departed application is reported, not deleted: its stack keeps running and
// the name is what a later prune acts on.
func TestOrphansAreReportedAndAdoptedBack(t *testing.T) {
	edge, core := spec("edge", "releases/edge.yaml"), spec("core", "releases/core.yaml")
	loop, _, publish := newLoop(t, twoApps, edge, core)

	publish(oneApp)
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if got := loop.Status().AppSet.Orphaned; !slices.Equal(got, []string{"core"}) {
		t.Errorf("orphaned = %v, want [core]", got)
	}

	publish(twoApps)
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if got := loop.Status().AppSet.Orphaned; len(got) != 0 {
		t.Errorf("orphaned = %v, want empty: the application came back and reconciles its own stack again", got)
	}
}

// An application joining the set brings a credential the startup-built map has
// never seen, and one that stops declaring registryAuth must stop sending one.
func TestCredentialsFollowTheSet(t *testing.T) {
	const withAuth = `applications:
  - name: edge
    registryAuth: registry-creds
    source:
      repoURL: https://example.com/infra.git
      revision: main
      releaseFile: releases/edge.yaml
`
	m := pathMode(t, withAuth)
	rec := newFakeReconciler()
	resolved := map[string]bool{}
	loop := NewLoop(m.loader, rec, LoopOptions{
		Log: discard(),
		Credentials: func(s application.Spec) (regauth.Resolver, error) {
			resolved[s.Name] = true
			if s.RegistryAuth == "" {
				return nil, nil
			}
			return func(string) (string, error) { return "", nil }, nil
		},
	})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if !resolved["edge"] || !rec.hasCred("edge") {
		t.Error("the joining application's credential was never resolved and handed over")
	}

	// It stops declaring one: the credential must be cleared, not left behind.
	m.publish(oneApp)
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if rec.hasCred("edge") {
		t.Error("the credential survived an application that no longer declares registryAuth")
	}
}

// A credential that cannot be resolved fails its own application and nothing
// else — the answer #50 settled on for a destination that will not resolve.
func TestUnresolvableCredentialFailsOnlyItsApplication(t *testing.T) {
	m := pathMode(t, twoApps)
	rec := newFakeReconciler()
	loop := NewLoop(m.loader, rec, LoopOptions{
		Log: discard(),
		Credentials: func(s application.Spec) (regauth.Resolver, error) {
			if s.Name == "core" {
				return nil, errors.New("registryAuth secret \"creds\" is not mounted")
			}
			return nil, nil
		},
	})

	err := loop.Once(context.Background())
	if err == nil {
		t.Fatal("Once = nil, want the credential failure reported")
	}
	if !strings.Contains(err.Error(), "is not mounted") {
		t.Errorf("error %q does not carry the credential failure", err)
	}
	if !slices.Equal(rec.names(), []string{"edge"}) {
		t.Errorf("running set = %v, want the healthy application started anyway", rec.names())
	}
	if rec.sawCred("core") {
		t.Error("a credential was installed for an application whose secret would not resolve")
	}
}

// A controller that has never loaded a set is not stale: there is no last-good
// set for it to be running. Filing the loudest failure this loop has under the
// quietest heading is how a controller reconciling nothing gets mistaken for one
// reconciling something slightly out of date.
func TestAFirstLoadThatFailsIsNotStale(t *testing.T) {
	dir := t.TempDir()
	loop := NewLoop(
		NewPath(PathConfig{Dir: dir, Path: "applications.yaml"}),
		newFakeReconciler(),
		LoopOptions{Mode: "path", Log: discard(), Credentials: noCredentials},
	)

	if err := loop.Once(context.Background()); err == nil {
		t.Fatal("Once = nil, want the read to fail")
	}

	status := loop.Status()
	if status.AppSet.Stale {
		t.Error("stale is set, but nothing has ever loaded and nothing is running")
	}
	if status.AppSet.Error == "" {
		t.Error("no error reported for a first load that failed")
	}
	if status.Applications != 0 {
		t.Errorf("applications = %d, want 0", status.Applications)
	}
	if !status.AppSet.LoadedAt.IsZero() {
		t.Error("loadedAt is set, but no load has ever succeeded")
	}

	// And when the set does turn up — the sidecar finished, the remote came
	// back — the loop picks it up with no help. That is what makes starting
	// without one safe rather than merely quiet.
	if err := os.WriteFile(filepath.Join(dir, "applications.yaml"), []byte(oneApp), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want the set to load once it arrives", err)
	}
	if status := loop.Status(); status.AppSet.Error != "" || status.Applications != 1 {
		t.Errorf("status reports error=%q applications=%d, want a clean load of 1",
			status.AppSet.Error, status.Applications)
	}
}

func TestStatusReportsTheSource(t *testing.T) {
	loop, rec, _ := newLoop(t, twoApps)
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}

	status := loop.Status()
	if status.AppSet.Mode != "path" {
		t.Errorf("mode = %q, want the label the caller configured", status.AppSet.Mode)
	}
	// Passed in, not derived: the bootstrap is the only thing that knows where
	// it pointed the controller, and it is the one anchor git cannot repoint.
	if status.AppSet.Source != testSource {
		t.Errorf("source = %q, want the description the caller configured", status.AppSet.Source)
	}
	if status.AppSet.Revision != "" {
		t.Errorf("revision = %q, want empty: a path source has no commit", status.AppSet.Revision)
	}
	if status.AppSet.LoadedAt.IsZero() {
		t.Error("loadedAt is zero after a successful load")
	}
	if status.Applications != len(rec.names()) || status.Applications != 2 {
		t.Errorf("applications = %d, want the %d being reconciled", status.Applications, len(rec.names()))
	}
}

// The git mode's revision is what the status endpoint reports as the commit the
// running set came from.
func TestStatusReportsTheGitRevision(t *testing.T) {
	m := gitMode(t, oneApp)
	rec := newFakeReconciler()
	loop := NewLoop(m.loader, rec, LoopOptions{Mode: "git", Log: discard(), Credentials: noCredentials})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if got := loop.Status().AppSet.Revision; len(got) != 40 {
		t.Errorf("revision = %q, want the full resolved commit", got)
	}
}

// Run is the production entry point: it ticks, and it stops when it is told to.
func TestRunAppliesChangesUntilCancelled(t *testing.T) {
	m := pathMode(t, oneApp)
	rec := newFakeReconciler(spec("edge", "releases/edge.yaml"))
	loop := NewLoop(m.loader, rec, LoopOptions{
		Interval: time.Millisecond, Log: discard(), Credentials: noCredentials,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	m.publish(twoApps)

	deadline := time.After(5 * time.Second)
	for {
		if len(loop.Status().AppSet.Orphaned) == 0 && loop.Status().Applications == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the published set was not applied by the running loop")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// A load that keeps failing must not stop the loop: the fix an operator is about
// to commit has to be picked up on the next tick.
func TestRunKeepsTickingThroughFailures(t *testing.T) {
	m := pathMode(t, oneApp)
	rec := newFakeReconciler(spec("edge", "releases/edge.yaml"))
	loop := NewLoop(m.loader, rec, LoopOptions{
		Interval: time.Millisecond, Log: discard(), Credentials: noCredentials,
	})

	m.publish(invalidSets["invalid source"])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = loop.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for loop.Status().AppSet.Error == "" {
		select {
		case <-deadline:
			t.Fatal("the failing load was never reported")
		case <-time.After(time.Millisecond):
		}
	}

	m.publish(twoApps)
	for {
		status := loop.Status()
		if status.AppSet.Error == "" && status.Applications == 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the loop did not recover: %+v", status)
		case <-time.After(time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------- prune

type fakePruner struct {
	mu       sync.Mutex
	calls    [][]string
	declared [][]string
	returns  []string
	err      error
}

func (p *fakePruner) Departed(_ context.Context, desired, declared []string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, slices.Clone(desired))
	p.declared = append(p.declared, slices.Clone(declared))
	return p.returns, p.err
}

// lastDeclared is the release names the sweep was told are still held, which is
// what stops it deleting a renamed application's stack.
func (p *fakePruner) lastDeclared() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.declared) == 0 {
		return nil
	}
	return p.declared[len(p.declared)-1]
}

func (p *fakePruner) called() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *fakePruner) lastDesired() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return nil
	}
	return p.calls[len(p.calls)-1]
}

// newPruningLoop is newLoop with a pruner attached, which is the only thing that
// makes the loop destructive.
func newPruningLoop(t *testing.T, initial string, p Pruner, apps ...application.Spec) (*Loop, *fakeReconciler, func(string)) {
	t.Helper()
	m := pathMode(t, initial)
	rec := newFakeReconciler(apps...)
	loop := NewLoop(m.loader, rec, LoopOptions{
		Mode: "path", Source: testSource, Log: discard(), Credentials: noCredentials, Pruner: p,
	})
	return loop, rec, m.publish
}

func TestPrunedApplicationLeavesOrphanedAndJoinsPruned(t *testing.T) {
	p := &fakePruner{returns: []string{"core"}}
	loop, _, publish := newPruningLoop(t, twoApps, p,
		spec("edge", "releases/edge.yaml"), spec("core", "releases/core.yaml"))

	publish(oneApp)
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}

	got := loop.Status().AppSet
	if len(got.Orphaned) != 0 {
		t.Errorf("orphaned = %v, want empty once the resources are gone", got.Orphaned)
	}
	if want := []string{"core"}; !slices.Equal(got.Pruned, want) {
		t.Errorf("pruned = %v, want %v", got.Pruned, want)
	}
	// The desired set the sweep classifies against is what actually loaded.
	if want := []string{"edge"}; !slices.Equal(p.lastDesired(), want) {
		t.Errorf("swept against %v, want %v", p.lastDesired(), want)
	}
}

// The D-e default: with no pruner the departure is reported and the stack is
// left running.
func TestWithoutAPrunerOrphansAreOnlyReported(t *testing.T) {
	loop, _, publish := newPruningLoop(t, twoApps, nil,
		spec("edge", "releases/edge.yaml"), spec("core", "releases/core.yaml"))

	publish(oneApp)
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}

	got := loop.Status().AppSet
	if want := []string{"core"}; !slices.Equal(got.Orphaned, want) {
		t.Errorf("orphaned = %v, want %v", got.Orphaned, want)
	}
	if len(got.Pruned) != 0 {
		t.Errorf("pruned = %v, want nothing deleted without a pruner", got.Pruned)
	}
}

// The first guard. A set that will not parse leaves the last-good one running,
// and nothing may be deleted on the strength of a set nobody could read.
func TestALoadFailureNeverPrunes(t *testing.T) {
	p := &fakePruner{}
	loop, _, publish := newPruningLoop(t, twoApps, p,
		spec("edge", "releases/edge.yaml"), spec("core", "releases/core.yaml"))

	publish(invalidSets["unknown key"])
	if err := loop.Once(t.Context()); err == nil {
		t.Fatal("Once = nil, want the load failure")
	}
	if p.called() != 0 {
		t.Errorf("pruned after a failed load (%d calls); the desired set was never known", p.called())
	}
}

// The second guard. A set that loaded but could not be brought up is a set
// whose running state is not yet the declared one.
func TestAnApplyFailureNeverPrunes(t *testing.T) {
	p := &fakePruner{}
	loop, rec, publish := newPruningLoop(t, oneApp, p, spec("edge", "releases/edge.yaml"))

	rec.setFailAdd("core")
	publish(twoApps)
	if err := loop.Once(t.Context()); err == nil {
		t.Fatal("Once = nil, want the apply failure")
	}
	if p.called() != 0 {
		t.Errorf("pruned after a failed apply (%d calls)", p.called())
	}
}

// A sweep that partly worked reports both halves: what went is recorded, and
// what did not surfaces on the status endpoint.
func TestAPartialSweepRecordsWhatWentAndReportsTheRest(t *testing.T) {
	p := &fakePruner{returns: []string{"core"}, err: errors.New("network still attached")}
	loop, _, publish := newPruningLoop(t, twoApps, p,
		spec("edge", "releases/edge.yaml"), spec("core", "releases/core.yaml"))

	publish(oneApp)
	if err := loop.Once(t.Context()); err == nil {
		t.Fatal("Once = nil, want the sweep failure")
	}

	got := loop.Status().AppSet
	if want := []string{"core"}; !slices.Equal(got.Pruned, want) {
		t.Errorf("pruned = %v, want %v recorded even though the sweep errored", got.Pruned, want)
	}
	if !strings.Contains(got.Error, "network still attached") {
		t.Errorf("status error = %q, want the sweep failure in it", got.Error)
	}
}

// A sweep repeats every pass, so the same application comes back until its
// resources are actually gone. It must not accumulate in the report.
func TestRepeatedPruningDoesNotDuplicateTheReport(t *testing.T) {
	p := &fakePruner{returns: []string{"core"}}
	loop, _, publish := newPruningLoop(t, twoApps, p,
		spec("edge", "releases/edge.yaml"), spec("core", "releases/core.yaml"))

	publish(oneApp)
	for range 3 {
		if err := loop.Once(t.Context()); err != nil {
			t.Fatalf("Once = %v, want nil", err)
		}
	}

	if want := []string{"core"}; !slices.Equal(loop.Status().AppSet.Pruned, want) {
		t.Errorf("pruned = %v, want %v", loop.Status().AppSet.Pruned, want)
	}
}

// The fourth guard, and issue #62. A rename is one departure and one arrival
// naming the same deployed release, and the arrival has not fetched git yet
// when the sweep would run. Sweeping in that pass deletes the stack the rename
// was supposed to hand over — and with pruneVolumes, its data.
func TestARenameIsNotSweptBeforeTheNewNameHasReconciled(t *testing.T) {
	p := &fakePruner{}
	loop, rec, publish := newPruningLoop(t, oneApp, p, spec("edge", "releases/edge.yaml"))

	publish(renamedApp)
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if p.called() != 0 {
		t.Fatalf("swept while edge-eu had not reconciled (%d calls); that deletes the renamed stack", p.called())
	}
	if want := []string{"edge-eu"}; !slices.Equal(loop.Status().AppSet.PruneHeldBy, want) {
		t.Errorf("pruneHeldBy = %v, want %v", loop.Status().AppSet.PruneHeldBy, want)
	}

	// Once it has planned it reports the release it declares — the same one the
	// departed name still owns — and that is what the sweep is told to spare.
	rec.declares("edge-eu", "whoami")
	rec.reconciled("edge-eu")
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if p.called() != 1 {
		t.Fatalf("calls = %d, want 1 once the new name had reconciled", p.called())
	}
	if want := []string{"edge-eu"}; !slices.Equal(p.lastDesired(), want) {
		t.Errorf("swept against %v, want %v", p.lastDesired(), want)
	}
	if want := []string{"whoami"}; !slices.Equal(p.lastDeclared(), want) {
		t.Errorf("declared = %v, want %v; the renamed stack is deleted without it", p.lastDeclared(), want)
	}
	if got := loop.Status().AppSet.PruneHeldBy; len(got) != 0 {
		t.Errorf("pruneHeldBy = %v, want empty once a sweep has run", got)
	}
}

// The declared list is deduplicated and covers every application, because two
// applications may legitimately declare the same release name and the sweep
// only needs to know that somebody holds it.
func TestTheSweepIsToldEveryReleaseStillHeld(t *testing.T) {
	p := &fakePruner{}
	loop, rec, _ := newPruningLoop(t, twoApps, p,
		spec("edge", "releases/edge.yaml"), spec("core", "releases/core.yaml"))
	rec.declares("edge", "whoami", "shared")
	rec.declares("core", "shared", "api")

	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if want := []string{"whoami", "shared", "api"}; !slices.Equal(p.lastDeclared(), want) {
		t.Errorf("declared = %v, want %v", p.lastDeclared(), want)
	}
}

// A restart is the case a remembered-departure design cannot cover: nothing was
// watched leaving, so there is nothing to hold back. Holding on the reconciler's
// own state instead covers it, because after a restart every application is
// unreconciled.
func TestNothingIsSweptUntilEveryApplicationHasReconciled(t *testing.T) {
	p := &fakePruner{}
	loop, rec, _ := newPruningLoop(t, twoApps, p,
		spec("edge", "releases/edge.yaml"), spec("core", "releases/core.yaml"))
	rec.holdBack("edge", "core")

	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if p.called() != 0 {
		t.Fatalf("swept on a controller whose applications had none of them planned (%d calls)", p.called())
	}
	if want := []string{"edge", "core"}; !slices.Equal(loop.Status().AppSet.PruneHeldBy, want) {
		t.Errorf("pruneHeldBy = %v, want %v", loop.Status().AppSet.PruneHeldBy, want)
	}

	// One is not enough: the sweep classifies against the whole set.
	rec.reconciled("edge")
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if p.called() != 0 {
		t.Fatalf("swept with core still unplanned (%d calls)", p.called())
	}

	rec.reconciled("core")
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if p.called() != 1 {
		t.Errorf("calls = %d, want 1 once both had planned", p.called())
	}
}

// The gate is over what the reconciler currently holds, not over what it has
// ever held, so an application that never plans and then leaves the set stops
// holding the sweep instead of disabling prune for good.
func TestAnApplicationLeavingTheSetStopsHoldingPrune(t *testing.T) {
	p := &fakePruner{}
	loop, _, publish := newPruningLoop(t, oneApp, p, spec("edge", "releases/edge.yaml"))

	publish(twoApps)
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if p.called() != 0 {
		t.Fatalf("swept while the newly added core had not reconciled (%d calls)", p.called())
	}

	publish(oneApp)
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if p.called() != 1 {
		t.Errorf("calls = %d, want 1 once the unreconciled application had left", p.called())
	}
}

// An application that was deleted and has since been re-declared is running
// again, reinstalled from git. Reporting it as pruned would say the opposite of
// what is on the swarm.
func TestAReturningApplicationLeavesThePrunedList(t *testing.T) {
	p := &fakePruner{returns: []string{"core"}}
	loop, _, publish := newPruningLoop(t, twoApps, p,
		spec("edge", "releases/edge.yaml"), spec("core", "releases/core.yaml"))

	publish(oneApp)
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if len(loop.Status().AppSet.Pruned) != 1 {
		t.Fatalf("setup: pruned = %v, want core", loop.Status().AppSet.Pruned)
	}

	p.returns = nil
	publish(twoApps)
	if err := loop.Once(t.Context()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}

	if got := loop.Status().AppSet.Pruned; len(got) != 0 {
		t.Errorf("pruned = %v, want empty once the application is back", got)
	}
}

// ------------------------------------------------------------- cache reclaim

// fakeReclaimer records the keep list of every sweep, which is the whole of what
// the loop decides: the sweeper itself owns when a candidate is old enough to
// delete.
type fakeReclaimer struct {
	keeps [][]string
	err   error
}

func (f *fakeReclaimer) Sweep(keep []string) error {
	f.keeps = append(f.keeps, slices.Clone(keep))
	return f.err
}

// The sweep is told what the reconciler holds, not what the file declares. An
// application whose Remove failed is still reconciling and still reading its
// checkout, and a sweep driven by the file would have handed its name over as
// departed while it was.
func TestReclaimIsDrivenByTheRunningSet(t *testing.T) {
	m := pathMode(t, twoApps)
	rec := newFakeReconciler()
	sweeper := &fakeReclaimer{}
	loop := NewLoop(m.loader, rec, LoopOptions{
		Mode: "path", Log: discard(), Credentials: noCredentials, Reclaimer: sweeper,
	})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if len(sweeper.keeps) != 1 || !slices.Equal(sweeper.keeps[0], []string{"edge", "core"}) {
		t.Fatalf("swept keeping %v, want both running applications", sweeper.keeps)
	}

	// One leaves the set: the next sweep stops naming it, and only then can the
	// sweeper start its grace period.
	m.publish(oneApp)
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}
	if len(sweeper.keeps) != 2 || !slices.Equal(sweeper.keeps[1], []string{"edge"}) {
		t.Fatalf("swept keeping %v, want only the application still in the set", sweeper.keeps)
	}
}

// A removal that failed keeps the application in the running set, so its clone
// is kept — even though the file has already stopped declaring it. This is the
// difference between reading the reconciler and reading the file.
func TestReclaimKeepsAnApplicationThatCouldNotBeRemoved(t *testing.T) {
	m := pathMode(t, twoApps)
	rec := newFakeReconciler()
	sweeper := &fakeReclaimer{}
	loop := NewLoop(m.loader, rec, LoopOptions{
		Mode: "path", Log: discard(), Credentials: noCredentials, Reclaimer: sweeper,
	})
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}

	rec.setFailRemove("core")
	m.publish(oneApp)
	if err := loop.Once(context.Background()); err == nil {
		t.Fatal("Once = nil, want the failed removal reported")
	}

	if len(sweeper.keeps) != 2 {
		t.Fatalf("swept %d times, want one sweep per pass", len(sweeper.keeps))
	}
	if last := sweeper.keeps[1]; !slices.Contains(last, "core") {
		t.Errorf("swept keeping %v, want the application still reconciling kept", last)
	}
}

// It is not behind the clean-apply gate the prune sweep is behind. What it
// deletes is a cache rebuilt on the next fetch, and letting the manager's volume
// fill because some other application will not start is the worse failure.
func TestReclaimRunsEvenWhenAnAddFails(t *testing.T) {
	m := pathMode(t, twoApps)
	rec := newFakeReconciler()
	rec.setFailAdd("core")
	sweeper := &fakeReclaimer{}
	loop := NewLoop(m.loader, rec, LoopOptions{
		Mode: "path", Log: discard(), Credentials: noCredentials, Reclaimer: sweeper,
	})

	if err := loop.Once(context.Background()); err == nil {
		t.Fatal("Once = nil, want the failed add reported")
	}
	if len(sweeper.keeps) != 1 {
		t.Errorf("swept %d times, want the reclaim to have run anyway", len(sweeper.keeps))
	}
}

// A sweep that could not delete something is reported rather than swallowed: the
// pass did not do everything it set out to, and the status endpoint says so.
func TestReclaimFailureIsReported(t *testing.T) {
	m := pathMode(t, oneApp)
	loop := NewLoop(m.loader, newFakeReconciler(), LoopOptions{
		Mode: "path", Log: discard(), Credentials: noCredentials,
		Reclaimer: &fakeReclaimer{err: errors.New("permission denied")},
	})

	err := loop.Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Once = %v, want the sweep failure carried out", err)
	}
	if got := loop.Status().AppSet.Error; !strings.Contains(got, "permission denied") {
		t.Errorf("status error = %q, want the sweep failure reported", got)
	}
}
