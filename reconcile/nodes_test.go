// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// rosterBackend is a backend that can describe the swarm — the capability the
// OSS applier implements and a Phase 3 remote one may not.
type rosterBackend struct {
	charts.Backend
	nodes []application.SwarmNode
	err   error
}

func (r rosterBackend) SwarmNodeRoster(context.Context) ([]application.SwarmNode, error) {
	return r.nodes, r.err
}

func TestNodesServesTheRosterTheBackendReports(t *testing.T) {
	want := []application.SwarmNode{
		{ID: "1", Hostname: "mgr-1", Role: "manager", Status: "ready", Leader: true, TasksRunning: 3, TasksDesired: 4},
	}
	r := newTestWith(t, nil, &fakeEngine{}, nil, fakeRegistry{backend: rosterBackend{nodes: want}})

	got, err := r.Nodes(t.Context())
	if err != nil {
		t.Fatalf("Nodes = %v, want nil", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0] != want[0] {
		t.Errorf("nodes = %+v, want %+v", got.Nodes, want)
	}

	// swarms.Target names the local swarm with an empty string, so that is what
	// the roster is qualified with. Anything else — a cluster ID, a hostname —
	// would put something that is not a destination in a field that means one
	// everywhere else, and a multi-swarm registry has no way to correct it.
	if got.Swarm != "" {
		t.Errorf("swarm = %q, want the empty local target", got.Swarm)
	}
}

// Implementing the method is not the same as being able to answer: which
// backend serves a destination is settled per request. A backend with no roster
// is reported as a destination that cannot answer, which the API renders as the
// same 501 a reconciler with no Nodes method at all gives — not as a failed
// read, which would tell an operator to retry something nothing will answer.
func TestNodesReportsABackendWithoutARosterAsUnsupported(t *testing.T) {
	r := newTestWith(t, nil, &fakeEngine{}, nil, fakeRegistry{})

	_, err := r.Nodes(t.Context())
	if !errors.Is(err, application.ErrUnsupported) {
		t.Fatalf("Nodes = %v, want application.ErrUnsupported", err)
	}
}

// A read that failed must not arrive as the sentinel, or a daemon that blinked
// is reported for ever as a build that cannot list nodes.
func TestNodesKeepsAFailedReadDistinctFromAnUnsupportedOne(t *testing.T) {
	boom := errors.New("daemon unreachable")
	r := newTestWith(t, nil, &fakeEngine{}, nil, fakeRegistry{backend: rosterBackend{err: boom}})

	_, err := r.Nodes(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("Nodes = %v, want the daemon's error", err)
	}
	if errors.Is(err, application.ErrUnsupported) {
		t.Error("a failed read was reported as an unsupported destination")
	}
}

func TestNodesSurfacesAnUnresolvableDestination(t *testing.T) {
	r := newTestWith(t, nil, &fakeEngine{}, nil, fakeRegistry{err: errors.New("no such swarm")})

	if _, err := r.Nodes(t.Context()); err == nil {
		t.Fatal("an unresolvable local swarm produced a roster and no error")
	}
}
