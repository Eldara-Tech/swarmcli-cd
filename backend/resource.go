// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"

	"github.com/Eldara-Tech/swarmcli/charts"

	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
)

// defaultNetworkDriver matches what `docker stack deploy` assumes when a
// manifest names no driver.
const defaultNetworkDriver = "overlay"

// stackFilter selects the resources carrying one stack's namespace label. A
// stack is that label plus a name prefix and nothing else — there is no /stacks
// endpoint to ask instead.
func stackFilter(name string) filters.Args {
	return filters.NewArgs(filters.Arg("label", convert.LabelNamespace+"="+name))
}

// applyNetworks creates the stack's networks that do not exist yet.
//
// An existing network is left alone and its differences reported. Swarm cannot
// update a network in place, so the only way to change one is to remove and
// recreate it — which disconnects every attached service, i.e. an outage, for
// what is usually a cosmetic difference. `docker stack deploy` skips silently,
// which swarmcli-cd#1 lists among its defects; skipping is right, being silent
// about it is not.
func (b *Backend) applyNetworks(ctx context.Context, stack *cdcompose.Stack) error {
	existing, err := b.api.NetworkList(ctx, network.ListOptions{Filters: stackFilter(stack.Namespace.Name())})
	if err != nil {
		return fmt.Errorf("listing the stack's networks: %w", err)
	}
	live := make(map[string]network.Summary, len(existing))
	for _, n := range existing {
		live[n.Name] = n
	}

	for _, nw := range stack.Networks {
		opts := nw.Spec
		if opts.Driver == "" {
			opts.Driver = defaultNetworkDriver
		}

		if cur, ok := live[nw.Name]; ok {
			if diff := networkDiff(opts, cur); len(diff) > 0 {
				b.log.Warn("network exists with different options; not recreated, because removing it would disconnect every attached service",
					"network", nw.Name, "differences", diff)
			}
			continue
		}
		if _, err := b.api.NetworkCreate(ctx, nw.Name, opts); err != nil {
			return fmt.Errorf("creating network %q: %w", nw.Name, err)
		}
		b.log.Info("network created", "network", nw.Name, "driver", opts.Driver)
	}
	return nil
}

// networkDiff names the options that differ between what the manifest asks for
// and what is on the swarm.
//
// Only fields the manifest actually sets are compared. The daemon fills in
// plenty the caller never asked for, and reporting those would make the warning
// fire on every reconcile of a perfectly correct network.
func networkDiff(want network.CreateOptions, got network.Summary) []string {
	var diff []string
	if want.Driver != got.Driver {
		diff = append(diff, fmt.Sprintf("driver(want=%s got=%s)", want.Driver, got.Driver))
	}
	if want.Attachable != got.Attachable {
		diff = append(diff, fmt.Sprintf("attachable(want=%t got=%t)", want.Attachable, got.Attachable))
	}
	if want.Internal != got.Internal {
		diff = append(diff, fmt.Sprintf("internal(want=%t got=%t)", want.Internal, got.Internal))
	}
	return diff
}

// applySecrets creates the stack's secrets, refusing a content change.
func (b *Backend) applySecrets(ctx context.Context, secrets []swarm.SecretSpec) error {
	for _, spec := range secrets {
		cur, _, err := b.api.SecretInspectWithRaw(ctx, spec.Name)
		switch {
		case errdefs.IsNotFound(err):
			if _, err := b.api.SecretCreate(ctx, spec); err != nil {
				return fmt.Errorf("creating secret %q: %w", spec.Name, err)
			}
			b.log.Info("secret created", "secret", spec.Name)
		case err != nil:
			return fmt.Errorf("inspecting secret %q: %w", spec.Name, err)
		default:
			// A secret's data is unreadable once stored — GetSecret nils out
			// Spec.Data — so unlike a config there is nothing to compare
			// against. Only the labels can be updated, and that is all this
			// does. Content changes have to arrive as a new name, which is why
			// a chart hashes content into it.
			spec.Data = nil
			if err := b.api.SecretUpdate(ctx, cur.ID, cur.Version, spec); err != nil {
				return fmt.Errorf("updating secret %q: %w", spec.Name, err)
			}
		}
	}
	return nil
}

// applyConfigs creates the stack's configs, refusing a content change.
func (b *Backend) applyConfigs(ctx context.Context, configs []swarm.ConfigSpec) error {
	for _, spec := range configs {
		cur, _, err := b.api.ConfigInspectWithRaw(ctx, spec.Name)
		switch {
		case errdefs.IsNotFound(err):
			if _, err := b.api.ConfigCreate(ctx, spec); err != nil {
				return fmt.Errorf("creating config %q: %w", spec.Name, err)
			}
			b.log.Info("config created", "config", spec.Name)
		case err != nil:
			return fmt.Errorf("inspecting config %q: %w", spec.Name, err)
		case !bytes.Equal(cur.Spec.Data, spec.Data):
			// `docker stack deploy` sends the new data to ConfigUpdate and lets
			// the daemon answer "only updates to Labels are allowed" — an error
			// naming neither the config nor the remedy, arriving partway
			// through a deploy. Say what is wrong and what to do instead.
			return fmt.Errorf("config %q already exists on this swarm with different content. "+
				"Swarm configs are immutable: only labels can be changed. Give the config a name that "+
				"changes with its content — a version suffix or a content hash — so that a new one is "+
				"created and the services referencing it are updated to it", spec.Name)
		default:
			data := spec.Data
			spec.Data = nil
			if err := b.api.ConfigUpdate(ctx, cur.ID, cur.Version, spec); err != nil {
				return fmt.Errorf("updating config %q: %w", spec.Name, err)
			}
			spec.Data = data
		}
	}
	return nil
}

