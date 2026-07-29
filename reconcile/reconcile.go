// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package reconcile is the pull loop: for each application, fetch the
// repository, render it, plan against the swarm, and — when the sync policy
// says so — apply.
//
// Each application runs on its own schedule in its own goroutine. One
// application whose repository is unreachable must not stall the others, and a
// shared queue would make exactly that happen.
//
// The set of applications is mutable while the loop runs: Add, Remove and
// Replace start, stop and retune per-application loops under the reconciler's
// lock. This is what lets the app set itself be reconciled from git (issue #47);
// the loop that drives those operations from a diff is a separate concern.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/compose"
	"github.com/Eldara-Tech/swarmcli-cd/drift"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/health"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
	"github.com/Eldara-Tech/swarmcli-cd/prune"
	"github.com/Eldara-Tech/swarmcli-cd/regauth"
	"github.com/Eldara-Tech/swarmcli-cd/source"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

const (
	// DefaultInterval matches ArgoCD's. Every tick costs a git fetch, a full
	// render of every release, and a read of the swarm's release records, so
	// the default is deliberately not aggressive; an application that needs to
	// be quicker sets its own.
	DefaultInterval = 3 * time.Minute

	// maxBackoff bounds how far a permanently broken application backs off.
	// Long enough to stop hammering a dead remote, short enough that fixing
	// the cause does not need a restart to be noticed.
	maxBackoff = 30 * time.Minute

	// maxBackoffShift caps the exponent so the shift cannot overflow.
	maxBackoffShift = 8
)

// Fetcher brings an application's repository to a revision. *git.Sourcer
// implements it.
type Fetcher interface {
	Fetch(ctx context.Context, app string, src application.Source) (git.Checkout, error)
}

// Builder turns a working tree into a plan's inputs. *source.Builder
// implements it.
type Builder interface {
	Build(ctx context.Context, app string, spec application.Source, co git.Checkout) (*source.Built, error)
}

// Engine is the part of the chart engine this loop uses. *charts.Engine
// implements it.
type Engine interface {
	PlanApply(ctx context.Context, rf *charts.ReleaseFile, src charts.ChartSource, opts charts.PlanOptions) (*charts.Plan, error)
	Apply(ctx context.Context, plan *charts.Plan, opts charts.InstallOptions) ([]charts.ApplyResult, error)
	History(ctx context.Context, release string) ([]charts.Release, error)
	// Uninstall removes a release's stack and its recorded revisions, keeping
	// its volumes unless asked. Used only by prune, so an application whose
	// sync policy does not enable it never reaches this.
	Uninstall(ctx context.Context, release string, purgeVolumes bool) (*charts.UninstallResult, error)
}

// Options configures a Reconciler. Everything has a working default except the
// fetcher and the builder, which have no sensible one.
type Options struct {
	Fetcher   Fetcher
	Builder   Builder
	Swarms    swarms.Registry
	NewEngine func(charts.Backend) Engine
	Interval  time.Duration
	Log       *slog.Logger
	Now       func() time.Time
	// RegistryAuth resolves an application's image-pull credential, keyed by
	// application name. An application absent from the map deploys public images
	// only. Built at startup by regauth.Load, which is where a missing or
	// unparseable secret becomes a startup error.
	RegistryAuth map[string]regauth.Resolver
	// ForbiddenSecretMounts names the controller's own mounted secrets, which no
	// reconciled stack may mount. Controller-wide; built at startup from the
	// controller's /run/secrets. Empty disables the check.
	ForbiddenSecretMounts map[string]struct{}

	// ControllerID is the identity half of the owner stamp this reconciler
	// writes, so that a second controller on the same swarm does not read these
	// releases as its own. Empty is application.DefaultControllerID.
	ControllerID string
}

// appEntry is one application's mutable record: the spec it reconciles against,
// what was last observed of it, and the handle that stops its loop. Every field
// is guarded by Reconciler.mu except syncing, which is locked directly through
// the *appEntry so a long reconcile does not hold the map's lock.
type appEntry struct {
	// syncing serialises work for this application, so a manual sync and a
	// scheduled tick cannot render or deploy it at once. Locked via the
	// *appEntry, independent of mu.
	syncing sync.Mutex

	spec   application.Spec
	status application.Status
	// plan is the last plan for this application so the diff endpoint can be
	// served without re-rendering. A plan carries whole manifests, which is why
	// it is kept per application rather than per revision.
	plan *charts.Plan

	// cancel stops this application's loop; done is closed when that goroutine
	// has returned. Both are nil until the loop is started — before Run, or for
	// an application added to a not-yet-running reconciler.
	cancel context.CancelFunc
	done   chan struct{}
}

// Reconciler runs the loop and holds what it last observed.
type Reconciler struct {
	fetch      Fetcher
	build      Builder
	swarms     swarms.Registry
	newEngine  func(charts.Backend) Engine
	interval   time.Duration
	log        *slog.Logger
	now        func() time.Time
	forbidden  map[string]struct{}
	controller string

	mu sync.RWMutex
	// regAuth is the per-application image-pull credential, guarded by mu
	// because an application that joins the set at runtime brings one with it.
	regAuth map[string]regauth.Resolver
	// apps is the reconciled set, keyed by name; order is the sequence they were
	// declared or added in, which is the order Views reports them. Both are
	// guarded by mu.
	apps  map[string]*appEntry
	order []string
	// root is the context Run supervises the loops under: every loop's context
	// derives from it, so cancelling Run cancels them all. It is nil until Run
	// is called; wg tracks the live loop goroutines so Run can drain them.
	root context.Context
	wg   sync.WaitGroup
}

