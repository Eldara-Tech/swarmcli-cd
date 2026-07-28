// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import "testing"

func TestOwnerIDRoundTrip(t *testing.T) {
	id := OwnerID("prod", "edge")
	if id != "cd/prod/edge" {
		t.Errorf("OwnerID = %q, want cd/prod/edge", id)
	}
	if got, ok := AppFromOwnerID("prod", id); !ok || got != "edge" {
		t.Errorf("AppFromOwnerID = %q, %v; want edge, true", got, ok)
	}
}

// An empty controller is the default one on both sides, so a caller that never
// configures an identity still round-trips.
func TestAnEmptyControllerIsTheDefaultOne(t *testing.T) {
	if OwnerID("", "edge") != OwnerID(DefaultControllerID, "edge") {
		t.Error("an empty controller id must stamp as the default one")
	}
	if got, ok := AppFromOwnerID("", OwnerID(DefaultControllerID, "edge")); !ok || got != "edge" {
		t.Errorf("AppFromOwnerID = %q, %v; want edge, true", got, ok)
	}
}

// The reason the controller half exists: one controller must not read another's
// releases as its own, because prune deletes on exactly that judgement.
func TestAnotherControllersReleasesAreNotMine(t *testing.T) {
	theirs := OwnerID("staging", "edge")
	if app, ok := AppFromOwnerID("prod", theirs); ok {
		t.Errorf("AppFromOwnerID(prod, %q) = %q, true; want another controller's release rejected", theirs, app)
	}
}

// Everything prune must refuse to read as one of this controller's
// applications. Each false here is a release it will not delete.
func TestAppFromOwnerIDRejectsForeignIDs(t *testing.T) {
	for _, id := range []string{
		"",              // no stamp
		"cd/prod/",      // this controller, naming no application
		"cd/prod",       // the pre-controller-id format, which is nobody's now
		"cd/edge",       // likewise, and not to be mistaken for controller "edge"
		"apply/prod",    // the command line
		"flux/prod",     // another tool
		"cdx/prod/edge", // a prefix that merely starts the same way
		"cd/prodx/edge", // a controller id that merely starts the same way
		"edge",          // unprefixed
	} {
		if app, ok := AppFromOwnerID("prod", id); ok {
			t.Errorf("AppFromOwnerID(prod, %q) = %q, true; want it rejected", id, app)
		}
	}
}

// An application name cannot contain a slash, so a stamp with an extra segment
// is malformed rather than a nested application.
func TestAnExtraSegmentIsNotAnApplication(t *testing.T) {
	if app, ok := AppFromOwnerID("prod", "cd/prod/edge/extra"); ok {
		t.Errorf("AppFromOwnerID = %q, true; want a malformed stamp rejected", app)
	}
}

func TestValidateControllerID(t *testing.T) {
	for _, ok := range []string{"default", "prod", "prod-eu", "a.b_c", "Prod1"} {
		if err := ValidateControllerID(ok); err != nil {
			t.Errorf("ValidateControllerID(%q) = %v, want nil", ok, err)
		}
	}
	// A slash would make the stamp ambiguous about where the controller ends; a
	// colon is what the chart engine rejects; whitespace is a difference an
	// operator cannot see but prune would act on.
	for _, bad := range []string{"", "a/b", "a:b", "a b", "trailing "} {
		if err := ValidateControllerID(bad); err == nil {
			t.Errorf("ValidateControllerID(%q) = nil, want a refusal", bad)
		}
	}
}
