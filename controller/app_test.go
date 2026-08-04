// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/api"
	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/client"
	"github.com/Eldara-Tech/swarmcli-cd/drift"
)

// start runs the real API server over rec and returns the address that points
// the CLI at it.
func start(t *testing.T, rec *stubReconciler) string {
	t.Helper()
	srv := httptest.NewServer(apiHandler(t, rec))
	t.Cleanup(srv.Close)
	return srv.URL
}

// startTLS is start over a TLS listener, returning its https URL and the
// server's certificate written out as the PEM file --ca-cert takes.
//
// httptest's certificate already carries 127.0.0.1 and ::1 as SANs, so the URL
// is a loopback https one — which is what stack.yml's TLS example probes — and
// it is self-signed, so it is its own authority and the same file serves as the
// CA the CLI is pointed at.
func startTLS(t *testing.T, rec *stubReconciler) (server, caCert string) {
	t.Helper()
	srv := httptest.NewTLSServer(apiHandler(t, rec))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "controller.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv.URL, path
}

// apiHandler builds the real API server over rec. Testing against the actual
// server rather than a hand-written stub is what makes these tests cover the
// guard, the envelopes and the status codes as well as the rendering.
func apiHandler(t *testing.T, rec *stubReconciler) http.Handler {
	t.Helper()
	t.Setenv(authz.EnvTokenFile, "")
	t.Setenv(authz.EnvToken, "s3cret")
	// Not authz.Get(): the default authorizer read its token from the process
	// environment in init(), long before t.Setenv. This one expects the same
	// bearer credential the client will present, so the server's guard is still
	// the real one being exercised.
	swapAuthorizer(t, bearerAuthorizer{token: "s3cret"})

	h, err := api.New(rec, api.Options{}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// cli runs one command against the server and returns its exit code.
func cli(t *testing.T, server string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append(args, "--server", server), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAppList(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, stdout, stderr := cli(t, server, "app", "list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"NAME", "edge", "synced", "healthy", "2/2", "8b65cf9"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	// Abbreviated, not truncated to nothing: the full revision is in the JSON.
	if strings.Contains(stdout, "8b65cf951c7e") {
		t.Errorf("stdout = %q, want the revision abbreviated", stdout)
	}
}

// --output json is a contract with whatever calls it, so it has to be the
// controller's own bytes rather than a re-encoding of this binary's types.
func TestAppListJSONIsTheControllersOwnResponse(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, stdout, stderr := cli(t, server, "app", "list", "-o", "json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var got struct {
		Applications []application.View `json:"applications"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if len(got.Applications) != 1 || got.Applications[0].Spec.Name != "edge" {
		t.Errorf("decoded %+v, want one application named edge", got.Applications)
	}
	// The list endpoint strips per-release detail; re-encoding a decoded value
	// would have silently reinstated the empty array.
	if strings.Contains(stdout, "\"releases\"") {
		t.Errorf("stdout = %q, want the list response's own shape", stdout)
	}
}

func TestAppGet(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, stdout, stderr := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"edge", "https://example.com/infra.git @ main", "local swarm", "synced", "web", "web_nginx", "2/2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// A flag after the application name is the way people actually type it, and the
// flag package stops parsing at the first positional.
func TestFlagsMayFollowTheApplicationName(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, stdout, _ := cli(t, server, "app", "get", "edge", "-o", "json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !json.Valid([]byte(stdout)) {
		t.Errorf("stdout = %q, want JSON — the -o after the name was ignored", stdout)
	}
}

func TestAppGetUnknownApplication(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, _, stderr := cli(t, server, "app", "get", "nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no such application") {
		t.Errorf("stderr = %q, want the API's own message", stderr)
	}
}

// Not reconciled yet is neither an error nor an empty diff, and the difference
// matters to anyone deciding whether to sync.
func TestAppDiffBeforeTheFirstReconcile(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView(), diffErr: application.ErrNotPlanned})

	code, stdout, stderr := cli(t, server, "app", "diff", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "has not been reconciled yet") {
		t.Errorf("stdout = %q, want it to say so", stdout)
	}
}

func TestAppDiff(t *testing.T) {
	rec := &stubReconciler{view: syncedView(), diffs: []application.ReleaseDiff{
		{Release: "web", Action: "upgrade", Diff: "-image: nginx:1.0\n+image: nginx:1.1\n"},
	}}
	server := start(t, rec)

	code, stdout, stderr := cli(t, server, "app", "diff", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"=== web (upgrade)", "+image: nginx:1.1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestAppHistory(t *testing.T) {
	rec := &stubReconciler{view: syncedView(), history: application.History{Releases: []application.ReleaseHistory{
		{Name: "web", Revisions: []application.Revision{
			{Revision: 2, Chart: "swarmcli/nginx", Version: "0.1.1", Status: "deployed", Created: "2026-07-22T10:00:00Z", Owner: "swarmcli-cd"},
		}},
		{Name: "cache"},
	}}}
	server := start(t, rec)

	code, stdout, stderr := cli(t, server, "app", "history", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"web", "swarmcli/nginx", "deployed", "swarmcli-cd", "cache", "(never deployed)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestAppSyncReturnsWithoutWaiting(t *testing.T) {
	rec := &stubReconciler{view: syncedView()}
	server := start(t, rec)

	code, stdout, stderr := cli(t, server, "app", "sync", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Sync started") {
		t.Errorf("stdout = %q, want it to say the sync started", stdout)
	}
}

func TestAppSyncWaitsForSuccess(t *testing.T) {
	pollFast(t)
	rec := &stubReconciler{view: syncedView(), onSync: succeedAt(time.Now().Add(time.Minute))}
	server := start(t, rec)

	code, stdout, stderr := cli(t, server, "app", "sync", "edge", "--wait", "--timeout", "10s")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Synced edge") {
		t.Errorf("stdout = %q, want the outcome", stdout)
	}
}

// The exit code is the whole point of --wait: CI has nothing else to check.
func TestAppSyncWaitFailsWhenTheSyncFails(t *testing.T) {
	pollFast(t)
	rec := &stubReconciler{view: syncedView(), onSync: func(v *application.View) {
		observe(v)
		v.Status.Sync.LastSync = &application.SyncResult{
			FinishedAt: time.Now().Add(time.Minute),
			Succeeded:  false,
			Error:      "chart render failed",
		}
	}}
	server := start(t, rec)

	code, _, stderr := cli(t, server, "app", "sync", "edge", "--wait", "--timeout", "10s")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "chart render failed") {
		t.Errorf("stderr = %q, want the controller's reason", stderr)
	}
}

func TestAppSyncWaitTimesOut(t *testing.T) {
	pollFast(t)
	// A sync that never records an outcome: the wait must end by itself rather
	// than hanging whatever called it.
	rec := &stubReconciler{view: syncedView(), onSync: func(*application.View) {}}
	server := start(t, rec)

	code, _, stderr := cli(t, server, "app", "sync", "edge", "--wait", "--timeout", "100ms")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "timed out") {
		t.Errorf("stderr = %q, want a timeout", stderr)
	}
}

// A previous sync's result must not be mistaken for this one's.
func TestAppSyncWaitIgnoresAnEarlierResult(t *testing.T) {
	pollFast(t)
	earlier := time.Now().Add(-time.Hour)
	view := syncedView()
	view.Status.Sync.LastSync = &application.SyncResult{FinishedAt: earlier, Succeeded: true, Revision: "old"}

	rec := &stubReconciler{view: view, onSync: succeedAt(time.Now().Add(time.Minute))}
	server := start(t, rec)

	code, stdout, stderr := cli(t, server, "app", "sync", "edge", "--wait", "--timeout", "10s")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if strings.Contains(stdout, "old") {
		t.Errorf("stdout = %q, want the new sync's revision", stdout)
	}
}

func TestAppUsageErrors(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	for name, args := range map[string][]string{
		"unknown subcommand": {"app", "lst"},
		"unknown flag":       {"app", "list", "--verbose"},
		"missing name":       {"app", "get"},
		"extra argument":     {"app", "get", "edge", "web"},
		"invalid output":     {"app", "list", "-o", "yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := cli(t, server, args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if len(stdout) != 0 {
				t.Errorf("stdout = %q, want the error on stderr only", stdout)
			}
			if !strings.Contains(stderr, "Error:") {
				t.Errorf("stderr = %q, want an error", stderr)
			}
		})
	}
}

// A rejected token must point at the variable that supplied it, rather than
// leaving an operator to guess which of the two the client used.
func TestRejectedTokenNamesItsSource(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})
	t.Setenv(authz.EnvToken, "wrong")

	code, _, stderr := cli(t, server, "app", "list")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, authz.EnvToken) {
		t.Errorf("stderr = %q, want it to name %s", stderr, authz.EnvToken)
	}
}

func TestAppHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{{"app"}, {"app", "help"}, {"app", "list", "--help"}} {
		stdout.Reset()
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, code)
		}
		if !strings.Contains(stdout.String(), "swarmcli-cd app <command>") {
			t.Errorf("run(%v) printed %q, want the usage", args, stdout.String())
		}
	}
}

// terminal is the heart of --wait, and the two-phase reconcile record is what a
// stub cannot model: a reconcile records its plan (out of sync, no result)
// before it applies, and only then records the outcome. Waiting must sit through
// the first and conclude on the second — the integration test caught --wait
// returning mid-deploy when this was keyed on ObservedAt alone.
func TestTerminal(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	later := at.Add(time.Minute)

	for name, tc := range map[string]struct {
		before, after application.Status
		want          bool
	}{
		"pre-apply out-of-sync record, apply still in flight": {
			before: application.Status{Sync: application.Sync{State: application.SyncOutOfSync}},
			after:  application.Status{Sync: application.Sync{State: application.SyncOutOfSync}},
			want:   false,
		},
		"apply recorded a result": {
			before: application.Status{},
			after:  application.Status{Sync: application.Sync{State: application.SyncSynced, LastSync: &application.SyncResult{FinishedAt: at, Succeeded: true}}},
			want:   true,
		},
		"a new result replaces an earlier one, even if still out of sync": {
			before: application.Status{Sync: application.Sync{LastSync: &application.SyncResult{FinishedAt: at}}},
			after:  application.Status{Sync: application.Sync{State: application.SyncOutOfSync, LastSync: &application.SyncResult{FinishedAt: later}}},
			want:   true,
		},
		"sync failed before it deployed": {
			before: application.Status{},
			after:  application.Status{Error: "fetching source: repository not found"},
			want:   true,
		},
		"nothing to deploy, already synced": {
			before: application.Status{Sync: application.Sync{State: application.SyncSynced}},
			after:  application.Status{Sync: application.Sync{State: application.SyncSynced}},
			want:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := terminal(tc.before, tc.after); got != tc.want {
				t.Errorf("terminal = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveServer(t *testing.T) {
	env := func(string) string { return "http://from-env:8080" }
	if got := resolveServer("http://from-flag:9090", env); got != "http://from-flag:9090" {
		t.Errorf("resolveServer = %q, want the flag to win", got)
	}
	if got := resolveServer("", env); got != "http://from-env:8080" {
		t.Errorf("resolveServer = %q, want the environment", got)
	}
	if got := resolveServer("", func(string) string { return "" }); got != client.DefaultServer {
		t.Errorf("resolveServer = %q, want the default", got)
	}
}

// bearerAuthorizer accepts one token, as the default authorizer does, but reads
// it from the test rather than from the environment at init time.
type bearerAuthorizer struct{ token string }

func (bearerAuthorizer) Ready() error { return nil }

func (a bearerAuthorizer) Authenticate(r *http.Request) (authz.Subject, error) {
	if r.Header.Get("Authorization") != "Bearer "+a.token {
		return authz.Subject{}, errors.New("invalid or missing bearer token")
	}
	return authz.Subject{Name: "admin"}, nil
}

func (bearerAuthorizer) Authorize(context.Context, authz.Subject, authz.Action, string) error {
	return nil
}

func (bearerAuthorizer) Visible(_ context.Context, _ authz.Subject, _ authz.Action, apps []string) ([]string, error) {
	return apps, nil
}

// errNoSuchApplication is what the stub returns for an application it does not
// have; the API turns it into the response the client sees.
var errNoSuchApplication = errors.New("no such application")

// A sync that fails before it deploys — an unreachable repository, a chart that
// will not render — records no SyncResult at all, only a status error. Waiting
// on LastSync alone reported that as a timeout, which is how this was found.
func TestAppSyncWaitReportsAFailureThatNeverDeployed(t *testing.T) {
	pollFast(t)
	rec := &stubReconciler{view: syncedView(), onSync: func(v *application.View) {
		observe(v)
		v.Status.Error = "fetching source: repository not found"
	}}
	server := start(t, rec)

	code, _, stderr := cli(t, server, "app", "sync", "edge", "--wait", "--timeout", "10s")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "repository not found") {
		t.Errorf("stderr = %q, want the controller's reason", stderr)
	}
}

// Nothing to deploy is a success: the swarm already matches git, which is what
// was asked for. It records no SyncResult either.
func TestAppSyncWaitOnAnUpToDateApplication(t *testing.T) {
	pollFast(t)
	rec := &stubReconciler{view: syncedView(), onSync: observe}
	server := start(t, rec)

	code, stdout, stderr := cli(t, server, "app", "sync", "edge", "--wait", "--timeout", "10s")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "already up to date") {
		t.Errorf("stdout = %q, want it to say so", stdout)
	}
}

// pollFast shortens the wait loop for the duration of one test.
func pollFast(t *testing.T) {
	t.Helper()
	original := syncPollInterval
	syncPollInterval = time.Millisecond
	t.Cleanup(func() { syncPollInterval = original })
}

// observe advances what the controller has observed, which is what the wait
// watches. Every onSync below does it, because every real reconcile does.
func observe(v *application.View) { v.Status.ObservedAt = time.Now().Add(time.Minute) }

func succeedAt(finished time.Time) func(*application.View) {
	return func(v *application.View) {
		observe(v)
		v.Status.Sync.LastSync = &application.SyncResult{
			Revision:   "8b65cf951c7e",
			FinishedAt: finished,
			Succeeded:  true,
		}
	}
}

func syncedView() application.View {
	return application.View{
		Spec: application.Spec{
			Name: "edge",
			Source: application.Source{
				RepoURL:     "https://example.com/infra.git",
				Revision:    "main",
				ReleaseFile: "swarmcli-release.yaml",
			},
		},
		Status: application.Status{
			Sync:       application.Sync{State: application.SyncSynced, Revision: "8b65cf951c7e"},
			Health:     application.Health{State: application.HealthHealthy, Services: application.ServiceCounts{Healthy: 2, Total: 2}},
			ObservedAt: time.Now(),
			Releases: []application.ReleaseStatus{{
				Name: "web", Chart: "swarmcli/nginx", Version: "0.1.1", Revision: 2,
				Action: application.ActionUnchanged,
				Sync:   application.SyncSynced,
				Health: application.Health{State: application.HealthHealthy},
				Services: []application.ServiceStatus{
					{Name: "web_nginx", Mode: "replicated", Running: 2, Desired: 2, Health: application.HealthHealthy},
				},
			}},
		},
	}
}

// stubReconciler is the API's dependency, not the CLI's: these tests drive the
// real server, and this is what it serves.
type stubReconciler struct {
	mu      sync.Mutex
	view    application.View
	diffs   []application.ReleaseDiff
	diffErr error
	history application.History
	onSync  func(*application.View)
}

func (s *stubReconciler) Views() []application.View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []application.View{s.view}
}

func (s *stubReconciler) View(app string) (application.View, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if app != s.view.Spec.Name {
		return application.View{}, false
	}
	return s.view, true
}

func (s *stubReconciler) Diffs(app string) ([]application.ReleaseDiff, error) {
	if app != s.view.Spec.Name {
		return nil, errNoSuchApplication
	}
	return s.diffs, s.diffErr
}

func (s *stubReconciler) History(_ context.Context, app string) (application.History, error) {
	if app != s.view.Spec.Name {
		return application.History{}, errNoSuchApplication
	}
	return s.history, nil
}

func (s *stubReconciler) AcceptSync(app string) (func(context.Context) error, error) {
	return func(ctx context.Context) error { return s.SyncNow(ctx, app) }, nil
}

func (s *stubReconciler) SyncNow(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onSync != nil {
		s.onSync(&s.view)
	}
	return nil
}

// An application the controller has not reached yet carries a zero time, and
// year 1 reads like a clock fault rather than "not yet".
func TestGetRendersAnUnobservedApplication(t *testing.T) {
	view := syncedView()
	view.Status = application.Status{}
	server := start(t, &stubReconciler{view: view})

	code, stdout, stderr := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Observed     never") {
		t.Errorf("stdout = %q, want it to say never", stdout)
	}
	if strings.Contains(stdout, "0001-01-01") {
		t.Errorf("stdout = %q, want no zero time", stdout)
	}
}

func TestHealthcheck(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, stdout, stderr := cli(t, server, "healthcheck")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "ok") {
		t.Errorf("stdout = %q, want ok", stdout)
	}
}

// The probe carries no credential, because /healthz takes none: a healthcheck
// that needed one would have to be given it in the stack file.
func TestHealthcheckNeedsNoToken(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})
	t.Setenv(authz.EnvToken, "")

	if code, _, stderr := cli(t, server, "healthcheck"); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
}

// The loopback exception must not leak out of the healthcheck. `app` reads
// diffs and triggers syncs, so it verifies — and against the self-signed
// certificate stack.yml's TLS example deploys, that means it needs the
// certificate, which is the whole reason --ca-cert exists rather than
// --insecure.
func TestAppVerifiesTheControllersCertificate(t *testing.T) {
	server, caCert := startTLS(t, &stubReconciler{view: syncedView()})
	t.Setenv(client.EnvCACert, "")

	code, _, stderr := cli(t, server, "app", "list")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 without --ca-cert", code)
	}
	// The raw x509 sentence names no remedy, and this is the first thing an
	// operator meets after turning TLS on.
	if !strings.Contains(stderr, "--ca-cert") {
		t.Errorf("stderr = %q, want it to name the flag that fixes it", stderr)
	}

	code, stdout, stderr := cli(t, server, "app", "list", "--ca-cert", caCert)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 with --ca-cert (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "edge") {
		t.Errorf("stdout = %q, want the application", stdout)
	}
}

// D24. SWARMCLI_CD_SERVER is shared by all four commands, so pointing it at
// https://127.0.0.1:8080 to keep the healthcheck alive would otherwise break
// `docker exec … swarmcli-cd app list` inside the controller — the use case
// client.DefaultServer's own doc comment names. stack.yml sets both variables
// together, so this is the path that deployment actually takes.
func TestTheCACertMayComeFromTheEnvironment(t *testing.T) {
	server, caCert := startTLS(t, &stubReconciler{view: syncedView()})
	t.Setenv(client.EnvCACert, caCert)

	for _, command := range [][]string{{"app", "list"}, {"status"}} {
		t.Run(command[0], func(t *testing.T) {
			if code, _, stderr := cli(t, server, command...); code != 0 {
				t.Errorf("exit = %d, want 0 (stderr: %s)", code, stderr)
			}
		})
	}
}

func TestResolveCACert(t *testing.T) {
	env := func(string) string { return "/from/env.pem" }
	if got := resolveCACert("/from/flag.pem", env); got != "/from/flag.pem" {
		t.Errorf("resolveCACert = %q, want the flag to win", got)
	}
	if got := resolveCACert("", env); got != "/from/env.pem" {
		t.Errorf("resolveCACert = %q, want the environment", got)
	}
	// Unset rather than a default: the system pool is what verifies a
	// certificate from a real CA, and naming a file nobody installed would
	// break that.
	if got := resolveCACert("", func(string) string { return "" }); got != "" {
		t.Errorf("resolveCACert = %q, want it unset", got)
	}
}

// The restart loop, named. Go's server answers a plaintext request to a TLS
// listener with a 400 whose body says exactly what happened, and client.message
// drops it for the status line — so without tlsHint the operator of a task
// Swarm restarts every interval reads "400 bad request" and nothing else.
func TestHealthcheckSaysTLSWhenItProbedATLSListenerOverHTTP(t *testing.T) {
	server, _ := startTLS(t, &stubReconciler{view: syncedView()})
	plaintext := "http://" + strings.TrimPrefix(server, "https://")

	code, _, stderr := cli(t, plaintext, "healthcheck")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--tls-cert") || !strings.Contains(stderr, "https://") {
		t.Errorf("stderr = %q, want it to name TLS and the URL to probe instead", stderr)
	}
}

func TestHealthcheckFailsWhenNothingIsListening(t *testing.T) {
	// Port 1 on the loopback: nothing binds it, and the connection is refused
	// immediately rather than hanging.
	code, _, stderr := cli(t, "http://127.0.0.1:1", "healthcheck", "--timeout", "2s")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stderr == "" {
		t.Error("stderr is empty, want the reason")
	}
}

// driftedView is syncedView with the swarm moved underneath it: the plan still
// says unchanged, and the running service does not match.
func driftedView() application.View {
	v := syncedView()
	v.Status.Sync.State = application.SyncOutOfSync
	v.Status.Sync.Summary = application.SyncSummary{Unchanged: 1, Drifted: 1}
	v.Status.Drift = &application.Drift{
		State:    application.DriftStateDetected,
		Services: 2,
		Message:  `release 'web': 2 service(s) do not match the repository`,
	}
	v.Status.Releases[0].Sync = application.SyncOutOfSync
	v.Status.Releases[0].Drift = &application.ReleaseDrift{
		State: application.DriftStateDetected,
		Services: []application.ServiceDrift{
			{
				Name:   "web_nginx",
				Reason: application.DriftModified,
				Fields: []application.FieldDrift{{Field: "replicas", Desired: "2", Live: "7"}},
			},
			{Name: "web_sidecar", Reason: application.DriftMissing},
		},
	}
	return v
}

// The column exists only when something answers it, so the default mode every
// deployment runs keeps the table it had.
func TestAppListOmitsTheDriftColumnWhenNothingUsesIt(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, stdout, stderr := cli(t, server, "app", "list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if strings.Contains(stdout, "DRIFT") {
		t.Errorf("stdout = %q, want no drift column for a manifest-mode application", stdout)
	}
}

// SYNC, HEALTH and REVISION are all the last successful observation, and
// nothing moves them when a reconcile never reaches one. So an application whose
// repository has been unreachable for a week reported last week's plan as
// "synced / healthy" with an observedAt of three seconds ago, and Status.Error —
// the only field that said otherwise — appeared in `app get` and the JSON alone
// (#107).
func TestAppListMarksAnApplicationWhoseReconcileIsFailing(t *testing.T) {
	view := syncedView()
	view.Status.Error = "fetching source: dial tcp 10.0.0.9:22: connect: connection refused"
	server := start(t, &stubReconciler{view: view})

	code, stdout, stderr := cli(t, server, "app", "list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	// The column marks the row, and the reason goes under the table — a REASON
	// cell wide enough for a dial error would wreck every other row.
	for _, want := range []string{"RECONCILE", "failed", "edge: the last reconcile failed", "connection refused"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// And it stays off a fleet that is fine, for the reason the drift column does:
// a column of "ok" down a list of twenty says nothing, and would bury the one
// row that has something to report on the day there is one.
func TestAppListOmitsTheReconcileColumnWhenNothingIsFailing(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, stdout, stderr := cli(t, server, "app", "list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if strings.Contains(stdout, "RECONCILE") {
		t.Errorf("stdout = %q, want no reconcile column when every reconcile got through", stdout)
	}
}

func TestAppListShowsDrift(t *testing.T) {
	server := start(t, &stubReconciler{view: driftedView()})

	code, stdout, stderr := cli(t, server, "app", "list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"DRIFT", "detected", "out-of-sync"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// An operator seeing "out-of-sync" needs to know which field moved and to what,
// or the only way to find out is to go and read the swarm by hand — which is
// what this whole mode exists to save them.
func TestAppGetShowsWhatDrifted(t *testing.T) {
	server := start(t, &stubReconciler{view: driftedView()})

	code, stdout, stderr := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"Drift", "detected",
		"web drift:", "SERVICE", "REASON", "FIELD", "DESIRED", "LIVE",
		"web_nginx", "modified", "replicas", "7",
		// A service the manifest declares and the swarm does not have is its own
		// finding, with no field to name.
		"web_sidecar", "missing",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// A rolled-back service is the one finding a sync will not put right, so the
// table's REASON cell is not enough on its own: an operator reading "rolled-back"
// with no further word would reasonably wait for the next sync to fix it, and
// waiting is exactly wrong. The daemon's own account names the task that failed,
// which is where they go next.
func TestAppGetExplainsARollbackWillNotBeReapplied(t *testing.T) {
	view := driftedView()
	view.Status.Releases[0].Drift.Services = []application.ServiceDrift{{
		Name:    "web_nginx",
		Reason:  application.DriftRolledBack,
		Fields:  []application.FieldDrift{{Field: "image", Desired: "nginx:1.3", Live: "nginx:1.2"}},
		Message: "update paused due to failure or early termination of task 9xk2",
	}}
	server := start(t, &stubReconciler{view: view})

	code, stdout, stderr := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"rolled-back", "image", "nginx:1.3",
		"rolled back by Swarm", "will not re-apply",
		"early termination of task 9xk2",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestAppGetOmitsDriftWhenNotUsed(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, stdout, _ := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(stdout, "Drift") {
		t.Errorf("stdout = %q, want no drift line for a manifest-mode application", stdout)
	}
}

// unreadView is a release whose live state could not be read: no findings, and
// a message saying why. The rollup is computed rather than written out, because
// which release it ends up naming is the whole point of the test below.
func unreadView() application.View {
	v := syncedView()
	v.Status.Releases[0].Drift = &application.ReleaseDrift{
		State:   application.DriftStateUnknown,
		Message: "reading services: Cannot connect to the Docker daemon at unix:///var/run/docker.sock",
	}
	v.Status.Drift = drift.Application(v.Status.Releases)
	return v
}

// A release with no findings is not necessarily a release that converged. One
// that could not be read has nothing to list and everything to say, and the
// reconciler will not converge drift it could not read — so an operator who is
// shown nothing waits for a correction that is never coming (#199).
func TestAppGetShowsAReleaseWhoseLiveStateCouldNotBeRead(t *testing.T) {
	server := start(t, &stubReconciler{view: unreadView()})

	code, stdout, stderr := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"web drift:", "unknown", "Cannot connect to the Docker daemon"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	// The sentence goes under the heading, not into a cell: there are no
	// findings to tabulate, and a REASON column wide enough for a daemon error
	// would wreck every other release's table.
	if strings.Contains(stdout, "KIND") {
		t.Errorf("stdout = %q, want no findings table for a release with no findings", stdout)
	}
}

// The case the application line cannot cover. Detected outranks Unknown in the
// rollup deliberately — a difference that was found leads — so the one message
// that line carries names the drifted release, and the unread one is left with
// this section as its only surface. Both are reported, and the rollup is not
// touched: a header saying "unknown" when a service demonstrably does not match
// would be the same bug facing the other way.
func TestAppGetNamesAnUnreadReleaseBesideADriftedOne(t *testing.T) {
	v := driftedView()
	unread := v.Status.Releases[0]
	unread.Name = "cache"
	unread.Sync = application.SyncSynced
	unread.Services = nil
	unread.Drift = &application.ReleaseDrift{
		State:   application.DriftStateUnknown,
		Message: "reading services: context deadline exceeded",
	}
	v.Status.Releases = append(v.Status.Releases, unread)
	v.Status.Drift = drift.Application(v.Status.Releases)
	server := start(t, &stubReconciler{view: v})

	code, stdout, stderr := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"Drift        detected",
		"web drift:", "web_nginx", "modified",
		"cache drift:", "unknown", "context deadline exceeded",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// The two halves of one record, which reconcile.withOrphans produces: a release
// whose manifest would not resolve — a config it mounts, deleted by hand — loses
// its field comparison, and the sweep over the same release still finds services
// the repository no longer declares. The findings carry the state to Detected
// with the failed read's message still on it, and printing the table alone would
// serve a partial answer as a whole one.
func TestAppGetSaysWhatCouldNotBeComparedBesideWhatWasFound(t *testing.T) {
	v := driftedView()
	v.Status.Releases[0].Drift = &application.ReleaseDrift{
		State:    application.DriftStateDetected,
		Message:  "converting the manifest: config 'web_settings' not found",
		Services: []application.ServiceDrift{{Name: "web_sidecar", Reason: application.DriftUnexpected, Orphaned: true}},
	}
	v.Status.Drift = drift.Application(v.Status.Releases)
	server := start(t, &stubReconciler{view: v})

	code, stdout, _ := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"web drift:", "config 'web_settings' not found",
		"web_sidecar", "unexpected (orphaned)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// An unknown state with no message still says the question went unanswered,
// which is the whole of what the section is for. Nothing here produces one —
// every unknown this controller writes carries the error that caused it — but a
// newer controller's payload or a companion backend could, and the state alone
// is worth the line. The separator is what has nothing to separate.
func TestAppGetReportsAnUnknownDriftStateWithNoMessage(t *testing.T) {
	v := unreadView()
	v.Status.Releases[0].Drift.Message = ""
	server := start(t, &stubReconciler{view: v})

	code, stdout, _ := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	// Matched whole, because the thing being asserted is what is *not* there:
	// the state alone, with no dangling separator behind it.
	if !strings.Contains(stdout, "web drift:\n  unknown\n") {
		t.Errorf("stdout = %q, want the release named with its state and nothing after it", stdout)
	}
}

// The other release with no findings, and the one that genuinely has nothing to
// report: compared, and converged. It prints no section at all, which is what
// the rest of a converged application does.
func TestAppGetOmitsAReleaseThatWasComparedAndMatched(t *testing.T) {
	v := syncedView()
	v.Status.Releases[0].Drift = &application.ReleaseDrift{State: application.DriftStateNone}
	v.Status.Drift = drift.Application(v.Status.Releases)
	server := start(t, &stubReconciler{view: v})

	code, stdout, _ := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(stdout, " drift:") {
		t.Errorf("stdout = %q, want no drift section for a release that matches", stdout)
	}
}

// An operator has to be able to tell the two apart before enabling anything: an
// orphaned service is what a sync will delete, an unexpected one without the
// marker is something this controller will never touch.
func TestAppGetMarksAnOrphanedServiceAndLeavesOtherUnexpectedOnesPlain(t *testing.T) {
	v := driftedView()
	v.Status.Releases[0].Drift.Services = []application.ServiceDrift{
		{Name: "web_sidecar", Reason: application.DriftUnexpected, Orphaned: true},
		{Name: "web_stranger", Reason: application.DriftUnexpected},
	}
	server := start(t, &stubReconciler{view: v})

	code, stdout, _ := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "unexpected (orphaned)") {
		t.Errorf("stdout does not mark the orphan:\n%s", stdout)
	}
	if strings.Contains(stdout, "web_stranger  unexpected (orphaned)") {
		t.Errorf("stdout marks a service nobody proved was ours:\n%s", stdout)
	}
}

// An application whose release file declares no wave renders exactly as it
// always has. Every application that existed before waves is one of these, and a
// column of zeroes on all of them would be noise rather than information.
func TestAppGetOmitsTheWaveColumnForASingleWave(t *testing.T) {
	server := start(t, &stubReconciler{view: syncedView()})

	code, stdout, _ := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(stdout, "WAVE") {
		t.Errorf("stdout = %q, want no wave column when there is only one wave", stdout)
	}
}

// One that does declare waves shows which one each release is in — the order the
// releases are listed in is the order a sync applies them, so this is how a
// reader sees where a stalled sync stopped.
func TestAppGetShowsTheWaveWhenThereIsMoreThanOne(t *testing.T) {
	v := syncedView()
	v.Status.Releases = []application.ReleaseStatus{
		{Name: "db", Chart: "r/pg", Version: "1", Action: application.ActionUnchanged, Sync: application.SyncSynced},
		{Name: "api", Chart: "r/api", Version: "1", Action: application.ActionInstall, Sync: application.SyncOutOfSync, Wave: 2},
	}
	server := start(t, &stubReconciler{view: v})

	code, stdout, _ := cli(t, server, "app", "get", "edge")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "WAVE") {
		t.Fatalf("stdout = %q, want a wave column", stdout)
	}
	if !regexp.MustCompile(`api\s+r/api\s+1\s+0\s+install\s+out-of-sync\s+\S*\s*2`).MatchString(stdout) {
		t.Errorf("stdout = %q, want api reported in wave 2", stdout)
	}
}
