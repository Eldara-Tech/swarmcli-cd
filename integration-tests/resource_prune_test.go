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

// The case in #80, for the three kinds #75 left open. A network, a config and a
// secret dropped from a template are removed; the ones the chart still declares
// are not.
//
// Deliberately the default drift mode, for the reason the service sweep is:
// dropping something from a template is git moving, not the swarm moving, and a
// feature hung off driftDetection: live would be a silent no-op for every
// application running the default.
//
// One install covers all of it. The real-swarm job is already most of the way
// through its budget, and what needs a real daemon here is the drain: the
// dropped network carries the dropped service's tasks, and Swarm will not remove
// a network anything is still attached to. No fake reproduces that, and it is
// why this syncs to convergence rather than asserting after one pass.
func TestResourcesDroppedFromATemplateArePruned(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-res-prune"
	dir := t.TempDir()
	repo := gitRepo(t, chartFilesWithPrunables(t, release, dir, true))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, sweepingApp("edge", repo, application.DriftManifest))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	// Everything the chart declared is there, so what follows is a removal
	// rather than something that was never created.
	if got, want := stackConfigNames(t, cli, release), []string{release + "_drop", release + "_keep"}; !slices.Equal(got, want) {
		t.Fatalf("configs = %v, want %v before the drop", got, want)
	}
	if got, want := stackSecretNames(t, cli, release), []string{release + "_drop"}; !slices.Equal(got, want) {
		t.Fatalf("secrets = %v, want %v before the drop", got, want)
	}
	kept := serviceOf(t, cli, release+"_app").ID
	records := releaseRecordCount(t, cli, release)

	commitChange(t, repo, chartFilesWithPrunables(t, release, dir, false))

	wantConfigs := []string{release + "_keep"}
	wantNetworks := []string{release + "_keep"}
	syncUntilConverged(t, rec, "edge", func() bool {
		return slices.Equal(stackConfigNames(t, cli, release), wantConfigs) &&
			len(stackSecretNames(t, cli, release)) == 0 &&
			slices.Equal(stackNetworkNames(t, cli, release), wantNetworks)
	})

	// The half the chart still declares survives. A sweep that deleted
	// everything under the namespace would pass without this.
	if got := stackConfigNames(t, cli, release); !slices.Equal(got, wantConfigs) {
		t.Errorf("configs = %v, want only the one still declared", got)
	}
	if got := stackNetworkNames(t, cli, release); !slices.Equal(got, wantNetworks) {
		t.Errorf("networks = %v, want only the one still declared", got)
	}
	if got := stackSecretNames(t, cli, release); len(got) != 0 {
		t.Errorf("secrets = %v, want the dropped one gone", got)
	}

	// The service sweep still works, and the surviving service keeps its id — a
	// count would not tell a prune from a redeploy.
	if want := []string{release + "_app"}; !slices.Equal(serviceNamesOf(t, cli, release), want) {
		t.Errorf("services = %v, want %v", serviceNamesOf(t, cli, release), want)
	}
	if got := serviceOf(t, cli, release+"_app").ID; got != kept {
		t.Errorf("the surviving service was recreated: id %q, want %q", got, kept)
	}

	// The release history is the evidence the sweep proves ownership with, so a
	// sweep that consumed it would work once and never again.
	if got := releaseRecordCount(t, cli, release); got <= records {
		t.Errorf("release records = %d, want more than the %d before an upgrade — history intact", got, records)
	}
}

// The #62 lesson, one kind further out. A config carrying the stack's namespace
// label that no revision of ours ever declared is not ours to delete, whatever
// the label says — the label says where it lives, not who put it there.
func TestAConfigThisControllerNeverDeclaredIsNeverPruned(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-res-stranger"
	dir := t.TempDir()
	repo := gitRepo(t, chartFilesWithPrunables(t, release, dir, true))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, sweepingApp("edge", repo, application.DriftManifest))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	stranger := release + "_stranger"
	createConfigByHand(t, cli, release, stranger)

	// Drop the extras, so the sweep runs and has something it may legitimately
	// delete beside the one it may not.
	commitChange(t, repo, chartFilesWithPrunables(t, release, dir, false))
	syncUntilConverged(t, rec, "edge", func() bool {
		return len(stackSecretNames(t, cli, release)) == 0
	})

	if got := stackConfigNames(t, cli, release); !slices.Contains(got, stranger) {
		t.Errorf("configs = %v, want the hand-made %q left alone — it was never proved ours", got, stranger)
	}
}
