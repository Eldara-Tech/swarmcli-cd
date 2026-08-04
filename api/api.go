// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package api serves the controller's HTTP interface.
//
// Per D1 it is designed UI-first: the API comes first, a TUI view second and a
// web UI third, and it has to already be the shape those need. That is a
// constraint on the endpoint set rather than a slogan — every screen a UI has
// is one request, and every action a user can take is one endpoint:
//
//	GET  /healthz                                unauthenticated liveness
//	GET  /ui/bootstrap.json                      what the login screen needs,
//	                                             before anyone is authenticated
//	GET  /api/v1/status                          the controller's own state
//	GET  /api/v1/applications                    the list view
//	GET  /api/v1/applications/{app}              the detail view
//	GET  /api/v1/applications/{app}/diff         the diff view
//	GET  /api/v1/applications/{app}/history      the history view
//	POST /api/v1/applications/{app}/sync         the sync button
//	GET  /api/v1/events                          live updates, so nothing polls
//	GET  /api/v1/capabilities                    what this build is and grants
//	GET  /                                       the web UI, and the fallback
//	                                             for its client-side routes
//	GET  /assets/{path...}                       the UI's hashed build output
//
// The last two serve whatever the caller passed as Options.UI, and this package
// neither embeds nor imports it — see package web.
//
// Applications are read-only: they are declared in the app set, which is either
// mounted at deploy time or committed to git, and changing them means changing
// that file rather than posting to this API. The paths are nouns so that CRUD
// can be added later without any of them moving.
//
// That set is the core's. A companion module adds routes of its own through the
// extension seam, and everything Handler registers — core route or companion's —
// is served behind guard, with the authz.Action the route declares, unless the
// companion declared it public on purpose. See docs/extensibility.md.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/extension"
	"github.com/Eldara-Tech/swarmcli-cd/feature"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
)

// Reconciler is what the API serves. *reconcile.Reconciler implements it.
//
// This package does not import reconcile, and the two sentinels the handlers
// below match on live in application for that reason. An alternative reconciler
// is the whole point of stating this as an interface, and one that had to import
// the OSS applier — go-git, the chart engine, the moby client — for two error
// values would have had no way to take it up.
type Reconciler interface {
	Views() []application.View
	View(app string) (application.View, bool)
	Diffs(app string) ([]application.ReleaseDiff, error)
	History(ctx context.Context, app string) (application.History, error)
	// AcceptSync rather than SyncNow: the handler has to know whether a sync was
	// started before it writes the response, and the sync itself outlives the
	// request. See sync below.
	AcceptSync(app string) (func(context.Context) error, error)
}

// Controller reports the controller's own state, as distinct from the
// applications'. *appset.Loop implements it.
type Controller interface {
	Status() application.ControllerStatus
}

// Server is the HTTP interface. It is also a notify.Notifier: the event stream
// is fed by the same seam that feeds the log, which is why notify appends
// rather than replaces — a companion adding Slack must not silently kill the
// UI's live updates.
type Server struct {
	rec        Reconciler
	controller Controller
	authz      authz.Authorizer
	features   feature.Reporter
	version    string
	log        *slog.Logger
	// ui serves the browser UI. Never nil; see Options.UI.
	ui     http.Handler
	events *stream
	// syncing runs a sync detached from the request that asked for it.
	// Overridable in tests, which otherwise have to race a goroutine.
	syncing func(app string, run func(context.Context))
	// routes is what Handler registered, kept so that the caller can log it.
	// Empty until Handler has succeeded; see Routes.
	routes []RegisteredRoute
}

