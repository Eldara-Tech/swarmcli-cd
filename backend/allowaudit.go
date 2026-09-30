// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"slices"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
)

// UnpermittedNames converts a manifest and returns the names in it that the
// allowlist would have to name for rejectForbiddenResources to let a deploy of
// it through: what its services reference external:, volumes and cluster mounts
// that are not the release's own, and networks it joins or declares outside the
// release. Each list is sorted, and names only.
//
// What no allowlist can grant is left out — the controller's own secrets,
// configs, volumes and networks — because naming it would not help, and for the
// self release it is its own. So is a declared config, secret or network whose
// name something else holds: after a deploy that succeeded, what holds a name
// the release declares is the release's, and asking is a read per name.
func (b *Backend) UnpermittedNames(ctx context.Context, req capability.AllowRequest) (application.Allow, error) {
	// Conversion reads an allowlist for one thing, a bind's source, which is not
	// what this reports; permitting every path keeps a bind from failing it.
	stack, err := cdcompose.ConvertUnresolved(ctx, req.Manifest, req.Name, req.Files, b.api,
		application.Allow{HostPaths: []string{"/"}})
	if err != nil {
		return application.Allow{}, err
	}
	mine, err := b.mounts(ctx)
	if err != nil {
		return application.Allow{}, err
	}

	var need application.Allow
	add := func(list *[]string, allowed []string, controllers map[string]struct{}, name string) {
		if _, theirs := controllers[name]; theirs || permits(allowed, name) || slices.Contains(*list, name) {
			return
		}
		*list = append(*list, name)
	}
	for _, svc := range stack.Services {
		secrets, configs := externalRefs(stack, svc)
		for _, name := range secrets {
			add(&need.Secrets, req.Allow.Secrets, mine.secrets, name)
		}
		for _, name := range configs {
			add(&need.Configs, req.Allow.Configs, mine.configs, name)
		}
		for _, m := range volumeSources(svc) {
			if !ownVolume(stack, m) {
				add(&need.Volumes, req.Allow.Volumes, mine.volumes, m.Source)
			}
		}
	}
	for _, name := range stack.ExternalNetworks {
		if !inControllersStack(mine.namespace, name) {
			add(&need.Networks, req.Allow.Networks, nil, name)
		}
	}
	for _, nw := range stack.Networks {
		if !inControllersStack(mine.namespace, nw.Name) && !scopedUnder(stack.Namespace.Name(), nw.Name) {
			add(&need.Networks, req.Allow.Networks, nil, nw.Name)
		}
	}
	for _, list := range [][]string{need.Secrets, need.Configs, need.Volumes, need.Networks} {
		slices.Sort(list)
	}
	return need, nil
}
