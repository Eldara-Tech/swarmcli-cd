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

// doomedSet is everything one release has that a sweep may delete.
//
// The two halves travel together because they are proved together, and are kept
// apart because they are written at different moments: pruneFirst orders the
// services, and the other three kinds always go after the apply.
type doomedSet struct {
	services  []departedService
	resources []departedResource
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
func (r *Reconciler) observe(ctx context.Context, spec application.Spec, b charts.Backend, engine Engine, plan *charts.Plan) (map[string]*application.ReleaseDrift, map[string]doomedSet) {
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

	drifts := r.liveDrift(spec, plan, views)
	if !sweep {
		return drifts, nil
	}
	doomed := r.departed(ctx, spec, ldb, engine, plan, views)
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
func (r *Reconciler) liveDrift(spec application.Spec, plan *charts.Plan, views map[string]releaseView) map[string]*application.ReleaseDrift {
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
		out[rp.Name] = drift.Live(v.desired, v.live)
	}
	return out
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
func (r *Reconciler) departed(ctx context.Context, spec application.Spec, ldb liveDriftBackend, engine Engine, plan *charts.Plan, views map[string]releaseView) map[string]doomedSet {
	var out map[string]doomedSet
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
			set.resources = append(set.resources, departedResource{application.ResourceConfig, name, v.configs[name]})
		}
		for _, name := range claimed.secrets {
			set.resources = append(set.resources, departedResource{application.ResourceSecret, name, v.secrets[name]})
		}
		for _, name := range claimed.networks {
			set.resources = append(set.resources, departedResource{application.ResourceNetwork, name, v.networks[name]})
		}

		if out == nil {
			out = make(map[string]doomedSet, len(plan.Releases))
		}
		out[rp.Name] = set
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
func withOrphans(drifts map[string]*application.ReleaseDrift, doomed map[string]doomedSet) map[string]*application.ReleaseDrift {
	if len(doomed) == 0 {
		return drifts
	}
	if drifts == nil {
		drifts = make(map[string]*application.ReleaseDrift, len(doomed))
	}

	for release, set := range doomed {
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

	live, doomed := r.observe(ctx, spec, backend, engine, plan)
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

	if install+upgrade+len(fixable)+len(doomed) == 0 {
		return nil
	}
	if !spec.SyncPolicy.Automated && !force {
		return nil
	}
	if err := checkCompat(plan); err != nil {
		return err
	}
	return r.apply(ctx, spec, backend, engine, plan, built, checkout, fixable, doomed)
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
func (r *Reconciler) apply(ctx context.Context, spec application.Spec, backend charts.Backend, engine Engine, plan *charts.Plan, built *source.Built, checkout git.Checkout, drifted []string, doomed map[string]doomedSet) error {
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
		// Both sweeps, because pruneFirst is about not overlapping and a service
		// renamed within a template overlaps exactly as a renamed release does.
		// They are independent, so one failing is no reason not to attempt the
		// other, and either failing stops the apply.
		if err := errors.Join(
			r.prune(ctx, spec, backend, engine, plan.Orphaned),
			r.pruneServices(ctx, spec, backend, doomed),
		); err != nil {
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
		if err := errors.Join(
			r.prune(ctx, spec, backend, engine, plan.Orphaned),
			r.pruneServices(ctx, spec, backend, doomed),
		); err != nil {
			r.recordResult(spec.Name, result)
			return err
		}
	}

	// Always here, under both orderings, and never before the apply.
	//
	// pruneFirst exists because two instances of a workload at once is worse
	// than none; a network or a config has no such hazard, and none of the three
	// can be removed before the service holding it has gone anyway. Running them
	// after the apply means that under pruneFirst the departing services are
	// already gone by the time their configs are attempted, which is the best
	// order available rather than a compromise.
	r.pruneResources(ctx, spec, backend, doomed)

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
	// Re-observed, not carried forward: the correction and the sweep above are
	// exactly the things this needs to confirm, and reporting the pre-apply
	// reading would leave an application that has just been put right still
	// showing drifted services and orphans that are already gone. The second
	// reading is cheap in the case that matters — a sweep that worked leaves no
	// candidates, so no history is read at all.
	afterLive, _ := r.observe(ctx, spec, backend, engine, after)
	r.record(spec.Name, backend, after, checkout.Revision, result, afterLive)
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
		notify.Dispatch(ctx, notify.Event{
			Application: spec.Name,
			Type:        notify.ResourcesPruned,
			At:          r.now(),
			Message:     "pruned " + strings.Join(removed, ", "),
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
// next interval tries again — which costs nothing, because the evidence is an
// immutable revision record that is still there.
//
// This is also why there is no retry loop here, unlike prune.purgeVolumes. That
// one has to settle within the pass because the uninstall around it is about to
// delete the very records that prove ownership; here nothing is being deleted
// that a later sweep would need.
func (r *Reconciler) pruneResources(ctx context.Context, spec application.Spec, backend charts.Backend, doomed map[string]doomedSet) {
	if !spec.SyncPolicy.PruneResources || len(doomed) == 0 {
		return
	}
	remover, ok := backend.(resourceRemover)
	if !ok {
		return // pruneServices has already said so.
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
				// Warn, not error: still in use is the expected answer while the
				// services that referenced it are shutting down.
				r.log.Warn("could not prune a resource the release no longer declares; it will be tried again",
					"application", spec.Name, "release", release,
					"kind", string(res.kind), "resource", res.name, "error", err)
				errs = append(errs, fmt.Errorf("pruning %s %q of release %q: %w", res.kind, res.name, release, err))
				continue
			}
			removed = append(removed, string(res.kind)+" "+res.name)
			r.log.Warn("pruned a resource the release no longer declares",
				"application", spec.Name, "release", release,
				"kind", string(res.kind), "resource", res.name)
		}
	}

	if len(removed) > 0 {
		notify.Dispatch(ctx, notify.Event{
			Application: spec.Name,
			Type:        notify.ResourcesPruned,
			At:          r.now(),
			Message:     "pruned " + strings.Join(removed, ", "),
		})
	}
	if err := errors.Join(errs...); err != nil {
		notify.Dispatch(ctx, notify.Event{
			Application: spec.Name,
			Type:        notify.PruneFailed,
			At:          r.now(),
			Message:     err.Error(),
		})
	}
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
