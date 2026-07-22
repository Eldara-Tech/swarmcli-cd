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
//	GET  /api/v1/applications                    the list view
//	GET  /api/v1/applications/{app}              the detail view
//	GET  /api/v1/applications/{app}/diff         the diff view
//	GET  /api/v1/applications/{app}/history      the history view
//	POST /api/v1/applications/{app}/sync         the sync button
//	GET  /api/v1/events                          live updates, so nothing polls
//
// Applications are read-only in Phase 1 — they come from a mounted
// applications.yaml, and there is no hot reload because Docker configs are
// immutable. The paths are nouns so that CRUD can be added later without any of
// them moving.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/reconcile"
)

// Reconciler is what the API serves. *reconcile.Reconciler implements it.
type Reconciler interface {
	Views() []application.View
	View(app string) (application.View, bool)
	Diffs(app string) ([]application.ReleaseDiff, error)
	History(ctx context.Context, app string) (application.History, error)
	SyncNow(ctx context.Context, app string) error
}

// Server is the HTTP interface. It is also a notify.Notifier: the event stream
// is fed by the same seam that feeds the log, which is why notify appends
// rather than replaces — a companion adding Slack must not silently kill the
// UI's live updates.
type Server struct {
	rec    Reconciler
	authz  authz.Authorizer
	log    *slog.Logger
	events *stream
	// syncing runs a sync detached from the request that asked for it.
	// Overridable in tests, which otherwise have to race a goroutine.
	syncing func(app string, run func(context.Context))
}

// Options tune a Server. Every field has a working default.
type Options struct {
	Authorizer authz.Authorizer
	Log        *slog.Logger
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
	s := &Server{rec: rec, authz: o.Authorizer, log: o.Log, events: newStream(o.Log)}
	s.syncing = s.detach
	return s
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated, and deliberately says nothing: a container healthcheck
	// runs beside the process and cannot carry a credential without putting one
	// in the stack file and in `docker inspect` output. What it discloses is
	// that something is listening, which a TCP connect already tells you.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.Handle("GET /api/v1/applications", s.guard(authz.ActionRead, s.list))
	mux.Handle("GET /api/v1/applications/{app}", s.guard(authz.ActionRead, s.detail))
	mux.Handle("GET /api/v1/applications/{app}/diff", s.guard(authz.ActionRead, s.diff))
	mux.Handle("GET /api/v1/applications/{app}/history", s.guard(authz.ActionRead, s.history))
	mux.Handle("POST /api/v1/applications/{app}/sync", s.guard(authz.ActionSync, s.sync))
	mux.Handle("GET /api/v1/events", s.guard(authz.ActionRead, s.stream))

	return mux
}

// guard authenticates and authorises before the handler runs.
//
// Nothing behind it touches Docker, or even reads the reconciler's state, until
// both have passed: the controller holds write access to the swarm, so an API
// that authorised late would be one bug away from being a root shell.
func (s *Server) guard(act authz.Action, h func(http.ResponseWriter, *http.Request)) http.Handler {
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
		h(w, r)
	})
}

// list serves the list view: every application with its sync state, health and
// last synced revision, in one request.
//
// Per-release detail is stripped. Status.Releases is omitempty precisely so
// that a list of twenty applications is not twenty release tables, and its
// absence unambiguously means "not requested" rather than "none" — the engine
// rejects a release file declaring no releases.
func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	views := s.rec.Views()
	out := make([]application.View, 0, len(views))
	for _, v := range views {
		v.Status.Releases = nil
		out = append(out, v)
	}
	write(w, http.StatusOK, map[string]any{"applications": out})
}

// detail serves one application with its releases and their services.
func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	view, ok := s.rec.View(r.PathValue("app"))
	if !ok {
		fail(w, http.StatusNotFound, "no such application")
		return
	}
	write(w, http.StatusOK, view)
}

// diff serves the manifest change each release would undergo.
func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	diffs, err := s.rec.Diffs(r.PathValue("app"))
	switch {
	case errors.Is(err, reconcile.ErrNotPlanned):
		// Not an error the caller can fix, and not a 404 either: the
		// application exists and has simply not been reconciled yet.
		write(w, http.StatusOK, map[string]any{"releases": []application.ReleaseDiff{}, "planned": false})
		return
	case err != nil:
		fail(w, http.StatusNotFound, "no such application")
		return
	}
	write(w, http.StatusOK, map[string]any{"releases": diffs, "planned": true})
}

// history serves every declared release's revisions in one request.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	hist, err := s.rec.History(r.Context(), r.PathValue("app"))
	switch {
	case errors.Is(err, reconcile.ErrNotPlanned):
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
func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if _, ok := s.rec.View(app); !ok {
		fail(w, http.StatusNotFound, "no such application")
		return
	}

	s.syncing(app, func(ctx context.Context) {
		if err := s.rec.SyncNow(ctx, app); err != nil {
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
