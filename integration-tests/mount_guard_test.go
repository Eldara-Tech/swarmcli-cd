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

// A chart may not bind the docker daemon's socket.
//
// It is the channel that decides whether any of the others matter. A chart
// chooses where its services run, so `node.role == manager` plus that socket is
// root over the swarm — the controller's credentials read straight out of its own
// service spec, any service overwritten, the controller deleted — and every guard
// that compares names is decoration beside it (#103).
//
// Here as well as in compose's unit tests because the claim is about the whole
// chain: CE renders the chart, plans the release, pre-flights its external
// references, and hands the manifest to this applier. Nothing upstream inspects a
// bind, so this is the proof that the refusal is the applier's and that a refused
// chart leaves nothing running.
//
// The volume and network halves of #103 have no test here and cannot have one.
// Both turn on what Swarm has mounted into the **controller**, and this harness
// runs the reconciler in-process, where readSelfMounts correctly finds no swarm
// task and both guards go quiet — which is the same reason the guard above is
// reachable at all, since a release record is read off the swarm rather than off
// the controller. They are unit-tested in backend against a fake that answers as
// a deployed controller does.
func TestAChartMayNotBindTheDockerSocket(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-guard-socket"
	t.Cleanup(func() { removeStack(t, release) })

	files := chartFiles(release, 1)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  app:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    volumes:\n" +
		"      - /var/run/docker.sock:/var/run/docker.sock:ro\n" +
		"    deploy:\n" +
		"      placement:\n" +
		"        constraints: [\"node.role == manager\"]\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n"

	rec := reconciler(t, releaseApp("socket", gitRepo(t, files), true))
	err := rec.SyncNow(context.Background(), "socket")
	if err == nil {
		t.Fatal("SyncNow(socket) = nil, want the deploy refused for binding the docker socket")
	}
	if !strings.Contains(err.Error(), "docker daemon's socket") {
		t.Fatalf("SyncNow(socket) = %v, want it refused by the bind guard", err)
	}
	if names := serviceNamesOf(t, cli, release); len(names) != 0 {
		t.Errorf("services = %v, want none created by a refused deploy", names)
	}
}

// The other way to get at a name — the config is one of the stack's **own**, no
// `external:` anywhere, and `name:` points it at something that already exists —
// used to be covered here by TestAStackMayNotClaimAReleaseRecordAsItsOwn.
//
// #99 removed the route rather than the rule. That shape needs a stack-owned
// config, a config's only content is `file:`, and `file:` read the controller's
// own filesystem, so it is refused before conversion. A config has no `driver:`
// to declare it without content the way a secret does, so there is no manifest
// left that reaches the declared-name branch of the guard for a config, and no
// honest way to deploy this against a real swarm.
//
// The rule is still enforced and still tested: backend's
// TestDeployStackRefusesAStackDeclaringAReleaseRecordName addresses
// rejectForbiddenResources directly. What is gone is the end-to-end proof — that
// the record exists under the claimed name, that the refusal comes from the
// applier rather than from something upstream, and that a refused deploy leaves
// the victim's record unlabelled. Restore this test if a chart is ever given a
// way to carry content of its own.
