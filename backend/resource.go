// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"bytes"
	"context"
	"fmt"
	"maps"
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

// createdLabel marks a config or secret this controller created, as opposed to
// one it found already there and adopted.
//
// The two are indistinguishable afterwards and the difference decides whether
// the sweep may delete it. applySecrets and applyConfigs below take an existing
// object of the declared name and relabel it into the stack's namespace — the
// same thing `docker stack deploy` does — and that namespace label is exactly
// what makes it a prune candidate. So the pre-seed pattern, an operator running
// `docker secret create web_db_password` before a chart declares
// `db_password`, would end with this controller deleting material it never held
// a copy of, on the strength of a revision that merely *declared* the name.
//
// A stored revision proves the repository asked for a name. This proves this
// controller made the object. LiveConfigs and LiveSecrets require both, because
// neither on its own is ownership.
//
// **Two kinds and not three, because only two of them adopt.** applyNetworks
// leaves an existing network entirely alone — it does not relabel one into the
// stack's namespace — so a network can only be a candidate if it already
// carried this stack's namespace label, which means either this controller
// created it or somebody deliberately labelled it as this release's. There is
// no adoption to catch, and requiring a marker there would instead stop the
// sweep touching every network created before this label existed, silently.
//
// Written only on creation, and carried across an update rather than
// re-derived: a config or secret update replaces the whole annotation set, so
// an object this controller did create would otherwise lose its marker on the
// next reconcile. An object that has never carried one never gains one, which
// is the deliberate part — an adopted object and one created by an older build
// of this controller read identically from here, and what they have in common
// is that ownership cannot be proved. Both are reported and neither is deleted,
// the same answer Claim gives a candidate older than the retained history.
const createdLabel = "com.swarmcli.cd.created"

// stampCreated returns labels plus the creation marker.
//
// A copy, never a mutation: the map belongs to the converted stack, which is
// read again after this — DeployStack applies the unresolved conversion and
// then converts a second time — and a label written into it here would travel
// into specs this never saw.
func (b *Backend) stampCreated(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	maps.Copy(out, labels)
	out[createdLabel] = b.now().UTC().Format(time.RFC3339)
	return out
}

// keepCreated carries an existing creation marker onto the labels that are
// about to replace it, and leaves them alone when there is none to carry.
func keepCreated(labels, current map[string]string) map[string]string {
	v, ok := current[createdLabel]
	if !ok {
		return labels
	}
	out := make(map[string]string, len(labels)+1)
	maps.Copy(out, labels)
	out[createdLabel] = v
	return out
}

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
			spec.Labels = b.stampCreated(spec.Labels)
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
			//
			// This is also the adoption path: a secret of the declared name that
			// somebody else created is relabelled into the stack's namespace
			// here. It does not gain a creation marker, and that is what keeps
			// the sweep off it.
			spec.Data = nil
			spec.Labels = keepCreated(spec.Labels, cur.Spec.Labels)
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
			spec.Labels = b.stampCreated(spec.Labels)
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
			// The adoption path, as in applySecrets: a config of the declared
			// name that already carries exactly this content is relabelled into
			// the stack's namespace rather than refused. It does not gain a
			// creation marker either.
			data := spec.Data
			spec.Data = nil
			spec.Labels = keepCreated(spec.Labels, cur.Spec.Labels)
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

