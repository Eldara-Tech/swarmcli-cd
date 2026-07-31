// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package appset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/regauth"
)

// DefaultInterval is how often the app set is re-read. It matches the
// reconciler's own default: an operator tuning "how quickly does a commit take
// effect" should not have to learn that the two tiers move at different speeds.
const DefaultInterval = 3 * time.Minute

// Reconciler is the running set the loop steers. *reconcile.Reconciler
// implements it.
//
// Add and Replace are separate rather than one upsert because the distinction is
// the loop's to make: replacing keeps an application's recorded status and its
// running loop, and adding starts one. A caller that could not tell them apart
// would silently restart every application on every change.
type Reconciler interface {
	Views() []application.View
	Add(spec application.Spec) error
	Replace(spec application.Spec) error
	Remove(name string) error
	SetRegistryAuth(app string, resolver regauth.Resolver)
}

// LoopOptions tunes a Loop. Everything has a working default.
type LoopOptions struct {
	// Mode labels how the set is sourced for the status endpoint: "static",
	// "git" or "path". The source does not name itself — the bootstrap decides
	// what to call the thing it configured.
	Mode string
	// Source is where that set lives, for the status endpoint to report: the
	// repository and revision, or the directory. Like Mode it is passed in
	// rather than derived, because it is the bootstrap anchor and describing it
	// is the bootstrap's to do.
	Source   string
	Interval time.Duration
	Log      *slog.Logger

	// Credentials resolves the image-pull credential of an application joining
	// or changing in the set. The default reads the Docker secret the
	// application's registryAuth names, which is the same thing the controller
	// does at startup for the applications it booted with.
	Credentials func(spec application.Spec) (regauth.Resolver, error)

	// Pruner deletes the resources of an application that has left the set.
	// Nil is report-only — the default, and what every deployment that has not
	// asked for prune gets.
	Pruner Pruner

	// Reclaimer deletes the on-disk caches — the clone and the chart cache — of
	// an application that has left the set. Unlike Pruner it is not an opt-in:
	// it deletes nothing that is deployed and nothing that cannot be rebuilt
	// from git. Nil is a loop with no data directory to sweep, which is what a
	// test wants.
	Reclaimer Reclaimer
}

// Pruner deletes the deployed resources of applications absent from the set it
// is given. *prune.Pruner implements it.
//
// An interface rather than the concrete type because it is the one seam in this
// loop that deletes things, and a test of the loop's guards must be able to
// assert that it was never called.
type Pruner interface {
	Departed(ctx context.Context, desired, declared []string) ([]string, error)
}

// Reclaimer deletes the on-disk caches of applications absent from the set it
// is given. *reclaim.Sweeper implements it.
//
// An interface for the reason Pruner is one: it is the other thing in this loop
// that removes something, and a test has to be able to read exactly what it was
// told to keep.
type Reclaimer interface {
	Sweep(keep []string) error
}

// Loop keeps the running set equal to the app set.
//
// On each tick it loads the set and diffs it by application name against what
// is actually running, adding, replacing and removing accordingly. A load that
// fails changes nothing: the last-good set keeps running and the reason is
// reported.
//
// It is deliberately not the thing that decides an application is out of sync —
// that is the reconciler's, per application, on its own schedule. This loop only
// decides which applications there are.
type Loop struct {
	src    *Loader
	rec    Reconciler
	mode   string
	source string

	interval  time.Duration
	log       *slog.Logger
	creds     func(application.Spec) (regauth.Resolver, error)
	pruner    Pruner
	reclaimer Reclaimer

	mu sync.RWMutex
	// lastErr is why the last attempt failed, and stale reports that the load
	// itself failed over a set that is already running — so what is running is
	// last-good rather than what the source now says. An error without stale is
	// either a set that loaded and could not be entirely applied, or a first
	// load that failed and left nothing running at all.
	lastErr string
	stale   bool
	// orphaned names applications removed from the set, in the order they left.
	orphaned []string
	// pruned names applications whose resources have been deleted, in the order
	// they went. With prune enabled a departure passes through orphaned and out
	// again within the same pass, so the two lists are disjoint by construction.
	pruned []string
	// pruneHeldBy names the applications whose first reconcile the last sweep
	// waited on. Nil once a sweep has run.
	pruneHeldBy []string
}

// NewLoop returns a Loop driving rec from src.
func NewLoop(src *Loader, rec Reconciler, o LoopOptions) *Loop {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Credentials == nil {
		o.Credentials = mountedCredential
	}
	return &Loop{
		src: src, rec: rec, mode: o.Mode, source: o.Source,
		interval: o.Interval, log: o.Log, creds: o.Credentials,
		pruner: o.Pruner, reclaimer: o.Reclaimer,
	}
}

