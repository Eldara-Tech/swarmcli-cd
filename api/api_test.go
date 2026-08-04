// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/extension"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
)

// --- fakes ---

type fakeReconciler struct {
	mu       sync.Mutex
	views    []application.View
	diffs    []application.ReleaseDiff
	diffErr  error
	history  application.History
	histErr  error
	syncErr  error
	synced   []string
	histCall int
	// acceptErr fails the reservation, and pending makes it report that a sync
	// is already queued.
	acceptErr error
	pending   bool
}

func (f *fakeReconciler) Views() []application.View { return f.views }

func (f *fakeReconciler) View(app string) (application.View, bool) {
	for _, v := range f.views {
		if v.Spec.Name == app {
			return v, true
		}
	}
	return application.View{}, false
}

func (f *fakeReconciler) Diffs(string) ([]application.ReleaseDiff, error) {
	return f.diffs, f.diffErr
}

func (f *fakeReconciler) History(context.Context, string) (application.History, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.histCall++
	return f.history, f.histErr
}

func (f *fakeReconciler) SyncNow(_ context.Context, app string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synced = append(f.synced, app)
	return f.syncErr
}

// AcceptSync is the reservation half: it decides whether a sync is started at
// all, which is what the handler answers with, and returns the work to run
// detached.
func (f *fakeReconciler) AcceptSync(app string) (func(context.Context) error, error) {
	// Membership first, as the real one does: it resolves the entry before it
	// reserves anything, so an unknown application never reaches the queue.
	if _, ok := f.View(app); !ok {
		return nil, errors.New("no such application")
	}
	f.mu.Lock()
	accept, pending := f.acceptErr, f.pending
	f.mu.Unlock()
	if pending {
		return nil, application.ErrSyncPending
	}
	if accept != nil {
		return nil, accept
	}
	return func(ctx context.Context) error { return f.SyncNow(ctx, app) }, nil
}

func (f *fakeReconciler) syncedApps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.synced...)
}

// allowAll authorises everything and records what it was asked.
type allowAll struct {
	mu    sync.Mutex
	calls []string
}

func (a *allowAll) Ready() error { return nil }

func (a *allowAll) Authenticate(*http.Request) (authz.Subject, error) {
	return authz.Subject{Name: "tester"}, nil
}

func (a *allowAll) Authorize(_ context.Context, _ authz.Subject, act authz.Action, app string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, string(act)+":"+app)
	return nil
}

func (a *allowAll) Visible(_ context.Context, _ authz.Subject, _ authz.Action, apps []string) ([]string, error) {
	return apps, nil
}

type denyAuthn struct{ authz.Authorizer }

func (denyAuthn) Authenticate(*http.Request) (authz.Subject, error) {
	return authz.Subject{}, errors.New("no token")
}

// projectAuthorizer is the shape a companion implementing projects has: one
// subject, one application, and nothing said about the rest. It is what the
// seam could express and the API could not ask.
//
// Both its methods refuse a subject that is not the one Authenticate returned,
// so a guard that dropped the subject on the way to a handler fails these tests
// as a 403 rather than passing them by accident.
type projectAuthorizer struct{ visible string }

func (projectAuthorizer) Ready() error { return nil }

func (projectAuthorizer) Authenticate(*http.Request) (authz.Subject, error) {
	return authz.Subject{Name: "tenant", Groups: []string{"project-a"}}, nil
}

func (p projectAuthorizer) Authorize(_ context.Context, s authz.Subject, _ authz.Action, app string) error {
	if s.Name != "tenant" {
		return errors.New("the subject the guard authenticated did not reach the decision")
	}
	// An empty application is the unscoped question the guard asks for a
	// collection endpoint, which this tenant is allowed to ask.
	if app == "" || app == p.visible {
		return nil
	}
	return errors.New("not yours")
}

func (p projectAuthorizer) Visible(_ context.Context, s authz.Subject, _ authz.Action, apps []string) ([]string, error) {
	if s.Name != "tenant" {
		return nil, errors.New("the subject the guard authenticated did not reach the decision")
	}
	var out []string
	for _, app := range apps {
		if app == p.visible {
			out = append(out, app)
		}
	}
	return out, nil
}

// refuseVisible authenticates and authorises the endpoint but cannot answer the
// per-member question — a policy engine that did not respond.
type refuseVisible struct{ *allowAll }

func (refuseVisible) Visible(context.Context, authz.Subject, authz.Action, []string) ([]string, error) {
	return nil, errors.New("policy engine unavailable")
}

type denyAuthz struct{ authz.Authorizer }

func (denyAuthz) Authenticate(*http.Request) (authz.Subject, error) {
	return authz.Subject{Name: "tester"}, nil
}

func (denyAuthz) Authorize(context.Context, authz.Subject, authz.Action, string) error {
	return errors.New("not yours")
}

func view(name string) application.View {
	return application.View{
		Spec: application.Spec{Name: name},
		Status: application.Status{
			Sync:   application.Sync{State: application.SyncSynced, Revision: strings.Repeat("a", 40)},
			Health: application.Health{State: application.HealthHealthy, Services: application.ServiceCounts{Healthy: 2, Total: 2}},
			Releases: []application.ReleaseStatus{{
				Name: "whoami", Chart: "whoami", Version: "0.1.8",
				Services: []application.ServiceStatus{{Name: "whoami", Running: 2, Desired: 2}},
			}},
		},
	}
}

func testServer(t *testing.T, rec Reconciler, a authz.Authorizer) (*Server, http.Handler) {
	t.Helper()
	if a == nil {
		a = &allowAll{}
	}
	s := New(rec, Options{Authorizer: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	// Run a triggered sync inline, so a test asserts on what happened rather
	// than on when a goroutine got round to it.
	s.syncing = func(_ string, run func(context.Context)) { run(context.Background()) }
	h, err := s.Handler()
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}
	return s, h
}

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
	return rr
}

func decode[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %s: %v", rr.Body.String(), err)
	}
	return out
}

// --- tests ---

// The acceptance rule from #16: the list view is one request and contains
// everything it renders — and nothing it does not. Twenty applications must not
// mean twenty release tables.
func TestListIsOneRequestAndStripsReleaseDetail(t *testing.T) {
	rec := &fakeReconciler{views: []application.View{view("edge"), view("prod")}}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	got := decode[struct {
		Applications []application.View `json:"applications"`
	}](t, rr)
	if len(got.Applications) != 2 {
		t.Fatalf("got %d applications, want 2", len(got.Applications))
	}
	for _, v := range got.Applications {
		if v.Status.Releases != nil {
			t.Errorf("%s carried release detail into the list view", v.Spec.Name)
		}
		// Everything the row renders is still there.
		if v.Status.Sync.State == "" || v.Status.Health.State == "" || v.Status.Sync.Revision == "" {
			t.Errorf("%s is missing something the list row renders: %+v", v.Spec.Name, v.Status)
		}
		if v.Status.Health.Services.Total == 0 {
			t.Errorf("%s lost its service counts", v.Spec.Name)
		}
	}

	// Stripping must not have mutated what the reconciler holds.
	if rec.views[0].Status.Releases == nil {
		t.Error("the list handler emptied the reconciler's own status")
	}
}

func TestDetailCarriesReleasesAndServices(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{views: []application.View{view("edge")}}, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	got := decode[application.View](t, rr)
	if len(got.Status.Releases) != 1 || len(got.Status.Releases[0].Services) != 1 {
		t.Errorf("detail = %+v, want releases and their services", got.Status)
	}
}

