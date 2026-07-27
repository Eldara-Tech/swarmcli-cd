// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/api"
	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

func TestStatus(t *testing.T) {
	server := startStatus(t, healthyStatus())

	code, stdout, stderr := cli(t, server, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"Mode", "git",
		"Source", "https://example.com/apps.git @ main (apps/applications.yaml)",
		"Revision", "8b65cf9",
		"Applications", "4",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	// The commit is abbreviated the way git does it; the full one is in the
	// JSON, which is what anything acting on it should read.
	if strings.Contains(stdout, "8b65cf951c7e") {
		t.Errorf("stdout = %q, want the revision abbreviated", stdout)
	}
	// The three lines that only exist when something is wrong must not appear
	// on a healthy controller, or an operator learns to skip the block.
	for _, unwanted := range []string{"Stale", "Error", "Orphaned", "Pruned"} {
		if strings.Contains(stdout, unwanted) {
			t.Errorf("stdout = %q, want no %s line on a healthy controller", stdout, unwanted)
		}
	}
}

// Stale is the thing this command exists to say: the running set is the last one
// that validated and a newer one is being refused.
func TestStatusReportsAStaleAppSet(t *testing.T) {
	s := healthyStatus()
	s.AppSet.Stale = true
	s.AppSet.Error = `apps/applications.yaml@d4e5f6: applications[1]: duplicate application name "edge"`
	s.AppSet.Orphaned = []string{"legacy-api", "old-edge"}
	server := startStatus(t, s)

	code, stdout, stderr := cli(t, server, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"Stale", "the running set is the last one that validated",
		"Error", "duplicate application name",
		"Orphaned", "legacy-api, old-edge",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// Prune leaves a departed application with no other trace: it is out of the app
// set, out of Orphaned, and off the swarm. This line is the only thing that
// distinguishes "deleted" from "the controller never noticed".
func TestStatusReportsWhatWasPruned(t *testing.T) {
	s := healthyStatus()
	s.AppSet.Pruned = []string{"legacy-api", "old-edge"}
	server := startStatus(t, s)

	code, stdout, stderr := cli(t, server, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Pruned") || !strings.Contains(stdout, "legacy-api, old-edge") {
		t.Errorf("stdout = %q, want it to report what was pruned", stdout)
	}
}

// A stale app set is a state, not a failure of the command: the controller
// answered. `healthcheck` is the one command whose exit code is a verdict.
func TestStatusExitsZeroWhenTheAppSetIsStale(t *testing.T) {
	s := healthyStatus()
	s.AppSet.Stale = true
	s.AppSet.Error = "the app set could not be loaded"

	if code, _, stderr := cli(t, startStatus(t, s), "status"); code != 0 {
		t.Errorf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
}

// A controller that has never loaded a set carries a zero time and no revision.
// Printing year 1 would read as a clock fault rather than "not yet".
func TestStatusRendersAControllerThatHasNeverLoaded(t *testing.T) {
	server := startStatus(t, application.ControllerStatus{
		AppSet: application.AppSetStatus{Mode: "path", Source: "/var/lib/appset (applications.yaml)"},
	})

	code, stdout, stderr := cli(t, server, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "never") {
		t.Errorf("stdout = %q, want the load time to read never", stdout)
	}
	if strings.Contains(stdout, "Revision") {
		t.Errorf("stdout = %q, want no revision line when there is none", stdout)
	}
}

// An unwired status provider is what the API answers with when no app-set
// source is configured, and "unknown" is a truer word for it than a blank.
func TestStatusRendersAnUnknownMode(t *testing.T) {
	server := startStatus(t, application.ControllerStatus{})

	code, stdout, stderr := cli(t, server, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "unknown") {
		t.Errorf("stdout = %q, want the mode to read unknown", stdout)
	}
	if strings.Contains(stdout, "Source") {
		t.Errorf("stdout = %q, want no source line when there is none", stdout)
	}
}

// The JSON is the controller's own response, not this binary's re-encoding of
// it: that is what makes it safe to script against as the types grow.
func TestStatusJSONIsTheControllersOwnResponse(t *testing.T) {
	server := startStatus(t, healthyStatus())

	code, stdout, stderr := cli(t, server, "status", "-o", "json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	var got application.ControllerStatus
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout)
	}
	if got.AppSet.Revision != "8b65cf951c7e" {
		t.Errorf("revision = %q, want the full commit", got.AppSet.Revision)
	}
	if got.AppSet.Source != "https://example.com/apps.git @ main (apps/applications.yaml)" {
		t.Errorf("source = %q, want the bootstrap's own description", got.AppSet.Source)
	}
	if got.Applications != 4 {
		t.Errorf("applications = %d, want 4", got.Applications)
	}
}

func TestStatusUsageErrors(t *testing.T) {
	server := startStatus(t, healthyStatus())

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"an unknown flag", []string{"status", "--moed", "json"}, "moed"},
		{"an unexpected argument", []string{"status", "edge"}, "edge"},
		{"an invalid output format", []string{"status", "-o", "yaml"}, "yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := cli(t, server, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to name %q", stderr, tc.want)
			}
			if stdout != "" {
				t.Errorf("wrote %q to stdout, want the error on stderr only", stdout)
			}
		})
	}
}

func TestStatusHelp(t *testing.T) {
	server := startStatus(t, healthyStatus())

	for _, arg := range []string{"--help", "-h"} {
		code, stdout, _ := cli(t, server, "status", arg)
		if code != 0 {
			t.Errorf("status %s = %d, want 0", arg, code)
		}
		if !strings.Contains(stdout, "swarmcli-cd status [options]") {
			t.Errorf("stdout = %q, want the status usage", stdout)
		}
	}
}

// A rejection has to point at the variable to fix, exactly as the app commands
// do — the shared explain path, reached from a second command.
func TestStatusRejectedTokenNamesItsSource(t *testing.T) {
	server := startStatus(t, healthyStatus())
	t.Setenv(authz.EnvTokenFile, "")
	t.Setenv(authz.EnvToken, "the-wrong-token")

	code, _, stderr := cli(t, server, "status")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, authz.EnvToken) {
		t.Errorf("stderr = %q, want it to name %s", stderr, authz.EnvToken)
	}
}

// A command missing from the top-level help is a command nobody finds.
func TestUsageListsStatus(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status") {
		t.Errorf("stdout = %q, want it to list the status command", stdout.String())
	}
}

// startStatus runs the real API server reporting status and returns its URL.
// The server is the real one so the guard, the envelope and the status code are
// all exercised alongside the rendering.
func startStatus(t *testing.T, status application.ControllerStatus) string {
	t.Helper()
	t.Setenv(authz.EnvTokenFile, "")
	t.Setenv(authz.EnvToken, "s3cret")
	swapAuthorizer(t, bearerAuthorizer{token: "s3cret"})

	handler := api.New(&stubReconciler{view: syncedView()}, api.Options{
		Controller: stubController{status: status},
	}).Handler()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func healthyStatus() application.ControllerStatus {
	return application.ControllerStatus{
		AppSet: application.AppSetStatus{
			Mode:     "git",
			Source:   "https://example.com/apps.git @ main (apps/applications.yaml)",
			Revision: "8b65cf951c7e",
			LoadedAt: time.Now(),
		},
		Applications: 4,
	}
}

type stubController struct{ status application.ControllerStatus }

func (s stubController) Status() application.ControllerStatus { return s.status }
