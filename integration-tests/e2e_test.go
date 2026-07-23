// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// The whole point of the repository, end to end: a git repository the test
// created, an application pointing at it, a full reconcile, and a service
// actually running on a real swarm.
func TestReconcileDeploysAndConverges(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-deploy"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, releaseApp("deploy", repo, true))
	if err := rec.SyncNow(context.Background(), "deploy"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}

	waitForRunning(t, cli, release, 1)

	// And the controller's own view agrees with the swarm: a re-plan after the
	// apply is what turns "deployed" into "observed synced".
	view, ok := rec.View("deploy")
	if !ok {
		t.Fatal("no view for the application after a sync")
	}
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("sync state = %q, want synced", view.Status.Sync.State)
	}
	if view.Status.Health.State != application.HealthHealthy {
		t.Errorf("health = %q, want healthy", view.Status.Health.State)
	}
}

// Drift, in both directions: mutate the repository, watch the state go
// OutOfSync without anything being applied, then sync and watch it converge.
// The application is manual, so the drift is observed but not acted on until the
// explicit sync — which is exactly the transition the state exists to report.
func TestDriftIsDetectedAndSynced(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-drift"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, releaseApp("drift", repo, false))
	ctx := context.Background()

	// A manual application still deploys on an explicit sync — "manual" means
	// "not on a schedule", not "never".
	if err := rec.SyncNow(ctx, "drift"); err != nil {
		t.Fatalf("initial SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	// Move the desired state: two replicas now.
	commitChange(t, repo, chartFiles(release, 2))

	// A plain reconcile of a manual application detects the drift and records
	// it, but must not apply it.
	if err := rec.Sync(ctx, "drift"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	view, _ := rec.View("drift")
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Fatalf("sync state = %q, want out-of-sync", view.Status.Sync.State)
	}
	if got := runningReplicas(t, cli, release); got != 1 {
		t.Errorf("running replicas = %d, want the drift unapplied at 1", got)
	}

	// The explicit sync applies it and converges.
	if err := rec.SyncNow(ctx, "drift"); err != nil {
		t.Fatalf("SyncNow after drift = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	view, _ = rec.View("drift")
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("sync state = %q, want synced after the sync", view.Status.Sync.State)
	}
}

// The destination is not decoration. A seam that is silent when it is wrong
// needs a test that would actually fail if it were ignored — swarmcli#487 — so
// this asserts in both directions: a bad destination deploys nothing and says
// why, and the good destination in the tests above genuinely does deploy.
func TestUnresolvableDestinationDeploysNothing(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-destination"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	app := releaseApp("destination", repo, true)
	// A swarm the local registry cannot resolve. The OSS build resolves exactly
	// one — the swarm it runs in — so any name is unreachable, which is the
	// failure this guards against reaching the daemon at all.
	app.Destination = application.Destination{Swarm: "somewhere-else"}
	rec := reconciler(t, app)

	err := rec.SyncNow(context.Background(), "destination")
	if err == nil {
		t.Fatal("SyncNow = nil, want a failure for an unresolvable destination")
	}
	if !strings.Contains(err.Error(), "somewhere-else") {
		t.Errorf("error %q does not name the destination", err)
	}

	// The direction that would catch a backend ignoring the destination: nothing
	// was deployed. If the resolve were skipped, the release would be running on
	// the local swarm despite naming another.
	if got := runningReplicas(t, cli, release); got != 0 {
		t.Errorf("running replicas = %d, want 0 — the destination was ignored", got)
	}
}
