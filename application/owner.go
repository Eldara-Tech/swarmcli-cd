// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import (
	"fmt"
	"strings"
	"unicode"
)

// ownerPrefix namespaces every owner id this controller stamps. The command
// line stamps "apply/", so a release file applied by hand and an application
// reconciled here can never claim each other's releases.
const ownerPrefix = "cd/"

// DefaultControllerID is the identity a controller stamps with when the
// deployment does not choose one.
//
// It is a real default rather than a required flag because the single-controller
// case is the overwhelmingly common one and should not need ceremony. Two
// controllers sharing a swarm must be given distinct ids — see OwnerID.
const DefaultControllerID = "default"

// OwnerID is the id this controller stamps a release with and classifies
// deployed releases against: "cd/<controller>/<application>".
//
// Both halves are load-bearing and for different reasons.
//
// The application half is what keeps sibling applications apart. Several
// applications share one swarm, and an id that named only the controller would
// make each of them report the others' releases as its own orphans.
//
// The controller half is what keeps whole controllers apart, and exists because
// prune acts on the difference. A sweep asks "which releases on this swarm
// belong to an application my app set no longer declares", and without a
// controller in the id, a second swarmcli-cd on the same swarm answers that
// question about the first one's applications — and deletes them. Two
// controllers sharing a swarm must therefore be given distinct ids, or each
// will treat the other's work as departed.
//
// It lives here, in the wire contract, rather than in the reconciler that
// writes it, because it is also what prune reads back off the swarm to decide
// what may be deleted. Two copies of this format that drifted apart would not
// fail loudly — the reconciler would keep stamping and prune would quietly stop
// recognising, which is a deletion bug in whichever direction it broke.
func OwnerID(controller, app string) string {
	if controller == "" {
		controller = DefaultControllerID
	}
	return ownerPrefix + controller + "/" + app
}

// AppFromOwnerID reports which of this controller's applications an owner id
// names, and whether the id belongs to this controller at all.
//
// False for everything else on the swarm: an "apply/" stamp from the command
// line, another tool's id, a bare prefix naming no application, and — the case
// this exists for — an id belonging to a different swarmcli-cd. Prune treats
// false as "not mine", which is what stops it deleting a release it did not
// install.
//
// A stamp in the pre-controller-id format ("cd/<app>") is likewise not this
// controller's. That is deliberate — it reads as unmanaged, so prune leaves it
// alone, and the migration errs towards not deleting.
//
// It does heal, and it is worth being exact about when. Ownership is part of
// what decides whether a release needs deploying (swarmcli#511), so a stamp in
// this format contradicts the one this controller would write, and the first
// reconcile that plans the release redeploys it once and stamps it properly.
// That is the next pass for an automated application and not until it is asked
// for a manual one, so the old format outlives the upgrade by an interval at
// least. What keeps that interval safe is not the stamp but prune's second
// signal: a release an application still declares is never swept, whatever it
// is stamped with (#62).
func AppFromOwnerID(controller, id string) (string, bool) {
	if controller == "" {
		controller = DefaultControllerID
	}
	app, ok := strings.CutPrefix(id, ownerPrefix+controller+"/")
	if !ok || app == "" || strings.Contains(app, "/") {
		return "", false
	}
	return app, true
}

// ValidateControllerID refuses an id that would produce an unparseable stamp or
// one that cannot be told apart from another.
//
// A slash would make "cd/<controller>/<application>" ambiguous about where the
// controller ends, and a colon is what the chart engine itself rejects. Space
// is refused because an id that differs from another only by trailing
// whitespace is the kind of distinction an operator cannot see and prune would
// act on.
func ValidateControllerID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("the controller id is empty")
	case strings.ContainsAny(id, "/:"):
		return fmt.Errorf("invalid controller id '%s': it must not contain '/' or ':'", id)
	case strings.IndexFunc(id, unicode.IsSpace) >= 0:
		return fmt.Errorf("invalid controller id '%s': it must not contain whitespace", id)
	}
	return nil
}
