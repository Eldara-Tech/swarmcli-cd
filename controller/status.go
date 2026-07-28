// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/client"
)

const statusUsage = `Usage: swarmcli-cd status [options]

Shows the controller itself rather than what it reconciles: where its
application set is sourced from, the commit and time of the last set that
validated, and whether what is running is a last-good set because a newer one is
being refused.

Options:
  --server <url>       Controller to talk to (default $` + client.EnvServer + `,
                       or ` + client.DefaultServer + `)
  -o, --output <fmt>   text or json (default text). json is the controller's
                       own response, unmodified

The admin token comes from ` + authz.EnvTokenFile + ` or
` + authz.EnvToken + `, never from a flag.
`

// runStatus reports the controller's own state.
//
// It exits 0 whenever the controller answered, including when what it answered
// is that the app set is stale. Reads report and checks judge: `healthcheck`
// and `validate` are the commands whose exit code is a verdict, and a read that
// failed on a state rather than on an error would be a quieter third.
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "")
	output := fs.String("output", "text", "")
	fs.StringVar(output, "o", "text", "")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, statusUsage)
			return 0
		}
		return usageErr(stderr, err.Error(), statusUsage)
	}
	if fs.NArg() > 0 {
		return usageErr(stderr, fmt.Sprintf("unexpected argument %q", fs.Arg(0)), statusUsage)
	}
	if *output != "text" && *output != "json" {
		return usageErr(stderr, fmt.Sprintf("invalid --output %q (want text or json)", *output), statusUsage)
	}

	token, tokenErr := authz.TokenFromEnv(os.Getenv, os.ReadFile)
	c := client.New(resolveServer(*server, os.Getenv), token)

	status, raw, err := c.Status(context.Background())
	if err != nil {
		return fail(stderr, explain(err, tokenErr))
	}
	if *output == "json" {
		if err := writeJSON(stdout, raw); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	renderStatus(stdout, status)
	return 0
}

// renderStatus prints the controller's state, leaving out the three lines that
// only exist when something is wrong. A "Stale: no" line on every healthy
// controller would train an operator to stop reading the block.
func renderStatus(out io.Writer, s application.ControllerStatus) {
	row := func(label, value string) { _, _ = fmt.Fprintf(out, "%-13s %s\n", label, value) }

	row("Mode", state(s.AppSet.Mode))
	if s.AppSet.Source != "" {
		row("Source", s.AppSet.Source)
	}
	if s.AppSet.Revision != "" {
		row("Revision", short(s.AppSet.Revision))
	}
	row("Loaded", observed(s.AppSet.LoadedAt))
	row("Applications", fmt.Sprint(s.Applications))

	if s.AppSet.Stale {
		row("Stale", "yes — the running set is the last one that validated")
	}
	if s.AppSet.Error != "" {
		row("Error", s.AppSet.Error)
	}
	if len(s.AppSet.Orphaned) > 0 {
		row("Orphaned", strings.Join(s.AppSet.Orphaned, ", "))
	}
	// Only when something was actually deleted, like the three above it: on a
	// controller running the default this line never appears at all.
	if len(s.AppSet.Pruned) > 0 {
		row("Pruned", strings.Join(s.AppSet.Pruned, ", "))
	}
	// Prune waiting on a first reconcile is normal for a few seconds after
	// startup and a problem if it persists, and the line reads the same either
	// way — which is the point. Without it, "prune is enabled and nothing has
	// been pruned" has no visible explanation at all.
	if len(s.AppSet.PruneHeldBy) > 0 {
		row("Prune held", "waiting for "+strings.Join(s.AppSet.PruneHeldBy, ", "))
	}
}
