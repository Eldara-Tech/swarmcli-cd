// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

// Package integration exercises the whole controller against a real Docker
// Swarm: a git repository the test creates, an application pointing at it, the
// real moby-client backend, and assertions on what is actually running.
//
// Everything the unit tests fake — the daemon, the clone, the deploy — is real
// here, because the applier cannot be meaningfully tested any other way. It is
// gated behind the `integration` build tag and skips unless the daemon it finds
// is a swarm manager, so `go test ./...` on a laptop without a swarm is a skip
// rather than a failure.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/reconcile"
	"github.com/Eldara-Tech/swarmcli-cd/source"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// branch is the branch every fixture repository commits to. The application's
// revision names it rather than a SHA, so a second commit is picked up on the
// next fetch — which is what makes the drift test a moving target rather than a
// pinned one.
const branch = "main"

// dockerClient connects to the daemon the test environment points at, and skips
// the whole suite when it is not a swarm manager. A worker or a plain daemon
// cannot serve the API the applier drives, and a hard failure there would just
// be noise on a developer's machine.
func dockerClient(t *testing.T) *dockerclient.Client {
	t.Helper()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	info, err := cli.Info(context.Background())
	if err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	if info.Swarm.LocalNodeState != swarm.LocalNodeStateActive || !info.Swarm.ControlAvailable {
		t.Skip("daemon is not a swarm manager; run `docker swarm init` to enable the integration tests")
	}
	return cli
}

// gitRepo writes files into a fresh repository on the default branch and returns
// its absolute path. The git sourcer accepts an absolute path as a repository
// URL, so nothing here touches the network.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: "refs/heads/" + branch},
		Bare:        false,
	})
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	commit(t, repo, dir, files, "initial")
	return dir
}

// commitChange writes files over an existing repository and commits them, so a
// test can move the desired state and watch the controller notice.
func commitChange(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("git open: %v", err)
	}
	commit(t, repo, dir, files, "change")
}

func commit(t *testing.T, repo *gogit.Repository, dir string, files map[string]string, message string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	// A fixed signature: the test controls the clock so nothing here depends on
	// the machine's git identity being configured.
	_, err = wt.Commit(message, &gogit.CommitOptions{
		Author: &object.Signature{Name: "swarmcli-cd tests", Email: "hello@eldara.io", When: time.Unix(1_700_000_000, 0)},
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}
}

// reconciler wires the real controller for the given applications: the real git
// sourcer, the real chart builder, and the local swarm registry — which resolves
// to a real moby-client backend against the daemon the process was started with.
// It is the wiring controller.serve builds, without the HTTP server.
//
// Passing none is how the app-set tests start: an empty reconciler is what the
// set itself then fills.
func reconciler(t *testing.T, apps ...application.Spec) *reconcile.Reconciler {
	t.Helper()
	data := t.TempDir()
	return reconcile.New(apps, reconcile.Options{
		Fetcher:      git.New(filepath.Join(data, "repos"), git.Auth{}),
		Builder:      source.NewBuilder(filepath.Join(data, "charts"), nil),
		Swarms:       swarms.Get(),
		ControllerID: controllerID(t),
		Log:          testLog(),
	})
}

// controllerID gives each test its own controller identity.
//
// These tests share one swarm and leave their release records behind — an
// uninstall deletes them, but the ordinary teardown removes the stack and
// deliberately keeps the history. A prune sweep looks at every release on the
// swarm and deletes the ones stamped for an application its app set does not
// declare, so under a shared identity one test's sweep would delete another
// test's releases. Distinct ids are what production requires of two controllers
// sharing a swarm, and they are what makes these tests independent.
func controllerID(t *testing.T) string {
	t.Helper()
	// Subtests put a slash in the name, which the stamp format forbids.
	return strings.ReplaceAll(t.Name(), "/", "-")
}

// testLog keeps the reconciler's own logging out of the test output, which is
// otherwise dominated by it on a real deploy.
func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// releaseApp is an application whose repository already contains a release file.
func releaseApp(name, repoDir string, automated bool) application.Spec {
	return application.Spec{
		Name: name,
		Source: application.Source{
			RepoURL:     repoDir,
			Revision:    branch,
			ReleaseFile: "swarmcli-release.yaml",
		},
		SyncPolicy:     application.SyncPolicy{Automated: automated, Wait: true, Timeout: application.Duration(3 * time.Minute)},
		DriftDetection: application.DriftManifest,
	}
}

// chartFiles is the fixture repository: a one-service chart and the release file
// that installs it. The service is a busybox that sleeps — the smallest thing
// that reliably reaches "running" on any runner, needing no healthcheck and a
// tag that is always pullable.
func chartFiles(release string, replicas int) map[string]string {
	return map[string]string{
		"swarmcli-release.yaml": "" +
			"apiVersion: v1\n" +
			"releases:\n" +
			"  - name: " + release + "\n" +
			"    chart: ./charts/app\n",
		"charts/app/Chart.yaml":  "apiVersion: v1\nname: app\nversion: 0.1.0\n",
		"charts/app/values.yaml": "replicas: " + itoa(replicas) + "\n",
		"charts/app/templates/stack.yaml": "" +
			"version: \"3.9\"\n" +
			"services:\n" +
			"  app:\n" +
			"    image: busybox:1.36\n" +
			"    command: [\"sleep\", \"3600\"]\n" +
			"    deploy:\n" +
			"      replicas: {{ .Values.replicas }}\n" +
			"      labels:\n" +
			"        com.swarmcli.release: {{ .Release.Name }}\n",
	}
}

// itoa avoids dragging strconv into the fixture for a single small integer.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// runningReplicas counts the tasks of the release's service that are actually
// running, filtered by the label the chart stamps. Reading the label rather
// than a service name keeps the assertion tied to what the chart declared.
func runningReplicas(t *testing.T, cli *dockerclient.Client, release string) int {
	t.Helper()
	ctx := context.Background()
	services, err := cli.ServiceList(ctx, swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", "com.swarmcli.release="+release)),
	})
	if err != nil {
		t.Fatalf("listing services: %v", err)
	}
	running := 0
	for _, svc := range services {
		tasks, err := cli.TaskList(ctx, swarm.TaskListOptions{
			Filters: filters.NewArgs(filters.Arg("service", svc.ID)),
		})
		if err != nil {
			t.Fatalf("listing tasks: %v", err)
		}
		for _, task := range tasks {
			if task.Status.State == swarm.TaskStateRunning {
				running++
			}
		}
	}
	return running
}