func TestUnknownApplicationIs404(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{}, nil)
	for _, path := range []string{
		"/api/v1/applications/absent",
		"/api/v1/applications/absent/sync",
	} {
		method := "GET"
		if strings.HasSuffix(path, "/sync") {
			method = "POST"
		}
		if rr := do(t, h, method, path); rr.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", method, path, rr.Code)
		}
	}
}

// An application that has not been reconciled yet is not an error and not a
// 404: it exists, and there is simply nothing to diff. A UI renders an empty
// panel, not a failure.
func TestDiffBeforeTheFirstReconcile(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{diffErr: application.ErrNotPlanned}, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/diff")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	got := decode[struct {
		Releases []application.ReleaseDiff `json:"releases"`
		Planned  bool                      `json:"planned"`
	}](t, rr)
	if got.Planned {
		t.Error("planned = true before anything was planned")
	}
	if got.Releases == nil {
		t.Error("releases = null, want an empty list a UI can range over")
	}
}

func TestDiffServesTheManifestChange(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{diffs: []application.ReleaseDiff{
		{Release: "whoami", Action: application.ActionUpgrade, Diff: "-replicas: 1\n+replicas: 3\n"},
	}}, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/diff")
	got := decode[struct {
		Releases []application.ReleaseDiff `json:"releases"`
		Planned  bool                      `json:"planned"`
	}](t, rr)
	if !got.Planned || len(got.Releases) != 1 || !strings.Contains(got.Releases[0].Diff, "replicas") {
		t.Errorf("diff = %+v", got)
	}
}

// One request for the whole history screen, however many releases it covers.
func TestHistoryIsOneRequestForEveryRelease(t *testing.T) {
	rec := &fakeReconciler{history: application.History{Releases: []application.ReleaseHistory{
		{Name: "whoami", Revisions: []application.Revision{{Revision: 2}, {Revision: 1}}},
		{Name: "redis", Revisions: []application.Revision{{Revision: 1}}},
	}}}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/history")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	got := decode[application.History](t, rr)
	if len(got.Releases) != 2 {
		t.Fatalf("got %d releases, want both in one response", len(got.Releases))
	}
	if rec.histCall != 1 {
		t.Errorf("made %d calls, want one request to serve the screen", rec.histCall)
	}
}

// History reads the swarm, so it can fail in ways the cached views cannot. That
// is a 502 rather than a 404 or a 500: the request was fine and this controller
// is fine, the swarm did not answer.
func TestHistoryFailureIsABadGateway(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{histErr: errors.New("swarm unreachable")}, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/history")
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "swarm unreachable") {
		t.Error("the daemon's own error text was echoed to an API client")
	}
}

// The sync button returns at once and the work continues behind it.
func TestSyncIsAcceptedNotAwaited(t *testing.T) {
	rec := &fakeReconciler{views: []application.View{view("edge")}}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "POST", "/api/v1/applications/edge/sync")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	got := decode[struct {
		Application string `json:"application"`
		Accepted    bool   `json:"accepted"`
	}](t, rr)
	if got.Application != "edge" || !got.Accepted {
		t.Errorf("body = %+v", got)
	}
	if apps := rec.syncedApps(); len(apps) != 1 || apps[0] != "edge" {
		t.Errorf("synced %v, want edge", apps)
	}
}

// A sync that fails still returns 202: the request to start one succeeded, and
// the outcome arrives on the event stream and the status endpoint. Reporting it
// on the response would mean waiting for it, which is what 202 exists to avoid.
func TestSyncFailureDoesNotChangeTheResponse(t *testing.T) {
	rec := &fakeReconciler{views: []application.View{view("edge")}, syncErr: errors.New("swarm said no")}
	_, h := testServer(t, rec, nil)

	if rr := do(t, h, "POST", "/api/v1/applications/edge/sync"); rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rr.Code)
	}
}

// The sync must outlive the request that asked for it. A request context is
// cancelled the moment the response is written, so a handler that passed it
// through would cancel every sync instantly — and it would look like a
// controller ignoring the button.
func TestTriggeredSyncOutlivesTheRequest(t *testing.T) {
	got := make(chan context.Context, 1)
	rec := &ctxCapturingReconciler{
		fakeReconciler: &fakeReconciler{views: []application.View{view("edge")}},
		capture:        func(c context.Context) { got <- c },
	}
	// Deliberately not the inline syncing hook the other tests use: this one is
	// about what the real detach hands over.
	s := New(rec, Options{Authorizer: &allowAll{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	h, err := s.Handler()
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/applications/edge/sync", nil).WithContext(ctx))
	// What net/http does once the response is written.
	cancel()

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	select {
	case c := <-got:
		if c.Err() != nil {
			t.Errorf("the detached sync got a cancelled context (%v); it inherited the request's", c.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the detached sync never ran")
	}
}

// registration is one route this package registers on the mux, beside whether
// the registration put it behind the guard.
type registration struct {
	pattern string
	guarded bool
}

// registeredRoutes reads the routes Handler() registers out of this package's
// own source, so that adding one to the mux adds it to this test too.
//
// A hardcoded list cannot do that. It is the list that stays as it was while
// Handler() grows, and a route missing from it is a route serving the docker
// socket to anyone who can reach the port — the one failure this package exists
// to prevent, passing CI green.
//
// net/http will not answer the question directly. ServeMux keeps its patterns
// unexported and offers no way to walk them; Handler(r) names the pattern that
// matched a request, which is a request the caller already had to know how to
// build. The alternatives were exporting a route table from api.go purely so a
// test could read it, or writing the list twice. Reading the registrations costs
// neither and fails closed in both directions: a call this cannot see is a route
// that is not registered, and a source tree it cannot parse fails outright below
// rather than silently yielding nothing to check.
//
// Whether a route is guarded is read from the same registration, and not from a
// list of exemptions kept beside it. An exemption list is the thing that stops
// being true: three public routes would have meant three hand-written entries,
// each of which is a place to write "public" about something that should not
// have been. The handler argument cannot lie about it — guard is what
// authenticates a request, so a registration that does not call it is a route
// nothing authenticates, whatever anything else says.
func registeredRoutes(t *testing.T) []registration {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing this package: %v", err)
	}

	var out []registration
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				// Any x.Handle / x.HandleFunc, not just one named receiver: a
				// second mux, or a route registered from another file in this
				// package, is exactly what must not slip past.
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				pattern, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				out = append(out, registration{pattern: pattern, guarded: isGuardCall(call.Args[1])})
				return true
			})
		}
	}

	// A parse that found nothing, or found something other than the routes, is
	// a broken test rather than a passing one.
	if len(out) == 0 {
		t.Fatal("no routes found in this package's source; the registrations moved and this test stopped checking anything")
	}
	// And a scan that recognised no guard at all would report every route as
	// public, which every check below would then agree with.
	if !slices.ContainsFunc(out, func(r registration) bool { return r.guarded }) {
		t.Fatalf("no registration in %v was recognised as guarded; guard was renamed or the registrations changed shape", out)
	}
	return out
}

// isGuardCall reports whether a handler argument is a call to guard.
//
// That call is the whole of what authentication and authorisation are here, so
// it is the only thing "this route is guarded" can mean. A handler wrapped in
// anything else — a func literal, a method value, an extension's handler — is
// served to whoever reaches the port.
func isGuardCall(arg ast.Expr) bool {
	call, ok := arg.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr: // s.guard(…)
		return fun.Sel.Name == "guard"
	case *ast.Ident: // guard(…), were it ever to stop being a method
		return fun.Name == "guard"
	}
	return false
}

