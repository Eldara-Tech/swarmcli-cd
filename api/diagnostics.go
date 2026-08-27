// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"fmt"
	"net/http"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

// diagnostics evaluates the overall health, sync convergence, and drift status
// across all reconciled applications and Swarm nodes.
func (s *Server) diagnostics(w http.ResponseWriter, _ *http.Request, _ authz.Subject) {
	views := s.rec.Views()
	var ctrlStatus application.ControllerStatus
	if s.controller != nil {
		ctrlStatus = s.controller.Status()
	}

	risks := make([]application.RiskItem, 0)
	checks := make([]application.CheckItem, 0)

	totalApps := len(views)
	healthyApps := 0
	syncedApps := 0
	driftFreeApps := 0

	for _, v := range views {
		isHealthy := v.Status.Health.State == application.HealthHealthy
		isSynced := v.Status.Sync.State == application.SyncSynced
		isDriftFree := v.Status.Drift == nil || v.Status.Drift.State == application.DriftStateNone || v.Status.Drift.State == ""

		if isHealthy {
			healthyApps++
		} else {
			state := v.Status.Health.State
			if state == "" {
				state = "unknown"
			}
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("health-%s", v.Spec.Name),
				Severity:    "bad",
				Application: v.Spec.Name,
				Title:       fmt.Sprintf("Application '%s' is degraded", v.Spec.Name),
				Summary:     fmt.Sprintf("Service health state is '%s'. One or more tasks failed to converge.", state),
				Remedy:      "Check service container logs or trigger a manual sync.",
			})
		}

		if isSynced {
			syncedApps++
		} else {
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("sync-%s", v.Spec.Name),
				Severity:    "warn",
				Application: v.Spec.Name,
				Title:       fmt.Sprintf("Application '%s' is out of sync", v.Spec.Name),
				Summary:     fmt.Sprintf("Desired git revision '%s' differs from running release.", v.Status.Sync.Revision),
				Remedy:      "Trigger a reconcile sync to apply target manifests.",
			})
		}

		if isDriftFree {
			driftFreeApps++
		} else {
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("drift-%s", v.Spec.Name),
				Severity:    "warn",
				Application: v.Spec.Name,
				Title:       fmt.Sprintf("Live drift detected on '%s'", v.Spec.Name),
				Summary:     "Swarm runtime configuration deviates from git-declared state.",
				Remedy:      "Review planned diff and apply reconciliation.",
			})
		}
	}

	if ctrlStatus.AppSet.Stale {
		risks = append(risks, application.RiskItem{
			ID:       "appset-stale",
			Severity: "bad",
			Title:    "Application Set manifest is stale",
			Summary:  fmt.Sprintf("Controller failed to load recent app-set: %s", ctrlStatus.AppSet.Error),
			Remedy:   "Fix application set YAML syntax or repository permissions.",
		})
	}

	// Build Diagnostic Checks
	checks = append(checks, application.CheckItem{
		ID:     "check-gitops-sync",
		Name:   "GitOps Manifest Convergence",
		Passed: totalApps == 0 || syncedApps == totalApps,
		Detail: fmt.Sprintf("%d of %d applications in sync with repository", syncedApps, totalApps),
	})

	checks = append(checks, application.CheckItem{
		ID:     "check-service-health",
		Name:   "Swarm Task & Service Health",
		Passed: totalApps == 0 || healthyApps == totalApps,
		Detail: fmt.Sprintf("%d of %d applications healthy and running", healthyApps, totalApps),
	})

	checks = append(checks, application.CheckItem{
		ID:     "check-drift-invariance",
		Name:   "Runtime Configuration Invariance",
		Passed: totalApps == 0 || driftFreeApps == totalApps,
		Detail: fmt.Sprintf("%d of %d applications free of unmanaged drift", driftFreeApps, totalApps),
	})

	checks = append(checks, application.CheckItem{
		ID:     "check-controller-integrity",
		Name:   "Controller & App-Set State",
		Passed: !ctrlStatus.AppSet.Stale,
		Detail: fmt.Sprintf("App-set mode '%s' active and operational", ctrlStatus.AppSet.Mode),
	})

	// Score calculation: 100 - penalties
	score := 100
	for _, r := range risks {
		switch r.Severity {
		case "bad":
			score -= 25
		case "warn":
			score -= 10
		case "info":
			score -= 5
		}
	}
	if score < 0 {
		score = 0
	}

	tone := "ok"
	if score < 60 {
		tone = "bad"
	} else if score < 90 {
		tone = "warn"
	}

	clearCount := 0
	for _, c := range checks {
		if c.Passed {
			clearCount++
		}
	}

	resp := application.DiagnosticsResponse{
		Score:      score,
		Tone:       tone,
		ClearCount: clearCount,
		TotalCount: len(checks),
		Risks:      risks,
		Checks:     checks,
	}

	write(w, http.StatusOK, resp)
}
