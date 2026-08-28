// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/v2/docker"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// SwarmNodeRoster describes every node of the swarm this backend applies to.
//
// It reads the same whole-swarm snapshot ReadStacks does — nodes, services and
// tasks in one round trip — because the task counts are the point of the roster
// and fetching them separately would be two reads of a store that answers both
// at once.
//
// # Why an empty roster is an error
//
// docker.SnapshotWith answers a locked swarm with an empty snapshot and a nil
// error, on purpose: the TUI it was written for prompts the operator to unlock
// rather than reporting a dead daemon. Passed through here that would serve a
// roster of no nodes, and a swarm with no nodes is not a thing — the API would
// answer 200 and the console would draw "the controller reported no swarm
// nodes" for a cluster that is merely locked. So both the locked flag and an
// empty list become errors, which is what keeps "could not read the roster"
// distinguishable from "read it, and the cluster is empty".
func (b *Backend) SwarmNodeRoster(ctx context.Context) ([]application.SwarmNode, error) {
	snap, err := docker.SnapshotWith(ctx, b.api)
	if err != nil {
		return nil, fmt.Errorf("reading the swarm snapshot: %w", err)
	}
	if snap.Locked {
		return nil, errors.New("the swarm is locked, so its node roster cannot be read")
	}
	if len(snap.Nodes) == 0 {
		return nil, errors.New("the swarm reported no nodes, which no reachable swarm does")
	}

	running, desired := taskCounts(snap.Tasks)

	out := make([]application.SwarmNode, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		out = append(out, application.SwarmNode{
			ID:           n.ID,
			Hostname:     n.Description.Hostname,
			Role:         string(n.Spec.Role),
			Availability: string(n.Spec.Availability),
			// The daemon's own word for a state it has no word for is
			// "unknown", and NodeStatus.State is omitempty — so a node the
			// manager has not heard from arrives with an empty string, which a
			// console would render as a blank cell rather than as a state.
			Status:        nodeState(n.Status.State),
			EngineVersion: n.Description.Engine.EngineVersion,
			Addr:          n.Status.Addr,
			// ManagerStatus is nil on every worker, so this is the leader test
			// and not merely the leader flag.
			Leader:       n.ManagerStatus != nil && n.ManagerStatus.Leader,
			TasksRunning: running[n.ID],
			TasksDesired: desired[n.ID],
		})
	}
	// By hostname, because the daemon returns nodes in no defined order and a
	// matrix whose rows moved between refreshes is unreadable. Hostname rather
	// than ID: it is what the operator recognises, and it is what the console
	// labels the row with.
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

// taskCounts totals the tasks scheduled on each node, by node ID.
//
// Two counts rather than one ratio, and neither is len(tasks on the node). The
// swarm keeps a task record long after the task stopped — that is how
// `docker service ps` shows a crash history — so counting every record on a
// node reports a number that only ever grows.
//
// Desired counts the tasks the orchestrator still wants running there; running
// counts those that are. A task being shut down is desired-shutdown and may
// still be running, so it is in neither total, and the pair therefore reads as
// "of the work meant to be on this node, how much is up" rather than as a
// count of records.
func taskCounts(tasks []swarm.Task) (running, desired map[string]int) {
	running = make(map[string]int)
	desired = make(map[string]int)
	for _, t := range tasks {
		if t.NodeID == "" {
			// Not yet scheduled. It belongs to no node's total, and attributing
			// it to the empty key would be a row the roster has no node for.
			continue
		}
		if t.DesiredState == swarm.TaskStateRunning {
			desired[t.NodeID]++
			if t.Status.State == swarm.TaskStateRunning {
				running[t.NodeID]++
			}
		}
	}
	return running, desired
}

// nodeState is the daemon's node state, with its own name for the empty one.
func nodeState(s swarm.NodeState) string {
	if s == "" {
		return string(swarm.NodeStateUnknown)
	}
	return string(s)
}
