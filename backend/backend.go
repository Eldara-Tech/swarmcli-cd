// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/charts"
	"github.com/Eldara-Tech/swarmcli/docker"

	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
	"github.com/Eldara-Tech/swarmcli-cd/regauth"
)

// Backend is a charts.Backend built on the moby client.
//
// It replaces charts.NewDockerBackend, which shells out to `docker stack
// deploy` — a development scaffold that would put the docker binary in the
// controller image, and whose applier defects are the reason swarmcli-cd#1
// rejects it: --prune touches services only and swallows its own list error, no
// dry-run, --detach returns before convergence, and update order is Go map
// iteration.
var _ charts.Backend = (*Backend)(nil)

// WithRegistryAuth returns a copy of the backend that authenticates its image
// pulls with auth. The copy shares the client — one swarm's connection pool is
// not duplicated per application — and differs only by the resolver, so the
// per-swarm backend stays shared while the credential stays per application.
//
// It returns charts.Backend so the reconciler can reach it through the swarms
// seam (which hands back that interface) with an optional-interface upgrade,
// rather than depending on this concrete type.
func (b *Backend) WithRegistryAuth(auth regauth.Resolver) charts.Backend {
	c := *b
	c.registryAuth = auth
	return &c
}

// DeployStack converges the swarm to a rendered manifest.
//
// Order matters and is the same order `docker stack deploy` uses: the things a
// service can reference have to exist before the service that references them.
// Volumes are absent from that list on purpose — Swarm creates a named volume
// on the node that first needs it, so there is nothing to pre-create.
//
// Nothing is deleted. Phase 1 is explicitly no prune.
func (b *Backend) DeployStack(name, manifest, resolve string) error {
	ctx := context.Background()

	stack, err := cdcompose.Convert(ctx, manifest, name, b.api)
	if err != nil {
		return err
	}
	if err := b.applyNetworks(ctx, stack); err != nil {
		return err
	}
	if err := b.applySecrets(ctx, stack.Secrets); err != nil {
		return err
	}
	if err := b.applyConfigs(ctx, stack.Configs); err != nil {
		return err
	}
	return b.ApplyServices(ctx, stack, resolve)
}

// RemoveStack deletes the services, networks, configs and secrets carrying the
// stack's namespace label — what `docker stack rm` removes, and nothing more.
//
// Volumes survive, as they do there: a stack's data outliving the stack is the
// whole point of a named volume, and charts has RemoveVolume for the caller
// that means it.
//
// The engine's own release records are untouched. They are Docker configs, but
// they carry com.swarmcli.* labels rather than a stack namespace, so the filter
// below cannot see them — which is what lets a release be uninstalled and its
// history still be readable.
func (b *Backend) RemoveStack(name string) error {
	// A destructive call with no stack to scope to would build the filter
	// "com.docker.stack.namespace=", and what that matches is the daemon's
	// business rather than something to find out here. The engine validates
	// release names, so this is unreachable today; it costs one line to keep it
	// that way.
	if name == "" {
		return fmt.Errorf("refusing to remove a stack with no name")
	}

	ctx := context.Background()

	services, err := b.api.ServiceList(ctx, swarm.ServiceListOptions{Filters: stackFilter(name)})
	if err != nil {
		return fmt.Errorf("listing the stack's services: %w", err)
	}
	for _, s := range services {
		if err := b.api.ServiceRemove(ctx, s.ID); err != nil {
			return fmt.Errorf("removing service %q: %w", s.Spec.Name, err)
		}
	}

	// Services first, then what they were using: a network still attached to a
	// running task cannot be removed, and a config or secret in use is refused
	// outright.
	configs, err := b.api.ConfigList(ctx, swarm.ConfigListOptions{Filters: stackFilter(name)})
	if err != nil {
		return fmt.Errorf("listing the stack's configs: %w", err)
	}
	for _, c := range configs {
		// Belt and braces over the namespace filter above. The engine stores
		// each release revision as a Docker config, and those survive an
		// uninstall — that is what makes a release's history readable after it
		// is gone. They survive only because they carry com.swarmcli.* labels
		// and no stack namespace, which is a property of how the engine happens
		// to stamp them rather than anything this code enforces. Saying so here
		// means a future change that did put a namespace on them would not
		// silently turn uninstall into "delete the history too".
		if c.Spec.Labels[charts.LabelType] == charts.TypeRelease {
			continue
		}
		if err := b.api.ConfigRemove(ctx, c.ID); err != nil {
			return fmt.Errorf("removing config %q: %w", c.Spec.Name, err)
		}
	}

	secrets, err := b.api.SecretList(ctx, swarm.SecretListOptions{Filters: stackFilter(name)})
	if err != nil {
		return fmt.Errorf("listing the stack's secrets: %w", err)
	}
	for _, s := range secrets {
		if err := b.api.SecretRemove(ctx, s.ID); err != nil {
			return fmt.Errorf("removing secret %q: %w", s.Spec.Name, err)
		}
	}

	networks, err := b.api.NetworkList(ctx, network.ListOptions{Filters: stackFilter(name)})
	if err != nil {
		return fmt.Errorf("listing the stack's networks: %w", err)
	}
	for _, n := range networks {
		if err := b.api.NetworkRemove(ctx, n.ID); err != nil {
			return fmt.Errorf("removing network %q: %w", n.Name, err)
		}
	}
	return nil
}

// RefreshSnapshot is a no-op: this backend holds no cache to invalidate.
//
// The method exists because the ambient CE backend reads through a process-wide
// snapshot with a 3s TTL and has to be told when it has gone stale. Here every
// read fetches, which is what makes one process able to serve several swarms
// without them evicting each other's state.
func (b *Backend) RefreshSnapshot() error { return nil }

// StackServices reads one stack's live service states.
//
// Every rule in here belongs to the chart engine and is reached through its own
// exported mapping (Eldara-Tech/swarmcli#508): the running count by actual
// rather than desired state (#480), the target over active nodes (#481), a
// completed one-shot job counting toward its target instead of reading 0/N
// (#443, #494). A second copy would diverge silently, and both directions of
// that are wrong — reporting a release converged while the engine would still
// be waiting, or degraded on a stack that is fine.
//
// A snapshot that cannot be read returns nil, matching the CE backend: the
// caller polls, so an unavailable daemon is "not converged yet" rather than a
// failure to report.
func (b *Backend) StackServices(name string) []charts.ServiceState {
	snap, err := docker.SnapshotWith(context.Background(), b.api)
	if err != nil {
		b.log.Warn("reading the swarm snapshot failed; reporting no services", "stack", name, "error", err)
		return nil
	}
	return charts.ServiceStatesFrom(snap, name)
}
