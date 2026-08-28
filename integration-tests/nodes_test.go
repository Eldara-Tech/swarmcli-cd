// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"log/slog"
	"testing"

	cdbackend "github.com/Eldara-Tech/swarmcli-cd/backend"
)

// The node roster is mapped out of the daemon's own node and task records, and
// a fake proves only that the mapping matches what the fake was written to
// return. What it cannot prove is that the fields exist where the mapping reads
// them: NodeStatus.Addr and Description.Engine.EngineVersion are populated by
// the daemon, ManagerStatus is nil on a worker and non-nil here, and a swarm of
// one is a swarm whose only node is the leader. So this asserts against a real
// manager, which is the only place those are facts rather than fixtures.
func TestSwarmNodeRosterAgainstARealSwarm(t *testing.T) {
	cli := dockerClient(t)
	b := cdbackend.New(cli, cdbackend.Options{Log: slog.New(slog.DiscardHandler)})

	nodes, err := b.SwarmNodeRoster(context.Background())
	if err != nil {
		t.Fatalf("SwarmNodeRoster: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("a reachable swarm manager reported no nodes")
	}

	var leaders int
	for _, n := range nodes {
		if n.ID == "" {
			t.Errorf("node %q has no id", n.Hostname)
		}
		if n.Hostname == "" {
			t.Errorf("node %s has no hostname", n.ID)
		}
		if n.Role != "manager" && n.Role != "worker" {
			t.Errorf("node %s has role %q, want manager or worker", n.Hostname, n.Role)
		}
		// Not merely non-empty: the empty state is the one the mapping renames,
		// so an assertion that only checked for "" would pass on a mapping that
		// had stopped reading the daemon's field at all.
		if n.Status == "" {
			t.Errorf("node %s has no status", n.Hostname)
		}
		if n.EngineVersion == "" {
			t.Errorf("node %s reports no engine version", n.Hostname)
		}
		if n.Leader {
			leaders++
		}
		// A node cannot be running more tasks than the orchestrator wants on
		// it. If it can, the two counts are being taken from different sets —
		// which is what counting task *records* rather than wanted work does.
		if n.TasksRunning > n.TasksDesired {
			t.Errorf("node %s runs %d tasks of %d desired", n.Hostname, n.TasksRunning, n.TasksDesired)
		}
	}

	// The daemon this suite skips without is a manager with control available,
	// so exactly one node in its roster is the leader. Zero would mean the
	// ManagerStatus pointer is never being read; more than one would mean the
	// flag is being read off the wrong node.
	if leaders != 1 {
		t.Errorf("the roster names %d leaders, want exactly 1", leaders)
	}
}
