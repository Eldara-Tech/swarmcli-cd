// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package drift decides whether an application matches git.
//
// Phase 1 implements one mode, driftDetection: manifest. Diffing the desired
// ServiceSpec against the running one is Phase 2 and answers a different
// question — this one compares what the repository renders to against what was
// last applied, which catches a changed chart version, changed values and a
// changed template, but not an operator running `docker service update` behind
// the controller's back.
//
// The whole decision comes out of the chart engine's plan. The engine already
// canonicalises values through a YAML round trip and compares the rendered
// manifest string, so asking it to plan is exactly as authoritative as its own
// apply — and it is the same work the sync would do, rather than an
// approximation of it.
package drift

import (
	"github.com/Eldara-Tech/swarmcli/charts"
	"github.com/Eldara-Tech/swarmcli/utils/textdiff"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// FromPlan maps a plan to an application's sync status and its per-release
// detail.
//
// The returned Sync carries no Revision and no LastSync: a plan does not know
// which commit produced it, or what happened the last time one was applied.
// The reconciler fills both. ReleaseStatus.Revision is likewise left zero —
// the chart revision number lives in the release records, not in the plan.
//
// Plan.Orphaned and Plan.Unmanaged are deliberately not surfaced. They are
// computed against Plan.Owner, which the engine derives by prefixing the
// release file's declared owner with "apply/" — the command line's namespace,
// not this controller's (Eldara-Tech/swarmcli#499). Beyond that they answer a
// per-swarm question, not a per-application one: with several applications on
// one swarm, each would report the others' releases as unmanaged.
func FromPlan(plan *charts.Plan) (application.Sync, []application.ReleaseStatus) {
	install, upgrade, unchanged := plan.Counts()

	sync := application.Sync{
		State: application.SyncSynced,
		Summary: application.SyncSummary{
			Install:   install,
			Upgrade:   upgrade,
			Unchanged: unchanged,
		},
	}
	if install+upgrade > 0 {
		sync.State = application.SyncOutOfSync
	}

	releases := make([]application.ReleaseStatus, 0, len(plan.Releases))
	for _, rp := range plan.Releases {
		releases = append(releases, releaseStatus(rp))
	}
	return sync, releases
}

func releaseStatus(rp charts.ReleasePlan) application.ReleaseStatus {
	state := application.SyncOutOfSync
	if rp.Action == charts.ActionUnchanged {
		state = application.SyncSynced
	}
	return application.ReleaseStatus{
		Name: rp.Name,
		// The reference as the release file wrote it, which is what an
		// operator recognises — not the chart's own name, which for a path
		// reference is a different string entirely.
		Chart:   rp.Ref,
		Version: rp.ToVersion,
		Action:  application.SyncAction(rp.Action),
		Sync:    state,
		Compat:  compat(rp.Compat),
	}
}

// compat surfaces a chart's declared engine requirement.
//
// The engine records the finding and never enforces it, so an unattended
// controller has to report it or the operator never learns the chart wanted a
// newer engine. A chart that declared nothing produces no finding worth
// showing: reporting "unknown" against every release would be noise that
// buries the one that matters.
func compat(f charts.CompatFinding) *application.Compat {
	if f.Required == "" {
		return nil
	}
	return &application.Compat{
		Status:   compatState(f.Status),
		Required: f.Required,
		Engine:   f.Engine,
		Reason:   f.Reason,
	}
}

func compatState(s charts.CompatStatus) application.CompatState {
	switch s {
	case charts.CompatOK:
		return application.CompatOK
	case charts.CompatIncompatible:
		return application.CompatIncompatible
	default:
		return application.CompatUnknown
	}
}

// Diffs renders the manifest change each release would undergo.
//
// Unchanged releases are omitted: their diff is empty by construction, and a
// response listing them would make a synced application look like it had
// twenty things to say.
//
// The rendering is the chart engine's own (utils/textdiff), so what the API
// serves is character-for-character what `swarmcli charts apply --diff` prints.
// Two renderings of the same change that disagree in whitespace would be worse
// than either alone.
func Diffs(plan *charts.Plan) []application.ReleaseDiff {
	var out []application.ReleaseDiff
	for _, rp := range plan.Releases {
		if rp.Action == charts.ActionUnchanged {
			continue
		}
		out = append(out, application.ReleaseDiff{
			Release: rp.Name,
			Action:  application.SyncAction(rp.Action),
			// CurrentManifest is empty for an install, so the whole manifest
			// renders as added, which is what an install is.
			Diff: textdiff.Lines(rp.CurrentManifest, rp.Manifest),
		})
	}
	return out
}
