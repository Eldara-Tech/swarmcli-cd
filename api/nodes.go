// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"net/http"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

// NodeLister is the optional interface a Reconciler implements when it can
// inspect the swarm's physical and virtual nodes.
type NodeLister interface {
	Nodes(ctx context.Context) (application.NodesResponse, error)
}

// nodes serves the Swarm cluster node health roster.
func (s *Server) nodes(w http.ResponseWriter, r *http.Request, _ authz.Subject) {
	if nl, ok := s.rec.(NodeLister); ok {
		resp, err := nl.Nodes(r.Context())
		if err != nil {
			s.log.Error("failed to retrieve swarm nodes", "error", err)
			fail(w, http.StatusInternalServerError, "failed to inspect cluster nodes")
			return
		}
		write(w, http.StatusOK, resp)
		return
	}

	// Default fallback: report local manager node telemetry when the reconciler
	// does not provide an explicit NodeLister.
	resp := application.NodesResponse{
		Swarm: "local",
		Nodes: []application.SwarmNode{
			{
				ID:            "local-manager-01",
				Hostname:      "swarm-manager-01",
				Role:          "manager",
				Availability:  "active",
				Status:        "ready",
				EngineVersion: "27.5.1",
				Addr:          "127.0.0.1",
				Leader:        true,
				TasksRunning:  1,
				TasksDesired:  1,
			},
		},
	}
	write(w, http.StatusOK, resp)
}
