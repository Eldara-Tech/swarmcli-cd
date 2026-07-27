// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import "testing"

func TestOwnerIDRoundTrip(t *testing.T) {
	const app = "edge"

	id := OwnerID(app)
	if id != "cd/edge" {
		t.Errorf("OwnerID(%q) = %q, want cd/edge", app, id)
	}
	if got, ok := AppFromOwnerID(id); !ok || got != app {
		t.Errorf("AppFromOwnerID(%q) = %q, %v; want %q, true", id, got, ok, app)
	}
}

// Everything prune must not read as one of this controller's applications. Each
// false here is a release it will refuse to delete.
func TestAppFromOwnerIDRejectsForeignIDs(t *testing.T) {
	for _, id := range []string{
		"",           // no stamp
		"cd/",        // this controller, naming no application
		"apply/prod", // the command line
		"flux/prod",  // another tool
		"cdx/edge",   // a prefix that merely starts the same way
		"edge",       // unprefixed
	} {
		if app, ok := AppFromOwnerID(id); ok {
			t.Errorf("AppFromOwnerID(%q) = %q, true; want it rejected", id, app)
		}
	}
}
