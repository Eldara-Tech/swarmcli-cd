// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package controller is swarmcli-cd's entry point.
//
// It lives here rather than in cmd/ because a main package cannot be imported.
// The private swarmcli-cd-be companion's whole main.go is a set of blank
// imports that register replacement seams, plus a call to Main; cmd/swarmcli-cd
// is the same file without the blank imports. See docs/extensibility.md.
package controller

import (
	"fmt"
	"io"
	"os"

	"github.com/Eldara-Tech/swarmcli/charts"
)

// version is the binary's own version, stamped at build time with
//
//	-X github.com/Eldara-Tech/swarmcli-cd/controller.version=<version>
//
// The path is qualified rather than main.version so that both editions stamp
// the same symbol: the companion builds its own cmd/ and would otherwise have
// to reimplement the reporting to stamp it.
//
// It is deliberately distinct from the chart-engine version reported by
// charts.EngineVersion(): the engine is the CE module's code, so this binary
// carries whichever engine it pinned, regardless of its own tag. Both are
// reported because a chart's swarmcliVersion constraint resolves against the
// engine, not against this.
var version = "dev"

const usage = `swarmcli-cd — GitOps continuous delivery for Docker Swarm

Commands:
  controller   Run the reconciler and the HTTP API
  app          Inspect and sync applications through a running controller
  version      Print the version
  help         Show this help

Run "swarmcli-cd controller --help" or "swarmcli-cd app help" for their options.
`

// Main runs the process and never returns. It is what cmd/swarmcli-cd calls,
// and what the private companion calls after its blank imports have registered
// their seams.
func Main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run dispatches a command and returns a process exit code. Main is a wrapper
// over it so that everything below is testable without exiting the test binary.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}

	switch args[0] {
	case "controller":
		return runController(args[1:], stdout, stderr)
	case "app":
		return appMain(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		_, _ = fmt.Fprintf(stdout, "swarmcli-cd %s (chart engine %s)\n", version, engineVersion())
		return 0
	case "help", "--help", "-h":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		return usageErr(stderr, fmt.Sprintf("unknown command %q", args[0]), usage)
	}
}

// engineVersion reports the embedded chart-engine version, or "unstamped" for a
// plain `go build`. An unstamped engine is not an error — charts.CheckCompat
// treats it as unknown rather than incompatible — so it is surfaced plainly
// instead of being hidden behind an empty string.
func engineVersion() string {
	if v := charts.EngineVersion(); v != "" {
		return v
	}
	return "unstamped"
}

// usageErr reports a misuse and returns exit code 2, leaving 1 to mean the
// command ran and failed. usageText is the help for whichever command was
// misused, so the message lands next to the options it should have used.
func usageErr(stderr io.Writer, msg, usageText string) int {
	_, _ = fmt.Fprintf(stderr, "Error: %s\n\n%s", msg, usageText)
	return 2
}

// fail reports a command that ran and failed.
func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "Error: %v\n", err)
	return 1
}
