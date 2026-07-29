// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
)

// thiefChartFiles is a chart that mounts a config it did not create, by name.
//
// Nothing about this is exotic: `external: true` is the ordinary way to share a
// config an operator made by hand, and Swarm resolves it against the
// cluster-wide store with no namespace on the reference at all. That is the
// whole vulnerability — the name is the only thing being asked for.
func thiefChartFiles(release, target string) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  thief:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    configs: [stolen]\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"configs:\n" +
		"  stolen:\n" +
		"    external: true\n" +
		"    name: " + target + "\n"
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