// declaredPublic is every pattern coreRoutes says is served with no credential.
// It is what Routes() reports to the startup log, and so what an operator reads.
func declaredPublic() []string {
	var out []string
	for _, rt := range coreRoutes {
		if rt.Public {
			out = append(out, rt.Pattern)
		}
	}
	slices.Sort(out)
	return out
}

// wildcard fills a pattern's {app} — or any other wildcard a later route
// introduces — with the application the fakes hold, so that a request reaches
// the guard rather than the router's own 404.
var wildcard = regexp.MustCompile(`\{[^}]*\}`)

// route is one request to send at the router.
type route struct{ method, path string }

// request turns a registered pattern into the request to send at it.
func request(pattern string) route {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		// A pattern with no method matches every method; GET will do.
		return route{"GET", wildcard.ReplaceAllString(pattern, "edge")}
	}
	return route{method, wildcard.ReplaceAllString(path, "edge")}
}

// The controller holds write access to the swarm, so an unauthenticated API is
// a root shell. Nothing behind the guard runs until both checks pass.
//
// The routes come from Handler()'s own registrations rather than from a list
// kept beside them, so a route added without a guard fails here instead of
// shipping.
func TestEveryApiEndpointIsGuarded(t *testing.T) {
	var paths []route
	for _, rt := range registeredRoutes(t) {
		if !rt.guarded {
			continue
		}
		paths = append(paths, request(rt.pattern))
	}
	if len(paths) == 0 {
		t.Fatal("no registered route is guarded; this test checks nothing")
	}

	t.Run("authentication", func(t *testing.T) {
		rec := &fakeReconciler{views: []application.View{view("edge")}}
		_, h := testServer(t, rec, denyAuthn{})
		for _, p := range paths {
			if rr := do(t, h, p.method, p.path); rr.Code != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401", p.method, p.path, rr.Code)
			}
		}
		if got := rec.syncedApps(); len(got) != 0 || rec.histCall != 0 {
			t.Error("a rejected request still reached the reconciler")
		}
	})

	t.Run("authorization", func(t *testing.T) {
		rec := &fakeReconciler{views: []application.View{view("edge")}}
		_, h := testServer(t, rec, denyAuthz{})
		for _, p := range paths {
			if rr := do(t, h, p.method, p.path); rr.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403", p.method, p.path, rr.Code)
			}
		}
		if got := rec.syncedApps(); len(got) != 0 || rec.histCall != 0 {
			t.Error("a rejected request still reached the reconciler")
		}
	})
}

// The routes registered without the guard are exactly the ones coreRoutes
// declares public — read from the registrations themselves, not from a list of
// exemptions somebody has to remember to keep true.
//
// Both directions catch a different mistake, and both are silent today.
// A route registered unguarded without being declared public is an
// unauthenticated endpoint on a process holding the docker socket, and it is
// also invisible to the WARN the startup log exists to print. A route declared
// public while actually registered behind the guard makes Routes() lie to that
// same log in the other direction, and makes TestEveryApiEndpointIsGuarded skip
// a route it should have been testing.
func TestTheUnguardedRoutesAreExactlyTheDeclaredPublicOnes(t *testing.T) {
	var unguarded []string
	for _, rt := range registeredRoutes(t) {
		if !rt.guarded {
			unguarded = append(unguarded, rt.pattern)
		}
	}
	slices.Sort(unguarded)

	if want := declaredPublic(); !slices.Equal(unguarded, want) {
		t.Errorf("registered with no guard: %v\ndeclared Public in coreRoutes: %v", unguarded, want)
	}
}

// Every endpoint asks its own question, and an application-scoped request
// carries its application so a companion's RBAC can scope on it.
//
// Diff and history are not `read`. A list row is a state and a revision; a diff
// is the rendered manifest and a history is a walk of the swarm's stored
// revisions, and an authorizer implementing projects has a reason to grant the
// first and not the others. While every route passed ActionRead it could not say
// so, and the constants are additive, so the distinction costs nothing to make.
func TestAuthorizerIsAskedTheRightQuestion(t *testing.T) {
	a := &allowAll{}
	_, h := testServer(t, &fakeReconciler{views: []application.View{view("edge")}}, a)

	do(t, h, "GET", "/api/v1/applications")
	do(t, h, "GET", "/api/v1/applications/edge")
	do(t, h, "GET", "/api/v1/applications/edge/diff")
	do(t, h, "GET", "/api/v1/applications/edge/history")
	do(t, h, "POST", "/api/v1/applications/edge/sync")

	want := []string{"read:", "read:edge", "diff:edge", "history:edge", "sync:edge"}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) != len(want) {
		t.Fatalf("asked %v, want %v", a.calls, want)
	}
	for i := range want {
		if a.calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, a.calls[i], want[i])
		}
	}
}

// The list is a collection, and authorising a collection once is a decision
// about the collection rather than about its members: it can only allow or deny
// the whole thing. A tenant with read access to one application therefore
// enumerated every application's name, repository URL, revision and error text,
// which is the disclosure Visible exists to close.
func TestTheListIsNarrowedToWhatTheSubjectMaySee(t *testing.T) {
	views := []application.View{view("edge"), view("prod")}

	_, narrowed := testServer(t, &fakeReconciler{views: views}, projectAuthorizer{visible: "edge"})
	rr := do(t, narrowed, "GET", "/api/v1/applications")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "prod") {
		t.Errorf("body %q names an application the subject may not see", rr.Body.String())
	}
	got := decode[struct {
		Applications []application.View `json:"applications"`
	}](t, rr)
	if len(got.Applications) != 1 || got.Applications[0].Spec.Name != "edge" {
		t.Fatalf("list = %+v, want only edge", got.Applications)
	}

	// An authorizer with nothing to narrow — which is the free build's, and the
	// zero-configuration answer — still returns everything. Narrowing must be
	// something an authorizer does, not something the API does to it.
	_, whole := testServer(t, &fakeReconciler{views: views}, nil)
	got = decode[struct {
		Applications []application.View `json:"applications"`
	}](t, do(t, whole, "GET", "/api/v1/applications"))
	if len(got.Applications) != 2 {
		t.Fatalf("got %d applications, want both: an authorizer that narrows nothing narrows nothing", len(got.Applications))
	}
}

// An authorizer that cannot answer the per-member question must not be read as
// permitting every member. Visible's error is a 403, like Authorize's.
func TestAListTheAuthorizerCannotNarrowIsRefused(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{views: []application.View{view("edge"), view("prod")}},
		refuseVisible{&allowAll{}})

	rr := do(t, h, "GET", "/api/v1/applications")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "edge") || strings.Contains(rr.Body.String(), "prod") {
		t.Errorf("body %q served the list it could not narrow", rr.Body.String())
	}
}

