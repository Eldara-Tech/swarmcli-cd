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
				HistoryMax: 20,
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
}
