// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eldara-Tech/swarmcli-cd/appset"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/git"
)

func TestControllerHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "swarmcli-cd controller [options]") {
		t.Errorf("stdout = %q, want the controller's usage", stdout.String())
	}
	// The credentials are the part an operator cannot guess, so the help has to
	// name them rather than leaving them to the README.
	if !strings.Contains(stdout.String(), authz.EnvTokenFile) {
		t.Errorf("stdout = %q, want it to name %s", stdout.String(), authz.EnvTokenFile)
	}
}

// An unknown flag must be an error rather than being ignored: the alternative
// is a controller that silently runs with a default the operator thought they
// had overridden.
func TestControllerRejectsAnUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--listem", ":9000"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "listem") {
		t.Errorf("stderr = %q, want it to name the flag", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q to stdout, want the error on stderr only", stdout.String())
	}
}

func TestControllerRejectsAPositionalArgument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "start"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "start") {
		t.Errorf("stderr = %q, want it to name the argument", stderr.String())
	}
}

// A file mounted at deploy time that cannot be read is fatal, as it has always
// been: nothing will change under the controller's feet, so a restart is the
// operator's notification. The authorizer is swapped in because serve refuses
// on an unready one before it reads anything, which is the whole point of that
// check going first.
func TestServeReportsAMissingConfig(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})

	err := serve(context.Background(), staticOptions(filepath.Join(t.TempDir(), "applications.yaml"), t.TempDir()), discardLog())
	if err == nil {
		t.Fatal("serve = nil, want an error for a missing applications file")
	}
	if !strings.Contains(err.Error(), "applications.yaml") {
		t.Errorf("serve = %v, want the error to name the file", err)
	}
}

// The controller refuses to start rather than serving an API that rejects
// everything: an unconfigured authorizer and a wrong token are indistinguishable
// from the outside, and only one of them is worth an operator's afternoon.
func TestServeRefusesAnUnreadyAuthorizer(t *testing.T) {
	swapAuthorizer(t, unreadyAuthorizer{})

	err := serve(context.Background(), staticOptions(writeConfig(t), t.TempDir()), discardLog())
	if err == nil {
		t.Fatal("serve = nil, want a refusal")
	}
	if !errors.Is(err, authz.ErrNoToken) {
		t.Errorf("serve = %v, want it to carry the authorizer's own reason", err)
	}
	if !strings.Contains(err.Error(), "test") {
		t.Errorf("serve = %v, want it to name the authorizer in force", err)
	}
}

// The clone root and the chart cache must be separate directories: everything
// under a clone is force-checked-out and cleaned on every fetch, so a chart
// cache living there would be deleted underneath the builder.
func TestServeCreatesSeparateCloneAndChartDirectories(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})
	data := t.TempDir()

	// Already cancelled: serve wires everything up, starts both components and
	// unwinds immediately, which is as far as this can go without a daemon.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := serve(ctx, staticOptions(writeConfig(t), data), discardLog()); err != nil {
		t.Fatalf("serve = %v, want nil", err)
	}

	for _, dir := range []string{"repos", "charts"} {
		if _, err := os.Stat(filepath.Join(data, dir)); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
	// The app set is not cloned in this mode, so its root is not created.
	if _, err := os.Stat(filepath.Join(data, "appset")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("appset directory = %v, want it absent in static mode", err)
	}
}

// Which selector was given decides the mode; nothing states it twice.
func TestAppSetModeFollowsTheSelectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    options
		want string
	}{
		{"no selector is the mounted file", options{configPath: "/etc/applications.yaml"}, appSetModeStatic},
		{"a repository", options{appSetRepo: "https://example.com/apps.git"}, appSetModeGit},
		{"a directory", options{appSetDir: "/var/lib/appset"}, appSetModePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.mode(); got != tc.want {
				t.Errorf("mode = %q, want %q", got, tc.want)
			}
		})
	}
}

