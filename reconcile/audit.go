// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"context"
	"maps"
	"slices"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
	"github.com/Eldara-Tech/swarmcli-cd/prune"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// releaseLister is the part of the chart engine the audit reads: the current
// revision of every release on a swarm. *charts.Engine implements it.
type releaseLister interface {
	List(ctx context.Context) ([]charts.Release, error)
}

// auditAllowlists warns, for each application in the set, of the allow entries
// the releases it has on the swarm need and its allowlist does not name — what
// the next deploy of one of them would be refused for.
//
// It is how an upgrade that narrowed what a release may reach without an entry
// says so before the first refusal does: every release this controller installed
// is read from its current record, with the manifest and files it was deployed
// with, and asked of the backend (capability.AllowAuditor). Nothing is refused
// and nothing is written. It reports names and never a value; the manifest is
// not logged. A backend or engine that cannot answer is skipped, and a release
// whose manifest cannot be read is said so and passed over.
func (r *Reconciler) auditAllowlists(ctx context.Context) {
	r.mu.RLock()
	bySwarm := map[string]map[string]application.Spec{}
	for _, name := range r.order {
		spec := r.apps[name].spec
		if bySwarm[spec.Destination.Swarm] == nil {
			bySwarm[spec.Destination.Swarm] = map[string]application.Spec{}
		}
		bySwarm[spec.Destination.Swarm][name] = spec
	}
	r.mu.RUnlock()

	for _, swarm := range slices.Sorted(maps.Keys(bySwarm)) {
		apps := bySwarm[swarm]
		backend, err := r.swarms.Backend(ctx, swarms.Target{Swarm: swarm})
		if err != nil {
			continue // the application's own reconcile reports an unreachable destination
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
			r.log.Warn("could not list the swarm's releases to check the applications' allowlists", "error", err)
			continue
		}

		need := map[string]*application.Allow{}
		held := map[string][]string{}
		for _, rel := range releases {
			// "" for a release no application of this controller installed.
			app, _ := prune.Owner(rel, r.controller)
			spec, inSet := apps[app]
			if !inSet {
				continue
			}
			names, err := auditor.UnpermittedNames(ctx, capability.AllowRequest{
				ManifestRequest: capability.ManifestRequest{Name: rel.Name, Manifest: rel.Manifest, Files: rel.Files},
				Allow:           spec.Allow,
			})
			if err != nil {
				r.log.Warn("could not read a release's recorded manifest to check its application's allowlist",
					"application", app, "release", rel.Name, "error", err)
				continue
			}
			if len(names.Secrets)+len(names.Configs)+len(names.Volumes)+len(names.Networks) == 0 {
				continue
			}
			if need[app] == nil {
				need[app] = &application.Allow{}
			}
			merge(&need[app].Secrets, names.Secrets)
			merge(&need[app].Configs, names.Configs)
			merge(&need[app].Volumes, names.Volumes)
			merge(&need[app].Networks, names.Networks)
			held[app] = append(held[app], rel.Name)
		}

		for _, app := range slices.Sorted(maps.Keys(need)) {
			attrs := []any{"application", app, "releases", held[app]}
			for _, entry := range []struct {
				field string
				names []string
			}{
				{"allow.secrets", need[app].Secrets},
				{"allow.configs", need[app].Configs},
				{"allow.volumes", need[app].Volumes},
				{"allow.networks", need[app].Networks},
			} {
				if len(entry.names) > 0 {
					attrs = append(attrs, entry.field, entry.names)
				}
			}
			r.log.Warn("releases of this application reference names its allowlist does not permit, and their next "+
				"deploy will be refused until it does: add these entries to the application's allow in the app set",
				attrs...)
		}
	}
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
