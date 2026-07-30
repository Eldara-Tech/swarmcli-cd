// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/charts"

	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
)

// The two halves of a live drift comparison, exported only because the caller
// that needs them cannot reach the daemon.
//
// The reconciler holds a charts.Backend, which carries no Docker client and no
// way to read a ServiceSpec — its StackServices returns charts.ServiceState, a
// display projection with a running count and no spec in it. Both methods here
// are one line over something that already exists; what they add is b.api. The
// reconciler reaches them through an optional-interface upgrade, the same way
// it reaches WithRegistryAuth, so a backend that cannot answer (a Phase 3
// remote one) simply does not implement them.
//
// Comparing the two is deliberately not here. That part needs no daemon, and
// keeping it in package drift is what lets it be tested exhaustively against
// hand-built spec pairs — which is the only way the normalisation rules can be
// got right.

// DesiredServices converts a rendered manifest to the Swarm specs it declares.
//
// It is the same conversion DeployStack does before applying, so what drift
// compares against is what a sync would write, rather than a second reading of
// the manifest that could disagree with it.
func (b *Backend) DesiredServices(ctx context.Context, manifest, stack string) (*cdcompose.Stack, error) {
	return cdcompose.Convert(ctx, manifest, stack, b.api)
}

// LiveServices returns the stack's running services with their full specs, by
// scoped name.
//
// Named for what it returns rather than StackServices, which is taken by the
// charts.Backend method above it and answers a different question.
func (b *Backend) LiveServices(ctx context.Context, stack string) (map[string]swarm.Service, error) {
	return b.stackServices(ctx, stack)
}

// RemoveService deletes one service by id.
//
// By id and not by name, because the caller has already read the service it
// means: resolving a name again here would open a window in which the name
// pointed at something else. A service already gone is not an error — a sweep
// that raced another deletion has still got the outcome it wanted.
//
// It is the whole of the applier's delete surface, and deliberately narrow.
// RemoveStack takes a namespace and removes everything under it; this takes one
// service the caller has proved it may delete, and the proof lives in the
// reconciler rather than here.
func (b *Backend) RemoveService(ctx context.Context, id string) error {
	if err := b.api.ServiceRemove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing service %q: %w", id, err)
	}
	return nil
}

// The other three kinds a manifest declares, read and removed the same way a
// service is: scoped by the stack's namespace label on the way in, deleted by id
// on the way out.
//
// Each lister is stack-scoped, and that is the whole reason they exist rather
// than the sweep reusing ListConfigs, NetworkScopes or SecretNames. Those three
// are deliberately unfiltered because the chart engine asks them "does this name
// exist anywhere on the swarm" about external resources, which carry no label to
// filter on (Eldara-Tech/swarmcli#510). Narrowing one of them to a namespace
// would break that pre-flight silently; adding a scoped reader beside it does
// not.
//
// Name to id, because that is exactly what the sweep needs: it matches on the
// scoped name, which is the only key a manifest and a live resource share, and
// deletes by id for the reason RemoveService gives.

// LiveNetworks returns the networks carrying this stack's namespace label, by
// scoped name.
func (b *Backend) LiveNetworks(ctx context.Context, stack string) (map[string]string, error) {
	nets, err := b.api.NetworkList(ctx, network.ListOptions{Filters: stackFilter(stack)})
	if err != nil {
		return nil, fmt.Errorf("listing the stack's networks: %w", err)
	}
	out := make(map[string]string, len(nets))
	for _, n := range nets {
		out[n.Name] = n.ID
	}
	return out, nil
}

// LiveConfigs returns the configs carrying this stack's namespace label, by
// scoped name.
//
// A release-history config is never included. It carries com.swarmcli.* labels
// and no namespace, so the filter above cannot see one anyway — this is the same
// belt and braces RemoveStack applies, and for the same reason: a future change
// that did put a namespace label on them must not silently turn this sweep into
// "delete the history too".
func (b *Backend) LiveConfigs(ctx context.Context, stack string) (map[string]string, error) {
	configs, err := b.api.ConfigList(ctx, swarm.ConfigListOptions{Filters: stackFilter(stack)})
	if err != nil {
		return nil, fmt.Errorf("listing the stack's configs: %w", err)
	}
	out := make(map[string]string, len(configs))
	for _, c := range configs {
		if c.Spec.Labels[charts.LabelType] == charts.TypeRelease {
			continue
		}
		out[c.Spec.Name] = c.ID
	}
	return out, nil
}

// LiveSecrets returns the secrets carrying this stack's namespace label, by
// scoped name.
func (b *Backend) LiveSecrets(ctx context.Context, stack string) (map[string]string, error) {
	secrets, err := b.api.SecretList(ctx, swarm.SecretListOptions{Filters: stackFilter(stack)})
	if err != nil {
		return nil, fmt.Errorf("listing the stack's secrets: %w", err)
	}
	out := make(map[string]string, len(secrets))
	for _, s := range secrets {
		out[s.Spec.Name] = s.ID
	}
	return out, nil
}

// RemoveNetwork deletes one network by id.
//
// Not RemoveOverlayNetwork, which resolves a name against an unfiltered list of
// every network on the swarm: that is the wrong cost on a path that runs every
// reconcile, and it reopens the window RemoveService closes by taking an id the
// caller has already read.
//
// A network Swarm has already garbage-collected is not an error. The last task
// leaving an overlay network removes it, which routinely happens between the
// sweep reading the network and deciding to delete it.
func (b *Backend) RemoveNetwork(ctx context.Context, id string) error {
	if err := b.api.NetworkRemove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing network %q: %w", id, err)
	}
	return nil
}

// RemoveConfig deletes one config by id.
//
// Distinct from DeleteConfig, which removes a release record by name on behalf
// of the chart engine. This one is the sweep's, takes an id, and tolerates a
// config that is already gone.
func (b *Backend) RemoveConfig(ctx context.Context, id string) error {
	if err := b.api.ConfigRemove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing config %q: %w", id, err)
	}
	return nil
}

// RemoveSecret deletes one secret by id.
func (b *Backend) RemoveSecret(ctx context.Context, id string) error {
	if err := b.api.SecretRemove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing secret %q: %w", id, err)
	}
	return nil
}
