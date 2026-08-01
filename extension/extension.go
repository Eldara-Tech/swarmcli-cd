// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package extension is the fifth seam: it lets a companion module add HTTP
// routes to the controller's API without forking package api or package
// controller.
//
// A route is declared, not registered. The extension returns what it wants
// served and the core decides what wraps it, because what wraps it is an
// authorisation decision on a process holding write access to the swarm. The
// alternative — handing a companion the mux — gives away three things the core
// cannot get back: the guarantee that a route is authorised, the ability to name
// every route in the startup log, and the chance to refuse a colliding pattern
// before net/http panics on it. A companion that returns a table can only be
// unauthenticated by saying so, in a method whose name is PublicRoutes.
//
// docs/extensibility.md is the fuller version — what a companion may replace,
// what it may add, and what it still cannot do.
//
// # Registration is not over at the end of init()
//
// This is a seam.List rather than a seam.Slot: several companions may each add
// routes, and appending is the only sensible composition. So, per seam's own
// contract, this seam is not settled at the end of init() and a consumer may not
// snapshot it at construction and keep the result. api.Handler reads All once,
// at the moment it builds the mux — which is during wiring, after every init()
// has run — and that is why reading it once is correct there rather than an
// exception to the rule.
//
// # No OSS default, and no init()
//
// Unlike the other four seams this package registers nothing. There is no
// behaviour to default: no route is the correct behaviour for a build with no
// companion loaded, and seam.List's zero value already is exactly that. It is a
// deliberate deviation from docs/extensibility.md's "register the default from
// the package's own init()" step, and the reason the step does not apply is what
// the step is for — a Slot with nothing registered hands its consumer a nil
// implementation, and a List has no such value to hand out.
package extension

import (
	"net/http"

	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/seam"
)

// Handler serves one extension route. It is api's guarded handler shape: the
// subject is a parameter rather than a value smuggled through the request
// context, because a handler that makes a second and finer authorisation
// decision with it — the way the core's list endpoint narrows its collection —
// would otherwise be handed a zero Subject by a context lookup that came back
// empty. An authorizer given one fails closed, which is the right direction and
// the wrong failure: it looks like a caller with no permissions rather than like
// the wiring mistake it is. api states the same argument beside its own guarded
// type, and the two shapes are identical so that the core can convert between
// them without this package importing api.
//
// For a route returned from PublicRoutes the subject is the zero value — nothing
// authenticated the request — so a public handler must not consult it.
type Handler func(w http.ResponseWriter, r *http.Request, subject authz.Subject)

// Route is one pattern the core will serve.
//
// It is a struct rather than a parameter list, for the reason secrets.Request
// states: a field added later costs an implementation outside this repository
// nothing, and widening a parameter list is a breaking change to an interface
// implemented outside it.
type Route struct {
	// Pattern is a net/http ServeMux pattern, method included:
	// "GET /api/v1/projects". Any path — there is no reserved prefix, because an
	// SSO callback path is often fixed by the identity provider's registration
	// and cannot be moved. The core refuses to start if it collides with a core
	// route or with another extension's.
	Pattern string
	// Action is the authz.Action the core authorises this route with, before the
	// handler runs. Empty is a startup error rather than a default: there is no
	// action that is obviously right for a route this repository has never seen,
	// and guessing one would be guessing at a permission.
	//
	// authz.Action is a string type and its constants are additive, so an
	// extension may declare an action of its own — its authorizer is the only
	// thing that has to recognise it, and authz.Action's contract is that an
	// unrecognised action is refused.
	Action authz.Action
	// Handler serves the route, behind whatever the core decided wraps it.
	Handler Handler
}

// Extension is a set of routes to serve.
type Extension interface {
	// Routes are served behind the same guard as every core route:
	// authenticated, then authorised with the route's Action.
	Routes() []Route
	// PublicRoutes are served with no authentication and no authorisation.
	//
	// It is a separate method rather than a field on Route so that an
	// unauthenticated endpoint on a process holding the Docker socket cannot be
	// created by leaving a bool unset: an unset field reads like every other
	// unset field in a struct literal, while a second method has to be written,
	// named and returned from. It exists for one shape: a callback that arrives
	// carrying a credential this controller did not issue, which therefore
	// cannot be authenticated by the authorizer that guards everything else.
	// Every route returned here is logged at WARN at startup, by pattern and by
	// the name it registered under.
	//
	// A Route returned here must leave Action empty.
	PublicRoutes() []Route
}

var list seam.List[Extension]

// Register appends an extension. It removes nothing. Call it from an init().
//
// The name is what the controller logs beside the routes e contributes,
// including the WARN line for each of its public ones, so an operator reading
// the log can tell which module added an endpoint.
func Register(name string, e Extension) { list.Register(name, e) }

// All returns every registered extension.
func All() []Extension { return list.All() }

// Active names every registered extension, for startup logging.
func Active() []string { return list.Names() }
