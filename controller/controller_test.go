// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{arg}, &stdout, &stderr); code != 0 {
			t.Fatalf("run(%q) = %d, want 0 (stderr: %q)", arg, code, stderr.String())
		}
		got := stdout.String()
		if !strings.HasPrefix(got, "swarmcli-cd ") {
			t.Errorf("run(%q) = %q, want it to start with %q", arg, got, "swarmcli-cd ")
		}
		// The engine version comes from the CE module, so this also asserts the
		// dependency is wired up rather than merely present in go.mod.
		if !strings.Contains(got, "chart engine ") {
			t.Errorf("run(%q) = %q, want it to report the chart engine version", arg, got)
		}
	}
}

// An unstamped `go build` must still report something intelligible rather than
// an empty parenthetical — charts.CheckCompat treats an unknown engine as
// unknown, not as incompatible, so it is not an error condition.
func TestEngineVersionUnstamped(t *testing.T) {
	if got := engineVersion(); got == "" {
		t.Error("engineVersion() = \"\", want a non-empty value")
	}
}

// A bare invocation is not an error: there is no TUI here for it to launch
// instead, so it says what the binary can do and exits successfully.
func TestRunWithoutArgumentsPrintsUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, code)
		}
		if !strings.Contains(stdout.String(), "swarmcli-cd controller") {
			t.Errorf("run(%v) printed %q, want the usage", args, stdout.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("run(%v) wrote %q to stderr, want nothing", args, stderr.String())
		}
	}
}

// A typo must fail loudly rather than being ignored, and 2 distinguishes "you
// held it wrong" from "it ran and failed".
func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controler"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "controler") {
		t.Errorf("stderr = %q, want it to name the unknown command", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q to stdout, want the error on stderr only", stdout.String())
	}
}