// --- the release engine's own config store ---

// CreateConfig stores one release revision.
//
// The extra swarmcli.created label matches what the CE backend writes, so a
// release recorded by this controller and one recorded from the command line
// look the same to the TUI's config view.
func (b *Backend) CreateConfig(ctx context.Context, name string, data []byte, labels map[string]string) error {
	all := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		all[k] = v
	}
	all["swarmcli.created"] = b.now().UTC().Format(time.RFC3339)

	_, err := b.api.ConfigCreate(ctx, swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: name, Labels: all},
		Data:        data,
	})
	return err
}

// ListConfigs returns every config's name and labels.
//
// One ConfigList call, not a list followed by an inspect per config. The CE
// backend inspects each one because the TUI wants CreatedAt, which ConfigList
// omits — but the engine only reads Name and Labels here, and this runs on
// every reconcile against a store that grows by one config per release
// revision.
func (b *Backend) ListConfigs(ctx context.Context) ([]charts.ConfigMeta, error) {
	configs, err := b.api.ConfigList(ctx, swarm.ConfigListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing configs: %w", err)
	}
	out := make([]charts.ConfigMeta, 0, len(configs))
	for _, c := range configs {
		out = append(out, charts.ConfigMeta{Name: c.Spec.Name, Labels: c.Spec.Labels})
	}
	return out, nil
}

func (b *Backend) InspectConfig(ctx context.Context, name string) ([]byte, error) {
	cfg, _, err := b.api.ConfigInspectWithRaw(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("inspecting config %q: %w", name, err)
	}
	return cfg.Spec.Data, nil
}

func (b *Backend) DeleteConfig(ctx context.Context, name string) error {
	if err := b.api.ConfigRemove(ctx, name); err != nil {
		return fmt.Errorf("removing config %q: %w", name, err)
	}
	return nil
}

// --- pre-flight and cleanup for external resources ---

// StackVolumes names the volumes carrying this stack's namespace label.
func (b *Backend) StackVolumes(ctx context.Context, name string) ([]string, error) {
	resp, err := b.api.VolumeList(ctx, volume.ListOptions{Filters: stackFilter(name)})
	if err != nil {
		return nil, fmt.Errorf("listing the stack's volumes: %w", err)
	}
	out := make([]string, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		out = append(out, v.Name)
	}
	sort.Strings(out)
	return out, nil
}

func (b *Backend) RemoveVolume(ctx context.Context, name string) error {
	if err := b.api.VolumeRemove(ctx, name, false); err != nil {
		return fmt.Errorf("removing volume %q: %w", name, err)
	}
	return nil
}

// NetworkScopes maps every network's name to its scope, for the engine's
// external-network pre-flight.
func (b *Backend) NetworkScopes(ctx context.Context) (map[string]string, error) {
	nets, err := b.api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	scopes := make(map[string]string, len(nets))
	for _, n := range nets {
		scopes[n.Name] = n.Scope
	}
	return scopes, nil
}

func (b *Backend) CreateOverlayNetwork(ctx context.Context, name, driver string, attachable bool) error {
	if driver == "" {
		driver = defaultNetworkDriver
	}
	if _, err := b.api.NetworkCreate(ctx, name, network.CreateOptions{Driver: driver, Attachable: attachable}); err != nil {
		return fmt.Errorf("creating network %q: %w", name, err)
	}
	return nil
}

// RemoveOverlayNetwork removes a network by name, rolling back one this engine
// auto-created for an install whose deploy then failed. A network that is
// already gone is not an error: the caller is undoing, and undoing something
// that did not happen has succeeded.
func (b *Backend) RemoveOverlayNetwork(ctx context.Context, name string) error {
	nets, err := b.api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing networks: %w", err)
	}
	for _, n := range nets {
		if n.Name == name {
			if err := b.api.NetworkRemove(ctx, n.ID); err != nil {
				return fmt.Errorf("removing network %q: %w", name, err)
			}
			return nil
		}
	}
	return nil
}

// SecretNames is the set of existing secret names, for the engine's pre-flight.
// An external secret cannot be auto-created — its content is exactly what the
// manifest does not carry — so this only ever answers "is it there".
func (b *Backend) SecretNames(ctx context.Context) (map[string]struct{}, error) {
	secrets, err := b.api.SecretList(ctx, swarm.SecretListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing secrets: %w", err)
	}
	out := make(map[string]struct{}, len(secrets))
	for _, s := range secrets {
		out[s.Spec.Name] = struct{}{}
	}
	return out, nil
}
