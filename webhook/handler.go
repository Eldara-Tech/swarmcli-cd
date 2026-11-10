// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package webhook turns a forge's push notification into a refresh of whatever
// the push touched, so a commit is picked up in seconds rather than at the next
// interval.
//
//	POST /hooks/github   GitHub, GitHub Enterprise, Gitea and Forgejo
//	POST /hooks/gitlab   GitLab
//
// It is served on a listener of its own and never on the API's. The API carries
// root-equivalent access behind the admin token and is deliberately left
// unpublished, where a forge has to be able to reach this one from outside. So
// what a delivery can do is kept as small as it can be made: the secret is
// checked over the raw body before a byte of it is parsed, and a delivery that
// passes can only ask for what the controller would have done at its next tick
// anyway — re-read the app set from the repository it already follows, or
// reconcile the applications that already track the pushed branch or tag. The
// payload never chooses what is deployed, a manual policy is never applied, and
// the app set's own sweeps — the prune of departed applications and the cache
// reclaim — stay on its interval. An application's reconcile is the one its tick
// runs, though, so its own prune and pruneResources run on a refresh too.
package webhook

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// maxBody is the largest delivery read. It is the app set's own cap and well
// above any push a forge sends — GitHub truncates the commit list it puts in
// one — and it is applied before authentication, because reading an unbounded
// body is the cheapest way to hurt a process before it has checked anything.
const maxBody = 4 << 20 // 4 MiB

// maxDeliveries bounds how many deliveries are handled at once. Taken without
// blocking, so the one over the bound is refused at once rather than queued: a
// delivery holds up to maxBody in memory until it is verified, so the worst case
// is 64 MiB buffered. Sixteen rather than a handful because a slot is held while
// an unauthenticated body arrives, and every slot a stalled one holds is a real
// delivery answered 503 — four of which in a row disable a GitLab hook.
const maxDeliveries = 16

// bodyReadTimeout bounds how long one delivery's body may take to arrive, which
// is how long a slot can be held by somebody who has not authenticated. The
// listener's own ReadTimeout is thirty seconds, so without this a handful of
// connections that send headers and then stall would hold every slot that long,
// over and over. Five seconds is far more than a 4 MiB body needs on any link a
// forge delivers over.
const bodyReadTimeout = 5 * time.Second

// Reconciler is what a delivery refreshes. *reconcile.Reconciler implements it.
type Reconciler interface {
	Views() []application.View
	Refresh(app string) error
}

// AppSet is the loop a delivery to the app set's repository wakes.
// *appset.Loop implements it.
type AppSet interface {
	Refresh()
}

// Options configure the handler.
type Options struct {
	// Secrets are what a delivery may be signed with: see SecretsFromEnv. With
	// none, every delivery is refused; the controller will not start that way.
	Secrets    []string
	Reconciler Reconciler
	// AppSet is woken by a push to AppSetRepo at AppSetRevision, the app set's
	// own repository and branch or tag. Both are empty outside git mode, where
	// the set has no repository to push to, and then nothing wakes it.
	AppSet         AppSet
	AppSetRepo     string
	AppSetRevision string
	Log            *slog.Logger
}

// forge is what differs between the two routes: how a delivery proves where it
// came from, and where in it the push is described.
type forge struct {
	name   string
	verify func(s secrets, h http.Header, body []byte, now time.Time) error
	parse  func(h http.Header, body []byte) (push, bool, error)
}

var (
	github = forge{name: "github", verify: verifyGitHub, parse: parseGitHub}
	gitlab = forge{name: "gitlab", verify: verifyGitLab, parse: parseGitLab}
)

type handler struct {
	secrets        secrets
	rec            Reconciler
	appSet         AppSet
	appSetRepo     string
	appSetRevision string
	log            *slog.Logger
	now            func() time.Time
	floor          *floor
	slots          chan struct{}
	// bodyTimeout is bodyReadTimeout, a field so a test can wait it out in
	// milliseconds.
	bodyTimeout time.Duration
}

// New returns the handler the webhook listener serves.
func New(o Options) http.Handler { return newHandler(o).routes() }

