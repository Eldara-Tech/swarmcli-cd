// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// The test the whole feature stands or falls on.
//
// Nothing was touched, so nothing must be reported. A spec read back from the
// daemon is not the one that was written — replicas defaulted, the image
// resolved to a digest, resources returned as zero structs, labels as an empty
// map, a published port assigned, platforms filled from the image manifest —
// and every one of those is a way a comparison can report drift that is not
// there. On an automated application that is not merely noise: the controller
// would redeploy an untouched service on every tick, forever.
//
// The unit suite asserts the same rules against hand-built specs. Only this can
// say whether the daemon actually behaves the way those specs assume, which is
// the gap the #54 and #62 retrospectives are about.
func TestLiveDriftIsNotReportedOnAnUntouchedStack(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-clean"
	repo := gitRepo(t, richChartFiles(t, release, 2))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("clean", repo, true))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "clean"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 3) // two app replicas and one sidecar

	// Twice: the first reconcile follows the deploy that wrote the spec, the
	// second sees it only as the daemon returns it.
	for _, pass := range []string{"after the deploy", "on the next reconcile"} {
		if err := rec.Sync(ctx, "clean"); err != nil {
			t.Fatalf("Sync %s = %v, want nil", pass, err)
		}
		d := driftOf(t, rec, "clean")
		if d == nil {
			t.Fatalf("%s: no drift axis at all on a live-mode application", pass)
		}
		if d.State != application.DriftStateNone {
			t.Errorf("%s: drift = %q with %+v, want none — nothing was touched",
				pass, d.State, d.Services)
		}
		view, _ := rec.View("clean")
		if view.Status.Sync.State != application.SyncSynced {
			t.Errorf("%s: sync = %q, want synced", pass, view.Status.Sync.State)
		}
	}
}

// The canonical case from the issue: an operator scales a service by hand.
//
// On a manual application it is reported and left alone, which is what makes the
// report trustworthy — the controller says what it sees before it is allowed to
// act on it.
func TestLiveDriftDetectsAnOutOfBandScale(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-detect"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, liveDriftApp("detect", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "detect"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	three := uint64(3)
	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.Mode.Replicated.Replicas = &three
	})
	waitForRunning(t, cli, release, 3)

	if err := rec.Sync(ctx, "detect"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	d := driftOf(t, rec, "detect")
	if d == nil || d.State != application.DriftStateDetected {
		t.Fatalf("drift = %+v, want detected", d)
	}
	if got := fieldOf(t, d, release+"_app", "replicas"); got.Desired != "1" || got.Live != "3" {
		t.Errorf("replicas drift = %+v, want desired 1 and live 3", got)
	}

	// Folded into the sync axis, so the sync button and anything alerting on it
	// work without knowing this mode exists.
	view, _ := rec.View("detect")
	if view.Status.Sync.State != application.SyncOutOfSync {
		t.Errorf("sync = %q, want out-of-sync", view.Status.Sync.State)
	}
	if view.Status.Sync.Summary.Drifted != 1 {
		t.Errorf("summary.drifted = %d, want 1", view.Status.Sync.Summary.Drifted)
	}

	// And nothing was corrected: manual reports, it does not act.
	if got := runningReplicas(t, cli, release); got != 3 {
		t.Errorf("running replicas = %d, want the manual application to have left them at 3", got)
	}
}

// The same drift on an automated application, which is what the mode is for:
// the swarm goes back to what the repository declares without anyone asking.
func TestLiveDriftConvergesAnOutOfBandScale(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-converge"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, liveDriftApp("converge", repo, true))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "converge"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	four := uint64(4)
	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.Mode.Replicated.Replicas = &four
	})
	waitForRunning(t, cli, release, 4)

	if err := rec.Sync(ctx, "converge"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	waitForRunning(t, cli, release, 1)

	// The confirming re-plan re-compares, so a correction that landed reports
	// itself as settled rather than leaving the application permanently drifted.
	if d := driftOf(t, rec, "converge"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
	view, _ := rec.View("converge")
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("sync = %q, want synced after the correction", view.Status.Sync.State)
	}
}