// The event stream is the same collection arriving live, and it had the same
// hole: the endpoint was authorised once and then published every
// application's events to whoever was attached.
//
// Each event is about one application, so each is authorised as it arrives. The
// one the subject may not see is published first, so a stream that carried it is
// caught by the very next read rather than by a timeout.
func TestTheEventStreamOnlyCarriesWhatTheSubjectMaySee(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, projectAuthorizer{visible: "edge"})
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Wait for the subscription rather than sleeping on it.
	for range 200 {
		if s.events.count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.events.count() != 1 {
		t.Fatal("the request never subscribed")
	}

	for _, app := range []string{"prod", "edge"} {
		s.Notify(context.Background(), notify.Event{
			Application: app,
			Type:        notify.SyncSucceeded,
			Revision:    "abc123",
			At:          time.Unix(0, 0).UTC(),
		})
	}

	// Read off the goroutine and bound the wait. A filter that dropped both
	// events would otherwise leave this blocked on a stream nothing will ever
	// write to, and the failure would be the test binary's own timeout minutes
	// later rather than this assertion.
	type frame struct {
		text string
		err  error
	}
	frames := make(chan frame, 1)
	go func() {
		buf := make([]byte, 512)
		n, err := resp.Body.Read(buf)
		frames <- frame{string(buf[:n]), err}
	}()

	var got frame
	select {
	case got = <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived: the one event the subject may see was filtered out too")
	}
	if got.err != nil && got.err != io.EOF {
		t.Fatalf("reading the stream: %v", got.err)
	}
	if strings.Contains(got.text, "prod") {
		t.Errorf("frame %q carries an application the subject may not see", got.text)
	}
	if !strings.Contains(got.text, `"application":"edge"`) {
		t.Errorf("frame %q lost the one event the subject may see", got.text)
	}
}

// A container healthcheck runs beside the process and cannot carry a
// credential without putting one in the stack file, so this one endpoint is
// open — and says nothing beyond "something is listening".
func TestHealthzIsOpenAndSaysNothing(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{views: []application.View{view("edge")}}, denyAuthn{})

	rr := do(t, h, "GET", "/healthz")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a credential", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "edge") {
		t.Errorf("body %q discloses something about the deployment", rr.Body.String())
	}
}

// uiServer is a router whose UI routes serve a stand-in for the web package's
// handler, so that "the UI is what answered, and with what path" is something a
// test can see. api never imports web; this is the whole of what it knows.
func uiServer(t *testing.T) http.Handler {
	t.Helper()
	s := New(&fakeReconciler{views: []application.View{view("edge")}}, Options{
		Authorizer: &allowAll{},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		UI: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "ui:"+r.URL.Path)
		}),
	})
	h, err := s.Handler()
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}
	return h
}

// Everything the mux did not claim is the UI, because everything the mux did
// not claim is a route belonging to the router in the browser.
func TestTheUiServesTheRootAndTheAssets(t *testing.T) {
	h := uiServer(t)

	for _, path := range []string{"/", "/applications", "/applications/edge", "/assets/app-a1b2c3.js"} {
		rr := do(t, h, "GET", path)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rr.Code)
		}
		if got, want := rr.Body.String(), "ui:"+path; got != want {
			t.Errorf("GET %s served %q, want %q", path, got, want)
		}
	}
}

// A GET pattern matches HEAD as well, so the UI handler sees two methods where
// the route table names one.
func TestTheUiIsAlsoReachedByHead(t *testing.T) {
	if rr := do(t, uiServer(t), "HEAD", "/"); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// GET / matches every path no more specific pattern claimed, so without the
// prefix check in root a mistyped endpoint would answer 200 text/html and
// become a parse error a long way from the mistake. The API answers about the
// API, in the shape everything else in it answers.
func TestAnUnknownApiPathIsAJsonNotFound(t *testing.T) {
	h := uiServer(t)

	// The last is the same path with an escaped 'a': the check is on the
	// decoded path, so a spelling cannot walk around it.
	for _, path := range []string{"/api/", "/api/v1/typo", "/api/v1/applications/edge/typo", "/%61pi/v1/typo"} {
		rr := do(t, h, "GET", path)
		if rr.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: body %q", path, rr.Code, rr.Body.String())
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q, want JSON", path, ct)
		}
		if got := decode[map[string]string](t, rr); got["error"] == "" {
			t.Errorf("GET %s answered %q, which carries no error message", path, rr.Body.String())
		}
	}
}

// Two things the SPA fallback costs, recorded here rather than discovered
// later. ServeMux answers a request that matched no pattern by reporting the
// methods that would have matched, and with GET / registered every path now
// matches under GET.
//
// Accepted rather than solved. Additionally registering a methodless /api/
// pattern would turn the first case into a JSON 404 and would not recover the
// second, since a pattern matching every method takes the request either way.
func TestTheFallbackChangesWhatAWrongMethodAnswers(t *testing.T) {
	h := uiServer(t)

	rr := do(t, h, "POST", "/api/v1/typo")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/typo = %d, want 405", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, want the methods GET / registered", allow)
	}

	// And a wrong method on a route that does exist is the JSON 404 above
	// rather than the 405 naming POST that it used to be.
	rr = do(t, h, "GET", "/api/v1/applications/edge/sync")
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET …/sync = %d, want 404", rr.Code)
	}
}

// Three test files in this repository construct api.Options{}, and every
// deployment run with --ui=false does the same thing. A nil handler would panic
// on the first request a browser made.
func TestWithNoUiTheRoutesStillAnswer(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{}, nil)

	for _, path := range []string{"/", "/assets/app-a1b2c3.js"} {
		if rr := do(t, h, "GET", path); rr.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rr.Code)
		}
	}
}

// The Reconciler interface at the top of api.go exists so that something other
// than the OSS applier can serve these endpoints, and it only does that if this
// package compiles against nothing but the interface. An import of reconcile —
// even for one sentinel error, which is what this was — makes it decorative:
// any replacement would still have had to drag in go-git, the chart engine and
// the moby client to answer a request. Both sentinels live in application.
//
// Derived from the source rather than written down, for the reason
// registeredRoutes gives: a list kept beside the code is the list that stops
// being true.
func TestTheApiDoesNotImportTheReconciler(t *testing.T) {
	const forbidden = "github.com/Eldara-Tech/swarmcli-cd/reconcile"

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing this package: %v", err)
	}

	parsed := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			parsed++
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: unquoting %s: %v", name, imp.Path.Value, err)
				}
				if path == forbidden {
					t.Errorf("%s imports %s; the Reconciler interface is there so that it does not have to", name, forbidden)
				}
			}
		}
	}
	// A parse that read nothing passes vacuously, which is the one way this
	// test could stop checking anything without saying so.
	if parsed == 0 {
		t.Fatal("no source files parsed; this test stopped checking anything")
	}
}

// --- event stream ---

