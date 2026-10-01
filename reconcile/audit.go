// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
	"github.com/Eldara-Tech/swarmcli-cd/prune"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// releaseLister is the part of the chart engine the audit and the status read:
// the current revision of every release on a swarm. Asserted here, because both
// reach it through a type assertion that would otherwise fall back silently.
type releaseLister interface {
	List(ctx context.Context) ([]charts.Release, error)
}

var _ releaseLister = (*charts.Engine)(nil)

// startAuditLocked runs auditAllowlists for specs beside the loops, counted in
// wg as they are and guarded as startLoopLocked is: once Run's context is done,
// drain may be waiting on wg, and nothing may be added to it. The caller holds mu.
//
// A panic is recovered and logged, as reconcile recovers one: the audit only
// warns, and it reads every recorded manifest through the compose converter, so
// one it cannot convert must not take the controller down on every start.
func (r *Reconciler) startAuditLocked(specs []application.Spec) {
	if r.root == nil || r.root.Err() != nil || len(specs) == 0 {
		return
	}
	ctx := r.root
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				r.log.Error("recovered a panic checking the applications' allowlists", "panic", p)
			}
		}()
		r.auditAllowlists(ctx, specs)
	}()
}

// auditAllowlists warns, for each application in specs, of the allow entries
// the releases it has on the swarm need and its allowlist does not name — what a
// deploy of one of them would be refused for.
//
// It is how an upgrade that narrowed what a release may reach without an entry
// says so before the first refusal does: every release this controller installed
// for one of them is read from its current record, with the manifest and files
// it was deployed with, and asked of the backend (capability.AllowAuditor).
// Nothing is refused and nothing is written; it reports names, and the manifest
// is not logged.
//
// Two warnings, because the two call for different answers. A name that the
// release's own prefix used to hand it, and that is scoped under another
// application's release or one this controller did not install — "web_a_db" for
// release "web", beside release "web_a" — is that release's: reaching it without
// an entry is exactly what is refused now, so it is listed to review, and to
// grant only as a shared external: reference. Every other name is one to add.
//
// A release the application no longer declares is read too, since its record is
// what there is; a sweep, not an entry, is the answer to one of those. A backend
// or engine that cannot answer is skipped, and a release that cannot be read is
// said so and passed over.
func (r *Reconciler) auditAllowlists(ctx context.Context, specs []application.Spec) {
	bySwarm := map[string]map[string]application.Spec{}
	for _, spec := range specs {
		if bySwarm[spec.Destination.Swarm] == nil {
			bySwarm[spec.Destination.Swarm] = map[string]application.Spec{}
		}
		bySwarm[spec.Destination.Swarm][spec.Name] = spec
	}

	for _, swarm := range slices.Sorted(maps.Keys(bySwarm)) {
		if ctx.Err() != nil {
			return
		}
		apps := bySwarm[swarm]
		backend, err := r.swarms.Backend(ctx, swarms.Target{Swarm: swarm})
		if err != nil {
			r.log.Warn("could not resolve a swarm to check the applications' allowlists", "swarm", swarm, "error", err)
			continue
		}
		auditor, ok := backend.(capability.AllowAuditor)
		if !ok {
			continue
		}
		lister, ok := r.newEngine(backend).(releaseLister)
		if !ok {
			continue
		}
		releases, err := lister.List(ctx)
		if err != nil {
			r.log.Warn("could not list the swarm's releases to check the applications' allowlists", "swarm", swarm, "error", err)
			continue
		}
		names := make([]string, 0, len(releases))
		appOf := make(map[string]string, len(releases))
		for _, rel := range releases {
			names = append(names, rel.Name)
			// "" for a release no application of this controller installed.
			appOf[rel.Name], _ = prune.Owner(rel, r.controller)
		}

		need, others := map[string]*application.Allow{}, map[string]*application.Allow{}
		held, heldOthers, owners := map[string]*[]string{}, map[string]*[]string{}, map[string]*[]string{}
		list := func(m map[string]*[]string, app string) *[]string {
			if m[app] == nil {
				m[app] = &[]string{}
			}
			return m[app]
		}
		checked := 0
		for _, rel := range releases {
			if ctx.Err() != nil {
				return
			}
			app := appOf[rel.Name]
			spec, inSet := apps[app]
			if !inSet {
				continue
			}
			got, err := unpermitted(ctx, auditor, capability.AllowRequest{
				ManifestRequest: capability.ManifestRequest{Name: rel.Name, Manifest: rel.Manifest, Files: rel.Files},
				Allow:           spec.Allow,
			})
			if err != nil {
				r.log.Warn("could not check a release against its application's allowlist",
					"application", app, "release", rel.Name, "error", err)
				continue
			}
			checked++
			for _, field := range allowFields(&got) {
				for _, name := range *field.names {
					// Another's only if the prefix rule used to hand it to this
					// release: scoped under this release's name as well.
					into, releases, owner := need, held, ownerOf(name, names)
					if owner != "" && appOf[owner] != app && strings.HasPrefix(strings.ToLower(name), strings.ToLower(rel.Name)+"_") {
						into, releases = others, heldOthers
						merge(list(owners, app), []string{owner})
					}
					if into[app] == nil {
						into[app] = &application.Allow{}
					}
					merge(field.of(into[app]), []string{name})
					merge(list(releases, app), []string{rel.Name})
				}
			}
		}

		for _, app := range slices.Sorted(maps.Keys(need)) {
			r.log.Warn("releases of this application reference names its allowlist does not name, and a deploy of "+
				"them is refused until it does: add these entries to the application's allow in the app set, or "+
				"remove a release the application no longer declares", attrs(app, *held[app], need[app])...)
		}
		for _, app := range slices.Sorted(maps.Keys(others)) {
			r.log.Warn("releases of this application reference names scoped under another release on this swarm, "+
				"which they reached before without an allow entry and are refused now: those are the other "+
				"release's. Permit one only if it is meant to be shared, and then as an external: reference — "+
				"permitting one the release declares hands the other release's object to this one",
				append(attrs(app, *heldOthers[app], others[app]), "scopedUnder", *owners[app])...)
		}
		r.log.Info("checked the applications' releases against their allowlists", "swarm", swarm, "releases", checked)
	}
}

