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
	"sort"
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
	ctx := context.Background()
	backend, err := swarms.Get().Backend(ctx, "")
	if err != nil {
		t.Logf("cleanup: resolving backend: %v", err)
		return
	}
	if err := backend.RemoveStack(ctx, release); err != nil {
		t.Logf("cleanup: removing stack %q: %v", release, err)
	}
}

// chartFilesWithResources is chartFiles plus the things prune has to clean up
// beside the service: an overlay network the stack owns and a named volume it
// must not touch.
//
// Configs and secrets are absent here because this fixture does not need them —
// see richChartFiles for one that references both. Since #99 that has to be an
// `external:` reference to something already on the swarm: a chart cannot carry
// content, because the only key that ever did read the controller's own
// filesystem rather than the chart's.
//
// What this fixture is for is the two behaviours only a real swarm can show — a
// network that can only be removed once its tasks are gone, and a volume that
// survives.
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

// removeNetworkWhenReleased deletes a network once the tasks attached to it have
// gone.
//
// It has to retry for the same reason removeVolumeWhenReleased does: Swarm
// refuses to remove a network anything still references, and a task that has just
// been told to stop still does. Best effort — this is teardown of something a
// test created outside any stack, so RemoveStack will never reach it.
func removeNetworkWhenReleased(t *testing.T, cli *dockerclient.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(volumeReleaseTimeout)
	for {
		err := cli.NetworkRemove(context.Background(), name)
		if err == nil || errdefs.IsNotFound(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Logf("cleanup: removing network %q: %v", name, err)
			return
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
//
// It takes t because the mounted config and secret have to exist on the swarm
// before the deploy: they are `external:`, which since #99 is the only way a
// chart can reference either. The fixture creates them itself, named after the
// release so that the twelve tests using it do not share one pair.
//
// What the comparison actually reads is the reference on the service — a name,
// an id and a target path — and an external reference carries all three, so
// nothing about the mount comparison is weaker for the resources being an
// operator's rather than the stack's. What no longer happens is the deploy
// creating them, which this fixture never asserted.
func richChartFiles(t *testing.T, release string, replicas int) map[string]string {
	t.Helper()
	site, token := createExternalConfigAndSecret(t, release)

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
		// A read-only bind beside the named volume, so both mount types and the
		// read-only flag are all round-tripped through a real daemon. An absolute
		// path that exists on any node, because a swarm bind names a path on
		// whichever node runs the task.
		"      - /etc/hostname:/etc/host-name:ro\n" +
		"    configs: [site]\n" +
		"    secrets: [token]\n" +
		// Container labels, which are not the service labels under deploy: two
		// different sets, two different flags, and only these reach the container.
		"    labels:\n" +
		"      role: web\n" +
		"    ports:\n" +
		// Fixed rather than dynamic, so the assertion does not depend on
		// whatever the runner had free. High and uncommon to avoid a clash.
		"      - \"39117:8080\"\n" +
		// And one published without naming a port, which is the case the
		// comparison has to be right about and no unit test can settle: the
		// daemon allocates one from the ingress range. It does so on the
		// service's runtime endpoint rather than on its spec, so both sides
		// should still read zero — and if that is ever wrong, this is the line
		// that turns the clean test red.
		"      - \"8081\"\n" +
		"    deploy:\n" +
		"      replicas: {{ .Values.replicas }}\n" +
		// Explicit, so the comparison is exercised against a stated mode as
		// well as against the sidecar's unstated one, which the daemon returns
		// as vip.
		"      endpoint_mode: vip\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"        tier: web\n" +
		// Every one of these is declared *partially* on purpose, which is the
		// whole of what makes them worth deploying. A struct the manifest leaves
		// out entirely round-trips as nothing; the defaulting happens inside one
		// it declared, where the daemon returns the unstated fields under the
		// names of their defaults. A fixture that spelled all of them out would
		// exercise none of that.
		"      update_config:\n" +
		"        parallelism: 1\n" +
		"      restart_policy:\n" +
		"        condition: any\n" +
		"      placement:\n" +
		"        constraints: [node.role == manager]\n" +
		"        preferences:\n" +
		"          - spread: node.id\n" +
		// Above the replica count the clean test deploys, so this bounds nothing
		// and cannot leave a task unschedulable on a one-node runner.
		"        max_replicas_per_node: 3\n" +
		"      resources:\n" +
		"        limits:\n" +
		"          cpus: \"0.5\"\n" +
		"          memory: 64M\n" +
		"          pids: 100\n" +
		"        reservations:\n" +
		"          cpus: \"0.1\"\n" +
		"          memory: 16M\n" +
		"  sidecar:\n" +
		"    image: busybox:1.36\n" +
		// Split across entrypoint and command deliberately: Swarm's Command is
		// compose's entrypoint and its Args is compose's command, so this is the
		// only arrangement that puts something in both.
		"    entrypoint: [\"/bin/sleep\"]\n" +
		"    command: [\"3600\"]\n" +
		"    networks: [internal]\n" +
		// The literal-field group. None of these needs a normalisation — the
		// round trip carries them across untouched — so what this fixture is for
		// is proving exactly that, on a real daemon, in one deploy.
		//
		// group_add and oom_score_adj are absent because the compose v3.9 schema
		// has neither, so a chart cannot declare them at all. They are still
		// compared: `--group-add` and `--dns-option-add` can put something there
		// against a desired side that is always empty, which is the only
		// direction those two can drift in.
		"    user: \"1000:1000\"\n" +
		"    hostname: sidecar-host\n" +
		// `default` is the only isolation a Linux daemon accepts, and it is also
		// what an empty one reads back as — so this states the value the
		// normalisation has to produce, from the other side.
		"    isolation: default\n" +
		"    stdin_open: true\n" +
		"    working_dir: /tmp\n" +
		"    stop_signal: SIGTERM\n" +
		"    stop_grace_period: 5s\n" +
		"    read_only: true\n" +
		"    init: true\n" +
		"    tty: true\n" +
		"    cap_add: [NET_ADMIN]\n" +
		"    cap_drop: [MKNOD]\n" +
		"    extra_hosts:\n" +
		"      - \"somewhere:10.11.12.13\"\n" +
		"    sysctls:\n" +
		"      net.core.somaxconn: \"1024\"\n" +
		"    ulimits:\n" +
		"      nofile:\n" +
		"        soft: 1024\n" +
		"        hard: 2048\n" +
		"    dns:\n" +
		"      - 1.1.1.1\n" +
		"    logging:\n" +
		"      driver: json-file\n" +
		"      options:\n" +
		"        max-size: 10m\n" +
		// A check that passes the first time it runs, and quickly. A task with a
		// healthcheck sits in `starting` until it first passes, so one that failed
		// would not fail this fixture — it would hang waitForRunning and then get
		// the container killed, which reads as something else entirely.
		"    healthcheck:\n" +
		"      test: [\"CMD-SHELL\", \"true\"]\n" +
		"      interval: 5s\n" +
		"      timeout: 3s\n" +
		"      retries: 2\n" +
		"      start_period: 2s\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"networks:\n" +
		"  internal: {}\n" +
		"volumes:\n" +
		"  data: {}\n" +
		// `external: true` with a sibling `name:` — the service keeps referencing
		// the compose key, and the name is what the resource is called on the
		// swarm. That is the form Eldara-Tech/swarmcli#513 taught CE's
		// external-reference pre-flight to resolve, and this repo pins past it.
		"configs:\n" +
		"  site:\n" +
		"    external: true\n" +
		"    name: \"" + site + "\"\n" +
		"secrets:\n" +
		"  token:\n" +
		"    external: true\n" +
		"    name: \"" + token + "\"\n"
	return files
}

// createExternalConfigAndSecret puts a config and a secret on the swarm for a
// chart to reference, the way an operator would, and returns their names.
//
// Since #99 a chart cannot carry content of its own, so a fixture that needs a
// mounted config or secret has to be handed one. These carry no namespace label:
// they are nobody's stack's, which is the whole point of an external reference,
// and it also keeps them out of every stack-scoped lister in this file.
//
// Removal is best-effort and runs after the stack's, because t.Cleanup is LIFO
// and every caller registers this first. Swarm refuses to remove a config a
// service still holds, and a cleanup that failed the test for losing that race
// would be worse than a leftover on a developer's swarm.
func createExternalConfigAndSecret(t *testing.T, release string) (config, secret string) {
	t.Helper()
	cli := dockerClient(t)
	ctx := context.Background()
	config, secret = externalConfigName(release), externalSecretName(release)

	// Best effort, for a leftover from a run that did not reach its cleanup: the
	// name is fixed per release, so one stale resource would fail the create and
	// every test using this fixture with it.
	_ = cli.ConfigRemove(ctx, config)
	_ = cli.SecretRemove(ctx, secret)

	if _, err := cli.ConfigCreate(ctx, swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: config},
		Data:        []byte("listen 8080\n"),
	}); err != nil {
		t.Fatalf("creating external config %q: %v", config, err)
	}
	t.Cleanup(func() { _ = cli.ConfigRemove(ctx, config) })

	if _, err := cli.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: secret},
		Data:        []byte("s3cr3t\n"),
	}); err != nil {
		t.Fatalf("creating external secret %q: %v", secret, err)
	}
	t.Cleanup(func() { _ = cli.SecretRemove(ctx, secret) })

	return config, secret
}

