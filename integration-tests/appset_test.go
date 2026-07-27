// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eldara-Tech/swarmcli-cd/appset"
)

// The app set moving is the whole of issue #47: an operator commits a change to
// the applications file and the controller's running set follows, with no
// redeploy and no restart. This is that, against a real swarm — one application
// deployed under set A, then a set B that removes it, retunes the other and adds
// a third.
//
// The removed application's stack must still be running afterwards. That is
// decision D-e: app-of-apps reports an orphan, it does not prune one, and the
// difference is a deployment quietly disappearing because somebody edited a
// file.
func TestAppSetChangeDrivesTheRunningSet(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-appset-leaver"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	dir := t.TempDir()
	publish := appSetPublisher(t, dir)

	// Set A: the application that will leave, deploying for real, plus one that
	// stays and will be retuned. Only the leaver is automated, so this costs one
	// deploy rather than three.
	publish(`applications:
  - name: leaver
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
  - name: keeper
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
`)

	src := appset.NewPath(appset.PathConfig{Dir: dir, Path: "applications.yaml"})
	rec := reconciler(t)
	loop := appset.NewLoop(src, rec, appset.LoopOptions{Mode: "path", Log: testLog()})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("first pass = %v, want nil", err)
	}
	if got := len(rec.Views()); got != 2 {
		t.Fatalf("running set has %d applications, want set A's 2", got)
	}

	// Deploy the leaver, so there is a real stack to be orphaned.
	if err := rec.SyncNow(context.Background(), "leaver"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	// Set B: leaver removed, keeper retuned, joiner added.
	publish(`applications:
  - name: keeper
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      interval: 10m
  - name: joiner
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
`)

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("second pass = %v, want nil", err)
	}

	names := map[string]bool{}
	for _, view := range rec.Views() {
		names[view.Spec.Name] = true
	}
	if len(names) != 2 || !names["keeper"] || !names["joiner"] {
		t.Errorf("running set = %v, want keeper and joiner", names)
	}
	if _, ok := rec.View("leaver"); ok {
		t.Error("the removed application is still being reconciled")
	}

	keeper, ok := rec.View("keeper")
	if !ok {
		t.Fatal("the retuned application left the set")
	}
	if keeper.Spec.SyncPolicy.Interval == 0 {
		t.Error("the retuned application still carries set A's policy")
	}

	// D-e: reported, not pruned. The stack outlives the application.
	if got := runningReplicas(t, cli, release); got != 1 {
		t.Errorf("the removed application's stack has %d running tasks, want 1: an orphan is reported, not deleted", got)
	}

	status := loop.Status()
	if len(status.AppSet.Orphaned) != 1 || status.AppSet.Orphaned[0] != "leaver" {
		t.Errorf("orphaned = %v, want [leaver]", status.AppSet.Orphaned)
	}
	if status.Applications != 2 {
		t.Errorf("status reports %d applications, want the 2 being reconciled", status.Applications)
	}
	if status.AppSet.Error != "" || status.AppSet.Stale {
		t.Errorf("status reports error=%q stale=%v, want a clean load", status.AppSet.Error, status.AppSet.Stale)
	}
}

// A commit that does not validate must leave the running set exactly as it was,
// and say so. This is the failure the whole last-good rule exists for: it is the
// one an operator will actually cause.
func TestBadAppSetLeavesTheRunningSetAlone(t *testing.T) {
	dockerClient(t)
	repo := gitRepo(t, chartFiles("e2e-appset-bad", 1))

	dir := t.TempDir()
	publish := appSetPublisher(t, dir)
	publish(`applications:
  - name: keeper
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
`)

	src := appset.NewPath(appset.PathConfig{Dir: dir, Path: "applications.yaml"})
	rec := reconciler(t)
	loop := appset.NewLoop(src, rec, appset.LoopOptions{Mode: "path", Log: testLog()})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("first pass = %v, want nil", err)
	}

	publish("applications:\n  - name: keeper\n    sauce: nonsense\n")
	if err := loop.Once(context.Background()); err == nil {
		t.Fatal("second pass = nil, want the validation error")
	}

	if got := len(rec.Views()); got != 1 {
		t.Errorf("running set has %d applications, want the last-good 1", got)
	}
	status := loop.Status()
	if !status.AppSet.Stale {
		t.Error("stale is not set, but what is running is a last-good set")
	}
	if status.AppSet.Error == "" {
		t.Error("the refusal was not reported")
	}
}

// appSetPublisher writes the app-set file the way the path mode requires:
// atomically, so a reader never sees half of it.
func appSetPublisher(t *testing.T, dir string) func(string) {
	t.Helper()
	return func(content string) {
		t.Helper()
		tmp := filepath.Join(dir, ".applications.yaml.tmp")
		if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "applications.yaml")); err != nil {
			t.Fatal(err)
		}
	}
}
