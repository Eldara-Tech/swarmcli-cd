// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/charts"
)

const liveManifest = `version: "3.9"
services:
  web:
    image: nginx:1.2
    deploy:
      replicas: 2
`

// The desired half has to be the same conversion an apply would do, or drift
// compares against something a sync would never write.
func TestDesiredServicesConvertsTheManifest(t *testing.T) {
	stack, err := testBackend(t, &fakeAPI{}, nil).DesiredServices(context.Background(), liveManifest, "s")
	if err != nil {
		t.Fatalf("DesiredServices = %v, want nil", err)
	}
	if len(stack.Services) != 1 {
		t.Fatalf("got %d services, want 1", len(stack.Services))
	}

	svc := stack.Services[0]
	if svc.Name != "web" || svc.Spec.Name != "s_web" {
		t.Errorf("service = %q/%q, want the unscoped name and the scoped spec name", svc.Name, svc.Spec.Name)
	}
	// The stack image label is what makes an image comparison tag-to-tag rather
	// than tag-to-digest, so its absence would break drift silently.
	if got := svc.Spec.Labels[convert.LabelImage]; got != "nginx:1.2" {
		t.Errorf("stack image label = %q, want the tag the manifest asked for", got)
	}
	if r := svc.Spec.Mode.Replicated; r == nil || r.Replicas == nil || *r.Replicas != 2 {
		t.Errorf("replicas = %+v, want 2", r)
	}
}

func TestDesiredServicesReportsAnUnusableManifest(t *testing.T) {
	_, err := testBackend(t, &fakeAPI{}, nil).DesiredServices(context.Background(), "services: [", "s")
	if err == nil {
		t.Fatal("DesiredServices = nil, want an error for a manifest that does not parse")
	}
}

// A stack is a namespace label and nothing more, so an unscoped read would
// report every service on the swarm as belonging to this release.
func TestLiveServicesAreScopedToTheStack(t *testing.T) {
	api := &fakeAPI{existing: []swarm.Service{
		deployed("s_web", "nginx:1.2", "nginx:1.2@sha256:aaaa", 1),
		deployed("s_db", "postgres:16", "postgres:16@sha256:bbbb", 1),
	}}

	live, err := testBackend(t, api, nil).LiveServices(context.Background(), "s")
	if err != nil {
		t.Fatalf("LiveServices = %v, want nil", err)
	}

	if len(api.labelFilters) != 1 || api.labelFilters[0] != convert.LabelNamespace+"=s" {
		t.Errorf("listed with %v, want a single filter on the stack namespace label", api.labelFilters)
	}
	// Keyed by the scoped name, which is what a live spec carries and what the
	// desired side's Spec.Name is, so the two sides can be matched at all.
	if _, ok := live["s_web"]; !ok || len(live) != 2 {
		t.Errorf("got %d services keyed %v, want both keyed by scoped name", len(live), keysOf(live))
	}
}

func TestLiveServicesReportsAListFailure(t *testing.T) {
	api := &fakeAPI{listErr: errors.New("swarm unreachable")}
	_, err := testBackend(t, api, nil).LiveServices(context.Background(), "s")
	if err == nil {
		t.Fatal("LiveServices = nil, want the list error surfaced")
	}
	if !strings.Contains(err.Error(), "listing the stack's services") {
		t.Errorf("error %q does not say what failed", err)
	}
}

func keysOf(m map[string]swarm.Service) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The one read here that must NOT be scoped, and the assertion is the filter
// itself: a service can be attached to an external network or a predefined one,
// neither of which carries the stack's namespace label, so a scoped listing would
// leave exactly those attachments unnameable — and an attachment that cannot be
// named is the one worth reporting.
func TestLiveNetworkNamesAreReadAcrossTheWholeSwarm(t *testing.T) {
	api := &fakeAPI{networks: []network.Summary{
		stackNetwork("n1", "s_front", "s"),
		stackNetwork("n2", "other_front", "other"),
		{ID: "n3", Name: "shared"}, // external: no namespace label at all
	}}

	got, err := testBackend(t, api, nil).LiveNetworkNames(context.Background())
	if err != nil {
		t.Fatalf("LiveNetworkNames = %v, want nil", err)
	}

	want := map[string]string{"n1": "s_front", "n2": "other_front", "n3": "shared"}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v — id to name, the opposite direction from LiveNetworks", got, want)
	}
	for _, f := range api.labelFilters {
		if f != "" {
			t.Errorf("listed with filter %q, want the whole swarm's networks", f)
		}
	}
}