// externalConfigName and externalSecretName are what richChartFiles' resources
// are called on the swarm.
//
// Named here rather than spelled out at each use because a live drift field is
// keyed by the reference's name — `secrets[<name>]` — so an assertion about one
// has to agree with the fixture exactly. A hyphen rather than the `_` of a scoped
// name, because these belong to no stack.
func externalConfigName(release string) string { return release + "-site" }
func externalSecretName(release string) string { return release + "-token" }

// richTasks is how many running tasks richChartFiles deploys for a given app
// replica count: the app's own, plus the sidecar beside it.
//
// Named rather than written out at each call site so that adding a service to
// that fixture stays a one-line change here instead of a sweep through every
// test that waits on it.
func richTasks(appReplicas int) int { return appReplicas + 1 }

// jobChartFiles is a one-service chart whose service is a replicated job.
//
// It is separate from richChartFiles, and that separation is the finding: a job
// whose task sleeps never completes, so a release containing one never satisfies
// the convergence wait a deploy performs. Putting a job in the shared fixture
// made every test using it time out after three minutes. It gets its own chart
// and its own application, deployed with syncPolicy.wait off.
func jobChartFiles(release string, concurrency int) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/values.yaml"] = "replicas: " + itoa(concurrency) + "\n"
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  app:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    deploy:\n" +
		"      mode: replicated-job\n" +
		"      replicas: {{ .Values.replicas }}\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n"
	return files
}