// Options tune a Server. Every field has a working default.
type Options struct {
	Authorizer authz.Authorizer
	Log        *slog.Logger
	// Controller reports where the app set came from and whether it is loading.
	// Absent, the status endpoint still answers — with the application count and
	// an empty app-set mode, which is what "no app-set source is wired" looks
	// like. A status endpoint that 404s is a status endpoint a monitor cannot
	// tell from a dead controller.
	Controller Controller
	// UI is what the two public UI routes serve; web.Handler is what production
	// passes. Absent — a build run with --ui=false, and every test that
	// constructs a zero Options — they answer 404, because the routes are
	// registered either way and only the response differs. New defaults it for
	// the same reason Handler refuses a nil extension handler: the alternative
	// is a panic on the first request that reaches it.
	UI http.Handler
	// Version is the build's own version, reported by the capability document.
	// The controller passes the string goreleaser and the Dockerfile stamp.
	// Absent, the document reports an empty string rather than inventing a
	// number: a caller reading "" knows nobody stamped one, and a caller
	// reading "unknown" has to be told what that means.
	Version string
	// Features reports what this build grants, for the capability document.
	// Absent, the seam in force answers — which is the only thing production
	// ever wants, and is a field only so that a test can install a report
	// without registering one process-wide.
	//
	// Read once here because the seam is a Slot and a Slot is settled by the
	// end of init(). What settles is who answers: Report is still called per
	// request, so a licence that lapses stops being reported without a restart.
	Features feature.Reporter
}

