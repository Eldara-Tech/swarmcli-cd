// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/client"
)

// appUsage is a var rather than a const only because it quotes the default
// timeout, which no const can produce.
var appUsage = `Usage: swarmcli-cd app <command> [options]

Commands:
  list                 List applications with their sync state and health
  get <app>            Show one application, its releases and their services
  diff <app>           Show what a sync would change
  history <app>        Show each release's revisions, newest first
  sync <app>           Reconcile now

Options:
  --server <url>       Controller to talk to (default $` + client.EnvServer + `,
                       or ` + client.DefaultServer + `)
  -o, --output <fmt>   text or json (default text). json is the controller's
                       own response, unmodified
  --wait               sync: wait for the sync to finish, and exit non-zero if
                       it failed
  --timeout <dur>      sync: how long --wait waits (default ` + defaultSyncTimeout.String() + `)

The admin token comes from ` + authz.EnvTokenFile + ` or
` + authz.EnvToken + `, never from a flag: a token in argv is a
token in "ps" and in the shell history.

Every command reads the controller's API, so anything it shows a UI can show
too, and nothing it does needs access to the swarm itself.
`

// defaultSyncTimeout bounds --wait. A sync under a wait policy blocks until the
// rollout converges, so this has to outlast a real deployment rather than a
// request.
const defaultSyncTimeout = 5 * time.Minute

// syncPollInterval is how often --wait re-reads the application. It is a
// variable so tests do not have to spend it.
var syncPollInterval = 2 * time.Second

// appMain dispatches `swarmcli-cd app ...`.
func appMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stdout, appUsage)
		return 0
	}

	sub, rest := args[0], args[1:]
	switch sub {
	case "help", "--help", "-h":
		_, _ = fmt.Fprint(stdout, appUsage)
		return 0
	case "list", "get", "diff", "history", "sync":
		return appCommand(sub, rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown command %q", sub), appUsage)
	}
}

// appCommand parses the flags every app subcommand shares and runs one.
func appCommand(sub string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("app "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "")
	output := fs.String("output", "text", "")
	fs.StringVar(output, "o", "text", "")
	wait := fs.Bool("wait", false, "")
	timeout := fs.Duration("timeout", defaultSyncTimeout, "")

	positional, err := parseWithPositionals(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, appUsage)
			return 0
		}
		return usageErr(stderr, err.Error(), appUsage)
	}

	if *output != "text" && *output != "json" {
		return usageErr(stderr, fmt.Sprintf("invalid --output %q (want text or json)", *output), appUsage)
	}

	// list takes no application; the rest take exactly one. Saying so is what
	// keeps `app get` from quietly listing everything.
	var app string
	if sub != "list" {
		if len(positional) == 0 {
			return usageErr(stderr, fmt.Sprintf("%s needs an application name", sub), appUsage)
		}
		app = positional[0]
		positional = positional[1:]
	}
	if len(positional) > 0 {
		return usageErr(stderr, fmt.Sprintf("unexpected argument %q", positional[0]), appUsage)
	}

	token, tokenErr := authz.TokenFromEnv(os.Getenv, os.ReadFile)
	c := client.New(resolveServer(*server, os.Getenv), token)
	ctx := context.Background()

	var runErr error
	switch sub {
	case "list":
		runErr = appList(ctx, c, stdout, *output)
	case "get":
		runErr = appGet(ctx, c, stdout, *output, app)
	case "diff":
		runErr = appDiff(ctx, c, stdout, *output, app)
	case "history":
		runErr = appHistory(ctx, c, stdout, *output, app)
	case "sync":
		runErr = appSync(ctx, c, stdout, app, *wait, *timeout)
	}
	if runErr != nil {
		return fail(stderr, explain(runErr, tokenErr))
	}
	return 0
}

// parseWithPositionals parses flags that may appear before, after or between
// positional arguments. The flag package stops at the first non-flag argument,
// which would make `app get edge -o json` silently ignore the -o.
func parseWithPositionals(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// resolveServer prefers the flag, then the environment, then this host.
func resolveServer(flagValue string, getenv func(string) string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := getenv(client.EnvServer); v != "" {
		return v
	}
	return client.DefaultServer
}

// explain turns a rejected request into something actionable. A 401 with no
// token configured is a different problem from a 401 with the wrong one, and
// only the API can tell the caller which of the two it saw.
func explain(err error, tokenErr error) error {
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		if tokenErr != nil {
			return fmt.Errorf("%w: %w", err, tokenErr)
		}
		return fmt.Errorf("%w: the controller rejected the token from %s", err, tokenSource(os.Getenv))
	default:
		return err
	}
}