// jobApp is a live-drift application that does not wait for convergence, because
// the release it deploys is a job that never converges by that definition.
func jobApp(name, repoDir string) application.Spec {
	app := liveDriftApp(name, repoDir, false)
	app.SyncPolicy.Wait = false
	return app
}

// bareImageChartFiles declares the two image references a manifest can write
// that the daemon does not store as written.
//
// It gets its own chart for the reason jobChartFiles does, and the reason is the
// finding: every other fixture here names `busybox:1.36`, which is already
// exactly the reference docker's client would send, so not one of them — the
// clean test included — could observe the client's own rewrite at all. That is
// how swarmcli-cd#104 shipped. imageWithTagString runs
// FamiliarString(TagNameOnly(ref)) over ContainerSpec.Image on the way into
// every ServiceCreate and ServiceUpdate, unconditionally, so:
//
//   - `busybox` is stored as `busybox:latest`, and
//   - `docker.io/library/busybox:1.36` is stored as `busybox:1.36`.
//
// Either one leaves the desired side saying one thing and the live side another
// on a stack nobody has touched, which on an automated application is a redeploy
// every tick for ever. Both are here because they are separate halves of the
// rewrite and only the first needs anything pulled that the runner does not
// already have — the second names the very image every other fixture uses, so it
// exercises the registry-stripping half at no cost.
func bareImageChartFiles(release string) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  bare:\n" +
		"    image: busybox\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    deploy:\n" +
		"      replicas: {{ .Values.replicas }}\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"  qualified:\n" +
		"    image: docker.io/library/busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    deploy:\n" +
		"      replicas: {{ .Values.replicas }}\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n"
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

// twoServiceChartFiles is a stack whose second service sits behind a value, so
// that one commit drops a service from the template — the case in #75 — while
// changing nothing else about the release.
//
// The sidecar owns a named volume, which is what lets a test assert the other
// half of the decision: the service goes, its volume does not.
func twoServiceChartFiles(release string, sidecar bool) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/values.yaml"] = "replicas: 1\nsidecar: " + truth(sidecar) + "\n"
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  app:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    deploy:\n" +
		"      replicas: {{ .Values.replicas }}\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"{{- if .Values.sidecar }}\n" +
		"  sidecar:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    volumes:\n" +
		"      - scratch:/scratch\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"volumes:\n" +
		"  scratch: {}\n" +
		"{{- end }}\n"
	return files
}

func truth(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// sweepingApp is an application that deletes the services its chart has stopped
// declaring. The drift mode is a parameter because the feature is deliberately
// independent of it.
func sweepingApp(name, repoDir string, mode application.DriftDetection) application.Spec {
	app := releaseApp(name, repoDir, true)
	app.DriftDetection = mode
	app.SyncPolicy.PruneResources = true
	return app
}

// serviceNamesOf lists a stack's running services by scoped name.
func serviceNamesOf(t *testing.T, cli *dockerclient.Client, release string) []string {
	t.Helper()
	svcs, err := cli.ServiceList(context.Background(), swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", "com.docker.stack.namespace="+release)),
	})
	if err != nil {
		t.Fatalf("listing the stack's services: %v", err)
	}
	names := make([]string, 0, len(svcs))
	for _, s := range svcs {
		names = append(names, s.Spec.Name)
	}
	sort.Strings(names)
	return names
}