// New returns a Server over rec.
//
// It does not register itself as a notifier. The caller does that, so that the
// notifier list is not appended to as a side effect of constructing a server —
// which in a test suite means one stream per test, all still subscribed.
func New(rec Reconciler, o Options) *Server {
	if o.Authorizer == nil {
		o.Authorizer = authz.Get()
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.UI == nil {
		o.UI = http.NotFoundHandler()
	}
	if o.Features == nil {
		o.Features = feature.Get()
	}
	s := &Server{rec: rec, controller: o.Controller, authz: o.Authorizer, features: o.Features, version: o.Version, log: o.Log, ui: o.UI, events: newStream(o.Log)}
	s.syncing = s.detach
	return s
}

// coreRoutes is every route this package registers itself: the pattern, and
// whether it is served without a credential.
//
// It is the seed of the collision check and the core half of what Routes
// reports, in one place so that the two cannot answer differently. What it
// deliberately is not is the thing Handler ranges over to register: the
// guarantee that every core route is guarded comes from api_test.go's
// registeredRoutes, which reads this package's own source and only sees a
// mux.Handle whose first argument is a string literal. A loop over this slice
// would hand it a selector instead, and the scan would go quiet rather than
// fail — the one way that test could stop checking anything without saying so.
// So the literals below stay, and TestZeroExtensionsChangesNothing compares
// them against this list: drift between the two is a test failure rather than a
// collision check with a hole in it.
var coreRoutes = []RegisteredRoute{
	{Pattern: "GET /healthz", Public: true},
	// Public because a login screen has to know whether to draw a token box or
	// an SSO button before anyone has a credential to authenticate with. Under
	// /ui/ rather than /api/v1/ because docs/api.md states that every
	// /api/v1/… endpoint requires the admin token, and that invariant is worth
	// more than the tidiness of one path: a public route under the API prefix
	// would make an operator check each one before believing it.
	{Pattern: "GET /ui/bootstrap.json", Public: true},
	{Pattern: "GET /api/v1/status"},
	{Pattern: "GET /api/v1/applications"},
	{Pattern: "GET /api/v1/applications/{app}"},
	{Pattern: "GET /api/v1/applications/{app}/diff"},
	{Pattern: "GET /api/v1/applications/{app}/history"},
	{Pattern: "POST /api/v1/applications/{app}/sync"},
	{Pattern: "GET /api/v1/events"},
	{Pattern: "GET /api/v1/capabilities"},
	// Public because a browser has no credential until the login screen it is
	// asking for has loaded. What they serve is the build's own bytes, which
	// disclose nothing about the swarm.
	{Pattern: "GET /", Public: true},
	{Pattern: "GET /assets/{path...}", Public: true},
}

// Handler returns the router.
//
// This is where the extension seam is read, and it is read exactly once. A
// seam.List is not settled at the end of init() and a consumer may not snapshot
// it at construction — but nothing joins this one after the mux exists, so the
// moment the mux is built is the moment the answer stops changing, and reading
// it here is the consumer-side half of that rule rather than an exception to it.
//
// The error is a refusal to start. Everything a companion declared is checked
// before a single route reaches the mux, because both alternatives reach an
// operator as an outage rather than as a controller that would not start:
// net/http panics on a duplicate pattern, deep inside wiring, and a nil handler
// would panic on the first request that hit it.
func (s *Server) Handler() (http.Handler, error) {
	guardedRoutes, publicRoutes, err := extensionRoutes()
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()

	// Unauthenticated, and deliberately says nothing: a container healthcheck
	// runs beside the process and cannot carry a credential without putting one
	// in the stack file and in `docker inspect` output. What it discloses is
	// that something is listening, which a TCP connect already tells you.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	// Unauthenticated for the reason coreRoutes gives, and it discloses only
	// how one may authenticate — which a caller staring at the login screen is
	// about to be told anyway.
	mux.HandleFunc("GET /ui/bootstrap.json", s.bootstrap)

	mux.Handle("GET /api/v1/status", s.guard(authz.ActionRead, s.status))
	mux.Handle("GET /api/v1/applications", s.guard(authz.ActionRead, s.list))
	mux.Handle("GET /api/v1/applications/{app}", s.guard(authz.ActionRead, s.detail))
	mux.Handle("GET /api/v1/applications/{app}/diff", s.guard(authz.ActionDiff, s.diff))
	mux.Handle("GET /api/v1/applications/{app}/history", s.guard(authz.ActionHistory, s.history))
	mux.Handle("POST /api/v1/applications/{app}/sync", s.guard(authz.ActionSync, s.sync))
	mux.Handle("GET /api/v1/events", s.guard(authz.ActionRead, s.stream))
	mux.Handle("GET /api/v1/capabilities", s.guard(authz.ActionRead, s.capabilities))

	// The UI, and the hashed build output the document it serves asks for.
	// Registered whether or not anything was embedded and whether or not --ui
	// is on, so that coreRoutes stays a static list of literals and no
	// extension can ever claim GET /.
	mux.HandleFunc("GET /", s.root)
	mux.HandleFunc("GET /assets/{path...}", s.ui.ServeHTTP)

	routes := append([]RegisteredRoute(nil), coreRoutes...)

	// The core decides what wraps a companion's handler, which is the whole
	// reason a route is declared here rather than registered by whoever owns it:
	// guard is not something an extension can forget or opt out of by leaving a
	// field unset. The conversion is exact — extension.Handler and guarded have
	// identical underlying types, deliberately, so that the two shapes are
	// provably the same without extension having to import this package.
	for _, rt := range guardedRoutes {
		mux.Handle(rt.route.Pattern, s.guard(rt.route.Action, guarded(rt.route.Handler)))
		routes = append(routes, RegisteredRoute{Pattern: rt.route.Pattern, Extension: rt.name})
	}
	for _, rt := range publicRoutes {
		// rt is a fresh variable per iteration since Go 1.22, so each closure
		// captures its own route. Said out loud because the bug it used to be is
		// silent: every public pattern would serve the last handler declared,
		// and an SSO callback answering with another module's handler is not a
		// failure that shows up as an error anywhere.
		mux.Handle(rt.route.Pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The zero Subject, because nothing authenticated this request.
			// extension.Handler says the same from the other side: a public
			// handler must not consult it, since an authorizer handed one fails
			// closed and the refusal would look like a caller with no
			// permissions rather than like the route being public.
			rt.route.Handler(w, r, authz.Subject{})
		}))
		routes = append(routes, RegisteredRoute{Pattern: rt.route.Pattern, Extension: rt.name, Public: true})
	}

	s.routes = routes
	return mux, nil
}

// RegisteredRoute is one route the server serves, and who put it there.
type RegisteredRoute struct {
	Pattern   string // "GET /api/v1/projects"
	Extension string // the name it registered under; empty for a core route
	Public    bool   // served with no authentication
}

