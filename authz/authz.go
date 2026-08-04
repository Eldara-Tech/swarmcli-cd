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
//
// It travels by value from Authenticate to Authorize through api.guard, which
// is code the companion does not control, so whatever an authorizer learns
// while authenticating has to fit in here or be learned again. An SSO
// authorizer that has just validated an ID token holding groups and a tenant
// would otherwise have two options, both bad: re-resolve against the identity
// provider on every authorisation — a network round trip inside a request the
// controller expects to be cheap — or cache on a display name, which is
// neither unique nor stable.
//
// It is a struct for the reason secrets.Request gives, and the same rule
// applies: fields may be added, and an implementation outside this repository
// keeps compiling when they are.
type Subject struct {
	// Name is who the subject is, for logs and for display. It is not a key:
	// nothing in this repository looks a subject up by it.
	Name string
	// Groups is whatever group, role or claim membership the authorizer
	// resolved. Every identity source this seam is likely to meet has the
	// concept — LDAP, OIDC, SAML — which is why it is named here rather than
	// left to Extra.
	Groups []string
	// Extra is the authorizer's own, carried from Authenticate to Authorize
	// unchanged.
	//
	// Nothing in this repository reads it, logs it, serialises it or copies
	// what it points at, and nothing ever will: it exists so that an
	// authorizer can carry state the core has no concept of — a tenant, a
	// licence entitlement, a decoded token — through core code, without that
	// concept having to be named in an Apache-2.0 tree that never uses it.
	//
	// The core treats the value as immutable. An implementation that puts a
	// pointer here gets the same pointer back.
	Extra any
}

// Action is what a request is trying to do.
//
// A string type rather than an enumeration with a fixed range, so that this list
// can grow without breaking an authorizer implemented outside this repository —
// the same rule the seam's structs follow, one level down. The obligation that
// comes with it is the companion's: an action it does not recognise is a
// permission it was not written to grant, so it must refuse. Authorisation may
// only degrade closed.
type Action string

const (
	// ActionRead is the list view, the detail view, the controller's own status
	// and the event stream — the state of things, as the controller holds it.
	ActionRead Action = "read"
	// ActionDiff is the manifest change a sync would make.
	//
	// Its own action because it is its own disclosure. A list row is a state and
	// a revision; a diff is the rendered manifest — images, environment, mounts,
	// command lines — which is the closest thing this API has to reading the
	// repository. An authorizer has a reason to grant a project's operators the
	// first and not the second, and while every route passed ActionRead it had no
	// way to say so.
	ActionDiff Action = "diff"
	// ActionHistory is a release's recorded revisions.
	//
	// Separate for the same reason as ActionDiff, and for one of its own: it is
	// the only read that goes to the swarm rather than to the controller's own
	// cache, so it is the only one whose cost an authorizer might want to gate.
	ActionHistory Action = "history"
	// ActionSync triggers a reconcile that applies whatever the plan contains.
	// The only action that writes.
	ActionSync Action = "sync"
	// ActionController is the controller and the build themselves, as distinct
	// from the applications they reconcile: where the app set is sourced from,
	// how many applications there are in total, why a load was refused, the
	// version, the licence, and which seam implementation is loaded behind each
	// seam.
	//
	// Its own action because none of that is a per-application fact, so Visible
	// cannot narrow it the way it narrows a list of names — and because
	// ActionRead is the widest action this API has and the one an authorizer
	// implementing projects hands to ordinary tenants. A tenant scoped to one
	// project has no business reading the repository the whole fleet is
	// deployed from, the total number of applications in it, the build number,
	// or a map of the modules loaded into a process holding the docker socket.
	//
	// **Refusing it is not a 403.** The two endpoints that ask are still
	// reachable with ActionRead and answer a narrowed document, because a
	// browser reads both on every screen — the list screen shows the app set's
	// shape and the shell reads the capability report to decide what to draw.
	// That is also what makes this action safe to have added: an authorizer
	// that predates it refuses it, as this type's contract requires, and gets
	// less disclosed rather than a screen that stopped working.
	ActionController Action = "controller"
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

	// Visible narrows applications to the ones s may perform act on, in the
	// order it was given them. An authorizer with nothing to narrow returns its
	// input unchanged; an error is a 403, like Authorize's.
	//
	// It exists because Authorize answers about one application and the API has
	// two endpoints that answer about all of them — the list view and the event
	// stream. Authorising those once with an empty application is a decision
	// about the collection, not about its members, so an authorizer
	// implementing projects can only allow or deny the whole thing: a tenant
	// with read access to one application would enumerate every application's
	// name, repository URL, revision and error text.
	//
	// A separate method rather than a loop over Authorize because a companion
	// may back each decision with a policy engine, and a list endpoint should
	// cost one call rather than one per application. The event stream is the
	// other shape and keeps using Authorize: there the question really is about
	// one application at a time, as each event arrives.
	//
	// On this interface rather than beside it as an optional one, which is the
	// opposite of the call swarms makes for Lister and NodeReach — and the
	// difference is which way the absence fails. A registry that cannot
	// enumerate makes a sweep cover less; an authorizer that cannot narrow
	// would make a list disclose more, because the fallback for a missing
	// narrowing is to return everything. Authorisation may only degrade
	// closed, so there is no version of this a companion is allowed not to
	// implement.
	Visible(ctx context.Context, s Subject, act Action, applications []string) ([]string, error)
}

// LoginMethod is one way to obtain a credential this authorizer accepts.
//
// It is a struct for the reason Subject and secrets.Request are: an
// implementation outside this repository receives it, so a field added later —
// an icon, an ordering hint — costs it nothing.
type LoginMethod struct {
	// ID is what the UI branches on: "token", "sso".
	ID string
	// Label is what the login screen shows on the box or the button.
	Label string
	// Start is where the browser goes to begin, and is empty for a credential
	// the operator types in. An SSO authorizer points it at the login route it
	// declared through extension.PublicRoutes.
	Start string
}

// LoginMethods is the optional interface an Authorizer implements to say how a
// browser may authenticate.
//
// It is beside the seam rather than on it, which is the shape swarms.Lister and
// swarms.NodeReach use and the opposite of the call made for Visible. The test
// is always which way the absence fails, and here it fails harmlessly: an
// authorizer that does not implement this is presented as a token box, which is
// what every authorizer before this one was. Visible is on the interface
// precisely because its absence would make a list disclose more, and
// authorisation may only degrade closed.
//
// **The default authorizer implements it**, and returns the token box. An
// SSO-only companion returns only its own method, so the box disappears — which
// is the point: a deployment that has moved to SSO should not offer a box for a
// credential it no longer issues.
type LoginMethods interface {
	// LoginMethods names every way a browser may obtain a credential, in the
	// order the login screen should offer them.
	LoginMethods() []LoginMethod
}

// MethodsFor reports how a browser may authenticate against a.
//
// A function here rather than a type assertion at the call site, because the
// fallback is a *value* and not merely "do less": written in api it would put
// this package's label for this package's authorizer somewhere that has no
// business spelling it, and the two spellings of the same box would drift.
func MethodsFor(a Authorizer) []LoginMethod {
	if lm, ok := a.(LoginMethods); ok {
		return lm.LoginMethods()
	}
	return []LoginMethod{tokenLogin}
}

var slot seam.Slot[Authorizer]

// Register installs a as the authorizer, replacing whatever was there. Call it
// from an init().
func Register(name string, a Authorizer) { slot.Register(name, a) }

// Get returns the authorizer in force.
func Get() Authorizer { return slot.Get() }

// Active names the authorizer in force, for startup logging.
func Active() string { return slot.Name() }