// createServiceByHand deploys a service into a stack's namespace the way an
// operator with a shell would: carrying the label that says it belongs to the
// release, and with nothing anywhere saying this controller put it there.
func createServiceByHand(t *testing.T, cli *dockerclient.Client, release, name string) {
	t.Helper()
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   name,
			Labels: map[string]string{"com.docker.stack.namespace": release},
		},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:   "busybox:1.36",
			Command: []string{"sleep", "3600"},
		}},
	}
	if _, err := cli.ServiceCreate(context.Background(), spec, swarm.ServiceCreateOptions{}); err != nil {
		t.Fatalf("creating service %q by hand: %v", name, err)
	}
}

// ------------------------------------------ what a chart stops declaring (#80)

// chartFilesWithPrunables is a stack that puts a service and a network behind a
// value, so that one commit drops both while changing nothing else about the
// release.
//
// The half that stays is the point. A sweep that deleted everything under the
// namespace would satisfy a fixture that only dropped things, so `keep` is
// declared in both renders and asserted to survive.
//
// It used to drop a config and a secret too, and that was most of what it was
// for — #80 covered the three kinds #75 left open. Since #99 a stack cannot own
// either: content came from `file:`, which read the controller's filesystem, and
// an `external:` declaration is a reference that declaredNames deliberately does
// not report, so it is never a sweep candidate. There is no manifest left that
// puts a config or a secret in range of the sweep, so those two are gone rather
// than converted into a case that would pass while exercising nothing.
//
// The drop of the `drop` network is what needs the real swarm: the sidecar is
// attached to it, so it cannot be removed until that service's tasks have
// drained.
func chartFilesWithPrunables(release string, extras bool) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/values.yaml"] = "replicas: 1\nextras: " + truth(extras) + "\n"
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  app:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    networks: [keep]\n" +
		"    deploy:\n" +
		"      replicas: {{ .Values.replicas }}\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"{{- if .Values.extras }}\n" +
		"  sidecar:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    networks: [drop]\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"{{- end }}\n" +
		"networks:\n" +
		"  keep: {}\n" +
		"{{- if .Values.extras }}\n" +
		"  drop: {}\n" +
		"{{- end }}\n"
	return files
}

// stackConfigNames lists a stack's configs by scoped name, excluding the release
// records — those carry no namespace label, so this is belt and braces over the
// filter and a check that they never come into range.
func stackConfigNames(t *testing.T, cli *dockerclient.Client, release string) []string {
	t.Helper()
	configs, err := cli.ConfigList(context.Background(), swarm.ConfigListOptions{Filters: stackFilter(release)})
	if err != nil {
		t.Fatalf("listing configs of %q: %v", release, err)
	}
	names := make([]string, 0, len(configs))
	for _, c := range configs {
		if c.Spec.Labels[charts.LabelType] == charts.TypeRelease {
			continue
		}
		names = append(names, c.Spec.Name)
	}
	sort.Strings(names)
	return names
}

// stackNetworkNames lists a stack's networks by name.
func stackNetworkNames(t *testing.T, cli *dockerclient.Client, release string) []string {
	t.Helper()
	nets, err := cli.NetworkList(context.Background(), network.ListOptions{Filters: stackFilter(release)})
	if err != nil {
		t.Fatalf("listing networks of %q: %v", release, err)
	}
	names := make([]string, 0, len(nets))
	for _, n := range nets {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return names
}

// syncUntilConverged reconciles until done, or fails saying it never did.
//
// A single Sync is not enough for a resource sweep and that is by design: Swarm
// refuses to remove a config, secret or network that is still in use, and one
// whose service was removed moments ago is in use until its tasks have gone. The
// controller warns and tries again on the next interval, so a test has to do
// what the controller does. The fixture runs `sleep`, which ignores SIGTERM, so
// the drain takes the full stop-grace period.
func syncUntilConverged(t *testing.T, rec *reconcile.Reconciler, app string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for pass := 1; ; pass++ {
		if err := rec.Sync(context.Background(), app); err != nil {
			t.Logf("sync pass %d: %v", pass, err)
		}
		if done() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep never converged after %d passes", pass)
		}
		time.Sleep(3 * time.Second)
	}
}

// createConfigByHand stores a config in a stack's namespace the way an operator
// with a shell would: carrying the label that says it belongs to the release,
// and with nothing anywhere saying this controller put it there.
func createConfigByHand(t *testing.T, cli *dockerclient.Client, release, name string) {
	t.Helper()
	_, err := cli.ConfigCreate(context.Background(), swarm.ConfigSpec{
		Annotations: swarm.Annotations{
			Name:   name,
			Labels: map[string]string{"com.docker.stack.namespace": release},
		},
		Data: []byte("not ours\n"),
	})
	if err != nil {
		t.Fatalf("creating config %q by hand: %v", name, err)
	}
	t.Cleanup(func() { _ = cli.ConfigRemove(context.Background(), name) })
}
