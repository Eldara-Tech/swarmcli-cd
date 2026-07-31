// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func fullView() View {
	at := time.Date(2026, 7, 22, 9, 41, 10, 0, time.UTC)
	historyMax := 20
	return View{
		Spec: Spec{
			Name: "edge",
			Source: Source{
				RepoURL:     "https://github.com/acme/infra.git",
				Revision:    "main",
				ReleaseFile: "swarm/prod/swarmcli-release.yaml",
			},
			Destination: Destination{},
			SyncPolicy: SyncPolicy{
				Automated:  true,
				Interval:   Duration(60 * time.Second),
				Wait:       true,
				Timeout:    Duration(10 * time.Minute),
				HistoryMax: &historyMax,
			},
			DriftDetection: DriftManifest,
		},
		Status: Status{
			Sync: Sync{
				State:    SyncOutOfSync,
				Revision: "9f3c1ab",
				Summary:  SyncSummary{Upgrade: 1, Unchanged: 3},
				LastSync: &SyncResult{
					Revision:   "4b7e02d",
					StartedAt:  at.Add(-time.Hour),
					FinishedAt: at.Add(-time.Hour + 35*time.Second),
					Succeeded:  true,
				},
			},
			Health: Health{
				State:    HealthHealthy,
				Services: ServiceCounts{Healthy: 7, Total: 7},
			},
			Releases: []ReleaseStatus{{
				Name:     "traefik",
				Chart:    "swarmcli-charts/traefik",
				Version:  "0.1.1",
				Revision: 4,
				Action:   ActionUpgrade,
				Sync:     SyncOutOfSync,
				Health:   Health{State: HealthProgressing, Services: ServiceCounts{Healthy: 1, Total: 2}},
				Services: []ServiceStatus{{
					Name:        "traefik_edge",
					Mode:        "replicated",
					Running:     1,
					Desired:     2,
					Health:      HealthProgressing,
					UpdateState: "updating",
				}},
				Compat: &Compat{Status: CompatOK, Required: ">= 1.13.0", Engine: "1.13.0-rc4"},
			}},
			ObservedAt: at,
		},
	}
}

func TestViewJSONRoundTrip(t *testing.T) {
	want := fullView()

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got View
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the value\n got: %+v\nwant: %+v", got, want)
	}
}

