// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package prune deletes the resources of an application that has left the app
// set.
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

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// Engine is the part of the chart engine a sweep uses. *charts.Engine
// implements it.
//
// Declared here rather than reused from reconcile so that prune does not
// import the reconciler for a two-method interface it can state itself.
type Engine interface {
	List(ctx context.Context) ([]charts.Release, error)
	Uninstall(ctx context.Context, release string, purgeVolumes bool) (*charts.UninstallResult, error)
}

// Options configures a Pruner. Everything has a working default.
type Options struct {
	Swarms swarms.Registry
	Engine func(charts.Backend) Engine
	// Volumes extends the deletion to the named volumes of what it removes.
	// Off by default, and the only irreversible part: everything else prune
	// deletes is recreated from git the moment the application comes back.
	Volumes bool
	Log     *slog.Logger
}

// Pruner deletes the releases of applications that have left the app set.
type Pruner struct {
	swarms  swarms.Registry
	engine  func(charts.Backend) Engine
	volumes bool
	log     *slog.Logger
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
	return &Pruner{swarms: o.Swarms, engine: o.Engine, volumes: o.Volumes, log: o.Log}
}

// Departed deletes the releases of every application this controller owns that
// is absent from desired, and returns the applications it emptied.
//
// desired must be an app set that has actually loaded. The caller vouches for
// that: this cannot tell "the set declares nothing" apart from "the set could
// not be read", and the two want opposite things done. See appset.Loop, which
// only calls this after a load and an apply have both succeeded.
//
// Every application is attempted and the failures are collected rather than
// returned at the first. They are independent, and because the next sweep
// rediscovers whatever is still stamped on the swarm, a failure costs a
// reconcile interval rather than an orphan nobody looks at again.
func (p *Pruner) Departed(ctx context.Context, desired []string) ([]string, error) {
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
	for _, app := range departed(releases, desired) {
		failed := false
		for _, release := range app.releases {
			if err := p.uninstall(ctx, engine, app.name, release); err != nil {
				errs = append(errs, err)
				failed = true
			}
		}
		// Only when the whole application went. A partial deletion is still an
		// application with resources on the swarm, and reporting it as pruned
		// would say the opposite of what the errors say.
		if !failed {
			pruned = append(pruned, app.name)
		}
	}
	return pruned, errors.Join(errs...)
}

func (p *Pruner) uninstall(ctx context.Context, engine Engine, app, release string) error {
	result, err := engine.Uninstall(ctx, release, p.volumes)
	if err != nil {
		return fmt.Errorf("pruning release %q of departed application %q: %w", release, app, err)
	}
	// Warn rather than Info, like the departure itself: this is the controller
	// deleting somebody's running stack, and it is the log line an operator
	// goes looking for afterwards.
	p.log.Warn("pruned the release of an application that left the app set",
		"application", app, "release", release, "volumes", p.volumes)

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
// absent from desired.
//
// The result is in the order List returned the releases, which is by name, so
// what gets logged and reported does not reshuffle between sweeps.
func departed(releases []charts.Release, desired []string) []departedApp {
	keep := make(map[string]struct{}, len(desired))
	for _, name := range desired {
		keep[name] = struct{}{}
	}

	var out []departedApp
	at := map[string]int{}
	for _, rel := range releases {
		app, ok := owner(rel)
		if !ok {
			continue
		}
		if _, still := keep[app]; still {
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

// owner reports which of this controller's applications installed a release,
// and whether this controller installed it at all.
//
// Both halves of the stamp have to agree, which is the check charts makes
// before it will call a release orphaned. An id-only comparison would treat a
// stamp copied onto a second release as proof of ownership, and prune would
// then delete a release nobody installed under that name. A stamp that does not
// parse is not evidence of anything either, so it counts as unowned — and
// unowned is never pruned.
func owner(rel charts.Release) (string, bool) {
	ref, err := charts.ParseOwner(rel.Owner)
	if err != nil {
		return "", false
	}
	if ref.Kind != charts.OwnerKindRelease || ref.Name != rel.Name {
		return "", false
	}
	return application.AppFromOwnerID(ref.ID)
}