// Run keeps the running set current until ctx is cancelled.
//
// There is no backoff. A failing load is one small file read again in three
// minutes, and backing off would mean the fix an operator has just committed
// takes longer to land the longer the mistake went unnoticed — which is the
// wrong way round for the one loop whose job is to pick up corrections.
func (l *Loop) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}

		if err := l.Once(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Logged here and reported on the status endpoint; it is not a
			// reason to stop, which is the entire point of keeping last-good.
			l.log.Error("app-set reconcile failed", "error", err)
		}
		timer.Reset(l.interval)
	}
}

// Once loads the app set and brings the running set to it. It is what Run
// calls, exported so a caller — a test, or a webhook later — can drive one pass
// without waiting for a tick.
//
// The diff runs on every pass, not only when the file has changed. What is
// being reconciled towards is the desired state, not the last edit to it: an
// application that could not be started last tick is still missing, and a loop
// that only acted on changes would have recorded it as added once and never
// mentioned it again. Diffing an unchanged set costs a map and a comparison per
// application and almost always decides to do nothing.
func (l *Loop) Once(ctx context.Context) error {
	file, changed, err := l.src.Load(ctx)
	if err != nil {
		// Stale means what is running is a last-good set. A controller that has
		// never loaded one has nothing running and nothing to be stale about —
		// a louder problem than a refused commit, not a quieter one, and
		// reporting it as stale would file it under the wrong heading. The zero
		// application count beside the error is what says it.
		l.record(err, l.src.Current() != nil)
		return err
	}
	if changed {
		revision, _ := l.src.LastLoad()
		l.log.Info("app set loaded", "applications", len(file.Applications), "revision", revision)
	}

	err = l.apply(file.Applications)
	// Only behind a clean apply. A set that loaded but could not be brought up
	// is a set whose running state is not yet the declared one, and deletions
	// derived from it would be acting on a diff that is still half-applied.
	if err == nil {
		err = l.pruneDeparted(ctx, file.Applications)
	}
	// Deliberately not behind that gate. What this deletes is a cache rebuilt on
	// the next fetch, and it is keyed on the set the reconciler actually holds
	// rather than on the file — so an application whose removal failed keeps its
	// clone, and one whose add failed loses only a cache. Letting the data
	// directory fill because some unrelated application will not start is the
	// worse failure by a distance, and the volume it fills is the manager's.
	if rerr := l.reclaimDeparted(); rerr != nil {
		err = errors.Join(err, rerr)
	}
	// Recorded either way, so a pass that succeeded clears a previous failure —
	// including a pass that found nothing to do, because the set having become
	// readable again is itself the news.
	l.record(err, false)
	return err
}

// pruneDeparted deletes the resources of applications this set no longer
// declares, when the deployment asked for that.
//
// Two of the three guards against a controller mistaking a broken app set for
// an empty one are in where this is called from: a pass that failed to load
// returns before reaching it, and a pass whose apply failed skips it. The third
// is the pruner's own refusal to act on a set that declares no applications.
//
// The fourth guard is here, and it is about a different mistake: an application
// that has joined the set but not yet reconciled has not told the controller
// which releases it declares, and the sweep decides what to delete from exactly
// that. Renaming an application is the case it exists for (#62). The rename
// arrives as one departure and one arrival, apply starts the new application's
// loop and returns, and this runs immediately afterwards while that loop is
// still fetching git — so the release the departed name still owns is swept
// before its successor can claim it, and with pruneVolumes the data goes too.
// A sweep is one config list against a clone and a render, so it wins that race
// essentially always; it is not a narrow window.
//
// So the sweep waits for every application to have planned once. It is over the
// applications the reconciler currently holds rather than over remembered
// departures, which is what makes it survive a restart: after one, every
// application is unreconciled and the wait covers all of them, where a
// remembered departure would have been forgotten precisely when it was needed.
//
// The names go in even when the sweep also returns an error: it reports what it
// managed to delete alongside what it could not, and a partial success that
// went unrecorded would leave the status endpoint claiming resources are still
// there when they are gone.
func (l *Loop) pruneDeparted(ctx context.Context, desired []application.Spec) error {
	if l.pruner == nil {
		return nil
	}

	// One snapshot for both questions below, so the set the gate clears is the
	// same set the releases are read from.
	views := l.rec.Views()

	if held := unreconciled(views); len(held) > 0 {
		l.recordPruneHeld(held)
		// Info and every pass, not once: an application that can never plan
		// holds the sweep indefinitely, and that is a condition an operator has
		// to be able to find rather than one that scrolled past at startup.
		l.log.Info("prune held: an application has not reconciled yet, so what it declares is not known",
			"applications", held)
		return nil
	}
	l.recordPruneHeld(nil)

	names := make([]string, 0, len(desired))
	for _, spec := range desired {
		names = append(names, spec.Name)
	}

	pruned, err := l.pruner.Departed(ctx, names, declared(views))
	l.recordPruned(pruned)
	if err != nil {
		return fmt.Errorf("pruning departed applications: %w", err)
	}
	return nil
}