// The list view is one request carrying everything a row renders, which is
// what makes omitting releases from it safe.
func TestListRowCarriesEverythingARowRenders(t *testing.T) {
	v := fullView()
	v.Status.Releases = nil

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(data)

	if strings.Contains(got, `"releases"`) {
		t.Errorf("releases should be omitted when not populated: %s", got)
	}
	for _, want := range []string{
		`"name":"edge"`,
		`"state":"out-of-sync"`,
		`"revision":"9f3c1ab"`,
		`"state":"healthy"`,
		`"services":{"healthy":7,"total":7}`,
		`"lastSync"`,
		`"observedAt"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("list row is missing %s: %s", want, got)
		}
	}
}

func TestSpecFromYAML(t *testing.T) {
	const src = `
name: whoami
source:
  repoURL: https://github.com/acme/infra.git
  revision: v1.2.0
  chart:
    release: hello
    ref: swarmcli-charts/whoami
    version: "0.1.8"
    values: [values/hello.yaml]
    repositories:
      - name: swarmcli-charts
        url: https://eldara-tech.github.io/swarmcli-charts
destination:
  swarm: staging
syncPolicy:
  automated: true
  interval: 90s
  timeout: 5m
driftDetection: manifest
`
	var got Spec
	if err := yaml.Unmarshal([]byte(src), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Source.Chart == nil {
		t.Fatal("chart source was not decoded")
	}
	if got.Source.Chart.Ref != "swarmcli-charts/whoami" || got.Source.Chart.Version != "0.1.8" {
		t.Errorf("chart source decoded wrong: %+v", *got.Source.Chart)
	}
	if got.Source.ReleaseFile != "" {
		t.Errorf("releaseFile should be empty for a chart source, got %q", got.Source.ReleaseFile)
	}
	if got.SyncPolicy.Interval != Duration(90*time.Second) {
		t.Errorf("interval = %v, want 90s", got.SyncPolicy.Interval)
	}
	if got.SyncPolicy.Timeout != Duration(5*time.Minute) {
		t.Errorf("timeout = %v, want 5m", got.SyncPolicy.Timeout)
	}
	if !got.DriftDetection.Valid() {
		t.Errorf("driftDetection %q should be valid", got.DriftDetection)
	}
	if got.Destination.Swarm != "staging" {
		t.Errorf("swarm = %q, want staging", got.Destination.Swarm)
	}
	if got.SyncPolicy.Prune || got.SyncPolicy.PruneVolumes {
		t.Errorf("prune defaults should be off, got prune=%v pruneVolumes=%v",
			got.SyncPolicy.Prune, got.SyncPolicy.PruneVolumes)
	}
}

// The charset has two jobs at once: narrow enough that a name cannot become a
// path outside the chart engine's cache, wide enough to leave the naming real
// chart repositories use alone.
func TestValidRepositoryName(t *testing.T) {
	for name, want := range map[string]bool{
		"swarmcli-charts":             true,
		"swarmcli_charts.v2":          true,
		"0charts":                     true,
		"":                            false,
		"..":                          false,
		".hidden":                     false,
		"has/slash":                   false,
		`has\backslash`:               false,
		"../../var/lib/swarmcli-cd/x": false,
		"has space":                   false,
		"-dash-first":                 false,
	} {
		if got := ValidRepositoryName(name); got != want {
			t.Errorf("ValidRepositoryName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSyncPolicyPruneFromYAML(t *testing.T) {
	const src = `
automated: true
prune: true
pruneVolumes: true
`
	var got SyncPolicy
	if err := yaml.Unmarshal([]byte(src), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Prune || !got.PruneVolumes {
		t.Errorf("prune=%v pruneVolumes=%v, want both true", got.Prune, got.PruneVolumes)
	}
}

// The prune fields are additive to a wire contract that is already published, so
// an application that does not mention them must serialise exactly as it did
// before they existed.
func TestPruneOffIsAbsentFromJSON(t *testing.T) {
	data, err := json.Marshal(SyncPolicy{Automated: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"prune", "pruneVolumes"} {
		if strings.Contains(string(data), key) {
			t.Errorf("%q should be omitted when off, got %s", key, data)
		}
	}
}

func TestAppSetStatusPrunedRoundTrip(t *testing.T) {
	want := AppSetStatus{
		Mode:        "git",
		Orphaned:    []string{"gone"},
		Pruned:      []string{"deleted"},
		PruneHeldBy: []string{"joining"},
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got AppSetStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the status:\n got %+v\nwant %+v", got, want)
	}

	// Empty rather than null for a controller that has pruned nothing, which is
	// every controller running the default.
	if data, err = json.Marshal(AppSetStatus{Mode: "static"}); err != nil {
		t.Fatalf("marshal: %v", err)
	} else if strings.Contains(string(data), "pruned") {
		t.Errorf("pruned should be omitted when empty, got %s", data)
	} else if strings.Contains(string(data), "pruneHeldBy") {
		t.Errorf("pruneHeldBy should be omitted when empty, got %s", data)
	}
}

// A manifest-mode application must serialise exactly as it did before the live
// axis existed. That is the whole compatibility promise of adding this feature:
// the mode every existing deployment runs on is untouched, and a client that
// has never heard of drift sees nothing new.
//
// Pinned as a literal rather than compared against a computed value, because a
// test that builds its expectation from the same types it is checking would
// accept any change made to both.
func TestManifestModeStatusCarriesNoDriftFields(t *testing.T) {
	status := Status{
		Sync: Sync{
			State:   SyncSynced,
			Summary: SyncSummary{Unchanged: 2},
		},
		Health: Health{State: HealthHealthy, Services: ServiceCounts{Healthy: 3, Total: 3}},
		Releases: []ReleaseStatus{{
			Name: "api", Chart: "./charts/api", Version: "0.1.0", Revision: 4,
			Action: ActionUnchanged, Sync: SyncSynced,
			Health: Health{State: HealthHealthy, Services: ServiceCounts{Healthy: 3, Total: 3}},
		}},
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	const want = `{"sync":{"state":"synced","summary":{"install":0,"upgrade":0,"unchanged":2}},` +
		`"health":{"state":"healthy","services":{"healthy":3,"total":3}},` +
		`"releases":[{"name":"api","chart":"./charts/api","version":"0.1.0","revision":4,` +
		`"action":"unchanged","sync":"synced",` +
		`"health":{"state":"healthy","services":{"healthy":3,"total":3}}}],` +
		`"observedAt":"0001-01-01T00:00:00Z"}`
	if string(data) != want {
		t.Errorf("manifest-mode status changed shape:\n got %s\nwant %s", data, want)
	}
}

// The live axis is nil, not an Unknown state, for an application that does not
// use it. "Not asked" and "asked, could not tell" are different, and only the
// second should ever show a state.
func TestDriftAxisIsOmittedWhenAbsent(t *testing.T) {
	data, err := json.Marshal(Status{Sync: Sync{State: SyncSynced}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "drift") {
		t.Errorf("drift should be omitted for a manifest-mode status, got %s", data)
	}
}

func TestReleaseDriftRoundTrip(t *testing.T) {
	want := ReleaseStatus{
		Name:   "api",
		Action: ActionUnchanged,
		Sync:   SyncOutOfSync,
		Drift: &ReleaseDrift{
			State: DriftStateDetected,
			Services: []ServiceDrift{{
				Name:      "api_web",
				Reason:    DriftModified,
				Fields:    []FieldDrift{{Field: "replicas", Desired: "1", Live: "3"}},
				Truncated: 2,
			}},
		},
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ReleaseStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the release:\n got %+v\nwant %+v", got, want)
	}
}

// The one place pruneResources widens the wire: a manifest-mode application that
// has enabled it does gain a drift axis, carrying the orphans and nothing else.
//
// No field comparison was performed and none is implied, which is what keeps
// `live`'s cost out of the default mode. The invariant above is untouched — it
// is about an application that has enabled nothing.
func TestManifestModeWithResourcePruneCarriesOrphansAndNoFieldDrift(t *testing.T) {
	status := Status{
		Sync: Sync{State: SyncOutOfSync, Summary: SyncSummary{Unchanged: 1}},
		Releases: []ReleaseStatus{{
			Name: "api", Action: ActionUnchanged, Sync: SyncOutOfSync,
			Drift: &ReleaseDrift{
				State: DriftStateDetected,
				Services: []ServiceDrift{{
					Name: "api_sidecar", Reason: DriftUnexpected, Orphaned: true,
				}},
			},
		}},
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	const want = `"drift":{"state":"detected","services":[` +
		`{"name":"api_sidecar","reason":"unexpected","orphaned":true}]}`
	if !strings.Contains(string(data), want) {
		t.Errorf("orphan reporting changed shape:\n got %s\nwant it to contain %s", data, want)
	}
}

// An unexpected service nobody proved is this application's carries no marker,
// so "orphaned" is absent from the payload rather than present and false.
func TestAnUnprovenUnexpectedServiceCarriesNoOrphanedField(t *testing.T) {
	data, err := json.Marshal(ServiceDrift{Name: "api_stranger", Reason: DriftUnexpected})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"name":"api_stranger","reason":"unexpected"}`; string(data) != want {
		t.Errorf("got %s, want %s", data, want)
	}
}

// A departed network, config or secret rides on the same axis, as its own list.
// Every entry is an orphan by construction, so there is no marker to carry: the
// three kinds are never field-compared, so appearing here can only mean the
// repository has stopped declaring it.
func TestDepartedResourcesMarshalAsTheirOwnAxis(t *testing.T) {
	data, err := json.Marshal(ReleaseDrift{
		State: DriftStateDetected,
		Resources: []ResourceDrift{
			{Kind: ResourceConfig, Name: "api_conf-a1b2"},
			{Kind: ResourceNetwork, Name: "api_internal"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	const want = `{"state":"detected","resources":[` +
		`{"kind":"config","name":"api_conf-a1b2"},` +
		`{"kind":"network","name":"api_internal"}]}`
	if string(data) != want {
		t.Errorf("got %s\nwant %s", data, want)
	}
}

// Every addition for #80 is additive and omitempty, so an application that has
// not enabled the sweep marshals exactly as it did before.
func TestAStatusWithoutTheSweepCarriesNoResourceFields(t *testing.T) {
	data, err := json.Marshal(ReleaseDrift{
		State:    DriftStateDetected,
		Services: []ServiceDrift{{Name: "api_web", Reason: DriftModified}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "resources") {
		t.Errorf("got %s, want no resources key at all", data)
	}

	summary, err := json.Marshal(Drift{State: DriftStateDetected, Services: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"state":"detected","services":1}`; string(summary) != want {
		t.Errorf("got %s, want %s", summary, want)
	}
}

// The three answers historyMax has, and why it needs a pointer to give them.
// Absent is the bound the controller applies for you; an explicit 0 is the chart
// engine's keep-everything, which was the old default and is the thing that
// fills a manager's raft log one deploy at a time.
func TestRetention(t *testing.T) {
	keepAll, twenty := 0, 20
	for name, tc := range map[string]struct {
		policy SyncPolicy
		want   int
	}{
		"absent":                  {SyncPolicy{}, DefaultHistoryMax},
		"explicit zero keeps all": {SyncPolicy{HistoryMax: &keepAll}, 0},
		"a number":                {SyncPolicy{HistoryMax: &twenty}, 20},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.policy.Retention(); got != tc.want {
				t.Errorf("Retention() = %d, want %d", got, tc.want)
			}
		})
	}
}

// An application that says nothing about historyMax still marshals without the
// key: the default is applied where the number is used, not written into the
// spec the API serves back.
func TestAnUnsetHistoryMaxIsAbsentFromTheWire(t *testing.T) {
	data, err := json.Marshal(SyncPolicy{Automated: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "historyMax") {
		t.Errorf("got %s, want no historyMax key", data)
	}

	keepAll := 0
	data, err = json.Marshal(SyncPolicy{Automated: true, HistoryMax: &keepAll})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// omitempty on a pointer omits nil, not the zero it points at — which is the
	// whole reason the field is one.
	if !strings.Contains(string(data), `"historyMax":0`) {
		t.Errorf("got %s, want an explicit zero on the wire", data)
	}
}
