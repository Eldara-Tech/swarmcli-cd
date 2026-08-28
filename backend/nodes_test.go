// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"
)

// node builds a swarm.Node the daemon's shape, so the mapping is exercised
// against the fields a real roster arrives in rather than against a flat one.
func node(id, hostname string, role swarm.NodeRole, state swarm.NodeState, leader bool) swarm.Node {
	n := swarm.Node{
		ID: id,
		Spec: swarm.NodeSpec{
			Role:         role,
			Availability: swarm.NodeAvailabilityActive,
		},
		Description: swarm.NodeDescription{
			Hostname: hostname,
			Engine:   swarm.EngineDescription{EngineVersion: "28.5.2"},
		},
		Status: swarm.NodeStatus{State: state, Addr: "10.0.0." + id},
	}
	if role == swarm.NodeRoleManager {
		n.ManagerStatus = &swarm.ManagerStatus{Leader: leader}
	}
	return n
}

func task(nodeID string, desired, actual swarm.TaskState) swarm.Task {
	return swarm.Task{NodeID: nodeID, DesiredState: desired, Status: swarm.TaskStatus{State: actual}}
}

func TestSwarmNodeRosterReportsWhatTheDaemonReported(t *testing.T) {
	api := &fakeAPI{nodes: []swarm.Node{
		node("2", "worker-a", swarm.NodeRoleWorker, swarm.NodeStateReady, false),
		node("1", "mgr-1", swarm.NodeRoleManager, swarm.NodeStateReady, true),
	}}
	got, err := testBackend(t, api, nil).SwarmNodeRoster(t.Context())
	if err != nil {
		t.Fatalf("SwarmNodeRoster: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("roster has %d nodes, want 2", len(got))
	}

	// Sorted by hostname, not returned in the daemon's order: a matrix whose
	// rows move between refreshes is unreadable, and the daemon promises no
	// order at all.
	if got[0].Hostname != "mgr-1" || got[1].Hostname != "worker-a" {
		t.Fatalf("roster order = %s, %s; want mgr-1, worker-a", got[0].Hostname, got[1].Hostname)
	}

	mgr := got[0]
	if mgr.ID != "1" || mgr.Role != "manager" || mgr.Availability != "active" ||
		mgr.Status != "ready" || mgr.EngineVersion != "28.5.2" || mgr.Addr != "10.0.0.1" {
		t.Errorf("manager mapped to %+v", mgr)
	}
	if !mgr.Leader {
		t.Error("the leader is not reported as one")
	}
	// ManagerStatus is nil on every worker, so a leader test reading only the
	// flag would panic rather than answer.
	if got[1].Leader {
		t.Error("a worker is reported as the leader")
	}
}

// The gauge the console draws is running over desired for one node, so the
// counts have to be the tasks scheduled *there*. Counting every task record on
// a node instead would report a number that only ever grows: the swarm keeps
// stopped tasks, which is how `docker service ps` shows a crash history.
func TestSwarmNodeRosterCountsTasksPerNodeAndIgnoresTheDead(t *testing.T) {
	api := &fakeAPI{
		nodes: []swarm.Node{
			node("1", "mgr-1", swarm.NodeRoleManager, swarm.NodeStateReady, true),
			node("2", "worker-a", swarm.NodeRoleWorker, swarm.NodeStateReady, false),
		},
		tasks: []swarm.Task{
			task("1", swarm.TaskStateRunning, swarm.TaskStateRunning),
			task("1", swarm.TaskStateRunning, swarm.TaskStateStarting),
			// Six historical failures on the same node. Every one is a task
			// record the daemon still returns, and none is work anybody wants.
			task("1", swarm.TaskStateShutdown, swarm.TaskStateFailed),
			task("1", swarm.TaskStateShutdown, swarm.TaskStateFailed),
			task("1", swarm.TaskStateShutdown, swarm.TaskStateFailed),
			task("1", swarm.TaskStateShutdown, swarm.TaskStateFailed),
			task("1", swarm.TaskStateShutdown, swarm.TaskStateFailed),
			task("1", swarm.TaskStateShutdown, swarm.TaskStateFailed),
			task("2", swarm.TaskStateRunning, swarm.TaskStateRunning),
			// Scheduled to no node yet. It belongs to no row.
			task("", swarm.TaskStateRunning, swarm.TaskStatePending),
		},
	}
	got, err := testBackend(t, api, nil).SwarmNodeRoster(t.Context())
	if err != nil {
		t.Fatalf("SwarmNodeRoster: %v", err)
	}

	byHost := map[string][2]int{}
	for _, n := range got {
		byHost[n.Hostname] = [2]int{n.TasksRunning, n.TasksDesired}
	}
	if want := [2]int{1, 2}; byHost["mgr-1"] != want {
		t.Errorf("mgr-1 tasks = %v (running, desired), want %v", byHost["mgr-1"], want)
	}
	if want := [2]int{1, 1}; byHost["worker-a"] != want {
		t.Errorf("worker-a tasks = %v (running, desired), want %v", byHost["worker-a"], want)
	}
}

// docker.SnapshotWith answers a locked swarm with an empty snapshot and a nil
// error on purpose. Passed through, that would serve a roster of no nodes, and
// the console would draw a cluster that is merely locked as one that is empty.
func TestSwarmNodeRosterRefusesALockedSwarmRatherThanReportingItEmpty(t *testing.T) {
	api := &fakeAPI{nodeErr: errors.New("Error response from daemon: swarm is encrypted and needs to be unlocked")}
	got, err := testBackend(t, api, nil).SwarmNodeRoster(t.Context())
	if err == nil {
		t.Fatalf("a locked swarm reported a roster of %d nodes, want an error", len(got))
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("error = %v, want it to say the swarm is locked", err)
	}
}

// A swarm with no nodes is not a thing, so an empty list is a read that failed
// rather than a cluster with nothing in it. The two are different states in the
// console and must stay distinguishable.
func TestSwarmNodeRosterRefusesAnEmptyRoster(t *testing.T) {
	got, err := testBackend(t, &fakeAPI{}, nil).SwarmNodeRoster(t.Context())
	if err == nil {
		t.Fatalf("an empty roster came back as %d nodes and no error", len(got))
	}
}

func TestSwarmNodeRosterSurfacesAFailedRead(t *testing.T) {
	api := &fakeAPI{nodeErr: errors.New("daemon unreachable")}
	if _, err := testBackend(t, api, nil).SwarmNodeRoster(t.Context()); err == nil {
		t.Fatal("a daemon failure produced a roster and no error")
	}
}

// NodeStatus.State is omitempty, so a node the manager has not heard from
// arrives with an empty string. Rendered raw that is a blank cell rather than a
// state, and the daemon has its own word for it.
func TestSwarmNodeRosterNamesTheUnknownState(t *testing.T) {
	api := &fakeAPI{nodes: []swarm.Node{node("1", "mgr-1", swarm.NodeRoleManager, "", true)}}
	got, err := testBackend(t, api, nil).SwarmNodeRoster(t.Context())
	if err != nil {
		t.Fatalf("SwarmNodeRoster: %v", err)
	}
	if got[0].Status != "unknown" {
		t.Errorf("status = %q, want %q", got[0].Status, "unknown")
	}
}