// tokenSource names where the token that was presented came from, so that a
// rejection points at the variable to fix rather than at both of them.
func tokenSource(getenv func(string) string) string {
	if getenv(authz.EnvTokenFile) != "" {
		return authz.EnvTokenFile
	}
	return authz.EnvToken
}

func appList(ctx context.Context, c *client.Client, out io.Writer, format string) error {
	apps, raw, err := c.List(ctx)
	if err != nil {
		return err
	}
	if format == "json" {
		return writeJSON(out, raw)
	}

	if len(apps.Applications) == 0 {
		_, _ = fmt.Fprintln(out, "No applications.")
		return nil
	}
	rows := make([][]string, 0, len(apps.Applications))
	for _, v := range apps.Applications {
		rows = append(rows, []string{
			v.Spec.Name,
			state(v.Status.Sync.State),
			state(v.Status.Health.State),
			services(v.Status.Health.Services),
			short(v.Status.Sync.Revision),
		})
	}
	table(out, []string{"NAME", "SYNC", "HEALTH", "SERVICES", "REVISION"}, rows)
	return nil
}

func appGet(ctx context.Context, c *client.Client, out io.Writer, format, app string) error {
	view, raw, err := c.Get(ctx, app)
	if err != nil {
		return err
	}
	if format == "json" {
		return writeJSON(out, raw)
	}

	_, _ = fmt.Fprintf(out, "%s\n", view.Spec.Name)
	_, _ = fmt.Fprintf(out, "  Source       %s @ %s\n", view.Spec.Source.RepoURL, view.Spec.Source.Revision)
	if ch := view.Spec.Source.Chart; ch != nil {
		_, _ = fmt.Fprintf(out, "  Chart        %s as %s\n", chartRef(*ch), ch.Release)
	} else {
		_, _ = fmt.Fprintf(out, "  Release file %s\n", view.Spec.Source.ReleaseFile)
	}
	_, _ = fmt.Fprintf(out, "  Destination  %s\n", destination(view.Spec.Destination))
	_, _ = fmt.Fprintf(out, "  Sync         %s%s\n", state(view.Status.Sync.State), revisionSuffix(view.Status.Sync.Revision))
	_, _ = fmt.Fprintf(out, "  Health       %s (%s services)\n", state(view.Status.Health.State), services(view.Status.Health.Services))
	if msg := view.Status.Health.Message; msg != "" {
		_, _ = fmt.Fprintf(out, "  Message      %s\n", msg)
	}
	if last := view.Status.Sync.LastSync; last != nil {
		_, _ = fmt.Fprintf(out, "  Last sync    %s at %s%s\n",
			outcome(last.Succeeded), last.FinishedAt.Format(time.RFC3339), revisionSuffix(last.Revision))
		if last.Error != "" {
			_, _ = fmt.Fprintf(out, "               %s\n", last.Error)
		}
	}
	if view.Status.Error != "" {
		_, _ = fmt.Fprintf(out, "  Error        %s\n", view.Status.Error)
	}
	_, _ = fmt.Fprintf(out, "  Observed     %s\n", observed(view.Status.ObservedAt))

	if len(view.Status.Releases) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(view.Status.Releases))
	for _, r := range view.Status.Releases {
		rows = append(rows, []string{
			r.Name, r.Chart, r.Version, fmt.Sprint(r.Revision),
			state(r.Action), state(r.Sync), state(r.Health.State),
		})
	}
	_, _ = fmt.Fprintln(out)
	table(out, []string{"RELEASE", "CHART", "VERSION", "REV", "ACTION", "SYNC", "HEALTH"}, rows)

	for _, r := range view.Status.Releases {
		if len(r.Services) == 0 {
			continue
		}
		serviceRows := make([][]string, 0, len(r.Services))
		for _, s := range r.Services {
			serviceRows = append(serviceRows, []string{
				s.Name, s.Mode, fmt.Sprintf("%d/%d", s.Running, s.Desired),
				state(s.Health), s.UpdateState, s.Message,
			})
		}
		_, _ = fmt.Fprintf(out, "\n%s:\n", r.Name)
		table(out, []string{"SERVICE", "MODE", "REPLICAS", "HEALTH", "UPDATE", "MESSAGE"}, serviceRows)
	}
	return nil
}

