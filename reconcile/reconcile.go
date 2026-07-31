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
//
// Every piece of work done for an application goes through that application's
// entry, and the only way to start any is to take its lease. That is what makes
// the set safely mutable: the entry knows what is running for it, can cancel all
// of it at once, and can be waited on until it has stopped — whether the work
// was started by the loop, by the API, or by anything added later. Keeping those
// three facts in three separate places is what issue #106 was.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/mount"
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

// maxPruneAttempts bounds how many consecutive reconciles will try to delete
// the same network, config or secret before this controller stops trying.
//
// Swarm refuses to remove anything still in use, and while a release's tasks
// drain that refusal is the ordinary answer rather than a fault — which is why
// the sweep retries at all. But some refusals never clear: a chart that drops a
// network another stack is still attached to gets swarmkit's FailedPrecondition
// (manager/controlapi/network.go:205), rendered as HTTP 400
// (api/server/httpstatus/status.go:87), on this reconcile and on every one
// after it. The two are indistinguishable from here — same code, same message
// shape — so the only thing that separates "draining" from "never" is how long
// it has been going on.
//
// Retrying for ever is not free, which is what the sweep's own docstring used
// to claim. Each pass costs a full observe including a walk of the release's
// history, an apply that deploys nothing because the gate was opened by a
// deletion that will not happen, a warning, a PruneFailed event, and — because
// an orphan marks the release as drifted — an application that reports out of
// sync until somebody intervenes, for ever. The escape the design assumed,
// history trimming past the revision that proves the claim, never arrives:
// pruneHistory only runs after a deploy that records a revision, and nothing
// here records one.
//
// Three, because two is within the range a slow drain genuinely takes and the
// cost of a fourth attempt is another whole interval of the above. Giving up is
// not forgetting: the resource is warned about on every reconcile that still
// finds it, which is how an operator finds a condition that needs a human — the
// same choice appset.Loop makes for a held prune. A variable so a test does not
// need to run three reconciles to observe the fourth.
var maxPruneAttempts = 3

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
	// retiring holds the entries whose work outlived Remove's bounded wait, so
	// that Draining can report an application nothing is reconciling but
	// something is still writing. Guarded by mu; drained by Draining.
	retiring []*appEntry
	// root is the context Run supervises the set under. Loops do not derive from
	// it — each runs on its own entry's context, and startLoopLocked links the
	// two — so cancelling Run retires every entry, which stops a sync the API
	// started as well as the loop. It is nil until Run is called.
	//
	// wg tracks the live loop goroutines. It is not the whole drain, because a
	// sync started through the API belongs to no goroutine it counts; the
	// entries' leases are the other half. See drain.
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
		r.noteInterval(spec)
		r.apps[spec.Name] = newEntry(spec)
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

	// Taking mu is the happens-before edge between startLoopLocked's wg.Add and
	// the Wait inside drain. Without it there is none: an Add that had passed
	// the guard below but not yet reached wg.Add could run concurrently with a
	// Wait that the last loop goroutine had just released, which is the runtime
	// panic "WaitGroup misuse: Add called concurrently with Wait". Holding it
	// once is enough — an Add already past the guard completes its wg.Add before
	// releasing mu, and one that is not past it finds the run context cancelled
	// and starts nothing.
	r.mu.Lock()
	entries := make([]*appEntry, 0, len(r.order))
	for _, name := range r.order {
		entries = append(entries, r.apps[name])
	}
	r.mu.Unlock()

	r.drain(entries)
	return ctx.Err()
}

// drain waits for every loop goroutine and every in-flight sync to stop, bounded
// by drainTimeout.
//
// The loop goroutines are what wg counts. The leases are what covers everything
// else, and the reason this is not just wg.Wait: a sync started through the API
// runs detached from any request and is registered with no wait group, so before
// the lease existed a shutdown could not see it at all.
//
// Nothing here cancels: the run context is already done, which retires every
// entry through the link startLoopLocked made, so this is only the waiting half.
func (r *Reconciler) drain(entries []*appEntry) {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.wg.Wait()
		for _, e := range entries {
			e.held <- struct{}{}
		}
	}()

	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()

	select {
	case <-stopped:
	case <-timer.C:
		// The goroutine above is left parked on whichever lease it is waiting
		// for. That is deliberate: this is the shutdown path, the process is
		// about to exit, and the alternative — another channel to unwind it —
		// would only add a way for shutdown itself to go wrong.
		var busy []string
		for _, e := range entries {
			if e.busy() {
				busy = append(busy, e.name)
			}
		}
		r.log.Warn("gave up waiting for in-flight syncs to stop",
			"after", drainTimeout, "applications", busy)
	}
}

// startLoopLocked starts an application's reconcile loop. The caller holds mu.
// It is a no-op before Run has set the run context — an application added to a
// not-yet-running reconciler is simply recorded and started when Run is — and
// once that context is cancelled, so nothing is started into a shutdown that is
// already draining.
//
// The loop runs on the entry's own context rather than on a child of the run
// context, and the two are linked instead. That is what gives the application
// one cancel rather than two: Remove and the controller stopping both go through
// e.cancel, and so both stop a sync the API started as well as the loop.
func (r *Reconciler) startLoopLocked(e *appEntry) {
	if r.root == nil || r.root.Err() != nil || e.done != nil {
		return
	}
	unlink := context.AfterFunc(r.root, e.cancel)
	e.done = make(chan struct{})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(e.done)
		// Safe to unregister only because the loop returns when its context is
		// done, so by here e.cancel has either run or is no longer wanted.
		defer unlink()
		r.loop(e)
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
	r.noteInterval(spec)
	e := newEntry(spec)
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
	r.noteInterval(spec)
	e.spec = spec
	// A new spec is a new set of inputs — a different chart path, different
	// values files — so whatever the old one could not render reproducibly is no
	// longer evidence about this one. Held state that survived an edit meant to
	// fix it would need a manual sync to shift, which is the wrong way round.
	e.unstable, e.unstableReleases = "", nil
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
// cancels everything being done for the application — the loop and any sync in
// flight, whoever started it — and waits for all of it to stop. The deployed
// stack is left running and becomes unmanaged (D-e); pruning it is separate
// (issue #54).
//
// "Everything" is the change. This used to cancel and wait for the loop
// goroutine alone, while a sync started through the API ran detached from any
// context and was tracked by nothing — so the guarantee its comment made was
// false for precisely the caller relying on it, appset.Loop. With prune enabled
// the interleaving destroyed data: the sweep found the application gone,
// uninstalled its releases and volumes, and the sync still running then
// recreated the whole stack against volumes that were being deleted, writing
// fresh revision records for something reconciled by nobody.
//
// The wait is bounded, because cancellation does not reach every daemon call and
// removals run first in an app-set pass — an unbounded wait would hold up every
// other membership change behind one departing application. So this no longer
// promises quiescence: it promises removal. Whether the application's work has
// actually stopped is the separate question Draining answers, and a caller that
// deletes resources must ask it.
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
	done := e.done
	r.mu.Unlock()

	// Outside the lock: the work being stopped takes mu to read its spec and to
	// record what it found, so waiting for it while holding mu would deadlock.
	e.cancel()
	if done != nil {
		<-done
	}
	if err := e.drain(removeDrain); err != nil {
		// Warn, not an error return: the removal succeeded and retrying it would
		// find nothing. What is left is a fact about the departed application
		// that the prune sweep has to know, so it is recorded where Draining can
		// report it rather than thrown at a caller that cannot act on it.
		r.log.Warn("an application left the set while it was still syncing; its resources will not be pruned until that stops",
			"application", name, "error", err)
		r.mu.Lock()
		r.retiring = append(r.retiring, e)
		r.mu.Unlock()
	}
	return nil
}

// Draining names the applications that have left the set but whose work has not
// stopped, and forgets the ones that have since finished.
//
// It is the gap Remove's bounded wait leaves, made visible. A departed
// application whose sync is still running is still deploying services and still
// writing revision records, so deleting its resources is the same interleaving
// Remove exists to prevent — one pass later. The app-set sweep consults this for
// the same reason it waits for every application to have planned once: what it
// is about to delete has to be something nobody is still writing.
//
// Sweeping on read rather than on a timer, because the only thing that needs the
// answer is the caller asking for it.
func (r *Reconciler) Draining() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var names []string
	kept := r.retiring[:0]
	for _, e := range r.retiring {
		if !e.busy() {
			continue
		}
		kept = append(kept, e)
		names = append(names, e.name)
	}
	clear(r.retiring[len(kept):])
	r.retiring = kept
	return names
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

// allowedRefsBackend is the optional interface a backend implements to take one
// application's allowlist of what its charts may reach outside their own
// releases. *backend.Backend satisfies it.
type allowedRefsBackend interface {
	WithAllowedReferences(application.Allow) charts.Backend
}

// withAllowedReferences scopes a backend to one application's permissions.
//
// Applied unconditionally, unlike its two neighbours above, and the difference is
// the point: an empty allowlist is not "no policy", it is the policy that permits
// nothing, so there is no empty case to short-circuit past. Skipping it would
// leave whichever application deployed last through this per-swarm backend having
// set the permissions for the next one.
//
// It is the only per-application fact the applier gets. Everything else it
// refuses — the controller's own secrets, configs, volume, network, and the
// engine's release records — is the same answer for every application and is
// derived where it is enforced, which is why #102 and #103 needed no wiring at
// all. This one cannot be: which application a deploy belongs to is knowledge
// that exists here and nowhere below.
//
// A backend that does not support the upgrade — a Phase 3 remote one — enforces
// its own, which is also why this is not a check the reconciler makes for itself:
// the manifest is refused where the specs are, so nothing can reach the swarm
// past a layer that did not look.
func withAllowedReferences(b charts.Backend, allow application.Allow) charts.Backend {
	if ab, ok := b.(allowedRefsBackend); ok {
		return ab.WithAllowedReferences(allow)
	}
	return b
}

