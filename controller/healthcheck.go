// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/client"
)

// A var rather than a const only because it quotes the default timeout.
var healthcheckUsage = `Usage: swarmcli-cd healthcheck [options]

Probes a controller's liveness endpoint and exits 0 when it answers. This is
what stack.yml's healthcheck runs — the image declares no HEALTHCHECK of its
own, so the deployment chooses its own timings — and it is a subcommand rather
than a curl because the image carries this binary and need carry nothing else.

Options:
  --server <url>       Controller to probe (default $` + client.EnvServer + `,
                       or ` + client.DefaultServer + `). An https URL naming a
                       loopback address is probed without verifying the
                       certificate — this asserts liveness, not identity, and
                       the connection never leaves the host. A controller
                       serving TLS is probed as https://127.0.0.1:8080
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
		return usageErr(stderr, fmt.Sprintf("unexpected argument '%s'", fs.Arg(0)), healthcheckUsage)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	target := resolveServer(*server, os.Getenv)

	// No token: /healthz is unauthenticated by design, and a healthcheck that
	// needed a credential would have to be given one in the stack file.
	//
	// The probe derives its scheme instead of learning the controller's
	// configuration, which it cannot: this is a separate process invocation and
	// knows nothing about --tls-cert. Given an https loopback URL it skips
	// verification; anything else verifies normally. See client.Options.
	c, err := client.NewWithOptions(target, "", client.Options{SkipVerifyOnLoopback: true})
	if err != nil {
		return fail(stderr, err)
	}
	if err := c.Health(ctx); err != nil {
		return fail(stderr, tlsHint(target, err))
	}
	_, _ = fmt.Fprintln(stdout, "ok")
	return 0
}

// tlsHint names the one failure the status line hides.
//
// Go's server answers a plaintext request to a TLS listener with 400 and a body
// reading "Client sent an HTTP request to an HTTPS server", but client.message
// falls back to the status line for any non-JSON body — deliberately, because a
// proxy's HTML error page is the case an operator is far more likely to meet. So
// without this the operator of a task Swarm is restarting every interval reads
// "400 bad request" in `docker inspect` and nothing that says TLS, which is the
// concrete mechanism behind "nothing in the logs says TLS".
//
// Targeted at this one path rather than widening message: this is the only
// caller that knows the request was a liveness probe of a controller that may
// have been given --tls-cert.
func tlsHint(server string, err error) error {
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest ||
		!strings.HasPrefix(server, "http://") {
		return err
	}
	return fmt.Errorf("%w: if the controller was given --tls-cert, probe %s instead",
		err, "https://"+strings.TrimPrefix(server, "http://"))
}
