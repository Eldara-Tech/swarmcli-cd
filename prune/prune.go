// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package prune deletes what git no longer declares: the resources of an
// application that has left the app set, and the rule deciding which services a
// release still in it has stopped declaring.
//
// The two halves sit here together because they answer the same question at
// different scopes — "is this provably ours, and provably obsolete" — and get it
// right the same way, by refusing to act on a single forgeable signal. The
// sweeping of departed applications is below; the service rule is Undeclared and
// Claim at the foot of the file, kept as pure functions so that the reconciler
// owns the reads and this owns the policy.
//
// App-of-apps (#47, decision D-e) is deliberately report-only: an application
// dropped from the git app set stops being reconciled and its stack is left
// running and surfaced as orphaned. This package is the acting half (#54),
// behind its own opt-in, because reporting an orphan is safe and deleting one
// is not.
//
// # Two senses of "prune"
//
// The chart engine already has Engine.Prune, and it means something else:
// trimming an individual release's revision history to the last N, which this
// repository exposes as SyncPolicy.HistoryMax. Nothing here touches revision
// history. "Prune" in this package always means deleting a release's deployed
// resources.
//
// # Why the swarm is the source of truth
//
// The obvious signal — the list of applications the running loop watched leave
// — does not survive a restart, and a controller that forgot its orphans on
// every restart would leave exactly the stacks nobody is looking at. So the
// departure is not remembered; it is rediscovered. Every release this
// controller installs carries the owner stamp application.OwnerID writes, and
// that stamp is stored on the swarm. Any release stamped for an application the
// app set no longer declares has, by definition, been left behind.
//
// The stamp is also what makes deletion safe. A release without one, or with
// one another tool wrote, is unmanaged here and is never a candidate — the same
// distinction charts.Plan draws between Orphaned and Unmanaged, for the same
// reason.
//
// # The stamp is not enough on its own
//
// A stamp records who installed a release, and it is only rewritten by a
// reconcile that deploys. Rename an application without changing anything else
// and the release is now planned under an owner that contradicts its stamp,
// which the plan does notice (swarmcli#511) — but noticing corrects the stamp
// no earlier than the next reconcile that gets that far, and a sweep can run
// first. Until it does, the release still carries the departed name's stamp,
// and reading the stamp alone would delete a stack that a current application
// is reconciling (#62). So the sweep is given the releases the current
// applications declare as well, and spares anything in that list. The stamp
// says who installed it; that list says whether anybody is still responsible
// for it.
//
// # Two controllers on one swarm
//
// The stamp names a controller as well as an application, and the sweep only
// considers its own. Without that, a second swarmcli-cd sharing the swarm would
// answer "which releases belong to an application my app set no longer
// declares" about the *first* controller's applications, and delete them — a
// silent, swarm-wide deletion of somebody else's work, triggered by nothing more
// than starting a second controller.
//
// Two controllers on one swarm must therefore be given distinct ids. The
// default is shared, so two controllers that both take it will still collide;
// that is a deployment mistake this package cannot detect from the inside,
// because a controller cannot tell "my own releases from before a restart" from
// "another controller using my id".
//
// # One swarm
//
// The sweep covers the swarm this controller runs in, because swarms.Registry
// resolves exactly that one and cannot enumerate others (D2). That is complete
// for the OSS build. A multi-swarm companion replacing the seam would have to
// extend it with a lister before this could find a departed application's
// releases on a swarm it no longer names.
package prune

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// Uninstaller deletes a release's recorded revisions once its resources are
// gone. *charts.Engine implements it.
type Uninstaller interface {
	Uninstall(ctx context.Context, release string, purgeVolumes bool) (*charts.UninstallResult, error)
}

// Engine is the part of the chart engine a sweep uses. *charts.Engine
// implements it.
//
// Declared here rather than reused from reconcile so that prune does not
// import the reconciler for an interface it can state itself.
type Engine interface {
	Uninstaller
	List(ctx context.Context) ([]charts.Release, error)
}

