// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package config

import (
	"os"
	"path/filepath"
	"reflect"
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

// An omitted driftDetection is the cheaper mode rather than an error. It stays
// the default now that there are two: live costs a read of the swarm per
// release per tick and, with an automated policy, writes — neither of which an
// operator who wrote nothing asked for.
func TestDriftDetectionDefaults(t *testing.T) {
	f, err := Parse([]byte(valid), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if got := f.Applications[0].DriftDetection; got != application.DriftManifest {
		t.Errorf("driftDetection = %q, want it defaulted to manifest", got)
	}
}

func TestDriftDetectionLiveAccepted(t *testing.T) {
	src := "applications:\n  - name: edge\n    source:\n      repoURL: https://x/y.git\n" +
		"      revision: main\n      releaseFile: r.yaml\n    driftDetection: live\n"
	f, err := Parse([]byte(src), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if got := f.Applications[0].DriftDetection; got != application.DriftLive {
		t.Errorf("driftDetection = %q, want live", got)
	}
}

// A release name is the Swarm stack namespace, so an application that names no
// release must reach the swarm under the name the operator wrote down — the
// application's (#139). Anything else is a stack an operator cannot find.
func TestChartReleaseDefaultsToTheApplicationName(t *testing.T) {
	src := "applications:\n  - name: eldara-zammad\n    source:\n      repoURL: https://x/y.git\n" +
		"      revision: main\n      chart: {ref: swarmcli-charts/zammad, version: \"0.1.0\", " +
		"repositories: [{name: swarmcli-charts, url: https://x/}]}\n"
	f, err := Parse([]byte(src), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if got := f.Applications[0].Source.Chart.Release; got != "eldara-zammad" {
		t.Errorf("release = %q, want the application's name", got)
	}
}

// The default is a default and not a rule: an application that says which
// release it installs still installs that one.
func TestAnExplicitChartReleaseIsKept(t *testing.T) {
	src := "applications:\n  - name: eldara-zammad\n    source:\n      repoURL: https://x/y.git\n" +
		"      revision: main\n      chart: {release: zammad, path: ./c}\n"
	f, err := Parse([]byte(src), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if got := f.Applications[0].Source.Chart.Release; got != "zammad" {
		t.Errorf("release = %q, want zammad", got)
	}
}

// Two applications sharing a release name share a Swarm stack: each deploys over
// the other every interval, and whichever stops declaring it reads the other's
// live stack as its own orphan. The error names both, because the file is where
// it is fixed and one name does not say which pair to look at.
func TestTwoApplicationsMayNotClaimOneRelease(t *testing.T) {
	const src = `
applications:
  - name: eldara-zammad
    source:
      repoURL: https://x/y.git
      revision: main
      chart: {release: zammad, path: ./c}
  - name: acme-zammad
    source:
      repoURL: https://x/z.git
      revision: main
      chart: {release: zammad, path: ./c}
`
	_, err := Parse([]byte(src), "applications.yaml")
	if err == nil {
		t.Fatal("Parse = nil, want a shared-release error")
	}
	for _, want := range []string{"eldara-zammad", "acme-zammad", `"zammad"`, "stack"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// The collision above is what the default makes unreachable: application names
// are unique within a set, so a set that writes no release name down cannot
// produce two claims on one stack.
func TestDefaultedReleasesCannotCollide(t *testing.T) {
	const src = `
applications:
  - name: eldara-zammad
    source: {repoURL: https://x/y.git, revision: main, chart: {path: ./c}}
  - name: acme-zammad
    source: {repoURL: https://x/z.git, revision: main, chart: {path: ./c}}
`
	f, err := Parse([]byte(src), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	a, b := f.Applications[0].Source.Chart.Release, f.Applications[1].Source.Chart.Release
	if a != "eldara-zammad" || b != "acme-zammad" {
		t.Errorf("releases = %q and %q, want each application's own name", a, b)
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
		"no chart source":    {base("      chart: {release: h}\n"), "one of path or ref"},
		"both chart source":  {base("      chart: {release: h, path: ./c, ref: r/c, version: \"1\"}\n"), "both path and ref"},
		"version with path":  {base("      chart: {release: h, path: ./c, version: \"1\"}\n"), "cannot be set with a path"},
		"ref without ver":    {base("      chart: {release: h, ref: repo/chart}\n"), "version is required with a ref"},
		"malformed ref":      {base("      chart: {release: h, ref: chart, version: \"1\"}\n"), "want repository/chart"},
		"ref without repos":  {base("      chart: {release: h, ref: repo/chart, version: \"1\"}\n"), "needs source.chart.repositories"},
		"traversal repo":     {base("      chart: {release: h, ref: repo/chart, version: \"1\", repositories: [{name: \"../../../../var/lib/swarmcli-cd/appset/applications\", url: https://x/}]}\n"), "repositories[0]: invalid name"},
		"slash repo":         {base("      chart: {release: h, ref: repo/chart, version: \"1\", repositories: [{name: \"has/slash\", url: https://x/}]}\n"), "repositories[0]: invalid name"},
		"leading dot repo":   {base("      chart: {release: h, ref: repo/chart, version: \"1\", repositories: [{name: \".hidden\", url: https://x/}]}\n"), "repositories[0]: invalid name"},
		"escaping values":    {base("      chart: {release: h, path: ./c, values: [../../secrets.yaml]}\n"), "escapes the repository"},
		"escaping chart":     {base("      chart: {release: h, path: ../../../charts/evil}\n"), "escapes the repository"},
		"empty values entry": {base("      chart: {release: h, path: ./c, values: [\"\"]}\n"), "values[0] is required"},
		"negative history":   {base("      releaseFile: r.yaml\n    syncPolicy: {historyMax: -1}\n"), "historyMax cannot be negative"},
		"negative interval":  {base("      releaseFile: r.yaml\n    syncPolicy: {interval: -5s}\n"), "cannot be negative"},
		"unknown drift":      {base("      releaseFile: r.yaml\n    driftDetection: livee\n"), "unsupported driftDetection"},
		"miscased drift":     {base("      releaseFile: r.yaml\n    driftDetection: Live\n"), "unsupported driftDetection"},
		"slash regauth":      {base("      releaseFile: r.yaml\n    registryAuth: \"has/slash\"\n"), "invalid registryAuth"},
		"traversal regauth":  {base("      releaseFile: r.yaml\n    registryAuth: \"..\"\n"), "invalid registryAuth"},
		"volumes no prune":   {base("      releaseFile: r.yaml\n    syncPolicy: {pruneVolumes: true}\n"), "pruneVolumes means nothing without prune"},
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

func TestPruneWithVolumesAccepted(t *testing.T) {
	const src = `
applications:
  - name: edge
    source: {repoURL: https://x/y.git, revision: main, releaseFile: r.yaml}
    syncPolicy: {automated: true, prune: true, pruneVolumes: true}
`
	f, err := Parse([]byte(src), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if p := f.Applications[0].SyncPolicy; !p.Prune || !p.PruneVolumes {
		t.Errorf("prune=%v pruneVolumes=%v, want both true", p.Prune, p.PruneVolumes)
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

// The charset a chart repository name is held to has to leave the naming people
// actually use alone: the refusals above are worth nothing if they also refuse
// "swarmcli-charts_v2.1".
func TestValidRepositoryNameAccepted(t *testing.T) {
	const src = `
applications:
  - name: edge
    source:
      repoURL: https://x/y.git
      revision: main
      chart:
        release: h
        ref: swarmcli-charts_v2.1/whoami
        version: "1"
        repositories: [{name: swarmcli-charts_v2.1, url: "https://x/"}]
`
	f, err := Parse([]byte(src), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if got := f.Applications[0].Source.Chart.Repositories[0].Name; got != "swarmcli-charts_v2.1" {
		t.Errorf("repository name = %q, want swarmcli-charts_v2.1", got)
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

func TestPruneFirstNeedsPrune(t *testing.T) {
	const src = `
applications:
  - name: edge
    source: {repoURL: https://x/y.git, revision: main, releaseFile: r.yaml}
    syncPolicy: {pruneFirst: true}
`
	_, err := Parse([]byte(src), "applications.yaml")
	if err == nil || !strings.Contains(err.Error(), "pruneFirst means nothing without prune or pruneResources") {
		t.Fatalf("Parse = %v, want pruneFirst refused without prune", err)
	}
}

// pruneResources stands alone, unlike pruneVolumes and pruneFirst. It is a
// sibling of prune rather than an extension of it: prune decides what happens to
// a whole release the application stopped declaring, pruneResources what happens
// inside one it still declares, and wanting the second without the first is a
// coherent position rather than a half-written config.
func TestPruneResourcesNeedsNoOtherFlag(t *testing.T) {
	const src = `
applications:
  - name: edge
    source: {repoURL: https://x/y.git, revision: main, releaseFile: r.yaml}
    syncPolicy: {automated: true, pruneResources: true}
`
	f, err := Parse([]byte(src), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if p := f.Applications[0].SyncPolicy; !p.PruneResources || p.Prune {
		t.Errorf("pruneResources=%v prune=%v, want true and false", p.PruneResources, p.Prune)
	}
}

// pruneFirst orders both sweeps, so either gate gives it something to order.
func TestPruneFirstIsAcceptedWithPruneResourcesAlone(t *testing.T) {
	const src = `
applications:
  - name: edge
    source: {repoURL: https://x/y.git, revision: main, releaseFile: r.yaml}
    syncPolicy: {automated: true, pruneResources: true, pruneFirst: true}
`
	if _, err := Parse([]byte(src), "applications.yaml"); err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
}

// An operator who writes nothing gets the bound; one who writes 0 gets the chart
// engine's keep-everything. Both have to survive the parse as different answers,
// which is the whole reason historyMax is a pointer.
func TestHistoryMaxDistinguishesUnsetFromZero(t *testing.T) {
	base := "applications:\n  - name: edge\n    source:\n      repoURL: https://x/y.git\n" +
		"      revision: main\n      releaseFile: r.yaml\n"

	f, err := Parse([]byte(base), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if got := f.Applications[0].SyncPolicy; got.HistoryMax != nil {
		t.Errorf("historyMax = %v, want it left unset", *got.HistoryMax)
	} else if got.Retention() != application.DefaultHistoryMax {
		t.Errorf("Retention() = %d, want the default %d", got.Retention(), application.DefaultHistoryMax)
	}

	f, err = Parse([]byte(base+"    syncPolicy: {historyMax: 0}\n"), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want an explicit zero accepted", err)
	}
	if got := f.Applications[0].SyncPolicy; got.HistoryMax == nil || *got.HistoryMax != 0 {
		t.Fatalf("historyMax = %v, want an explicit 0", got.HistoryMax)
	} else if got.Retention() != 0 {
		t.Errorf("Retention() = %d, want 0 — an explicit zero still means keep all", got.Retention())
	}
}

// The gate as an operator writes it, and the shape it decodes to. Five lists
// rather than one, because a bare name says nothing about which kind of thing it
// is and the answer differs: a path matches by containment on a node, a secret by
// equality across the cluster.
func TestAllowDecodes(t *testing.T) {
	f, err := Parse([]byte(`
applications:
  - name: edge
    source:
      repoURL: https://x/y.git
      revision: main
      releaseFile: r.yaml
    allow:
      hostPaths: [/var/run/docker.sock]
      secrets: [shared-apikey]
      configs: [shared-site]
      volumes: [shared-cache]
      networks: [traefik-public]
`), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	got := f.Applications[0].Allow
	want := application.Allow{
		HostPaths: []string{"/var/run/docker.sock"},
		Secrets:   []string{"shared-apikey"},
		Configs:   []string{"shared-site"},
		Volumes:   []string{"shared-cache"},
		Networks:  []string{"traefik-public"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("allow = %+v, want %+v", got, want)
	}
}

// An application that says nothing permits nothing, and that is a decode result
// rather than a default anything applies. Every app set written before this field
// existed reads this way.
func TestAnApplicationWithoutAllowPermitsNothing(t *testing.T) {
	f, err := Parse([]byte(valid), "applications.yaml")
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	if got := f.Applications[0].Allow; !reflect.DeepEqual(got, application.Allow{}) {
		t.Errorf("allow = %+v, want the zero value", got)
	}
}

// The entries that cannot match anything, refused at startup rather than at the
// deploy they were meant to permit. An allowlist fails silently in the direction
// that is hardest to spot: a mistyped entry permits nothing, and the deploy is
// then refused with a message about the chart.
func TestAllowIsValidated(t *testing.T) {
	for _, tc := range []struct{ name, allow, want string }{
		{"a relative host path", "hostPaths: [srv/app]", "must be absolute"},
		{"a host path with a trailing slash", "hostPaths: [/srv/app/]", "simplest form"},
		{"a host path with a traversal", "hostPaths: [/srv/app/../..]", "simplest form"},
		{"an empty host path", `hostPaths: [""]`, "is empty"},
		{"a secret with a slash", "secrets: [team-b/creds]", "allow.secrets[0]"},
		{"a config with a space", `configs: ["my config"]`, "allow.configs[0]"},
		{"a volume starting with a dot", "volumes: [.hidden]", "allow.volumes[0]"},
		{"an empty network name", `networks: [""]`, "allow.networks[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("applications:\n  - name: edge\n    source:\n      repoURL: https://x/y.git\n"+
				"      revision: main\n      releaseFile: r.yaml\n    allow:\n      "+tc.allow+"\n"), "applications.yaml")
			if err == nil {
				t.Fatal("Parse = nil, want the entry refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say what is wrong", err)
			}
		})
	}
}

// The entry an operator writes when they mean the node. It is not refused: they
// are the highest privilege there is here, and a rule that second-guessed them
// would be a policy this file is deliberately not.
func TestTheWholeNodeIsAValidHostPath(t *testing.T) {
	if _, err := Parse([]byte("applications:\n  - name: edge\n    source:\n      repoURL: https://x/y.git\n"+
		"      revision: main\n      releaseFile: r.yaml\n    allow:\n      hostPaths: [/]\n"), "applications.yaml"); err != nil {
		t.Fatalf("Parse = %v, want an operator able to say what they mean", err)
	}
}
