// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out bytes.Buffer
		if err := run([]string{arg}, &out); err != nil {
			t.Fatalf("run(%q) returned %v, want nil", arg, err)
		}
		got := out.String()
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

func TestRunUnimplemented(t *testing.T) {
	var out bytes.Buffer
	err := run(nil, &out)
	if !errors.Is(err, errNotImplemented) {
		t.Fatalf("run(nil) = %v, want errNotImplemented", err)
	}
	if out.Len() != 0 {
		t.Errorf("run(nil) wrote %q to stdout, want nothing", out.String())
	}
}
