// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/feature"
)

// discoveryServer builds a router over o, which testServer cannot: the two
// documents are about the options themselves — the version stamped in and the
// reporter in force — rather than about the reconciler behind them.
func discoveryServer(t *testing.T, o Options) http.Handler {
	t.Helper()
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// Otherwise New falls back to the seam, which in a test binary is the real
	// token authorizer with no token configured — every guarded route a 401 for
	// a reason that has nothing to do with what is being tested.
	if o.Authorizer == nil {
		o.Authorizer = &allowAll{}
	}
	h, err := New(&fakeReconciler{}, o).Handler()
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}
	return h
}

// ssoOnly is the companion shape: it issues no admin token, so the token box
// would be a dead end.
type ssoOnly struct{ authz.Authorizer }

func (ssoOnly) LoginMethods() []authz.LoginMethod {
	return []authz.LoginMethod{{ID: "sso", Label: "Sign in with SSO", Start: "/auth/login"}}
}

// noMethods names nothing at all, which is the case that decides whether the
// document marshals [] or null.
type noMethods struct{ authz.Authorizer }

func (noMethods) LoginMethods() []authz.LoginMethod { return nil }

// reporter answers with what it was built with, including keys feature.All
// never mentions.
type reporter struct{ report feature.Report }

func (r reporter) Report(context.Context) feature.Report { return r.report }

// The login screen has to be drawn before anyone can authenticate, so this one
// answers without a credential — including behind an authorizer that rejects
// everything, which is what proves it is not merely being let through by a
// permissive fake.
//
// And it carries no version string. An unauthenticated caller does not need the
// build number, and handing one out is free CVE matching.
func TestBootstrapIsPublicAndCarriesNoVersion(t *testing.T) {
	const stamped = "9.9.9-test"
	h := discoveryServer(t, Options{Authorizer: denyAuthn{}, Version: stamped})

	rr := do(t, h, "GET", "/ui/bootstrap.json")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the login screen cannot authenticate to find out how to authenticate", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: installing a licence changes what this says", got)
	}

	if body := rr.Body.String(); strings.Contains(body, stamped) {
		t.Errorf("the public document carries the build version: %s", body)
	}
	// The whole document, not just the version: anything else added here is
	// also unauthenticated disclosure, and this is where that gets noticed.
	keys := decode[map[string]json.RawMessage](t, rr)
	if len(keys) != 1 {
		t.Errorf("the public document has keys %v, want only login", slices.Sorted(maps.Keys(keys)))
	}

	got := decode[struct {
		Login []loginOption `json:"login"`
	}](t, rr)
	want := []loginOption{{ID: "token", Label: "Admin token"}}
	if !slices.Equal(got.Login, want) {
		t.Errorf("login = %+v, want exactly the token box: %+v", got.Login, want)
	}
}

// A deployment that has moved to SSO must not offer a box for a credential it
// no longer issues, which is the whole point of asking the authorizer rather
// than hardcoding the box here.
func TestBootstrapReportsTheAuthorizersOwnMethods(t *testing.T) {
	h := discoveryServer(t, Options{Authorizer: ssoOnly{}})

	got := decode[struct {
		Login []loginOption `json:"login"`
	}](t, do(t, h, "GET", "/ui/bootstrap.json"))

	want := []loginOption{{ID: "sso", Label: "Sign in with SSO", Start: "/auth/login"}}
	if !slices.Equal(got.Login, want) {
		t.Errorf("login = %+v, want only the authorizer's own: %+v", got.Login, want)
	}
}

// An authorizer that names no method marshals as [], not null: a login screen
// iterating null is a TypeError rather than an empty list of buttons.
func TestBootstrapMarshalsNoMethodsAsAnEmptyList(t *testing.T) {
	h := discoveryServer(t, Options{Authorizer: noMethods{}})

	if body := strings.TrimSpace(do(t, h, "GET", "/ui/bootstrap.json").Body.String()); !strings.Contains(body, `"login":[]`) {
		t.Errorf("body = %s, want an empty login array", body)
	}
}

// The free build's answer, and the acceptance criterion for the whole seam:
// community, every feature false, no licence.
func TestCapabilitiesReportsTheFreeBuild(t *testing.T) {
	h := discoveryServer(t, Options{Version: "1.2.0"})

	rr := do(t, h, "GET", "/api/v1/capabilities")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	got := decode[capabilityDocument](t, rr)

	if got.Version != "1.2.0" {
		t.Errorf("version = %q, want the stamped one", got.Version)
	}
	if got.Edition != feature.EditionCommunity {
		t.Errorf("edition = %q, want %q", got.Edition, feature.EditionCommunity)
	}
	if got.Licence != nil {
		t.Errorf("licence = %+v, want null: this build has no licensed module in it", *got.Licence)
	}
	for _, name := range feature.All() {
		granted, present := got.Features[name]
		if !present {
			t.Errorf("features has no key %q; a UI cannot tell false from absent", name)
		}
		if granted {
			t.Errorf("the free build reports %q granted", name)
		}
	}
	if len(got.Features) != len(feature.All()) {
		t.Errorf("features = %v, want exactly feature.All()", got.Features)
	}

	// The seams an operator with the token but not the logs would otherwise
	// have no way to read. seams.feature is not redundant with edition: it
	// names the reporter whether or not a licence verified.
	if got.Seams.Authz == "" || got.Seams.Swarms == "" || got.Seams.Secrets == "" {
		t.Errorf("seams = %+v, want every Slot seam named", got.Seams)
	}
	if got.Seams.Feature != feature.Active() {
		t.Errorf("seams.feature = %q, want %q", got.Seams.Feature, feature.Active())
	}
	// Empty lists rather than null, for the reason the login list is.
	if body := rr.Body.String(); strings.Contains(body, `"extension":null`) || strings.Contains(body, `"notify":null`) {
		t.Errorf("a seam list marshalled as null: %s", body)
	}
}

