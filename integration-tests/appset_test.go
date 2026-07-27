// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/appset"
)

// The app set moving is the whole of issue #47: an operator commits a change to
// the applications file and the controller's running set follows, with no
// redeploy and no restart. This is that, against a real swarm — one application
// deployed under set A, then a set B that removes it, retunes the other and adds
// a third.
//
// The removed application's stack must still be running afterwards. That is
// decision D-e: app-of-apps reports an orphan, it does not prune one, and the
// difference is a deployment quietly disappearing because somebody edited a
// file.
func TestAppSetChangeDrivesTheRunningSet(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-appset-leaver"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	dir := t.TempDir()
	publish := appSetPublisher(t, dir)

	// Set A: the application that will leave, deploying for real, plus one that
	// stays and will be retuned. Only the leaver is automated, so this costs one
	// deploy rather than three.
	publish(`applications:
  - name: leaver
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      wait: true
      timeout: 3m
  - name: keeper
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
`)

	src := appset.NewPath(appset.PathConfig{Dir: dir, Path: "applications.yaml"})
	rec := reconciler(t)
	loop := appset.NewLoop(src, rec, appset.LoopOptions{Mode: "path", Log: testLog()})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("first pass = %v, want nil", err)
	}
	if got := len(rec.Views()); got != 2 {
		t.Fatalf("running set has %d applications, want set A's 2", got)
	}

	// Deploy the leaver, so there is a real stack to be orphaned.
	if err := rec.SyncNow(context.Background(), "leaver"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	// Set B: leaver removed, keeper retuned, joiner added.
	publish(`applications:
  - name: keeper
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      interval: 10m
  - name: joiner
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
`)

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("second pass = %v, want nil", err)
	}

	names := map[string]bool{}
	for _, view := range rec.Views() {
		names[view.Spec.Name] = true
	}
	if len(names) != 2 || !names["keeper"] || !names["joiner"] {
		t.Errorf("running set = %v, want keeper and joiner", names)
	}
	if _, ok := rec.View("leaver"); ok {
		t.Error("the removed application is still being reconciled")
	}

	keeper, ok := rec.View("keeper")
	if !ok {
		t.Fatal("the retuned application left the set")
	}
	if keeper.Spec.SyncPolicy.Interval == 0 {
		t.Error("the retuned application still carries set A's policy")
	}

	// D-e: reported, not pruned. The stack outlives the application.
	if got := runningReplicas(t, cli, release); got != 1 {
		t.Errorf("the removed application's stack has %d running tasks, want 1: an orphan is reported, not deleted", got)
	}

	status := loop.Status()
	if len(status.AppSet.Orphaned) != 1 || status.AppSet.Orphaned[0] != "leaver" {
		t.Errorf("orphaned = %v, want [leaver]", status.AppSet.Orphaned)
	}
	if status.Applications != 2 {
		t.Errorf("status reports %d applications, want the 2 being reconciled", status.Applications)
	}
	if status.AppSet.Error != "" || status.AppSet.Stale {
		t.Errorf("status reports error=%q stale=%v, want a clean load", status.AppSet.Error, status.AppSet.Stale)
	}
}

// A commit that does not validate must leave the running set exactly as it was,
// and say so. This is the failure the whole last-good rule exists for: it is the
// one an operator will actually cause.
func TestBadAppSetLeavesTheRunningSetAlone(t *testing.T) {
	dockerClient(t)
	repo := gitRepo(t, chartFiles("e2e-appset-bad", 1))

	dir := t.TempDir()
	publish := appSetPublisher(t, dir)
	publish(`applications:
  - name: keeper
    source:
      repoURL: ` + repo + `
      revision: ` + branch + `
      releaseFile: swarmcli-release.yaml
`)

	src := appset.NewPath(appset.PathConfig{Dir: dir, Path: "applications.yaml"})
	rec := reconciler(t)
	loop := appset.NewLoop(src, rec, appset.LoopOptions{Mode: "path", Log: testLog()})

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("first pass = %v, want nil", err)
	}

	publish("applications:\n  - name: keeper\n    sauce: nonsense\n")
	if err := loop.Once(context.Background()); err == nil {
		t.Fatal("second pass = nil, want the validation error")
	}

	if got := len(rec.Views()); got != 1 {
		t.Errorf("running set has %d applications, want the last-good 1", got)
	}
	status := loop.Status()
	if !status.AppSet.Stale {
		t.Error("stale is not set, but what is running is a last-good set")
	}
	if status.AppSet.Error == "" {
		t.Error("the refusal was not reported")
	}
}

