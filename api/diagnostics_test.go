// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// The diagnostics endpoint, and the readings a console must not get wrong.
//
// Most of what is below started life as component tests on the Diagnostics
// screen (#257). The screen then moved its arithmetic to this handler, which
// is the right place for it — but the move brought two of the defects with it,
// and left the regressions behind in a file that no longer computes anything.
// They belong here now, next to the code that decides.

// app builds a view with the three axes set explicitly, so a test says what it
// is testing rather than inheriting a fixture's defaults.
func app(name string, sync application.SyncState, health application.HealthState) application.View {
	v := view(name)
	v.Status.Sync.State = sync
	v.Status.Health.State = health
	v.Status.Drift = nil
	v.Status.Error = ""
	return v
}

func diagnose(t *testing.T, views []application.View, ctrl *application.ControllerStatus) application.DiagnosticsResponse {
	t.Helper()
	s, h := testServer(t, &fakeReconciler{views: views}, nil)
	if ctrl != nil {
		s.controller = &fakeController{status: *ctrl}
	}
	rr := do(t, h, "GET", "/api/v1/diagnostics")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/diagnostics = %d, want 200", rr.Code)
	}
	return decode[application.DiagnosticsResponse](t, rr)
}

func risk(resp application.DiagnosticsResponse, id string) (application.RiskItem, bool) {
	for _, r := range resp.Risks {
		if r.ID == id {
			return r, true
		}
	}
	return application.RiskItem{}, false
}

func check(t *testing.T, resp application.DiagnosticsResponse, id string) application.CheckItem {
	t.Helper()
	for _, c := range resp.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in %v", id, resp.Checks)
	return application.CheckItem{}
}

// A healthy set, so a test that is about applications is not also about the
// app set — an unwired controller would otherwise raise nothing and pass.
func loadedSet() *application.ControllerStatus {
	return &application.ControllerStatus{
		AppSet:       application.AppSetStatus{Mode: "git", LoadedAt: time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)},
		Applications: 1,
	}
}

// The tone drove the console's chip, its ring and its wording, and it was a
// threshold on the score: `tone = "ok"` at 90 or above. One warn risk costs
// exactly 10, so a single out-of-sync application rendered the word "Nominal"
// beside a risk list showing one.
func TestOneOutOfSyncApplicationIsNotNominal(t *testing.T) {
	resp := diagnose(t, []application.View{app("edge", application.SyncOutOfSync, application.HealthHealthy)}, loadedSet())

	if resp.Score != 90 {
		t.Errorf("score = %d, want 90 — the arithmetic that made this look nominal", resp.Score)
	}
	if resp.Tone == "ok" {
		t.Errorf("tone = %q with %d risk(s); a console showing Nominal beside a risk list is the defect", resp.Tone, len(resp.Risks))
	}
	if len(resp.Risks) != 1 {
		t.Fatalf("risks = %v, want exactly the sync one", resp.Risks)
	}
}

// The score and the tone must not be able to disagree at any tuning of the
// penalties, which is the whole reason the tone is taken from the risks.
func TestToneNeverReadsOkWhileARiskStands(t *testing.T) {
	for _, tc := range []struct {
		name string
		view application.View
	}{
		{"out of sync", app("a", application.SyncOutOfSync, application.HealthHealthy)},
		{"degraded", app("a", application.SyncSynced, application.HealthDegraded)},
		{"missing", app("a", application.SyncSynced, application.HealthMissing)},
		{"progressing", app("a", application.SyncSynced, application.HealthProgressing)},
		{"unreported", app("a", application.SyncUnknown, application.HealthUnknown)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := diagnose(t, []application.View{tc.view}, loadedSet())
			if len(resp.Risks) == 0 {
				t.Fatalf("no risk raised for %s", tc.name)
			}
			if resp.Tone == "ok" {
				t.Errorf("tone = ok with risks %v", resp.Risks)
			}
		})
	}
}

// Every enum's Unknown member is the empty string, so this is what an
// application the controller has accepted and not yet reconciled reports.
// Counted as clear it scored a controller that had reconciled nothing at 100.
func TestAnUnreportedApplicationIsNeitherClearNorDegraded(t *testing.T) {
	resp := diagnose(t, []application.View{app("edge", application.SyncUnknown, application.HealthUnknown)}, loadedSet())

	if resp.Score == 100 {
		t.Errorf("score = 100 for a fleet nobody has reported on")
	}
	if resp.Tone == "ok" {
		t.Errorf("tone = ok for a fleet nobody has reported on")
	}

	r, ok := risk(resp, "health-edge")
	if !ok {
		t.Fatalf("no health risk raised; risks = %v", resp.Risks)
	}
	if r.Severity != severityUnknown {
		t.Errorf("severity = %q, want %q — calling this bad says a fleet 20s into its first start is degraded",
			r.Severity, severityUnknown)
	}
	if r.Title == "Application 'edge' is degraded" {
		t.Errorf("title = %q, which is a claim the controller never made", r.Title)
	}
}

// A rollout in flight is not a fault. Reported as one — which "anything not
// healthy is degraded" did — every ordinary deploy raised a critical risk.
func TestARolloutInFlightIsAWarningAndNotADegradation(t *testing.T) {
	resp := diagnose(t, []application.View{app("edge", application.SyncSynced, application.HealthProgressing)}, loadedSet())

	r, ok := risk(resp, "health-edge")
	if !ok {
		t.Fatalf("no health risk raised; risks = %v", resp.Risks)
	}
	if r.Severity != severityWarn {
		t.Errorf("severity = %q, want %q for a rollout in progress", r.Severity, severityWarn)
	}
}

