// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"fmt"
	"net/http"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

// The three severities a risk carries, worst first.
//
// `severityUnknown` is the one worth explaining, and it is the same argument
// api/severity.ts makes on the other side of the wire: application/enum.go
// makes the empty string the Unknown member of every enum, so an application
// the controller has accepted and has not yet reconciled reports `unknown` on
// both axes — and so does one whose state a newer controller describes in a
// word this build has never heard. Neither is a fault and neither is an
// all-clear, so it is its own tier. Folded into `bad` it paints a fleet twenty
// seconds into its first start as degraded; folded into clear it reports a
// fleet nobody has looked at as fine.
const (
	severityBad     = "bad"
	severityWarn    = "warn"
	severityUnknown = "unknown"
)

// What each severity costs the score. The score is a summary for a human to
// glance at; the tone below is what the UI colours, and it does not read this.
var penalties = map[string]int{
	severityBad:     25,
	severityWarn:    10,
	severityUnknown: 5,
}

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
		name := v.Spec.Name

		// A failed reconcile outranks every axis below it, because each of those
		// is then the last successful observation rather than the current one —
		// the applications list marks the same row stale for the same reason.
		if v.Status.Error != "" {
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("reconcile-%s", name),
				Severity:    severityBad,
				Application: name,
				Title:       fmt.Sprintf("Application '%s' failed to reconcile", name),
				Summary:     fmt.Sprintf("The last reconcile did not complete: %s", v.Status.Error),
				Remedy:      "Check the controller logs, then trigger a manual sync once the cause is fixed.",
			})
		}

		switch v.Status.Health.State {
		case application.HealthHealthy:
			healthyApps++
		case application.HealthDegraded, application.HealthMissing:
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("health-%s", name),
				Severity:    severityBad,
				Application: name,
				Title:       fmt.Sprintf("Application '%s' is %s", name, v.Status.Health.State),
				Summary:     fmt.Sprintf("Service health state is '%s'. One or more tasks failed to converge.", v.Status.Health.State),
				Remedy:      "Check service container logs or trigger a manual sync.",
			})
		case application.HealthProgressing:
			// A rollout in flight is not a fault. Reported as one — which is
			// what "anything that is not healthy is degraded" did — every
			// ordinary deploy would raise a critical risk while it ran.
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("health-%s", name),
				Severity:    severityWarn,
				Application: name,
				Title:       fmt.Sprintf("Application '%s' is still rolling out", name),
				Summary:     "A rollout is in progress and has not finished converging yet.",
				Remedy:      "Wait for the rollout to settle, then re-check.",
			})
		default:
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("health-%s", name),
				Severity:    severityUnknown,
				Application: name,
				Title:       fmt.Sprintf("Application '%s' has not been reported on", name),
				Summary:     "The controller has not reported a health state for this application yet.",
				Remedy:      "None needed if the controller has just started; otherwise check that its loop is running.",
			})
		}

		switch v.Status.Sync.State {
		case application.SyncSynced:
			syncedApps++
		case application.SyncOutOfSync:
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("sync-%s", name),
				Severity:    severityWarn,
				Application: name,
				Title:       fmt.Sprintf("Application '%s' is out of sync", name),
				Summary:     fmt.Sprintf("Desired git revision '%s' differs from running release.", v.Status.Sync.Revision),
				Remedy:      "Trigger a reconcile sync to apply target manifests.",
			})
		default:
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("sync-%s", name),
				Severity:    severityUnknown,
				Application: name,
				Title:       fmt.Sprintf("Sync state for '%s' is unknown", name),
				Summary:     "The controller has not reported whether this application matches the repository.",
				Remedy:      "None needed if the controller has just started; otherwise check that its loop is running.",
			})
		}

		// A nil Drift is manifest mode never having asked, which is not the same
		// as having asked and found nothing — so it is not a finding, and it is
		// not a clean bill of health either. An *unknown* state is one the
		// controller reported and this build cannot read, which is.
		switch {
		case v.Status.Drift == nil, v.Status.Drift.State == application.DriftStateNone:
			driftFreeApps++
		case v.Status.Drift.State == application.DriftStateDetected:
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("drift-%s", name),
				Severity:    severityWarn,
				Application: name,
				Title:       fmt.Sprintf("Live drift detected on '%s'", name),
				Summary:     "Swarm runtime configuration deviates from git-declared state.",
				Remedy:      "Review planned diff and apply reconciliation.",
			})
		default:
			risks = append(risks, application.RiskItem{
				ID:          fmt.Sprintf("drift-%s", name),
				Severity:    severityUnknown,
				Application: name,
				Title:       fmt.Sprintf("Drift state for '%s' is unknown", name),
				Summary:     "The controller reported a drift state this build does not recognise.",
				Remedy:      "Check whether the controller is newer than this UI.",
			})
		}
	}

	// The app set is the other half of the answer, and one boolean was reading
	// it: `!Stale` passed for a controller that had never loaded a set at all.
	shape := ctrlStatus.Shape()
	if risk, ok := appSetRisk(shape, ctrlStatus); ok {
		risks = append(risks, risk)
	}

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
		Passed: shape == application.AppSetOK || shape == application.AppSetUnwired,
		Detail: appSetDetail(shape, ctrlStatus),
	})

	score := 100
	for _, r := range risks {
		score -= penalties[r.Severity]
	}
	if score < 0 {
		score = 0
	}

	clearCount := 0
	for _, c := range checks {
		if c.Passed {
			clearCount++
		}
	}

	resp := application.DiagnosticsResponse{
		Score:      score,
		Tone:       toneOf(risks),
		ClearCount: clearCount,
		TotalCount: len(checks),
		Risks:      risks,
		Checks:     checks,
	}

	write(w, http.StatusOK, resp)
}