// Routes reports every route Handler registered — the core's first, then each
// extension's in registration order — and is meaningful only once Handler has
// succeeded, because until the seam has been read the set is not decided.
//
// It exists for the startup log rather than for the router. An operator's only
// signal that a companion added an unauthenticated endpoint to a process
// holding the docker socket is a line naming the pattern and the module that
// added it, and a companion's source is not something the operator running the
// binary necessarily has.
//
// The caller logs the guarded ones as a list and warns, one line each, on the
// public routes that carry an extension name. Not on a public core route: this
// repository argued for that one in its own source and no deployment can change
// it, and a WARN on every startup about something nobody can act on is how an
// operator learns to ignore the WARN that matters.
func (s *Server) Routes() []RegisteredRoute {
	return append([]RegisteredRoute(nil), s.routes...)
}

// loaded is one registered extension beside the name it registered under.
type loaded struct {
	name string
	ext  extension.Extension
}

// activeExtensions is the seam, read as one list of name-and-implementation
// pairs.
//
// A variable rather than a call to extension.All written where it is used, for
// the reason Server.syncing is one: a test has to be able to install a set. The
// seam is a seam.List, a List appends and has no removal, and the tests that
// matter most here declare a colliding pattern on purpose — registering that
// into the process-wide list would make every later call to Handler in the same
// binary fail on a collision a finished test installed. Nothing in production
// assigns this. TestTheSeamIsWhatHandlerReads is what keeps the indirection
// honest, by going through extension.Register itself.
var activeExtensions = func() []loaded {
	// Two reads rather than one, because the seam reports names and
	// implementations separately and pairing them by index is what Active
	// documents. The bound is for the one case that cannot arise from an init()
	// but is cheap to survive: a registration landing between the two calls.
	names, all := extension.Active(), extension.All()
	out := make([]loaded, len(all))
	for i, e := range all {
		out[i].ext = e
		if i < len(names) {
			out[i].name = names[i]
		}
	}
	return out
}

// declared is one route an extension asked for, beside the name it registered
// under — which a collision message and the startup log both need and which the
// Route itself does not carry.
type declared struct {
	name  string
	route extension.Route
}

// claimant is what holds a pattern: an index into the extension list, or
// coreOwner for one of this package's own.
//
// An index rather than a name because names need not be unique — notify permits
// the same — so "this extension declared it twice" and "another extension that
// happens to share its name declared it" are different sentences, and only the
// index tells them apart.
type claimant struct {
	ext    int
	public bool
}

const coreOwner = -1

