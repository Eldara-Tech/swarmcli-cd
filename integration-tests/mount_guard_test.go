// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// thiefChartFiles is a chart that mounts a config it did not create, by name.
//
// Nothing about this is exotic: `external: true` is the ordinary way to share a
// config an operator made by hand, and Swarm resolves it against the
// cluster-wide store with no namespace on the reference at all. That is the
// whole vulnerability — the name is the only thing being asked for.
//
// key is what the service references and what the top-level entry is keyed on.
// name, when non-empty, is a `name:` beside `external: true`, which is what the
// resource is then actually called; key becomes a template-local alias that
// names nothing on the swarm.
func thiefChartFiles(release, key, name string) map[string]string {
	nameLine := ""
	if name != "" {
		nameLine = "    name: \"" + name + "\"\n"
	}
	files := chartFiles(release, 1)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  thief:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    configs: [\"" + key + "\"]\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"configs:\n" +
		"  \"" + key + "\":\n" +
		"    external: true\n" +
		nameLine
	return files
}

// externalForms is the two ways a manifest can say which external config it
// wants, and the guard has to stop both.
//
// They are one code path by the time cd sees them — docker/cli's loader
// normalises a sibling `name:` onto the entry, and a reference resolves to that
// name rather than to the map key — so what differs is only what a chart author
// writes. Both are covered because each is load-bearing for a different reason.
//
// The `name:` form is the real attack shape, and until recently it could not be
// tested here at all: CE's external-reference pre-flight read the deprecated
// `external: {name: X}` form and not the current `external: true` with a
// sibling `name:`, so a chart using it was refused upstream with a confusing
// error before the applier ever saw it. Eldara-Tech/swarmcli#513 fixed that, so
// the pre-flight now resolves the name, finds the record and hands the stack to
// this applier — which is what makes "the guard stopped it" a claim this test
// can prove rather than an assumption about which layer refused first. Its
// compose key is deliberately the name of nothing on the swarm, so a regression
// that resolved back to the key would fail the pre-flight instead of reaching
// the guard, and this test would notice.
//
// The key form is what every published chart in Eldara-Tech/swarmcli-charts is
// written as, so it is the one a regression would break in the field.
//
// alias is the compose key when it is not the name itself; empty means the key
// *is* the name.
var externalForms = []struct {
	form, thief, alias string
}{
	{form: "compose-key", thief: "e2e-guard-thief-key"},
	{form: "top-level-name", thief: "e2e-guard-thief-name", alias: "innocent-looking-alias"},
}

// A tenant stack may not mount the chart engine's release records.
//
// Each is an ordinary Docker config holding a rendered manifest, so reading one
// hands over another application's deployed state — images, environment
// variable names, mounts, placement. They are also the half of #63 that no set
// captured at startup could cover, because a new one is written on every deploy.
//
// Both ways of naming an external config are covered; see externalForms for why
// each earns its place.
//
// This runs against a real swarm because the guard turns on what a real daemon
// reports: that the release record exists under the name the manifest asks for,
// that CE's external-reference pre-flight is satisfied by it, and that the
// refusal therefore comes from the applier rather than from something upstream
// declining for an unrelated reason.
func TestAStackMayNotMountAReleaseRecord(t *testing.T) {
	cli := dockerClient(t)
	const victim = "e2e-guard-victim"
	victimRepo := gitRepo(t, chartFiles(victim, 1))
	t.Cleanup(func() { removeStack(t, victim) })

	ctx := context.Background()
	rec := reconciler(t, releaseApp("victim", victimRepo, true))
	if err := rec.SyncNow(ctx, "victim"); err != nil {
		t.Fatalf("SyncNow(victim) = %v, want nil", err)
	}
	waitForRunning(t, cli, victim, 1)

	// The record the first application just wrote. Naming revision 1 explicitly
	// rather than searching for it keeps the test honest about what it is
	// reaching for.
	record := "swarmcli.release." + victim + ".v1"

	// One victim for both forms. Each theft is refused before anything is
	// created, so a second one costs a reconcile and no deploy.
	for _, tc := range externalForms {
		t.Run(tc.form, func(t *testing.T) {
			key, name := record, ""
			if tc.alias != "" {
				key, name = tc.alias, record
			}
			t.Cleanup(func() { removeStack(t, tc.thief) })
			thiefRepo := gitRepo(t, thiefChartFiles(tc.thief, key, name))

			rec := reconciler(t, releaseApp("thief", thiefRepo, true))
			err := rec.SyncNow(ctx, "thief")
			if err == nil {
				t.Fatal("SyncNow(thief) = nil, want the deploy refused for mounting a release record")
			}
			if !strings.Contains(err.Error(), "release record") {
				t.Fatalf("SyncNow(thief) = %v, want it refused by the mount guard", err)
			}

			// Refused whole. The guard runs before any resource is created, so a
			// refusal must not leave a service behind for somebody to wonder about.
			if names := serviceNamesOf(t, cli, tc.thief); len(names) != 0 {
				t.Errorf("services = %v, want none created by a refused deploy", names)
			}
		})
	}
}

// claimerChartFiles is the other way to get at a name: the config is one of the
// stack's **own** — no `external:` anywhere — and `name:` points it at something
// that already exists. Conversion namespace-scopes a declaration by default,
// which is what makes an unscoped one worth refusing.
//
// The `file:` is sourced from a directory the test owns, resolved against the
// controller's filesystem — which here is the test process. Its contents are
// irrelevant: a resource that already exists is never created from them.
func claimerChartFiles(t *testing.T, release, target string) map[string]string {
	t.Helper()
	decoy := filepath.Join(t.TempDir(), "decoy.conf")
	if err := os.WriteFile(decoy, []byte("not the real thing\n"), 0o600); err != nil {
		t.Fatalf("writing the decoy: %v", err)
	}
	files := chartFiles(release, 1)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  claimer:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    configs: [\"mine\"]\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"configs:\n" +
		"  mine:\n" +
		"    name: \"" + target + "\"\n" +
		"    file: " + decoy + "\n"
	return files
}

// A tenant stack may not claim one of the engine's release records as its own
// (#86).
//
// This is the same theft as the test above by a different route, and the route is
// the point: nothing here is an `external:` reference, so the guard's reference
// check sees a name the stack declares and passes it, and CE's external-reference
// pre-flight has nothing to pre-flight either. What used to stop it was the
// applier noticing, three steps later, that a config with that name already held
// different content — an error about immutability, for an attempt to take another
// release's rendered manifest over, after a network had already been created.
//
// A real swarm is what makes it a proof: the record exists under the name the
// manifest claims, the daemon would have handed it over, and the refusal comes
// from the applier rather than from something upstream declining for an unrelated
// reason.
func TestAStackMayNotClaimAReleaseRecordAsItsOwn(t *testing.T) {
	cli := dockerClient(t)
	const (
		victim  = "e2e-claim-victim"
		claimer = "e2e-claim-claimer"
	)
	victimRepo := gitRepo(t, chartFiles(victim, 1))
	t.Cleanup(func() { removeStack(t, victim); removeStack(t, claimer) })

	ctx := context.Background()
	rec := reconciler(t, releaseApp("victim", victimRepo, true))
	if err := rec.SyncNow(ctx, "victim"); err != nil {
		t.Fatalf("SyncNow(victim) = %v, want nil", err)
	}
	waitForRunning(t, cli, victim, 1)

	record := "swarmcli.release." + victim + ".v1"
	claimerRepo := gitRepo(t, claimerChartFiles(t, claimer, record))

	rec2 := reconciler(t, releaseApp("claimer", claimerRepo, true))
	err := rec2.SyncNow(ctx, "claimer")
	if err == nil {
		t.Fatal("SyncNow(claimer) = nil, want the deploy refused for claiming a release record")
	}
	if !strings.Contains(err.Error(), "declares") || !strings.Contains(err.Error(), "release record") {
		t.Fatalf("SyncNow(claimer) = %v, want it refused by the mount guard for declaring the name", err)
	}

	if names := serviceNamesOf(t, cli, claimer); len(names) != 0 {
		t.Errorf("services = %v, want none created by a refused deploy", names)
	}
	// The victim's record is still the victim's, with its own content and no
	// tenant namespace label on it.
	if got := configLabels(t, cli, record)["com.docker.stack.namespace"]; got != "" {
		t.Errorf("the release record carries namespace label %q; a refused deploy relabelled it", got)
	}
}
