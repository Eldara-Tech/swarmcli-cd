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
	"strings"
	"sync"
	"time"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/drift"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/health"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
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
	fetch     Fetcher
	build     Builder
	swarms    swarms.Registry
	newEngine func(charts.Backend) Engine
	interval  time.Duration
	log       *slog.Logger
	now       func() time.Time
	regAuth   map[string]regauth.Resolver
	forbidden map[string]struct{}

	mu sync.RWMutex
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

	r := &Reconciler{
		fetch:     o.Fetcher,
		build:     o.Builder,
		swarms:    o.Swarms,
		newEngine: o.NewEngine,
		interval:  o.Interval,
		log:       o.Log,
		now:       o.Now,
		regAuth:   o.RegistryAuth,
		forbidden: o.ForbiddenSecretMounts,
		apps:      make(map[string]*appEntry, len(apps)),
		order:     make([]string, 0, len(apps)),
	}
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
	backend = withRegistryAuth(backend, r.regAuth[spec.Name])
	backend = withForbiddenSecrets(backend, r.forbidden)
	engine := r.newEngine(backend)

	plan, err := engine.PlanApply(ctx, built.ReleaseFile, built.Charts, charts.PlanOptions{
		// The controller's own namespace. The command line stamps "apply/", so
		// a release file applied by hand and an application reconciled here can
		// never claim each other's releases — and classifying against this id
		// rather than the file's is what lets this application recognise the
		// releases it installed itself.
		Owner:    ownerID(spec.Name),
		ReadFile: built.ReadFile,
	})
	if err != nil {
		return fmt.Errorf("planning: %w", err)
	}

	// Read before recording: record overwrites the state this compares
	// against, so asking afterwards would always find them equal and drift
	// would never look like a transition.
	was := r.syncState(spec.Name)
	r.record(spec.Name, backend, plan, checkout.Revision, nil)

	install, upgrade, _ := plan.Counts()
	if install+upgrade == 0 {
		return nil
	}
	if was != application.SyncOutOfSync {
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

	if !spec.SyncPolicy.Automated && !force {
		return nil
	}
	if err := checkCompat(plan); err != nil {
		return err
	}
	return r.apply(ctx, spec, backend, engine, plan, built, checkout)
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
// ownerID is the id this controller stamps its releases with and classifies
// them against. It is per application rather than per controller: several
// applications share one swarm, and a per-controller id would make each of them
// report the others' releases as its own orphans.
func ownerID(app string) string { return "cd/" + app }

func (r *Reconciler) apply(ctx context.Context, spec application.Spec, backend charts.Backend, engine Engine, plan *charts.Plan, built *source.Built, checkout git.Checkout) error {
	started := r.now()
	notify.Dispatch(ctx, notify.Event{
		Application: spec.Name,
		Type:        notify.SyncStarted,
		Revision:    checkout.Revision,
		At:          started,
	})

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

	after, err := engine.PlanApply(ctx, built.ReleaseFile, built.Charts, charts.PlanOptions{
		Owner:    ownerID(spec.Name),
		ReadFile: built.ReadFile,
	})
	if err != nil {
		// The deploy landed; only the confirmation failed. Reporting the sync
		// as failed would be wrong, so the result stands and the error is the
		// reconcile's.
		r.recordResult(spec.Name, result)
		return fmt.Errorf("re-planning after apply: %w", err)
	}
	r.record(spec.Name, backend, after, checkout.Revision, result)
	return nil
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
func (r *Reconciler) record(app string, backend charts.Backend, plan *charts.Plan, revision string, result *application.SyncResult) {
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
	}

	e.status = application.Status{
		Sync:       sync,
		Health:     health.Application(releases),
		Releases:   releases,
		ObservedAt: r.now(),
	}
	e.plan = plan
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