// dispatch sends one event, stamped with the application it is about and the
// destination it concerns.
//
// Every event this package raises goes through here rather than through
// notify.Dispatch directly, and that is the whole of the fix for #131. Both
// fields it fills are properties of the *application* rather than of the thing
// that happened, so every literal had to remember them — and Event.Swarm was
// declared, documented and then left empty at all thirteen of them, because an
// Apache-2.0 build resolves one swarm and nothing it can test would ever notice.
// The companion that routes production alerts to one channel and staging to
// another is what notices, by which time the literals are somewhere else.
//
// Nothing else is filled in. At is the moment the event describes, which is not
// always now — a sync's outcome carries the time the sync finished — and a
// helper that guessed it would be wrong exactly where it mattered.
func dispatch(ctx context.Context, spec application.Spec, e notify.Event) {
	e.Application = spec.Name
	e.Swarm = spec.Destination.Swarm
	notify.Dispatch(ctx, e)
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
func (r *Reconciler) withOutOfBandNotifier(ctx context.Context, b charts.Backend, spec application.Spec) charts.Backend {
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
		dispatch(ctx, spec, notify.Event{
			Type: notify.LiveDriftDetected,
			At:   r.now(),
			Message: "service " + service + " was changed by something else while this sync was writing it; " +
				"the change has been overwritten with what the repository declares",
		})
	})
}

// stackServicesReader is the optional interface a backend implements to report
// a failed read rather than an empty stack. *backend.Backend satisfies it.
//
// charts.Backend.StackServices has no error return, and for its CE caller that
// is right: it polls, so a daemon that could not be asked is "not converged yet"
// and the next poll asks again. awaitConverged below is that caller. record is
// not — it turns the same nil into "deployed, but no services are present on the
// swarm", the loudest thing the health rollup can say, so one slow daemon
// flipped every release of an application from healthy to missing (#107).
//
// The honest read is an upgrade beside it, in the shape liveDriftBackend and
// resourceLister already use. A backend that does not implement it reads exactly
// as before.
type stackServicesReader interface {
	ReadStackServices(ctx context.Context, name string) ([]charts.ServiceState, error)
}

// stacksReader is the optional interface a backend implements to answer for
// several releases from one look at the swarm. *backend.Backend satisfies it.
//
// The read behind StackServices is a whole-swarm snapshot — NodeList,
// ServiceList, TaskList and Info — which is then filtered by stack name. Asking
// it once per release therefore fetched the entire swarm once per release and
// threw away all but one stack's worth each time, so an application declaring
// ten releases cost ten identical round trips on every single reconcile, on the
// manager node the controller itself runs on.
//
// One request, one answer, filtered per release afterwards, which is what the
// call already did internally.
type stacksReader interface {
	ReadStacks(ctx context.Context, releases []string) (map[string][]charts.ServiceState, error)
}

// stackServices reads one release's live services, keeping the read failure
// where the backend can tell one from an empty stack.
func stackServices(ctx context.Context, b charts.Backend, release string) ([]charts.ServiceState, error) {
	if sr, ok := b.(stackServicesReader); ok {
		return sr.ReadStackServices(ctx, release)
	}
	return b.StackServices(ctx, release), nil
}

// readStacks reads several releases' live services at once.
//
// The contract is all-or-nothing, and deliberately so: a nil error means every
// requested release is in the map, and a non-nil one means the swarm could not
// be read and none of them are. That is what the underlying call actually does —
// one snapshot either arrives or does not — and it is the reading #107 settled
// on, where a daemon that could not be asked must never be published as a swarm
// with no services on it.
//
// The fallback for a backend without the batched seam keeps that contract rather
// than a weaker one: it stops at the first failure, because every one of its
// per-release calls is the same whole-swarm read and a failure of one is a
// failure of all.
func readStacks(ctx context.Context, b charts.Backend, releases []string) (map[string][]charts.ServiceState, error) {
	if len(releases) == 0 {
		return nil, nil
	}
	if sr, ok := b.(stacksReader); ok {
		return sr.ReadStacks(ctx, releases)
	}

	out := make(map[string][]charts.ServiceState, len(releases))
	for _, release := range releases {
		states, err := stackServices(ctx, b, release)
		if err != nil {
			return nil, err
		}
		out[release] = states
	}
	return out, nil
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

// networkNamer is the optional interface a backend implements to name the
// networks a service is attached to. *backend.Backend satisfies it.
//
// A spec names them by id, because the daemon rewrites each target to one as it
// writes the service, so without this the attachments cannot be compared at all.
// Separate from liveDriftBackend for the reason resourceLister is: a backend that
// can read services but not list networks should lose that one field and keep
// every other comparison.
//
// Not stack-scoped, unlike resourceLister's three. A service may be attached to
// an external network or a predefined one, neither of which carries the stack's
// namespace label, and an attachment that could not be named is exactly the one
// worth reporting.
type networkNamer interface {
	LiveNetworkNames(ctx context.Context) (map[string]string, error)
}

// declaredLister is the optional interface a backend implements to answer what a
// manifest declares without needing any of it to exist. *backend.Backend
// satisfies it.
//
// DesiredServices cannot answer that question about a *stored* revision.
// Converting a service resolves every config and secret it mounts to the id Swarm
// addresses it by, so it asks today's swarm about yesterday's references — and a
// revision that mounted a config a previous sweep has since deleted no longer
// converts at all. The sweep then loses that revision's claims and leaves
// resources behind (#87). Nothing about proving ownership wants an id; the scoped
// name is the whole of it.
//
// Separate from liveDriftBackend rather than added to it, for the reason
// resourceLister gives: a backend that cannot answer this should lose the sweep's
// history walk, not its live drift too. And separate from DesiredServices rather
// than replacing it, because live drift compares whole ServiceSpecs — its field
// list is an allowlist today, but widening it (#76) must not be the thing that
// starts diffing against a placeholder id.
type declaredLister interface {
	DeclaredResources(ctx context.Context, manifest, stack string) (*compose.Stack, error)
}

// declaredReader returns the conversion to ask what a manifest declares.
//
// A backend that does not implement declaredLister gets the resolving one, which
// is what both callers used before #87: the same answer, unavailable in the same
// narrow cases, rather than no sweep at all.
func declaredReader(ldb liveDriftBackend) func(context.Context, string, string) (*compose.Stack, error) {
	if dl, ok := ldb.(declaredLister); ok {
		return dl.DeclaredResources
	}
	return ldb.DesiredServices
}

// resourceLister is the optional interface a backend implements to read the
// other three kinds a manifest declares, by scoped name. *backend.Backend
// satisfies it.
//
// Separate from liveDriftBackend because live drift compares services and
// nothing else: a backend that can answer one and not the other should lose
// only the half it cannot answer. A backend implementing neither prunes
// nothing, which is the degradation live drift already has.
//
// Name to id is all the sweep needs — it matches on the scoped name, the only
// key a manifest and a live resource share, and deletes by id.
type resourceLister interface {
	LiveNetworks(ctx context.Context, stack string) (map[string]string, error)
	LiveConfigs(ctx context.Context, stack string) (map[string]string, error)
	LiveSecrets(ctx context.Context, stack string) (map[string]string, error)
}

// resourceRemover is the optional interface a backend implements to delete a
// single resource of each kind. *backend.Backend satisfies it.
//
// Separate from the readers rather than folded into them, so that a backend
// which can read the swarm but not write to it still reports what a sweep would
// remove instead of losing the report along with the removal.
type resourceRemover interface {
	RemoveService(ctx context.Context, id string) error
	RemoveNetwork(ctx context.Context, id string) error
	RemoveConfig(ctx context.Context, id string) error
	RemoveSecret(ctx context.Context, id string) error
}

// releaseView is what both the live comparison and the service sweep need from
// one release: the specs the repository renders to, and the services actually
// running under its namespace.
type releaseView struct {
	// desired is the resolving conversion of the manifest, for the live-drift
	// comparison only: that diffs whole ServiceSpecs, so it must never be handed
	// a reference resolved to a placeholder id. Read only for a release live
	// drift will actually compare — a settled one — because it is also the
	// reading that cannot be made before a deploy has created what the manifest
	// newly declares.
	desired *compose.Stack
	// declared is the same manifest converted without resolving what its services
	// mount, which is the whole of what the sweep needs from it: the scoped names
	// a stored revision proves are this application's. It works on an upgraded
	// release too, whose new manifest may mount a config this deploy has not
	// created yet (#84, #87).
	declared *compose.Stack
	live     map[string]swarm.Service
	// networks, configs and secrets are the other three kinds carrying the
	// release's namespace label, by scoped name to id. Read only for a sweeping
	// application whose backend can list them; nil otherwise, which makes them
	// candidates for nothing.
	networks map[string]string
	configs  map[string]string
	secrets  map[string]string
	// err is whatever stopped the release's services being read, or its manifest
	// being read for names. Both features answer it the same way — report it and
	// change nothing — because neither rewrites nor deletes anything on the
	// strength of a read it could not make.
	err error
	// desiredErr is what stopped the resolving conversion, and is kept apart from
	// err for the reason resErr is: it costs live drift its comparison and the
	// sweep nothing. A config a settled release mounts having been deleted by hand
	// is exactly that — the manifest no longer resolves, while what it declares is
	// as legible as ever.
	desiredErr error
	// resErr is the same for the other three kinds, and is kept apart from err
	// on purpose: a backend that cannot list them should lose only that half.
	// Folding the two would make a failure to list configs stop the service
	// sweep as well, which is a strictly worse answer to the same problem.
	resErr error
}

// departedService is one service a sweep may delete: the scoped name to report
// it by, the id to delete it by, and the named volumes it was mounting.
//
// The id is read before the apply and stays valid across it. An apply only ever
// writes the services the manifest declares, and this is by definition one it
// does not.
type departedService struct {
	name    string
	id      string
	volumes []string
}

// departedResource is one network, config or secret a sweep may delete.
//
// No volumes field: a volume is only ever reachable through the service that
// mounts it, and #75 settled that those are reported and never deleted.
type departedResource struct {
	kind application.ResourceKind
	name string
	id   string
}

// doomedSet is everything one release has that a sweep may delete, plus what it
// has stopped trying to.
//
// The first two halves travel together because they are proved together, and are
// kept apart because they are written at different moments: pruneFirst orders
// the services, and the other three kinds always go after the apply.
//
// retained is proved in exactly the same way and is not acted on: see
// maxPruneAttempts. It is carried here rather than dropped so that the pass can
// report it and carry its failure count forward — a set with nothing but
// retained in it is a release with nothing left to do.
type doomedSet struct {
	services  []departedService
	resources []departedResource
	retained  []departedResource
}

// empty reports a set that will not delete anything, which is not the same as a
// set with nothing in it: everything in it may have been retained.
func (d doomedSet) empty() bool {
	return len(d.services) == 0 && len(d.resources) == 0
}

// actionable counts the releases a sweep would actually delete something from.
//
// It is what the apply gate asks, rather than the size of the map, because a
// release whose every candidate has been retained has nothing for an apply to
// do — and opening the gate for it would deploy a plan of unchanged releases,
// re-plan to confirm it, and find the same nothing on the next interval and for
// ever.
func actionable(doomed map[string]doomedSet) int {
	var n int
	for _, set := range doomed {
		if !set.empty() {
			n++
		}
	}
	return n
}

// pruneKey identifies one resource across reconciles, for the failure count.
// Release, kind and name together, because the four kinds share one namespace
// of scoped names and a name alone is not evidence about which one it is.
func pruneKey(release string, res departedResource) string {
	return release + "/" + string(res.kind) + "/" + res.name
}

// failedPrunes is a copy of an application's consecutive-failure counts, so the
// caller can work on it without holding the lock. An application that has been
// removed from the set has none, which is what a fresh map means.
func (r *Reconciler) failedPrunes(e *appEntry) map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.currentLocked(e) {
		return maps.Clone(e.pruneFailures)
	}
	return nil
}

