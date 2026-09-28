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

// The case in #80, for networks. A network dropped from a template is removed;
// the one the chart still declares is not.
//
// It covered configs and secrets too until #99 refused the only key that let a
// chart declare either. A secret is still out of range — an `external:` reference
// is not a declaration — and a config is back in it, covered by
// TestAShippedConfigIsDeployedRotatedAndPruned below. See chartFilesWithPrunables.
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
	repo := gitRepo(t, chartFilesWithPrunables(release, true))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, sweepingApp("edge", repo, application.DriftManifest))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	// Everything the chart declared is there, so what follows is a removal
	// rather than something that was never created.
	if got, want := stackNetworkNames(t, cli, release), []string{release + "_drop", release + "_keep"}; !slices.Equal(got, want) {
		t.Fatalf("networks = %v, want %v before the drop", got, want)
	}
	kept := serviceOf(t, cli, release+"_app").ID
	records := releaseRecordCount(t, cli, release)

	commitChange(t, repo, chartFilesWithPrunables(release, false))

	wantNetworks := []string{release + "_keep"}
	syncUntilConverged(t, rec, "edge", func() bool {
		return slices.Equal(stackNetworkNames(t, cli, release), wantNetworks)
	})

	// The half the chart still declares survives. A sweep that deleted
	// everything under the namespace would pass without this.
	if got := stackNetworkNames(t, cli, release); !slices.Equal(got, wantNetworks) {
		t.Errorf("networks = %v, want only the one still declared", got)
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
//
// A fixture that declares no config makes this the sharp case: declaredNames
// reports none for the release, so every namespace-labelled config on it is
// something the sweep sees as undeclared, and the ownership check is the only
// thing between that and deleting all of them.
func TestAConfigThisControllerNeverDeclaredIsNeverPruned(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-res-stranger"
	repo := gitRepo(t, chartFilesWithPrunables(release, true))
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
	commitChange(t, repo, chartFilesWithPrunables(release, false))
	syncUntilConverged(t, rec, "edge", func() bool {
		return slices.Equal(stackNetworkNames(t, cli, release), []string{release + "_keep"})
	})

	if got := stackConfigNames(t, cli, release); !slices.Contains(got, stranger) {
		t.Errorf("configs = %v, want the hand-made %q left alone — it was never proved ours", got, stranger)
	}
}

// A chart's own configs, end to end: the content comes from the chart — one from
// a file it ships, one from a value — reaches the swarm as those bytes, and a
// config superseded by new content is removed by the sweep.
//
// The rotation is #80's config half, which #99 took away by leaving a chart no
// way to own a config. It is the shape a chart actually retires one in: Swarm
// will not change a config's data, so new content arrives under a new name, and
// the old name stays behind — still carrying the release's namespace label —
// until something proves it is this application's and deletes it. The proof is
// the stored revision that declared it, and that revision only converts with the
// files it was deployed with, so this is also the test that a stored revision's
// files reach the sweep.
//
// One install covers deploy, rotation and prune, for the budget reason the test
// above gives.
func TestAShippedConfigIsDeployedRotatedAndPruned(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-res-rotate"
	repo := gitRepo(t, chartFilesWithAShippedConfig(release, 1, "listen 8080;\n"))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, sweepingApp("edge", repo, application.DriftManifest))
	if err := rec.SyncNow(context.Background(), "edge"); err != nil {
		t.Fatalf("SyncNow = %v, want the chart's own configs deployed", err)
	}
	waitForRunning(t, cli, release, 1)

	site, operator := release+"_site-1", release+"_operator"
	if got, want := stackConfigNames(t, cli, release), []string{operator, site}; !slices.Equal(got, want) {
		t.Fatalf("configs = %v, want %v", got, want)
	}
	if got := configData(t, cli, site); got != "listen 8080;\n" {
		t.Errorf("%s holds %q, want the chart's files/site.conf", site, got)
	}
	if got := configData(t, cli, operator); got != "from a value\n" {
		t.Errorf("%s holds %q, want the value it names", operator, got)
	}

	commitChange(t, repo, chartFilesWithAShippedConfig(release, 2, "listen 9090;\n"))

	rotated := release + "_site-2"
	want := []string{operator, rotated}
	syncUntilConverged(t, rec, "edge", func() bool {
		return slices.Equal(stackConfigNames(t, cli, release), want)
	})

	if got := configData(t, cli, rotated); got != "listen 9090;\n" {
		t.Errorf("%s holds %q, want the new content", rotated, got)
	}
	var mounted []string
	for _, ref := range serviceOf(t, cli, release+"_app").Spec.TaskTemplate.ContainerSpec.Configs {
		mounted = append(mounted, ref.ConfigName)
	}
	slices.Sort(mounted)
	if !slices.Equal(mounted, want) {
		t.Errorf("the service mounts %v, want %v", mounted, want)
	}
}
