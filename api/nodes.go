// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

// NodeLister is the optional interface a Reconciler implements when it can
// inspect the swarm's physical and virtual nodes.
//
// Optional in the seam sense: *reconcile.Reconciler implements it, so this
// build answers, but a reconciler reached through the same interface that
// cannot is expected rather than broken. What such a build must not do is
// answer anyway — see nodes below.
//
// Implementing the method is not the same as being able to answer. Which
// backend serves a destination is settled per request, so a reconciler that has
// this method may still meet a backend with no roster behind it; it reports
// that with application.ErrUnsupported, and nodes answers it identically to a
// reconciler that never had the method.
type NodeLister interface {
	Nodes(ctx context.Context) (application.NodesResponse, error)
}

// nodes serves the Swarm cluster node health roster.
//
// A build that cannot answer says so, with a status a client can branch on. It
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
	switch {
	case errors.Is(err, application.ErrUnsupported):
		// The same answer, and deliberately the same sentence, as a reconciler
		// that is not a NodeLister at all. Whether a build cannot list nodes
		// because its reconciler has no method or because the backend behind
		// this destination has no roster is a distinction inside the
		// controller; from the caller's side it is one fact, and a client
		// branching on the status must not have to tell them apart.
		fail(w, http.StatusNotImplemented, "this controller does not report swarm node telemetry")
		return
	case err != nil:
		s.log.Error("failed to retrieve swarm nodes", "error", err)
		fail(w, http.StatusInternalServerError, "failed to inspect cluster nodes")
		return
	}
	write(w, http.StatusOK, resp)
}