// extensionRoutes reads the seam and returns what it declared, split by whether
// the core will guard it, or an error naming the extension that made the set
// unservable.
//
// Every check runs before Handler touches the mux and the first failure returns,
// so a set with one bad route registers none of them. Half a companion's routes
// serving while the other half did not is the worst of the available failures:
// the controller would be up, the operator would have no reason to look, and
// which half survived would be a matter of declaration order.
func extensionRoutes() (guardedRoutes, publicRoutes []declared, err error) {
	exts := activeExtensions()

	seen := make(map[string]claimant, len(coreRoutes))
	for _, rt := range coreRoutes {
		seen[rt.Pattern] = claimant{ext: coreOwner}
	}

	// claim records a pattern or explains who already had it.
	//
	// The comparison is exact-string, and that is not the question ServeMux
	// asks: two patterns can conflict without being equal — "GET /a/{x}"
	// against "GET /a/b" — and such a pair passes here, reaches the mux and
	// still panics there. Accepted rather than solved. This catches the
	// overwhelmingly likely case, a companion re-registering a path the core
	// already serves, and reimplementing net/http's pattern.conflictsWith in
	// this repository would be a second copy of standard-library logic that
	// drifts from the first.
	claim := func(i int, rt extension.Route, public bool) error {
		held, taken := seen[rt.Pattern]
		switch {
		case !taken:
			seen[rt.Pattern] = claimant{ext: i, public: public}
			return nil
		case held.ext == coreOwner:
			return fmt.Errorf("extension '%s' declares route '%s', which is a core route", exts[i].name, rt.Pattern)
		case held.ext == i && held.public != public:
			// The dangerous one. Registering both would put the same pattern on
			// the mux twice, and which registration won would decide whether the
			// endpoint was authenticated at all.
			return fmt.Errorf("extension '%s' declares route '%s' as both a guarded and a public route", exts[i].name, rt.Pattern)
		case held.ext == i:
			return fmt.Errorf("extension '%s' declares route '%s' twice", exts[i].name, rt.Pattern)
		default:
			return fmt.Errorf("extension '%s' declares route '%s', which extension '%s' already declared", exts[i].name, rt.Pattern, exts[held.ext].name)
		}
	}

	for i, e := range exts {
		for _, rt := range e.ext.Routes() {
			if rt.Handler == nil {
				// Every other malformed field is caught here; a nil handler left
				// to the mux would panic on the first request that reached it.
				return nil, nil, fmt.Errorf("extension '%s' declares route '%s' with no handler", e.name, rt.Pattern)
			}
			if rt.Action == "" {
				return nil, nil, fmt.Errorf("extension '%s' declares route '%s' with no action: a guarded route names the authz.Action the core authorises it with, and there is none that is obviously right for a route this repository has never seen", e.name, rt.Pattern)
			}
			if err := claim(i, rt, false); err != nil {
				return nil, nil, err
			}
			guardedRoutes = append(guardedRoutes, declared{name: e.name, route: rt})
		}
		for _, rt := range e.ext.PublicRoutes() {
			if rt.Handler == nil {
				return nil, nil, fmt.Errorf("extension '%s' declares public route '%s' with no handler", e.name, rt.Pattern)
			}
			if rt.Action != "" {
				// A contradiction, and the likeliest cause is a companion author
				// who believed the action would be enforced — which on a route
				// nothing authenticates it cannot be.
				return nil, nil, fmt.Errorf("extension '%s' declares public route '%s' with action '%s': a public route is served with no authentication and no authorisation, so the action would never be asked", e.name, rt.Pattern, rt.Action)
			}
			if err := claim(i, rt, true); err != nil {
				return nil, nil, err
			}
			publicRoutes = append(publicRoutes, declared{name: e.name, route: rt})
		}
	}
	return guardedRoutes, publicRoutes, nil
}

// guarded is a handler that runs only behind the guard, and is handed the
// subject the guard authenticated.
//
// A parameter rather than a value smuggled through the request context, because
// two of these handlers make a second and finer authorisation decision with it —
// list narrows its collection, the event stream authorises each event as it
// arrives — and a context lookup that came back empty would hand them a zero
// Subject. An authorizer given one fails closed, which is the right direction
// and the wrong failure: it would look like a caller with no permissions rather
// than like the wiring mistake it is.
type guarded func(w http.ResponseWriter, r *http.Request, subject authz.Subject)

// guard authenticates and authorises before the handler runs.
//
// Nothing behind it touches Docker, or even reads the reconciler's state, until
// both have passed: the controller holds write access to the swarm, so an API
// that authorised late would be one bug away from being a root shell.
//
// What it decides is the endpoint, not the collection behind it. The two
// endpoints that answer about every application ask again, per member, with what
// this hands them.
func (s *Server) guard(act authz.Action, h guarded) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, err := s.authz.Authenticate(r)
		if err != nil {
			// No WWW-Authenticate challenge: a browser prompting for basic
			// credentials on an API that takes a bearer token helps nobody.
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// PathValue is empty for the unscoped endpoints, which is what
		// Authorize documents as "not scoped to one application".
		if err := s.authz.Authorize(r.Context(), subject, act, r.PathValue("app")); err != nil {
			fail(w, http.StatusForbidden, "forbidden")
			return
		}
		h(w, r, subject)
	})
}

