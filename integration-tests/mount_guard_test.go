// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
	dockerclient "github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli-cd/application"
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

// socketChartFiles is the chart the whole of #64 is about: a service that binds
// the daemon's socket and asks to run on a manager, which is what Traefik's swarm
// provider, a Portainer agent and an autoheal sidecar all are.
func socketChartFiles(release string) map[string]string {
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
	return files
}

// A chart may not bind the docker daemon's socket unless its application says so.
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

	rec := reconciler(t, releaseApp("socket", gitRepo(t, socketChartFiles(release)), true))
	err := rec.SyncNow(context.Background(), "socket")
	if err == nil {
		t.Fatal("SyncNow(socket) = nil, want the deploy refused for binding the docker socket")
	}
	if !strings.Contains(err.Error(), "allow.hostPaths") {
		t.Fatalf("SyncNow(socket) = %v, want it refused by the bind guard", err)
	}
	if names := serviceNamesOf(t, cli, release); len(names) != 0 {
		t.Errorf("services = %v, want none created by a refused deploy", names)
	}
}

// And the capability #64 gives back: the same chart, under an application whose
// app-set entry permits that path, deploys.
//
// It is the one half of this feature that only a real swarm proves. The unit
// tests establish that the manifest converts and that the applier does not refuse
// it; what they cannot say is that the spec the daemon is then handed is one it
// accepts — a bind mount is a field Swarm validates on its own terms, and a guard
// that let the manifest through only to produce a service that never starts would
// have given nothing back. So this waits for the release to converge rather than
// merely for the deploy to return.
//
// The gate is the app set's, so nothing in the chart differs between this test
// and the one above. That is the property: the same chart is deployable or not
// depending on a file the chart author cannot write to.
func TestAChartMayBindTheDockerSocketWhenPermitted(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-gate-socket"
	t.Cleanup(func() { removeStack(t, release) })

	app := releaseApp("socket", gitRepo(t, socketChartFiles(release)), true)
	app.Allow = application.Allow{HostPaths: []string{"/var/run/docker.sock"}}

	rec := reconciler(t, app)
	if err := rec.SyncNow(context.Background(), "socket"); err != nil {
		t.Fatalf("SyncNow(socket) = %v, want the permitted bind deployed", err)
	}
	if names := serviceNamesOf(t, cli, release); len(names) != 1 {
		t.Fatalf("services = %v, want the one the chart declares", names)
	}

	// The mount as the daemon holds it, which is the whole point of asking a real
	// swarm: a permission that produced a service Swarm would not run is not a
	// capability restored.
	mounts := mountsOf(t, cli, release+"_app")
	if len(mounts) != 1 || mounts[0].Source != "/var/run/docker.sock" || !mounts[0].ReadOnly {
		t.Fatalf("mounts = %+v, want the socket bound read-only", mounts)
	}
}

// driverOptsChartFiles is a chart declaring a volume of its own whose driver_opts
// have the local driver bind a node directory, and mounting it by name.
func driverOptsChartFiles(release string) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  app:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    volumes:\n" +
		"      - data:/data\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		"volumes:\n" +
		"  data:\n" +
		"    driver: local\n" +
		"    driver_opts:\n" +
		"      type: none\n" +
		"      o: bind\n" +
		"      device: /tmp\n"
	return files
}

// A chart's own volume carrying driver_opts is refused unless its application
// names it in allow.volumes, through the whole chain: nothing upstream reads a
// volume's driver options, so the refusal is the applier's, and it leaves nothing
// running.
func TestAVolumeWithDriverOptionsNeedsAllowVolumes(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-guard-driveropts"
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, releaseApp("driveropts", gitRepo(t, driverOptsChartFiles(release)), true))
	err := rec.SyncNow(context.Background(), "driveropts")
	if err == nil {
		t.Fatal("SyncNow(driveropts) = nil, want the deploy refused for a volume with driver options")
	}
	if !strings.Contains(err.Error(), "allow.volumes") {
		t.Fatalf("SyncNow(driveropts) = %v, want it refused by the volume guard", err)
	}
	if names := serviceNamesOf(t, cli, release); len(names) != 0 {
		t.Errorf("services = %v, want none created by a refused deploy", names)
	}
}

// And the same chart deploys once its application names the volume, with the
// options reaching Swarm as the manifest wrote them. /tmp exists on every node, so
// the volume the local driver creates from them converges.
func TestAVolumeWithDriverOptionsDeploysWhenPermitted(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-gate-driveropts"
	// Swarm leaves a stack's volumes behind, and this one binds a node path.
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	app := releaseApp("driveropts", gitRepo(t, driverOptsChartFiles(release)), true)
	app.Allow = application.Allow{Volumes: []string{release + "_data"}}

	rec := reconciler(t, app)
	if err := rec.SyncNow(context.Background(), "driveropts"); err != nil {
		t.Fatalf("SyncNow(driveropts) = %v, want the permitted volume deployed", err)
	}
	if names := serviceNamesOf(t, cli, release); len(names) != 1 {
		t.Fatalf("services = %v, want the one the chart declares", names)
	}
	mounts := mountsOf(t, cli, release+"_app")
	if len(mounts) != 1 || mounts[0].VolumeOptions == nil || mounts[0].VolumeOptions.DriverConfig == nil ||
		mounts[0].VolumeOptions.DriverConfig.Options["device"] != "/tmp" {
		t.Fatalf("mounts = %+v, want the volume with its driver options", mounts)
	}
}