// The h2 row is the cheap companion to D23's drain test. A TLS listener
// negotiates HTTP/2 whether or not anyone intended it — Server.ServeTLS calls
// setupHTTP2_ServeTLS — and the event stream is the only handler whose
// behaviour differs under it, since h2 does its own framing and flow control
// over one multiplexed connection. A stream that buffered rather than delivered
// under h2 would reach a browser as a UI that never updates.
func TestEventStreamDeliversWhatTheNotifierIsGiven(t *testing.T) {
	for _, tc := range []struct {
		name      string
		h2        bool
		wantProto string
	}{
		{"HTTP/1.1", false, "HTTP/1.1"},
		{"HTTP/2 over TLS", true, "HTTP/2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, h := testServer(t, &fakeReconciler{}, nil)
			srv := httptest.NewUnstartedServer(h)
			if tc.h2 {
				srv.EnableHTTP2 = true
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()

			req, err := http.NewRequest("GET", srv.URL+"/api/v1/events", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// srv.Client() trusts the server's own certificate, so this is one
			// client for both rows rather than a TLS branch here.
			resp, err := srv.Client().Do(req.WithContext(ctx))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.Proto != tc.wantProto {
				t.Fatalf("protocol = %s, want %s; the row proves nothing otherwise", resp.Proto, tc.wantProto)
			}
			if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
				t.Errorf("content type = %q, want text/event-stream", got)
			}

			// Wait for the subscription rather than sleeping on it.
			for range 200 {
				if s.events.count() > 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if s.events.count() != 1 {
				t.Fatal("the request never subscribed")
			}

			s.Notify(context.Background(), notify.Event{
				Application: "edge",
				Type:        notify.SyncSucceeded,
				Revision:    "abc123",
				At:          time.Unix(0, 0).UTC(),
			})

			buf := make([]byte, 512)
			n, err := resp.Body.Read(buf)
			if err != nil && err != io.EOF {
				t.Fatalf("reading the stream: %v", err)
			}
			frame := string(buf[:n])
			// The event name is what an EventSource listener binds to.
			if !strings.Contains(frame, "event: sync-succeeded") {
				t.Errorf("frame %q carries no event name", frame)
			}
			if !strings.Contains(frame, `"application":"edge"`) || !strings.Contains(frame, `"revision":"abc123"`) {
				t.Errorf("frame %q lost the payload", frame)
			}
		})
	}
}

// The reconciler stamps every event it raises with the application's
// destination (#131), and until #142 the wire shape dropped it: a consumer was
// told which application had synced and never where, which is the one question
// a multi-swarm UI exists to answer.
//
// Asserted over the decoded frame rather than over wire, because the frame is
// what a consumer parses — a field that exists in Go and never reaches the JSON
// is exactly the gap being closed here, and a test reading the struct would
// have passed throughout it. The empty case asserts the key is *present*, for
// the reason the tag carries no omitempty; see wire.
func TestTheStreamCarriesTheEventsDestination(t *testing.T) {
	for _, tc := range []struct {
		name  string
		swarm string
	}{
		{"a named destination", "production"},
		{"the swarm the controller runs in", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := firstEventFrame(t, notify.Event{
				Application: "edge",
				Type:        notify.SyncSucceeded,
				Swarm:       tc.swarm,
				At:          time.Unix(0, 0).UTC(),
			})
			got, ok := data["swarm"]
			if !ok {
				t.Fatalf("frame %v carries no swarm key", data)
			}
			if got != tc.swarm {
				t.Errorf("swarm = %v, want %q", got, tc.swarm)
			}
		})
	}
}

// firstEventFrame raises e on a running server's event stream and returns the
// `data` object of the frame a subscriber receives, decoded into a map.
//
// A map rather than wire: decoding into the type under test would let a renamed
// or dropped JSON tag round-trip cleanly and prove nothing about what a client
// written against the documented shape actually finds.
func firstEventFrame(t *testing.T, e notify.Event) map[string]any {
	t.Helper()

	s, h := testServer(t, &fakeReconciler{}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Wait for the subscription rather than sleeping on it.
	for range 200 {
		if s.events.count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.events.count() != 1 {
		t.Fatal("the request never subscribed")
	}
	s.Notify(context.Background(), e)

	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("reading the stream: %v", err)
	}
	frame := string(buf[:n])
	_, payload, ok := strings.Cut(frame, "data: ")
	if !ok {
		t.Fatalf("frame %q has no data line", frame)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &out); err != nil {
		t.Fatalf("decoding %q: %v", payload, err)
	}
	return out
}

// A browser that stopped reading must never be able to stall a reconcile.
// notify.Notifier's contract is that it does not block, so a subscriber that
// falls behind loses events rather than applying back-pressure — the status
// endpoint is authoritative and a client that missed some re-reads it.
func TestASlowSubscriberIsDroppedFromNotBlocking(t *testing.T) {
	s := New(&fakeReconciler{}, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Authorizer: &allowAll{}})
	id, ch := s.events.subscribe()
	defer s.events.unsubscribe(id)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range subscriberBuffer * 3 {
			s.Notify(context.Background(), notify.Event{Application: "edge", Type: notify.DriftDetected})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked on a subscriber that was not reading")
	}
	if len(ch) != subscriberBuffer {
		t.Errorf("buffered %d events, want the buffer full at %d and the rest dropped", len(ch), subscriberBuffer)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	s := New(&fakeReconciler{}, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Authorizer: &allowAll{}})
	id, _ := s.events.subscribe()
	if s.events.count() != 1 {
		t.Fatalf("count = %d, want 1", s.events.count())
	}
	s.events.unsubscribe(id)
	if s.events.count() != 0 {
		t.Errorf("count = %d, want 0", s.events.count())
	}
	// Unsubscribing twice must not panic on a closed channel.
	s.events.unsubscribe(id)
	s.Notify(context.Background(), notify.Event{Application: "edge", Type: notify.DriftDetected})
}

// The zero Options must produce a usable server: the authorizer falls back to
// whatever the seam has registered, which is how the controller wires it.
func TestZeroOptionsIsUsable(t *testing.T) {
	s := New(&fakeReconciler{}, Options{})
	if s.authz == nil || s.log == nil || s.events == nil || s.syncing == nil {
		t.Errorf("New left something nil: %+v", s)
	}
}

// A read that fails for any reason other than "not reconciled yet" is a 404:
// the only way Diffs errors is an application it does not know.
func TestDiffForAnUnknownApplication(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{diffErr: errors.New("no such application")}, nil)
	if rr := do(t, h, "GET", "/api/v1/applications/absent/diff"); rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHistoryBeforeTheFirstReconcile(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{histErr: application.ErrNotPlanned}, nil)

	rr := do(t, h, "GET", "/api/v1/applications/edge/history")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	got := decode[application.History](t, rr)
	if got.Releases == nil {
		t.Error("releases = null, want an empty list a UI can range over")
	}
}

// A response writer that cannot flush would produce a stream that silently
// never arrives, which is worse than refusing.
func TestStreamRefusesAWriterThatCannotFlush(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, nil)
	rr := httptest.NewRecorder()

	h.ServeHTTP(unflushable{rr}, httptest.NewRequest("GET", "/api/v1/events", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if s.events.count() != 0 {
		t.Error("it subscribed anyway")
	}
}

// unflushable hides httptest.ResponseRecorder's Flush method without hiding the
// rest of it.
type unflushable struct{ rec *httptest.ResponseRecorder }

func (u unflushable) Header() http.Header         { return u.rec.Header() }
func (u unflushable) Write(b []byte) (int, error) { return u.rec.Write(b) }
func (u unflushable) WriteHeader(code int)        { u.rec.WriteHeader(code) }

// ctxCapturingReconciler reports the context a triggered sync was actually
// handed, which is the whole point of detaching it from the request.
type ctxCapturingReconciler struct {
	*fakeReconciler
	capture func(context.Context)
}

func (c *ctxCapturingReconciler) AcceptSync(app string) (func(context.Context) error, error) {
	return func(ctx context.Context) error {
		c.capture(ctx)
		return c.fakeReconciler.SyncNow(ctx, app)
	}, nil
}

// fakeController is what the app-set loop is to this package: one call
// reporting where the set came from and how loading it went.
type fakeController struct{ status application.ControllerStatus }

func (f *fakeController) Status() application.ControllerStatus { return f.status }

func TestStatusServesTheControllerState(t *testing.T) {
	want := application.ControllerStatus{
		AppSet: application.AppSetStatus{
			Mode:     "git",
			Revision: strings.Repeat("b", 40),
			LoadedAt: time.Date(2026, 7, 27, 9, 12, 4, 0, time.UTC),
			Error:    `applications[1]: duplicate application name "edge"`,
			Stale:    true,
			Orphaned: []string{"legacy-api"},
		},
		Applications: 2,
	}

	s, h := testServer(t, &fakeReconciler{views: []application.View{view("edge"), view("core")}}, nil)
	s.controller = &fakeController{status: want}

	rr := do(t, h, "GET", "/api/v1/status")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/status = %d, want 200", rr.Code)
	}
	got := decode[application.ControllerStatus](t, rr)
	if got.AppSet.Mode != want.AppSet.Mode || got.AppSet.Revision != want.AppSet.Revision {
		t.Errorf("app set = %+v, want %+v", got.AppSet, want.AppSet)
	}
	if !got.AppSet.LoadedAt.Equal(want.AppSet.LoadedAt) {
		t.Errorf("loadedAt = %v, want %v", got.AppSet.LoadedAt, want.AppSet.LoadedAt)
	}
	if !got.AppSet.Stale || got.AppSet.Error != want.AppSet.Error {
		t.Errorf("stale=%v error=%q, want the refusal reported", got.AppSet.Stale, got.AppSet.Error)
	}
	if len(got.AppSet.Orphaned) != 1 || got.AppSet.Orphaned[0] != "legacy-api" {
		t.Errorf("orphaned = %v, want [legacy-api]", got.AppSet.Orphaned)
	}
	if got.Applications != 2 {
		t.Errorf("applications = %d, want 2", got.Applications)
	}
}