// root serves the UI, and stops it answering for the API.
//
// "GET /" matches every path no more specific pattern claimed, so an
// unregistered /api/v1/typo would otherwise reach the SPA and answer 200
// text/html — turning a client's typo into a parse error somewhere far away
// rather than into the 404 the mux used to give. The prefix is checked on the
// decoded path, so an escaped spelling of it lands in the same place.
//
// What this cannot recover is the method. ServeMux answers a request that
// matched no pattern by reporting which methods would have matched, and with
// GET / registered every path matches under GET — so POST /api/v1/typo is a 405
// naming GET and HEAD rather than a 404, and GET …/sync, which used to be a 405
// naming POST, is the JSON 404 above. Both are accepted rather than solved, and
// both are pinned by a test. Additionally registering a methodless /api/
// pattern would make the first a JSON 404 too and would not bring the second
// back — a pattern matching every method takes the request before the POST
// route can report itself — so it buys one status code for a second claim on
// /api/. See docs/api.md.
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		fail(w, http.StatusNotFound, "no such endpoint")
		return
	}
	s.ui.ServeHTTP(w, r)
}

// status serves the controller's own state: where the app set came from, when
// it last loaded, and whether what is running is a last-good set because a newer
// one is being refused.
//
// It is guarded like every other read. What it discloses — a repository path, a
// commit, a validation error naming applications — is exactly what an
// unauthenticated caller should not be able to enumerate about a controller that
// holds the docker socket.
//
// That argument is about an unauthenticated caller and it is not the only one
// this endpoint needs. The document answers about every application at once, so
// the guard's single unscoped decision is a decision about the collection rather
// than about its members — the reason Visible exists — and it carries three
// application-name lists plus the app set's own identity. A tenant scoped to one
// project would otherwise read the names of applications in every other, and the
// repository the whole fleet is deployed from.
//
// So it is narrowed twice over. The name lists go through Visible exactly as the
// list endpoint's rows do, always, since a name is a per-application fact
// whatever else the subject may read. The controller-wide fields — source,
// revision, the total count, and the error text that names applications — are
// ActionController's, and a subject the authorizer will not grant it gets a
// document with those removed rather than a 403: the list screen reads this on
// every load, and an authorizer that predates the action refuses it.
func (s *Server) status(w http.ResponseWriter, r *http.Request, subject authz.Subject) {
	views := s.rec.Views()
	st := application.ControllerStatus{Applications: len(views)}
	if s.controller != nil {
		st = s.controller.Status()
	}

	// One Visible call, over every name this response could carry rather than
	// over the views alone. Orphaned and Pruned name applications that have
	// *left* the set: by construction they are not among the views, so asking
	// only about those would drop the two lists for every subject including an
	// admin — which is what the test for this endpoint caught.
	reachable := dedupe(viewNames(views), st.AppSet.Orphaned, st.AppSet.Pruned, st.AppSet.PruneHeldBy)
	visible, err := s.visibleNames(r, subject, reachable)
	if err != nil {
		fail(w, http.StatusForbidden, "forbidden")
		return
	}
	st.AppSet.Orphaned = keepVisible(st.AppSet.Orphaned, visible)
	st.AppSet.Pruned = keepVisible(st.AppSet.Pruned, visible)
	st.AppSet.PruneHeldBy = keepVisible(st.AppSet.PruneHeldBy, visible)

	if s.authz.Authorize(r.Context(), subject, authz.ActionController, "") != nil {
		st.AppSet.Source = ""
		st.AppSet.Revision = ""
		// Replaced rather than blanked. Stale is not narrowed and says a newer
		// set is being refused, but the other failure this field reports — a set
		// that loaded and could not be applied in full — is stale false, and
		// blanking the text is what turns that one into a controller reporting
		// itself healthy. The sentence carries the fact without the names.
		if st.AppSet.Error != "" {
			st.AppSet.Error = "the app set has an error; ask an administrator"
		}
		// How many this subject can see, not how many there are: the total is
		// the size of the fleet. Counted over the views rather than off
		// len(visible), which also holds the departed names asked about above
		// and would report a count no list can produce.
		st.Applications = len(keepVisible(viewNames(views), visible))
	}
	write(w, http.StatusOK, st)
}