// New returns a Reconciler for the given applications.
func New(apps []application.Spec, o Options) *Reconciler {
	if o.Swarms == nil {
		o.Swarms = swarms.Get()
	}
	if o.NewEngine == nil {
		o.NewEngine = func(b charts.Backend) Engine { return charts.NewEngineWith(b) }
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ControllerID == "" {
		o.ControllerID = application.DefaultControllerID
	}

	r := &Reconciler{
		fetch:      o.Fetcher,
		build:      o.Builder,
		swarms:     o.Swarms,
		newEngine:  o.NewEngine,
		interval:   o.Interval,
		log:        o.Log,
		now:        o.Now,
		forbidden:  o.ForbiddenSecretMounts,
		controller: o.ControllerID,
		// Copied, not adopted: the map is written to as the set changes, and the
		// caller's copy — built once at startup — is not ours to mutate.
		regAuth: make(map[string]regauth.Resolver, len(o.RegistryAuth)),
		apps:    make(map[string]*appEntry, len(apps)),
		order:   make([]string, 0, len(apps)),
	}
	maps.Copy(r.regAuth, o.RegistryAuth)
	for _, spec := range apps {
		r.apps[spec.Name] = &appEntry{
			spec:   spec,
			status: application.Status{Sync: application.Sync{State: application.SyncUnknown}},
		}
		r.order = append(r.order, spec.Name)
	}
	return r
}

// Run reconciles until ctx is cancelled.
//
// It starts a loop for every application in the set and then blocks, so that
// applications added while it runs are supervised under the same context and a
// removed one's loop is cancelled with it. A destination the swarm registry
// cannot resolve is not fatal here: it fails its own application on the next
// tick, surfaced on that application's status, rather than stopping the loop
// that observes every other one.
func (r *Reconciler) Run(ctx context.Context) error {
	r.mu.Lock()
	r.root = ctx
	for _, name := range r.order {
		r.startLoopLocked(r.apps[name])
	}
	r.mu.Unlock()

	<-ctx.Done()
	r.wg.Wait()
	return ctx.Err()
}

// startLoopLocked starts an application's reconcile loop under the run context.
// The caller holds mu. It is a no-op before Run has set that context — an
// application added to a not-yet-running reconciler is simply recorded and
// started when Run is — and once the context is cancelled, so nothing is started
// into a shutdown that is already draining.
func (r *Reconciler) startLoopLocked(e *appEntry) {
	if r.root == nil || r.root.Err() != nil || e.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.root)
	e.cancel = cancel
	e.done = make(chan struct{})
	name := e.spec.Name
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(e.done)
		defer cancel()
		r.loop(ctx, name)
	}()
}

// Add starts reconciling a new application. It returns an error if one of that
// name is already present: the caller that drives the set from a git diff
// (issue #52) tells add from replace itself and does not rely on this being an
// upsert.
func (r *Reconciler) Add(spec application.Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.apps[spec.Name]; ok {
		return fmt.Errorf("application %q already present", spec.Name)
	}
	e := &appEntry{
		spec:   spec,
		status: application.Status{Sync: application.Sync{State: application.SyncUnknown}},
	}
	r.apps[spec.Name] = e
	r.order = append(r.order, spec.Name)
	r.startLoopLocked(e)
	return nil
}

// Replace swaps the spec a running application reconciles against, keyed by
// name, keeping its recorded status and its loop: the next tick reads the new
// spec, so a healthy loop is retuned rather than restarted. A change of name is
// not a replace — it is a Remove of the old and an Add of the new, which the
// caller does, because the two report as different applications.
func (r *Reconciler) Replace(spec application.Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.apps[spec.Name]
	if !ok {
		return fmt.Errorf("no such application %q", spec.Name)
	}
	e.spec = spec
	return nil
}

// SetRegistryAuth installs the image-pull credential an application's images are
// pulled with, replacing whatever it had. A nil resolver removes it, which is
// what an application that has just dropped its registryAuth needs — otherwise
// the credential it no longer declares would keep being sent.
//
// It exists because the set is mutable: regauth.Load resolves the applications
// declared at startup, and an application that joins later brings a credential
// that map has never seen. The caller driving the set resolves it and puts it
// here before the application is added, so the first reconcile already has it.
func (r *Reconciler) SetRegistryAuth(app string, resolver regauth.Resolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if resolver == nil {
		delete(r.regAuth, app)
		return
	}
	r.regAuth[app] = resolver
}

// registryAuth returns an application's credential, or nil for one that
// declared none.
func (r *Reconciler) registryAuth(app string) regauth.Resolver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.regAuth[app]
}

// Remove stops reconciling an application and drops what was observed of it. It
// cancels the application's loop and waits for that goroutine to return before
// reporting done, so a removed application is provably no longer reconciling —
// a caller replacing the whole set can rely on that. The deployed stack is left
// running and becomes unmanaged (D-e); pruning it is separate (issue #54).
func (r *Reconciler) Remove(name string) error {
	r.mu.Lock()
	e, ok := r.apps[name]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("no such application %q", name)
	}
	delete(r.apps, name)
	// With the entry goes the credential: a name that returns to the set later
	// is a new application as far as this reconciler is concerned, and must not
	// inherit a secret the departed one happened to declare.
	delete(r.regAuth, name)
	r.order = removeName(r.order, name)
	cancel, done := e.cancel, e.done
	r.mu.Unlock()

	// Outside the lock: the loop takes mu to read its spec, so waiting for it
	// while holding mu would deadlock.
	if cancel != nil {
		cancel()
		<-done
	}
	return nil
}

