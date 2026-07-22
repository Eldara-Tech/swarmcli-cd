// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package health

import (
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// stableAge outlives the default stability window, so a service carrying it has
// already served that window out and parity is the only thing left to decide.
const stableAge = time.Hour

func running(name string, running, desired int) charts.ServiceState {
	return charts.ServiceState{
		Name: name, Mode: "replicated",
		Running: running, Desired: desired, NewestTaskAge: stableAge,
	}
}

// The six cases #15 names, over the engine's own predicate. What is asserted
// here is the mapping onto an application's health, not the rules underneath —
// those are swarmcli's, and tested there.
func TestReleaseHealthPerService(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      charts.ServiceState
		syncFailed bool
		want       application.HealthState
		message    string
	}{
		{
			name:  "never updated and at parity",
			state: running("api", 2, 2),
			want:  application.HealthHealthy,
		},
		{
			name:  "updating",
			state: charts.ServiceState{Name: "api", Running: 2, Desired: 2, UpdateState: "updating", NewestTaskAge: stableAge},
			want:  application.HealthProgressing,
			// An update in flight is not a failure.
			message: "rolling update in progress",
		},
		{
			name:  "paused is wedged",
			state: charts.ServiceState{Name: "api", Running: 2, Desired: 2, UpdateState: "paused"},
			want:  application.HealthDegraded,
			// Swarm never rolls back a rollback; this needs a human.
			message: "update paused",
		},
		{
			name: "inside the stability window",
			state: charts.ServiceState{
				Name: "api", Running: 1, Desired: 1,
				Monitor: 90 * time.Second, NewestTaskAge: 60 * time.Second,
			},
			want:    application.HealthProgressing,
			message: "stability window",
		},
		{
			name:  "completed one-shot job",
			state: charts.ServiceState{Name: "migrate", Running: 0, Completed: 1, Desired: 1, Job: true, NewestTaskAge: stableAge},
			want:  application.HealthHealthy,
		},
		{
			name:  "no tasks at all",
			state: running("api", 0, 3),
			want:  application.HealthProgressing,
			// Progressing, not degraded: nothing here says it has failed, only
			// that it has not arrived.
			message: "0/3 tasks running",
		},
		{
			name:       "no tasks after a failed sync",
			state:      running("api", 0, 3),
			syncFailed: true,
			want:       application.HealthDegraded,
			message:    "the last sync did not succeed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, services := Release(Input{
				States: []charts.ServiceState{tc.state}, Installed: true, SyncFailed: tc.syncFailed,
			})
			if h.State != tc.want {
				t.Errorf("release health = %q, want %q", h.State, tc.want)
			}
			if len(services) != 1 {
				t.Fatalf("got %d services, want 1", len(services))
			}
			if services[0].Health != tc.want {
				t.Errorf("service health = %q, want %q", services[0].Health, tc.want)
			}
			if tc.message != "" && !strings.Contains(services[0].Message, tc.message) {
				t.Errorf("message = %q, want it to mention %q", services[0].Message, tc.message)
			}
		})
	}
}

// A release the plan would install is declared and not deployed. Missing rather
// than Degraded: a UI has to tell "not there" from "there and broken", and this
// is also the ordinary state of a newly declared release before its first sync.
func TestUninstalledReleaseIsMissing(t *testing.T) {
	h, services := Release(Input{Installed: false})
	if h.State != application.HealthMissing {
		t.Errorf("health = %q, want missing", h.State)
	}
	if services != nil {
		t.Errorf("services = %v, want none for something that was never deployed", services)
	}
}

// The release record exists but nothing is running under it — the stack went
// away underneath the controller. Still Missing: there is nothing running to be
// unhealthy, and "it is gone" is a different problem from "it is failing".
func TestDeployedReleaseWithNoServicesIsMissing(t *testing.T) {
	h, _ := Release(Input{Installed: true})
	if h.State != application.HealthMissing {
		t.Errorf("health = %q, want missing", h.State)
	}
	if !strings.Contains(h.Message, "no services") {
		t.Errorf("message = %q, want it to say the swarm has nothing", h.Message)
	}
}

