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
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/client"
)

// A var rather than a const only because it quotes the default timeout.
var healthcheckUsage = `Usage: swarmcli-cd healthcheck [options]

Probes a controller's liveness endpoint and exits 0 when it answers. This is
what the container's HEALTHCHECK runs, which is why it is a subcommand rather
than a curl: the image carries this binary and need carry nothing else.

Options:
  --server <url>       Controller to probe (default $` + client.EnvServer + `,
                       or ` + client.DefaultServer + `)
  --timeout <dur>      Give up after this long (default ` + healthcheckTimeout.String() + `)
`

// healthcheckTimeout bounds the probe. A container healthcheck is killed by the
// daemon at its own timeout anyway, and reporting unhealthy quickly is the
// point of the exercise.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck probes /healthz and turns the answer into an exit code.
func runHealthcheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "")
	timeout := fs.Duration("timeout", healthcheckTimeout, "")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, healthcheckUsage)
			return 0
		}
		return usageErr(stderr, err.Error(), healthcheckUsage)
	}
	if fs.NArg() > 0 {
		return usageErr(stderr, fmt.Sprintf("unexpected argument %q", fs.Arg(0)), healthcheckUsage)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// No token: /healthz is unauthenticated by design, and a healthcheck that
	// needed a credential would have to be given one in the stack file.
	if err := client.New(resolveServer(*server, os.Getenv), "").Health(ctx); err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintln(stdout, "ok")
	return 0
}