// removeName returns order without name, leaving the original backing array
// untouched so a Views slice taken earlier is not mutated underneath it.
func removeName(order []string, name string) []string {
	out := order[:0:0]
	for _, n := range order {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}

// registryAuthBackend is the optional interface a backend implements to
// authenticate image pulls with an application's credential. *backend.Backend
// satisfies it; a Phase 3 remote backend reached through the same swarms seam
// need not, and authenticates its own way.
type registryAuthBackend interface {
	WithRegistryAuth(regauth.Resolver) charts.Backend
}

// withRegistryAuth scopes a backend to one application's credential. A nil
// resolver — the application declared no registryAuth — or a backend that does
// not support the upgrade leaves it unchanged, so public-image applications and
// alternative backends are untouched.
func withRegistryAuth(b charts.Backend, auth regauth.Resolver) charts.Backend {
	if auth == nil {
		return b
	}
	if ab, ok := b.(registryAuthBackend); ok {
		return ab.WithRegistryAuth(auth)
	}
	return b
}

// forbidSecretsBackend is the optional interface a backend implements to refuse
// a stack mounting the controller's own secrets. *backend.Backend satisfies it.
type forbidSecretsBackend interface {
	WithForbiddenSecrets(map[string]struct{}) charts.Backend
}

// withForbiddenSecrets scopes a backend to the controller's forbidden secret
// set. An empty set or a backend that does not support the upgrade leaves it
// unchanged.
func withForbiddenSecrets(b charts.Backend, names map[string]struct{}) charts.Backend {
	if len(names) == 0 {
		return b
	}
	if fb, ok := b.(forbidSecretsBackend); ok {
		return fb.WithForbiddenSecrets(names)
	}
	return b
}

// outOfBandBackend is the optional interface a backend implements to report a
// mutation that lost its compare-and-swap. *backend.Backend satisfies it.
type outOfBandBackend interface {
	WithOutOfBandNotifier(func(service string)) charts.Backend
}

// withOutOfBandNotifier makes the backend report a write that raced one of ours.
//
// Swarm gives the controller exactly one signal that something else is writing
// to a service — ?version= is mandatory, so a mutation that loses the race is
// refused — and until now that signal was detected and thrown away: the backend
// called a no-op default because only this layer knows which application a
// write belongs to.
//
// The write is still retried and still lands, which is right: the desired spec
// is complete, so re-applying it *is* correcting the drift. What must not happen
// is the overwrite being silent, and this is what stops it being.
//
// Reported once per service per sync rather than once per attempt. The retry
// loop can fire three times for one conflict, and three identical notifications
// describe one event.
func (r *Reconciler) withOutOfBandNotifier(ctx context.Context, b charts.Backend, app string) charts.Backend {
	ob, ok := b.(outOfBandBackend)
	if !ok {
		return b
	}

	var mu sync.Mutex
	seen := map[string]struct{}{}
	return ob.WithOutOfBandNotifier(func(service string) {
		mu.Lock()
		_, already := seen[service]
		seen[service] = struct{}{}
		mu.Unlock()
		if already {
			return
		}
		notify.Dispatch(ctx, notify.Event{
			Application: app,
			Type:        notify.LiveDriftDetected,
			At:          r.now(),
			Message: "service " + service + " was changed by something else while this sync was writing it; " +
				"the change has been overwritten with what the repository declares",
		})
	})
}

// liveDriftBackend is the optional interface a backend implements to expose the
// two halves of a live drift comparison. *backend.Backend satisfies it.
//
// It exists because charts.Backend carries no way to read a ServiceSpec — its
// StackServices returns a display projection with a running count and no spec —
// and no Docker client for the reconciler to convert a manifest with. A backend
// that cannot answer, such as a Phase 3 remote one reached through the same
// swarms seam, simply does not implement it and its applications report no live
// drift rather than failing.
//
// Only the read half needs a seam. Correcting drift is DeployStack, which is on
// charts.Backend already.
type liveDriftBackend interface {
	DesiredServices(ctx context.Context, manifest, stack string) (*compose.Stack, error)
	LiveServices(ctx context.Context, stack string) (map[string]swarm.Service, error)
}

// liveDrift compares each settled release against what is running, for an
// application that asked for it.
//
// Only releases the plan calls unchanged are compared, and that is the decision
// that makes the whole mode affordable and safe. A release the plan would
// install has nothing running to compare; one it would upgrade is about to have
// its spec overwritten, so a spec-level difference is noise against a manifest
// the sync axis already reports as out of date. What is left is exactly the
// case nothing else can see: a release git has not touched whose services no
// longer match it.
//
// A release that cannot be read reports Unknown rather than failing the
// reconcile — this is a read for a reporting axis, and a daemon hiccup does not
// make the deploy wrong. It is also not converged: the controller does not
// write on the strength of a read it could not make.
func (r *Reconciler) liveDrift(ctx context.Context, spec application.Spec, b charts.Backend, plan *charts.Plan) map[string]*application.ReleaseDrift {
	if spec.DriftDetection != application.DriftLive {
		return nil
	}
	ldb, ok := b.(liveDriftBackend)
	if !ok {
		return nil
	}

	out := make(map[string]*application.ReleaseDrift, len(plan.Releases))
	for _, rp := range plan.Releases {
		if rp.Action != charts.ActionUnchanged {
			continue
		}
		desired, err := ldb.DesiredServices(ctx, rp.Manifest, rp.Name)
		if err == nil {
			var live map[string]swarm.Service
			if live, err = ldb.LiveServices(ctx, rp.Name); err == nil {
				out[rp.Name] = drift.Live(desired, live)
				continue
			}
		}
		r.log.Warn("could not compare a release against what is running",
			"application", spec.Name, "release", rp.Name, "error", err)
		out[rp.Name] = &application.ReleaseDrift{
			State:   application.DriftStateUnknown,
			Message: err.Error(),
		}
	}
	return out
}

// driftedReleases names the releases matching want, in plan order.
func driftedReleases(plan *charts.Plan, live map[string]*application.ReleaseDrift, want func(*application.ReleaseDrift) bool) []string {
	if len(live) == 0 {
		return nil
	}
	var out []string
	for _, rp := range plan.Releases {
		if want(live[rp.Name]) {
			out = append(out, rp.Name)
		}
	}
	return out
}

// detected is a release whose live comparison found a difference. Unknown is
// deliberately not one: a comparison that could not be made has found nothing.
func detected(d *application.ReleaseDrift) bool {
	return d != nil && d.State == application.DriftStateDetected
}

// convergeable is a release that redeploying its manifest could actually put
// right.
//
// A modified service is rewritten and a missing one is created, so both are
// fixable. An *unexpected* service is not: applying deletes nothing — by design,
// established in Phase 1 and unchanged here — so a redeploy would leave it
// exactly where it is and the next reconcile would find it again. Treating it as
// convergeable would redeploy the whole stack on every tick forever, raising a
// correction event each time, while never removing the one thing it was
// reacting to.
//
// So it is reported and not acted on. The application stays out of sync, which
// is the truth: the swarm does not match the repository, and putting that right
// means deleting something this controller has decided it does not delete.
func convergeable(d *application.ReleaseDrift) bool {
	if !detected(d) {
		return false
	}
	for _, svc := range d.Services {
		if svc.Reason == application.DriftModified || svc.Reason == application.DriftMissing {
			return true
		}
	}
	return false
}

// loop reconciles one application on its own schedule, reading its current spec
// each tick so a Replace is picked up without the loop being restarted.
func (r *Reconciler) loop(ctx context.Context, app string) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if err := r.Sync(ctx, app); err != nil {
			// A cancelled context is a shutdown, not a failure to back off
			// from.
			if ctx.Err() != nil {
				return
			}
			failures++
			r.log.Error("reconcile failed", "application", app, "failures", failures, "error", err)
		} else {
			failures = 0
		}

		timer.Reset(backoff(r.intervalFor(r.currentSpec(app)), failures))
	}
}

