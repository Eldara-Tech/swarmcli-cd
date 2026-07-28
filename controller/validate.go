// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/Eldara-Tech/swarmcli-cd/config"
)

const defaultValidateFile = "applications.yaml"

var validateUsage = `Usage: swarmcli-cd validate [options]

Validates an applications file and exits without starting the controller.

Options:
  --file <path>   Applications file path (default ` + defaultValidateFile + `)
`

// runValidate checks one applications file and exits with a plain verdict.
func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	file := fs.String("file", defaultValidateFile, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, validateUsage)
			return 0
		}
		return usageErr(stderr, err.Error(), validateUsage)
	}
	if fs.NArg() > 0 {
		return usageErr(stderr, fmt.Sprintf("unexpected argument %q", fs.Arg(0)), validateUsage)
	}

	cfg, err := config.Load(*file)
	if err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "OK: %s (%d applications)\n", cfg.Path, len(cfg.Applications))
	return 0
}
