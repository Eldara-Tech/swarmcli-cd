// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package reconcile is the pull loop: for each application, fetch the
// repository, render it, plan against the swarm, and — when the sync policy
// says so — apply.
//
// Each application runs on its own schedule in its own goroutine. One
// application whose repository is unreachable must not stall the others, and a
// shared queue would make exactly that happen.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/drift"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
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
	PlanApply(ctx context.Context, rf *charts.ReleaseFile, src charts.ChartSource) (*charts.Plan, error)
	Apply(ctx context.Context, plan *charts.Plan, opts charts.InstallOptions) ([]charts.ApplyResult, error)
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
}

// Reconciler runs the loop and holds what it last observed.
type Reconciler struct {
	apps      []application.Spec
	fetch     Fetcher
	build     Builder
	swarms    swarms.Registry
	newEngine func(charts.Backend) Engine
	interval  time.Duration
	log       *slog.Logger
	now       func() time.Time

	// syncing serialises work for one application, so a manual sync and a
	// scheduled tick cannot render or deploy the same application at once.
	syncing map[string]*sync.Mutex

	mu     sync.RWMutex
	status map[string]application.Status
	// plans holds the last plan per application so the diff endpoint can be
	// served without re-rendering. A plan carries whole manifests, which is
	// why it is kept per application rather than per revision.
	plans map[string]*charts.Plan
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
		apps:      apps,
		fetch:     o.Fetcher,
		build:     o.Builder,
		swarms:    o.Swarms,
		newEngine: o.NewEngine,
		interval:  o.Interval,
		log:       o.Log,
		now:       o.Now,
		syncing:   make(map[string]*sync.Mutex, len(apps)),
		status:    make(map[string]application.Status, len(apps)),
		plans:     make(map[string]*charts.Plan, len(apps)),
	}
	for _, spec := range apps {
		r.syncing[spec.Name] = &sync.Mutex{}
		r.status[spec.Name] = application.Status{Sync: application.Sync{State: application.SyncUnknown}}
	}
	return r
}

// Run reconciles until ctx is cancelled.
//
// Destinations are resolved first and a failure is fatal. The config loader
// deliberately does not validate them — only the swarm registry knows what it
// can reach — so this is where a typo in a swarm name becomes a startup error
// rather than a per-application failure discovered three minutes later.
func (r *Reconciler) Run(ctx context.Context) error {
	if err := r.checkDestinations(ctx); err != nil {
		return err
	}

	var wg sync.WaitGroup
	for i := range r.apps {
		wg.Add(1)
		go func(spec application.Spec) {
			defer wg.Done()
			r.loop(ctx, spec)
		}(r.apps[i])
	}
	wg.Wait()
	return ctx.Err()
}

func (r *Reconciler) checkDestinations(ctx context.Context) error {
	for _, spec := range r.apps {
		if _, err := r.swarms.Backend(ctx, spec.Destination.Swarm); err != nil {
			return fmt.Errorf("application %q: destination: %w", spec.Name, err)
		}
	}
	return nil
}

// loop reconciles one application on its own schedule.
func (r *Reconciler) loop(ctx context.Context, spec application.Spec) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if err := r.Sync(ctx, spec.Name); err != nil {
			// A cancelled context is a shutdown, not a failure to back off
			// from.
			if ctx.Err() != nil {
				return
			}
			failures++
			r.log.Error("reconcile failed", "application", spec.Name, "failures", failures, "error", err)
		} else {
			failures = 0
		}

		timer.Reset(backoff(r.intervalFor(spec), failures))
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
	spec, ok := r.spec(app)
	if !ok {
		return fmt.Errorf("no such application %q", app)
	}

	lock := r.syncing[app]
	lock.Lock()
	defer lock.Unlock()

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
	engine := r.newEngine(backend)

	plan, err := engine.PlanApply(ctx, built.ReleaseFile, built.Charts)
	if err != nil {
		return fmt.Errorf("planning: %w", err)
	}

	// Read before recording: record overwrites the state this compares
	// against, so asking afterwards would always find them equal and drift
	// would never look like a transition.
	was := r.syncState(spec.Name)
	r.record(spec.Name, plan, checkout.Revision, nil)

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
	return r.apply(ctx, spec, engine, plan, built, checkout)
}

