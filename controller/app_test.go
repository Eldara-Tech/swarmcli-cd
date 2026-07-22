// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/api"
	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/client"
	"github.com/Eldara-Tech/swarmcli-cd/reconcile"
)

// start runs the real API server over rec and returns the arguments that point
// the CLI at it. Testing against the actual server rather than a hand-written
// stub is what makes these tests cover the guard, the envelopes and the status
// codes as well as the rendering.
func start(t *testing.T, rec *stubReconciler) string {
	t.Helper()
	t.Setenv(authz.EnvTokenFile, "")
	t.Setenv(authz.EnvToken, "s3cret")
	// Not authz.Get(): the default authorizer read its token from the process
	// environment in init(), long before t.Setenv. This one expects the same
	// bearer credential the client will present, so the server's guard is still
	// the real one being exercised.
	swapAuthorizer(t, bearerAuthorizer{token: "s3cret"})

	srv := httptest.NewServer(api.New(rec, api.Options{}).Handler())
	t.Cleanup(srv.Close)
	return srv.URL
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
	server := start(t, &stubReconciler{view: syncedView(), diffErr: reconcile.ErrNotPlanned})

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