// viewNames is the applications in views, in the reconciler's order.
func viewNames(views []application.View) []string {
	names := make([]string, 0, len(views))
	for _, v := range views {
		names = append(names, v.Spec.Name)
	}
	return names
}

// dedupe is the union of several name lists, first occurrence winning, so that
// one Visible call can answer for all of them. Asking twice about a name an
// authorizer backs with a policy engine is a round trip bought for nothing.
func dedupe(lists ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(lists[0]))
	for _, list := range lists {
		for _, name := range list {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// visibleNames asks the authorizer which of names this subject may read, as a
// set.
//
// One call for the whole collection, which is what Visible documents and what
// keeps an authorizer backing it with a policy engine to one round trip for a
// list rather than one per row. It takes the names rather than reading the
// views again, so the answer is about the collection the caller is holding —
// two reads could disagree, and the caller is about to serialise one of them.
//
// A name Visible returned that was never offered cannot conjure anything: both
// callers use the result to filter a list they already had.
func (s *Server) visibleNames(r *http.Request, subject authz.Subject, names []string) (map[string]struct{}, error) {
	allowed, err := s.authz.Visible(r.Context(), subject, authz.ActionRead, names)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		set[name] = struct{}{}
	}
	return set, nil
}

// keepVisible drops the names that are not in allowed, preserving order and
// preserving the difference between "none" and "not reported": omitempty is on
// all three of the lists this narrows, and a subject who may see nothing in one
// gets the key absent rather than an empty array claiming the sweep found
// nothing.
func keepVisible(names []string, allowed map[string]struct{}) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := allowed[name]; ok {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// list serves the list view: every application the caller may see, with its
// sync state, health and last synced revision, in one request.
//
// Per-release detail is stripped. Status.Releases is omitempty precisely so
// that a list of twenty applications is not twenty release tables, and its
// absence unambiguously means "not requested" rather than "none" — the engine
// rejects a release file declaring no releases.
//
// Narrowed rather than served whole. The guard authorised this endpoint once,
// with an empty application, which is a decision about the collection and not
// about its members — so an authorizer implementing projects could only allow or
// deny the entire list, and a tenant with read access to one application
// enumerated every application's name, repository URL, revision and error text.
// Visible is the per-member question asked in one call, which is what keeps a
// companion backing it with a policy engine to one round trip for a list rather
// than one per row.
//
// The reconciler's order survives, and a name Visible returned that was never
// offered cannot conjure a row: what goes out is the views whose names came
// back, not the names.
func (s *Server) list(w http.ResponseWriter, r *http.Request, subject authz.Subject) {
	views := s.rec.Views()
	allowed, err := s.visibleNames(r, subject, viewNames(views))
	if err != nil {
		fail(w, http.StatusForbidden, "forbidden")
		return
	}

	out := make([]application.View, 0, len(allowed))
	for _, v := range views {
		if _, ok := allowed[v.Spec.Name]; !ok {
			continue
		}
		v.Status.Releases = nil
		out = append(out, v)
	}
	write(w, http.StatusOK, map[string]any{"applications": out})
}

// detail serves one application with its releases and their services.
func (s *Server) detail(w http.ResponseWriter, r *http.Request, _ authz.Subject) {
	view, ok := s.rec.View(r.PathValue("app"))
	if !ok {
		fail(w, http.StatusNotFound, "no such application")
		return
	}
	write(w, http.StatusOK, view)
}

// diff serves the manifest change each release would undergo.
func (s *Server) diff(w http.ResponseWriter, r *http.Request, _ authz.Subject) {
	diffs, err := s.rec.Diffs(r.PathValue("app"))
	switch {
	case errors.Is(err, application.ErrNotPlanned):
		// Not an error the caller can fix, and not a 404 either: the
		// application exists and has simply not been reconciled yet. An empty
		// list rather than the nil one a converged plan produces; see
		// application.DiffResponse for why the two must stay different.
		write(w, http.StatusOK, application.DiffResponse{Releases: []application.ReleaseDiff{}})
		return
	case err != nil:
		fail(w, http.StatusNotFound, "no such application")
		return
	}
	write(w, http.StatusOK, application.DiffResponse{Releases: diffs, Planned: true})
}

// history serves every declared release's revisions in one request.
func (s *Server) history(w http.ResponseWriter, r *http.Request, _ authz.Subject) {
	hist, err := s.rec.History(r.Context(), r.PathValue("app"))
	switch {
	case errors.Is(err, application.ErrNotPlanned):
		write(w, http.StatusOK, application.History{Releases: []application.ReleaseHistory{}})
		return
	case err != nil:
		s.log.Warn("reading history failed", "application", r.PathValue("app"), "error", err)
		fail(w, http.StatusBadGateway, "could not read release history from the swarm")
		return
	}
	write(w, http.StatusOK, hist)
}

// sync triggers a reconcile and returns immediately.
//
// A sync fetches, renders, plans and deploys, and under a wait policy it blocks
// until the rollout converges or times out — minutes, legitimately. Holding the
// request open for that would hang a browser tab and be cut by the first proxy
// in front of it, with no way for the caller to learn what happened afterwards.
// The event stream and the status endpoint are how a caller follows it, which
// is what they are for.
// A request that arrives while one sync is running and another is already queued
// behind it is coalesced onto that queued one rather than adding to the pile.
// Still 202, because the state the caller asked to have reconciled will be
// reconciled — by the sync already waiting, which has not read the repository
// yet. The response says which of the two happened rather than claiming a sync
// was started that was not.
//
// This is the only handler that starts work, so it is the only one with an actor
// to record, and it records it on the sync's context rather than through
// AcceptSync. Reconciler is an interface so that an alternative reconciler can
// implement it; widening a method there to carry an attribution label would
// break every implementation for something none of them has to understand. See
// notify.WithActor.
func (s *Server) sync(w http.ResponseWriter, r *http.Request, subject authz.Subject) {
	app := r.PathValue("app")

	run, err := s.rec.AcceptSync(app)
	switch {
	case errors.Is(err, application.ErrSyncPending):
		write(w, http.StatusAccepted, map[string]any{"application": app, "accepted": true, "coalesced": true})
		return
	case err != nil:
		fail(w, http.StatusNotFound, "no such application")
		return
	}

	s.syncing(app, func(ctx context.Context) {
		// Marked here rather than on r.Context(): detach severs the request's
		// context, so a value put on it would never reach the sync it was for.
		if err := run(notify.WithActor(ctx, subject.Name)); err != nil {
			// Already recorded on the application's status and dispatched as a
			// sync-failed event; this is the log line that says a manual one
			// was what failed.
			s.log.Error("manual sync failed", "application", app, "error", err)
		}
	})
	write(w, http.StatusAccepted, map[string]any{"application": app, "accepted": true})
}

// detach runs a sync outside the request that asked for it.
//
// The context is explicitly severed from the request's. A request context is
// cancelled the moment the response is written, so passing it here would abort
// every sync the instant its 202 went out — and the failure would look like a
// controller that ignores the button.
func (s *Server) detach(_ string, run func(context.Context)) {
	go run(context.WithoutCancel(context.Background()))
}

// Drain ends every connected event stream. The caller registers it with
// http.Server.RegisterOnShutdown; see stream.closeAll for why Shutdown cannot
// do it on its own.
func (s *Server) Drain() { s.events.closeAll() }

// Notify feeds the event stream. It is the notify.Notifier implementation; the
// caller registers it.
func (s *Server) Notify(ctx context.Context, e notifyEvent) { s.events.publish(ctx, e) }

func write(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, code int, message string) {
	write(w, code, map[string]string{"error": message})
}