func TestLiveNetworkNamesReportsAListFailure(t *testing.T) {
	api := &fakeAPI{networkErr: errors.New("swarm unreachable")}
	_, err := testBackend(t, api, nil).LiveNetworkNames(context.Background())
	if err == nil || !strings.Contains(err.Error(), "listing the swarm's networks") {
		t.Errorf("LiveNetworkNames = %v, want the list error surfaced and named", err)
	}
}

// ------------------------------------------- the other three kinds (#80)

// Each lister is a namespace filter and nothing else, so a filter that never
// arrived is the whole of what could go wrong. Asserted twice: the fake applies
// the filter, so an unfiltered read returns the sibling stack's resource too,
// and labelFilters catches a read scoped by the wrong label.
func TestLiveResourceListersAreScopedToTheStack(t *testing.T) {
	api := &fakeAPI{
		networks: []network.Summary{
			stackNetwork("n1", "s_front", "s"),
			stackNetwork("n2", "other_front", "other"),
		},
		configs: []swarm.Config{
			{ID: "c1", Spec: swarm.ConfigSpec{Annotations: stackScoped("s_site", "s")}},
			{ID: "c2", Spec: swarm.ConfigSpec{Annotations: stackScoped("other_site", "other")}},
		},
		secrets: []swarm.Secret{
			{ID: "k1", Spec: swarm.SecretSpec{Annotations: stackScoped("s_key", "s")}},
			{ID: "k2", Spec: swarm.SecretSpec{Annotations: stackScoped("other_key", "other")}},
		},
	}
	b := testBackend(t, api, nil)
	ctx := context.Background()

	nets, err := b.LiveNetworks(ctx, "s")
	if err != nil {
		t.Fatalf("LiveNetworks = %v, want nil", err)
	}
	cfgs, err := b.LiveConfigs(ctx, "s")
	if err != nil {
		t.Fatalf("LiveConfigs = %v, want nil", err)
	}
	secs, err := b.LiveSecrets(ctx, "s")
	if err != nil {
		t.Fatalf("LiveSecrets = %v, want nil", err)
	}

	for what, got := range map[string]map[string]string{
		"networks": nets, "configs": cfgs, "secrets": secs,
	} {
		if len(got) != 1 {
			t.Errorf("%s = %v, want only this stack's", what, got)
		}
	}
	// Scoped name to id: the name is the only key a manifest and a live resource
	// share, and the id is what the removal takes.
	if nets["s_front"] != "n1" || cfgs["s_site"] != "c1" || secs["s_key"] != "k1" {
		t.Errorf("nets=%v configs=%v secrets=%v, want scoped name to id", nets, cfgs, secs)
	}
	for _, f := range api.labelFilters {
		if f != convert.LabelNamespace+"=s" {
			t.Errorf("a list was scoped by %q, want the stack namespace label", f)
		}
	}
}

// A release record is the evidence the sweep proves ownership with. Sweeping one
// up would delete the history that says what this controller installed — the
// same guard RemoveStack applies, one scope down.
func TestLiveConfigsNeverIncludesAReleaseRecord(t *testing.T) {
	record := swarm.ConfigSpec{Annotations: swarm.Annotations{
		Name: "swarmcli.release.s.v1",
		Labels: map[string]string{
			convert.LabelNamespace: "s",
			charts.LabelType:       charts.TypeRelease,
		},
	}}
	api := &fakeAPI{configs: []swarm.Config{
		{ID: "rel", Spec: record},
		{ID: "c1", Spec: swarm.ConfigSpec{Annotations: stackScoped("s_site", "s")}},
	}}

	got, err := testBackend(t, api, nil).LiveConfigs(context.Background(), "s")
	if err != nil {
		t.Fatalf("LiveConfigs = %v, want nil", err)
	}
	if _, ok := got["swarmcli.release.s.v1"]; ok {
		t.Error("a release record is in range of the sweep; that would delete the ownership evidence")
	}
	if len(got) != 1 || got["s_site"] != "c1" {
		t.Errorf("configs = %v, want the stack's own config alone", got)
	}
}

func TestLiveResourceListersReportAFailure(t *testing.T) {
	b := testBackend(t, &fakeAPI{networkErr: errors.New("swarm unreachable")}, nil)
	if _, err := b.LiveNetworks(context.Background(), "s"); err == nil ||
		!strings.Contains(err.Error(), "listing the stack's networks") {
		t.Errorf("LiveNetworks = %v, want the list error surfaced and named", err)
	}
}