// recordPruneFailures replaces an application's counts. Replaces rather than
// merges: the caller has just re-derived the whole candidate set, so a key it
// left out is a resource that is no longer there — deleted, re-declared by a
// commit, or removed by hand — and its history is over.
func (r *Reconciler) recordPruneFailures(e *appEntry, counts map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentLocked(e) {
		e.pruneFailures = counts
	}
}

// resourceNames is one release's scoped names, by kind.
//
// Kinds are kept apart rather than merged into one list because the four share a
// single namespace of scoped names — a config and a service can both be
// "rel_app" — so a name is only ever evidence about its own kind.
type resourceNames struct {
	services []string
	networks []string
	configs  []string
	secrets  []string
}

func (n resourceNames) empty() bool {
	return len(n.services) == 0 && len(n.networks) == 0 && len(n.configs) == 0 && len(n.secrets) == 0
}

// observe reads what is running for the releases this application's settings ask
// about, and returns both the live-drift report and the services a sweep would
// delete.
//
// The two come back together because they read the same thing, and enabling both
// should not cost two reads of it. What they ask for differs in one way: live
// drift looks only at a settled release, while the sweep looks at an upgraded one
// too, because the pass that drops a service from a template is an upgrade and
// that is the case worth putting right at once rather than an interval later.
//
// That difference decides which conversion each gets, and it is not a detail.
// This runs *before* the apply, so an upgraded release's manifest is the new one —
// and converting a service resolves every config and secret it mounts against the
// daemon, which for a config this upgrade is about to create resolves against
// nothing. Asking the resolving conversion here would fail on exactly the upgrade
// that adds a mounted config, costing that release both its drift report and its
// sweep for the interval (#84, #87). So the sweep, which wants names, gets the
// conversion that needs nothing to exist; live drift, which diffs whole
// ServiceSpecs, gets the resolving one and only for the settled releases it
// actually compares — where everything it mounts is by definition already there.
//
// A release the plan would install is read for neither. Nothing of ours is
// deployed under that name, so there is nothing to compare against and nothing
// this controller could prove is its own to delete.
func (r *Reconciler) observe(ctx context.Context, e *appEntry, spec application.Spec, b charts.Backend, engine Engine, plan *charts.Plan) (map[string]*application.ReleaseDrift, map[string]doomedSet) {
	live := spec.DriftDetection == application.DriftLive
	sweep := spec.SyncPolicy.PruneResources
	if !live && !sweep {
		return nil, nil
	}
	ldb, ok := b.(liveDriftBackend)
	if !ok {
		return nil, nil
	}
	// A backend that cannot list the other three kinds still sweeps services.
	// Losing the half it cannot answer is better than losing both.
	rl, _ := b.(resourceLister)
	readDeclared := declaredReader(ldb)
	// Per swarm rather than per release, so one read answers every release below.
	nets := r.networkNames(ctx, spec, ldb, live)

	views := make(map[string]releaseView, len(plan.Releases))
	for _, rp := range plan.Releases {
		if rp.Action == charts.ActionInstall {
			continue
		}
		if rp.Action != charts.ActionUnchanged && !sweep {
			continue
		}
		var v releaseView
		v.live, v.err = ldb.LiveServices(ctx, rp.Name)
		if v.err == nil && live && rp.Action == charts.ActionUnchanged {
			v.desired, v.desiredErr = ldb.DesiredServices(ctx, rp.Manifest, rp.Name)
		}
		if v.err == nil && sweep {
			// The resolving conversion, when there is one, has already answered
			// this: the two produce the same names, and the ids it also carries
			// are simply unread here. So a live-drifting sweeper converts once.
			if v.desired != nil {
				v.declared = v.desired
			} else {
				v.declared, v.err = readDeclared(ctx, rp.Manifest, rp.Name)
			}
		}
		if v.err == nil && sweep && rl != nil {
			v.networks, v.configs, v.secrets, v.resErr = liveResources(ctx, rl, rp.Name)
		}
		if v.err != nil {
			r.log.Warn("could not read a release's running services",
				"application", spec.Name, "release", rp.Name, "error", v.err)
		}
		if v.desiredErr != nil {
			// Not the sweep's problem, and said separately from err so that it
			// does not read as one: what a release declares is still legible.
			r.log.Warn("could not convert a release's manifest to compare its running services against",
				"application", spec.Name, "release", rp.Name, "error", v.desiredErr)
		}
		if v.resErr != nil {
			r.log.Warn("could not read a release's networks, configs and secrets, so none of them can be pruned",
				"application", spec.Name, "release", rp.Name, "error", v.resErr)
		}
		views[rp.Name] = v
	}

	drifts := r.liveDrift(spec, plan, views, nets)
	if !sweep {
		return drifts, nil
	}
	doomed := r.departed(ctx, e, spec, ldb, engine, plan, views)
	return withOrphans(drifts, doomed), doomed
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
func (r *Reconciler) liveDrift(spec application.Spec, plan *charts.Plan, views map[string]releaseView, nets drift.NetworkNames) map[string]*application.ReleaseDrift {
	if spec.DriftDetection != application.DriftLive {
		return nil
	}

	out := make(map[string]*application.ReleaseDrift, len(plan.Releases))
	for _, rp := range plan.Releases {
		if rp.Action != charts.ActionUnchanged {
			continue
		}
		v, ok := views[rp.Name]
		if !ok {
			continue
		}
		// Either failure leaves nothing to compare: one is the running services,
		// the other the manifest they would be compared against.
		if err := errors.Join(v.err, v.desiredErr); err != nil {
			out[rp.Name] = &application.ReleaseDrift{
				State:   application.DriftStateUnknown,
				Message: err.Error(),
			}
			continue
		}
		out[rp.Name] = drift.Live(v.desired, v.live, nets)
	}
	return out
}

// networkNames reads the swarm's networks by id, for the one compared field that
// a spec stores as one.
//
// Read once per reconcile rather than per release, because it is a property of
// the swarm and not of a stack, and only in live mode because nothing else looks
// at it. A backend that does not implement the seam, or a listing that fails,
// yields nil and the attachment comparison is skipped: this is a read for a
// reporting axis, and losing one field is not a reason to fail a reconcile or to
// call an otherwise readable release unknown.
func (r *Reconciler) networkNames(ctx context.Context, spec application.Spec, ldb liveDriftBackend, live bool) drift.NetworkNames {
	if !live {
		return nil
	}
	nn, ok := ldb.(networkNamer)
	if !ok {
		return nil
	}
	names, err := nn.LiveNetworkNames(ctx)
	if err != nil {
		r.log.Warn("could not name the swarm's networks, so no release's attached networks can be compared",
			"application", spec.Name, "error", err)
		return nil
	}
	return names
}

// departed names the services running under each release that its manifest no
// longer declares and that a revision this controller stamped for this
// application did.
//
// That third clause is what makes deleting safe, and note what it is not: the
// stack's namespace label. The label is the only thing tying a service to a
// release, and anything can carry it — a sidecar somebody attached by hand reads
// exactly like a service we installed. A stored revision does not: this
// controller wrote it, it names the owner, and it never changes afterwards. The
// same rule as #62 one scope down — the label says where a service lives, the
// record says whose it is.
//
// A release with no candidates costs nothing beyond the read that has already
// happened. One with candidates reads its history once and walks it newest
// first, converting a revision's manifest only while something is still
// unaccounted for, so the ordinary case reads one revision rather than a whole
// history.
func (r *Reconciler) departed(ctx context.Context, e *appEntry, spec application.Spec, ldb liveDriftBackend, engine Engine, plan *charts.Plan, views map[string]releaseView) map[string]doomedSet {
	var out map[string]doomedSet
	// The counts as they stand, and the counts this pass leaves behind. Only a
	// resource that is still a candidate is carried over, which is what keeps
	// the map from growing a key per resource this application has ever
	// dropped — and what lets a resource somebody removed by hand start again
	// from nothing if its name ever comes back.
	was, still := r.failedPrunes(e), map[string]int{}
	defer func() { r.recordPruneFailures(e, still) }()

	for _, rp := range plan.Releases {
		v, ok := views[rp.Name]
		if !ok || v.err != nil {
			continue
		}
		declared := declaredNames(v.declared)
		candidates := resourceNames{services: prune.Undeclared(runningNames(v.live), declared.services)}
		// A read that failed proves nothing either way, so the kinds it covered
		// are candidates for nothing while the services carry on.
		if v.resErr == nil {
			candidates.networks = prune.Undeclared(names(v.networks), declared.networks)
			candidates.configs = prune.Undeclared(names(v.configs), declared.configs)
			candidates.secrets = prune.Undeclared(names(v.secrets), declared.secrets)
		}
		if candidates.empty() {
			continue
		}
		claimed := r.claimed(ctx, spec, ldb, engine, rp.Name, candidates)
		if claimed.empty() {
			continue
		}

		var set doomedSet
		for _, name := range claimed.services {
			set.services = append(set.services, departedService{
				name:    name,
				id:      v.live[name].ID,
				volumes: mountedVolumes(v.live[name]),
			})
		}
		// Configs, then secrets, then networks: the order RemoveStack removes
		// them in, which is the order they stop being referenced in.
		for _, name := range claimed.configs {
			set.add(departedResource{application.ResourceConfig, name, v.configs[name]}, rp.Name, was, still)
		}
		for _, name := range claimed.secrets {
			set.add(departedResource{application.ResourceSecret, name, v.secrets[name]}, rp.Name, was, still)
		}
		for _, name := range claimed.networks {
			set.add(departedResource{application.ResourceNetwork, name, v.networks[name]}, rp.Name, was, still)
		}
		if len(set.retained) > 0 {
			// Every pass it is still there, not once when it was given up on.
			// This is a condition an operator has to be able to find rather than
			// one that scrolled past hours ago, and the sweep has no other way
			// to say so — it is deliberately absent from the status, which
			// reports what a sync will do and no longer intends to do anything
			// about these.
			r.log.Warn("resources the release no longer declares could not be removed and will not be tried again; "+
				"remove them by hand once whatever still references them is gone",
				"application", spec.Name, "release", rp.Name,
				"resources", resourceLabels(set.retained), "attempts", maxPruneAttempts)
		}

		if out == nil {
			out = make(map[string]doomedSet, len(plan.Releases))
		}
		out[rp.Name] = set
	}
	return out
}

// add files one proved resource under whichever of the two lists its history
// puts it in, and carries that history into the next pass.
func (d *doomedSet) add(res departedResource, release string, was, still map[string]int) {
	key := pruneKey(release, res)
	n := was[key]
	if n > 0 {
		still[key] = n
	}
	if n >= maxPruneAttempts {
		d.retained = append(d.retained, res)
		return
	}
	d.resources = append(d.resources, res)
}

// resourceLabels renders resources for a log line, kind included: the name
// alone does not say which of the three to go and look at.
func resourceLabels(res []departedResource) []string {
	out := make([]string, 0, len(res))
	for _, r := range res {
		out = append(out, string(r.kind)+" "+r.name)
	}
	return out
}

// claimed walks a release's stored revisions, newest first, and returns the
// candidates one of this application's own revisions declared.
//
// A history that cannot be read prunes nothing. So does a revision that cannot
// be converted — it is no evidence either way, and an older one may still claim
// what it could not.
//
// Which conversion answers that matters, and is why declaredLister exists: this
// asks what a revision from the past declared, so it must not depend on any of it
// still existing. A backend that cannot answer that falls back to the resolving
// conversion, which is what this did before #87 — the same claims, lost in the
// same narrow case, rather than no sweep at all.
func (r *Reconciler) claimed(ctx context.Context, spec application.Spec, ldb liveDriftBackend, engine Engine, release string, candidates resourceNames) resourceNames {
	readDeclared := declaredReader(ldb)

	revisions, err := engine.History(ctx, release)
	if err != nil {
		r.log.Warn("could not read a release's history, so none of its resources can be pruned",
			"application", spec.Name, "release", release, "error", err)
		return resourceNames{}
	}

	var claimed resourceNames
	rest := candidates
	for i := len(revisions) - 1; i >= 0 && !rest.empty(); i-- {
		rev := revisions[i]
		if app, ok := prune.Owner(rev, r.controller); !ok || app != spec.Name {
			continue
		}
		stack, err := readDeclared(ctx, rev.Manifest, release)
		if err != nil {
			r.log.Warn("could not read what a stored revision declared",
				"application", spec.Name, "release", release,
				"revision", rev.Revision, "error", err)
			continue
		}
		// One conversion answers all four kinds, so sweeping four costs what
		// sweeping one did.
		declared := declaredNames(stack)
		var found resourceNames
		found.services, rest.services = prune.Claim(rest.services, declared.services)
		found.networks, rest.networks = prune.Claim(rest.networks, declared.networks)
		found.configs, rest.configs = prune.Claim(rest.configs, declared.configs)
		found.secrets, rest.secrets = prune.Claim(rest.secrets, declared.secrets)
		claimed.services = append(claimed.services, found.services...)
		claimed.networks = append(claimed.networks, found.networks...)
		claimed.configs = append(claimed.configs, found.configs...)
		claimed.secrets = append(claimed.secrets, found.secrets...)
	}
	slices.Sort(claimed.services)
	slices.Sort(claimed.networks)
	slices.Sort(claimed.configs)
	slices.Sort(claimed.secrets)
	return claimed
}

// withOrphans records what the sweep found on the drift axis, so that a deletion
// is visible in the status and not only in the log.
//
// In live mode the services are already reported as unexpected and this only
// marks them, which is the difference between "something is running here that
// the manifest does not declare" and "this goes on the next sync". In manifest
// mode liveDrift does not run at all, so a report is built carrying the orphans
// and nothing else: no field comparison was performed and none is implied. That
// is the one place this feature adds a drift axis where manifest mode had none,
// and it is deliberate — deleting a service with no record of it anywhere but a
// log line would be worse.
//
// A release whose every candidate has been retained is skipped entirely, and
// with it the DriftStateDetected that record folds through to SyncOutOfSync.
// Both mean "the next sync puts this right", and for a resource this controller
// has stopped trying to delete that is false — saying it anyway is what leaves
// an application permanently out of sync over something nothing here will ever
// do. The record of it is the warning departed raises on every pass, which is
// the honest shape for a finding whose remedy is a person.
func withOrphans(drifts map[string]*application.ReleaseDrift, doomed map[string]doomedSet) map[string]*application.ReleaseDrift {
	if actionable(doomed) == 0 {
		return drifts
	}
	if drifts == nil {
		drifts = make(map[string]*application.ReleaseDrift, len(doomed))
	}

	for release, set := range doomed {
		if set.empty() {
			continue
		}
		rd := drifts[release]
		if rd == nil {
			rd = &application.ReleaseDrift{}
			drifts[release] = rd
		}
		for _, svc := range set.services {
			if !markOrphaned(rd, svc.name) {
				rd.Services = append(rd.Services, application.ServiceDrift{
					Name:     svc.name,
					Reason:   application.DriftUnexpected,
					Orphaned: true,
				})
			}
		}
		// Appended rather than marked: nothing else reports these kinds, so
		// there is never an existing entry to find. drift.Live compares
		// services and only services.
		for _, res := range set.resources {
			rd.Resources = append(rd.Resources, application.ResourceDrift{Kind: res.kind, Name: res.name})
		}
		rd.State = application.DriftStateDetected
	}
	return drifts
}

// markOrphaned marks an already-reported service and says whether it found one.
func markOrphaned(rd *application.ReleaseDrift, name string) bool {
	for i := range rd.Services {
		if rd.Services[i].Name == name {
			rd.Services[i].Orphaned = true
			return true
		}
	}
	return false
}

// runningNames is the scoped names of what is deployed.
func runningNames(live map[string]swarm.Service) []string {
	out := make([]string, 0, len(live))
	for name := range live {
		out = append(out, name)
	}
	return out
}

// names is the keys of a scoped-name-to-id map.
func names(live map[string]string) []string {
	out := make([]string, 0, len(live))
	for name := range live {
		out = append(out, name)
	}
	return out
}

// declaredNames is the scoped names a manifest declares, by kind.
//
// The scoped name is what the live resource carries and the only key the two
// sides can be matched on, which is also why drift.Live keys on it. Every name
// here is already scoped: docker/cli's convert prefixes the namespace, and a
// manifest that set an explicit `name:` gets that name on both sides.
//
// External declarations are absent from all four lists, because convert drops
// them: an external resource is not ours to create and so is not ours to delete.
// That it is missing from the declared side is not a hazard — an external
// resource carries no namespace label either, so it is never a candidate, and no
// revision's manifest can claim one.
func declaredNames(stack *compose.Stack) resourceNames {
	if stack == nil {
		return resourceNames{}
	}
	out := resourceNames{
		services: make([]string, 0, len(stack.Services)),
		networks: make([]string, 0, len(stack.Networks)),
		configs:  make([]string, 0, len(stack.Configs)),
		secrets:  make([]string, 0, len(stack.Secrets)),
	}
	for _, svc := range stack.Services {
		out.services = append(out.services, svc.Spec.Name)
	}
	for _, nw := range stack.Networks {
		out.networks = append(out.networks, nw.Name)
	}
	for _, cfg := range stack.Configs {
		out.configs = append(out.configs, cfg.Name)
	}
	for _, sec := range stack.Secrets {
		out.secrets = append(out.secrets, sec.Name)
	}
	return out
}

// liveResources reads the other three kinds carrying a release's namespace.
//
// All three or none: a partial read cannot be told apart from a release that
// genuinely has none of that kind, and the difference decides whether something
// gets deleted.
func liveResources(ctx context.Context, rl resourceLister, release string) (networks, configs, secrets map[string]string, err error) {
	if networks, err = rl.LiveNetworks(ctx, release); err != nil {
		return nil, nil, nil, err
	}
	if configs, err = rl.LiveConfigs(ctx, release); err != nil {
		return nil, nil, nil, err
	}
	if secrets, err = rl.LiveSecrets(ctx, release); err != nil {
		return nil, nil, nil, err
	}
	return networks, configs, secrets, nil
}

// mountedVolumes names the volumes a service mounts, for reporting when it is
// pruned. Bind mounts and tmpfs are not named resources and are left out.
func mountedVolumes(s swarm.Service) []string {
	cs := s.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return nil
	}
	var out []string
	for _, m := range cs.Mounts {
		if m.Type == mount.TypeVolume && m.Source != "" {
			out = append(out, m.Source)
		}
	}
	slices.Sort(out)
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
// So it is reported and not converged. Putting it right means deleting the
// service, which is a different verb on a different list: syncPolicy.pruneResources
// and the sweep in departed, which deletes only what this controller can prove it
// installed. With that off — the default — the application stays out of sync,
// which is the truth.
//
// A RolledBack service goes further and vetoes the whole release, sibling
// differences included. Swarm reverted that spec because it would not converge,
// so re-applying the manifest re-applies the rejected spec — and a deploy is
// release-granular, with no way to rewrite the service beside it without also
// rewriting this one. Correcting anyway would push, be rolled back, and find the
// same drift next interval, for ever — a drift-converged event and a
// live-drift-detected event apiece, every interval, while nothing converges
// (swarmcli-cd#90). The release is reported, the daemon's reason is carried on the
// service, and the remedy is a commit — which arrives as an upgrade rather than as
// drift, and clears the state with it.
func convergeable(d *application.ReleaseDrift) bool {
	if !detected(d) {
		return false
	}
	for _, svc := range d.Services {
		if svc.Reason == application.DriftRolledBack {
			return false
		}
	}
	for _, svc := range d.Services {
		if svc.Reason == application.DriftModified || svc.Reason == application.DriftMissing {
			return true
		}
	}
	return false
}

// correcting names the services a redeploy of the release is being made for:
// the ones the comparison found modified or missing, and nothing else.
//
// It is what a correction may be judged by. An unexpected service — running
// under the release's namespace label and declared by nothing — is not in the
// deploy at all, because applying deletes nothing and writes only what the
// manifest declares. Judging the write by one of those means judging it by
// something it never touched.
func correcting(d *application.ReleaseDrift) []string {
	if d == nil {
		return nil
	}
	var out []string
	for _, svc := range d.Services {
		if svc.Reason == application.DriftModified || svc.Reason == application.DriftMissing {
			out = append(out, svc.Name)
		}
	}
	return out
}

// asDeclared names the services of a release the live comparison found running
// exactly what the repository declares: everything it read and did not report.
//
// It is this half of the reading that health needs and never had. drift/live.go
// already decides that a rollback which restored what git wants is history
// rather than a finding — that is what makes it not drift — and the health axis
// took the opposite view of the same service, because CE's Convergence reads
// rollback_completed as wedged. That is right for its own caller, waitReady
// polling a service it has just written. It is wrong here, where the polling
// never stops and the status can be weeks old: no drift, so no redeploy, so no
// ServiceUpdate, so nothing ever clears it and the release reads Degraded for
// ever (#130). What health does with this is health's; all this says is what was
// compared and found equal.
//
// A service the comparison reported at all is left out, unexpected ones
// included: one it found running nothing the repository declares is not one it
// found running what the repository declares.
//
// Nil whenever nothing was compared. Manifest mode makes no live comparison, and
// a release whose live state could not be read reports Unknown; neither proves
// anything about any service. "Equal" also means equal in the fields drift
// compares, which is an allowlist — the same evidence the whole mode acts on,
// and no weaker here than it is there.
func asDeclared(states []charts.ServiceState, rd *application.ReleaseDrift) []string {
	if rd == nil || rd.State == application.DriftStateUnknown {
		return nil
	}
	out := make([]string, 0, len(states))
	for _, s := range states {
		if !slices.ContainsFunc(rd.Services, func(sd application.ServiceDrift) bool { return sd.Name == s.Name }) {
			out = append(out, s.Name)
		}
	}
	return out
}

// loop reconciles one application on its own schedule, reading its current spec
// each tick so a Replace is picked up without the loop being restarted.
//
// It runs on the entry's context, so it stops when the application is removed
// and when the controller does, through the same cancel.
func (r *Reconciler) loop(e *appEntry) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	failures := 0
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-timer.C:
		}

		if err := r.sync(e.ctx, e, false); err != nil {
			// A cancelled context is a shutdown, not a failure to back off
			// from.
			if e.ctx.Err() != nil {
				return
			}
			failures++
			r.log.Error("reconcile failed", "application", e.name, "failures", failures, "error", err)
		} else {
			failures = 0
		}

		timer.Reset(backoff(r.intervalFor(r.currentSpec(e)), failures))
	}
}

