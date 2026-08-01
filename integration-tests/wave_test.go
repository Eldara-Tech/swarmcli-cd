// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
	dockerclient "github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/reconcile"
)

// ---------------------------------------------------------------- sync waves
//
// The unit suites prove the grouping against fakes. What only a real swarm can
// settle is whether the thing a barrier waits on — swarm's own account of
// whether a stack has converged — actually gates the next wave, and whether a
// wave that never comes up really does stop the ones after it.

// waveRelease is one entry in a multi-release fixture.
type waveRelease struct {
	release string
	wave    int
	// stack is the chart's templates/stack.yaml. Empty means plainStack.
	stack string
}

// multiReleaseChartFiles writes a release file declaring several releases, each
// with a chart of its own.
//
// It is the piece the harness has never had: chartFiles is the only writer of
// swarmcli-release.yaml in this suite and emits exactly one entry, so before
// waves nothing here could express an ordering at all.
func multiReleaseChartFiles(releases []waveRelease) map[string]string {
	files := map[string]string{}
	doc := "apiVersion: v1\nreleases:\n"
	for _, r := range releases {
		doc += "" +
			"  - name: " + r.release + "\n" +
			"    chart: ./charts/" + r.release + "\n"
		if r.wave != 0 {
			doc += "    wave: " + itoa(r.wave) + "\n"
		}
		stack := r.stack
		if stack == "" {
			stack = plainStack
		}
		files["charts/"+r.release+"/Chart.yaml"] = "apiVersion: v1\nname: " + r.release + "\nversion: 0.1.0\n"
		files["charts/"+r.release+"/values.yaml"] = "{}\n"
		files["charts/"+r.release+"/templates/stack.yaml"] = stack
	}
	files["swarmcli-release.yaml"] = doc
	return files
}

// plainStack is the sleeping busybox the rest of the suite uses: the smallest
// thing that reliably reaches running, with a tag that is always pullable.
const plainStack = `version: "3.9"
services:
  app:
    image: busybox:1.36
    command: ["sleep", "3600"]
    deploy:
      replicas: 1
      labels:
        com.swarmcli.release: {{ .Release.Name }}
`

// stallStack never converges: no node carries the label, so the task stays
// pending forever. Nothing is pulled and nothing is started.
//
// It is deliberately NOT a failing healthcheck, which is the obvious way to
// build this and the wrong one. A healthchecked task sits in `starting` until
// its check first passes, and a check that keeps failing outside start_period
// gets the container killed by swarmkit — so the fixture would read as a
// crash-loop rather than as a stall, which is what richChartFiles steers around
// where it explains its own healthcheck. A task no node can schedule is
// unambiguous, and it costs nothing to tear down.
const stallStack = `version: "3.9"
services:
  app:
    image: busybox:1.36
    command: ["sleep", "3600"]
    deploy:
      replicas: 1
      placement:
        constraints: [node.labels.swarmcli_cd_absent_label == true]
      labels:
        com.swarmcli.release: {{ .Release.Name }}
`

// slowStack converges, but only after a delay, so "did the sync return before
// this was live" has a stable answer.
//
// Two rules make it work and are easy to get wrong. A healthchecked task stays
// in `starting` until the first healthy event, which is the lever — gating the
// check on a file the container creates after a delay keeps it out of `running`
// for exactly that long. And start_period MUST cover the delay: outside it,
// `retries` consecutive failures mark the container unhealthy and swarmkit
// shuts it down, so the fixture would restart-loop instead of coming up slowly.
const slowStack = `version: "3.9"
services:
  app:
    image: busybox:1.36
    command: ["sh", "-c", "sleep 15; touch /tmp/ready; sleep 3600"]
    healthcheck:
      test: ["CMD-SHELL", "test -f /tmp/ready"]
      interval: 2s
      retries: 3
      start_period: 45s
    deploy:
      replicas: 1
      labels:
        com.swarmcli.release: {{ .Release.Name }}
`

// waveApp is releaseApp without its wait policy.
//
// releaseApp hard-codes Wait: true and a three-minute timeout, and both are
// wrong here. wait moves a failure off the wave barrier and onto the per-release
// wait, which is the thing under test; and three minutes is what a stalled wave
// would cost in wall clock. releaseApp itself is untouched — every other test in
// this suite depends on its defaults.
func waveApp(name, repoDir string, timeout time.Duration) application.Spec {
	app := releaseApp(name, repoDir, true)
	app.SyncPolicy.Wait = false
	app.SyncPolicy.Timeout = application.Duration(timeout)
	return app
}