// unpermitted asks the backend, turning a panic into an error, so that one
// release the converter cannot handle is reported like any other it cannot read.
func unpermitted(ctx context.Context, auditor capability.AllowAuditor, req capability.AllowRequest) (names application.Allow, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic reading the recorded manifest: %v", p)
		}
	}()
	return auditor.UnpermittedNames(ctx, req)
}

// ownerOf is the release a name is scoped under, among names: the longest whose
// name followed by '_' begins it, compared without regard to case, or "".
func ownerOf(name string, releases []string) string {
	owner := ""
	for _, rel := range releases {
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(rel)+"_") && len(rel) > len(owner) {
			owner = rel
		}
	}
	return owner
}

// allowField is one of the name lists in an application.Allow, with the key a
// warning names it by.
type allowField struct {
	key   string
	names *[]string
	of    func(*application.Allow) *[]string
}

func allowFields(a *application.Allow) []allowField {
	return []allowField{
		{"allow.secrets", &a.Secrets, func(x *application.Allow) *[]string { return &x.Secrets }},
		{"allow.configs", &a.Configs, func(x *application.Allow) *[]string { return &x.Configs }},
		{"allow.volumes", &a.Volumes, func(x *application.Allow) *[]string { return &x.Volumes }},
		{"allow.networks", &a.Networks, func(x *application.Allow) *[]string { return &x.Networks }},
	}
}

// attrs is a warning's attributes: the application, its releases read, and each
// field with names in it.
func attrs(app string, releases []string, names *application.Allow) []any {
	out := []any{"application", app, "releases", releases}
	for _, field := range allowFields(names) {
		if len(*field.names) > 0 {
			out = append(out, field.key, *field.names)
		}
	}
	return out
}

// merge adds the names in from that into does not already hold, keeping it sorted.
func merge(into *[]string, from []string) {
	for _, name := range from {
		if !slices.Contains(*into, name) {
			*into = append(*into, name)
		}
	}
	slices.Sort(*into)
}