// intervalFor is how long this application waits between ticks: its own
// syncPolicy.interval, floored, or the controller-wide one.
//
// The floor is on the application's number and not on the controller's, because
// only one of the two is untrusted. syncPolicy.interval arrives in the app set,
// which is itself reconciled from git — so whoever can commit to that repository
// chooses it — while the controller-wide interval is set once by whoever runs
// the controller, and a deployment that wants to reconcile everything every
// second is entitled to say so about its own machine.
//
// Clamped rather than refused. Refusing at load time would stop the whole file
// from being applied, so one bad interval would freeze every other application's
// updates — a worse failure than the one being prevented. The `validate` command
// refuses it instead, which is where an operator finds out before committing,
// and noteInterval says so once at the point the spec is adopted so a clamped
// application is not silently slower than it asked to be.
func (r *Reconciler) intervalFor(spec application.Spec) time.Duration {
	if d := time.Duration(spec.SyncPolicy.Interval); d > 0 {
		return max(d, application.MinInterval)
	}
	return r.interval
}

// noteInterval says once that an application asked to reconcile faster than the
// floor allows.
//
// At the three points a spec enters the set rather than in intervalFor, which
// runs every tick: at the floor a warning there would be several thousand
// identical lines a day, and the thing an operator needs to know is that the
// number they wrote is not the number in force.
func (r *Reconciler) noteInterval(spec application.Spec) {
	if d := time.Duration(spec.SyncPolicy.Interval); d > 0 && d < application.MinInterval {
		r.log.Warn("syncPolicy.interval is below the floor and has been clamped",
			"application", spec.Name, "declared", d, "using", application.MinInterval)
	}
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

// cancelled reports that an error is the controller stopping rather than
// something going wrong.
//
// loop above already makes that judgement for the backoff, and everything that
// reports needs the same one. Two routine triggers made it a lie: every rolling
// restart cancels each application's context mid-flight — near-certain under
// syncPolicy.wait, where an apply blocks for as long as the rollout takes — and
// Remove cancels the application it is retiring. Both arrived as context.Canceled
// wrapped in whatever the call was doing at the time, were dispatched as
// sync-failed and recorded as an application-level error, and so paged somebody
// for a clean shutdown or for one application cleanly retired (#107).
//
// context.Canceled only. A deadline that expired is a timeout, which is a real
// failure and exactly what syncPolicy.timeout exists to report.
func cancelled(err error) bool { return errors.Is(err, context.Canceled) }

// entry resolves an application name to the entry that currently holds it.
func (r *Reconciler) entry(app string) (*appEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.apps[app]; ok {
		return e, nil
	}
	return nil, fmt.Errorf("no such application %q", app)
}

// Sync reconciles one application now, applying if its policy allows.
func (r *Reconciler) Sync(ctx context.Context, app string) error {
	e, err := r.entry(app)
	if err != nil {
		return err
	}
	return r.sync(ctx, e, false)
}

// SyncNow reconciles one application now and applies whatever the plan
// contains, whether or not the policy is automated. It is what the API's sync
// action calls: a manual policy means "do not deploy on a schedule", not "never
// deploy".
//
// Requests that arrive while one is already running collapse onto the single
// one already queued, and the extras return application.ErrSyncPending. Without
// that, N requests all serialised on the lease and redeployed the swarm N
// times, with the scheduled tick queued behind all of them — at twenty requests
// under a wait policy the application went unobserved for over an hour.
//
// The queue slot goes back the moment this sync starts rather than when it
// finishes. A request arriving while one runs is asking about a repository this
// one has already read, so coalescing it there would silently drop a fix
// somebody had just pushed.
func (r *Reconciler) SyncNow(ctx context.Context, app string) error {
	run, err := r.AcceptSync(app)
	if err != nil {
		return err
	}
	return run(ctx)
}

// AcceptSync reserves an application's manual-sync slot and returns the sync to
// run, or application.ErrSyncPending if one is already running with another
// queued behind it.
//
// The split exists so the caller can detach the work and still answer honestly.
// The API returns 202 before the sync has done anything, so if the decision to
// coalesce were made inside the detached goroutine the response would already
// have gone out claiming a sync was started that never was.
func (r *Reconciler) AcceptSync(app string) (func(context.Context) error, error) {
	e, err := r.entry(app)
	if err != nil {
		return nil, err
	}

	select {
	case e.pending <- struct{}{}:
	default:
		return nil, application.ErrSyncPending
	}

	return func(ctx context.Context) error {
		work, release, err := e.acquire(ctx)
		<-e.pending
		if err != nil {
			return err
		}
		defer release()
		return r.reconcile(work, e, true)
	}, nil
}

// sync takes the application's lease and reconciles it.
func (r *Reconciler) sync(ctx context.Context, e *appEntry, force bool) error {
	work, release, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return r.reconcile(work, e, force)
}

// reconcile is one pass over one application, with the lease already held.
//
// The recover is what keeps one application's panic from being every
// application's outage. Nothing in this repository recovered anywhere, so a
// panic in one loop took down the process — every other application's reconcile,
// the app-set loop, and the API that would have explained it — which is exactly
// the blast radius this package's own doc comment rejects for an unreachable
// repository. Under Swarm the task restarts, so the symptom was a controller
// restart-looping on one bad application with the trace only in the daemon's
// logs and no per-application attribution.
//
// It becomes an ordinary reconcile failure, which needs no new state: setError
// records it against the application, the loop counts it and backs off, and a
// deterministic panic therefore settles at one stack trace every thirty minutes
// on the one application rather than a restart loop. Fixing the cause is picked
// up on the next tick, with no restart needed.
func (r *Reconciler) reconcile(ctx context.Context, e *appEntry, force bool) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic reconciling %q: %v", e.name, p)
			// The stack is the only thing that makes a recovered panic
			// diagnosable, and it is not in the error because the error is what
			// the status endpoint publishes.
			r.log.Error("recovered a panic reconciling an application",
				"application", e.name, "panic", p, "stack", string(debug.Stack()))
		}
		if err != nil {
			r.setError(e, err)
		}
	}()

	// Read the current spec now: a Replace may have swapped it while this
	// reconcile waited for the lease, and the swapped-in spec is the one to act
	// on.
	return r.reconcileHeld(ctx, e, r.currentSpec(e), force)
}