// A bootstrap that says two things, or half of one, is refused rather than
// resolved by a precedence rule nobody can remember. Each error has to name the
// flag that has to change.
func TestAppSetSelectorsAreValidated(t *testing.T) {
	valid := func(o options) options {
		o.appSetInterval = appset.DefaultInterval
		return o
	}
	for _, tc := range []struct {
		name string
		o    options
		want string
	}{
		{"two sources", valid(options{appSetRepo: "https://example.com/apps.git", appSetRevision: "main", appSetPath: "applications.yaml", appSetDir: "/var/lib/appset"}), "--appset-dir"},
		{"an explicit config beside a repository", valid(options{configSet: true, appSetRepo: "https://example.com/apps.git", appSetRevision: "main", appSetPath: "applications.yaml"}), "--config"},
		{"an explicit config beside a directory", valid(options{configSet: true, appSetDir: "/var/lib/appset"}), "--config"},
		{"a repository with no revision", valid(options{appSetRepo: "https://example.com/apps.git", appSetPath: "applications.yaml"}), "--appset-revision"},
		{"a repository with no path", valid(options{appSetRepo: "https://example.com/apps.git", appSetRevision: "main"}), "--appset-path"},
		{"a revision with no repository", valid(options{appSetRevision: "main"}), "--appset-revision"},
		{"a path with no source", valid(options{appSetPath: "applications.yaml"}), "--appset-path"},
		{"an interval of zero", options{appSetDir: "/var/lib/appset"}, "--appset-interval"},
		{"volumes without prune", valid(options{pruneVolumes: true}), "--prune-volumes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.o.validate()
			if err == nil {
				t.Fatal("validate = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// The default --config must not trip the ambiguity check: every deployment has
// one, and only an operator who typed it meant it.
func TestTheDefaultConfigIsNotAnExplicitOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// A repository that cannot be resolved, so this gets past the flags and
	// fails later — the assertion is only that it is not a usage error.
	code := run([]string{"controller", "--appset-repo", "/nonexistent.git", "--appset-revision", "main",
		"--appset-path", "applications.yaml", "--listen", testListen, "--data", t.TempDir()}, &stdout, &stderr)
	if code == 2 {
		t.Errorf("run = 2 (%s), want the default --config not to count as explicit", stderr.String())
	}
}

func TestControllerRejectsAnExplicitConfigBesideARepository(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"controller", "--config", "/etc/applications.yaml",
		"--appset-repo", "https://example.com/apps.git", "--appset-revision", "main",
		"--appset-path", "applications.yaml"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--config") {
		t.Errorf("stderr = %q, want it to name the flag", stderr.String())
	}
}

// The app set is a file inside a root, in both modes, and the directory mode is
// the only one with a name this end can guess.
func TestAppSetSourceDescribesWhereTheSetLives(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    options
		want string
	}{
		{
			"a repository names its revision and file",
			options{appSetRepo: "https://example.com/apps.git", appSetRevision: "main", appSetPath: "apps/applications.yaml"},
			"https://example.com/apps.git @ main (apps/applications.yaml)",
		},
		{
			"a directory defaults the file",
			options{appSetDir: "/var/lib/appset"},
			"/var/lib/appset (applications.yaml)",
		},
		{
			"a directory keeps a file it was given",
			options{appSetDir: "/var/lib/appset", appSetPath: "prod.yaml"},
			"/var/lib/appset (prod.yaml)",
		},
		{
			"the mounted file is itself the description",
			options{configPath: "/etc/swarmcli-cd/applications.yaml"},
			"/etc/swarmcli-cd/applications.yaml",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, got := appSetSource(tc.o, git.Auth{}, t.TempDir())
			if src == nil {
				t.Fatal("appSetSource returned no loader")
			}
			if got != tc.want {
				t.Errorf("description = %q, want %q", got, tc.want)
			}
		})
	}
}

// A set something else produces may simply not be ready yet: a repository
// briefly unreachable at boot, or a sidecar that has not finished its first
// sync. Exiting would restart-loop the controller under Swarm and take the API
// that could explain it down with it, so it starts with no applications and
// retries.
func TestServeStartsWhenASourcedAppSetCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    func(data string) options
	}{
		{"an unreachable repository", func(data string) options {
			return options{
				listen: testListen, dataDir: data, appSetInterval: appset.DefaultInterval,
				appSetRepo:     filepath.Join(data, "no-such-repo.git"),
				appSetRevision: "main",
				appSetPath:     "applications.yaml",
			}
		}},
		{"a directory nothing has written yet", func(data string) options {
			return options{
				listen: testListen, dataDir: data, appSetInterval: appset.DefaultInterval,
				appSetDir: filepath.Join(data, "not-synced-yet"),
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapAuthorizer(t, readyAuthorizer{})
			data := t.TempDir()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			if err := serve(ctx, tc.o(data), discardLog()); err != nil {
				t.Fatalf("serve = %v, want it to start anyway and retry", err)
			}
		})
	}
}