// reclaimDeparted deletes the on-disk caches of applications the reconciler no
// longer holds.
//
// Driven by the running set rather than by the file, and that is what makes it
// safe rather than merely tidy: an application whose Remove failed is still in
// it and keeps its checkout, so there is no pass in which the file has dropped a
// name that something here is still reconciling. The other direction — a sync
// the API started before the removal, which Remove neither cancels nor waits for
// (#106) — is covered by the sweep's own grace period, not by this.
func (l *Loop) reclaimDeparted() error {
	if l.reclaimer == nil {
		return nil
	}
	views := l.rec.Views()
	names := make([]string, 0, len(views))
	for _, view := range views {
		names = append(names, view.Spec.Name)
	}
	if err := l.reclaimer.Sweep(names); err != nil {
		return fmt.Errorf("reclaiming the caches of departed applications: %w", err)
	}
	return nil
}

// unreconciled names the applications that have not completed a plan, in the
// reconciler's order.
//
// SyncUnknown is the state an application is added with and the only one
// drift.FromPlan never produces, so it means "has not planned" and nothing
// else. A failed reconcile is deliberately not enough: it moves ObservedAt but
// not the sync state, and an application whose first git fetch happened to fail
// has told the controller no more about what it declares than one that has not
// run at all. Accepting the attempt would reopen the rename window for exactly
// as long as a repository is unreachable.
func unreconciled(views []application.View) []string {
	var held []string
	for _, view := range views {
		if view.Status.Sync.State == application.SyncUnknown {
			held = append(held, view.Spec.Name)
		}
	}
	return held
}

// declared names every release the applications still in the set hold,
// deduplicated and in the reconciler's order. It is what stops the sweep
// deleting a release whose stamp is stale but whose stack somebody is still
// reconciling.
//
// The gate above is what makes this trustworthy: every application has planned,
// so every one of them has reported the releases it declares, and an empty list
// here means the applications really declare nothing rather than that they have
// not looked yet.
func declared(views []application.View) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, view := range views {
		for _, rel := range view.Status.Releases {
			if _, ok := seen[rel.Name]; ok {
				continue
			}
			seen[rel.Name] = struct{}{}
			out = append(out, rel.Name)
		}
	}
	return out
}

// apply drives the reconciler to the desired set.
//
// Every step is attempted and the failures are collected rather than returned
// at the first one. The steps are independent — one application that cannot be
// added is no reason to leave a departed one reconciling — and because the diff
// is taken against what is running rather than against the last file, whatever
// failed is simply diffed again on the next tick.
func (l *Loop) apply(desired []application.Spec) error {
	add, replace, remove := diff(l.rec.Views(), desired)
	var errs []error

	// Removals first, so a set that swaps one application for another shrinks
	// before it grows.
	for _, name := range remove {
		if err := l.rec.Remove(name); err != nil {
			errs = append(errs, fmt.Errorf("removing %q: %w", name, err))
			continue
		}
		l.orphan(name)
		// Warn, not Info: something is deployed that nobody is reconciling any
		// more, and that is the sort of thing an operator finds out about
		// months later if it is whispered.
		l.log.Warn("application left the app set; its stack is left running and is no longer reconciled",
			"application", name)
	}

	for _, spec := range replace {
		if err := l.credential(spec); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := l.rec.Replace(spec); err != nil {
			errs = append(errs, fmt.Errorf("replacing %q: %w", spec.Name, err))
			continue
		}
		l.log.Info("application changed in the app set", "application", spec.Name)
	}

	for _, spec := range add {
		if err := l.credential(spec); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := l.rec.Add(spec); err != nil {
			errs = append(errs, fmt.Errorf("adding %q: %w", spec.Name, err))
			continue
		}
		l.adopt(spec.Name)
		l.log.Info("application joined the app set", "application", spec.Name)
	}

	return errors.Join(errs...)
}

// credential resolves an application's image-pull credential and hands it to the
// reconciler before the application is added or replaced.
//
// A credential that cannot be resolved fails that application and nothing else.
// The alternative — refusing the whole set — would let one application whose
// secret was never mounted block every other change in the file, and the
// per-application answer is the one the destination check already settled on.
func (l *Loop) credential(spec application.Spec) error {
	resolver, err := l.creds(spec)
	if err != nil {
		return err
	}
	// Unconditionally, including the nil an application without registryAuth
	// resolves to: that is what clears the credential of one that has just
	// stopped declaring it.
	l.rec.SetRegistryAuth(spec.Name, resolver)
	return nil
}