func (r *Reconciler) intervalFor(spec application.Spec) time.Duration {
	if d := time.Duration(spec.SyncPolicy.Interval); d > 0 {
		return d
	}
	return r.interval
}

// backoff doubles the interval per consecutive failure, up to maxBackoff.
func backoff(interval time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return interval
	}
	if failures > maxBackoffShift {
		failures = maxBackoffShift
	}
	d := interval << failures
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

// Sync reconciles one application now, applying if its policy allows.
func (r *Reconciler) Sync(ctx context.Context, app string) error {
	return r.reconcile(ctx, app, false)
}

// SyncNow reconciles one application now and applies whatever the plan
// contains, whether or not the policy is automated. It is what the API's sync
// action calls: a manual policy means "do not deploy on a schedule", not "never
// deploy".
func (r *Reconciler) SyncNow(ctx context.Context, app string) error {
	return r.reconcile(ctx, app, true)
}

func (r *Reconciler) reconcile(ctx context.Context, app string, force bool) error {
	r.mu.RLock()
	e, ok := r.apps[app]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no such application %q", app)
	}

	e.syncing.Lock()
	defer e.syncing.Unlock()

	// Read the current spec under the lock: a Replace may have swapped it since
	// this reconcile was scheduled, and the swapped-in spec is the one to act on.
	r.mu.RLock()
	spec := e.spec
	r.mu.RUnlock()

	err := r.reconcileLocked(ctx, spec, force)
	if err != nil {
		r.setError(app, err)
	}
	return err
}

