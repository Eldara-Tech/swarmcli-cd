// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package authz answers who is calling the HTTP API and whether they may do
// what they are asking.
//
// Authentication and authorisation are one seam because they are replaced
// together: per D1 the Business Edition swaps authentication for SSO and
// authorisation for projects and RBAC. Splitting them would mean two companion
// packages that have to agree about the same subject.
package authz

import (
	"context"
	"net/http"

	"github.com/Eldara-Tech/swarmcli-cd/seam"
)

// Subject is whoever is making a request.
type Subject struct {
	Name string
}

// Action is what a request is trying to do. Phase 1 has two: read anything, or
// trigger a sync.
type Action string

const (
	ActionRead Action = "read"
	ActionSync Action = "sync"
)

// Authorizer gates every API request.
type Authorizer interface {
	// Ready reports whether this authorizer is configured well enough to be
	// used. The controller refuses to start when it is not.
	//
	// This exists because the alternative failure mode is silent: an
	// unconfigured authorizer that merely rejects everything looks, to an
	// operator, exactly like a wrong token. A startup error names the problem.
	Ready() error

	// Authenticate resolves a request to a subject. An error is a 401.
	Authenticate(r *http.Request) (Subject, error)

	// Authorize reports whether s may perform act on the named application. An
	// empty application means the request is not scoped to one. An error is a
	// 403.
	Authorize(ctx context.Context, s Subject, act Action, application string) error
}

var slot seam.Slot[Authorizer]

// Register installs a as the authorizer, replacing whatever was there. Call it
// from an init().
func Register(name string, a Authorizer) { slot.Register(name, a) }

// Get returns the authorizer in force.
func Get() Authorizer { return slot.Get() }

// Active names the authorizer in force, for startup logging.
func Active() string { return slot.Name() }