// A status endpoint that fails when no app-set source is wired is one a monitor
// cannot tell from a dead controller, so it answers with what it does know.
func TestStatusWithoutAControllerStillAnswers(t *testing.T) {
	_, h := testServer(t, &fakeReconciler{views: []application.View{view("edge")}}, nil)

	rr := do(t, h, "GET", "/api/v1/status")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/status = %d, want 200", rr.Code)
	}
	got := decode[application.ControllerStatus](t, rr)
	if got.Applications != 1 || got.AppSet.Mode != "" {
		t.Errorf("got %+v, want the application count and no app-set mode", got)
	}
}

// TestShutdownEndsTheEventStreams is why Drain exists.
//
// http.Server.Shutdown waits for connections to go idle and does not cancel
// in-flight request contexts, and an event stream never goes idle — so every
// shutdown with a UI attached spent the whole timeout achieving nothing and then
// logged that the API had not shut down cleanly. Swarm sends SIGKILL ten seconds
// after SIGTERM, so that was half the budget spent on a false alarm.
func TestShutdownEndsTheEventStreams(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, nil)

	httpSrv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	httpSrv.RegisterOnShutdown(s.Drain)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = httpSrv.Serve(ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	for range 200 {
		if s.events.count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.events.count() != 1 {
		t.Fatal("the request never subscribed")
	}

	// A generous timeout that a correct shutdown never comes close to, so the
	// assertion is about promptness rather than about the number.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := httpSrv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil: the stream should have been ended, not waited out", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Shutdown took %s: the event stream was waited out rather than ended", took)
	}
}

// A request that slips past the listener while the streams are being ended must
// not subscribe to a feed nothing will publish to, and then hold the drain open
// waiting for it.
func TestAStreamOpenedDuringShutdownEndsAtOnce(t *testing.T) {
	s, _ := testServer(t, &fakeReconciler{}, nil)
	s.Drain()

	id, events := s.events.subscribe()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("a stream opened during shutdown received an event, want a closed channel")
		}
	case <-time.After(time.Second):
		// Not a closed channel and not an event: the handler would sit here
		// until its request context ended, holding the drain open — which is
		// the whole thing being prevented.
		t.Fatal("a stream opened during shutdown was left waiting on a feed nothing will publish to")
	}
	if s.events.count() != 0 {
		t.Fatalf("count = %d, want 0: a late subscriber must not be registered", s.events.count())
	}
	s.events.unsubscribe(id) // must not double-close
}

