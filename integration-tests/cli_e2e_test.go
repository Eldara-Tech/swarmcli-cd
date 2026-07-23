// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCLIEndToEnd drives the whole binary the way an operator does, which is the
// gap the reconciler-level tests leave: it builds swarmcli-cd, runs `controller`
// as a subprocess against a real swarm, and drives `app ...` against it as
// separate subprocesses — so the CLI, the HTTP client, the API server, the
// reconciler and the backend are all the real ones, wired end to end.
//
// It is the manual smoke test made automatic. Everything it asserts a human
// would otherwise have to check by hand before a release.
func TestCLIEndToEnd(t *testing.T) {
	cli := dockerClient(t) // skips unless the daemon is a swarm manager
	bin := buildBinary(t)

	const release = "e2e-cli"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	appsFile := filepath.Join(t.TempDir(), "applications.yaml")
	writeFile(t, appsFile, applicationsYAML(release, repo))

	const token = "e2e-admin-token"
	addr := freeAddr(t)
	server := "http://" + addr

	ctl := startController(t, bin, appsFile, t.TempDir(), addr, token)
	waitHealthy(t, bin, server, token, ctl)

	run := func(args ...string) cliResult { return runCLI(t, bin, server, token, args...) }

	// The application is visible before anything is deployed.
	if r := run("app", "list"); r.code != 0 || !strings.Contains(r.stdout, release) {
		t.Fatalf("app list: code=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
	}

	// A wrong token is rejected over the wire — the guard is the real one.
	if r := runCLI(t, bin, server, "the-wrong-token", "app", "list"); r.code == 0 {
		t.Errorf("app list with a wrong token = 0, want a rejection (stdout %q)", r.stdout)
	}

	// The deploy, and the assertion that it actually ran: `--wait` returns 0
	// only once the swarm has converged.
	if r := run("app", "sync", release, "--wait", "--timeout", "3m"); r.code != 0 {
		t.Fatalf("app sync --wait: code=%d stderr=%q\n%s", r.code, r.stderr, ctl.log())
	}
	waitForRunning(t, cli, release, 1)

	if r := run("app", "get", release); !containsFold(r.stdout, "synced") || !containsFold(r.stdout, "healthy") {
		t.Errorf("app get did not report synced+healthy:\n%s", r.stdout)
	}

	// Drift, both directions. Move the desired state to two replicas and let the
	// loop notice — a manual application plans but does not apply, so the state
	// goes out-of-sync with the change unapplied.
	commitChange(t, repo, chartFiles(release, 2))
	waitForSyncState(t, run, release, "out-of-sync", ctl)
	if got := runningReplicas(t, cli, release); got != 1 {
		t.Errorf("running replicas = %d while drift is only detected, want the change unapplied at 1", got)
	}

	// The diff shows the pending upgrade...
	if r := run("app", "diff", release); r.code != 0 || !containsFold(r.stdout, "upgrade") {
		t.Errorf("app diff did not show the pending upgrade: code=%d stdout=%q", r.code, r.stdout)
	}
	// ...and the explicit sync applies it and converges.
	if r := run("app", "sync", release, "--wait", "--timeout", "3m"); r.code != 0 {
		t.Fatalf("second app sync --wait: code=%d stderr=%q\n%s", r.code, r.stderr, ctl.log())
	}
	waitForRunning(t, cli, release, 2)

	// The clean-shutdown assertion is in ctl's cleanup: SIGTERM must exit 0, or
	// every rollout under Swarm becomes a restart loop.
}

// ---------------------------------------------------------------- the binary

// buildBinary compiles swarmcli-cd once and returns its path. Driving the real
// binary rather than calling into the packages is the point: it is the only way
// to exercise argument parsing, exit codes and the process lifecycle, which is
// exactly what a human running it by hand was checking.
func buildBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "swarmcli-cd")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/swarmcli-cd")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the binary: %v\n%s", err, out)
	}
	return bin
}

// ---------------------------------------------------------------- controller

// controller is a running controller subprocess.
type controller struct {
	cmd     *exec.Cmd
	logPath string
}

// log returns the controller's output so far, for a failing assertion to print.
func (c *controller) log() string {
	b, _ := os.ReadFile(c.logPath)
	return "--- controller log ---\n" + string(b)
}

// startController runs `controller` as a subprocess and registers a cleanup that
// asserts it exits 0 on SIGTERM — the signal Swarm sends, and the one a
// non-zero exit on would turn into a restart loop.
func startController(t *testing.T, bin, configPath, dataDir, addr, token string) *controller {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "controller.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "controller", "--config", configPath, "--data", dataDir, "--listen", addr)
	cmd.Env = append(os.Environ(), "SWARMCLI_CD_ADMIN_TOKEN="+token)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the controller: %v", err)
	}
	c := &controller{cmd: cmd, logPath: logPath}

	t.Cleanup(func() {
		_ = logFile.Close()
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return // already gone (a test that killed it, or a crash)
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		err := cmd.Wait()
		if err != nil {
			t.Errorf("controller did not exit cleanly on SIGTERM: %v\n%s", err, c.log())
		}
	})
	return c
}

// waitHealthy blocks until the controller answers, using the healthcheck
// subcommand — which is also the container's HEALTHCHECK, so this covers it too.
func waitHealthy(t *testing.T, bin, server, token string, ctl *controller) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if runCLI(t, bin, server, token, "healthcheck").code == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("controller never became healthy\n%s", ctl.log())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- CLI driver

type cliResult struct {
	stdout string
	stderr string
	code   int
}

// runCLI runs one CLI invocation against the controller and returns what it
// printed and its exit code.
func runCLI(t *testing.T, bin, server, token string, args ...string) cliResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(),
		"SWARMCLI_CD_SERVER="+server,
		"SWARMCLI_CD_ADMIN_TOKEN="+token,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("running %v: %v", args, err)
	}
	return cliResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// ---------------------------------------------------------------- helpers

// freeAddr picks a loopback address with a free port. The tiny window between
// closing the listener and the controller binding it is tolerable on a
// single-purpose CI runner and buys a test that never collides with another.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func applicationsYAML(release, repoDir string) string {
	return "" +
		"applications:\n" +
		"  - name: " + release + "\n" +
		"    source:\n" +
		"      repoURL: " + repoDir + "\n" +
		"      revision: " + branch + "\n" +
		"      releaseFile: swarmcli-release.yaml\n" +
		"    syncPolicy:\n" +
		// Manual: the loop plans on every tick but never applies, so the test
		// drives the deploys itself. A short interval so a commit is re-planned
		// in seconds — the diff and the unapplied-drift state are then real
		// rather than racing the default three-minute tick.
		"      automated: false\n" +
		"      interval: 2s\n" +
		// Wait for convergence before recording the result, so the sync reports
		// Healthy rather than Progressing: without it apply returns before the
		// tasks are running, and `app get` right after a sync would be racing
		// the rollout.
		"      wait: true\n" +
		"      timeout: 3m\n"
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// waitForSyncState polls `app get` until the reported sync state contains want,
// so a test can wait for the background loop to observe a change rather than
// racing it. The reconcile interval is seconds, so this settles quickly.
func waitForSyncState(t *testing.T, run func(...string) cliResult, release, want string, ctl *controller) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		r := run("app", "get", release)
		if r.code == 0 && containsFold(r.stdout, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("application never reached sync state %q\nlast:\n%s%s", want, r.stdout, ctl.log())
		}
		time.Sleep(time.Second)
	}
}