// toneOf is the worst severity present, and it is deliberately not a threshold
// on the score.
//
// A threshold was: `tone = "ok"` whenever the arithmetic landed on 90 or above,
// and one warn risk costs exactly 10 — so a single out-of-sync application
// rendered the word "Nominal" beside a risk list showing one. Taking the tone
// from the risks themselves means the chip, the ring and the list cannot
// disagree, whatever the penalties are tuned to.
func toneOf(risks []application.RiskItem) string {
	tone := "ok"
	for _, r := range risks {
		switch r.Severity {
		case severityBad:
			return "bad"
		case severityWarn, severityUnknown:
			// Not "ok": a fleet nobody has reported on is not a nominal fleet.
			tone = "warn"
		}
	}
	return tone
}

// appSetRisk is the finding an unhealthy app set raises, if any.
func appSetRisk(shape application.AppSetShape, st application.ControllerStatus) (application.RiskItem, bool) {
	switch shape {
	case application.AppSetStale:
		return application.RiskItem{
			ID:       "appset-stale",
			Severity: severityBad,
			Title:    "Application set manifest is stale",
			Summary:  fmt.Sprintf("A newer application set is being refused; what is running is the last one that validated: %s", st.AppSet.Error),
			Remedy:   "Fix the application-set YAML or the repository permissions.",
		}, true
	case application.AppSetNeverLoaded:
		return application.RiskItem{
			ID:       "appset-never-loaded",
			Severity: severityBad,
			Title:    "No application set has ever loaded",
			Summary:  fmt.Sprintf("Nothing is being reconciled, and the applications list is empty for a reason unrelated to any application in it: %s", st.AppSet.Error),
			Remedy:   "Check that the controller can reach its source, then restart it.",
		}, true
	case application.AppSetPartial:
		return application.RiskItem{
			ID:       "appset-partial",
			Severity: severityWarn,
			Title:    "The last application-set load reported an error",
			Summary:  fmt.Sprintf("The set loaded and is being reconciled, but part of the change did not land: %s", st.AppSet.Error),
			Remedy:   "Fix the named application, then let the next load pick it up.",
		}, true
	default:
		// AppSetOK, and AppSetUnwired — which is a deployment choice rather than
		// a fault, and colouring it as one teaches an operator to ignore red.
		return application.RiskItem{}, false
	}
}

// appSetDetail is the sentence the controller-integrity check shows.
//
// It said "App-set mode ” active and operational" for a controller with no
// reconcile loop, which is the empty Mode printed into a claim that contradicts
// it.
func appSetDetail(shape application.AppSetShape, st application.ControllerStatus) string {
	switch shape {
	case application.AppSetUnwired:
		return "No application-set source is wired; there is nothing to load."
	case application.AppSetNeverLoaded:
		return "No application set has ever loaded."
	case application.AppSetStale:
		return fmt.Sprintf("Mode '%s' is serving a last-good set; a newer one is being refused.", st.AppSet.Mode)
	case application.AppSetPartial:
		return fmt.Sprintf("Mode '%s' loaded, but part of the last change did not land.", st.AppSet.Mode)
	default:
		return fmt.Sprintf("App-set mode '%s' active and operational", st.AppSet.Mode)
	}
}
