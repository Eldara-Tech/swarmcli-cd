// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

const valid = `
apiVersion: v1
applications:
  - name: edge
    source:
      repoURL: https://github.com/acme/infra.git
      revision: main
      releaseFile: swarm/prod/swarmcli-release.yaml
    syncPolicy:
      automated: true
      interval: 60s
      wait: true
      timeout: 10m
      historyMax: 20
  - name: hello
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
      swarm: ""
    driftDetection: manifest
`

func TestParseValid(t *testing.T) {
	f, err := Parse([]byte(valid), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if len(f.Applications) != 2 {
		t.Fatalf("got %d applications, want 2", len(f.Applications))
	}
	if f.Path != "applications.yaml" {
		t.Errorf("Path = %q, want applications.yaml", f.Path)
	}

	edge := f.Applications[0]
	if edge.Name != "edge" || edge.Source.ReleaseFile != "swarm/prod/swarmcli-release.yaml" {
		t.Errorf("first application decoded wrong: %+v", edge)
	}
	if edge.SyncPolicy.Interval != application.Duration(60*time.Second) {
		t.Errorf("interval = %v, want 60s", edge.SyncPolicy.Interval)
	}

	hello := f.Applications[1]
	if hello.Source.Chart == nil || hello.Source.Chart.Version != "0.1.8" {
		t.Errorf("second application decoded wrong: %+v", hello.Source)
	}
}

// An omitted driftDetection is the mode this build implements rather than an
// error: there is one mode, so requiring it in every application would be
// boilerplate that cannot say anything else.
func TestDriftDetectionDefaults(t *testing.T) {
	f, err := Parse([]byte(valid), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if got := f.Applications[0].DriftDetection; got != application.DriftManifest {
		t.Errorf("driftDetection = %q, want it defaulted to manifest", got)
	}
}

// A misspelled key that was quietly ignored would leave a setting an operator
// believes they configured silently doing nothing.
func TestUnknownKeysRejected(t *testing.T) {
	const src = `
applications:
  - name: edge
    source:
      repoURL: https://github.com/acme/infra.git
      revision: main
      releaseFile: r.yaml
    syncPolicey: {automated: true}
`
	_, err := Parse([]byte(src), "applications.yaml")
	if err == nil {
		t.Fatal("Parse = nil, want an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "syncPolicey") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

func TestValidationErrors(t *testing.T) {
	base := func(body string) string {
		return "applications:\n  - name: edge\n    source:\n      repoURL: https://x/y.git\n      revision: main\n" + body
	}

	for name, tc := range map[string]struct{ src, want string }{
		"empty file":         {"", "no applications"},
		"no applications":    {"applications: []\n", "no applications"},
		"bad apiVersion":     {"apiVersion: v2\napplications: []\n", "apiVersion"},
		"missing name":       {"applications:\n  - source:\n      repoURL: https://x/y.git\n      revision: main\n      releaseFile: r.yaml\n", "name is required"},
		"bad name":           {"applications:\n  - name: Edge Prod\n    source:\n      repoURL: https://x/y.git\n      revision: main\n      releaseFile: r.yaml\n", "invalid name"},
		"colon in name":      {"applications:\n  - name: \"a:b\"\n    source:\n      repoURL: https://x/y.git\n      revision: main\n      releaseFile: r.yaml\n", "invalid name"},
		"no repoURL":         {"applications:\n  - name: edge\n    source:\n      revision: main\n      releaseFile: r.yaml\n", "repoURL is required"},
		"no revision":        {"applications:\n  - name: edge\n    source:\n      repoURL: https://x/y.git\n      releaseFile: r.yaml\n", "revision is required"},
		"no source type":     {base(""), "one of releaseFile or chart"},
		"both source types":  {base("      releaseFile: r.yaml\n      chart: {release: h, path: ./c}\n"), "both releaseFile and chart"},
		"absolute path":      {base("      releaseFile: /etc/passwd\n"), "must be relative"},
		"escaping path":      {base("      releaseFile: ../../etc/passwd\n"), "escapes the repository"},
		"no chart release":   {base("      chart: {path: ./c}\n"), "chart.release is required"},
		"no chart source":    {base("      chart: {release: h}\n"), "one of path or ref"},
		"both chart source":  {base("      chart: {release: h, path: ./c, ref: r/c, version: \"1\"}\n"), "both path and ref"},
		"version with path":  {base("      chart: {release: h, path: ./c, version: \"1\"}\n"), "cannot be set with a path"},
		"ref without ver":    {base("      chart: {release: h, ref: repo/chart}\n"), "version is required with a ref"},
		"malformed ref":      {base("      chart: {release: h, ref: chart, version: \"1\"}\n"), "want repository/chart"},
		"ref without repos":  {base("      chart: {release: h, ref: repo/chart, version: \"1\"}\n"), "needs source.chart.repositories"},
		"escaping values":    {base("      chart: {release: h, path: ./c, values: [../../secrets.yaml]}\n"), "escapes the repository"},
		"escaping chart":     {base("      chart: {release: h, path: ../../../charts/evil}\n"), "escapes the repository"},
		"empty values entry": {base("      chart: {release: h, path: ./c, values: [\"\"]}\n"), "values[0] is required"},
		"negative history":   {base("      releaseFile: r.yaml\n    syncPolicy: {historyMax: -1}\n"), "historyMax cannot be negative"},
		"negative interval":  {base("      releaseFile: r.yaml\n    syncPolicy: {interval: -5s}\n"), "cannot be negative"},
		"unknown drift":      {base("      releaseFile: r.yaml\n    driftDetection: live\n"), "unsupported driftDetection"},
		"slash regauth":      {base("      releaseFile: r.yaml\n    registryAuth: \"has/slash\"\n"), "invalid registryAuth"},
		"traversal regauth":  {base("      releaseFile: r.yaml\n    registryAuth: \"..\"\n"), "invalid registryAuth"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src), "applications.yaml")
			if err == nil {
				t.Fatalf("Parse = nil, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "applications.yaml") {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
}

func TestValidRegistryAuthAccepted(t *testing.T) {
	const src = `
applications:
  - name: edge
    registryAuth: swarmcli-cd-regauth-edge
    source: {repoURL: https://x/y.git, revision: main, releaseFile: r.yaml}
`
	f, err := Parse([]byte(src), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if got := f.Applications[0].RegistryAuth; got != "swarmcli-cd-regauth-edge" {
		t.Errorf("RegistryAuth = %q, want swarmcli-cd-regauth-edge", got)
	}
}

func TestDuplicateNames(t *testing.T) {
	const src = `
applications:
  - name: edge
    source: {repoURL: https://x/y.git, revision: main, releaseFile: r.yaml}
  - name: edge
    source: {repoURL: https://x/y.git, revision: main, releaseFile: r.yaml}
`
	_, err := Parse([]byte(src), "applications.yaml")
	if err == nil || !strings.Contains(err.Error(), "duplicate application name") {
		t.Errorf("Parse = %v, want a duplicate-name error", err)
	}
}

// A repositories entry that is half-filled resolves nothing, so it fails at
// load rather than at the first reconcile.
func TestIncompleteRepository(t *testing.T) {
	const src = `
applications:
  - name: edge
    source:
      repoURL: https://x/y.git
      revision: main
      chart:
        release: h
        ref: repo/chart
        version: "1"
        repositories: [{name: repo}]
`
	_, err := Parse([]byte(src), "applications.yaml")
	if err == nil || !strings.Contains(err.Error(), "needs both name and url") {
		t.Errorf("Parse = %v, want an incomplete-repository error", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "applications.yaml")
	if err := os.WriteFile(p, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := Load(p)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if f.Path != p {
		t.Errorf("Path = %q, want %q", f.Path, p)
	}
	if len(f.Applications) != 2 {
		t.Errorf("got %d applications, want 2", len(f.Applications))
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil || !strings.Contains(err.Error(), "reading applications file") {
		t.Errorf("Load = %v, want a read error", err)
	}
}
