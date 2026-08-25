// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

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
	return cdcompose.Convert(ctx, manifest, stack, b.api, b.allow)
}

// DeclaredResources converts a rendered manifest to the specs it declares,
// without resolving what its services mount to the ids Swarm addresses them by.
//
// It is for the caller that wants names — the sweep proving a resource was
// declared by one of this application's own stored revisions. That caller is
// asking about the past, so it must not need any of it to still exist:
// DesiredServices resolves every mounted config and secret against the daemon,
// and a revision that mounted one a previous sweep has already deleted does not
// convert at all (#87).
//
// Not a substitute for DesiredServices. What it returns is what ConvertUnresolved
// returns, so the config and secret references in it carry a placeholder id and
// nothing may be applied from it.
func (b *Backend) DeclaredResources(ctx context.Context, manifest, stack string) (*cdcompose.Stack, error) {
	return cdcompose.ConvertUnresolved(ctx, manifest, stack, b.api, b.allow)
}

// LiveServices returns the stack's running services with their full specs, by
// scoped name.
//
// Named for what it returns rather than StackServices, which is taken by the
// charts.Backend method above it and answers a different question.
func (b *Backend) LiveServices(ctx context.Context, stack string) (map[string]swarm.Service, error) {
	return b.stackServices(ctx, stack)
}

// LiveNetworkNames returns every network on the swarm, by id.
//
// It belongs to the live comparison rather than to the sweep, which is why it
// sits here beside LiveServices and not below with LiveNetworks and the other
// two scoped listers: it is deliberately *not* stack-scoped, and putting it in
// that group would contradict the one thing that group has in common.
//
// A spec names the networks its service is attached to by id — the daemon
// rewrites each Target to one as it creates or updates the service
// (daemon/cluster.populateNetworkID) — while a manifest names a network. So
// comparing attachments is a resolution step before it is a comparison, and a
// stack-scoped listing cannot perform it: a service may be attached to an
// external network or to one of the swarm's predefined ones, and neither carries
// this stack's namespace label. An id that could not be named would have to be
// treated as opaque, which is precisely the attachment worth reporting.
//
// Id to name, the opposite direction from LiveNetworks, because these are
// opposite jobs: the sweep matches on a name and deletes by id, and this reads an
// id and reports a name.
func (b *Backend) LiveNetworkNames(ctx context.Context) (map[string]string, error) {
	nets, err := b.api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the swarm's networks: %w", err)
	}
	out := make(map[string]string, len(nets))
	for _, n := range nets {
		out[n.ID] = n.Name
	}
	return out, nil
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
		return fmt.Errorf("removing service '%s': %w", id, err)
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
//
// The two that can be adopted also drop anything without the creation marker
// resource.go writes, and that second condition is not a refinement of the
// first — it is the other half of the ownership proof. The namespace label says
// a resource belongs to a release, and applySecrets and applyConfigs put it on
// objects they *adopted* rather than created, so a secret an operator seeded
// before a chart ever named it carries the same label as one this controller
// made. The marker is the only thing that tells them apart, and the difference
// is whether deleting it loses material nothing can restore. Filtered here
// rather than in the caller because this is the read that decides what a
// candidate even is.
//
// LiveNetworks deliberately does not ask, because applyNetworks does not adopt:
// see createdLabel.

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

// LiveConfigs returns the configs this controller created under this stack's
// namespace, by scoped name.
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
		if _, ours := c.Spec.Labels[createdLabel]; !ours {
			continue
		}
		out[c.Spec.Name] = c.ID
	}
	return out, nil
}

// LiveSecrets returns the secrets this controller created under this stack's
// namespace, by scoped name.
func (b *Backend) LiveSecrets(ctx context.Context, stack string) (map[string]string, error) {
	secrets, err := b.api.SecretList(ctx, swarm.SecretListOptions{Filters: stackFilter(stack)})
	if err != nil {
		return nil, fmt.Errorf("listing the stack's secrets: %w", err)
	}
	out := make(map[string]string, len(secrets))
	for _, s := range secrets {
		if _, ours := s.Spec.Labels[createdLabel]; !ours {
			continue
		}
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
//
// Finding that out takes a second look, and classifying the error is not enough
// on its own — the premise RemoveStack states and answers with stackRemains,
// which this had no equivalent of. A swarm-scoped removal is proxied through
// swarmkit, and the helper that resolves the network there is the one of its
// five siblings that does *not* wrap "not found" in errdefs.NotFound
// (daemon/cluster/helpers.go:238, against :51, :87, :131, :167 and :274 which
// all do). An unclassified error is scored as Unknown and rendered 500
// (api/server/httpstatus/status.go:16), which the client turns into ErrInternal
// (client/errors.go:124), so errdefs.IsNotFound is false for a network that had
// already gone. Reporting that as a failed prune, on the path whose whole
// purpose is honest reporting, is the defect.
//
// So the state is the answer and the error is only a hint about where to look:
// a removal that failed is followed by a re-read, and a network that is no
// longer listed reached the state this was asking for whatever the call said.
func (b *Backend) RemoveNetwork(ctx context.Context, id string) error {
	err := b.api.NetworkRemove(ctx, id)
	if err == nil || errdefs.IsNotFound(err) {
		return nil
	}

	gone, checkErr := b.networkGone(ctx, id)
	if checkErr != nil {
		// Both, because neither answers on its own: the removal may well have
		// worked, and the read that would have said so is the thing that failed.
		return errors.Join(fmt.Errorf("removing network '%s': %w", id, err), checkErr)
	}
	if gone {
		return nil
	}
	return fmt.Errorf("removing network '%s': %w", id, err)
}

// networkGone reports whether the swarm still lists a network.
//
// Scoped by id rather than listing everything, because this is the network the
// caller already holds — and the result is scanned for that id as well, so a
// daemon or a fake that ignored the filter still gives the right answer rather
// than a confident wrong one.
func (b *Backend) networkGone(ctx context.Context, id string) (bool, error) {
	nets, err := b.api.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("id", id)),
	})
	if err != nil {
		return false, fmt.Errorf("re-checking network '%s': %w", id, err)
	}
	for _, n := range nets {
		if n.ID == id {
			return false, nil
		}
	}
	return true, nil
}

// RemoveConfig deletes one config by id.
//
// Distinct from DeleteConfig, which removes a release record by name on behalf
// of the chart engine. This one is the sweep's, takes an id, and tolerates a
// config that is already gone.
func (b *Backend) RemoveConfig(ctx context.Context, id string) error {
	if err := b.api.ConfigRemove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing config '%s': %w", id, err)
	}
	return nil
}

// RemoveSecret deletes one secret by id.
func (b *Backend) RemoveSecret(ctx context.Context, id string) error {
	if err := b.api.SecretRemove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing secret '%s': %w", id, err)
	}
	return nil
}
