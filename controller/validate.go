// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"strings"
	"time"

	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/Eldara-Tech/swarmcli-cd/application"

	"github.com/Eldara-Tech/swarmcli-cd/config"
)

const validateUsage = `Usage: swarmcli-cd validate [options]

Validates an applications file and exits without starting the controller. This
is the check to run in the app set's own CI: it is the same parse and the same
rules the controller applies before it will swap a new set in.

It reads the file and nothing else. A file that validates can still fail to
deploy — whether a releaseFile or chart path exists in its repository, whether
a destination names a swarm this controller knows, and whether a registryAuth
secret is mounted are all answered at reconcile time, against things this
command cannot see.

Options:
  --file <path>   Applications file path (default ` + defaultAppSetFile + `)
`

// runValidate checks one applications file and exits with a plain verdict.
func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	file := fs.String("file", defaultAppSetFile, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, validateUsage)
			return 0
		}
		return usageErr(stderr, err.Error(), validateUsage)
	}
	if fs.NArg() > 0 {
		return usageErr(stderr, fmt.Sprintf("unexpected argument '%s'", fs.Arg(0)), validateUsage)
	}

	cfg, err := config.Load(*file)
	if err != nil {
		return fail(stderr, err)
	}
	if err := checkIntervals(cfg.Applications); err != nil {
		return fail(stderr, err)
	}
	noun := "applications"
	if len(cfg.Applications) == 1 {
		noun = "application"
	}
	_, _ = fmt.Fprintf(stdout, "OK: %s (%d %s)\n", cfg.Path, len(cfg.Applications), noun)
	return 0
}

// checkIntervals refuses an application asking to reconcile faster than the
// floor allows.
//
// Here and not in config.Load, which is the same code path the running
// controller loads its app set through. Refusing there would mean one bad
// interval stopped the whole file from being applied, freezing every other
// application's updates — so the controller clamps and says so, and this is
// where an operator finds out before committing rather than afterwards from a
// log line. Refusing is the point of a validator: it is the one context with
// somebody present to fix it.
func checkIntervals(apps []application.Spec) error {
	var bad []string
	for _, app := range apps {
		if d := time.Duration(app.SyncPolicy.Interval); d > 0 && d < application.MinInterval {
			bad = append(bad, fmt.Sprintf("'%s': syncPolicy.interval %s is below the %s floor", app.Name, d, application.MinInterval))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return errors.New(strings.Join(bad, "; "))
}
