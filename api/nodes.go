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
//
// Optional in the seam sense: nothing in this repository implements it, and a
// build whose reconciler does not is expected rather than broken. What such a
// build must not do is answer anyway — see nodes below.
type NodeLister interface {
	Nodes(ctx context.Context) (application.NodesResponse, error)
}

// nodes serves the Swarm cluster node health roster.
//
// A build with no NodeLister says so, with a status a client can branch on. It
// used to answer 200 with a node it made up — one manager called
// swarm-manager-01 at 127.0.0.1 running engine 27.5.1, ready, one task of one —
// and the console drew that as its Swarm Node Matrix. Every field was a
// literal. An operator asking a control plane "what is my cluster" and being
// told a hostname that does not exist, a Docker version that is not theirs and
// a task count that is fiction is worse served than one told nothing: nothing
// is a question, and a green single-node roster is an answer.
//
// 501 rather than 200-with-an-empty-list for the same reason. Empty would mean
// "a swarm with no nodes", which is not a thing, and the UI could not tell it
// apart from a roster it failed to read.
func (s *Server) nodes(w http.ResponseWriter, r *http.Request, _ authz.Subject) {
	nl, ok := s.rec.(NodeLister)
	if !ok {
		fail(w, http.StatusNotImplemented, "this controller does not report swarm node telemetry")
		return
	}

	resp, err := nl.Nodes(r.Context())
	if err != nil {
		s.log.Error("failed to retrieve swarm nodes", "error", err)
		fail(w, http.StatusInternalServerError, "failed to inspect cluster nodes")
		return
	}
	write(w, http.StatusOK, resp)
}