// A sync requested while one is running with another already queued is
// coalesced onto that queued one. Still 202, because the state the caller asked
// to have reconciled will be — by the sync already waiting, which has not read
// the repository yet — but the response says which of the two happened rather
// than reporting a sync it did not start.
//
// The reservation is made in the request and not in the detached goroutine for
// exactly this reason: by the time that goroutine runs, the response has gone.
func TestASyncQueuedBehindAnotherIsReportedAsCoalesced(t *testing.T) {
	rec := &fakeReconciler{views: []application.View{{Spec: application.Spec{Name: "edge"}}}, pending: true}
	_, h := testServer(t, rec, nil)

	rr := do(t, h, "POST", "/api/v1/applications/edge/sync")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: the request is honoured by the sync already queued", rr.Code)
	}

	var body struct {
		Accepted  bool `json:"accepted"`
		Coalesced bool `json:"coalesced"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if !body.Accepted || !body.Coalesced {
		t.Fatalf("body = %+v, want accepted and coalesced", body)
	}
	if got := rec.syncedApps(); len(got) != 0 {
		t.Fatalf("ran %v, want nothing: a coalesced request must not start a second sync", got)
	}
}

// --- the extension seam ---

// fakeExtension is a companion's contribution: whatever routes it was built
// with. The seam is a declaration, so a fake needs no behaviour beyond
// returning what it was handed.
type fakeExtension struct{ routes, public []extension.Route }

func (f fakeExtension) Routes() []extension.Route       { return f.routes }
func (f fakeExtension) PublicRoutes() []extension.Route { return f.public }

// withExtensions installs a set of extensions for the duration of one test, in
// the style of run_test.go's swapAuthorizer.
//
// It swaps what Handler reads rather than calling extension.Register, and it has
// to: the seam is a seam.List, a List appends and has no removal, and half the
// tests below declare a colliding or malformed route on purpose. Registering one
// of those into the process-wide list would leave it registered, and every later
// call to Handler in this binary would fail on the collision a finished test
// installed. TestTheSeamIsWhatHandlerReads is what keeps this from testing a
// variable no companion can reach.
func withExtensions(t *testing.T, exts ...loaded) {
	t.Helper()
	original := activeExtensions
	t.Cleanup(func() { activeExtensions = original })
	activeExtensions = func() []loaded { return exts }
}

// extRoute is one declared route, kept short because every test below writes
// several.
func extRoute(pattern string, act authz.Action, h extension.Handler) extension.Route {
	return extension.Route{Pattern: pattern, Action: act, Handler: h}
}

// recordSubject is a handler that reports the subject it was given, which is
// the thing a context lookup would have got wrong.
func recordSubject(got *authz.Subject) extension.Handler {
	return func(w http.ResponseWriter, _ *http.Request, subject authz.Subject) {
		*got = subject
		w.WriteHeader(http.StatusOK)
	}
}

func okHandler(w http.ResponseWriter, _ *http.Request, _ authz.Subject) { w.WriteHeader(http.StatusOK) }

// refuses builds a router over the given extensions and returns the message
// Handler refused with. A router that built at all is the failure: the check has
// to happen before anything reaches the mux, so there is nothing to serve.
func refuses(t *testing.T, exts ...loaded) string {
	t.Helper()
	withExtensions(t, exts...)

	s := New(&fakeReconciler{}, Options{Authorizer: &allowAll{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	h, err := s.Handler()
	if err == nil {
		t.Fatal("Handler built a router for a set it had to refuse")
	}
	if h != nil {
		t.Errorf("Handler refused and still returned a router (%T); nothing must be servable", h)
	}
	if got := s.Routes(); len(got) != 0 {
		t.Errorf("Routes = %v after a refusal, want nothing: no route was registered", got)
	}
	return err.Error()
}

// names asserts that a refusal message identifies the offender. A refusal an
// operator cannot act on is barely better than the panic it replaced.
func names(t *testing.T, msg string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("error %q does not name %q", msg, w)
		}
	}
}

// The guarantee registeredRoutes structurally cannot give any more: it reads
// this package's source, and an extension route is registered from a pattern
// held in a slice, in a private module the scan cannot see. So the same two
// questions TestEveryApiEndpointIsGuarded asks are asked again here at runtime,
// against a route that came in through the seam.
//
// The core, not the companion, decides what wraps a declared handler. That is
// the entire security argument for a table rather than a mux, and this is where
// it is checked.
func TestAnExtensionRouteIsGuarded(t *testing.T) {
	reached := false
	install := func(t *testing.T) {
		t.Helper()
		reached = false
		withExtensions(t, loaded{name: "projects", ext: fakeExtension{routes: []extension.Route{
			extRoute("GET /api/v1/projects", authz.ActionRead, func(w http.ResponseWriter, _ *http.Request, _ authz.Subject) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}),
		}}})
	}

	t.Run("authentication", func(t *testing.T) {
		install(t)
		_, h := testServer(t, &fakeReconciler{}, denyAuthn{})
		if rr := do(t, h, "GET", "/api/v1/projects"); rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rr.Code)
		}
		if reached {
			t.Error("an unauthenticated request reached the extension's handler")
		}
	})

	t.Run("authorization", func(t *testing.T) {
		install(t)
		_, h := testServer(t, &fakeReconciler{}, denyAuthz{})
		if rr := do(t, h, "GET", "/api/v1/projects"); rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rr.Code)
		}
		if reached {
			t.Error("an unauthorised request reached the extension's handler")
		}
	})
}

// The route's own action, not read. authz.Action is a string type with additive
// constants precisely so an extension can declare one of its own, and an
// authorizer that was asked "read" about an endpoint the companion called
// "projects" was asked the wrong question — one it may well answer yes to.
//
// The application scope comes from the path the same way it does for a core
// route, so a companion's RBAC can scope on it without the core knowing what the
// wildcard means.
func TestAnExtensionRouteIsAuthorisedWithItsOwnAction(t *testing.T) {
	withExtensions(t, loaded{name: "projects", ext: fakeExtension{routes: []extension.Route{
		extRoute("GET /api/v1/projects/{app}", authz.Action("projects"), okHandler),
	}}})

	a := &allowAll{}
	_, h := testServer(t, &fakeReconciler{}, a)
	if rr := do(t, h, "GET", "/api/v1/projects/edge"); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	want := []string{"projects:edge"}
	if !slices.Equal(a.calls, want) {
		t.Errorf("asked %v, want %v", a.calls, want)
	}
}

// The subject the guard authenticated has to arrive at the handler, or a
// companion's own second decision — the one the core cannot make for it —
// is made about nobody.
//
// projectAuthorizer refuses any subject Authenticate did not return, so a zero
// Subject reaching the handler shows up as a 403 rather than as a passing test.
func TestTheSubjectReachesAnExtensionHandler(t *testing.T) {
	var got authz.Subject
	withExtensions(t, loaded{name: "projects", ext: fakeExtension{routes: []extension.Route{
		extRoute("GET /api/v1/projects", authz.ActionRead, recordSubject(&got)),
	}}})

	_, h := testServer(t, &fakeReconciler{}, projectAuthorizer{visible: "edge"})
	if rr := do(t, h, "GET", "/api/v1/projects"); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got.Name != "tenant" || !slices.Equal(got.Groups, []string{"project-a"}) {
		t.Errorf("handler got %+v, want the subject Authenticate returned", got)
	}
}

// The opt-out works, and only where it was asked for. One extension, two routes,
// one credential-less request each: the public one answers and the guarded one
// does not.
func TestAPublicRouteNeedsNoCredential(t *testing.T) {
	withExtensions(t, loaded{name: "sso", ext: fakeExtension{
		routes: []extension.Route{extRoute("GET /api/v1/projects", authz.ActionRead, okHandler)},
		public: []extension.Route{{Pattern: "GET /sso/callback", Handler: okHandler}},
	}})

	_, h := testServer(t, &fakeReconciler{}, denyAuthn{})
	if rr := do(t, h, "GET", "/sso/callback"); rr.Code != http.StatusOK {
		t.Errorf("public route = %d, want 200: nothing authenticates it", rr.Code)
	}
	if rr := do(t, h, "GET", "/api/v1/projects"); rr.Code != http.StatusUnauthorized {
		t.Errorf("guarded route = %d, want 401: declaring one route public must not open the other", rr.Code)
	}
}

// Nothing authenticated the request, so there is no subject and the handler is
// told so plainly. An authorizer handed a zero Subject fails closed, which is
// the right direction and the wrong failure — it reads as a caller with no
// permissions rather than as a route that was never going to have one.
func TestAPublicHandlerGetsTheZeroSubject(t *testing.T) {
	var got authz.Subject
	withExtensions(t, loaded{name: "sso", ext: fakeExtension{
		public: []extension.Route{{Pattern: "GET /sso/callback", Handler: recordSubject(&got)}},
	}})

	// allowAll would have produced a named subject for a guarded route, so a
	// non-zero one here means the public path went through the guard.
	_, h := testServer(t, &fakeReconciler{}, &allowAll{})
	if rr := do(t, h, "GET", "/sso/callback"); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	// DeepEqual against the zero value rather than a field-by-field check, so
	// that a field added to Subject later is covered by this without anyone
	// having to remember it.
	if !reflect.DeepEqual(got, authz.Subject{}) {
		t.Errorf("handler got %+v, want the zero Subject", got)
	}
}

// A companion re-registering a path the core already serves is the likeliest
// collision there is, and the one with teeth: net/http's last registration does
// not win, it panics, and either outcome would have a private module deciding
// what /api/v1/status means.
func TestARouteCollidingWithACoreRouteIsRefused(t *testing.T) {
	msg := refuses(t, loaded{name: "projects", ext: fakeExtension{routes: []extension.Route{
		extRoute("GET /api/v1/status", authz.ActionRead, okHandler),
	}}})
	names(t, msg, "projects", "GET /api/v1/status", "core route")
}

func TestTwoExtensionsCollidingAreRefused(t *testing.T) {
	msg := refuses(t,
		loaded{name: "sso", ext: fakeExtension{routes: []extension.Route{
			extRoute("GET /api/v1/projects", authz.ActionRead, okHandler),
		}}},
		loaded{name: "projects", ext: fakeExtension{routes: []extension.Route{
			extRoute("GET /api/v1/projects", authz.ActionRead, okHandler),
		}}},
	)
	// Both, because an operator with two companions loaded has to know which
	// pair to take up with whom.
	names(t, msg, "sso", "projects", "GET /api/v1/projects")
}

// The check is over one flat set, so an extension colliding with itself is
// caught by the same pass. It would otherwise reach the mux and panic there,
// which is a crash rather than a refusal to start.
func TestAnExtensionCollidingWithItselfIsRefused(t *testing.T) {
	msg := refuses(t, loaded{name: "sso", ext: fakeExtension{routes: []extension.Route{
		extRoute("GET /login", authz.ActionRead, okHandler),
		extRoute("GET /login", authz.ActionRead, okHandler),
	}}})
	names(t, msg, "sso", "GET /login", "twice")
}

// The dangerous member of that flat set. The same pattern in both methods would
// be registered twice, and which registration won would decide whether the
// endpoint was authenticated — a security property settled by the order of two
// loops.
func TestAPatternDeclaredBothGuardedAndPublicIsRefused(t *testing.T) {
	msg := refuses(t, loaded{name: "sso", ext: fakeExtension{
		routes: []extension.Route{extRoute("GET /sso/callback", authz.ActionRead, okHandler)},
		public: []extension.Route{{Pattern: "GET /sso/callback", Handler: okHandler}},
	}})
	names(t, msg, "sso", "GET /sso/callback", "guarded", "public")
}

// There is no action that is obviously right for a route this repository has
// never seen, and defaulting to one would be guessing at a permission on a
// process holding the docker socket.
func TestAGuardedRouteWithNoActionIsRefused(t *testing.T) {
	msg := refuses(t, loaded{name: "projects", ext: fakeExtension{routes: []extension.Route{
		{Pattern: "GET /api/v1/projects", Handler: okHandler},
	}}})
	names(t, msg, "projects", "GET /api/v1/projects", "no action")
}

// The contradiction, and the reason it is worth refusing rather than ignoring:
// the likeliest author of it believed the action would be enforced, and on a
// route nothing authenticates it cannot be.
func TestAPublicRouteWithAnActionIsRefused(t *testing.T) {
	msg := refuses(t, loaded{name: "sso", ext: fakeExtension{
		public: []extension.Route{extRoute("GET /sso/callback", authz.ActionSync, okHandler)},
	}})
	names(t, msg, "sso", "GET /sso/callback", "sync")
}

// A nil handler is the one malformed field that would otherwise reach an
// operator as an outage — a 500 on the first request that touched it — rather
// than as a controller that would not start.
func TestARouteWithNoHandlerIsRefused(t *testing.T) {
	msg := refuses(t, loaded{name: "projects", ext: fakeExtension{routes: []extension.Route{
		{Pattern: "GET /api/v1/projects", Action: authz.ActionRead},
	}}})
	names(t, msg, "projects", "GET /api/v1/projects", "no handler")
}

// The OSS-build no-op guarantee: with nothing registered the mux is exactly
// today's. The extension list is empty in every build of this repository, so
// this is the only case CI ever exercises end to end and it must be provably a
// no-op rather than merely believed to be one.
//
// It doubles as the check that coreRoutes has not drifted from the registrations
// themselves. registeredRoutes reads the mux.Handle literals out of this
// package's source; coreRoutes is what the collision check is seeded with and
// what Routes reports. If those two ever disagree, the collision check has a
// hole in it exactly the size of the disagreement, and this is where that shows
// up.
func TestZeroExtensionsChangesNothing(t *testing.T) {
	withExtensions(t)

	s, h := testServer(t, &fakeReconciler{views: []application.View{view("edge")}}, nil)

	// What the source registers, and whether it registered it behind the guard:
	// Routes() is what the startup log reads, so its Public has to be the
	// registration's rather than an intention recorded next to it.
	var want []string
	unguarded := make(map[string]bool)
	for _, rt := range registeredRoutes(t) {
		want = append(want, rt.pattern)
		unguarded[rt.pattern] = !rt.guarded
	}

	var got []string
	for _, rt := range s.Routes() {
		if rt.Extension != "" {
			t.Errorf("route %q names extension %q with nothing registered", rt.Pattern, rt.Extension)
		}
		if rt.Public != unguarded[rt.Pattern] {
			t.Errorf("route %q reports Public = %v, which is not what its registration does", rt.Pattern, rt.Public)
		}
		got = append(got, rt.Pattern)
	}

	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Routes reports %v, but this package's source registers %v", got, want)
	}

	// And the router still answers, which a list of names cannot show.
	if rr := do(t, h, "GET", "/api/v1/applications/edge"); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if rr := do(t, h, "GET", "/api/v1/projects"); rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: nothing registered that", rr.Code)
	}
}

// What the startup log is built from. The pattern alone is not enough: a WARN
// saying an unauthenticated endpoint exists, without saying which module added
// it, leaves an operator with two companions loaded no way to act on it.
func TestRoutesNamesEveryRegisteredRoute(t *testing.T) {
	withExtensions(t,
		loaded{name: "sso", ext: fakeExtension{
			routes: []extension.Route{extRoute("GET /sso/whoami", authz.ActionRead, okHandler)},
			public: []extension.Route{{Pattern: "GET /sso/callback", Handler: okHandler}},
		}},
		loaded{name: "projects", ext: fakeExtension{
			routes: []extension.Route{extRoute("GET /api/v1/projects", authz.Action("projects"), okHandler)},
		}},
	)

	s, _ := testServer(t, &fakeReconciler{}, nil)
	got := s.Routes()

	// The core's first, then every guarded route in registration order, then
	// every public one — the order Handler registers them in.
	want := append(append([]RegisteredRoute(nil), coreRoutes...),
		RegisteredRoute{Pattern: "GET /sso/whoami", Extension: "sso"},
		RegisteredRoute{Pattern: "GET /api/v1/projects", Extension: "projects"},
		RegisteredRoute{Pattern: "GET /sso/callback", Extension: "sso", Public: true},
	)
	if !slices.Equal(got, want) {
		t.Errorf("Routes =\n%+v\nwant\n%+v", got, want)
	}

	// The copy is a copy: a caller ranging over the result to build a log line
	// cannot edit what the server thinks it registered.
	got[0].Pattern = "GET /tampered"
	if s.Routes()[0].Pattern == "GET /tampered" {
		t.Error("mutating the result of Routes changed the server's own list")
	}
}

// armed is an extension that contributes its routes only while the test that
// registered it is running.
//
// extension.Register appends to a package-level seam with no removal, so a route
// left registered here would still be registered for every test that ran
// afterwards — and for a second run of this one under -count, where it would
// collide with itself and fail every Handler call in the binary.
type armed struct {
	mu     sync.Mutex
	on     bool
	routes []extension.Route
}

func (a *armed) Routes() []extension.Route {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.on {
		return nil
	}
	return a.routes
}

func (*armed) PublicRoutes() []extension.Route { return nil }

func (a *armed) set(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.on = on
}

// The one test that goes through extension.Register itself.
//
// Every other test here installs its set by swapping activeExtensions, for the
// reason withExtensions gives, and that indirection is only worth anything if
// what it stands in for is the seam a companion can actually reach. Without
// this, all of them could pass over a variable no init() anywhere writes to.
func TestTheSeamIsWhatHandlerReads(t *testing.T) {
	e := &armed{routes: []extension.Route{
		extRoute("GET /api/v1/seam-check", authz.ActionRead, okHandler),
	}}
	e.set(true)
	t.Cleanup(func() { e.set(false) })
	extension.Register("seam-check", e)

	s, h := testServer(t, &fakeReconciler{}, nil)
	if rr := do(t, h, "GET", "/api/v1/seam-check"); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a route registered through extension.Register was not served", rr.Code)
	}
	// And under the name it registered with, which is what the WARN line for a
	// public route depends on.
	if !slices.Contains(s.Routes(), RegisteredRoute{Pattern: "GET /api/v1/seam-check", Extension: "seam-check"}) {
		t.Errorf("Routes = %+v, want the seam's own registration named", s.Routes())
	}
}