// waitForRunning polls until the release has want running tasks, or fails. The
// deploy waits for convergence itself, so this mostly confirms it rather than
// waiting on it — but a task can restart, and asserting a point-in-time count
// straight after a wait would be flaky.
func waitForRunning(t *testing.T, cli *dockerclient.Client, release string, want int) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if got := runningReplicas(t, cli, release); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("release %q never reached %d running tasks (last: %d)", release, want, runningReplicas(t, cli, release))
		}
		time.Sleep(2 * time.Second)
	}
}

// removeStack tears the release down after a test. The daemon is thrown away in
// CI, but a developer runs these repeatedly against one swarm, and a leftover
// stack would make the next run's assertions lie.
func removeStack(t *testing.T, release string) {
	t.Helper()
	backend, err := swarms.Get().Backend(context.Background(), "")
	if err != nil {
		t.Logf("cleanup: resolving backend: %v", err)
		return
	}
	if err := backend.RemoveStack(release); err != nil {
		t.Logf("cleanup: removing stack %q: %v", release, err)
	}
}

// chartFilesWithResources is chartFiles plus the things prune has to clean up
// beside the service: an overlay network the stack owns and a named volume it
// must not touch.
//
// Configs and secrets are deliberately absent. Compose sources them with
// `file:`, which the loader resolves against the controller's own filesystem —
// a rendered chart has no directory to resolve against, so a fixture cannot
// declare one without hard-coding a checkout path. That RemoveStack deletes
// them by namespace filter is settled by its unit tests
// (backend.TestRemoveStackRemovesServicesFirstThenWhatTheyUsed); what this
// fixture is for is the two behaviours only a real swarm can show — a network
// that can only be removed once its tasks are gone, and a volume that survives.
func chartFilesWithResources(release string, replicas int) map[string]string {
	files := chartFiles(release, replicas)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  app:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    networks: [internal]\n" +
		"    volumes:\n" +
		"      - data:/data\n" +
		"    deploy:\n" +
		"      replicas: {{ .Values.replicas }}\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"networks:\n" +
		"  internal: {}\n" +
		"volumes:\n" +
		"  data: {}\n"
	return files
}