// mountsOf reads a service's mounts back off the daemon, which is the only place
// that says what Swarm accepted rather than what was asked for.
func mountsOf(t *testing.T, cli *dockerclient.Client, name string) []mount.Mount {
	t.Helper()
	svc, _, err := cli.ServiceInspectWithRaw(context.Background(), name, swarm.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("inspecting service %q: %v", name, err)
	}
	return svc.Spec.TaskTemplate.ContainerSpec.Mounts
}

// claimerChartFiles is the other way to get at a name: the config is one of the
// stack's **own** — no `external:` anywhere — and `name:` points it at something
// that already exists. Conversion namespace-scopes a declaration by default,
// which is what makes an unscoped one worth refusing.
//
// The content is a file the chart ships, which is the only way a config gets
// any. Its contents are irrelevant: a resource that already exists is never
// created from them.
func claimerChartFiles(release, target string) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/files/decoy.conf"] = "not the real thing\n"
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
		"    file: files/decoy.conf\n"
	return files
}

// A tenant stack may not claim one of the engine's release records as its own
// (#86).
//
// This is the same theft as TestAStackMayNotMountAReleaseRecord by a different
// route, and the route is the point: nothing here is an `external:` reference, so
// the guard's reference check sees a name the stack declares and passes it, and
// CE's external-reference pre-flight has nothing to pre-flight either. What used
// to stop it was the applier noticing, three steps later, that a config with that
// name already held different content — an error about immutability, for an
// attempt to take another release's rendered manifest over, after a network had
// already been created.
//
// A real swarm is what makes it a proof: the record exists under the name the
// manifest claims, the daemon would have handed it over, and the refusal comes
// from the declared-name guard rather than from something upstream declining for
// an unrelated reason — the chart's path is one the chart ships, so nothing that
// checks paths has anything to say about it.
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
	claimerRepo := gitRepo(t, claimerChartFiles(claimer, record))

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

// pathChartFiles is a chart declaring one config or secret whose file: is path,
// and shipping a file of its own so that the chart has one to name.
func pathChartFiles(release, kind, path string) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/files/token"] = "s3cr3t\n"
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  app:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sleep\", \"3600\"]\n" +
		"    deploy:\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n" +
		kind + ":\n" +
		"  loot:\n" +
		"    file: " + path + "\n"
	return files
}

// A chart's file: names content the chart ships and nothing else, through the
// whole chain — CE resolving the chart, the plan, and this applier. A path
// outside the chart is refused, and so is a secret's file: even when it names
// something the chart does ship; either way nothing is created.
//
// Which layer refuses the first two is deliberately not asserted: the chart
// engine refuses them while planning, and this applier would refuse them again.
// The secret case reaches the applier, because the engine resolves a secret's
// files/ path like any other.
func TestAChartMayNotReadAPathItDoesNotShip(t *testing.T) {
	cli := dockerClient(t)
	for _, tc := range []struct{ name, release, kind, path, why string }{
		{"an absolute path", "e2e-path-absolute", "configs", "/etc/hostname", "is an absolute path"},
		{"an escaping path", "e2e-path-escaping", "configs", "files/../../swarmcli-release.yaml", "escapes the chart"},
		{"a secret file", "e2e-path-secret", "secrets", "files/token", "file: is refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { removeStack(t, tc.release) })

			rec := reconciler(t, releaseApp("path", gitRepo(t, pathChartFiles(tc.release, tc.kind, tc.path)), true))
			err := rec.SyncNow(context.Background(), "path")
			if err == nil {
				t.Fatalf("SyncNow = nil, want file: %s refused", tc.path)
			}
			if !strings.Contains(err.Error(), tc.why) {
				t.Errorf("SyncNow = %v, want it to say %q", err, tc.why)
			}
			if names := serviceNamesOf(t, cli, tc.release); len(names) != 0 {
				t.Errorf("services = %v, want none created by a refused deploy", names)
			}
			if names := stackConfigNames(t, cli, tc.release); len(names) != 0 {
				t.Errorf("configs = %v, want none created by a refused deploy", names)
			}
			secrets, err := cli.SecretList(context.Background(), swarm.SecretListOptions{Filters: stackFilter(tc.release)})
			if err != nil {
				t.Fatal(err)
			}
			if len(secrets) != 0 {
				t.Errorf("%d secrets created by a refused deploy, want none", len(secrets))
			}
		})
	}
}