// The app set's clone must not share a root with the applications': everything
// under a clone is force-checked-out and cleaned on every fetch.
func TestServeCreatesTheAppSetCloneRoot(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})
	data := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := serve(ctx, options{
		listen: testListen, dataDir: data, appSetInterval: appset.DefaultInterval,
		appSetRepo:     filepath.Join(data, "no-such-repo.git"),
		appSetRevision: "main",
		appSetPath:     "applications.yaml",
	}, discardLog())
	if err != nil {
		t.Fatalf("serve = %v, want nil", err)
	}

	for _, dir := range []string{"repos", "charts", "appset"} {
		if _, err := os.Stat(filepath.Join(data, dir)); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
}

// The flags an operator cannot guess are the app-set ones, so the help has to
// name them rather than leaving them to the docs.
func TestControllerHelpNamesTheAppSetFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	for _, flagName := range []string{"--appset-repo", "--appset-revision", "--appset-dir", "--appset-path", "--appset-interval"} {
		if !strings.Contains(stdout.String(), flagName) {
			t.Errorf("stdout = %q, want it to name %s", stdout.String(), flagName)
		}
	}
}

// Prune is the one destructive setting, so the help has to say both that it
// exists and what turning it on costs.
func TestControllerHelpNamesThePruneFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	for _, want := range []string{"--prune", "--prune-volumes", "outage"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to mention %q", stdout.String(), want)
		}
	}
}

// testListen binds an ephemeral port on the loopback: the tests need a real
// listener but must not collide with anything, least of all each other.
const testListen = "127.0.0.1:0"

// staticOptions is the pre-#53 bootstrap: a file mounted at deploy time and no
// app-set selector.
func staticOptions(configPath, dataDir string) options {
	return options{
		configPath:     configPath,
		listen:         testListen,
		dataDir:        dataDir,
		appSetInterval: appset.DefaultInterval,
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeConfig writes the smallest applications file the loader accepts.
func writeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "applications.yaml")
	body := "applications:\n" +
		"  - name: edge\n" +
		"    source:\n" +
		"      repoURL: https://example.com/infra.git\n" +
		"      revision: main\n" +
		"      releaseFile: swarmcli-release.yaml\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// swapAuthorizer installs an authorizer for the duration of one test. The
// seam replaces rather than appends, so the original has to be put back.
func swapAuthorizer(t *testing.T, a authz.Authorizer) {
	t.Helper()
	original, originalName := authz.Get(), authz.Active()
	t.Cleanup(func() { authz.Register(originalName, original) })
	authz.Register("test", a)
}

type readyAuthorizer struct{ authz.Authorizer }

func (readyAuthorizer) Ready() error { return nil }

type unreadyAuthorizer struct{ authz.Authorizer }

func (unreadyAuthorizer) Ready() error { return authz.ErrNoToken }
