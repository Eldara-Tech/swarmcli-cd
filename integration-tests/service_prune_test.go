// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"slices"
	"testing"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// The case in #75, in the drift mode every deployment runs.
//
// A service dropped from a chart's template used to run for ever: applying
// creates and updates and deletes nothing, the release is still declared so it
// is not orphaned, and every subsequent reconcile reported the release unchanged
// because the rendered manifest genuinely did match the last applied one.
//
// Deliberately manifest mode. Removing a service from a template is git moving,
// not the swarm moving, and a fix that only worked under driftDetection: live
// would be a silent no-op for the default configuration. This is the test that
// would fail if the feature were quietly hung off the live comparison.
//
// The assertions are on identity rather than on counts: a stack deleted and
// reinstalled has the same number of services under the same names, and only the
// ids say which of the two happened.
func TestAServiceDroppedFromATemplateIsPruned(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-svc-prune"
	repo := gitRepo(t, twoServiceChartFiles(release, true))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, sweepingApp("edge", repo, application.DriftManifest))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)
	kept := serviceOf(t, cli, release+"_app").ID

	commitChange(t, repo, twoServiceChartFiles(release, false))
	if err := rec.Sync(ctx, "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if want := []string{release + "_app"}; !slices.Equal(serviceNamesOf(t, cli, release), want) {
		t.Errorf("services = %v, want %v", serviceNamesOf(t, cli, release), want)
	}
	if got := serviceOf(t, cli, release+"_app").ID; got != kept {
		t.Errorf("the surviving service was recreated: id %q, want %q", got, kept)
	}

	// The volume the pruned service mounted is reported and never deleted.
	// pruneVolumes means "when a whole release goes, its data goes with it";
	// a service leaving a template is a far more frequent event, and nothing
	// here can prove another stack does not mount the same volume.
	if got := stackVolumeCount(t, cli, release); got != 1 {
		t.Errorf("%d volumes, want the pruned service's volume left in place", got)
	}

	// The release is upgraded, not reinstalled, so its history is intact.
	history, err := rec.History(ctx, "edge")
	if err != nil {
		t.Fatalf("History = %v, want nil", err)
	}
	if len(history.Releases) != 1 || len(history.Releases[0].Revisions) != 2 {
		t.Errorf("history = %+v, want one release at two revisions", history.Releases)
	}
}

// The pre-#75 behaviour, asserted so it cannot regress into a deletion nobody
// enabled. Live mode, because that is where an unexpected service is reported at
// all — with pruneServices off, no proof is computed and nothing is marked.
func TestAServiceDroppedFromATemplateSurvivesWithoutPruneServices(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-svc-prune-off"
	repo := gitRepo(t, twoServiceChartFiles(release, true))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("edge", repo, true))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	commitChange(t, repo, twoServiceChartFiles(release, false))
	if err := rec.Sync(ctx, "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	// Twice: the drop lands on the first pass, and the release is only settled
	// enough to be compared against the swarm on the second.
	if err := rec.Sync(ctx, "edge"); err != nil {
		t.Fatalf("second Sync = %v, want nil", err)
	}

	names := serviceNamesOf(t, cli, release)
	if !slices.Contains(names, release+"_sidecar") {
		t.Fatalf("services = %v, want the sidecar still running", names)
	}

	d := driftOf(t, rec, "edge")
	if d == nil || d.State != application.DriftStateDetected {
		t.Fatalf("drift = %+v, want it reported as detected", d)
	}
	for _, svc := range d.Services {
		if svc.Name != release+"_sidecar" {
			continue
		}
		if svc.Reason != application.DriftUnexpected {
			t.Errorf("reason = %q, want unexpected", svc.Reason)
		}
		if svc.Orphaned {
			t.Error("orphaned is set on an application that never asked for the proof")
		}
	}
}

// The #62 lesson made executable, one scope down.
//
// A stack is a name prefix plus a com.docker.stack.namespace label and nothing
// else — no /stacks endpoint, no owner references — and anything can set that
// label. A service an operator attached to the stack by hand is indistinguishable
// from one this controller installed, on the label alone. It is reported, and it
// is never deleted, because no revision of ours ever declared it.
func TestAServiceThisControllerNeverDeclaredIsNeverPruned(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-svc-prune-stranger"
	repo := gitRepo(t, twoServiceChartFiles(release, false))
	t.Cleanup(func() {
		removeStack(t, release)
		removeVolumes(t, cli, release)
	})

	rec := reconciler(t, sweepingApp("edge", repo, application.DriftLive))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	stranger := release + "_stranger"
	createServiceByHand(t, cli, release, stranger)

	if err := rec.Sync(ctx, "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	if names := serviceNamesOf(t, cli, release); !slices.Contains(names, stranger) {
		t.Fatalf("services = %v, want the hand-made service still running", names)
	}

	d := driftOf(t, rec, "edge")
	if d == nil || d.State != application.DriftStateDetected {
		t.Fatalf("drift = %+v, want it reported", d)
	}
	for _, svc := range d.Services {
		if svc.Name == stranger && svc.Orphaned {
			t.Error("orphaned is set on a service no revision of ours declared — the label is not evidence")
		}
	}
}

// The case a cheaper design could not reach, and the reason the issue says these
// services run *for ever*.
//
// Diffing the last applied manifest against the freshly rendered one names the
// departed service at the instant of the drop and costs nothing. But once that
// drop is applied, the recorded manifest no longer mentions the service either:
// the diff is empty from then on, and every service that leaked before the
// feature was switched on stays leaked, with `docker service rm` the only
// remedy. Proving ownership from the release's stored revisions instead is what
// makes a leftover recoverable however long ago it was left.
//
// So: drop the service with the sweep off, then turn it on — which is also what
// a controller that was down for the commit sees when it comes back.
func TestALeftoverServiceIsPrunedOnceTheSweepIsTurnedOn(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-svc-prune-backfill"
	repo := gitRepo(t, twoServiceChartFiles(release, true))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	ctx := context.Background()
	before := reconciler(t, releaseApp("edge", repo, true))
	if err := before.SyncNow(ctx, "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)
	kept := serviceOf(t, cli, release+"_app").ID

	commitChange(t, repo, twoServiceChartFiles(release, false))
	if err := before.Sync(ctx, "edge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if names := serviceNamesOf(t, cli, release); !slices.Contains(names, release+"_sidecar") {
		t.Fatalf("services = %v, want the sidecar left behind with the sweep off", names)
	}

	// A second controller over the same swarm and the same repository, with the
	// sweep on. The release is settled now — its recorded manifest matches what
	// git renders — so nothing about the plan says a service ever went.
	after := reconciler(t, sweepingApp("edge", repo, application.DriftManifest))
	if err := after.SyncNow(ctx, "edge"); err != nil {
		t.Fatalf("SyncNow after enabling = %v, want nil", err)
	}

	if want := []string{release + "_app"}; !slices.Equal(serviceNamesOf(t, cli, release), want) {
		t.Errorf("services = %v, want %v", serviceNamesOf(t, cli, release), want)
	}
	if got := serviceOf(t, cli, release+"_app").ID; got != kept {
		t.Errorf("the surviving service was recreated: id %q, want %q", got, kept)
	}
}