func appDiff(ctx context.Context, c *client.Client, out io.Writer, format, app string) error {
	diff, raw, err := c.Diff(ctx, app)
	if err != nil {
		return err
	}
	if format == "json" {
		return writeJSON(out, raw)
	}

	// Not an error, and not "no changes" either: the controller has not planned
	// this application yet, so it has nothing to compare.
	if !diff.Planned {
		_, _ = fmt.Fprintf(out, "%s has not been reconciled yet.\n", app)
		return nil
	}
	if len(diff.Releases) == 0 {
		_, _ = fmt.Fprintln(out, "No changes.")
		return nil
	}
	for i, r := range diff.Releases {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		_, _ = fmt.Fprintf(out, "=== %s (%s)\n", r.Release, state(r.Action))
		if strings.TrimSpace(r.Diff) == "" {
			_, _ = fmt.Fprintln(out, "(no manifest change)")
			continue
		}
		_, _ = fmt.Fprintln(out, strings.TrimRight(r.Diff, "\n"))
	}
	return nil
}

func appHistory(ctx context.Context, c *client.Client, out io.Writer, format, app string) error {
	history, raw, err := c.History(ctx, app)
	if err != nil {
		return err
	}
	if format == "json" {
		return writeJSON(out, raw)
	}

	if len(history.Releases) == 0 {
		_, _ = fmt.Fprintf(out, "%s has no releases.\n", app)
		return nil
	}
	for i, r := range history.Releases {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		_, _ = fmt.Fprintf(out, "%s\n", r.Name)
		// Declared but never deployed is a real state, and a different one from
		// "no such release".
		if len(r.Revisions) == 0 {
			_, _ = fmt.Fprintln(out, "  (never deployed)")
			continue
		}
		rows := make([][]string, 0, len(r.Revisions))
		for _, rev := range r.Revisions {
			rows = append(rows, []string{
				fmt.Sprint(rev.Revision), rev.Chart, rev.Version, rev.Status, rev.Created, rev.Owner,
			})
		}
		table(out, []string{"REV", "CHART", "VERSION", "STATUS", "CREATED", "OWNER"}, rows)
	}
	return nil
}

// appSync triggers a reconcile, and with wait follows it to its outcome.
func appSync(ctx context.Context, c *client.Client, out io.Writer, app string, wait bool, timeout time.Duration) error {
	// Read what the controller last observed before asking it to observe again.
	// Both timestamps are the controller's own, so the comparison is immune to
	// clock skew here, and taking it before the request means a sync that
	// finishes before the first poll is still seen.
	var before application.Status
	if wait {
		view, _, err := c.Get(ctx, app)
		if err != nil {
			return err
		}
		before = view.Status
	}

	if err := c.Sync(ctx, app); err != nil {
		return err
	}
	if !wait {
		_, _ = fmt.Fprintf(out, "Sync started for %s. Follow it with: swarmcli-cd app get %s\n", app, app)
		return nil
	}

	_, _ = fmt.Fprintf(out, "Syncing %s...\n", app)
	status, err := awaitSync(ctx, c, app, before, timeout)
	if err != nil {
		return err
	}
	return reportSync(out, app, before, status)
}