// ListConfigs returns every config's name and labels, and the payload of the
// ones holding release history.
//
// One ConfigList call and nothing else. A config's payload comes back in the
// list response, not only on inspect, so passing it through means the engine
// decodes release history straight from this call instead of inspecting each
// stored revision (Eldara-Tech/swarmcli#510). That matters here more than
// anywhere: this runs several times per reconcile per application, against a
// store that grows by one config per release revision, and a controller
// reconciles on a timer whether or not anything changed.
//
// Deliberately unfiltered, and that is a conclusion rather than an omission. The
// engine asks this one method two different questions — which configs hold
// release history, and which config names exist at all — and the second is asked
// about a chart's external configs, which carry no swarmcli label. Filtering on
// the release label server-side would answer the first cheaply and make the
// second report every external config as absent, which is a hard error refusing
// the deploy. Nor is there a projection to ask for instead: GET /configs is
// converted by the same daemon function as GET /configs/{id}, so a name cannot
// be fetched without its payload — unlike a secret, whose payload the list
// deliberately withholds — and the accepted filters are id, name, names and
// label, with no negation to list "everything else" with.
//
// What can be bounded is what is *kept*. Only a release record is ever decoded —
// ConfigMeta.Data is documented as optional, and allRevisions skips anything not
// carrying the release label before it looks — so every other config contributes
// its name and labels and its payload is dropped here rather than held for the
// length of a plan. On a swarm whose stacks mount configs of their own that is
// the difference between retaining the release store and retaining the whole
// config store, on the manager node holding the raft log.
func (b *Backend) ListConfigs(ctx context.Context) ([]charts.ConfigMeta, error) {
	configs, err := b.api.ConfigList(ctx, swarm.ConfigListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing configs: %w", err)
	}
	out := make([]charts.ConfigMeta, 0, len(configs))
	for _, c := range configs {
		meta := charts.ConfigMeta{Name: c.Spec.Name, Labels: c.Spec.Labels}
		if c.Spec.Labels[charts.LabelType] == charts.TypeRelease {
			meta.Data = c.Spec.Data
		}
		out = append(out, meta)
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

// StackVolumes names the volumes carrying this stack's namespace label **on the
// node this controller talks to**, which on a swarm of more than one node is
// not the same question as "this stack's volumes".
//
// GET /volumes answers from the node-local volume store
// (api/server/router/volume/volume_routes.go:35 →
// volume/service/service.go:265): the daemon lists what this engine has, and
// only CSI *cluster* volumes are added from swarm, and only on a manager
// (volume_routes.go:41). An ordinary named volume is created by the engine that
// runs the task that mounts it, so a stack's volumes live on whichever nodes
// scheduled its tasks and are invisible from everywhere else.
//
// There is no swarm-wide named-volume listing to ask instead, and swarms.Registry
// resolves one swarm rather than one node, so this is the whole of what the OSS
// build can see. SwarmNodes below is how a caller finds out whether that is
// everything.
//
// Guarded like RemoveStack, and not only for symmetry: this is the list a purge
// deletes from, and the chart engine's own Uninstall reaches it even when the
// stack removal before it failed — it collects that error and carries on. A
// release named for the controller's stack would name the volume holding every
// application's git clone and chart cache, so the refusal has to be on the read
// that produces the list rather than only on the removal that precedes it (#102).
func (b *Backend) StackVolumes(ctx context.Context, name string) ([]string, error) {
	if err := b.rejectOwnNamespace(ctx, name); err != nil {
		return nil, err
	}
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

// SwarmNodes counts the swarm's nodes, which is the only thing that says
// whether StackVolumes could have seen everything: on a single-node swarm the
// node-local volume store *is* the swarm's, and on any larger one it is a
// fraction of unknown size.
//
// A node that is not a manager cannot list nodes, and that failure is not
// smoothed over here — the caller decides what an unanswerable question means,
// and for a deletion it has to mean "assume not".
func (b *Backend) SwarmNodes(ctx context.Context) (int, error) {
	nodes, err := b.api.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return 0, fmt.Errorf("listing the swarm's nodes: %w", err)
	}
	return len(nodes), nil
}

// RemoveVolume deletes one volume by name.
//
// A volume that is already gone is not an error, as it is not for the four
// removals in live.go. The caller retries this for as long as the volume is
// still held by a container that has not finished shutting down, so a volume
// that vanished in between — another sweep, an operator, a `docker volume
// prune` — would otherwise burn the whole settle budget and then fail the prune
// naming "still in use" for something that had reached the asked-for state on
// the first attempt.
//
// The classification is reliable here in a way it is not for a network: a
// missing volume is answered by the local volume store, or on a manager by
// getVolume, and both wrap in a not-found the client recognises
// (daemon/cluster/helpers.go:274).
func (b *Backend) RemoveVolume(ctx context.Context, name string) error {
	if err := b.api.VolumeRemove(ctx, name, false); err != nil && !errdefs.IsNotFound(err) {
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