func (r *Reconciler) reconcileLocked(ctx context.Context, spec application.Spec, force bool) error {
	checkout, err := r.fetch.Fetch(ctx, spec.Name, spec.Source)
	if err != nil {
		return fmt.Errorf("fetching source: %w", err)
	}

	built, err := r.build.Build(ctx, spec.Name, spec.Source, checkout)
	if err != nil {
		return fmt.Errorf("reading source: %w", err)
	}

	backend, err := r.swarms.Backend(ctx, spec.Destination.Swarm)
	if err != nil {
		return fmt.Errorf("resolving destination: %w", err)
	}
	backend = withRegistryAuth(backend, r.registryAuth(spec.Name))
	backend = withForbiddenSecrets(backend, r.forbidden)
	backend = r.withOutOfBandNotifier(ctx, backend, spec.Name)
	engine := r.newEngine(backend)

	plan, err := engine.PlanApply(ctx, built.ReleaseFile, built.Charts, charts.PlanOptions{
		// The controller's own namespace. The command line stamps "apply/", so
		// a release file applied by hand and an application reconciled here can
		// never claim each other's releases — and classifying against this id
		// rather than the file's is what lets this application recognise the
		// releases it installed itself.
		Owner:    application.OwnerID(r.controller, spec.Name),
		ReadFile: built.ReadFile,
	})
	if err != nil {
		return fmt.Errorf("planning: %w", err)
	}

	live := r.liveDrift(ctx, spec, backend, plan)
	// Two lists, because reporting and acting are not the same set: everything
	// found is reported, and only what a redeploy could actually put right is
	// acted on.
	drifted := driftedReleases(plan, live, detected)
	fixable := driftedReleases(plan, live, convergeable)

	// Read before recording: record overwrites the states this compares
	// against, so asking afterwards would always find them equal and drift
	// would never look like a transition.
	was := r.syncState(spec.Name)
	wasLive := r.driftState(spec.Name)
	r.record(spec.Name, backend, plan, checkout.Revision, nil, live)

	// Both notifications come before the gate below, because something found and
	// not correctable is exactly the case an operator has to be told about: the
	// controller will not act on it, so nothing else will say so.
	//
	// The git one is gated on the plan rather than on the state, because the
	// state now goes out-of-sync for live drift too. One change must not produce
	// two notifications saying different things about it.
	install, upgrade, _ := plan.Counts()
	if install+upgrade > 0 && was != application.SyncOutOfSync {
		// Only on the transition. A manual-policy application sits out of
		// sync indefinitely by design, and notifying every tick would train
		// an operator to ignore the one that matters.
		notify.Dispatch(ctx, notify.Event{
			Application: spec.Name,
			Type:        notify.DriftDetected,
			Revision:    checkout.Revision,
			At:          r.now(),
		})
	}
	if len(drifted) > 0 && wasLive != application.DriftStateDetected {
		notify.Dispatch(ctx, notify.Event{
			Application: spec.Name,
			Type:        notify.LiveDriftDetected,
			Revision:    checkout.Revision,
			At:          r.now(),
			Message:     "the running services of " + strings.Join(drifted, ", ") + " no longer match the repository",
		})
	}

	if install+upgrade+len(fixable) == 0 {
		return nil
	}
	if !spec.SyncPolicy.Automated && !force {
		return nil
	}
	if err := checkCompat(plan); err != nil {
		return err
	}
	return r.apply(ctx, spec, backend, engine, plan, built, checkout, fixable)
}

