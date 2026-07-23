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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	dockerclient "github.com/docker/docker/client"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

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

// reconciler wires the real controller for one application: the real git
// sourcer, the real chart builder, and the local swarm registry — which resolves
// to a real moby-client backend against the daemon the process was started with.
// It is the wiring controller.serve builds, without the HTTP server.
func reconciler(t *testing.T, app application.Spec) *reconcile.Reconciler {
	t.Helper()
	data := t.TempDir()
	return reconcile.New([]application.Spec{app}, reconcile.Options{
		Fetcher: git.New(filepath.Join(data, "repos"), git.Auth{}),
		Builder: source.NewBuilder(filepath.Join(data, "charts"), nil),
		Swarms:  swarms.Get(),
	})
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