// reportSync turns the status the controller settled on into an outcome.
//
// The order matters. A sync that never reached the deploy — an unreachable
// repository, a chart that will not render — leaves no SyncResult at all and
// records its reason on the status, so the status error is checked first. A
// sync with nothing to deploy leaves the previous result in place and is a
// success: the swarm already matches git, which is what was asked for.
func reportSync(out io.Writer, app string, before, after application.Status) error {
	if after.Error != "" {
		return fmt.Errorf("sync of %s failed: %s", app, after.Error)
	}
	if last := after.Sync.LastSync; last != nil && changed(before.Sync.LastSync, last) {
		if !last.Succeeded {
			return fmt.Errorf("sync of %s failed: %s", app, last.Error)
		}
		_, _ = fmt.Fprintf(out, "Synced %s%s\n", app, revisionSuffix(last.Revision))
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s is already up to date%s\n", app, revisionSuffix(after.Sync.Revision))
	return nil
}

// awaitSync polls until the controller has observed the application again.
//
// ObservedAt is the completion signal rather than LastSync because a sync that
// fails before it deploys — an unreachable repository, a chart that will not
// render — never records a SyncResult at all, and neither does one that finds
// nothing to deploy. Waiting on LastSync would sit through the full timeout for
// both, and report a failure that had already happened as a timeout.
//
// It is the controller's own clock on both sides of the comparison. The cost is
// that a periodic reconcile landing between the read and the request is
// indistinguishable from the requested one; it is the same application against
// the same desired state, and the default interval is three minutes.
func awaitSync(ctx context.Context, c *client.Client, app string, before application.Status, timeout time.Duration) (application.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(syncPollInterval)
	defer ticker.Stop()

	timedOut := fmt.Errorf("timed out after %s waiting for %s to sync", timeout, app)

	for {
		view, _, err := c.Get(ctx, app)
		if err != nil {
			// The deadline lands mid-request as often as between them, and
			// "context deadline exceeded" would report the wait as a broken
			// connection rather than as the timeout it is.
			if ctx.Err() != nil {
				return application.Status{}, timedOut
			}
			return application.Status{}, err
		}
		if view.Status.ObservedAt.After(before.ObservedAt) {
			return view.Status, nil
		}

		select {
		case <-ctx.Done():
			return application.Status{}, timedOut
		case <-ticker.C:
		}
	}
}

// changed reports whether last is a different sync result from before.
func changed(before, last *application.SyncResult) bool {
	if before == nil {
		return true
	}
	return !last.FinishedAt.Equal(before.FinishedAt)
}

// writeJSON prints the controller's response as it arrived. It is not decoded
// and re-encoded: this output is a contract, and re-encoding would make it
// track this binary's types rather than the controller's.
func writeJSON(out io.Writer, raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		// Not valid JSON, which means it did not come from the API. Print it
		// anyway rather than swallowing whatever the caller was actually sent.
		_, err := out.Write(raw)
		return err
	}
	_, err := fmt.Fprintln(out, buf.String())
	return err
}

// table writes a tab-aligned table with a header row, matching the CE CLI's.
func table(out io.Writer, headers []string, rows [][]string) {
	tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	printRow(tw, headers)
	for _, r := range rows {
		printRow(tw, r)
	}
	_ = tw.Flush()
}

func printRow(w io.Writer, columns []string) {
	_, _ = fmt.Fprintln(w, strings.TrimRight(strings.Join(columns, "\t"), "\t"))
}

// state renders an enum member the way it marshals. The Unknown member is the
// empty string internally and "unknown" on the wire, and a blank column would
// read as "nothing to report" rather than "not known yet".
func state[T ~string](v T) string {
	if v == "" {
		return "unknown"
	}
	return string(v)
}

func services(c application.ServiceCounts) string { return fmt.Sprintf("%d/%d", c.Healthy, c.Total) }

// chartRef names a chart source the way the spec declares it: a path within the
// repository, or a repository reference at a pinned version.
func chartRef(c application.ChartSource) string {
	if c.Path != "" {
		return c.Path
	}
	if c.Version != "" {
		return c.Ref + " " + c.Version
	}
	return c.Ref
}

func destination(d application.Destination) string {
	if d.Swarm == "" {
		return "local swarm"
	}
	return d.Swarm
}

// observed renders when the controller last looked. An application it has not
// reached yet carries a zero time, and printing that as year 1 reads like a
// clock fault rather than "not yet".
func observed(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.Format(time.RFC3339)
}

func outcome(succeeded bool) string {
	if succeeded {
		return "succeeded"
	}
	return "failed"
}

func revisionSuffix(revision string) string {
	if revision == "" {
		return ""
	}
	return " at " + short(revision)
}

// short abbreviates a commit the way git does. The full revision is in the JSON
// output, which is what anything acting on it should be reading.
func short(revision string) string {
	if len(revision) <= 7 {
		return revision
	}
	return revision[:7]
}