// checkCompat refuses a plan containing a release this build's chart engine is
// too old for.
//
// PlanApply records the finding and never acts on it, because the layer that
// knows whether refusing is appropriate is the caller. Here it always is: this
// is an unattended reconciler with no operator to ask, and a chart declaring an
// engine floor usually fails inside Render anyway — with whatever error the
// missing feature happens to produce, minutes later and pointing at the wrong
// thing. Refusing names the version to upgrade to instead.
//
// The whole plan is gated before any of it is applied, matching PlanApply's
// contract and CE's own gate in cli/apply.go: a release that cannot run must
// not leave the swarm half converged. Releases that would be unchanged are
// exempt — they are already deployed, and applying will not touch them.
func checkCompat(plan *charts.Plan) error {
	var refused []string
	for _, release := range plan.Releases {
		if release.Action == charts.ActionUnchanged {
			continue
		}
		if release.Compat.Status == charts.CompatIncompatible {
			refused = append(refused, release.Compat.Message(""))
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to apply: %s", strings.Join(refused, "; "))
}

// apply deploys the plan and then re-plans.
//
// Re-planning costs a second render, and it is worth it: it is the difference
// between reporting that a sync was attempted and observing that it landed. It
// also surfaces a chart whose render is not deterministic — one that would
// otherwise sit permanently out of sync, deploying on every single tick — on
// the first sync rather than never.
func (r *Reconciler) apply(ctx context.Context, spec application.Spec, backend charts.Backend, engine Engine, plan *charts.Plan, built *source.Built, checkout git.Checkout, drifted []string) error {
	started := r.now()
	notify.Dispatch(ctx, notify.Event{
		Application: spec.Name,
		Type:        notify.SyncStarted,
		Revision:    checkout.Revision,
		At:          started,
	})

	// Before the apply, for a workload where two instances at once is worse
	// than none: the departing release is gone before its replacement exists,
	// so a rename cannot make them overlap. A failure here stops the apply,
	// which is the point — proceeding would create the overlap this ordering
	// was chosen to avoid.
	if spec.SyncPolicy.PruneFirst {
		if err := r.prune(ctx, spec, backend, engine, plan.Orphaned); err != nil {
			return err
		}
	}

	results, applyErr := engine.Apply(ctx, plan, charts.InstallOptions{
		// From the plan, not recomputed: apply must stamp the same owner it
		// classified against, or a release is installed under one id and the
		// next plan goes looking for its orphans under another.
		Owner:      plan.Owner,
		Wait:       spec.SyncPolicy.Wait,
		Timeout:    time.Duration(spec.SyncPolicy.Timeout),
		HistoryMax: spec.SyncPolicy.HistoryMax,
	})
	for _, res := range results {
		r.log.Info("release applied", "application", spec.Name, "release", res.Name, "action", res.Action, "revision", res.Revision)
	}

	// After the apply because the two are disjoint — a release the plan would
	// install or upgrade is never one this compared — and because a failed
	// apply is the wrong moment to start correcting something else.
	if applyErr == nil {
		applyErr = r.converge(ctx, spec, backend, plan, drifted)
	}

	result := &application.SyncResult{
		Revision:   checkout.Revision,
		StartedAt:  started,
		FinishedAt: r.now(),
		Succeeded:  applyErr == nil,
	}
	if applyErr != nil {
		result.Error = applyErr.Error()
	}

	notified := notify.Event{
		Application: spec.Name,
		Type:        notify.SyncSucceeded,
		Revision:    checkout.Revision,
		At:          result.FinishedAt,
	}
	if applyErr != nil {
		notified.Type = notify.SyncFailed
		notified.Message = applyErr.Error()
	}
	notify.Dispatch(ctx, notified)

	if applyErr != nil {
		r.recordResult(spec.Name, result)
		return fmt.Errorf("applying: %w", applyErr)
	}

	// After the apply and before the confirming re-plan. After, so a failed
	// apply leaves the old release running rather than already deleted — the
	// recoverable half of the two failure modes, and the right default for
	// everything that is not harmed by a moment of overlap. Before, so the
	// re-plan below observes the swarm as prune left it rather than reporting
	// the releases it has just deleted straight back as orphans.
	if !spec.SyncPolicy.PruneFirst {
		if err := r.prune(ctx, spec, backend, engine, plan.Orphaned); err != nil {
			r.recordResult(spec.Name, result)
			return err
		}
	}

	after, err := engine.PlanApply(ctx, built.ReleaseFile, built.Charts, charts.PlanOptions{
		Owner:    application.OwnerID(r.controller, spec.Name),
		ReadFile: built.ReadFile,
	})
	if err != nil {
		// The deploy landed; only the confirmation failed. Reporting the sync
		// as failed would be wrong, so the result stands and the error is the
		// reconcile's.
		r.recordResult(spec.Name, result)
		return fmt.Errorf("re-planning after apply: %w", err)
	}
	// Re-compared, not carried forward: the correction above is exactly the
	// thing this needs to confirm, and reporting the pre-apply comparison would
	// leave an application that has just been put right still reading drifted.
	r.record(spec.Name, backend, after, checkout.Revision, result, r.liveDrift(ctx, spec, backend, after))
	return nil
}

// converge redeploys the releases whose running services no longer match the
// repository.
//
// It cannot go through the engine. Engine.Apply skips a release it planned as
// unchanged, and a live-drifted release is unchanged by construction — that is
// what made it comparable in the first place. Forcing its action to Upgrade to
// get it through would write a new chart revision on every correction, filling
// the history with identical entries and churning HistoryMax. So this deploys
// the release's manifest directly, and no revision is recorded: the desired
// state did not change, the swarm was put back to it. The record is
// Sync.LastSync and the event below.
//
// The resolve mode is the one the apply path uses, which is what keeps a
// correction a correction. Asking the registry to re-resolve here would let a
// moving tag change the running image as a side effect of fixing someone's
// replica count.
//
// Failures are collected rather than returned at the first: one release that
// will not converge is no reason to leave the others drifted, and the caller
// fails the sync on whatever comes back.
func (r *Reconciler) converge(ctx context.Context, spec application.Spec, backend charts.Backend, plan *charts.Plan, drifted []string) error {
	if len(drifted) == 0 {
		return nil
	}

	manifests := make(map[string]string, len(plan.Releases))
	for _, rp := range plan.Releases {
		manifests[rp.Name] = rp.Manifest
	}

	var errs []error
	var converged []string
	for _, release := range drifted {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := backend.DeployStack(release, manifests[release], ""); err != nil {
			errs = append(errs, fmt.Errorf("converging release %q: %w", release, err))
			continue
		}
		if err := r.awaitConverged(ctx, spec, backend, release); err != nil {
			// Deployed but not settled. Not counted as converged: the write
			// landed, the correction did not finish, and the sync says so.
			errs = append(errs, fmt.Errorf("converging release %q: %w", release, err))
			continue
		}
		converged = append(converged, release)
		r.log.Warn("redeployed a release whose running services had been changed outside the repository",
			"application", spec.Name, "release", release)
	}

	if len(converged) > 0 {
		notify.Dispatch(ctx, notify.Event{
			Application: spec.Name,
			Type:        notify.DriftConverged,
			At:          r.now(),
			Message:     "redeployed " + strings.Join(converged, ", "),
		})
	}
	return errors.Join(errs...)
}

// convergePollInterval is how often a correction re-reads the swarm while
// waiting for it to settle. A variable so tests do not have to spend it.
var convergePollInterval = 2 * time.Second

// defaultConvergeTimeout bounds a wait the application did not bound itself,
// matching the chart engine's own default for the same policy.
const defaultConvergeTimeout = 5 * time.Minute

// awaitConverged blocks until a corrected release has settled, when the
// application's sync policy asks it to.
//
// A drift correction is a deploy, so syncPolicy.wait has to mean the same thing
// for it as for any other. Without this it did not: converging writes through
// Backend.DeployStack rather than Engine.Apply — because the engine skips a
// release it planned as unchanged, which every drifted release is — and the
// engine is where the wait lives (charts.Engine.deployAndRecord). So a
// correction returned the moment the daemon accepted the write.
//
// That mattered for reporting rather than for data. A correction that deploys
// and then wedges recorded a successful sync, and because health turns a slow
// rollout into a broken one only on the back of a failed sync
// (health.serviceHealth), the release then read as progressing indefinitely
// instead of degraded — the exact failure the wait policy exists to convert into
// something an operator is told about.
//
// It also gives the converge path the ordering the docs already promise, since
// releases are corrected in plan order: with wait set, a later release is not
// redeployed until an earlier one it depends on is live.
//
// The judgement is the chart engine's own exported rollup, not a second copy.
// Every rule in it was corrected at least once — the running count by actual
// rather than desired state, the target over active nodes, a completed one-shot
// job, the stability window measured from task creation — and a reimplementation
// would diverge silently in both directions.
func (r *Reconciler) awaitConverged(ctx context.Context, spec application.Spec, backend charts.Backend, release string) error {
	if !spec.SyncPolicy.Wait {
		return nil
	}

	timeout := time.Duration(spec.SyncPolicy.Timeout)
	if timeout <= 0 {
		timeout = defaultConvergeTimeout
	}
	deadline := r.now().Add(timeout)

	for {
		switch c := charts.Rollup(backend.StackServices(release)); c.Phase {
		case charts.PhaseWedged:
			// Swarm has given up and will not continue on its own, so waiting
			// out the deadline would only delay the same answer.
			return fmt.Errorf("release %q did not converge: %s", release, c.Reason)
		case charts.PhaseConverged:
			return nil
		}
		if !r.now().Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for release %q to converge", timeout, release)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(convergePollInterval):
		}
	}
}

// prune deletes the releases this application used to declare and no longer
// does, when its sync policy asks for it.
//
// orphaned is charts' own classification and is already the right list: it
// names releases carrying this application's owner stamp that its release file
// has stopped declaring. A release belonging to another application, or applied
// from the command line, fails that ownership check and is reported as
// unmanaged instead — so nothing here can reach outside the application it is
// reconciling.
//
// The sync is not failed by a prune that does not work. The deploy landed, and
// what is left behind is a release that will be classified as orphaned again on
// the next tick and retried then. The error is still returned, as the re-plan's
// is, because the reconcile did not do everything it set out to.
func (r *Reconciler) prune(ctx context.Context, spec application.Spec, backend charts.Backend, engine Engine, orphaned []string) error {
	if !spec.SyncPolicy.Prune || len(orphaned) == 0 {
		return nil
	}

	var errs []error
	var pruned []string
	for _, release := range orphaned {
		result, err := prune.Release(ctx, backend, engine, release, spec.SyncPolicy.PruneVolumes)
		if err != nil {
			errs = append(errs, fmt.Errorf("pruning release %q: %w", release, err))
			continue
		}
		pruned = append(pruned, release)
		r.log.Warn("pruned a release the application no longer declares",
			"application", spec.Name, "release", release, "volumes", spec.SyncPolicy.PruneVolumes)
		if result != nil && len(result.OrphanedNetworks) > 0 {
			// Left in place by the chart engine because they may be shared.
			// Reported, or an operator believes the cleanup was complete.
			r.log.Warn("networks created for the release were left in place; they may be shared",
				"application", spec.Name, "release", release, "networks", result.OrphanedNetworks)
		}
	}

	if len(pruned) > 0 {
		notify.Dispatch(ctx, notify.Event{
			Application: spec.Name,
			Type:        notify.ResourcesPruned,
			At:          r.now(),
			Message:     "pruned " + strings.Join(pruned, ", "),
		})
	}
	err := errors.Join(errs...)
	if err != nil {
		notify.Dispatch(ctx, notify.Event{
			Application: spec.Name,
			Type:        notify.PruneFailed,
			At:          r.now(),
			Message:     err.Error(),
		})
	}
	return err
}

// syncState reports what was last observed for an application, or unknown if it
// has been removed from the set.
func (r *Reconciler) syncState(app string) application.SyncState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.apps[app]; ok {
		return e.status.Sync.State
	}
	return application.SyncUnknown
}

