// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/feature"
)

// bothSeams is a reconciler wired for everything optional, which is what a
// build with a real applier and a log streamer looks like.
type bothSeams struct{ fakeReconciler }

func (*bothSeams) Nodes(context.Context) (application.NodesResponse, error) {
	return application.NodesResponse{}, nil
}

func (*bothSeams) ServiceLogs(context.Context, string, string, int, bool) (io.ReadCloser, error) {
	return nil, nil
}

// The key set is Capabilities()', not whatever computed the values. A UI hiding
// a control on capabilities["logs"] has to tell false from absent, and a
// dropped key would make the control vanish rather than stay hidden.
//
// It also catches the omission this map is most likely to suffer: a name added
// to Capabilities() with no branch computing it, which yields no key at all.
func TestCapabilityDocumentCarriesExactlyTheDeclaredNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  Reconciler
	}{
		{"no optional seams", &fakeReconciler{}},
		{"every optional seam", &bothSeams{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := discoveryServerWith(t, tc.rec, Options{})
			got := decode[capabilityDocument](t, do(t, h, "GET", "/api/v1/capabilities"))

			for _, name := range Capabilities() {
				if _, present := got.Capabilities[name]; !present {
					t.Errorf("capabilities has no key %q; a UI cannot tell false from absent", name)
				}
			}
			if len(got.Capabilities) != len(Capabilities()) {
				t.Errorf("capabilities = %v, want exactly Capabilities()", got.Capabilities)
			}
		})
	}
}

// The whole point of the map: a build with no streamer must say so, so the
// console can leave the control out rather than offer it and answer 501.
func TestCapabilityReportFollowsTheSeamsTheReconcilerImplements(t *testing.T) {
	h := discoveryServerWith(t, &fakeReconciler{}, Options{})
	got := decode[capabilityDocument](t, do(t, h, "GET", "/api/v1/capabilities"))
	if got.Capabilities[CapabilityLogs] || got.Capabilities[CapabilityNodes] {
		t.Errorf("a reconciler implementing neither seam reported %v", got.Capabilities)
	}

	h = discoveryServerWith(t, &bothSeams{}, Options{})
	got = decode[capabilityDocument](t, do(t, h, "GET", "/api/v1/capabilities"))
	if !got.Capabilities[CapabilityLogs] || !got.Capabilities[CapabilityNodes] {
		t.Errorf("a reconciler implementing both seams reported %v", got.Capabilities)
	}
}

// A capability is a fact about wiring and a feature is what a licence grants.
// Merging them would let a licensed reporter turn an endpoint on, or a build
// that wired a seam differently contradict its own licence document — so the
// two key sets must not overlap, in either direction.
func TestCapabilitiesAreNotFeatures(t *testing.T) {
	for _, c := range Capabilities() {
		for _, f := range feature.All() {
			if string(c) == string(f) {
				t.Errorf("%q is both a capability and a feature; one of them has to move", c)
			}
		}
	}
}

// The narrowed document keeps it, beside the feature map and for the same
// reason: the browser draws the UI from this, and a control present for one
// subject and absent for another is the dead control #178's criterion forbids.
func TestCapabilitiesSurviveTheNarrowedDocument(t *testing.T) {
	h := discoveryServerWith(t, &bothSeams{}, Options{
		Version:    "1.2.0",
		Authorizer: projectAuthorizer{visible: "edge"},
	})
	rr := do(t, h, "GET", "/api/v1/capabilities")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	got := decode[capabilityDocument](t, rr)

	// The controller half is still withheld, so this is genuinely the narrowed
	// document rather than a subject that turned out to be privileged.
	if got.Version != "" {
		t.Fatalf("version = %q, want it withheld", got.Version)
	}
	if len(got.Capabilities) != len(Capabilities()) {
		t.Errorf("capabilities = %v, want exactly Capabilities()", got.Capabilities)
	}
	if !got.Capabilities[CapabilityLogs] {
		t.Error("a tenant is told this build cannot stream logs, and it can")
	}
}