// The counts a list row renders, and the worst-wins rollup that decides its
// colour. One degraded service must not be hidden behind two healthy ones.
func TestReleaseRollupCountsAndWorstWins(t *testing.T) {
	h, services := Release(Input{Installed: true, States: []charts.ServiceState{
		running("web", 2, 2),
		{Name: "api", Running: 1, Desired: 1, UpdateState: "rollback_paused"},
		running("cache", 1, 1),
	}})

	if h.State != application.HealthDegraded {
		t.Errorf("health = %q, want degraded", h.State)
	}
	if !strings.Contains(h.Message, `service "api"`) {
		t.Errorf("message = %q, want the offending service named", h.Message)
	}
	if h.Services != (application.ServiceCounts{Healthy: 2, Total: 3}) {
		t.Errorf("counts = %+v, want 2/3", h.Services)
	}
	if len(services) != 3 {
		t.Errorf("got %d service rows, want one per service", len(services))
	}
}

// Per-service detail is what a UI descends into, so the raw facts travel with
// the verdict rather than being recomputed from it.
func TestServiceDetailCarriesTheFacts(t *testing.T) {
	_, services := Release(Input{Installed: true, States: []charts.ServiceState{{
		Name: "migrate", Mode: "replicated", Running: 0, Completed: 1, Desired: 1,
		Job: true, UpdateState: "completed", NewestTaskAge: stableAge,
	}}})

	got := services[0]
	if got.Name != "migrate" || got.Mode != "replicated" {
		t.Errorf("identity = %+v", got)
	}
	if got.Running != 0 || got.Completed != 1 || got.Desired != 1 {
		t.Errorf("counts = %+v, want the completed job counted", got)
	}
	if got.UpdateState != "completed" {
		t.Errorf("updateState = %q, want it carried through verbatim", got.UpdateState)
	}
}

func TestApplicationRollup(t *testing.T) {
	healthy := application.ReleaseStatus{
		Name:   "web",
		Health: application.Health{State: application.HealthHealthy, Services: application.ServiceCounts{Healthy: 2, Total: 2}},
	}
	progressing := application.ReleaseStatus{
		Name:   "api",
		Health: application.Health{State: application.HealthProgressing, Services: application.ServiceCounts{Healthy: 1, Total: 3}},
	}
	missing := application.ReleaseStatus{
		Name:   "worker",
		Health: application.Health{State: application.HealthMissing},
	}
	degraded := application.ReleaseStatus{
		Name:   "db",
		Health: application.Health{State: application.HealthDegraded, Services: application.ServiceCounts{Healthy: 0, Total: 1}},
	}

	t.Run("counts are summed across releases", func(t *testing.T) {
		got := Application([]application.ReleaseStatus{healthy, progressing})
		if got.Services != (application.ServiceCounts{Healthy: 3, Total: 5}) {
			t.Errorf("counts = %+v, want 3/5", got.Services)
		}
	})

	// Degraded outranks Missing deliberately: something running and broken is a
	// live problem, whereas Missing is the ordinary state of a release that has
	// been declared and not yet synced.
	t.Run("severity order", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			releases []application.ReleaseStatus
			want     application.HealthState
		}{
			{"all healthy", []application.ReleaseStatus{healthy, healthy}, application.HealthHealthy},
			{"progressing beats healthy", []application.ReleaseStatus{healthy, progressing}, application.HealthProgressing},
			{"missing beats progressing", []application.ReleaseStatus{progressing, missing}, application.HealthMissing},
			{"degraded beats missing", []application.ReleaseStatus{missing, degraded}, application.HealthDegraded},
			{"degraded wins from anywhere in the list", []application.ReleaseStatus{degraded, healthy, progressing}, application.HealthDegraded},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := Application(tc.releases); got.State != tc.want {
					t.Errorf("state = %q, want %q", got.State, tc.want)
				}
			})
		}
	})

	t.Run("the worst release is named", func(t *testing.T) {
		got := Application([]application.ReleaseStatus{healthy, degraded})
		if !strings.Contains(got.Message, `release "db"`) {
			t.Errorf("message = %q, want the offending release named", got.Message)
		}
	})

	// No releases means nothing has been observed, which is not the same as
	// nothing being wrong.
	t.Run("no releases is unknown", func(t *testing.T) {
		if got := Application(nil); got.State != application.HealthUnknown {
			t.Errorf("state = %q, want unknown", got.State)
		}
	})
}
