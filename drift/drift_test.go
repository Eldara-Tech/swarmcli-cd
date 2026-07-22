// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package drift

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

func TestSyncedWhenEveryReleaseIsUnchanged(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{
		{Name: "traefik", Action: charts.ActionUnchanged},
		{Name: "whoami", Action: charts.ActionUnchanged},
	}}

	sync, releases := FromPlan(plan)

	if sync.State != application.SyncSynced {
		t.Errorf("state = %q, want synced", sync.State)
	}
	if want := (application.SyncSummary{Unchanged: 2}); sync.Summary != want {
		t.Errorf("summary = %+v, want %+v", sync.Summary, want)
	}
	for _, r := range releases {
		if r.Sync != application.SyncSynced {
			t.Errorf("release %q = %q, want synced", r.Name, r.Sync)
		}
	}
}

// Any release the sync would touch makes the application OutOfSync — a stack
// that is three-quarters current is not current.
func TestOutOfSyncWhenAnythingWouldChange(t *testing.T) {
	for name, action := range map[string]charts.Action{
		"install": charts.ActionInstall,
		"upgrade": charts.ActionUpgrade,
	} {
		t.Run(name, func(t *testing.T) {
			plan := &charts.Plan{Releases: []charts.ReleasePlan{
				{Name: "traefik", Action: charts.ActionUnchanged},
				{Name: "whoami", Action: action},
			}}

			sync, releases := FromPlan(plan)

			if sync.State != application.SyncOutOfSync {
				t.Errorf("state = %q, want out-of-sync", sync.State)
			}
			if sync.Summary.Unchanged != 1 {
				t.Errorf("summary = %+v, want one unchanged", sync.Summary)
			}
			if releases[0].Sync != application.SyncSynced {
				t.Errorf("the unchanged release reported %q", releases[0].Sync)
			}
			if releases[1].Sync != application.SyncOutOfSync {
				t.Errorf("the changed release reported %q", releases[1].Sync)
			}
			if releases[1].Action != application.SyncAction(action) {
				t.Errorf("action = %q, want %q", releases[1].Action, action)
			}
		})
	}
}

func TestEmptyPlanIsSynced(t *testing.T) {
	sync, releases := FromPlan(&charts.Plan{})

	if sync.State != application.SyncSynced {
		t.Errorf("state = %q, want synced", sync.State)
	}
	if len(releases) != 0 {
		t.Errorf("releases = %v, want none", releases)
	}
}

// The reference as the release file wrote it is what an operator recognises;
// the chart's own name can be a different string entirely.
func TestReleaseCarriesTheReferenceAndTargetVersion(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{{
		Name:        "whoami",
		Ref:         "swarmcli-charts/whoami",
		Action:      charts.ActionUpgrade,
		FromVersion: "0.1.7",
		ToVersion:   "0.1.8",
		Chart:       charts.ReleaseChart{Name: "whoami", Version: "0.1.8"},
	}}}

	_, releases := FromPlan(plan)

	want := application.ReleaseStatus{
		Name:    "whoami",
		Chart:   "swarmcli-charts/whoami",
		Version: "0.1.8",
		Action:  application.ActionUpgrade,
		Sync:    application.SyncOutOfSync,
	}
	if !reflect.DeepEqual(releases[0], want) {
		t.Errorf("release = %+v, want %+v", releases[0], want)
	}
}

