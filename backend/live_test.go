// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/swarm"
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