// How long Release waits for **one** volume to be released by the tasks that
// were using it, and how often it retries. Variables rather than constants so a
// test does not spend the real interval discovering that the retry works.
//
// Per volume and not per release, because the thing being waited for is per
// volume: each one is held by its own containers, which shut down in their own
// time. A whole-release budget makes the last volume of a large stack inherit
// whatever the first few spent, so the same stack passes or fails depending on
// how many volumes it happens to declare.
//
// Thirty seconds is a guess at "the containers have gone", and it is not
// derived from stop_grace_period — the daemon waits that long before killing a
// container, so a stack declaring `stop_grace_period: 60s` will still be
// holding its volumes when this gives up. That is survivable rather than
// correct: the release records are still there, so the next sweep tries again
// and the second pass succeeds. It is only survivable because of that ordering.
var (
	volumeSettleTimeout  = 30 * time.Second
	volumeSettleInterval = 2 * time.Second
)

// Release deletes one release's deployed resources and then its records, in
// that order, and reports the networks the engine left behind.
//
// The order is the whole point. charts.Engine.Uninstall aggregates its errors
// and deletes the release-history configs regardless of whether its own stack
// removal worked — and those configs carry the owner stamp, which is the only
// thing that lets a later sweep find this release again. Calling it directly on
// a stack that will not come down would delete the evidence and strand the
// resources permanently, invisible to every future prune. So the resources come
// down first, through the backend, and the records are only deleted once there
// is nothing left for them to point at.
//
// This is shared with the per-application prune in reconcile, which faces the
// same hazard for the same reason.
//
// log is taken rather than returned-to, because what a volume purge managed is
// not a value the two callers would do anything different with — it is a thing
// an operator has to be told, and only this knows what was and was not covered.
func Release(ctx context.Context, log *slog.Logger, backend charts.Backend, engine Uninstaller, release string, volumes bool) (*charts.UninstallResult, error) {
	if err := backend.RemoveStack(release); err != nil {
		return nil, fmt.Errorf("removing the stack: %w", err)
	}
	if volumes {
		if err := purgeVolumes(ctx, log, backend, release); err != nil {
			return nil, err
		}
	}
	// Volumes already handled above, so this is only the records now.
	return engine.Uninstall(ctx, release, false)
}

// purgeVolumes deletes the stack's named volumes that this node can see,
// waiting for the tasks that mounted them to let go, and says plainly when that
// was not all of them.
//
// The services were removed a moment ago and Swarm does not wait for their
// containers to die, so the first attempt routinely fails with "volume is in
// use". Retrying is not papering over a race — it is the only way to observe
// the thing this is waiting for; the daemon offers no "tasks have drained"
// signal to poll instead.
//
// A volume that never frees is returned as an error and, because the caller
// has not yet deleted the release records, is retried by the next sweep rather
// than left as an untracked volume nobody knows to remove.
//
// # What this cannot do
//
// The listing is node-local — see backend.StackVolumes for the daemon's own
// code path — so on a swarm of more than one node this removes the volumes of
// the node the controller talks to and nothing else. Nothing in the OSS build
// can reach the others: swarms.Registry resolves one swarm as one daemon (D2),
// and there is no swarm-wide listing of named volumes to ask for instead.
//
// It does not stop and it does not refuse. Stopping would make --prune-volumes
// permanently fail on every multi-node swarm, including for the great majority
// of releases that declare no named volume at all — this cannot tell that case
// apart from the dangerous one, so refusing would trade silent data retention
// for a prune that never completes and an application that is never done. It
// deletes what it can reach and reports exactly that, with the label filter
// that finds the rest, because a cleanup an operator can finish is worth more
// than one they believe is complete.
//
// Nor does it hold the release records back. The records are what lets a *later
// sweep* find a release again, and a later sweep on this node would see the same
// node-local listing and be no better off — keeping them buys an operator
// nothing and costs an application that can never finish being pruned. The
// volumes themselves keep the stack's namespace label, which is a durable
// marker `docker volume ls` finds on each node, and that is the thread the
// warning below hands over.
func purgeVolumes(ctx context.Context, log *slog.Logger, backend charts.Backend, release string) error {
	names, err := backend.StackVolumes(ctx, release)
	if err != nil {
		return fmt.Errorf("listing the stack's volumes: %w", err)
	}

	for _, name := range names {
		// Per volume. Each is held by its own containers and drains on its own
		// schedule, so sharing one budget across a stack makes the last volume
		// answer for the first.
		deadline := time.Now().Add(volumeSettleTimeout)
		for {
			err := backend.RemoveVolume(ctx, name)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return fmt.Errorf("removing volume %q: %w", name, ctx.Err())
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("removing volume %q: still in use after %s: %w", name, volumeSettleTimeout, err)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("removing volume %q: %w", name, ctx.Err())
			case <-time.After(volumeSettleInterval):
			}
		}
	}

	if swarmWideVolumes(ctx, backend) {
		// Warn, like every other deletion here — this is data going — but only
		// when there was some. A release that declared no volume has nothing to
		// report, and on a one-node swarm an empty listing really does say so.
		if len(names) > 0 {
			log.Warn("deleted the release's volumes", "release", release, "volumes", names)
		}
		return nil
	}
	// Warn whether or not anything was removed here, and especially when
	// nothing was: an empty node-local listing on a multi-node swarm is not
	// evidence that the release had no volumes, and reporting it as a completed
	// purge is exactly the claim this must not make.
	log.Warn("deleted only this node's volumes for the release: the daemon's volume list is node-local "+
		"and this swarm has more than one node, so any volumes the release left on another node are still there",
		"release", release, "volumes", names,
		"remedy", "docker volume ls --filter label=com.docker.stack.namespace="+release+", on each node")
	return nil
}

