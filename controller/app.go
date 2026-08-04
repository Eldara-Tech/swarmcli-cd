// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"bytes"
	"context"
	"crypto/tls"
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
  --ca-cert <path>     PEM certificate to trust for an https --server (default
                       $` + client.EnvCACert + `). Needed for a self-signed
                       certificate, which is what a controller given --tls-cert
                       usually presents
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
		return usageErr(stderr, fmt.Sprintf("unknown command '%s'", sub), appUsage)
	}
}

// appCommand parses the flags every app subcommand shares and runs one.
func appCommand(sub string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("app "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "")
	caCert := fs.String("ca-cert", "", "")
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
		return usageErr(stderr, fmt.Sprintf("invalid --output '%s' (want text or json)", *output), appUsage)
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
		return usageErr(stderr, fmt.Sprintf("unexpected argument '%s'", positional[0]), appUsage)
	}

	token, tokenErr := authz.TokenFromEnv(os.Getenv, os.ReadFile)
	c, err := client.NewWithOptions(resolveServer(*server, os.Getenv), token,
		client.Options{CACert: resolveCACert(*caCert, os.Getenv)})
	if err != nil {
		return fail(stderr, err)
	}
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

// resolveCACert prefers the flag, then the environment, then the system pool.
//
// The same precedence as resolveServer, and deliberately the same shape: the
// two are set together — stack.yml's TLS example exports both — so a second
// precedence rule in this package would be one an operator has to remember.
func resolveCACert(flagValue string, getenv func(string) string) string {
	if flagValue != "" {
		return flagValue
	}
	return getenv(client.EnvCACert)
}

// explain turns a rejected request into something actionable. A 401 with no
// token configured is a different problem from a 401 with the wrong one, and
// only the API can tell the caller which of the two it saw.
func explain(err error, tokenErr error) error {
	// A certificate this client would not accept otherwise arrives as a raw
	// x509 sentence inside a URL error, naming no remedy at all — and it is
	// what an operator meets the first time --server points at a controller
	// running the self-signed certificate stack.yml's TLS example deploys.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return fmt.Errorf("%w: pass --ca-cert, or set %s, to the certificate that signed it", err, client.EnvCACert)
	}

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

	// The drift column only appears when something answers it. Every
	// application runs the default manifest mode, and a column of dashes down a
	// list of twenty would bury the one row that has something to say — the same
	// reason a release that declared no engine requirement reports no compat
	// finding rather than "unknown".
	showDrift := false
	for _, v := range apps.Applications {
		if v.Status.Drift != nil {
			showDrift = true
			break
		}
	}

	// The reconcile column appears on the same terms, and for a sharper reason.
	// SYNC, HEALTH and REVISION are all the last *successful* observation, and
	// nothing moves them when a reconcile never gets as far as one — so an
	// application whose repository has been unreachable for a week reported last
	// week's plan under an observedAt of three seconds ago, and the row read
	// green (#107). This is the column that says not to believe the others.
	showError := false
	for _, v := range apps.Applications {
		if v.Status.Error != "" {
			showError = true
			break
		}
	}

	headers := []string{"NAME", "SYNC"}
	if showDrift {
		headers = append(headers, "DRIFT")
	}
	headers = append(headers, "HEALTH", "SERVICES", "REVISION")
	if showError {
		headers = append(headers, "RECONCILE")
	}

	rows := make([][]string, 0, len(apps.Applications))
	for _, v := range apps.Applications {
		row := []string{v.Spec.Name, state(v.Status.Sync.State)}
		if showDrift {
			row = append(row, driftColumn(v.Status.Drift))
		}
		row = append(row,
			state(v.Status.Health.State),
			services(v.Status.Health.Services),
			short(v.Status.Sync.Revision),
		)
		if showError {
			row = append(row, reconcileColumn(v.Status.Error))
		}
		rows = append(rows, row)
	}
	table(out, headers, rows)
	printReconcileErrors(out, apps.Applications)
	return nil
}

// driftColumn renders the live axis for one row. A dash rather than "unknown"
// for an application that does not use the mode: it was not asked, which is a
// different thing from asked and unanswerable, and that second case is the one
// "unknown" has to keep for itself.
func driftColumn(d *application.Drift) string {
	if d == nil {
		return "-"
	}
	return state(d.State)
}