// The image case, which no unit test can prove.
//
// `docker service update --image` rewrites the spec's image and leaves the stack
// image label naming the tag the last deploy wrote. A comparison built on those
// two labels sees nothing; a converge built on the same assumption writes the
// operator's image straight back. Both halves are only observable against a real
// daemon, because both turn on what it does with an image reference.
func TestLiveDriftConvergesAnOutOfBandImage(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-image"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, liveDriftApp("image", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "image"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Image = "busybox:1.37"
	})

	if err := rec.Sync(ctx, "image"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	got := fieldOf(t, driftOf(t, rec, "image"), release+"_app", "image")
	if got.Desired != "busybox:1.36" {
		t.Errorf("desired image = %q, want what the chart declares", got.Desired)
	}

	// Manual, so correct it explicitly — and the correction must actually take,
	// which is what the prepareUpdate guard exists for.
	if err := rec.SyncNow(ctx, "image"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	svc := serviceOf(t, cli, release+"_app")
	if img := svc.Spec.TaskTemplate.ContainerSpec.Image; !hasTag(img, "busybox:1.36") {
		t.Errorf("running image = %q, want it back on busybox:1.36", img)
	}
	if d := driftOf(t, rec, "image"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// hasTag reports whether a live image reference names tag, ignoring any digest
// the daemon resolved it to.
func hasTag(image, tag string) bool {
	if image == tag {
		return true
	}
	return len(image) > len(tag) && image[:len(tag)] == tag && image[len(tag)] == '@'
}

// A published port moved by hand.
//
// A port has no stable key — two publishes can share a target, a protocol and a
// mode and differ only in the published port — so a change is reported as one
// entry gone and one arrived, which is what `--publish-rm` plus `--publish-add`
// actually did. Manual, so both halves can be asserted separately.
func TestLiveDriftDetectsAnOutOfBandPublishedPort(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-ports"
	repo := gitRepo(t, richChartFiles(t, release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("ports", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "ports"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		for i, p := range spec.EndpointSpec.Ports {
			if p.TargetPort == 8080 {
				spec.EndpointSpec.Ports[i].PublishedPort = 39118
			}
		}
	})

	if err := rec.Sync(ctx, "ports"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	d := driftOf(t, rec, "ports")
	gone := fieldOf(t, d, release+"_app", "ports[39117:8080/tcp/ingress]")
	if gone.Desired != "set" || gone.Live != "absent" {
		t.Errorf("the declared publish = %+v, want it reported missing from the swarm", gone)
	}
	arrived := fieldOf(t, d, release+"_app", "ports[39118:8080/tcp/ingress]")
	if arrived.Desired != "absent" || arrived.Live != "set" {
		t.Errorf("the hand-made publish = %+v, want it reported as unexpected", arrived)
	}

	// The dynamic publish beside it must not be caught up in this. The daemon
	// assigned it a host port, and if that assignment reached the spec it would
	// report here as well — on a port nobody touched.
	for _, svc := range d.Services {
		if svc.Name != release+"_app" {
			t.Errorf("service %q also drifted, want only the one that was changed", svc.Name)
			continue
		}
		for _, f := range svc.Fields {
			if f.Field != gone.Field && f.Field != arrived.Field {
				t.Errorf("also reported %+v, want only the port that was moved", f)
			}
		}
	}

	if err := rec.SyncNow(ctx, "ports"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)
	if d := driftOf(t, rec, "ports"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// A network detached by hand — the field that cannot be compared without naming
// what the daemon stored.
//
// The manifest names a network and the spec comes back carrying an id, so this
// is the test that says the resolution step happens at all: without it the
// desired and live sides are never equal and an untouched stack reports a
// detachment for ever, which the clean test above would catch — and with a
// resolution that silently declined, nothing here would be reported.
func TestLiveDriftDetectsAndRestoresADetachedNetwork(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-network"
	repo := gitRepo(t, richChartFiles(t, release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("network", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "network"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	updateOutOfBand(t, cli, release+"_sidecar", func(spec *swarm.ServiceSpec) {
		spec.TaskTemplate.Networks = nil
	})
	waitForRunning(t, cli, release, 2)

	if err := rec.Sync(ctx, "network"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	got := fieldOf(t, driftOf(t, rec, "network"), release+"_sidecar", "networks")
	if got.Desired != release+"_internal" {
		t.Errorf("desired networks = %q, want the scoped name the chart declares, not an id", got.Desired)
	}
	if got.Live != "" {
		t.Errorf("live networks = %q, want nothing left attached", got.Live)
	}

	if err := rec.SyncNow(ctx, "network"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	svc := serviceOf(t, cli, release+"_sidecar")
	if n := len(svc.Spec.TaskTemplate.Networks); n != 1 {
		t.Errorf("attachments after converging = %d, want the overlay back", n)
	}
	if d := driftOf(t, rec, "network"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// The service discovery mode, moved by hand on the service that declares none.
//
// Correcting it is the half no unit test can reach. The manifest said nothing
// about the mode, so the spec this controller writes back carries an *empty* one,
// and whether that puts a dnsrr service back on vip is the daemon's answer rather
// than this package's — the comparison only normalises what it reads.
//
// The sidecar rather than the app, because Swarm refuses dnsrr on a service with
// ingress publishing: the app could not be moved this way at all.
func TestLiveDriftDetectsAndRestoresTheEndpointMode(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-endpoint"
	repo := gitRepo(t, richChartFiles(t, release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("endpoint", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "endpoint"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	updateOutOfBand(t, cli, release+"_sidecar", func(spec *swarm.ServiceSpec) {
		if spec.EndpointSpec == nil {
			spec.EndpointSpec = &swarm.EndpointSpec{}
		}
		spec.EndpointSpec.Mode = swarm.ResolutionModeDNSRR
	})

	if err := rec.Sync(ctx, "endpoint"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	got := fieldOf(t, driftOf(t, rec, "endpoint"), release+"_sidecar", "endpointMode")
	if got.Desired != "vip" || got.Live != "dnsrr" {
		t.Errorf("endpointMode drift = %+v, want vip against dnsrr", got)
	}

	if err := rec.SyncNow(ctx, "endpoint"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	// Read the daemon, not the report: what was written back names no mode, so
	// this asserts the daemon's reading of an empty one.
	svc := serviceOf(t, cli, release+"_sidecar")
	if svc.Spec.EndpointSpec == nil || svc.Spec.EndpointSpec.Mode != swarm.ResolutionModeVIP {
		t.Errorf("endpoint mode after converging = %+v, want vip", svc.Spec.EndpointSpec)
	}
	if d := driftOf(t, rec, "endpoint"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// An attachment to a network the stack does not own — the case the unscoped
// listing exists for.
//
// LiveNetworkNames reads every network on the swarm rather than those carrying
// this stack's namespace label, precisely so an id like this one can be named. A
// scoped listing could not name it, and an attachment that cannot be named is not
// reported at all: the drift would be silently invisible rather than wrong. So
// this is the test that says that choice was necessary and that it works.
func TestLiveDriftReportsAnAttachmentOutsideTheStack(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-extra"
	const shared = "e2e-live-extra-shared"
	repo := gitRepo(t, richChartFiles(t, release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	ctx := context.Background()
	if _, err := cli.NetworkCreate(ctx, shared, network.CreateOptions{Driver: "overlay"}); err != nil {
		t.Fatalf("creating a network outside the stack: %v", err)
	}
	t.Cleanup(func() { removeNetworkWhenReleased(t, cli, shared) })

	rec := reconciler(t, liveDriftApp("extra", repo, false))
	if err := rec.SyncNow(ctx, "extra"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	// Attached by name, which the daemon resolves to the id it stores — the whole
	// reason the comparison has to resolve it back.
	updateOutOfBand(t, cli, release+"_sidecar", func(spec *swarm.ServiceSpec) {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks,
			swarm.NetworkAttachmentConfig{Target: shared})
	})
	waitForRunning(t, cli, release, 2)

	if err := rec.Sync(ctx, "extra"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	got := fieldOf(t, driftOf(t, rec, "extra"), release+"_sidecar", "networks")
	if got.Desired != release+"_internal" {
		t.Errorf("desired networks = %q, want only what the chart declares", got.Desired)
	}
	if want := []string{release + "_internal", shared}; !namesExactly(got.Live, want) {
		t.Errorf("live networks = %q, want %v — named, not an id", got.Live, want)
	}

	// And it goes on a converge, because the spec a redeploy writes lists the
	// attachments the manifest declares and no others.
	if err := rec.SyncNow(ctx, "extra"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	svc := serviceOf(t, cli, release+"_sidecar")
	if n := len(svc.Spec.TaskTemplate.Networks); n != 1 {
		t.Errorf("attachments after converging = %d, want the chart's one", n)
	}
	if d := driftOf(t, rec, "extra"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// assertNotReported fails if any service reports this field. It is how a test
// says "and nothing else moved", which for a comparison whose whole risk is
// false positives is half of what each case is for.
func assertNotReported(t *testing.T, d *application.ReleaseDrift, field string) {
	t.Helper()
	for _, svc := range d.Services {
		for _, f := range svc.Fields {
			if f.Field == field {
				t.Errorf("%s reported %+v, want it untouched and unreported", svc.Name, f)
			}
		}
	}
}

// namesExactly reports whether a rendered network list names these and no others.
// Order-insensitive deliberately: the rendering sorts, but asserting on that
// ordering would make the test about ASCII rather than about what is attached.
func namesExactly(rendered string, want []string) bool {
	got := strings.Split(rendered, ", ")
	slices.Sort(got)
	w := slices.Clone(want)
	slices.Sort(w)
	return slices.Equal(got, w)
}

// A mount removed by hand.
//
// The target is a real key — a container cannot mount two things at one path — so
// this reports as one line naming what should be mounted there and what is. The
// named volume rather than the bind, because it is the one whose source the
// manifest scopes, so the assertion also says the scoping survives the round trip.
func TestLiveDriftDetectsAnOutOfBandMountRemoval(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-mount"
	repo := gitRepo(t, richChartFiles(t, release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("mount", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "mount"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		kept := spec.TaskTemplate.ContainerSpec.Mounts[:0]
		for _, m := range spec.TaskTemplate.ContainerSpec.Mounts {
			if m.Target != "/data" {
				kept = append(kept, m)
			}
		}
		spec.TaskTemplate.ContainerSpec.Mounts = kept
	})
	waitForRunning(t, cli, release, 2)

	if err := rec.Sync(ctx, "mount"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	got := fieldOf(t, driftOf(t, rec, "mount"), release+"_app", "mounts[/data]")
	if got.Desired != "volume:"+release+"_data" {
		t.Errorf("desired mount = %q, want the scoped volume the chart declares", got.Desired)
	}
	if got.Live != "(absent)" {
		t.Errorf("live mount = %q, want it reported as gone", got.Live)
	}

	// The read-only bind beside it must not report. It is the mount whose
	// BindOptions the daemon drops on the way in, so a comparison that looked
	// inside a mount would find a difference here on every tick.
	assertNotReported(t, driftOf(t, rec, "mount"), "mounts[/etc/host-name]")

	if err := rec.SyncNow(ctx, "mount"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)
	if d := driftOf(t, rec, "mount"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// A mounted secret removed by hand.
//
// Both sides carry an id the daemon resolved, and neither is compared: what is
// compared is the name and where it lands, which is the pair `--secret-rm`
// changes. The config beside it is the control — untouched, and so unreported.
func TestLiveDriftDetectsAnOutOfBandSecretRemoval(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-secret"
	repo := gitRepo(t, richChartFiles(t, release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("secret", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "secret"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Secrets = nil
	})
	waitForRunning(t, cli, release, 2)

	if err := rec.Sync(ctx, "secret"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	d := driftOf(t, rec, "secret")
	got := fieldOf(t, d, release+"_app", "secrets["+release+"_token]")
	// The reference's file target is the source name when the manifest gives no
	// explicit one, which is what lands under /run/secrets.
	if got.Desired != "token" || got.Live != "(absent)" {
		t.Errorf("secret drift = %+v, want the mount target against nothing", got)
	}
	assertNotReported(t, d, "configs["+release+"_site]")

	if err := rec.SyncNow(ctx, "secret"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	svc := serviceOf(t, cli, release+"_app")
	if n := len(svc.Spec.TaskTemplate.ContainerSpec.Secrets); n != 1 {
		t.Errorf("secret references after converging = %d, want the chart's one back", n)
	}
	if d := driftOf(t, rec, "secret"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// An update policy edited by hand, on a service whose manifest declares only part
// of one.
//
// That is what makes it worth deploying. The chart names a parallelism and
// nothing else, so the running struct also carries a failure action and an order
// the manifest never mentioned — and the assertion that only the parallelism is
// reported is the assertion that those two are normalised rather than compared
// raw. Without it the release would report drift on every tick for ever.
func TestLiveDriftDetectsAnOutOfBandUpdateConfig(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-update"
	repo := gitRepo(t, richChartFiles(t, release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("update", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "update"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.UpdateConfig.Parallelism = 4
	})

	if err := rec.Sync(ctx, "update"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	d := driftOf(t, rec, "update")
	got := fieldOf(t, d, release+"_app", "updateConfig.parallelism")
	if got.Desired != "1" || got.Live != "4" {
		t.Errorf("parallelism drift = %+v, want 1 against 4", got)
	}
	// The two the manifest never stated: the daemon names them, and naming them
	// is not a change anybody made.
	assertNotReported(t, d, "updateConfig.failureAction")
	assertNotReported(t, d, "updateConfig.order")
	// Nor the restart policy or the placement beside it, both also partial.
	assertNotReported(t, d, "restartPolicy.maxAttempts")
	assertNotReported(t, d, "restartPolicy.condition")
	assertNotReported(t, d, "placement.preferences")

	if err := rec.SyncNow(ctx, "update"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)
	if d := driftOf(t, rec, "update"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// A healthcheck edited by hand.
//
// The container spec is the one place in this group the daemon stores verbatim,
// so what this really proves is the other half: that a chart declaring a
// healthcheck at all still reports nothing when nobody has touched it, which the
// clean test covers, and that a changed interval is legible here.
func TestLiveDriftDetectsAnOutOfBandHealthcheck(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-health"
	repo := gitRepo(t, richChartFiles(t, release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("health", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "health"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	updateOutOfBand(t, cli, release+"_sidecar", func(spec *swarm.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Healthcheck.Interval = time.Minute
	})
	waitForRunning(t, cli, release, 2)

	if err := rec.Sync(ctx, "health"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	d := driftOf(t, rec, "health")
	got := fieldOf(t, d, release+"_sidecar", "healthcheck.interval")
	if got.Desired != "5s" || got.Live != "1m0s" {
		t.Errorf("healthcheck drift = %+v, want 5s against 1m0s", got)
	}
	// The rest of the check is untouched and must stay silent, which is what says
	// the comparison is per field rather than on the struct as a whole.
	assertNotReported(t, d, "healthcheck.test")
	assertNotReported(t, d, "healthcheck.retries")
	assertNotReported(t, d, "healthcheck")

	if err := rec.SyncNow(ctx, "health"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)
	if d := driftOf(t, rec, "health"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// A service deleted out of band. The health axis only notices when a release has
// no services left at all, so without this a two-service stack missing one of
// them reports healthy.
//
// Manual, so that the two halves can be asserted separately. An automated
// application detects and corrects within the same reconcile — and then the
// confirming re-plan re-compares and reports none, which is the feature working
// and leaves nothing to assert about what was found.
func TestLiveDriftDetectsAndRestoresADeletedService(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-missing"
	repo := gitRepo(t, richChartFiles(t, release, 1))
	t.Cleanup(func() { removeStack(t, release); removeVolumes(t, cli, release) })

	rec := reconciler(t, liveDriftApp("missing", repo, false))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "missing"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)

	sidecar := serviceOf(t, cli, release+"_sidecar")
	if err := cli.ServiceRemove(ctx, sidecar.ID); err != nil {
		t.Fatalf("removing the sidecar out of band: %v", err)
	}

	if err := rec.Sync(ctx, "missing"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	// Reported as missing rather than as a field difference, and only the
	// service that went — the one still running must not be swept up in it.
	d := driftOf(t, rec, "missing")
	if d == nil || d.State != application.DriftStateDetected || len(d.Services) != 1 {
		t.Fatalf("drift = %+v, want exactly the departed service", d)
	}
	if d.Services[0].Name != release+"_sidecar" || d.Services[0].Reason != application.DriftMissing {
		t.Errorf("got %+v, want %s_sidecar missing", d.Services[0], release)
	}

	// And converging recreates it: a redeploy creates what is not there, which
	// is what makes a missing service convergeable where an unexpected one is not.
	if err := rec.SyncNow(ctx, "missing"); err != nil {
		t.Fatalf("SyncNow after the deletion = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 2)
	if d := driftOf(t, rec, "missing"); d == nil || d.State != application.DriftStateNone {
		t.Errorf("drift after converging = %+v, want none", d)
	}
}

// The default mode must be untouched by any of this: it does not look at the
// swarm, so it cannot report or correct an out-of-band change, and it must not
// grow the axis either.
func TestManifestModeIgnoresAnOutOfBandChange(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-live-manifest"
	repo := gitRepo(t, chartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, releaseApp("manifest", repo, true))
	ctx := context.Background()

	if err := rec.SyncNow(ctx, "manifest"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	two := uint64(2)
	updateOutOfBand(t, cli, release+"_app", func(spec *swarm.ServiceSpec) {
		spec.Mode.Replicated.Replicas = &two
	})
	waitForRunning(t, cli, release, 2)

	if err := rec.Sync(ctx, "manifest"); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	view, _ := rec.View("manifest")
	if view.Status.Drift != nil {
		t.Errorf("drift = %+v, want the axis absent in manifest mode", view.Status.Drift)
	}
	if view.Status.Sync.State != application.SyncSynced {
		t.Errorf("sync = %q, want synced — manifest mode compares against the last apply", view.Status.Sync.State)
	}
	if got := runningReplicas(t, cli, release); got != 2 {
		t.Errorf("running replicas = %d, want the hand-scaled 2 left alone", got)
	}
}