// swarmSizer counts the swarm's nodes. *backend.Backend implements it; a
// backend that does not cannot say whether its volume listing was the whole
// swarm's, which for a deletion has to mean the same as knowing it was not.
type swarmSizer interface {
	SwarmNodes(ctx context.Context) (int, error)
}

// swarmWideVolumes reports whether a node-local volume listing was in fact the
// whole swarm's — which is exactly the single-node case, and nothing weaker
// will do. A count that could not be read is not one node.
func swarmWideVolumes(ctx context.Context, backend charts.Backend) bool {
	s, ok := backend.(swarmSizer)
	if !ok {
		return false
	}
	n, err := s.SwarmNodes(ctx)
	return err == nil && n == 1
}

// Options configures a Pruner. Everything has a working default.
type Options struct {
	Swarms swarms.Registry
	Engine func(charts.Backend) Engine
	// Volumes extends the deletion to the named volumes of what it removes.
	// Off by default, and the only irreversible part: everything else prune
	// deletes is recreated from git the moment the application comes back.
	Volumes bool
	// ControllerID is this controller's half of the owner stamp, and bounds
	// what the sweep will consider at all. Empty is
	// application.DefaultControllerID — which is correct for the single
	// controller case and wrong the moment there are two, so a second
	// controller on the swarm must be given its own.
	ControllerID string
	Log          *slog.Logger
}

// Pruner deletes the releases of applications that have left the app set.
type Pruner struct {
	swarms     swarms.Registry
	engine     func(charts.Backend) Engine
	volumes    bool
	controller string
	log        *slog.Logger
}

// New returns a Pruner. A nil Pruner is the report-only default, so callers
// construct one only when prune is enabled.
func New(o Options) *Pruner {
	if o.Swarms == nil {
		o.Swarms = swarms.Get()
	}
	if o.Engine == nil {
		o.Engine = func(b charts.Backend) Engine { return charts.NewEngineWith(b) }
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.ControllerID == "" {
		o.ControllerID = application.DefaultControllerID
	}
	return &Pruner{
		swarms: o.Swarms, engine: o.Engine, volumes: o.Volumes,
		controller: o.ControllerID, log: o.Log,
	}
}

// Departed deletes the releases of every application this controller owns that
// is absent from desired, and returns the applications it emptied.
//
// desired must be an app set that has actually loaded. The caller vouches for
// that: this cannot tell "the set declares nothing" apart from "the set could
// not be read", and the two want opposite things done. See appset.Loop, which
// only calls this after a load and an apply have both succeeded.
//
// declared names the releases the applications still in the set hold, and no
// release named in it is ever deleted whatever its stamp says. That is what
// makes renaming an application a handover rather than a teardown (#62): the
// release keeps the departed application's stamp until a reconcile deploys it
// under the new one, which is the next pass for an automated application and
// not until it is asked for a manual one — either way, later than a sweep that
// runs in between. The stamp answers "who installed this"; declared answers "is
// anybody still responsible for it", and only the second is a safe basis for
// deleting.
//
// Every application is attempted and the failures are collected rather than
// returned at the first. They are independent, and because the next sweep
// rediscovers whatever is still stamped on the swarm, a failure costs a
// reconcile interval rather than an orphan nobody looks at again.
func (p *Pruner) Departed(ctx context.Context, desired, declared []string) ([]string, error) {
	// An app set that declares nothing is indistinguishable from one somebody
	// truncated, and acting on it would delete every managed stack on the
	// swarm. Refusing costs the ability to empty the set in one commit, which
	// an operator can still do one application at a time.
	if len(desired) == 0 {
		p.log.Warn("the app set declares no applications; pruning nothing, " +
			"because an empty set and a truncated one look the same from here")
		return nil, nil
	}

	backend, err := p.swarms.Backend(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("resolving the local swarm: %w", err)
	}
	engine := p.engine(backend)

	releases, err := engine.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the swarm's releases: %w", err)
	}

	var pruned []string
	var errs []error
	for _, app := range departed(releases, desired, declared, p.controller) {
		var failed []failure
		for _, release := range app.releases {
			if err := p.uninstall(ctx, backend, engine, app.name, release); err != nil {
				failed = append(failed, failure{release: release, err: err})
			}
		}
		if len(failed) == 0 {
			pruned = append(pruned, app.name)
			continue
		}

		// A removal can report a failure and still have done the job. Deleting
		// a swarm-scoped network is proxied through swarmkit, and its reply for
		// one that had already gone is neither a not-found the client
		// recognises nor reflected immediately in the network list — so neither
		// the error nor a re-list can answer "did this work".
		//
		// The release records can. They are ordinary Docker configs, written
		// and deleted through this node, and a release whose records are gone
		// is a release that was uninstalled. So the failures are re-examined
		// against what the swarm still lists, and the ones that turn out to
		// describe an already-finished deletion are dropped.
		still, err := p.stillListed(ctx, engine, failed)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(still) == 0 {
			pruned = append(pruned, app.name)
			continue
		}
		for _, f := range still {
			errs = append(errs, f.err)
		}
	}
	return pruned, errors.Join(errs...)
}

