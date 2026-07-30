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
// The target is the compose key rather than a `name:` override, which is both
// the shortest way to write it and the only one that gets this far: CE's
// external-reference pre-flight reads the deprecated `external: {name: X}` form
// and not the current `external: true` with a sibling `name:`, so the latter is
// refused upstream with a confusing error before the applier sees it
// (Eldara-Tech/swarmcli#513). Either way the stack does not deploy; only this
// form proves *this* guard is what stopped it.
func thiefChartFiles(release, target string) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  thief:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    configs: [\"" + target + "\"]\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"configs:\n" +
		"  \"" + target + "\":\n" +
		"    external: true\n"
	return files
}

// A tenant stack may not mount the chart engine's release records.
//
// Each is an ordinary Docker config holding a rendered manifest, so reading one
// hands over another application's deployed state — images, environment
// variable names, mounts, placement. They are also the half of #63 that no set
// captured at startup could cover, because a new one is written on every deploy.
//
// This runs against a real swarm because the guard turns on what a real daemon
// reports: that the release record exists under the name the manifest asks for,
// that CE's external-reference pre-flight is satisfied by it, and that the
// refusal therefore comes from the applier rather than from something upstream
// declining for an unrelated reason.
func TestAStackMayNotMountAReleaseRecord(t *testing.T) {
	cli := dockerClient(t)
	const (
		victim = "e2e-guard-victim"
		thief  = "e2e-guard-thief"
	)
	victimRepo := gitRepo(t, chartFiles(victim, 1))
	t.Cleanup(func() { removeStack(t, victim); removeStack(t, thief) })

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
	thiefRepo := gitRepo(t, thiefChartFiles(thief, record))

	rec2 := reconciler(t, releaseApp("thief", thiefRepo, true))
	err := rec2.SyncNow(ctx, "thief")
	if err == nil {
		t.Fatal("SyncNow(thief) = nil, want the deploy refused for mounting a release record")
	}
	if !strings.Contains(err.Error(), "release record") {
		t.Fatalf("SyncNow(thief) = %v, want it refused by the mount guard", err)
	}

	// Refused whole. The guard runs before any resource is created, so a refusal
	// must not leave a service behind for somebody to wonder about.
	if names := serviceNamesOf(t, cli, thief); len(names) != 0 {
		t.Errorf("services = %v, want none created by a refused deploy", names)
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