// currentSpec returns an application's spec as it stands now, or the zero spec
// if it has been removed. The loop reads through it so a Replace takes effect on
// the next tick.
func (r *Reconciler) currentSpec(app string) application.Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.apps[app]; ok {
		return e.spec
	}
	return application.Spec{}
}

// record stores the status a plan implies. A nil result leaves whatever the
// last sync recorded in place. An application removed while its sync was in
// flight is skipped, so a late write does not resurrect it.
//
// live is the per-release comparison against what is running, keyed by release
// name, and is nil for an application not using that mode — which leaves the
// whole axis absent from the status rather than present and empty.
func (r *Reconciler) record(app string, backend charts.Backend, plan *charts.Plan, revision string, result *application.SyncResult, live map[string]*application.ReleaseDrift) {
	sync, releases := drift.FromPlan(plan)
	sync.Revision = revision

	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.apps[app]
	if !ok {
		return
	}

	sync.LastSync = e.status.Sync.LastSync
	if result != nil {
		sync.LastSync = result
	}

	// Read the swarm before taking a view on it. A sync that did not succeed is
	// what separates a rollout that is slow from one that is broken, so the
	// health of each release is decided against the outcome recorded above
	// rather than against the previous tick's.
	syncFailed := sync.LastSync != nil && !sync.LastSync.Succeeded
	for i := range releases {
		rel := &releases[i]
		var states []charts.ServiceState
		if rel.Action != application.ActionInstall {
			states = backend.StackServices(rel.Name)
		}
		rel.Health, rel.Services = health.Release(health.Input{
			States: states,
			// A release the plan would install is declared and not deployed.
			// That is knowable from the plan, so it costs no call to the swarm.
			Installed:  rel.Action != application.ActionInstall,
			SyncFailed: syncFailed,
		})

		// Fold the live comparison into the sync axis. A release whose running
		// services no longer match the repository is out of sync in the plainest
		// sense of the word, and saying so is what makes the sync button, the
		// CLI's exit code and any alerting already watching that field work
		// without knowing this mode exists. What drifted, and that it was the
		// swarm rather than git that moved, is the Drift axis beside it.
		rel.Drift = live[rel.Name]
		if rel.Drift != nil && rel.Drift.State == application.DriftStateDetected {
			rel.Sync = application.SyncOutOfSync
			sync.State = application.SyncOutOfSync
			sync.Summary.Drifted++
		}
	}

	e.status = application.Status{
		Sync:       sync,
		Health:     health.Application(releases),
		Drift:      drift.Application(releases),
		Releases:   releases,
		ObservedAt: r.now(),
	}
	e.plan = plan
}