// stackFilter matches everything Swarm labels with a stack's namespace.
func stackFilter(release string) filters.Args {
	return filters.NewArgs(filters.Arg("label", "com.docker.stack.namespace="+release))
}

// stackServiceCount counts the release's services by namespace label rather
// than by the chart's own label, because prune's promise is about the stack.
func stackServiceCount(t *testing.T, cli *dockerclient.Client, release string) int {
	t.Helper()
	svcs, err := cli.ServiceList(context.Background(), swarm.ServiceListOptions{Filters: stackFilter(release)})
	if err != nil {
		t.Fatalf("listing services of %q: %v", release, err)
	}
	return len(svcs)
}

func stackNetworkCount(t *testing.T, cli *dockerclient.Client, release string) int {
	t.Helper()
	nets, err := cli.NetworkList(context.Background(), network.ListOptions{Filters: stackFilter(release)})
	if err != nil {
		t.Fatalf("listing networks of %q: %v", release, err)
	}
	return len(nets)
}

// stackServiceIDs identifies the running services rather than counting them.
// A stack that was deleted and reinstalled has the same count and the same
// names, and only the ids say which of the two happened.
func stackServiceIDs(t *testing.T, cli *dockerclient.Client, release string) []string {
	t.Helper()
	svcs, err := cli.ServiceList(context.Background(), swarm.ServiceListOptions{Filters: stackFilter(release)})
	if err != nil {
		t.Fatalf("listing services of %q: %v", release, err)
	}
	ids := make([]string, 0, len(svcs))
	for _, svc := range svcs {
		ids = append(ids, svc.ID)
	}
	slices.Sort(ids)
	return ids
}

func stackVolumeCount(t *testing.T, cli *dockerclient.Client, release string) int {
	t.Helper()
	vols, err := cli.VolumeList(context.Background(), volume.ListOptions{Filters: stackFilter(release)})
	if err != nil {
		t.Fatalf("listing volumes of %q: %v", release, err)
	}
	return len(vols.Volumes)
}

// releaseRecordCount counts the engine's own history configs for a release.
// Uninstall deletes them, so this going to zero is what says the release is
// gone rather than merely undeployed.
func releaseRecordCount(t *testing.T, cli *dockerclient.Client, release string) int {
	t.Helper()
	cfgs, err := cli.ConfigList(context.Background(), swarm.ConfigListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", charts.LabelType+"="+charts.TypeRelease),
			filters.Arg("label", charts.LabelRelease+"="+release),
		),
	})
	if err != nil {
		t.Fatalf("listing release records of %q: %v", release, err)
	}
	return len(cfgs)
}

// removeVolumes clears the volumes a pruned stack deliberately left behind, so
// a developer running these repeatedly against one swarm does not accumulate
// them. Best effort: a volume still attached to a dying task is not worth
// failing a passing test over.
func removeVolumes(t *testing.T, cli *dockerclient.Client, release string) {
	t.Helper()
	vols, err := cli.VolumeList(context.Background(), volume.ListOptions{Filters: stackFilter(release)})
	if err != nil {
		t.Logf("cleanup: listing volumes of %q: %v", release, err)
		return
	}
	for _, v := range vols.Volumes {
		if err := removeVolumeWhenReleased(cli, v.Name); err != nil {
			t.Logf("cleanup: removing volume %q: %v", v.Name, err)
		}
	}
}

// volumeReleaseTimeout bounds the wait for a stack's tasks to let go of its
// volumes. It has to outlast a stop grace period the task does not co-operate
// with: the fixture runs `sleep`, which ignores SIGTERM, so Swarm waits the full
// grace period and then kills it.
const volumeReleaseTimeout = 30 * time.Second

// removeVolumeWhenReleased deletes a volume once the tasks using it have gone.
//
// removeStack returns when the daemon has accepted the service removals, not
// when the containers have exited, so a cleanup that deletes straight afterwards
// races them and loses. `force` does not help: it covers a volume with no
// container attached, not one still in use.
//
// Best effort by design — this is teardown, and a developer running these
// repeatedly against one swarm is who it is for; CI throws the daemon away. What
// it must not do is give up on the first refusal, because that is the expected
// answer for the first few seconds every single time.
func removeVolumeWhenReleased(cli *dockerclient.Client, name string) error {
	deadline := time.Now().Add(volumeReleaseTimeout)
	for {
		err := cli.VolumeRemove(context.Background(), name, true)
		// Already gone counts as removed: something else may have reaped it, and
		// undoing something that did not happen has succeeded.
		if err == nil || errdefs.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still in use after %s: %w", volumeReleaseTimeout, err)
		}
		time.Sleep(time.Second)
	}
}

