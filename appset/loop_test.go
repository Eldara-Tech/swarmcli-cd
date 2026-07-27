// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package appset

import (
	"context"
	"errors"
	"log/slog"
	"os"
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

	added, replaced, removed []string
	// ops is every call in the order it arrived, which is the only place the
	// interleaving between kinds can be read from.
	ops []string

	// failAdd names an application whose Add fails, for the best-effort case.
	failAdd string
}

func newFakeReconciler(apps ...application.Spec) *fakeReconciler {
	return &fakeReconciler{apps: apps, creds: map[string]bool{}}
}

func (f *fakeReconciler) Views() []application.View {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]application.View, 0, len(f.apps))
	for _, spec := range f.apps {
		out = append(out, application.View{Spec: spec})
	}
	return out
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

// noCredentials is the injected credential resolver for tests that are not about
// credentials: every application resolves to none, which is what an application
// declaring no registryAuth gets in production.
func noCredentials(application.Spec) (regauth.Resolver, error) { return nil, nil }

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newLoop builds a loop over a path-mode source seeded with initial, and returns
// it with the fake it drives and the publisher that moves the set.
func newLoop(t *testing.T, initial string, apps ...application.Spec) (*Loop, *fakeReconciler, func(string)) {
	t.Helper()
	m := pathMode(t, initial)
	rec := newFakeReconciler(apps...)
	loop := NewLoop(m.loader, rec, LoopOptions{Mode: "path", Log: discard(), Credentials: noCredentials})
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

func TestStatusReportsTheSource(t *testing.T) {
	loop, rec, _ := newLoop(t, twoApps)
	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once = %v, want nil", err)
	}

	status := loop.Status()
	if status.AppSet.Mode != "path" {
		t.Errorf("mode = %q, want the label the caller configured", status.AppSet.Mode)
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