// createdAtOf is when the daemon created a service, which is the ordering
// primitive this suite has never needed before: no test reads it, and nothing
// else exposes the order releases were deployed in.
func createdAtOf(t *testing.T, cli *dockerclient.Client, name string) time.Time {
	t.Helper()
	return serviceOf(t, cli, name).Meta.CreatedAt
}

// releaseOf is driftOf's sibling for a multi-release application.
//
// driftOf asserts there is exactly one release, which every test here breaks, so
// it is left alone rather than widened — the twelve live-drift tests depend on
// that assertion.
func releaseOf(t *testing.T, rec *reconcile.Reconciler, app, release string) application.ReleaseStatus {
	t.Helper()
	view, ok := rec.View(app)
	if !ok {
		t.Fatalf("no such application %q", app)
	}
	for _, r := range view.Status.Releases {
		if r.Name == release {
			return r
		}
	}
	t.Fatalf("release %q not in the application's status", release)
	return application.ReleaseStatus{}
}

// Releases deploy in wave order, and each wave is live before the next starts.
func TestWavesDeployInWaveOrder(t *testing.T) {
	cli := dockerClient(t)
	const first, second, third = "e2e-wave-a", "e2e-wave-b", "e2e-wave-c"

	// Written last-wave-first on purpose: a controller that ignored waves and
	// used the file's order would deploy them in exactly the wrong sequence, so
	// this cannot pass by accident.
	repo := gitRepo(t, multiReleaseChartFiles([]waveRelease{
		{release: third, wave: 2},
		{release: first, wave: 0},
		{release: second, wave: 1},
	}))
	for _, r := range []string{first, second, third} {
		t.Cleanup(func() { removeStack(t, r) })
	}

	rec := reconciler(t, waveApp("waves", repo, 2*time.Minute))
	if err := rec.SyncNow(context.Background(), "waves"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}

	for _, r := range []string{first, second, third} {
		waitForRunning(t, cli, r, 1)
	}

	// The daemon's own record of when each service was created. A barrier ran
	// between each pair, so these are strictly increasing rather than merely
	// close together.
	a := createdAtOf(t, cli, first+"_app")
	b := createdAtOf(t, cli, second+"_app")
	c := createdAtOf(t, cli, third+"_app")
	if !a.Before(b) || !b.Before(c) {
		t.Errorf("created at %s / %s / %s, want wave order", a, b, c)
	}

	// And the status reports the wave, in the order the sync applied them.
	if got := releaseOf(t, rec, "waves", third).Wave; got != 2 {
		t.Errorf("%s reports wave %d, want 2", third, got)
	}
	view, _ := rec.View("waves")
	var names []string
	for _, r := range view.Status.Releases {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != strings.Join([]string{first, second, third}, ",") {
		t.Errorf("status lists %v, want them in wave order", names)
	}
}

// A wave that does not converge stops every wave after it.
//
// This is the acceptance criterion for the feature. The third wave must show no
// service AND no release record: an empty service list alone is weaker, because
// it is also what a deploy that was accepted and then failed looks like. The
// contrast with the second wave — which does have a record, because the engine
// records a revision before it waits — is what makes it proof.
func TestAWaveThatDoesNotConvergeStopsTheWavesAfterIt(t *testing.T) {
	cli := dockerClient(t)
	const first, stuck, never = "e2e-wavefail-a", "e2e-wavefail-b", "e2e-wavefail-c"

	repo := gitRepo(t, multiReleaseChartFiles([]waveRelease{
		{release: first, wave: 0},
		{release: stuck, wave: 1, stack: stallStack},
		{release: never, wave: 2},
	}))
	for _, r := range []string{first, stuck, never} {
		t.Cleanup(func() { removeStack(t, r) })
	}

	// The timeout bounds each wave, so it has to be comfortable for the healthy
	// one that goes first — everything above that is what the stalled wave costs.
	rec := reconciler(t, waveApp("waves", repo, 40*time.Second))
	err := rec.SyncNow(context.Background(), "waves")
	if err == nil {
		t.Fatal("SyncNow = nil, want the wave that never converges to fail the sync")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), stuck) {
		t.Errorf("error = %v, want a timeout naming %q", err, stuck)
	}

	// Wave 0 came up.
	waitForRunning(t, cli, first, 1)

	// Wave 1 was deployed and only then failed to converge, so it has a record.
	if got := releaseRecordCount(t, cli, stuck); got != 1 {
		t.Errorf("%s has %d release records, want 1 — it was deployed, it just never came up", stuck, got)
	}

	// Wave 2 was never reached at all.
	if got := serviceNamesOf(t, cli, never); len(got) != 0 {
		t.Errorf("%s has services %v, want none — it is in the wave after the one that stalled", never, got)
	}
	if got := releaseRecordCount(t, cli, never); got != 0 {
		t.Errorf("%s has %d release records, want 0 — the apply never reached it", never, got)
	}

	// And the application says so rather than only the log.
	view, _ := rec.View("waves")
	if last := view.Status.Sync.LastSync; last == nil || last.Succeeded {
		t.Errorf("lastSync = %+v, want a failure recorded", last)
	}
}

