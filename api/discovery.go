// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"net/http"

	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/extension"
	"github.com/Eldara-Tech/swarmcli-cd/feature"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
	"github.com/Eldara-Tech/swarmcli-cd/secrets"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// The two discovery documents, deliberately asymmetric: a login screen has to
// know how to log in before anyone is authenticated, and everything else about
// the build is behind the token.

// loginOption is one login method as the public document reports it.
//
// Its own type rather than authz.LoginMethod on the wire, for the reason
// api.guarded and extension.Handler are two identical types: the seam is free
// to grow a field and this endpoint is not. A field added to LoginMethod would
// otherwise appear on an unauthenticated endpoint the day a companion was
// recompiled, with nobody having decided that it should.
type loginOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// omitempty because a credential that is typed in has nowhere to send the
	// browser first, and a UI that saw "start": "" would have to know that an
	// empty string means "do not navigate".
	Start string `json:"start,omitempty"`
}

// bootstrap serves what a login screen needs before anyone is authenticated,
// and nothing else.
//
// In particular no version string. An unauthenticated caller does not need the
// build number, and handing one out is free CVE matching; the version is in the
// guarded document below.
//
// no-store because installing a licence, or loading an SSO companion, changes
// what this says — and a login screen cached from before that offers a box for
// a credential the deployment no longer issues, with no way for the operator to
// tell that the browser is the thing that is wrong.
func (s *Server) bootstrap(w http.ResponseWriter, _ *http.Request) {
	methods := authz.MethodsFor(s.authz)
	// Sized rather than nil, so an authorizer that names no method at all
	// marshals as [] and a UI can iterate it. A null there is a TypeError in
	// the login screen rather than an empty list of buttons.
	login := make([]loginOption, 0, len(methods))
	for _, m := range methods {
		login = append(login, loginOption{ID: m.ID, Label: m.Label, Start: m.Start})
	}
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusOK, map[string]any{"login": login})
}

// capabilityDocument is what an operator holding the token can read about the
// build itself.
//
// The licence is the seam's own type rather than a copy of it, which is the
// opposite of the call made for loginOption above, and the difference is who
// reads the result: a field a companion adds to feature.Licence reaches a
// caller this endpoint already authorised with ActionRead, not the internet.
type capabilityDocument struct {
	Version  string                `json:"version"`
	Edition  string                `json:"edition"`
	Features map[feature.Name]bool `json:"features"`
	Licence  *feature.Licence      `json:"licence"`
	Seams    seamsDocument         `json:"seams"`
}

// seamsDocument is the "seams" line controller.serve logs at startup, made
// readable by an operator who has the token but not the logs. The two are the
// same set of names on purpose: an operator comparing a support answer against
// their own logs must not find two different lists.
type seamsDocument struct {
	Swarms  string   `json:"swarms"`
	Authz   string   `json:"authz"`
	Notify  []string `json:"notify"`
	Secrets string   `json:"secrets"`
	// Feature is not redundant with Edition. Under D20 the licensed reporter is
	// always linked, so this reads "licence" while the edition still reads
	// "community" until a licence verifies, and that pair is the whole
	// diagnostic for "is the module missing, or is the licence missing" —
	// neither field answers it alone.
	Feature   string   `json:"feature"`
	Extension []string `json:"extension"`
}

// capabilities serves the build's own capability report.
//
// Reachable with ActionRead, and split. Every subject that may read anything
// gets the edition and the feature map, because that is what the browser draws
// the UI from and a control that vanished for a tenant would be the dead
// control #178's criterion forbids. Everything else is ActionController's:
//
//   - version, which bootstrap.json deliberately withholds from an
//     unauthenticated caller on the stated grounds that handing out a build
//     number is free CVE matching. Handing it to every subject with `read` —
//     the widest action this API has, and the one an authorizer implementing
//     projects gives ordinary tenants — gives that argument away.
//   - licence, which is the deployment's commercial state.
//   - seams, which names every implementation loaded into a process holding the
//     docker socket, companion extension modules included. That is a map of the
//     deployment.
//
// A narrowed document rather than a 403 because an authorizer that predates
// ActionController refuses it, as authz.Action's contract requires, and the
// shell reads this on every load.
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request, subject authz.Subject) {
	report := s.features.Report(r.Context())

	// The keys come from feature.All rather than from the report, so that a
	// reporter cannot decide what the document is shaped like. A UI hiding a
	// control on features["sso"] has to tell false from absent, and a key the
	// reporter simply omitted would make the control vanish rather than grey
	// out.
	features := make(map[feature.Name]bool, len(feature.All()))
	for _, name := range feature.All() {
		features[name] = report.Features[name]
	}

	doc := capabilityDocument{
		Edition:  report.Edition,
		Features: features,
		// The same shape whichever half this is, so that a client reads one
		// document type: the two name lists are [] rather than null for the
		// reason orEmpty exists.
		Seams: seamsDocument{Notify: []string{}, Extension: []string{}},
	}
	if s.authz.Authorize(r.Context(), subject, authz.ActionController, "") == nil {
		doc.Version = s.version
		doc.Licence = report.Licence
		doc.Seams = seamsDocument{
			Swarms:  swarms.Active(),
			Authz:   authz.Active(),
			Notify:  orEmpty(notify.Active()),
			Secrets: secrets.Active(),
			Feature: feature.Active(),
			// Asked per request rather than derived from Routes(), because
			// under D22 an unlicensed companion declares zero routes — so a
			// module read from the route table would vanish from this document
			// in exactly the state an operator is trying to diagnose.
			Extension: orEmpty(extension.Active()),
		}
	}
	write(w, http.StatusOK, doc)
}

// orEmpty makes a seam's name list safe to iterate in a browser: the List seams
// hand back a nil slice when nothing registered, and nil marshals as null.
func orEmpty(n []string) []string {
	if n == nil {
		return []string{}
	}
	return n
}