// appSetPublisher writes the app-set file the way the path mode requires:
// atomically, so a reader never sees half of it.
func appSetPublisher(t *testing.T, dir string) func(string) {
	t.Helper()
	return func(content string) {
		t.Helper()
		tmp := filepath.Join(dir, ".applications.yaml.tmp")
		if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "applications.yaml")); err != nil {
			t.Fatal(err)
		}
	}
}

// The whole of issue #53, driven the way an operator drives it: a controller
// bootstrapped with --appset-repo instead of --config, pulling its own
// application set out of git, and following a commit to it with no redeploy and
// no restart.
//
// It is deliberately the binary rather than the packages. The flags, the mode
// the status endpoint reports and the clone under --data are all bootstrap
// wiring, and nothing below the process boundary would prove any of it.
func TestGitSourcedAppSet(t *testing.T) {
	dockerClient(t) // skips unless the daemon is a swarm manager
	bin := buildBinary(t)

	const first, second = "e2e-git-appset", "e2e-git-appset-two"
	apps := gitRepo(t, chartFiles(first, 1))

	// The app set gets its own repository, which is the layout the docs
	// recommend: committing to it defines every application the controller
	// runs, and that is not the same permission as changing one app's chart.
	appSet := gitRepo(t, map[string]string{"apps/applications.yaml": appSetYAML(apps, first)})

	const token = "e2e-appset-token"
	addr := freeAddr(t)
	server := "http://" + addr
	dataDir := t.TempDir()

	ctl := startControllerWith(t, bin, dataDir, addr, token,
		"--appset-repo", appSet,
		"--appset-revision", branch,
		"--appset-path", "apps/applications.yaml",
		"--appset-interval", "1s")
	waitHealthy(t, bin, server, token, ctl)

	run := func(args ...string) cliResult { return runCLI(t, bin, server, token, args...) }

	// The set came from the repository, not from a file anybody mounted.
	if r := run("app", "list"); r.code != 0 || !strings.Contains(r.stdout, first) {
		t.Fatalf("app list: code=%d stdout=%q stderr=%q\n%s", r.code, r.stdout, r.stderr, ctl.log())
	}

	r := run("status")
	if r.code != 0 {
		t.Fatalf("status: code=%d stderr=%q", r.code, r.stderr)
	}
	for _, want := range []string{"git", appSet, "apps/applications.yaml"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status did not report %q:\n%s", want, r.stdout)
		}
	}
	// A revision is the difference between "configured for git" and "actually
	// pulled something".
	if strings.Contains(r.stdout, "never") {
		t.Errorf("status reports no successful load:\n%s\n%s", r.stdout, ctl.log())
	}

	// The clone lives under --data, in its own root: everything under an
	// application's clone is force-checked-out and cleaned on every fetch.
	if _, err := os.Stat(filepath.Join(dataDir, "appset")); err != nil {
		t.Errorf("app-set clone root: %v", err)
	}

	// And now the point of all of it: a commit adds an application, and the
	// running set follows without anybody redeploying the controller.
	commitChange(t, appSet, map[string]string{"apps/applications.yaml": appSetYAML(apps, first, second)})

	deadline := time.Now().Add(30 * time.Second)
	for {
		if out := run("app", "list").stdout; strings.Contains(out, second) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the committed application never joined the running set\n%s", ctl.log())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// appSetYAML is an app set declaring each name against the same repository.
// Every application is manual: this test is about the set moving, and a
// background deploy would only slow it down.
func appSetYAML(repoDir string, names ...string) string {
	out := "applications:\n"
	for _, name := range names {
		out += "" +
			"  - name: " + name + "\n" +
			"    source:\n" +
			"      repoURL: " + repoDir + "\n" +
			"      revision: " + branch + "\n" +
			"      releaseFile: swarmcli-release.yaml\n" +
			"    syncPolicy:\n" +
			"      automated: false\n"
	}
	return out
}