// A release file that declares no wave is one wave: the releases go out together
// and the sync does not block on either.
//
// Two guards in one test. The compatibility rule the whole feature rests on —
// nothing that did not block before starts blocking — and the "within a wave,
// together" property, which is what makes waves worth having over `wait`.
//
// It passes on main as it stands, and that is the point: a compatibility test
// that has only ever run against the new behaviour proves nothing about the old.
func TestASingleWaveNeitherWaitsNorSerialises(t *testing.T) {
	cli := dockerClient(t)
	const slow, fast = "e2e-onewave-slow", "e2e-onewave-fast"

	repo := gitRepo(t, multiReleaseChartFiles([]waveRelease{
		{release: slow, stack: slowStack},
		{release: fast},
	}))
	for _, r := range []string{slow, fast} {
		t.Cleanup(func() { removeStack(t, r) })
	}

	rec := reconciler(t, waveApp("waves", repo, 2*time.Minute))

	start := time.Now()
	if err := rec.SyncNow(context.Background(), "waves"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	elapsed := time.Since(start)

	// The slow release takes ~15s to report healthy. Returning well inside that
	// is what says no barrier fired and no per-release wait was added.
	if elapsed > 12*time.Second {
		t.Errorf("the sync took %s, want it to return before the slow release could be live — one wave is no boundary", elapsed)
	}

	// Both were nonetheless deployed, back to back rather than one after the
	// other's rollout.
	for _, r := range []string{slow, fast} {
		if got := serviceNamesOf(t, cli, r); len(got) == 0 {
			t.Errorf("%s was not deployed", r)
		}
	}
	gap := createdAtOf(t, cli, fast+"_app").Sub(createdAtOf(t, cli, slow+"_app"))
	if gap < 0 {
		gap = -gap
	}
	if gap > 10*time.Second {
		t.Errorf("the two services were created %s apart, want them deployed together within one wave", gap)
	}

	// And they do both come up, so the fixture is measuring a delay rather than
	// a failure.
	waitForRunning(t, cli, slow, 1)
	waitForRunning(t, cli, fast, 1)
}

// Waves order a drift correction too.
//
// The same dependency that makes an install ordered makes a correction ordered:
// putting a migration back before the API that reads it is the whole of why the
// release file declared a wave. Note the application does NOT set wait — the
// barrier holds regardless, which is what makes waves an ordering rather than a
// second name for `wait`.
func TestWavesOrderADriftCorrection(t *testing.T) {
	cli := dockerClient(t)
	const first, second = "e2e-wavedrift-a", "e2e-wavedrift-b"

	repo := gitRepo(t, multiReleaseChartFiles([]waveRelease{
		{release: first, wave: 0},
		{release: second, wave: 1},
	}))
	for _, r := range []string{first, second} {
		t.Cleanup(func() { removeStack(t, r) })
	}

	app := waveApp("waves", repo, 2*time.Minute)
	app.DriftDetection = application.DriftLive
	rec := reconciler(t, app)

	if err := rec.SyncNow(context.Background(), "waves"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, first, 1)
	waitForRunning(t, cli, second, 1)

	// Scale both behind the controller's back, which is drift it can correct.
	for _, r := range []string{first, second} {
		updateOutOfBand(t, cli, r+"_app", func(spec *swarm.ServiceSpec) {
			two := uint64(2)
			spec.Mode.Replicated.Replicas = &two
		})
	}

	if err := rec.SyncNow(context.Background(), "waves"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}

	// Both were put back, and wave 0's correction was written before wave 1's.
	a := serviceOf(t, cli, first+"_app").Meta.UpdatedAt
	b := serviceOf(t, cli, second+"_app").Meta.UpdatedAt
	if !a.Before(b) {
		t.Errorf("corrected at %s then %s, want wave 0 corrected first", a, b)
	}
	waitForRunning(t, cli, first, 1)
	waitForRunning(t, cli, second, 1)
}