// The engine records a chart's declared engine requirement and never enforces
// it, so an unattended controller has to report it.
func TestCompatIsSurfacedOnlyWhenTheChartDeclaredSomething(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{
		{Name: "silent", Action: charts.ActionUnchanged},
		{Name: "declared", Action: charts.ActionUpgrade, Compat: charts.CompatFinding{
			Status:   charts.CompatIncompatible,
			Required: ">= 1.14.0",
			Engine:   "1.13.0-rc4",
			Reason:   "engine is older than the chart requires",
		}},
		{Name: "unknown-engine", Action: charts.ActionUpgrade, Compat: charts.CompatFinding{
			Status:   charts.CompatUnknown,
			Required: ">= 1.13.0",
		}},
		{Name: "satisfied", Action: charts.ActionUpgrade, Compat: charts.CompatFinding{
			Status:   charts.CompatOK,
			Required: ">= 1.12.0",
			Engine:   "1.13.0-rc4",
		}},
	}}

	_, releases := FromPlan(plan)

	if releases[0].Compat != nil {
		t.Errorf("a chart declaring nothing produced %+v, want no finding", releases[0].Compat)
	}
	if got := releases[1].Compat; got == nil || got.Status != application.CompatIncompatible || got.Required != ">= 1.14.0" {
		t.Errorf("compat = %+v, want the incompatible finding", got)
	}
	if got := releases[2].Compat; got == nil || got.Status != application.CompatUnknown {
		t.Errorf("compat = %+v, want an unknown finding for a declared requirement", got)
	}
	if got := releases[3].Compat; got == nil || got.Status != application.CompatOK {
		t.Errorf("compat = %+v, want a satisfied finding", got)
	}
}

func TestDiffsSkipUnchangedReleases(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{
		{Name: "steady", Action: charts.ActionUnchanged, CurrentManifest: "a\n", Manifest: "a\n"},
		{Name: "moving", Action: charts.ActionUpgrade, CurrentManifest: "replicas: 1\n", Manifest: "replicas: 3\n"},
	}}

	got := Diffs(plan)

	if len(got) != 1 {
		t.Fatalf("got %d diffs, want only the changed release: %+v", len(got), got)
	}
	if got[0].Release != "moving" || got[0].Action != application.ActionUpgrade {
		t.Errorf("diff = %+v, want the upgrade", got[0])
	}
	if !strings.Contains(got[0].Diff, "- replicas: 1") || !strings.Contains(got[0].Diff, "+ replicas: 3") {
		t.Errorf("diff does not show the change:\n%s", got[0].Diff)
	}
}

// An install has no current manifest, so the whole thing is added — which is
// what an install is, and an empty diff would be a lie.
func TestDiffForAnInstallShowsTheWholeManifest(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{{
		Name:     "new",
		Action:   charts.ActionInstall,
		Manifest: "services:\n  web:\n    image: whoami\n",
	}}}

	got := Diffs(plan)

	if len(got) != 1 {
		t.Fatalf("got %d diffs, want 1", len(got))
	}
	if !strings.Contains(got[0].Diff, "+ services:") || !strings.Contains(got[0].Diff, "image: whoami") {
		t.Errorf("diff does not add the manifest:\n%s", got[0].Diff)
	}
	if strings.Contains(got[0].Diff, "- ") {
		t.Errorf("an install removed something:\n%s", got[0].Diff)
	}
}

func TestDiffsOfASyncedPlanAreEmpty(t *testing.T) {
	plan := &charts.Plan{Releases: []charts.ReleasePlan{{Name: "steady", Action: charts.ActionUnchanged}}}

	if got := Diffs(plan); len(got) != 0 {
		t.Errorf("Diffs = %+v, want none", got)
	}
}

// Orphaned and Unmanaged are computed against an owner id the engine namespaces
// under "apply/", so they are not this controller's to report — and they answer
// a per-swarm question rather than a per-application one.
func TestOrphanedAndUnmanagedAreNotSurfaced(t *testing.T) {
	plan := &charts.Plan{
		Owner:     "apply/prod",
		Releases:  []charts.ReleasePlan{{Name: "whoami", Action: charts.ActionUnchanged}},
		Orphaned:  []string{"gone"},
		Unmanaged: []string{"somebody-elses"},
	}

	sync, releases := FromPlan(plan)

	if sync.State != application.SyncSynced {
		t.Errorf("state = %q: releases nobody declared must not make an application out of sync", sync.State)
	}
	if len(releases) != 1 {
		t.Errorf("got %d releases, want only the declared one", len(releases))
	}
}