// reconcileColumn says whether the last reconcile got through. "ok" rather than
// a dash for the ones that did: unlike the drift column, every application was
// asked, so a dash would claim the question does not apply here — and this
// column only exists at all because one of them answered badly.
func reconcileColumn(err string) string {
	if err == "" {
		return "ok"
	}
	return "failed"
}

// printReconcileErrors names why, under the table rather than in it.
//
// The choice printRollbacks makes, for the same reason: a reconcile error is a
// sentence — a repository URL and a dial error, a chart that would not render —
// and a cell wide enough for one would wreck every other row. The column says
// which application to stop trusting; this says what happened to it, so the
// common case needs no second command.
func printReconcileErrors(out io.Writer, apps []application.View) {
	first := true
	for _, v := range apps {
		if v.Status.Error == "" {
			continue
		}
		if first {
			_, _ = fmt.Fprintln(out)
			first = false
		}
		_, _ = fmt.Fprintf(out, "%s: the last reconcile failed — %s\n", v.Spec.Name, v.Status.Error)
	}
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
	if d := view.Status.Drift; d != nil {
		_, _ = fmt.Fprintf(out, "  Drift        %s%s\n", state(d.State), driftMessage(d.Message))
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
	// The wave column appears only for an application whose release file declares
	// more than one. Every other application would get a column of zeroes, which
	// is a column that says nothing.
	waved := spansWaves(view.Status.Releases)

	rows := make([][]string, 0, len(view.Status.Releases))
	for _, r := range view.Status.Releases {
		row := []string{
			r.Name, r.Chart, r.Version, fmt.Sprint(r.Revision),
			state(r.Action), state(r.Sync), state(r.Health.State),
		}
		if waved {
			row = append(row, fmt.Sprint(r.Wave))
		}
		rows = append(rows, row)
	}
	headers := []string{"RELEASE", "CHART", "VERSION", "REV", "ACTION", "SYNC", "HEALTH"}
	if waved {
		headers = append(headers, "WAVE")
	}
	_, _ = fmt.Fprintln(out)
	table(out, headers, rows)

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

	printDrift(out, view.Status.Releases)
	return nil
}

// spansWaves reports whether an application's releases declare more than one
// wave, which is the only case where the ordering is worth a column. The
// releases are already listed in the order a sync applies them, so a reader with
// no waves declared loses nothing by not being told they are all wave 0.
func spansWaves(releases []application.ReleaseStatus) bool {
	for _, r := range releases {
		if r.Wave != releases[0].Wave {
			return true
		}
	}
	return false
}

// printDrift lists what no longer matches the repository, per release.
//
// Separate from the service tables above because it answers a different
// question: those say whether what is running is working, this says whether it
// is what git asked for. A release with nothing to report prints nothing — and
// that is only a release that was compared and converged, or one that was not
// compared at all.
//
// A release whose state is Unknown has no findings and is emphatically not one
// of those: it is the record that the question was asked and went unanswered,
// and the reconciler will not converge drift it could not read. Skipping it for
// having no rows was #199. The application line above prints one such message,
// but it prints the *worst* release — Detected outranks Unknown in the rollup —
// so an unread release beside a drifted one had nothing anywhere naming it.
func printDrift(out io.Writer, releases []application.ReleaseStatus) {
	for _, r := range releases {
		if r.Drift == nil || r.Drift.State == application.DriftStateNone {
			continue
		}
		rows := make([][]string, 0, len(r.Drift.Services)+len(r.Drift.Resources))
		for _, s := range r.Drift.Services {
			reason := driftReason(s)
			if len(s.Fields) == 0 {
				// Missing or unexpected: the service itself is the finding, and
				// there is no field to name.
				rows = append(rows, []string{"service", s.Name, reason, "", "", ""})
				continue
			}
			for _, f := range s.Fields {
				rows = append(rows, []string{"service", s.Name, reason, f.Field, f.Desired, f.Live})
			}
			if s.Truncated > 0 {
				rows = append(rows, []string{"service", s.Name, reason,
					fmt.Sprintf("(and %d more)", s.Truncated), "", ""})
			}
		}
		// Always orphaned, and never field-compared: a network cannot be updated
		// in place and a config is immutable, so the only finding either can
		// produce is that the repository has stopped declaring it.
		for _, res := range r.Drift.Resources {
			rows = append(rows, []string{string(res.Kind), res.Name, "unexpected (orphaned)", "", "", ""})
		}
		_, _ = fmt.Fprintf(out, "\n%s drift:\n", r.Name)
		if len(rows) == 0 {
			// The state and its message are the whole record. Said as a sentence
			// rather than a one-row table for the reason printRollbacks gives:
			// this message is a daemon's account of a read that failed, and a
			// cell wide enough for one would be a table of nothing else.
			_, _ = fmt.Fprintf(out, "  %s%s\n", state(r.Drift.State), driftMessage(r.Drift.Message))
			continue
		}
		// A record can carry both, and the message is then a caveat on the table
		// rather than the record: a sweeping application whose manifest would not
		// resolve — a config a settled release mounts, deleted by hand — loses its
		// field comparison and still reports the orphans the sweep found, so the
		// state reads Detected with the failed read's message still on it. What
		// follows is what was found; this line is what could not be looked for.
		// Without it the table reads as the whole answer.
		if r.Drift.Message != "" {
			_, _ = fmt.Fprintf(out, "  %s\n", r.Drift.Message)
		}
		table(out, []string{"KIND", "NAME", "REASON", "FIELD", "DESIRED", "LIVE"}, rows)
		printRollbacks(out, r.Drift.Services)
	}
}

// printRollbacks explains the one reason a sync will not put right.
//
// It goes under the table rather than in it: the daemon's account of a failed
// rollout is a sentence, and a REASON cell wide enough for it would wreck every
// other row. What it has to convey is that this is not waiting to be corrected —
// the platform rejected the spec the repository asks for, so the next sync will
// leave it exactly where it is, and the remedy is a commit.
func printRollbacks(out io.Writer, services []application.ServiceDrift) {
	for _, s := range services {
		if s.Reason != application.DriftRolledBack {
			continue
		}
		_, _ = fmt.Fprintf(out, "\n  %s was rolled back by Swarm; a sync will not re-apply it.\n", s.Name)
		if s.Message != "" {
			_, _ = fmt.Fprintf(out, "  %s\n", s.Message)
		}
	}
}

// driftReason renders one finding, marking an unexpected service this
// application provably installed and no longer declares.
//
// The distinction is the whole of what makes deleting one safe, so it belongs in
// front of an operator before they enable anything: an orphaned service is what
// a sync will remove once syncPolicy.pruneResources is on, and an unexpected one
// without the marker is something this controller will never touch.
func driftReason(s application.ServiceDrift) string {
	if s.Orphaned {
		return state(s.Reason) + " (orphaned)"
	}
	return state(s.Reason)
}

// driftMessage suffixes the state with its reason, where there is one.
func driftMessage(message string) string {
	if message == "" {
		return ""
	}
	return " — " + message
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

// awaitSync polls until the controller reaches a terminal state for the sync.
//
// A reconcile records twice: it records the plan before applying it — which
// advances ObservedAt with the still-out-of-sync state and no result — and then
// records the outcome after the apply. Waiting on ObservedAt alone returns on
// that first record, mid-deploy, reporting the rollout as already finished. So
// the wait keys on what the two later records actually set:
//
//   - an error, from a sync that failed before it could deploy;
//   - a new SyncResult, from an apply that ran (whether it succeeded or not);
//   - a Synced state with neither, from a sync that found nothing to deploy.
//
// The out-of-sync pre-apply record is none of these, so the wait sits through
// it — which is the whole point. ObservedAt still gates the check, so a poll
// that races ahead of the controller's first record does not conclude early.
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
		if s := view.Status; s.ObservedAt.After(before.ObservedAt) && terminal(before, s) {
			return s, nil
		}

		select {
		case <-ctx.Done():
			return application.Status{}, timedOut
		case <-ticker.C:
		}
	}
}

// terminal reports whether after is a state the sync will not move on from: it
// failed, it recorded a result, or it settled synced with nothing to do. An
// out-of-sync record with no result yet is the apply still in flight.
func terminal(before, after application.Status) bool {
	return after.Error != "" ||
		changed(before.Sync.LastSync, after.Sync.LastSync) ||
		after.Sync.State == application.SyncSynced
}

// changed reports whether last is a new sync result relative to before. A nil
// last is no result at all — the pre-apply record carries the previous one
// forward — so it is not a change.
func changed(before, last *application.SyncResult) bool {
	if last == nil {
		return false
	}
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
