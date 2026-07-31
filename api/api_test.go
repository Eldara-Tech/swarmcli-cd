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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
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
	h := s.Handler()
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
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/applications/edge/sync", nil).WithContext(ctx))
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

// openRoutes are the patterns that are deliberately not behind the guard. One,
// and it is argued for where it is registered: a container healthcheck runs
// beside the process and cannot carry a credential.
//
// Anything else added to Handler() is guarded or this test fails, which is the
// whole point of deriving the list rather than writing it down.
var openRoutes = map[string]bool{"GET /healthz": true}

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
func registeredRoutes(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing this package: %v", err)
	}

	var out []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
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
				out = append(out, pattern)
				return true
			})
		}
	}

	// A parse that found nothing, or found something other than the routes, is
	// a broken test rather than a passing one.
	if len(out) == 0 {
		t.Fatal("no routes found in this package's source; the registrations moved and this test stopped checking anything")
	}
	for pattern := range openRoutes {
		if !slices.Contains(out, pattern) {
			t.Fatalf("routes %v do not include the exempted %q; either it is gone or these are not the registrations", out, pattern)
		}
	}
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
	for _, pattern := range registeredRoutes(t) {
		if openRoutes[pattern] {
			continue
		}
		paths = append(paths, request(pattern))
	}
	if len(paths) == 0 {
		t.Fatal("every registered route is exempted; this test checks nothing")
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

// Reading and syncing are different actions, and an application-scoped request
// carries its application so a companion's RBAC can scope on it.
func TestAuthorizerIsAskedTheRightQuestion(t *testing.T) {
	a := &allowAll{}
	_, h := testServer(t, &fakeReconciler{views: []application.View{view("edge")}}, a)

	do(t, h, "GET", "/api/v1/applications")
	do(t, h, "GET", "/api/v1/applications/edge")
	do(t, h, "POST", "/api/v1/applications/edge/sync")

	want := []string{"read:", "read:edge", "sync:edge"}
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

func TestEventStreamDeliversWhatTheNotifierIsGiven(t *testing.T) {
	s, h := testServer(t, &fakeReconciler{}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

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
	s, _ := testServer(t, &fakeReconciler{}, nil)
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(unflushable{rr}, httptest.NewRequest("GET", "/api/v1/events", nil))
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