// failure is one release's uninstall error, kept beside the release it belongs
// to so it can be re-examined once the swarm has been asked again.
type failure struct {
	release string
	err     error
}

// stillListed returns the failures whose releases the swarm still has.
//
// A release absent from the list has no records left, which is the last thing
// an uninstall deletes — so whatever the call reported, it finished.
func (p *Pruner) stillListed(ctx context.Context, engine Engine, failed []failure) ([]failure, error) {
	releases, err := engine.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("re-checking which releases are left: %w", err)
	}
	present := make(map[string]struct{}, len(releases))
	for _, rel := range releases {
		present[rel.Name] = struct{}{}
	}

	var still []failure
	for _, f := range failed {
		if _, ok := present[f.release]; ok {
			still = append(still, f)
		}
	}
	return still, nil
}

func (p *Pruner) uninstall(ctx context.Context, backend charts.Backend, engine Engine, app, release string) error {
	result, err := Release(ctx, p.log, backend, engine, release, p.volumes)
	if err != nil {
		return fmt.Errorf("pruning release %q of departed application %q: %w", release, app, err)
	}
	// Warn rather than Info, like the departure itself: this is the controller
	// deleting somebody's running stack, and it is the log line an operator
	// goes looking for afterwards.
	//
	// It says nothing about volumes. It used to carry the setting, which reads
	// as a claim that the data went with the stack — and on a swarm of more
	// than one node that claim is only ever partly true. purgeVolumes reports
	// what actually happened, because it is the only thing that knows.
	p.log.Warn("pruned the release of an application that left the app set",
		"application", app, "release", release)

	// The chart engine leaves the networks it auto-created — they may be shared
	// with another stack — and reports them instead. Passing that on is the
	// difference between a cleanup an operator can finish and one they believe
	// is already complete.
	if result != nil && len(result.OrphanedNetworks) > 0 {
		p.log.Warn("networks created for the release were left in place; they may be shared",
			"application", app, "release", release, "networks", result.OrphanedNetworks)
	}
	return nil
}

// departedApp is one application's releases, as the swarm still holds them.
type departedApp struct {
	name     string
	releases []string
}

// departed groups the releases whose owner stamp names an application that is
// absent from desired, less any release an application still in the set
// declares.
//
// An application left with nothing after that filtering does not appear at all:
// there is nothing of its to delete, because every release it used to own now
// belongs to somebody who is still here.
//
// The result is in the order List returned the releases, which is by name, so
// what gets logged and reported does not reshuffle between sweeps.
func departed(releases []charts.Release, desired, declared []string, controller string) []departedApp {
	keep := make(map[string]struct{}, len(desired))
	for _, name := range desired {
		keep[name] = struct{}{}
	}
	held := make(map[string]struct{}, len(declared))
	for _, name := range declared {
		held[name] = struct{}{}
	}

	var out []departedApp
	at := map[string]int{}
	for _, rel := range releases {
		app, ok := Owner(rel, controller)
		if !ok {
			continue
		}
		if _, still := keep[app]; still {
			continue
		}
		if _, taken := held[rel.Name]; taken {
			continue
		}
		if i, seen := at[app]; seen {
			out[i].releases = append(out[i].releases, rel.Name)
			continue
		}
		at[app] = len(out)
		out = append(out, departedApp{name: app, releases: []string{rel.Name}})
	}
	return out
}