func (r *Reconciler) reconcileHeld(ctx context.Context, e *appEntry, spec application.Spec, force bool) error {
	checkout, err := r.fetch.Fetch(ctx, spec.Name, spec.Source)
	if err != nil {
		return fmt.Errorf("fetching source: %w", err)
	}

	built, err := r.build.Build(ctx, spec.Name, spec.Source, checkout)
	if err != nil {
		return fmt.Errorf("reading source: %w", err)
	}

	backend, err := r.swarms.Backend(ctx, swarms.Target{Swarm: spec.Destination.Swarm})
	if err != nil {
		return fmt.Errorf("resolving destination: %w", err)
	}
	backend = withRegistryAuth(backend, r.registryAuth(spec.Name))
	backend = withForbiddenSecrets(backend, r.forbidden)
	backend = withAllowedReferences(backend, spec.Allow)
	backend = r.withOutOfBandNotifier(ctx, backend, spec)
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

	live, doomed := r.observe(ctx, e, spec, backend, engine, plan)
	// Two lists, because reporting and acting are not the same set: everything
	// found is reported, and only what a redeploy could actually put right is
	// acted on. A third thing is acted on and is not a redeploy at all — doomed
	// names the services this application installed and no longer declares,
	// which no deploy can remove because applying deletes nothing.
	drifted := driftedReleases(plan, live, detected)
	fixable := driftedReleases(plan, live, convergeable)

	// Read before recording: record overwrites the states this compares
	// against, so asking afterwards would always find them equal and drift
	// would never look like a transition.
	was := r.syncState(e)
	wasLive := r.driftState(e)
	r.record(ctx, e, backend, plan, checkout.Revision, nil, live)

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
		dispatch(ctx, spec, notify.Event{
			Type:     notify.DriftDetected,
			Revision: checkout.Revision,
			At:       r.now(),
		})
	}
	if len(drifted) > 0 && wasLive != application.DriftStateDetected {
		dispatch(ctx, spec, notify.Event{
			Type:     notify.LiveDriftDetected,
			Revision: checkout.Revision,
			At:       r.now(),
			Message:  "the running services of " + strings.Join(drifted, ", ") + " no longer match the repository",
		})
	}

	if install+upgrade == 0 {
		// This render matched what is stored, so whatever the last apply could
		// not reproduce, this plan did. Clearing here rather than only after the
		// next apply is what keeps a chart that renders unstably *sometimes* —
		// a date rolling over at midnight is the honest version of it — from
		// staying held, and with it its drift correction, at a revision it has
		// since settled at.
		r.setUnstable(e, "", nil)
	}
	if install+upgrade+len(fixable)+actionable(doomed) == 0 {
		return nil
	}
	if !spec.SyncPolicy.Automated && !force {
		return nil
	}
	// Before checkCompat and before anything is written: an application whose
	// last apply did not settle is held here. A forced sync is not subject to it
	// — an operator asking explicitly is the way past.
	if !force {
		if err := r.checkReproducible(e, checkout.Revision); err != nil {
			return err
		}
	}
	if err := checkCompat(plan); err != nil {
		return err
	}
	return r.apply(ctx, e, spec, backend, engine, plan, built, checkout, live, doomed)
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

