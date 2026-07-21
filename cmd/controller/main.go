// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Command controller is the SwarmCLI CD reconciler: it converges a Docker Swarm
// to the desired state declared in a Git repository.
//
// It is a scaffold. The reconcile loop is not implemented yet — see
// https://github.com/Eldara-Tech/swarmcli-cd/issues/1 for the design and phases.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Eldara-Tech/swarmcli/charts"
)

// version is the controller's own version, stamped at build time with
//
//	-X main.version=<version>
//
// It is deliberately distinct from the chart-engine version reported by
// charts.EngineVersion(): the engine is the CE module's code, so this binary
// carries whichever engine it pinned, regardless of its own tag. Both are
// reported because a chart's swarmcliVersion constraint resolves against the
// engine, not against this.
var version = "dev"

// errNotImplemented is what every non-informational invocation returns until the
// reconciler exists. It names the tracking issue rather than failing blankly.
var errNotImplemented = errors.New("swarmcli-cd: the reconciler is not implemented yet — see https://github.com/Eldara-Tech/swarmcli-cd/issues/1")

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 1 {
		switch args[0] {
		case "version", "--version", "-v":
			_, err := fmt.Fprintf(out, "swarmcli-cd %s (chart engine %s)\n", version, engineVersion())
			return err
		}
	}
	return errNotImplemented
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