// #206: the three fields a tenant has no business reading, and the two it must
// keep reading.
//
// ActionRead is the widest action this API has and the one an authorizer
// implementing projects hands to ordinary tenants. What was behind it here is
// the build number — which bootstrap.json deliberately withholds from an
// unauthenticated caller on the grounds that it is free CVE matching — the
// deployment's licence, and the name of every seam implementation loaded into a
// process holding the docker socket, companion modules included.
//
// projectAuthorizer refuses an action it was not written for, as authz.Action's
// contract requires, so it is exactly an authorizer that predates
// ActionController — and the answer is a narrowed document rather than a 403,
// because the shell reads this on every load.
func TestCapabilitiesIsNarrowedForASubjectThatMayNotReadTheController(t *testing.T) {
	expires := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	h := discoveryServer(t, Options{
		Version:    "1.2.0",
		Authorizer: projectAuthorizer{visible: "edge"},
		Features: reporter{report: feature.Report{
			Edition:  "business",
			Features: feature.Set{feature.SSO: true},
			Licence:  &feature.Licence{Tier: "be", Status: feature.StatusValid, ExpiresAt: &expires},
		}},
	})

	rr := do(t, h, "GET", "/api/v1/capabilities")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a narrowed subject gets less, not a refusal", rr.Code)
	}
	got := decode[capabilityDocument](t, rr)

	if got.Version != "" {
		t.Errorf("version = %q, want it withheld", got.Version)
	}
	if got.Licence != nil {
		t.Errorf("licence = %+v, want it withheld", *got.Licence)
	}
	if got.Seams.Authz != "" || got.Seams.Swarms != "" || got.Seams.Secrets != "" || got.Seams.Feature != "" {
		t.Errorf("seams = %+v, want no implementation named", got.Seams)
	}
	if len(got.Seams.Notify) != 0 || len(got.Seams.Extension) != 0 {
		t.Errorf("seams = %+v, want no module named", got.Seams)
	}
	// Still the same document type, so a client reads one shape: the lists are
	// [] rather than null whichever half this is.
	if body := rr.Body.String(); strings.Contains(body, `"extension":null`) || strings.Contains(body, `"notify":null`) {
		t.Errorf("a seam list marshalled as null: %s", body)
	}

	// And what a tenant must keep: this is what the UI decides what to draw
	// from, and a control that vanished for a tenant would be the dead control
	// #178's criterion forbids.
	if got.Edition != "business" {
		t.Errorf("edition = %q, want the reporter's", got.Edition)
	}
	if !got.Features[feature.SSO] {
		t.Error("the reporter granted sso and the narrowed document does not say so")
	}
	if len(got.Features) != len(feature.All()) {
		t.Errorf("features = %v, want exactly feature.All()'s keys", got.Features)
	}
}

// The document's key set is feature.All()'s, not the reporter's. A UI hiding a
// control on features["sso"] has to tell false from absent, and a reporter that
// dropped a key would make the control vanish rather than grey out — while one
// that invented a key would put an undocumented name on the wire.
func TestCapabilitiesKeysComeFromTheSeamNotTheReporter(t *testing.T) {
	expires := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	h := discoveryServer(t, Options{Features: reporter{report: feature.Report{
		Edition: "business",
		// One real key, one this repository has never heard of, and three
		// missing.
		Features: feature.Set{feature.SSO: true, feature.Name("bogus"): true},
		Licence:  &feature.Licence{Tier: "be", Status: feature.StatusGrace, ExpiresAt: &expires},
	}}})

	got := decode[capabilityDocument](t, do(t, h, "GET", "/api/v1/capabilities"))

	if _, invented := got.Features[feature.Name("bogus")]; invented {
		t.Error("a key the reporter invented reached the document")
	}
	if len(got.Features) != len(feature.All()) {
		t.Errorf("features = %v, want exactly feature.All()'s keys", got.Features)
	}
	if !got.Features[feature.SSO] {
		t.Error("the reporter granted sso and the document does not say so")
	}
	if got.Features[feature.Projects] {
		t.Error("a key the reporter never mentioned came back granted")
	}

	// The badge's three fields, including the one D25 exists for: grace is not
	// valid, and a badge that could not say so could not say "expired, stops
	// working tomorrow".
	if got.Licence == nil {
		t.Fatal("licence = null, want the reporter's")
	}
	if got.Licence.Status != feature.StatusGrace || got.Licence.Tier != "be" {
		t.Errorf("licence = %+v, want the reporter's tier and status", *got.Licence)
	}
	if got.Licence.ExpiresAt == nil || !got.Licence.ExpiresAt.Equal(expires) {
		t.Errorf("licence.expiresAt = %v, want %v", got.Licence.ExpiresAt, expires)
	}
}