// unsettled names the releases a plan would still change, in plan order.
func unsettled(plan *charts.Plan) []string {
	var out []string
	for _, rp := range plan.Releases {
		if rp.Action != charts.ActionUnchanged {
			out = append(out, rp.Name)
		}
	}
	return out
}

// setUnstable records — or, with an empty revision, clears — the finding that an
// application's render is not reproducible.
func (r *Reconciler) setUnstable(e *appEntry, revision string, releases []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentLocked(e) {
		e.unstable, e.unstableReleases = revision, releases
	}
}

// checkReproducible refuses to deploy an application whose last apply did not
// settle at this revision.
//
// A chart that renders differently every time — `{{ now }}` in a label, uuidv4,
// randAlphaNum, all ordinary Helm idioms, and CE's renderFuncs removes only env,
// expandenv and getHostByName — makes every plan an upgrade. Nothing converges:
// each reconcile rewrites every service in the release, rolls every task, and
// writes another revision into raft. At the three-minute default that is a full
// rolling restart every three minutes and some 480 Docker Configs per release per
// day, indefinitely, on the manager node this controller runs on.
//
// The confirming re-plan has always been able to see it and only ever said so.
// This is the acting half: deployed once, still out of date on the very next
// render, is not a state another deploy improves. So the application is held at
// that revision, and the hold is released by the thing that could actually change
// the answer — a new commit, a changed spec, or an operator forcing a sync.
//
// The whole application is held rather than the offending release. An apply is
// not release-granular in the way that would help: the prune, the drift
// correction and the resource sweep all key off the same plan, and letting them
// act on a render nobody can reproduce is how a config gets deleted on the
// strength of one render and recreated by the next.
//
// It is not a backoff on top of a backoff. The reconcile loop already doubles its
// interval per consecutive failure, so returning an error here is what turns a
// three-minute redeploy loop into a thirty-minute one that deploys nothing while
// the status and the log say why.
func (r *Reconciler) checkReproducible(e *appEntry, revision string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if !r.currentLocked(e) || e.unstable == "" || e.unstable != revision {
		return nil
	}
	return fmt.Errorf("refusing to deploy %s again at %s: it was deployed and the very next render planned it as out of date once more, "+
		"so this chart does not render the same twice — a template using now, uuidv4 or randAlphaNum does exactly this. "+
		"Redeploying would roll every task and store another revision on every interval without ever converging, so it is held "+
		"until the revision changes, the application is edited, or a sync is requested",
		strings.Join(e.unstableReleases, ", "), revision)
}

// apply deploys the plan and then re-plans.
//
// Re-planning costs a second render, and it is worth it: it is the difference
// between reporting that a sync was attempted and observing that it landed. It
// also surfaces a chart whose render is not deterministic — one that would
// otherwise sit permanently out of sync, deploying on every single tick — on
// the first sync rather than never.
func (r *Reconciler) apply(ctx context.Context, e *appEntry, spec application.Spec, backend charts.Backend, engine Engine, plan *charts.Plan, built *source.Built, checkout git.Checkout, live map[string]*application.ReleaseDrift, doomed map[string]doomedSet) error {
	started := r.now()
	dispatch(ctx, spec, notify.Event{
		Type:     notify.SyncStarted,
		Revision: checkout.Revision,
		At:       started,
	})

	// Before the apply, for a workload where two instances at once is worse
	// than none: the departing release is gone before its replacement exists,
	// so a rename cannot make them overlap. A failure here stops the apply,
	// which is the point — proceeding would create the overlap this ordering
	// was chosen to avoid.
	if spec.SyncPolicy.PruneFirst {
		// Both sweeps, because pruneFirst is about not overlapping and a service
		// renamed within a template overlaps exactly as a renamed release does.
		// They are independent, so one failing is no reason not to attempt the
		// other, and either failing stops the apply.
		if err := errors.Join(
			r.prune(ctx, spec, backend, engine, plan.Orphaned),
			r.pruneServices(ctx, spec, backend, doomed),
		); err != nil {
			// Reported as a failed sync, which the other ordering's identical
			// failure deliberately is not. The asymmetry is the whole point of
			// the option: this sweep runs before the apply and stops it, so
			// nothing was deployed and the sync delivered none of what the
			// repository asked for. Below, the deploy has already landed and only
			// the cleanup did not, so its result stands.
			//
			// Returning bare left a sync-started with no outcome on the event
			// stream and a status still showing the *previous* sync's result —
			// so the one ordering that guarantees nothing is running was also the
			// one that reported the last time something was (#107).
			r.failSync(ctx, e, spec, checkout.Revision, started, err)
			return err
		}
	}

	results, applyErr := engine.Apply(ctx, plan, charts.InstallOptions{
		// From the plan, not recomputed: apply must stamp the same owner it
		// classified against, or a release is installed under one id and the
		// next plan goes looking for its orphans under another.
		Owner:   plan.Owner,
		Wait:    spec.SyncPolicy.Wait,
		Timeout: time.Duration(spec.SyncPolicy.Timeout),
		// Resolved rather than passed straight through: an application that says
		// nothing gets application.DefaultHistoryMax, and only one that writes 0
		// down gets the engine's keep-everything.
		HistoryMax: spec.SyncPolicy.Retention(),
	})
	for _, res := range results {
		r.log.Info("release applied", "application", spec.Name, "release", res.Name, "action", res.Action, "revision", res.Revision)
	}

	// After the apply because the two are disjoint — a release the plan would
	// install or upgrade is never one this compared — and because a failed
	// apply is the wrong moment to start correcting something else.
	if applyErr == nil {
		applyErr = r.converge(ctx, spec, backend, plan, live)
	}

	// A cancelled context is not an outcome and is dispatched as neither. The
	// controller is stopping, or Remove is retiring this application; both used
	// to arrive here as sync-failed carrying "context canceled" through every
	// notifier the deployment has, and to be written down as a failed sync. With
	// syncPolicy.wait an apply blocks for as long as the rollout takes, so a
	// clean `docker service update` of the controller paged somebody for each
	// application it caught mid-apply — which is most of them (#107).
	if cancelled(applyErr) {
		return fmt.Errorf("applying: %w", applyErr)
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
		Type:     notify.SyncSucceeded,
		Revision: checkout.Revision,
		At:       result.FinishedAt,
	}
	if applyErr != nil {
		notified.Type = notify.SyncFailed
		notified.Message = applyErr.Error()
	}
	dispatch(ctx, spec, notified)

	if applyErr != nil {
		r.recordResult(e, result)
		return fmt.Errorf("applying: %w", applyErr)
	}

	// After the apply and before the confirming re-plan. After, so a failed
	// apply leaves the old release running rather than already deleted — the
	// recoverable half of the two failure modes, and the right default for
	// everything that is not harmed by a moment of overlap. Before, so the
	// re-plan below observes the swarm as prune left it rather than reporting
	// the releases it has just deleted straight back as orphans.
	//
	// Held rather than returned on the spot, so that the resource sweep below is
	// still reached. That is the whole of the change here: returning early meant
	// one release whose teardown was stuck silenced the config, secret and
	// network sweep for every release in the plan, under a comment claiming the
	// sweep ran "always here, under both orderings" (#107).
	var swept error
	if !spec.SyncPolicy.PruneFirst {
		swept = errors.Join(
			r.prune(ctx, spec, backend, engine, plan.Orphaned),
			r.pruneServices(ctx, spec, backend, doomed),
		)
	}

	// Always here, under both orderings, and never before the apply.
	//
	// pruneFirst exists because two instances of a workload at once is worse
	// than none; a network or a config has no such hazard, and none of the three
	// can be removed before the service holding it has gone anyway. Running them
	// after the apply means that under pruneFirst the departing services are
	// already gone by the time their configs are attempted, which is the best
	// order available rather than a compromise.
	r.pruneResources(ctx, e, spec, backend, doomed)

	if swept != nil {
		r.recordResult(e, result)
		return swept
	}

	after, err := engine.PlanApply(ctx, built.ReleaseFile, built.Charts, charts.PlanOptions{
		Owner:    application.OwnerID(r.controller, spec.Name),
		ReadFile: built.ReadFile,
	})
	if err != nil {
		// The deploy landed; only the confirmation failed. Reporting the sync
		// as failed would be wrong, so the result stands and the error is the
		// reconcile's.
		r.recordResult(e, result)
		return fmt.Errorf("re-planning after apply: %w", err)
	}
	// The re-plan's other answer, and the acting half of what its comment above
	// only surfaced. A release this apply has just deployed and that the very
	// next render still calls out of date will not be reached by deploying it
	// again, so it is recorded and the next reconcile at this revision refuses.
	if stuck := unsettled(after); len(stuck) > 0 {
		r.setUnstable(e, checkout.Revision, stuck)
		r.log.Warn("a release was deployed and immediately planned as out of date again, so its chart does not render the same twice; it will not be deployed again at this revision",
			"application", spec.Name, "releases", stuck, "revision", checkout.Revision)
	} else {
		r.setUnstable(e, "", nil)
	}
	// Re-observed, not carried forward: the correction and the sweep above are
	// exactly the things this needs to confirm, and reporting the pre-apply
	// reading would leave an application that has just been put right still
	// showing drifted services and orphans that are already gone. The second
	// reading is cheap in the case that matters — a sweep that worked leaves no
	// candidates, so no history is read at all.
	afterLive, _ := r.observe(ctx, e, spec, backend, engine, after)
	r.record(ctx, e, backend, after, checkout.Revision, result, afterLive)
	return nil
}