// liveDriftApp is an application that decides sync against the running
// ServiceSpec rather than only against the last apply.
func liveDriftApp(name, repoDir string, automated bool) application.Spec {
	app := releaseApp(name, repoDir, automated)
	app.DriftDetection = application.DriftLive
	return app
}

// richChartFiles exercises every field the live comparison looks at, on a real
// daemon.
//
// That is the whole point of it. Each of these is somewhere swarmkit defaults,
// normalises or resolves what was written — nil replicas become 1, the image
// gains a digest, resources come back as zero structs rather than nil, labels as
// an empty map, a published port acquires whatever the daemon assigns — and a
// comparison that got any of them wrong would report drift on an untouched stack
// for as long as it was deployed. Only a round trip through a real swarm can
// show that, which is why this fixture is here and not in a unit test.
//
// Two services, so that "one of them is missing" is a case that exists.
func richChartFiles(release string, replicas int) map[string]string {
	files := chartFiles(release, replicas)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  app:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    environment:\n" +
		"      LOG_LEVEL: info\n" +
		"      REGION: eu\n" +
		"    networks: [internal]\n" +
		"    volumes:\n" +
		"      - data:/data\n" +
		"    ports:\n" +
		// Fixed rather than dynamic, so the assertion does not depend on
		// whatever the runner had free. High and uncommon to avoid a clash.
		"      - \"39117:8080\"\n" +
		"    deploy:\n" +
		"      replicas: {{ .Values.replicas }}\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"        tier: web\n" +
		"      placement:\n" +
		"        constraints: [node.role == manager]\n" +
		"      resources:\n" +
		"        limits:\n" +
		"          cpus: \"0.5\"\n" +
		"          memory: 64M\n" +
		"        reservations:\n" +
		"          cpus: \"0.1\"\n" +
		"          memory: 16M\n" +
		"  sidecar:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    networks: [internal]\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"networks:\n" +
		"  internal: {}\n" +
		"volumes:\n" +
		"  data: {}\n"
	return files
}

// serviceOf finds one of a stack's services by its scoped name.
func serviceOf(t *testing.T, cli *dockerclient.Client, name string) swarm.Service {
	t.Helper()
	svcs, err := cli.ServiceList(context.Background(), swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		t.Fatalf("listing service %q: %v", name, err)
	}
	for _, s := range svcs {
		if s.Spec.Name == name {
			return s
		}
	}
	t.Fatalf("no service named %q", name)
	return swarm.Service{}
}

// updateOutOfBand mutates a running service the way an operator with a shell
// would, through the daemon and behind the controller's back.
func updateOutOfBand(t *testing.T, cli *dockerclient.Client, name string, mutate func(*swarm.ServiceSpec)) {
	t.Helper()
	svc := serviceOf(t, cli, name)
	spec := svc.Spec
	mutate(&spec)
	if _, err := cli.ServiceUpdate(context.Background(), svc.ID, svc.Version, spec, swarm.ServiceUpdateOptions{}); err != nil {
		t.Fatalf("updating service %q out of band: %v", name, err)
	}
}

// driftOf returns the live-drift axis of an application's only release.
func driftOf(t *testing.T, rec *reconcile.Reconciler, app string) *application.ReleaseDrift {
	t.Helper()
	view, ok := rec.View(app)
	if !ok {
		t.Fatalf("no view for %q", app)
	}
	if len(view.Status.Releases) != 1 {
		t.Fatalf("got %d releases, want 1", len(view.Status.Releases))
	}
	return view.Status.Releases[0].Drift
}

// fieldOf returns one reported field difference for a service, or fails naming
// what was actually found.
func fieldOf(t *testing.T, d *application.ReleaseDrift, service, field string) application.FieldDrift {
	t.Helper()
	if d == nil {
		t.Fatal("no drift reported at all")
	}
	for _, s := range d.Services {
		if s.Name != service {
			continue
		}
		for _, f := range s.Fields {
			if f.Field == field {
				return f
			}
		}
	}
	t.Fatalf("no %q difference reported for %q; got %+v", field, service, d.Services)
	return application.FieldDrift{}
}
