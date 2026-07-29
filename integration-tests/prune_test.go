// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/appset"
	"github.com/Eldara-Tech/swarmcli-cd/prune"
)

// pruneUntilConverged runs the loop until done holds.
//
// Prune's contract is convergence across sweeps, not success in a single pass.
// RemoveStack deletes services and then the networks they were using with no
// wait between the two, so an overlay network whose tasks are still tearing
// down can refuse removal; the design absorbs that by rediscovering the release
// from its owner stamp and retrying on the next sweep. Asserting that one pass
// suffices would be asserting something the controller does not promise, and
// would be flaky for a reason that is not a defect.
func pruneUntilConverged(t *testing.T, loop *appset.Loop, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for pass := 1; ; pass++ {
		// Logged per pass, not only on the timeout. A sweep that deletes the
		// resources and still reports a failure converges — so the assertions
		// afterwards pass — while leaving nothing in the status to say what
		// went wrong, and that is exactly the case worth seeing.
		if err := loop.Once(context.Background()); err != nil {
			t.Logf("prune pass %d: %v", pass, err)
		}
		if done() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("prune never converged after %d passes", pass)
		}
		time.Sleep(3 * time.Second)
	}
}

// Prune is the acting half of app-of-apps (#54): where
// TestAppSetChangeDrivesTheRunningSet proves the D-e default — a departed
// application's stack is reported and left running — this proves that with
// prune enabled it actually goes, on a real swarm, and that nothing else does.
//
// The four things that matter, and each is a different way this could be wrong:
// the departed stack's services and networks are gone; its volume is not,
// because that is the one thing nothing can restore; its release history is
// gone, so the release is deleted rather than merely undeployed; and the
// application still in the set is untouched, which is the shared-resource
// guard the namespace filter gives us.
func TestPruneDeletesADepartedApplicationsResources(t *testing.T) {
	cli := dockerClient(t)
	const (
		goneRelease = "e2e-prune-gone"
		keptRelease = "e2e-prune-kept"
	)
	goneRepo := gitRepo(t, chartFilesWithResources(goneRelease, 1))
	keptRepo := gitRepo(t, chartFilesWithResources(keptRelease, 1))
	t.Cleanup(func() {
		removeStack(t, goneRelease)
		removeStack(t, keptRelease)
		removeVolumes(t, cli, goneRelease)
		removeVolumes(t, cli, keptRelease)
	})

	dir := t.TempDir()
	publish := appSetPublisher(t, dir)

	both := `applications:
  - name: gone
    source:
      repoURL: ` + goneRepo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
  - name: kept
    source:
      repoURL: ` + keptRepo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
`
	publish(both)

	src := appset.NewPath(appset.PathConfig{Dir: dir, Path: "applications.yaml"})
	rec := reconciler(t)
	loop := appset.NewLoop(src, rec, appset.LoopOptions{
		Mode: "path", Log: testLog(),
		Pruner: prune.New(prune.Options{ControllerID: controllerID(t), Log: testLog()}),
	})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("first pass = %v, want nil", err)
	}
	for _, app := range []string{"gone", "kept"} {
		if err := rec.SyncNow(context.Background(), app); err != nil {
			t.Fatalf("SyncNow(%q) = %v, want nil", app, err)
		}
	}
	waitForRunning(t, cli, goneRelease, 1)
	waitForRunning(t, cli, keptRelease, 1)

	// The volume only exists once a task has mounted it, so this is also what
	// confirms the fixture is exercising the case at all.
	if got := stackVolumeCount(t, cli, goneRelease); got != 1 {
		t.Fatalf("setup: the departing stack has %d volumes, want 1", got)
	}
	if got := releaseRecordCount(t, cli, goneRelease); got == 0 {
		t.Fatal("setup: the departing release recorded no history to delete")
	}

	// The commit that drops it.
	publish(`applications:
  - name: kept
    source:
      repoURL: ` + keptRepo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
`)

	pruneUntilConverged(t, loop, func() bool {
		return stackServiceCount(t, cli, goneRelease) == 0 &&
			stackNetworkCount(t, cli, goneRelease) == 0 &&
			releaseRecordCount(t, cli, goneRelease) == 0
	})

	// Retained, and deliberately: --prune-volumes is a second opt-in precisely
	// because this is the part no reconcile can put back.
	if got := stackVolumeCount(t, cli, goneRelease); got != 1 {
		t.Errorf("the departed stack has %d volumes, want its 1 retained without --prune-volumes", got)
	}

	// The sibling is untouched. Prune classifies by owner stamp and deletes by
	// stack namespace, so neither step can reach outside the application that
	// actually left.
	if got := runningReplicas(t, cli, keptRelease); got != 1 {
		t.Errorf("the application still in the set has %d running tasks, want 1", got)
	}
	if got := releaseRecordCount(t, cli, keptRelease); got == 0 {
		t.Error("the application still in the set lost its release history")
	}

	status := loop.Status()
	if len(status.AppSet.Pruned) != 1 || status.AppSet.Pruned[0] != "gone" {
		t.Errorf("pruned = %v, want [gone]", status.AppSet.Pruned)
	}
	if len(status.AppSet.Orphaned) != 0 {
		t.Errorf("orphaned = %v, want empty once the resources are gone", status.AppSet.Orphaned)
	}
	if status.AppSet.Error != "" || status.AppSet.Stale {
		t.Errorf("status reports error=%q stale=%v, want a clean pass", status.AppSet.Error, status.AppSet.Stale)
	}
}

// The other half of the same guarantee, and the one that protects every
// existing deployment: with volumes opted in, the volume goes too. Run against
// the same fixture so the only difference is the flag.
func TestPruneVolumesDeletesTheVolumeToo(t *testing.T) {
	cli := dockerClient(t)
	const (
		release       = "e2e-prune-volumes"
		anchorRelease = "e2e-prune-volumes-anchor"
	)
	repo := gitRepo(t, chartFilesWithResources(release, 1))
	anchorRepo := gitRepo(t, chartFiles(anchorRelease, 1))
	t.Cleanup(func() {
		removeStack(t, release)
		removeStack(t, anchorRelease)
		removeVolumes(t, cli, release)
	})

	dir := t.TempDir()
	publish := appSetPublisher(t, dir)
	publish(`applications:
  - name: gone
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
  - name: anchor
    source:
      repoURL: ` + anchorRepo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
`)

	src := appset.NewPath(appset.PathConfig{Dir: dir, Path: "applications.yaml"})
	rec := reconciler(t)
	loop := appset.NewLoop(src, rec, appset.LoopOptions{
		Mode: "path", Log: testLog(),
		Pruner: prune.New(prune.Options{Volumes: true, ControllerID: controllerID(t), Log: testLog()}),
	})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("first pass = %v, want nil", err)
	}
	for _, app := range []string{"gone", "anchor"} {
		if err := rec.SyncNow(context.Background(), app); err != nil {
			t.Fatalf("SyncNow(%q) = %v, want nil", app, err)
		}
	}
	waitForRunning(t, cli, release, 1)
	if got := stackVolumeCount(t, cli, release); got != 1 {
		t.Fatalf("setup: %d volumes, want 1", got)
	}

	// anchor stays, so the desired set is never empty — an empty one prunes
	// nothing by design, and this test would otherwise pass for that reason
	// rather than the one it is about. It has its own repository and its own
	// release, and is reconciled: an anchor sharing this release would be an
	// application still declaring it, which is now precisely the thing that
	// stops a sweep (#62), and an anchor that never reconciled would hold the
	// sweep back instead.
	publish(`applications:
  - name: anchor
    source:
      repoURL: ` + anchorRepo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
`)

	pruneUntilConverged(t, loop, func() bool {
		return stackVolumeCount(t, cli, release) == 0 &&
			stackServiceCount(t, cli, release) == 0
	})
}

// Issue #62, and the case that made prune destructive rather than merely
// surprising. Renaming an application — the application, not the release —
// must hand its stack over rather than delete and reinstall it. Run with
// volumes opted in, because that is the configuration where getting this wrong
// destroys the one thing no reconcile can put back.
//
// Both halves of the fix are exercised in order: the sweep is held while the
// renamed application has not reconciled, and once it has, the sweep runs and
// spares a release that is still stamped for the departed name because an
// application in the set declares it.
func TestRenamingAnApplicationKeepsItsStackAndVolume(t *testing.T) {
	cli := dockerClient(t)
	const (
		release       = "e2e-prune-rename"
		anchorRelease = "e2e-prune-rename-anchor"
	)
	repo := gitRepo(t, chartFilesWithResources(release, 1))
	anchorRepo := gitRepo(t, chartFiles(anchorRelease, 1))
	t.Cleanup(func() {
		removeStack(t, release)
		removeStack(t, anchorRelease)
		removeVolumes(t, cli, release)
	})

	dir := t.TempDir()
	publish := appSetPublisher(t, dir)
	// The only thing that changes between the two publishes is the application
	// name; the source and the release file are byte for byte the same, which
	// is what makes the plan come out unchanged and the stamp go stale.
	set := func(name string) string {
		return `applications:
  - name: ` + name + `
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
      # On, so that this also proves the two sweeps do not interact. A rename
      # changes no manifest, so no service is ever a candidate; if the service
      # sweep were keyed on anything weaker it would delete the very stack this
      # test exists to watch being handed over.
      pruneServices: true
  - name: anchor
    source:
      repoURL: ` + anchorRepo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
`
	}
	publish(set("edge"))

	src := appset.NewPath(appset.PathConfig{Dir: dir, Path: "applications.yaml"})
	rec := reconciler(t)
	loop := appset.NewLoop(src, rec, appset.LoopOptions{
		Mode: "path", Log: testLog(),
		Pruner: prune.New(prune.Options{Volumes: true, ControllerID: controllerID(t), Log: testLog()}),
	})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("first pass = %v, want nil", err)
	}
	for _, app := range []string{"edge", "anchor"} {
		if err := rec.SyncNow(context.Background(), app); err != nil {
			t.Fatalf("SyncNow(%q) = %v, want nil", app, err)
		}
	}
	waitForRunning(t, cli, release, 1)

	// Identity, not count: a stack that was deleted and reinstalled has the
	// same number of services under the same names, and only the ids say which
	// of the two happened.
	before := stackServiceIDs(t, cli, release)
	if len(before) == 0 {
		t.Fatal("setup: the stack has no services to keep")
	}
	if got := stackVolumeCount(t, cli, release); got != 1 {
		t.Fatalf("setup: %d volumes, want 1", got)
	}

	publish(set("edge-eu"))

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("the renaming pass = %v, want nil", err)
	}
	if held := loop.Status().AppSet.PruneHeldBy; len(held) != 1 || held[0] != "edge-eu" {
		t.Errorf("pruneHeldBy = %v, want [edge-eu] on the pass that renamed it", held)
	}

	if err := rec.SyncNow(context.Background(), "edge-eu"); err != nil {
		t.Fatalf("SyncNow(edge-eu) = %v, want nil", err)
	}
	// More than one, because a sweep that spares a release must keep sparing
	// it: nothing re-stamps a release whose plan came out unchanged, so every
	// later pass sees the same stale stamp and must reach the same answer.
	for pass := range 3 {
		if err := loop.Once(context.Background()); err != nil {
			t.Fatalf("sweep pass %d = %v, want nil", pass+1, err)
		}
	}

	if got := stackServiceIDs(t, cli, release); !slices.Equal(got, before) {
		t.Errorf("service ids = %v, want %v; the stack was recreated rather than handed over", got, before)
	}
	if got := len(stackServiceIDs(t, cli, release)); got != len(before) {
		t.Errorf("%d services after the rename, want the original %d; the service sweep took one", got, len(before))
	}
	if got := stackVolumeCount(t, cli, release); got != 1 {
		t.Errorf("%d volumes after the rename, want the original 1", got)
	}
	if got := releaseRecordCount(t, cli, release); got == 0 {
		t.Error("the release history is gone; the rename uninstalled the release")
	}
	if pruned := loop.Status().AppSet.Pruned; len(pruned) != 0 {
		t.Errorf("pruned = %v, want nothing pruned by a rename", pruned)
	}
	if got := runningReplicas(t, cli, anchorRelease); got != 1 {
		t.Errorf("the anchor has %d running tasks, want 1", got)
	}
}