// Owner reports which of this controller's applications installed a release, and
// whether this controller installed it at all. It reads a stored revision the
// same way, since a revision carries the stamp its release does.
//
// Both halves of the stamp have to agree, which is the check charts makes
// before it will call a release orphaned. An id-only comparison would treat a
// stamp copied onto a second release as proof of ownership, and prune would
// then delete a release nobody installed under that name. A stamp that does not
// parse is not evidence of anything either, so it counts as unowned — and
// unowned is never pruned.
func Owner(rel charts.Release, controller string) (string, bool) {
	ref, err := charts.ParseOwner(rel.Owner)
	if err != nil {
		return "", false
	}
	if ref.Kind != charts.OwnerKindRelease || ref.Name != rel.Name {
		return "", false
	}
	return application.AppFromOwnerID(controller, ref.ID)
}

// --- the resource rule ---
//
// A service, network, config or secret dropped from a chart's template is never
// removed by applying: backend.ApplyServices and its siblings create and update
// and delete nothing, and neither does charts.Apply. The release is still
// declared, so it is not orphaned and the sweep above never sees it. These two
// functions are the rule that finds it.
//
// The whole difficulty is that a stack is a name prefix plus a
// com.docker.stack.namespace label and nothing else — no /stacks endpoint, no
// owner references, no garbage collection. That label says a resource belongs to
// a release; it does not say this controller put it there, and anything can
// carry it. Deleting on it alone is the mistake #62 was filed for, one scope
// down. So Undeclared produces candidates and Claim requires a second signal
// before any of them is a deletion: a stored revision, stamped by this
// controller for this application, whose manifest declared that resource. The
// label says where a resource lives; the revision record says whose it is, and
// only the second is a safe basis for deleting.
//
// Both take plain name lists and neither knows what kind it is looking at, which
// is deliberate: one rule and one ownership model for all four kinds, applied
// once per kind. Applying it per kind rather than over a merged list is what
// keeps a config from being claimed by a same-named service — the four share one
// namespace of scoped names, so a name is only ever evidence about its own kind.

// Undeclared names the resources of one kind carrying a release's namespace that
// its rendered manifest does not declare.
//
// Both sides are namespace-scoped names — "<release>_<name>" — because that is
// what a live resource carries and the only key the two sides can be matched on.
//
// On its own this is not permission to delete anything; it is the candidate list
// for the ownership check. Sorted, so what gets logged and reported does not
// reshuffle between sweeps.
func Undeclared(running, declared []string) []string {
	if len(running) == 0 {
		return nil
	}
	keep := make(map[string]struct{}, len(declared))
	for _, name := range declared {
		keep[name] = struct{}{}
	}

	var out []string
	for _, name := range running {
		if _, ok := keep[name]; ok {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Claim splits candidates by whether declared names them: what this revision
// proves the repository once asked for, and what is still unaccounted for.
//
// Taking one revision at a time rather than the union of a whole history is what
// lets a caller walk revisions newest first and stop as soon as nothing is left
// unaccounted for — so the ordinary case, a resource dropped last week, reads one
// revision rather than every revision ever recorded. A caller sweeping several
// kinds converts each revision once and calls this per kind, so the walk costs
// what one kind costs.
//
// A candidate no revision of ours ever claims is not ours to delete. It may be
// another tool's, or ours from before the retained history; from here those are
// the same thing, which is that ownership cannot be proved.
func Claim(candidates, declared []string) (claimed, rest []string) {
	if len(candidates) == 0 {
		return nil, nil
	}
	in := make(map[string]struct{}, len(declared))
	for _, name := range declared {
		in[name] = struct{}{}
	}

	for _, name := range candidates {
		if _, ok := in[name]; ok {
			claimed = append(claimed, name)
			continue
		}
		rest = append(rest, name)
	}
	return claimed, rest
}
