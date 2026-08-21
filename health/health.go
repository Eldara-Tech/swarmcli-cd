// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package health answers "is what is running actually working".
//
// It is a separate axis from sync, which answers "does the swarm match git". A
// stack can be synced and degraded at once, and collapsing the two loses the
// distinction that makes the view useful — #1 lists per-stack *and* per-service
// health as one of the things nothing in the Swarm GitOps field does.
//
// Every judgement about a service comes from the chart engine's own exported
// predicate (charts.ServiceState.Convergence, Eldara-Tech/swarmcli#500). The
// rules it encodes were each corrected at least once — the running count by
// actual rather than desired state, the target over active nodes, a completed
// one-shot job, the stability window measured from task creation, paused as
// wedged rather than slow — and a second copy would diverge silently in both
// directions: reporting healthy while a `charts` deploy would still be waiting,
// or degraded on a stack that is fine.
//
// What this package adds is the part the engine has no opinion on: when a
// rollout that is merely slow should be called broken, and what a release that
// is not there at all should read as.
package health

import (
	"fmt"
	"slices"

	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// Input is what one release's health is decided from.
type Input struct {
	// States are the release's live services, from Backend.StackServices.
	States []charts.ServiceState
	// Installed is false for a release the plan would install: declared in the
	// repository, never deployed. That is Missing rather than Degraded — a UI
	// has to tell "not there" from "there and broken", and it is also the
	// ordinary state of a newly declared release before its first sync.
	Installed bool
	// SyncFailed reports that the last sync attempted for this application did
	// not succeed. It is what turns a rollout that is merely slow into one that
	// is broken; see Release.
	SyncFailed bool
	// ReadFailed reports that the swarm could not be asked, so States is empty
	// because nothing answered rather than because nothing is there.
	//
	// It exists because those two are the same value — a nil slice — coming out
	// of charts.Backend.StackServices, which has no error return, and opposite
	// findings coming out of Release: one is Missing, the loudest state here,
	// and the other is Unknown. Without the distinction a daemon that was
	// briefly unreachable flipped every release of an application to Missing and
	// paged whoever was listening, for a stack running perfectly (#107).
	ReadFailed bool
	// AsDeclared names the services a live comparison found running exactly what
	// the repository declares.
	//
	// It exists for one judgement, below: whether a rollback a service is still
	// carrying is about anything outstanding. Swarm keeps UpdateStatus on a
	// service until its *next* update, so a rollback that restored the spec git
	// asks for stays there indefinitely — and no next update is coming, because
	// there is no drift, so nothing redeploys it, so nothing clears it. The
	// release read Degraded for ever over an event that had already put the
	// service back where it belongs (#130).
	//
	// Empty is the ordinary value and asserts nothing. Drift detection is a
	// per-application setting, so most callers made no comparison at all, and a
	// status with no evidence against it is taken at face value.
	AsDeclared []string
}

// Release rolls one release's services up to a health state, and returns the
// per-service detail alongside it.
func Release(in Input) (application.Health, []application.ServiceStatus) {
	switch {
	case !in.Installed:
		return application.Health{
			State:   application.HealthMissing,
			Message: "declared in the repository but not deployed",
		}, nil
	case in.ReadFailed:
		// Asked and unanswerable, which is what Unknown is for and the one thing
		// no other state may be used for: every one of them asserts something
		// about the swarm, and nothing here has read it. Checked before the
		// empty case below, which is the assertion this would otherwise become.
		return application.Health{
			State:   application.HealthUnknown,
			Message: "the swarm could not be read, so this release's health is not known",
		}, nil
	case len(in.States) == 0:
		// The release record exists but the swarm has no services under it.
		// Missing rather than Degraded for the same reason: there is nothing
		// running to be unhealthy, and "it is gone" is a different problem for
		// an operator than "it is failing".
		return application.Health{
			State:   application.HealthMissing,
			Message: "deployed, but no services are present on the swarm",
		}, nil
	}

	services := make([]application.ServiceStatus, 0, len(in.States))
	worst := application.Health{State: application.HealthHealthy}
	healthy := 0

	for _, s := range in.States {
		state, message := serviceHealth(s, in)
		if state == application.HealthHealthy {
			healthy++
		}
		services = append(services, application.ServiceStatus{
			Name:        s.Name,
			Mode:        s.Mode,
			Running:     s.Running,
			Desired:     s.Desired,
			Completed:   s.Completed,
			Health:      state,
			UpdateState: s.UpdateState,
			Message:     message,
		})
		if rank(state) > rank(worst.State) {
			worst = application.Health{
				State:   state,
				Message: fmt.Sprintf("service '%s': %s", s.Name, message),
			}
		}
	}

	worst.Services = application.ServiceCounts{Healthy: healthy, Total: len(in.States)}
	return worst, services
}

// serviceHealth maps one service's convergence to a health state.
//
// Converged is healthy and wedged is degraded, both directly from the engine.
// The judgement this package makes is about the third: a service at 0/3 tasks
// is progressing according to the engine, and stays progressing for as long as
// it takes — which for an image that cannot be pulled is forever. Calling that
// healthy-ish indefinitely is the failure this whole rollup exists to avoid.
//
// The signal used is the application's own last sync. With a Wait policy that
// is the engine's --wait having timed out on exactly this convergence, so
// Degraded always has a concrete failure behind it rather than a stopwatch. An
// application that does not wait never produces that signal, and its slow
// release keeps reading progressing; that is a known limit, and preferable to a
// threshold that calls a slow first image pull broken.
//
// One wedged verdict can be overturned, and only one. A *completed* rollback is
// swarm finished: it restored the previous spec and stopped, so the service is
// running one spec and nothing more is going to happen to it. If that spec is
// what the repository declares, the deploy the status records is over and put
// right, and reporting it is reporting history — which for this caller never
// ends, because UpdateStatus survives until the next update and there is no next
// update for a service nothing is drifting (#130). `paused` and
// `rollback_paused` are swarm stopped part-way with its tasks in two
// generations, which a comparison of specs says nothing about, so both stay
// degraded whatever AsDeclared says.
//
// It is overturned by asking the engine again with the field the verdict came
// from cleared, rather than by substituting an answer. Every other rule then
// still decides — parity from the actual running count, a job's completed task,
// the stability window measured from task creation — and none of it is copied
// here. Clearing UpdateState puts the service on the arm a fresh install takes,
// which is what a service with no rollout outstanding is.
func serviceHealth(s charts.ServiceState, in Input) (application.HealthState, string) {
	c := s.Convergence()
	if c.Phase == charts.PhaseWedged &&
		s.UpdateState == string(swarm.UpdateStateRollbackCompleted) &&
		slices.Contains(in.AsDeclared, s.Name) {
		settled := s
		settled.UpdateState = ""
		c = settled.Convergence()
	}
	switch c.Phase {
	case charts.PhaseConverged:
		return application.HealthHealthy, ""
	case charts.PhaseWedged:
		return application.HealthDegraded, c.Reason
	default:
		if in.SyncFailed {
			return application.HealthDegraded, c.Reason + " (the last sync did not succeed)"
		}
		return application.HealthProgressing, c.Reason
	}
}

// Application rolls several releases up to one state for the whole application.
//
// Counts are summed rather than re-derived, so a list row's "7/9" is the same
// arithmetic the detail view shows per release.
func Application(releases []application.ReleaseStatus) application.Health {
	if len(releases) == 0 {
		return application.Health{State: application.HealthUnknown}
	}

	out := application.Health{State: application.HealthHealthy}
	for _, rel := range releases {
		out.Services.Healthy += rel.Health.Services.Healthy
		out.Services.Total += rel.Health.Services.Total
		if rank(rel.Health.State) > rank(out.State) {
			out.State = rel.Health.State
			out.Message = fmt.Sprintf("release '%s': %s", rel.Name, rel.Health.Message)
		}
	}
	return out
}

// rank orders health states by how much attention they need, so that a rollup
// surfaces the release that most wants a human rather than the first one seen.
//
// Degraded outranks Missing deliberately: something running and broken is a
// live problem, whereas Missing is the ordinary state of a release that has
// been declared and not yet synced — and the sync axis already says so, in the
// row right next to it.
//
// Unknown outranks Healthy alone, and has to outrank it. A release whose swarm
// could not be read is not a release known to be fine, so leaving it at the
// bottom would let the releases that did answer report the whole application as
// healthy on evidence nobody has — which is the same lie as the Missing one this
// ordering was extended to fix, told the quiet way round (#107).
func rank(s application.HealthState) int {
	switch s {
	case application.HealthDegraded:
		return 4
	case application.HealthMissing:
		return 3
	case application.HealthProgressing:
		return 2
	case application.HealthUnknown:
		return 1
	default:
		return 0
	}
}