// failSync terminates a sync that delivered nothing: it dispatches sync-failed
// and records the outcome, so that a notifier pairing sync-started with an
// outcome gets one and the status stops reporting the sync before this.
//
// Only the pruneFirst sweep uses it. The path below it needs a result on the
// success side too, for record to commit alongside the confirming re-plan, and a
// helper that produced both would be the shape this file had when a cleanup
// failure was being reported as a failed deploy. This one says one thing.
//
// A cancelled context is not a failure and gets neither event nor result, for
// the reason apply gives: a shutdown caught mid-sweep is the controller
// stopping, not the sweep failing.
func (r *Reconciler) failSync(ctx context.Context, e *appEntry, spec application.Spec, revision string, started time.Time, err error) {
	if cancelled(err) {
		return
	}

	result := &application.SyncResult{
		Revision:   revision,
		StartedAt:  started,
		FinishedAt: r.now(),
		Succeeded:  false,
		Error:      err.Error(),
	}
	dispatch(ctx, spec, notify.Event{
		Type:     notify.SyncFailed,
		Revision: revision,
		At:       result.FinishedAt,
		Message:  err.Error(),
	})
	// recordResult and not record: reconcileHeld published this pass's plan,
	// revision, drift and releases before apply was called, and nothing has been
	// deployed since, so the outcome is the only thing missing. Re-reading the
	// swarm would ask about a state that has not moved. Health catches up on the
	// next reconcile, when record reads this result as SyncFailed — exactly how a
	// failed apply already behaves.
	r.recordResult(e, result)
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
//
// It takes the whole comparison rather than the list of releases derived from
// it, and re-derives that list here, because the wait below needs the other half
// of the same reading: which *services* each correction is for. Deriving the
// releases from the report and passing the services beside them would be two
// parameters that must agree, for a walk over the plan that costs nothing.
func (r *Reconciler) converge(ctx context.Context, spec application.Spec, backend charts.Backend, plan *charts.Plan, live map[string]*application.ReleaseDrift) error {
	drifted := driftedReleases(plan, live, convergeable)
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
		if err := backend.DeployStack(ctx, release, manifests[release], ""); err != nil {
			errs = append(errs, fmt.Errorf("converging release %q: %w", release, err))
			continue
		}
		if err := r.awaitConverged(ctx, spec, backend, release, correcting(live[release])); err != nil {
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
		dispatch(ctx, spec, notify.Event{
			Type:    notify.DriftConverged,
			At:      r.now(),
			Message: "redeployed " + strings.Join(converged, ", "),
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
//
// What it is asked about is this correction's own work, which is the one thing
// this differs from waitReady in. StackServices answers for everything carrying
// the release's namespace label, and a correction writes only what the manifest
// declares — so a service somebody attached by hand, or one the chart dropped
// and the sweep has given up on (maxPruneAttempts), was rolled up into the
// verdict on a write that never touched it. The permanent version of that is
// Swarm's UpdateStatus, which persists until a service's *next* update: an
// undeclared service that was once rolled back carries rollback_completed for
// ever, so every correction of that release reported "did not converge" over
// somebody else's history (#130). A service the deploy does write has its status
// cleared by that write (manager/controlapi.UpdateService resets it), so nothing
// here can hide a rollback of our own.
//
// An empty scope rolls up everything, which is the state before a comparison
// narrowed it and cannot arise from convergeable: a release is only corrected
// when something was found modified or missing on it.
func (r *Reconciler) awaitConverged(ctx context.Context, spec application.Spec, backend charts.Backend, release string, wrote []string) error {
	if !spec.SyncPolicy.Wait {
		return nil
	}

	timeout := time.Duration(spec.SyncPolicy.Timeout)
	if timeout <= 0 {
		timeout = defaultConvergeTimeout
	}
	deadline := r.now().Add(timeout)

	for {
		switch c := charts.Rollup(scopedTo(backend.StackServices(ctx, release), wrote)); c.Phase {
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

// scopedTo keeps the states of the named services, so a rollup answers about
// them and not about everything sharing their namespace label.
//
// A name the swarm has nothing under is simply absent, and that is the answer
// rather than a gap: a service the comparison found missing has not been created
// yet, and a rollup of nothing is progressing — which is the correct verdict on
// a deploy whose service has not appeared.
func scopedTo(states []charts.ServiceState, names []string) []charts.ServiceState {
	if len(names) == 0 {
		return states
	}
	out := make([]charts.ServiceState, 0, len(names))
	for _, s := range states {
		if slices.Contains(names, s.Name) {
			out = append(out, s)
		}
	}
	return out
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
		result, err := prune.Release(ctx, r.log, backend, engine, release, prune.VolumePurge{
			Enabled:  spec.SyncPolicy.PruneVolumes,
			Registry: r.swarms,
			Target:   swarms.Target{Swarm: spec.Destination.Swarm},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("pruning release %q: %w", release, err))
			continue
		}
		pruned = append(pruned, release)
		// Nothing about volumes here: prune.Release reports what its purge
		// actually reached, which on a multi-node swarm is not the same as what
		// the setting asked for.
		r.log.Warn("pruned a release the application no longer declares",
			"application", spec.Name, "release", release)
		if result != nil && len(result.OrphanedNetworks) > 0 {
			// Left in place by the chart engine because they may be shared.
			// Reported, or an operator believes the cleanup was complete.
			r.log.Warn("networks created for the release were left in place; they may be shared",
				"application", spec.Name, "release", release, "networks", result.OrphanedNetworks)
		}
	}

	if len(pruned) > 0 {
		dispatch(ctx, spec, notify.Event{
			Type:    notify.ResourcesPruned,
			At:      r.now(),
			Message: "pruned " + strings.Join(pruned, ", "),
		})
	}
	err := errors.Join(errs...)
	// Not for a cancellation: a teardown the controller stopped part-way through
	// has not failed, and prune.purgeVolumes builds its error straight out of
	// ctx.Err().
	if err != nil && !cancelled(err) {
		dispatch(ctx, spec, notify.Event{
			Type:    notify.PruneFailed,
			At:      r.now(),
			Message: err.Error(),
		})
	}
	return err
}

// pruneServices deletes the services this application installed under a release
// it still declares and whose chart has stopped declaring them.
//
// What may be deleted was decided in departed, against the swarm, git and this
// controller's own revision records together. Nothing is re-derived here: this
// is the write, and separating it from the proof is what keeps the proof
// testable without a daemon.
//
// A backend that cannot delete a single service reports the orphans and removes
// nothing, rather than failing the sync over a capability the deployment does
// not have.
//
// The sync is not failed by a sweep that does not work, for the same reason the
// release sweep is not: the deploy landed, and a service that would not go is a
// candidate again on the next tick, since nothing about the evidence changed.
// The error is still returned, because the reconcile did not do everything it
// set out to.
func (r *Reconciler) pruneServices(ctx context.Context, spec application.Spec, backend charts.Backend, doomed map[string]doomedSet) error {
	if !spec.SyncPolicy.PruneResources || len(doomed) == 0 {
		return nil
	}
	remover, ok := backend.(resourceRemover)
	if !ok {
		r.log.Warn("this swarm's backend cannot remove a single resource, so what the chart no longer declares was left in place",
			"application", spec.Name)
		return nil
	}

	var errs []error
	var removed []string
	for _, release := range slices.Sorted(maps.Keys(doomed)) {
		for _, svc := range doomed[release].services {
			if err := remover.RemoveService(ctx, svc.id); err != nil {
				errs = append(errs, fmt.Errorf("pruning service %q of release %q: %w", svc.name, release, err))
				continue
			}
			removed = append(removed, svc.name)
			// Warn rather than Info, like every other deletion here: this is the
			// controller removing somebody's running service, and it is the log
			// line an operator goes looking for afterwards.
			r.log.Warn("pruned a service the release no longer declares",
				"application", spec.Name, "release", release, "service", svc.name)

			// Reported and never deleted. pruneVolumes means "when a whole
			// release goes, its data goes with it", which is a boundary an
			// operator can hold in their head; a service leaving a template is a
			// far more frequent event, and nothing here can prove another stack
			// does not mount the same volume. Saying so is the difference between
			// a cleanup an operator can finish and one they believe is complete.
			if len(svc.volumes) > 0 {
				r.log.Warn("volumes the pruned service mounted were left in place",
					"application", spec.Name, "release", release,
					"service", svc.name, "volumes", svc.volumes)
			}
		}
	}

	if len(removed) > 0 {
		dispatch(ctx, spec, notify.Event{
			Type:    notify.ResourcesPruned,
			At:      r.now(),
			Message: "pruned " + strings.Join(removed, ", "),
		})
	}
	err := errors.Join(errs...)
	// Not for a cancellation, for the reason the release sweep gives.
	if err != nil && !cancelled(err) {
		dispatch(ctx, spec, notify.Event{
			Type:    notify.PruneFailed,
			At:      r.now(),
			Message: err.Error(),
		})
	}
	return err
}

// pruneResources deletes the networks, configs and secrets this application
// installed under a release it still declares and whose chart has stopped
// declaring them.
//
// The same rule as pruneServices, proved in the same walk, written separately
// because it is written at a different moment — see the call site.
//
// It returns nothing, and that is the decision rather than an oversight. Swarm
// refuses to remove a config, secret or network that is still in use, and a
// resource whose service was removed moments ago is still in use while its tasks
// drain. That refusal is the ordinary case, not a fault: failing the reconcile
// on it would report an error on every interval for something that is working
// exactly as intended. So a failure is warned and raised as an event, and the
// next interval tries again.
//
// It does not try for ever, though, which the sweep used to and which is not
// the same thing as costing nothing — see maxPruneAttempts for what a permanent
// refusal costs in aggregate and why three passes is where this stops. The
// count is kept per resource here and read back in departed, which is what
// decides whether the resource arrives in resources or in retained.
//
// This is also why there is no retry loop here, unlike prune.purgeVolumes. That
// one has to settle within the pass because the uninstall around it is about to
// delete the very records that prove ownership; here nothing is being deleted
// that a later sweep would need.
func (r *Reconciler) pruneResources(ctx context.Context, e *appEntry, spec application.Spec, backend charts.Backend, doomed map[string]doomedSet) {
	if !spec.SyncPolicy.PruneResources || len(doomed) == 0 {
		return
	}
	remover, ok := backend.(resourceRemover)
	if !ok {
		return // pruneServices has already said so.
	}

	counts := r.failedPrunes(e)
	if counts == nil {
		counts = map[string]int{}
	}

	var errs []error
	var removed []string
	for _, release := range slices.Sorted(maps.Keys(doomed)) {
		for _, res := range doomed[release].resources {
			var err error
			switch res.kind {
			case application.ResourceConfig:
				err = remover.RemoveConfig(ctx, res.id)
			case application.ResourceSecret:
				err = remover.RemoveSecret(ctx, res.id)
			case application.ResourceNetwork:
				err = remover.RemoveNetwork(ctx, res.id)
			}
			if err != nil {
				counts[pruneKey(release, res)]++
				// Warn, not error: still in use is the expected answer while the
				// services that referenced it are shutting down. The attempt and
				// the limit say where this one stands rather than the line
				// promising another go — it may have just used its last, and
				// departed says so on the next pass.
				r.log.Warn("could not prune a resource the release no longer declares",
					"application", spec.Name, "release", release,
					"kind", string(res.kind), "resource", res.name, "error", err,
					"attempt", counts[pruneKey(release, res)], "limit", maxPruneAttempts)
				errs = append(errs, fmt.Errorf("pruning %s %q of release %q: %w", res.kind, res.name, release, err))
				continue
			}
			// Cleared, not merely left alone: a resource that went on the fourth
			// attempt after three refusals must not carry those three into a
			// name that comes back.
			delete(counts, pruneKey(release, res))
			removed = append(removed, string(res.kind)+" "+res.name)
			r.log.Warn("pruned a resource the release no longer declares",
				"application", spec.Name, "release", release,
				"kind", string(res.kind), "resource", res.name)
		}
	}
	r.recordPruneFailures(e, counts)

	if len(removed) > 0 {
		dispatch(ctx, spec, notify.Event{
			Type:    notify.ResourcesPruned,
			At:      r.now(),
			Message: "pruned " + strings.Join(removed, ", "),
		})
	}
	// Not for a cancellation, for the reason the release sweep gives.
	if err := errors.Join(errs...); err != nil && !cancelled(err) {
		dispatch(ctx, spec, notify.Event{
			Type:    notify.PruneFailed,
			At:      r.now(),
			Message: err.Error(),
		})
	}
}

// currentLocked reports whether e is still the entry the set holds under its
// name. The caller holds mu.
//
// It is the identity check that a name lookup cannot make. A name that leaves
// the set and returns gets a fresh entry with a fresh lease, so the departed
// application's sync — which may still be running, having been cancelled but not
// yet noticed — passes any test of the form "is this name present". It then
// overwrote the *new* application's status and plan with the departed one's
// readings. Comparing the pointer covers both cases at once: a name that never
// returned fails the map lookup, and one that did fails the comparison.
func (r *Reconciler) currentLocked(e *appEntry) bool { return r.apps[e.name] == e }

// syncState reports what was last observed for an application, or unknown if it
// has left the set.
func (r *Reconciler) syncState(e *appEntry) application.SyncState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.currentLocked(e) {
		return e.status.Sync.State
	}
	return application.SyncUnknown
}

// currentSpec returns an application's spec as it stands now, or the zero spec
// if it has left the set. The loop reads through it so a Replace takes effect on
// the next tick.
func (r *Reconciler) currentSpec(e *appEntry) application.Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.currentLocked(e) {
		return e.spec
	}
	return application.Spec{}
}

// record stores the status a plan implies. A nil result leaves whatever the
// last sync recorded in place. An application that has left the set while its
// sync was in flight is skipped, so a late write does not resurrect it or land
// on the successor of its name.
//
// live is the per-release comparison against what is running, keyed by release
// name, and is nil for an application not using that mode — which leaves the
// whole axis absent from the status rather than present and empty.
//
// The swarm is read *before* the lock is taken, and that is the point of the
// two-phase shape below. This used to hold the exclusive global lock across a
// live daemon call per release — each one a whole-swarm snapshot on an
// uncancellable context — so one application whose daemon was slow blocked every
// API read, every lifecycle mutation and every other application's reconcile,
// with RWMutex parking later readers behind the waiting writer. /healthz touches
// none of it and kept answering, so nothing restarted the container. Reading
// first is safe because the caller holds the application's lease: no other sync
// for it can be writing the fields this reads.
func (r *Reconciler) record(ctx context.Context, e *appEntry, backend charts.Backend, plan *charts.Plan, revision string, result *application.SyncResult, live map[string]*application.ReleaseDrift) {
	sync, releases := drift.FromPlan(plan)
	sync.Revision = revision

	r.mu.RLock()
	present := r.currentLocked(e)
	sync.LastSync = e.status.Sync.LastSync
	r.mu.RUnlock()
	if !present {
		return
	}
	if result != nil {
		sync.LastSync = result
	}

	// Read the swarm before taking a view on it. A sync that did not succeed is
	// what separates a rollout that is slow from one that is broken, so the
	// health of each release is decided against the outcome recorded above
	// rather than against the previous tick's.
	syncFailed := sync.LastSync != nil && !sync.LastSync.Succeeded

	// One read for every release that has one, rather than one read each. A
	// release the plan would install is declared and not deployed, which is
	// knowable from the plan, so it is left out of the request rather than asked
	// about and ignored.
	deployed := make([]string, 0, len(releases))
	for i := range releases {
		if releases[i].Action != application.ActionInstall {
			deployed = append(deployed, releases[i].Name)
		}
	}
	states, readErr := readStacks(ctx, backend, deployed)
	if readErr != nil {
		r.log.Warn("reading the swarm failed; publishing this application's releases as unread",
			"application", e.name, "error", readErr)
	}

	for i := range releases {
		rel := &releases[i]
		rel.Health, rel.Services = health.Release(health.Input{
			States: states[rel.Name],
			// A release the plan would install is declared and not deployed.
			// That is knowable from the plan, so it costs no call to the swarm.
			Installed:  rel.Action != application.ActionInstall,
			SyncFailed: syncFailed,
			// This is the one caller that must not read an unavailable daemon as
			// an empty swarm: it asks once and publishes the answer, rather than
			// polling until one arrives. See stackServicesReader. readStacks is
			// all-or-nothing, so a release that was asked about and is absent
			// from the answer was not read at all.
			ReadFailed: readErr != nil && rel.Action != application.ActionInstall,
			// The live comparison's other answer: what it read and did not
			// report. It is folded into the sync axis below, and this is the
			// one thing it settles on the health axis — whether a rollback a
			// service is still carrying is about anything outstanding.
			AsDeclared: asDeclared(states[rel.Name], live[rel.Name]),
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

	status := application.Status{
		Sync:       sync,
		Health:     health.Application(releases),
		Drift:      drift.Application(releases),
		Releases:   releases,
		ObservedAt: r.now(),
	}

	// Re-checked rather than assumed: the reads above are where the time goes,
	// and an application can leave the set during them.
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.currentLocked(e) {
		return
	}
	e.status = status
	e.plan = plan
}

// driftState reports the live-drift state last observed for an application.
// Unknown covers both "not using this mode" and "left the set", neither of which
// is a transition worth reporting.
func (r *Reconciler) driftState(e *appEntry) application.DriftState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.currentLocked(e) && e.status.Drift != nil {
		return e.status.Drift.State
	}
	return application.DriftStateUnknown
}

func (r *Reconciler) recordResult(e *appEntry, result *application.SyncResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.currentLocked(e) {
		return
	}
	e.status.Sync.LastSync = result
	e.status.ObservedAt = r.now()
}

// setError records why a reconcile failed without discarding what was last
// observed. A repository that cannot be reached does not make the swarm's
// state unknown — it makes it unverified, which is what the error says.
//
// A cancellation is not one of those and is dropped: the controller stopping,
// or this application being removed from the set, is not a reason to leave
// "context canceled" as the last word on every application in the list.
func (r *Reconciler) setError(e *appEntry, err error) {
	if cancelled(err) {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.currentLocked(e) {
		return
	}
	e.status.Error = err.Error()
	e.status.ObservedAt = r.now()
}

// View returns one application's spec and last observed status.
//
// The status is cloned, so what the caller gets is a snapshot it owns. A Status
// is handed out by value but carries slices and pointers, so without this every
// caller shared the store's backing array and its *ReleaseDrift, *Compat and
// *SyncResult — safe only while nobody sorted, appended or wrote through them,
// which is an invariant nothing stated and nothing enforced. See Status.Clone.
func (r *Reconciler) View(app string) (application.View, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.apps[app]
	if !ok {
		return application.View{}, false
	}
	return application.View{Spec: e.spec.Clone(), Status: e.status.Clone()}, true
}

// Views returns every application, in the order they were declared or added,
// each a snapshot the caller owns for the reason View gives.
func (r *Reconciler) Views() []application.View {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]application.View, 0, len(r.order))
	for _, name := range r.order {
		e := r.apps[name]
		out = append(out, application.View{Spec: e.spec.Clone(), Status: e.status.Clone()})
	}
	return out
}

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
		return nil, application.ErrNotPlanned
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
		return application.History{}, application.ErrNotPlanned
	}

	backend, err := r.swarms.Backend(ctx, swarms.Target{Swarm: spec.Destination.Swarm})
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
