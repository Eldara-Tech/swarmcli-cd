// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// The test the whole feature stands or falls on.
//
// Nothing was touched, so nothing must be reported. A spec read back from the
// daemon is not the one that was written — replicas defaulted, the image
// resolved to a digest, resources returned as zero structs, labels as an empty
// map, a published port assigned, platforms filled from the image manifest —
// and every one of those is a way a comparison can report drift that is not
// there. On an automated application that is not merely noise: the controller
// would redeploy an untouched service on every tick, forever.
//
// The unit suite asserts the same rules against hand-built specs. Only this can
// say whether the daemon actually behaves the way those specs assume, which is
// the gap the #54 and #62 retrospectives are about.
func TestLiveDriftIsNotReportedOnAnUntouchedStack(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-clean"
	repo := gitRepo(t, richChartFiles(release, 2))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("clean", repo, true))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "clean"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 3) // two app replicas and one sidecar

	// Twice: the first reconcile follows the deploy that wrote the spec, the
	// second sees it only as the daemon returns it.
	for _, pass := range []string{"after the deploy", "on the next reconcile"} {
		if err := rec.Sync(ctx, "clean"); err != nil {
			t.Fatalf("Sync %s = %v, want nil", pass, err)
		}
		d := driftOf(t, rec, "clean")
		if d == nil {
			t.Fatalf("%s: no drift axis at all on a live-mode application", pass)
		}
		if d.State != application.DriftStateNone {
			t.Errorf("%s: drift = %q with %+v, want none — nothing was touched",
				pass, d.State, d.Services)
		}
		view, _ := rec.View("clean")
		if view.Status.Sync.State != application.SyncSynced {
			t.Errorf("%s: sync = %q, want synced", pass, view.Status.Sync.State)
		}
	}
}

// The canonical case from the issue: an operator scales a service by hand.
//
// On a manual application it is reported and left alone, which is what makes the
// report trustworthy — the controller says what it sees before it is allowed to
// act on it.
func TestLiveDriftDetectsAnOutOfBandScale(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-detect"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, liveDriftApp("detect", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "detect"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	three := uint64(3)
	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.Mode.Replicated.Replicas = &three
	})
	waitForRunning(t, cli, release, 3)

	if err := rec.Sync(ctx, "detect"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	d := driftOf(t, rec, "detect")
	if d == nil || d.State != application.DriftStateDetected {
		t.Fatalf("drift = %+v, want detected", d)
	}
	if got := fieldOf(t, d, release+"_app", "replicas"); got.Desired != "1" || got.Live != "3" {
		t.Errorf("replicas drift = %+v, want desired 1 and live 3", got)
	}

	// Folded into the sync axis, so the sync button and anything alerting on it
	// work without knowing this mode exists.
	view, _ := rec.View("detect")
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Errorf("sync = %q, want out-of-sync", view.Status.Sync.State)
	}
	if view.Status.Sync.Summary.Drifted != 1 {
		t.Errorf("summary.drifted = %d, want 1", view.Status.Sync.Summary.Drifted)
	}

	// And nothing was corrected: manual reports, it does not act.
	if got := runningReplicas(t, cli, release); got != 3 {
		t.Errorf("running replicas = %d, want the manual application to have left them at 3", got)
	}
}

// The same drift on an automated application, which is what the mode is for:
// the swarm goes back to what the repository declares without anyone asking.
func TestLiveDriftConvergesAnOutOfBandScale(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-converge"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, liveDriftApp("converge", repo, true))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "converge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	four := uint64(4)
	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.Mode.Replicated.Replicas = &four
	})
	waitForRunning(t, cli, release, 4)

	if err := rec.Sync(ctx, "converge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	waitForRunning(t, cli, release, 1)

	// The confirming re-plan re-compares, so a correction that landed reports
	// itself as settled rather than leaving the application permanently drifted.
	if d := driftOf(t, rec, "converge"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
	view, _ := rec.View("converge")
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("sync = %q, want synced after the correction", view.Status.Sync.State)
	}
}

// The image case, which no unit test can prove.
//
// `docker service update --image` rewrites the spec's image and leaves the stack
// image label naming the tag the last deploy wrote. A comparison built on those
// two labels sees nothing; a converge built on the same assumption writes the
// operator's image straight back. Both halves are only observable against a real
// daemon, because both turn on what it does with an image reference.
func TestLiveDriftConvergesAnOutOfBandImage(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-image"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, liveDriftApp("image", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "image"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Image = "busybox:1.37"
	})

	if err := rec.Sync(ctx, "image"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	got := fieldOf(t, driftOf(t, rec, "image"), release+"_app", "image")
	if got.Desired != "busybox:1.36" {
		t.Errorf("desired image = %q, want what the chart declares", got.Desired)
	}

	// Manual, so correct it explicitly — and the correction must actually take,
	// which is what the prepareUpdate guard exists for.
	if err := rec.SyncNow(ctx, "image"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	svc := serviceOf(t, cli, release+"_app")
	if img := svc.Spec.TaskTemplate.ContainerSpec.Image; !hasTag(img, "busybox:1.36") {
		t.Errorf("running image = %q, want it back on busybox:1.36", img)
	}
	if d := driftOf(t, rec, "image"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// hasTag reports whether a live image reference names tag, ignoring any digest
// the daemon resolved it to.
func hasTag(image, tag string) bool {
	if image == tag {
		return true
	}
	return len(image) > len(tag) && image[:len(tag)] == tag && image[len(tag)] == '@'
}

// A service deleted out of band. The health axis only notices when a release has
// no services left at all, so without this a two-service stack missing one of
// them reports healthy.
func TestLiveDriftDetectsAndRestoresADeletedService(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-missing"
	repo := gitRepo(t, richChartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("missing", repo, true))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "missing"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	sidecar := serviceOf(t, cli, release+"_sidecar")
	if err := cli.ServiceRemove(ctx, sidecar.ID); err != nil {
		t.Fatalf("removing the sidecar out of band: %v", err)
	}

	if err := rec.Sync(ctx, "missing"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	// Reported as missing rather than as a field difference, and only the
	// service that went.
	d := driftOf(t, rec, "missing")
	if d == nil || d.State != application.DriftStateDetected || len(d.Services) != 1 {
		t.Fatalf("drift = %+v, want exactly the departed service", d)
	}
	if d.Services[0].Name != release+"_sidecar" || d.Services[0].Reason != application.DriftMissing {
		t.Errorf("got %+v, want %s_sidecar missing", d.Services[0], release)
	}

	// And an automated application puts it back.
	waitForRunning(t, cli, release, 2)
	if d := driftOf(t, rec, "missing"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// The default mode must be untouched by any of this: it does not look at the
// swarm, so it cannot report or correct an out-of-band change, and it must not
// grow the axis either.
func TestManifestModeIgnoresAnOutOfBandChange(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-manifest"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, releaseApp("manifest", repo, true))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "manifest"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	two := uint64(2)
	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.Mode.Replicated.Replicas = &two
	})
	waitForRunning(t, cli, release, 2)

	if err := rec.Sync(ctx, "manifest"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := rec.View("manifest")
	if view.Status.Drift != nil {
		t.Errorf("drift = %+v, want the axis absent in manifest mode", view.Status.Drift)
	}
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("sync = %q, want synced — manifest mode compares against the last apply", view.Status.Sync.State)
	}
	if got := runningReplicas(t, cli, release); got != 2 {
		t.Errorf("running replicas = %d, want the hand-scaled 2 left alone", got)
	}
}