// By id, not by name: the caller has already read the resource it means, and
// resolving a name again here would open a window in which it pointed at
// something else.
func TestResourceRemovalsGoByID(t *testing.T) {
	api := &fakeAPI{
		networks: []network.Summary{stackNetwork("n1", "s_front", "s")},
		configs:  []swarm.Config{{ID: "c1", Spec: swarm.ConfigSpec{Annotations: stackScoped("s_site", "s")}}},
		secrets:  []swarm.Secret{{ID: "k1", Spec: swarm.SecretSpec{Annotations: stackScoped("s_key", "s")}}},
	}
	b := testBackend(t, api, nil)
	ctx := context.Background()

	if err := b.RemoveConfig(ctx, "c1"); err != nil {
		t.Fatalf("RemoveConfig = %v, want nil", err)
	}
	if err := b.RemoveSecret(ctx, "k1"); err != nil {
		t.Fatalf("RemoveSecret = %v, want nil", err)
	}
	if err := b.RemoveNetwork(ctx, "n1"); err != nil {
		t.Fatalf("RemoveNetwork = %v, want nil", err)
	}

	want := []string{"config:c1", "secret:k1", "network:n1"}
	if !slices.Equal(api.removed, want) {
		t.Errorf("removed %v, want %v — each by id", api.removed, want)
	}
}

// Already gone is the outcome the caller wanted. It is also routine for a
// network: Swarm garbage-collects an overlay once its last task leaves, which
// happens while the services that used it are still shutting down.
//
// Services included, and they are the reason this matters most: RemoveService
// is the whole of the applier's delete surface, and a sweep that raced another
// deletion has still got the outcome it wanted. Treating that as a failure would
// make every retry of a part-failed sweep fail forever.
func TestResourceRemovalsTolerateSomethingAlreadyGone(t *testing.T) {
	api := &fakeAPI{removeErr: map[string]error{
		"service:s1": errdefs.ErrNotFound,
		"network:n1": errdefs.ErrNotFound,
		"config:c1":  errdefs.ErrNotFound,
		"secret:k1":  errdefs.ErrNotFound,
	}}
	b := testBackend(t, api, nil)
	ctx := context.Background()

	if err := b.RemoveService(ctx, "s1"); err != nil {
		t.Errorf("RemoveService = %v, want nil for one already gone", err)
	}
	if err := b.RemoveNetwork(ctx, "n1"); err != nil {
		t.Errorf("RemoveNetwork = %v, want nil for one already gone", err)
	}
	if err := b.RemoveConfig(ctx, "c1"); err != nil {
		t.Errorf("RemoveConfig = %v, want nil for one already gone", err)
	}
	if err := b.RemoveSecret(ctx, "k1"); err != nil {
		t.Errorf("RemoveSecret = %v, want nil for one already gone", err)
	}
	// Each was still attempted, by the id it was given: tolerating a not-found
	// must not become skipping the call.
	want := []string{"service:s1", "network:n1", "config:c1", "secret:k1"}
	if !slices.Equal(api.removed, want) {
		t.Errorf("removed %v, want %v", api.removed, want)
	}
}

// A refusal is not a not-found. Swarm refuses to remove anything still in use,
// and that has to reach the caller so it can report it and try again.
func TestResourceRemovalSurfacesARefusal(t *testing.T) {
	api := &fakeAPI{removeErr: map[string]error{
		"config:c1":  errors.New("config c1 is in use by service s_web"),
		"service:s1": errors.New("rpc error: code = Unknown desc = update out of sequence"),
	}}
	b := testBackend(t, api, nil)

	err := b.RemoveConfig(context.Background(), "c1")
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("RemoveConfig = %v, want the daemon's refusal surfaced", err)
	}
	// The service half. A delete that failed for any reason other than "it is
	// already gone" leaves the service running, and the sweep has to hear that:
	// it is what stops prune recording the release as departed.
	err = b.RemoveService(context.Background(), "s1")
	if err == nil || !strings.Contains(err.Error(), "out of sequence") {
		t.Fatalf("RemoveService = %v, want the daemon's refusal surfaced", err)
	}
	if !strings.Contains(err.Error(), "s1") {
		t.Errorf("error %q does not name the service that would not go", err)
	}
}

// ------------------------------------------ a network removal that worked (#108)