func newHandler(o Options) *handler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	// A repository that does not normalise is one no push can come from, and
	// the empty string it leaves is never a pushed repository.
	repo, _ := normalise(o.AppSetRepo)
	return &handler{
		secrets: newSecrets(o.Secrets), rec: o.Reconciler,
		appSet: o.AppSet, appSetRepo: repo, appSetRevision: o.AppSetRevision,
		log: o.Log, now: time.Now,
		floor: newFloor(application.MinInterval),
		slots: make(chan struct{}, maxDeliveries),

		bodyTimeout: bodyReadTimeout,
	}
}

// routes is the whole of what the listener serves. Anything else is the mux's
// own 404, and the wrong method its 405.
func (h *handler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/github", h.serve(github))
	mux.HandleFunc("POST /hooks/gitlab", h.serve(gitlab))
	return mux
}

// serve handles one forge's deliveries, in an order that is the point: bounded,
// then authenticated, and only then parsed.
//
// Every authenticated delivery that parses gets the same answer, 202 and
// {"accepted": true}, whether it woke anything or not — a push nothing tracks,
// a ping, another event. Every configured secret is good for every repository,
// so an answer that differed would let whoever holds any one of them probe what
// else this controller follows; what was woken is in the log instead. And 2xx
// at all because GitLab disables a hook after four failed deliveries in a row,
// and none of these is a failure.
func (h *handler) serve(f forge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Before the slot, so that it is the request's arrival and not its turn
		// that starts the clock. Real time, not h.now: it is the connection's.
		// The error is a writer that cannot set one, which only a test has.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(h.bodyTimeout))
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		default:
			fail(w, http.StatusServiceUnavailable, "too many deliveries in flight; redeliver it shortly")
			return
		}

		// A new GitHub hook defaults to a form-encoded body, which is the one
		// misconfiguration everybody makes once, so the answer says how to fix it.
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			fail(w, http.StatusUnsupportedMediaType, "want Content-Type application/json; on GitHub, set the hook's content type to application/json")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				fail(w, http.StatusRequestEntityTooLarge, "the body is too large")
				return
			}
			fail(w, http.StatusBadRequest, "the body could not be read")
			return
		}

		// Debug, and never the body or the signature: a rejection is anybody on
		// the internet, and at info they could fill the log.
		if err := f.verify(h.secrets, r.Header, body, h.now()); err != nil {
			h.log.Debug("webhook delivery rejected", "forge", f.name, "reason", err)
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		p, ok, err := f.parse(r.Header, body)
		if err != nil {
			// Not the decoder's error, which can quote the body.
			h.log.Debug("webhook delivery rejected", "forge", f.name, "reason", "the body is not JSON")
			fail(w, http.StatusBadRequest, "the body is not JSON")
			return
		}
		if ok {
			h.wake(f, p)
		} else {
			h.log.Debug("webhook delivery ignored: not a push", "forge", f.name)
		}
		write(w, http.StatusAccepted, map[string]bool{"accepted": true})
	}
}

// wake refreshes what a push touched, through the floor, and logs what it did.
func (h *handler) wake(f forge, p push) {
	repo, appSet, apps := h.match(p)
	if !appSet && len(apps) == 0 {
		h.log.Debug("webhook delivery ignored: nothing tracks this repository at this ref",
			"forge", f.name, "delivery", p.delivery, "repository", repo, "ref", p.ref)
		return
	}

	deferred := 0
	if appSet && h.floor.wake(target{appSet: true}, h.appSet.Refresh) {
		deferred++
	}
	for _, app := range apps {
		// An application that left the set since match read it has nothing
		// to refresh, and its error says only that.
		if h.floor.wake(target{app: app}, func() { _ = h.rec.Refresh(app) }) {
			deferred++
		}
	}
	h.log.Info("webhook delivery accepted",
		"forge", f.name, "delivery", p.delivery, "repository", repo, "ref", p.ref,
		"appSet", appSet, "applications", apps, "deferred", deferred)
}

func write(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, code int, message string) {
	write(w, code, map[string]string{"error": message})
}
