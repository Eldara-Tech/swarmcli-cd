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
			name: "a completed rollback is wedged",
			// At parity, and past the stability window, on the spec the deploy
			// set out to replace: swarmkit restores the previous spec before it
			// marks the rollback complete. The engine used to fall through to
			// the parity check here and answer converged, so this read Healthy
			// (Eldara-Tech/swarmcli#530, this repo's #104 item 4).
			state:   charts.ServiceState{Name: "api", Running: 2, Desired: 2, UpdateState: "rollback_completed", NewestTaskAge: stableAge},
			want:    application.HealthDegraded,
			message: "rolled back",
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

// Swarm keeps UpdateStatus on a service until its *next* update, so a rollback
// that ended with the repository's own spec running stays on the service for
// ever — and nothing is coming to clear it, because there is no drift, so
// nothing redeploys it. Read as wedged, that made the release Degraded
// permanently over a deploy that had already been undone and put right (#130).
//
// AsDeclared is the evidence that says so, and it overturns exactly one verdict:
// the paused pair is swarm stopped part-way, which a comparison of specs says
// nothing about, and stays degraded whatever the comparison found.
func TestACompletedRollbackIsHistoryOnceItsSpecIsWhatTheRepositoryDeclares(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		want  application.HealthState
	}{
		{"a finished rollback", "rollback_completed", application.HealthHealthy},
		{"a rollback swarm stopped part-way", "rollback_paused", application.HealthDegraded},
		{"an update swarm stopped part-way", "paused", application.HealthDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := charts.ServiceState{
				Name: "api", Running: 2, Desired: 2, UpdateState: tc.state, NewestTaskAge: stableAge,
			}
			h, services := Release(Input{
				States: []charts.ServiceState{state}, Installed: true, AsDeclared: []string{"api"},
			})
			if h.State != tc.want {
				t.Errorf("release health = %q, want %q", h.State, tc.want)
			}
			// The fact is still reported either way: what changed is the verdict
			// on it, not whether an operator can see what swarm did.
			if services[0].UpdateState != tc.state {
				t.Errorf("updateState = %q, want the daemon's own %q", services[0].UpdateState, tc.state)
			}
		})
	}
}

// Naming a service is not enough on its own. AsDeclared says the comparison
// found this spec equal to the repository's, and a service that is not at parity
// or is still inside its stability window is a live question the comparison
// never asked — so the rest of the engine's predicate still decides.
func TestAServiceRunningWhatIsDeclaredIsStillJudgedOnItsTasks(t *testing.T) {
	h, _ := Release(Input{
		States: []charts.ServiceState{
			{Name: "api", Running: 0, Desired: 3, UpdateState: "rollback_completed"},
		},
		Installed: true, AsDeclared: []string{"api"},
	})
	if h.State != application.HealthProgressing {
		t.Errorf("health = %q, want progressing — 0/3 tasks is not healthy however settled the rollout is", h.State)
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

// The other empty list, and the one that used to be indistinguishable from it.
// Nothing was read, so nothing may be asserted — and asserting the loudest thing
// this package can say, on a stack that is running perfectly, is what one slow
// daemon used to do to every release of an application (#107).
func TestAReleaseWhoseSwarmCouldNotBeReadIsUnknown(t *testing.T) {
	h, services := Release(Input{Installed: true, ReadFailed: true})
	if h.State != application.HealthUnknown {
		t.Errorf("health = %q, want unknown", h.State)
	}
	if strings.Contains(h.Message, "no services") {
		t.Errorf("message = %q, want it not to claim the swarm is empty", h.Message)
	}
	if services != nil {
		t.Errorf("services = %v, want none — none were read", services)
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
	unknown := application.ReleaseStatus{
		Name:   "cache",
		Health: application.Health{State: application.HealthUnknown},
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
			// A release nobody could read must not be reported as fine on the
			// strength of the ones that answered — the quiet half of #107.
			{"unknown beats healthy", []application.ReleaseStatus{healthy, unknown}, application.HealthUnknown},
			// And no further. "Could not tell" never outranks a problem somebody
			// did manage to see.
			{"progressing beats unknown", []application.ReleaseStatus{unknown, progressing}, application.HealthProgressing},
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