// driftState reports the live-drift state last observed for an application.
// Unknown covers both "not using this mode" and "removed from the set", neither
// of which is a transition worth reporting.
func (r *Reconciler) driftState(app string) application.DriftState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.apps[app]; ok && e.status.Drift != nil {
		return e.status.Drift.State
	}
	return application.DriftStateUnknown
}

func (r *Reconciler) recordResult(app string, result *application.SyncResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.apps[app]
	if !ok {
		return
	}
	e.status.Sync.LastSync = result
	e.status.ObservedAt = r.now()
}

// setError records why a reconcile failed without discarding what was last
// observed. A repository that cannot be reached does not make the swarm's
// state unknown — it makes it unverified, which is what the error says.
func (r *Reconciler) setError(app string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.apps[app]
	if !ok {
		return
	}
	e.status.Error = err.Error()
	e.status.ObservedAt = r.now()
}

// View returns one application's spec and last observed status.
func (r *Reconciler) View(app string) (application.View, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.apps[app]
	if !ok {
		return application.View{}, false
	}
	return application.View{Spec: e.spec, Status: e.status}, true
}

// Views returns every application, in the order they were declared or added.
func (r *Reconciler) Views() []application.View {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]application.View, 0, len(r.order))
	for _, name := range r.order {
		e := r.apps[name]
		out = append(out, application.View{Spec: e.spec, Status: e.status})
	}
	return out
}

// ErrNotPlanned is returned when an application has not been reconciled yet, so
// there is nothing to diff.
var ErrNotPlanned = errors.New("no plan yet")

// Diffs returns the manifest changes the last plan found. It does not
// re-render: what it reports is what the status reports, which is the point.
func (r *Reconciler) Diffs(app string) ([]application.ReleaseDiff, error) {
	r.mu.RLock()
	e, ok := r.apps[app]
	var plan *charts.Plan
	if ok {
		plan = e.plan
	}
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no such application %q", app)
	}
	if plan == nil {
		return nil, ErrNotPlanned
	}
	return drift.Diffs(plan), nil
}

// History returns the recorded revisions of every release the application
// declares, newest first.
//
// It reads the swarm rather than the status cache: history is the one thing
// that survives a controller restart with no database of its own, because the
// engine keeps one Docker Config per revision in Raft. Serving it from memory
// would make it disappear on exactly the restart it is most useful after.
func (r *Reconciler) History(ctx context.Context, app string) (application.History, error) {
	r.mu.RLock()
	e, ok := r.apps[app]
	var (
		spec application.Spec
		plan *charts.Plan
	)
	if ok {
		spec = e.spec
		plan = e.plan
	}
	r.mu.RUnlock()

	if !ok {
		return application.History{}, fmt.Errorf("no such application %q", app)
	}
	if plan == nil {
		return application.History{}, ErrNotPlanned
	}

	backend, err := r.swarms.Backend(ctx, spec.Destination.Swarm)
	if err != nil {
		return application.History{}, fmt.Errorf("resolving destination: %w", err)
	}
	engine := r.newEngine(backend)

	out := application.History{Releases: make([]application.ReleaseHistory, 0, len(plan.Releases))}
	for _, rp := range plan.Releases {
		revs, err := engine.History(ctx, rp.Name)
		if err != nil {
			// The engine has no record of it. That is expected precisely when
			// the plan would install it, and only then — anything else is a
			// real failure and is not worth hiding behind an empty table.
			if rp.Action == charts.ActionInstall {
				out.Releases = append(out.Releases, application.ReleaseHistory{Name: rp.Name})
				continue
			}
			return application.History{}, fmt.Errorf("reading the history of release %q: %w", rp.Name, err)
		}
		out.Releases = append(out.Releases, application.ReleaseHistory{Name: rp.Name, Revisions: revisions(revs)})
	}
	return out, nil
}

// revisions maps the engine's records to the wire shape, newest first.
//
// The engine returns them ascending, which is the order they were written; a
// history view reads the other way round, and reversing here means every client
// does not.
func revisions(revs []charts.Release) []application.Revision {
	out := make([]application.Revision, 0, len(revs))
	for i := len(revs) - 1; i >= 0; i-- {
		rel := revs[i]
		out = append(out, application.Revision{
			Revision: rel.Revision,
			Chart:    rel.Chart.Name,
			Version:  rel.Chart.Version,
			Status:   rel.Status,
			Created:  rel.Created,
			Owner:    rel.Owner,
		})
	}
	return out
}