// mountedCredential is the default Credentials: the application's own Docker
// secret, read from where Swarm mounts it. It goes through regauth.Load for one
// application so that a missing or unparseable secret is described in exactly
// the words the controller uses at startup.
func mountedCredential(spec application.Spec) (regauth.Resolver, error) {
	resolvers, err := regauth.Load([]application.Spec{spec}, regauth.DefaultSecretsDir, os.ReadFile)
	if err != nil {
		return nil, err
	}
	return resolvers[spec.Name], nil
}

// diff reports what has to happen for the running set to become the desired one.
//
// Running is what the reconciler holds, not the previously-loaded file: an
// application that failed to start last tick is still absent from it and is
// therefore retried, where a file-to-file diff would have recorded it as added
// once and never mentioned it again.
func diff(running []application.View, desired []application.Spec) (add, replace []application.Spec, remove []string) {
	current := make(map[string]application.Spec, len(running))
	for _, view := range running {
		current[view.Spec.Name] = view.Spec
	}

	wanted := make(map[string]struct{}, len(desired))
	for _, spec := range desired {
		wanted[spec.Name] = struct{}{}

		was, ok := current[spec.Name]
		switch {
		case !ok:
			add = append(add, spec)
		// Whole-spec equality rather than a field-by-field comparison: the spec
		// is plain data, both sides came out of the same parser, and a hand
		// written comparison would stop noticing the first field somebody adds.
		case !reflect.DeepEqual(was, spec):
			replace = append(replace, spec)
		}
	}

	// In the reconciler's order, so removals are reported the way the set was
	// declared rather than in map order.
	for _, view := range running {
		if _, ok := wanted[view.Spec.Name]; !ok {
			remove = append(remove, view.Spec.Name)
		}
	}
	return add, replace, remove
}

// Status reports where the set came from and how the last attempt went.
func (l *Loop) Status() application.ControllerStatus {
	revision, at := l.src.LastLoad()

	l.mu.RLock()
	defer l.mu.RUnlock()
	return application.ControllerStatus{
		AppSet: application.AppSetStatus{
			Mode:     l.mode,
			Source:   l.source,
			Revision: revision,
			LoadedAt: at,
			Error:    l.lastErr,
			Stale:    l.stale,
			// Cloned: the caller is a handler about to serialise it, and the
			// list keeps changing underneath.
			Orphaned:    slices.Clone(l.orphaned),
			Pruned:      slices.Clone(l.pruned),
			PruneHeldBy: slices.Clone(l.pruneHeldBy),
		},
		// What is being reconciled, which is not always what the file declares:
		// an application the set added but that could not be started is in the
		// file and not in this count.
		Applications: len(l.rec.Views()),
	}
}

func (l *Loop) record(err error, stale bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stale = stale
	// A cancelled context is the controller stopping, not a pass that failed.
	// Left as it was rather than cleared: what the status endpoint should report
	// on the way down is the last real outcome, not "context canceled".
	if errors.Is(err, context.Canceled) {
		return
	}
	if err == nil {
		l.lastErr = ""
		return
	}
	l.lastErr = err.Error()
}

// orphan records an application that left the set. Its stack is still deployed
// and nobody reconciles it any more (D-e of #47): reported, not removed.
func (l *Loop) orphan(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !slices.Contains(l.orphaned, name) {
		l.orphaned = append(l.orphaned, name)
	}
}

// adopt clears an orphan that has come back. The stack it left behind is the one
// the returning application now reconciles, so it is nobody's orphan.
//
// It clears the pruned record too: an application that was deleted and has
// since been re-declared is running again, reinstalled from git, and reporting
// it as pruned would describe the opposite of what is on the swarm.
func (l *Loop) adopt(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.orphaned = without(l.orphaned, name)
	l.pruned = without(l.pruned, name)
}

// recordPruned moves applications out of the orphan list and into the pruned
// one. An orphan is a stack nobody reconciles; once its resources are gone
// there is no stack, so reporting it under both headings would name a hazard
// that no longer exists.
func (l *Loop) recordPruned(names []string) {
	if len(names) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, name := range names {
		l.orphaned = without(l.orphaned, name)
		// Deduplicated like orphan, for the same reason: a sweep that partly
		// failed re-reports the same application next tick.
		if !slices.Contains(l.pruned, name) {
			l.pruned = append(l.pruned, name)
		}
	}
}

// recordPruneHeld records which applications the sweep is waiting on, or clears
// it once one has run.
func (l *Loop) recordPruneHeld(names []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneHeldBy = names
}

func without(names []string, name string) []string {
	return slices.DeleteFunc(names, func(n string) bool { return n == name })
}