// The other half of the reason stackRemains exists, on a path that had no
// equivalent of it.
//
// A swarm-scoped network removal is proxied through swarmkit, whose helper for
// a network is the one of its five siblings that does not classify "not found"
// (daemon/cluster/helpers.go:238); it arrives as a 500 and so as ErrInternal,
// which errdefs.IsNotFound answers false for. Reading the error alone therefore
// reports a deletion that succeeded as a failed prune — on the path whose whole
// purpose is honest reporting.
func TestRemoveNetworkAcceptsAnUnclassifiedErrorThatLeftNothingBehind(t *testing.T) {
	api := &fakeAPI{
		networks:   []network.Summary{stackNetwork("n1", "s_front", "s")},
		removeErr:  map[string]error{"network:n1": errors.New("network n1 not found")},
		removeGone: map[string]bool{"network:n1": true},
	}

	if err := testBackend(t, api, nil).RemoveNetwork(context.Background(), "n1"); err != nil {
		t.Fatalf("RemoveNetwork = %v, want nil — the network is gone, whatever the call said", err)
	}
}

// The other half: a refusal that left the network in place is a real failure and
// must not be explained away by the same re-check.
func TestRemoveNetworkStillFailsWhenTheNetworkSurvived(t *testing.T) {
	api := &fakeAPI{
		networks:  []network.Summary{stackNetwork("n1", "s_front", "s")},
		removeErr: map[string]error{"network:n1": errors.New("network n1 is in use by service other_web")},
	}

	err := testBackend(t, api, nil).RemoveNetwork(context.Background(), "n1")
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("RemoveNetwork = %v, want the daemon's refusal surfaced", err)
	}
}

// Neither error answers on its own: the removal may well have worked, and the
// read that would have said so is the thing that failed. Both are reported.
func TestRemoveNetworkReportsBothWhenTheRecheckAlsoFails(t *testing.T) {
	api := &fakeAPI{
		removeErr:  map[string]error{"network:n1": errors.New("network n1 not found")},
		networkErr: errors.New("swarm unreachable"),
	}

	err := testBackend(t, api, nil).RemoveNetwork(context.Background(), "n1")
	if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "swarm unreachable") {
		t.Fatalf("RemoveNetwork = %v, want both the removal error and the failed re-check", err)
	}
}

// ------------------------------------------- adoption is not ownership (#108)

// The gap Claim cannot close. A secret an operator seeded before any chart named
// it is relabelled into the stack's namespace by applySecrets, which is the only
// thing that makes it a sweep candidate — and a revision that merely declared
// the name then reads as proof of ownership. Recovery would need material this
// controller never had.
//
// The creation marker is the second half of the proof, and these two listers are
// where it is required, because this is the read that decides what a candidate
// even is.
func TestLiveConfigsAndSecretsIgnoreWhatWasAdoptedRatherThanCreated(t *testing.T) {
	api := &fakeAPI{
		configs: []swarm.Config{
			{ID: "c1", Spec: swarm.ConfigSpec{Annotations: stackScoped("s_ours", "s")}},
			{ID: "c2", Spec: swarm.ConfigSpec{Annotations: adopted("s_theirs", "s")}},
		},
		secrets: []swarm.Secret{
			{ID: "k1", Spec: swarm.SecretSpec{Annotations: stackScoped("s_ours", "s")}},
			{ID: "k2", Spec: swarm.SecretSpec{Annotations: adopted("s_theirs", "s")}},
		},
	}
	b := testBackend(t, api, nil)
	ctx := context.Background()

	cfgs, err := b.LiveConfigs(ctx, "s")
	if err != nil {
		t.Fatalf("LiveConfigs = %v, want nil", err)
	}
	secs, err := b.LiveSecrets(ctx, "s")
	if err != nil {
		t.Fatalf("LiveSecrets = %v, want nil", err)
	}

	for what, got := range map[string]map[string]string{"configs": cfgs, "secrets": secs} {
		if len(got) != 1 || got["s_ours"] == "" {
			t.Errorf("%s = %v, want only the one this controller created", what, got)
		}
	}
}

// The deliberate asymmetry, asserted so that "make all four kinds the same"
// cannot be applied here by eye. applyNetworks never relabels an existing
// network into a stack's namespace, so there is no adoption to catch — and
// requiring a marker would instead stop the sweep touching every network created
// before the label existed, silently, which is the one kind the sweep can act on
// at all now that a chart cannot own a config or secret (#99).
func TestLiveNetworksDoNotRequireTheCreationMarker(t *testing.T) {
	api := &fakeAPI{networks: []network.Summary{stackNetwork("n1", "s_front", "s")}}

	got, err := testBackend(t, api, nil).LiveNetworks(context.Background(), "s")
	if err != nil {
		t.Fatalf("LiveNetworks = %v, want nil", err)
	}
	if got["s_front"] != "n1" {
		t.Errorf("networks = %v, want the stack's network in range of the sweep", got)
	}
}
