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

	"github.com/Eldara-Tech/swarmcli-cd/authz"
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

func TestServeReportsAMissingConfig(t *testing.T) {
	err := serve(context.Background(), options{
		configPath: filepath.Join(t.TempDir(), "applications.yaml"),
		listen:     testListen,
		dataDir:    t.TempDir(),
	}, discardLog())
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

	err := serve(context.Background(), options{
		configPath: writeConfig(t),
		listen:     testListen,
		dataDir:    t.TempDir(),
	}, discardLog())
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

	if err := serve(ctx, options{configPath: writeConfig(t), listen: testListen, dataDir: data}, discardLog()); err != nil {
		t.Fatalf("serve = %v, want nil", err)
	}

	for _, dir := range []string{"repos", "charts"} {
		if _, err := os.Stat(filepath.Join(data, dir)); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
}

// testListen binds an ephemeral port on the loopback: the tests need a real
// listener but must not collide with anything, least of all each other.
const testListen = "127.0.0.1:0"

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