// apply deploys the plan and then re-plans.
//
// Re-planning costs a second render, and it is worth it: it is the difference
// between reporting that a sync was attempted and observing that it landed. It
// also surfaces a chart whose render is not deterministic — one that would
// otherwise sit permanently out of sync, deploying on every single tick — on
// the first sync rather than never.
func (r *Reconciler) apply(ctx context.Context, spec application.Spec, engine Engine, plan *charts.Plan, built *source.Built, checkout git.Checkout) error {
	started := r.now()
	notify.Dispatch(ctx, notify.Event{
		Application: spec.Name,
		Type:        notify.SyncStarted,
		Revision:    checkout.Revision,
		At:          started,
	})

	results, applyErr := engine.Apply(ctx, plan, charts.InstallOptions{
		// The controller's own namespace. The command line stamps "apply/",
		// so a release file applied by hand and an application reconciled
		// here can never claim each other's releases.
		Owner:      "cd/" + spec.Name,
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

	after, err := engine.PlanApply(ctx, built.ReleaseFile, built.Charts)
	if err != nil {
		// The deploy landed; only the confirmation failed. Reporting the sync
		// as failed would be wrong, so the result stands and the error is the
		// reconcile's.
		r.recordResult(spec.Name, result)
		return fmt.Errorf("re-planning after apply: %w", err)
	}
	r.record(spec.Name, after, checkout.Revision, result)
	return nil
}

// syncState reports what was last observed for an application.
func (r *Reconciler) syncState(app string) application.SyncState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status[app].Sync.State
}

// record stores the status a plan implies. A nil result leaves whatever the
// last sync recorded in place.
func (r *Reconciler) record(app string, plan *charts.Plan, revision string, result *application.SyncResult) {
	sync, releases := drift.FromPlan(plan)
	sync.Revision = revision

	r.mu.Lock()
	defer r.mu.Unlock()

	previous := r.status[app]
	sync.LastSync = previous.Sync.LastSync
	if result != nil {
		sync.LastSync = result
	}
	r.status[app] = application.Status{
		Sync: sync,
		// Health is filled by the health rollup; until then it is honestly
		// unknown rather than optimistically healthy.
		Health:     application.Health{State: application.HealthUnknown},
		Releases:   releases,
		ObservedAt: r.now(),
	}
	r.plans[app] = plan
}

func (r *Reconciler) recordResult(app string, result *application.SyncResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.status[app]
	status.Sync.LastSync = result
	status.ObservedAt = r.now()
	r.status[app] = status
}

// setError records why a reconcile failed without discarding what was last
// observed. A repository that cannot be reached does not make the swarm's
// state unknown — it makes it unverified, which is what the error says.
func (r *Reconciler) setError(app string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.status[app]
	status.Error = err.Error()
	status.ObservedAt = r.now()
	r.status[app] = status
}

func (r *Reconciler) spec(app string) (application.Spec, bool) {
	for _, s := range r.apps {
		if s.Name == app {
			return s, true
		}
	}
	return application.Spec{}, false
}

// View returns one application's spec and last observed status.
func (r *Reconciler) View(app string) (application.View, bool) {
	spec, ok := r.spec(app)
	if !ok {
		return application.View{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return application.View{Spec: spec, Status: r.status[app]}, true
}

// Views returns every application, in the order they were declared.
func (r *Reconciler) Views() []application.View {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]application.View, 0, len(r.apps))
	for _, spec := range r.apps {
		out = append(out, application.View{Spec: spec, Status: r.status[spec.Name]})
	}
	return out
}

// ErrNotPlanned is returned when an application has not been reconciled yet, so
// there is nothing to diff.
var ErrNotPlanned = errors.New("no plan yet")

// Diffs returns the manifest changes the last plan found. It does not
// re-render: what it reports is what the status reports, which is the point.
func (r *Reconciler) Diffs(app string) ([]application.ReleaseDiff, error) {
	if _, ok := r.spec(app); !ok {
		return nil, fmt.Errorf("no such application %q", app)
	}
	r.mu.RLock()
	plan := r.plans[app]
	r.mu.RUnlock()

	if plan == nil {
		return nil, ErrNotPlanned
	}
	return drift.Diffs(plan), nil
}