// A failed reconcile outranks every axis below it, because each of those is
// then the last successful observation rather than the current one.
func TestAFailedReconcileIsItsOwnCriticalRisk(t *testing.T) {
	v := app("edge", application.SyncSynced, application.HealthHealthy)
	v.Status.Error = "clone failed"
	resp := diagnose(t, []application.View{v}, loadedSet())

	r, ok := risk(resp, "reconcile-edge")
	if !ok {
		t.Fatalf("a failed reconcile raised no risk at all; risks = %v", resp.Risks)
	}
	if r.Severity != severityBad {
		t.Errorf("severity = %q, want %q", r.Severity, severityBad)
	}
	if resp.Tone != "bad" {
		t.Errorf("tone = %q, want bad", resp.Tone)
	}
}

// `!Stale` was the whole of the controller-integrity check, so a controller
// that had never loaded a set — and one with no reconcile loop wired at all —
// both reported as operational, the second with an empty mode printed into the
// sentence claiming it.
func TestTheControllerIntegrityCheckReadsEveryAppSetShape(t *testing.T) {
	loaded := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		status   application.ControllerStatus
		passed   bool
		riskID   string
		notInDet string
	}{
		{
			name:   "a healthy set",
			status: application.ControllerStatus{AppSet: application.AppSetStatus{Mode: "git", LoadedAt: loaded}, Applications: 1},
			passed: true,
		},
		{
			// Not a failure: api.go answers with a zero status when it has no
			// controller. Colouring it red teaches an operator to ignore red.
			name:     "no reconcile loop wired",
			status:   application.ControllerStatus{},
			passed:   true,
			notInDet: "active and operational",
		},
		{
			name:   "a set that has never loaded",
			status: application.ControllerStatus{AppSet: application.AppSetStatus{Mode: "git", Error: "connection refused"}},
			passed: false,
			riskID: "appset-never-loaded",
		},
		{
			name:   "a stale set",
			status: application.ControllerStatus{AppSet: application.AppSetStatus{Mode: "git", LoadedAt: loaded, Stale: true, Error: "duplicate name"}, Applications: 1},
			passed: false,
			riskID: "appset-stale",
		},
		{
			name:   "a load that did not land in full",
			status: application.ControllerStatus{AppSet: application.AppSetStatus{Mode: "git", LoadedAt: loaded, Error: "edge: chart render failed"}, Applications: 1},
			passed: false,
			riskID: "appset-partial",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			resp := diagnose(t, []application.View{app("edge", application.SyncSynced, application.HealthHealthy)}, &status)

			c := check(t, resp, "check-controller-integrity")
			if c.Passed != tc.passed {
				t.Errorf("passed = %v, want %v (detail %q)", c.Passed, tc.passed, c.Detail)
			}
			if tc.notInDet != "" && strings.Contains(c.Detail, tc.notInDet) {
				t.Errorf("detail = %q, which claims something this shape cannot support", c.Detail)
			}
			if tc.riskID != "" {
				if _, ok := risk(resp, tc.riskID); !ok {
					t.Errorf("no %s risk; risks = %v", tc.riskID, resp.Risks)
				}
				if resp.Tone == "ok" {
					t.Errorf("tone = ok while the app set is %s", tc.name)
				}
			}
		})
	}
}

// An empty fleet passes the three per-application checks vacuously, and that is
// allowed *because each one carries the count that makes it vacuous*. A bare
// "all stacks match their manifests" over nothing is a claim; "0 of 0" is not.
func TestChecksOverAnEmptyFleetCarryTheCountThatQualifiesThem(t *testing.T) {
	resp := diagnose(t, nil, loadedSet())

	for _, id := range []string{"check-gitops-sync", "check-service-health", "check-drift-invariance"} {
		c := check(t, resp, id)
		if !c.Passed {
			t.Errorf("%s failed over an empty fleet", id)
		}
		if !strings.Contains(c.Detail, "0 of 0") {
			t.Errorf("%s detail = %q, want the 0 of 0 that says it is vacuous", id, c.Detail)
		}
	}
	if resp.Tone != "ok" {
		t.Errorf("tone = %q, want ok: an empty fleet with a loaded set has nothing wrong with it", resp.Tone)
	}
}

// Drift is three-valued and the third value is not "no drift": a nil Drift is
// manifest mode never having asked, which is not a finding, while a state the
// controller reported and this build cannot read is.
func TestAnUnreadableDriftStateIsNotACleanBillOfHealth(t *testing.T) {
	notAsked := app("a", application.SyncSynced, application.HealthHealthy)
	if resp := diagnose(t, []application.View{notAsked}, loadedSet()); resp.Tone != "ok" {
		t.Errorf("tone = %q for manifest mode, which never asks about drift", resp.Tone)
	}

	unreadable := app("a", application.SyncSynced, application.HealthHealthy)
	unreadable.Status.Drift = &application.Drift{State: application.DriftState("something-newer")}
	resp := diagnose(t, []application.View{unreadable}, loadedSet())
	r, ok := risk(resp, "drift-a")
	if !ok {
		t.Fatalf("an unreadable drift state raised nothing; risks = %v", resp.Risks)
	}
	if r.Severity != severityUnknown {
		t.Errorf("severity = %q, want %q", r.Severity, severityUnknown)
	}
}
