// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import "strings"

// ownerPrefix namespaces every owner id this controller stamps. The command
// line stamps "apply/", so a release file applied by hand and an application
// reconciled here can never claim each other's releases.
const ownerPrefix = "cd/"

// OwnerID is the id this controller stamps a release with and classifies
// deployed releases against.
//
// It is per application rather than per controller: several applications share
// one swarm, and a per-controller id would make each of them report the others'
// releases as its own orphans.
//
// It lives here, in the wire contract, rather than in the reconciler that
// writes it, because it is also what prune reads back off the swarm to decide
// what may be deleted. Two copies of this string that drifted apart would not
// fail loudly — the reconciler would keep stamping and prune would quietly stop
// recognising, which is a deletion bug in whichever direction it broke.
func OwnerID(app string) string { return ownerPrefix + app }

// AppFromOwnerID reports which application an owner id names, and whether the
// id belongs to this controller at all.
//
// False for anything else on the swarm — an "apply/" stamp from the command
// line, another tool's id, or a bare "cd/" naming no application. Prune treats
// false as "not mine", which is what keeps it from deleting a release it did
// not install.
func AppFromOwnerID(id string) (string, bool) {
	app, ok := strings.CutPrefix(id, ownerPrefix)
	if !ok || app == "" {
		return "", false
	}
	return app, true
}
