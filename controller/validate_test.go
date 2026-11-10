// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHelp(t *testing.T) {
	for _, args := range [][]string{{"validate", "--help"}, {"validate", "-h"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, code)
		}
		if !strings.Contains(stdout.String(), "swarmcli-cd validate [options]") {
			t.Errorf("stdout = %q, want validate usage", stdout.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty", stderr.String())
		}
	}
}

func TestValidateSuccess(t *testing.T) {
	const appset = `
applications:
  - name: edge
    source:
      repoURL: https://github.com/acme/infra.git
      revision: main
      releaseFile: swarm/prod/swarmcli-release.yaml
`

	dir := t.TempDir()
	file := filepath.Join(dir, "applications.yaml")
	if err := os.WriteFile(file, []byte(appset), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "--file", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(validate) = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "OK: ") {
		t.Errorf("stdout = %q, want an OK verdict", stdout.String())
	}
	if !strings.Contains(stdout.String(), "(1 application)") {
		t.Errorf("stdout = %q, want the application count", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// An explicitly empty set is valid, so the pre-commit check passes the commit
// that removes the last application.
func TestValidateAcceptsAnExplicitlyEmptySet(t *testing.T) {
	file := filepath.Join(t.TempDir(), "applications.yaml")
	if err := os.WriteFile(file, []byte("applications: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "--file", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(validate) = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "(0 applications)") {
		t.Errorf("stdout = %q, want the application count", stdout.String())
	}
}

func TestValidateFailure(t *testing.T) {
	const broken = `
applications:
  - name: edge
    source:
      repoURL: https://github.com/acme/infra.git
`

	dir := t.TempDir()
	file := filepath.Join(dir, "applications.yaml")
	if err := os.WriteFile(file, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "--file", file}, &stdout, &stderr); code != 1 {
		t.Fatalf("run(validate) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "revision is required") {
		t.Errorf("stderr = %q, want a config validation error", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestValidateRejectsPositionalArgument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "applications.yaml"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(validate) = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Errorf("stderr = %q, want a usage rejection", stderr.String())
	}
}

func TestValidateRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "--nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(validate) = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "swarmcli-cd validate [options]") {
		t.Errorf("stderr = %q, want the validate usage", stderr.String())
	}
}

// The interval floor, refused here and clamped at runtime. The two halves are
// deliberately different: the controller must never refuse an app set over one
// application's number, because that would freeze every other application's
// updates too — so this is the one context with somebody present to fix it.
func TestValidateRefusesAnIntervalBelowTheFloor(t *testing.T) {
	const appset = `
applications:
  - name: edge
    source:
      repoURL: https://github.com/acme/infra.git
      revision: main
      releaseFile: swarm/prod/swarmcli-release.yaml
    syncPolicy:
      interval: 1ms
`

	dir := t.TempDir()
	file := filepath.Join(dir, "applications.yaml")
	if err := os.WriteFile(file, []byte(appset), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "--file", file}, &stdout, &stderr); code == 0 {
		t.Fatalf("run(validate) = 0, want non-zero for an interval below the floor (stdout: %q)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "below the") {
		t.Errorf("stderr = %q, want it to name the floor", stderr.String())
	}
	if !strings.Contains(stderr.String(), "edge") {
		t.Errorf("stderr = %q, want it to name the application", stderr.String())
	}
}

// --dir validates a directory of files the way the controller reads
// --appset-files: every app-set file in it, together, under the rules that span
// a set.
func TestValidateDir(t *testing.T) {
	app := func(name, repo string) string {
		return "applications:\n  - name: " + name + "\n    source: {repoURL: https://github.com/acme/" + repo + ".git, revision: main, releaseFile: r.yaml}\n"
	}
	for name, tc := range map[string]struct {
		files  map[string]string
		code   int
		stdout string
		stderr string
	}{
		"valid": {
			files:  map[string]string{"team-a.yaml": app("edge", "a"), "team-b.yml": app("core", "b"), "notes.txt": "not: [yaml"},
			stdout: "(2 applications)",
		},
		"a name in two files": {
			files:  map[string]string{"team-a.yaml": app("edge", "a"), "team-b.yaml": app("edge", "b")},
			code:   1,
			stderr: "team-b.yaml: applications[0]: duplicate application name 'edge', also declared in team-a.yaml",
		},
		"nothing to read": {
			files:  map[string]string{"notes.txt": "x"},
			code:   1,
			stderr: "no app-set files",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for file, content := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var stdout, stderr bytes.Buffer
			if code := run([]string{"validate", "--dir", dir}, &stdout, &stderr); code != tc.code {
				t.Fatalf("run(validate) = %d, want %d (stderr: %q)", code, tc.code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.stdout) || !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("stdout = %q, stderr = %q, want %q and %q", stdout.String(), stderr.String(), tc.stdout, tc.stderr)
			}
		})
	}
}

// --file and --dir are one set or the other; given both, which was meant is
// not this command's to guess.
func TestValidateRefusesFileAndDir(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "--file", "applications.yaml", "--dir", t.TempDir()}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(validate) = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--file and --dir are alternatives") {
		t.Errorf("stderr = %q, want the refusal", stderr.String())
	}
}
