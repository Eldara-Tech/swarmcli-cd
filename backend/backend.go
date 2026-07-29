// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/filters"
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

// WithForbiddenSecrets returns a copy of the backend that refuses to deploy a
// stack mounting any of the named secrets — the controller's own credentials.
// Controller-wide, not per application, but applied through the same
// optional-interface upgrade as WithRegistryAuth so the reconciler need not
// depend on this concrete type.
//
// It is the startup-derived half only. The backend adds what Swarm reports it
// has mounted, and the chart engine's release records, at deploy time — see
// rejectForbiddenMounts. Nothing an operator has to wire up can therefore be the
// difference between the guard being on and off.
func (b *Backend) WithForbiddenSecrets(names map[string]struct{}) charts.Backend {
	c := *b
	c.forbiddenSecrets = names
	return &c
}

// WithOutOfBandNotifier returns a copy of the backend that reports a lost
// compare-and-swap to fn.
//
// It is what makes Options.OnOutOfBandChange reachable at all from a reconcile:
// a backend is built once per swarm, by the swarms registry, which knows
// nothing about applications — and only the caller knows which application's
// sync a losing write belongs to. Applied through the same optional-interface
// upgrade as WithRegistryAuth, for the same reason.
//
// A nil fn leaves the existing notifier in place rather than removing it, so a
// caller that does not care cannot accidentally silence one that does.
func (b *Backend) WithOutOfBandNotifier(fn func(service string)) charts.Backend {
	if fn == nil {
		return b
	}
	c := *b
	c.onOutOfBandChange = fn
	return &c
}

// MountedSecretNames returns the names of the secrets Swarm has mounted into
// this controller, by listing dir (each secret is a file at /run/secrets/<name>).
// That set is exactly what a reconciled stack must not be allowed to mount: the
// admin token, the git token and every registryAuth are the controller's own
// credentials, and a stack mounting one by an `external` reference would read it.
//
// A dir that does not exist yields an empty set and no error: a controller run
// outside a swarm has no mounted secrets to protect, and must still start.
func MountedSecretNames(dir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing mounted secrets in %s: %w", dir, err)
	}
	out := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out[e.Name()] = struct{}{}
		}
	}
	return out, nil
}

// rejectForbiddenMounts refuses a stack that reaches outside itself for one of
// the controller's own secrets or configs, or for the chart engine's release
// history.
//
// Secrets and configs are cluster-global objects resolved by name, so the check
// is a name comparison. Only an `external` reference can ever match: a stack's
// own secrets and configs are namespace-scoped to "<stack>_<name>" by
// conversion, and cannot collide with an unscoped name. That is also what makes
// this cheap — externalRefs is usually empty, and a stack that reaches for
// nothing outside itself costs no lookup at all.
func (b *Backend) rejectForbiddenMounts(ctx context.Context, stack *cdcompose.Stack) error {
	// Read at most once per deploy, and only if some service reaches outside the
	// stack at all. Inside the loop each would be read once per such service,
	// for an answer that cannot differ between them.
	var mine selfMounts
	var history map[string]struct{}
	load := func() error {
		if history != nil {
			return nil
		}
		var err error
		if mine, err = b.mounts(ctx); err != nil {
			return err
		}
		history, err = b.releaseConfigNames(ctx)
		return err
	}

	for _, svc := range stack.Services {
		secrets, configs := externalRefs(stack, svc)
		if len(secrets) == 0 && len(configs) == 0 {
			continue
		}
		if err := load(); err != nil {
			return err
		}

		for _, name := range secrets {
			_, wired := b.forbiddenSecrets[name]
			_, mounted := mine.secrets[name]
			if wired || mounted {
				return fmt.Errorf("service %q mounts secret %q, which is a credential belonging to the "+
					"controller; a reconciled stack may not mount the controller's own secrets", svc.Name, name)
			}
		}
		for _, name := range configs {
			if _, forbidden := mine.configs[name]; forbidden {
				return fmt.Errorf("service %q mounts config %q, which is this controller's own state; "+
					"a reconciled stack may not mount it, because the application set names every "+
					"repository, revision and destination this controller applies", svc.Name, name)
			}
		}

		for _, name := range configs {
			if _, forbidden := history[name]; forbidden {
				return fmt.Errorf("service %q mounts config %q, which is a chart release record; "+
					"those hold the rendered manifest of every release on this swarm, so a reconciled "+
					"stack may not mount one", svc.Name, name)
			}
		}
	}
	return nil
}

// externalRefs names the secrets and configs one service references that the
// stack does not itself declare.
//
// Those are the only references that can reach another tenant's resources or the
// controller's: everything a manifest declares is namespace-scoped on the way in,
// so a reference to a bare name came from an `external:` declaration and resolves
// against the cluster-wide store.
func externalRefs(stack *cdcompose.Stack, svc cdcompose.Service) (secrets, configs []string) {
	cs := svc.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return nil, nil
	}

	declaredSecrets := make(map[string]struct{}, len(stack.Secrets))
	for _, s := range stack.Secrets {
		declaredSecrets[s.Name] = struct{}{}
	}
	for _, ref := range cs.Secrets {
		if _, own := declaredSecrets[ref.SecretName]; !own {
			secrets = append(secrets, ref.SecretName)
		}
	}

	declaredConfigs := make(map[string]struct{}, len(stack.Configs))
	for _, c := range stack.Configs {
		declaredConfigs[c.Name] = struct{}{}
	}
	for _, ref := range cs.Configs {
		if _, own := declaredConfigs[ref.ConfigName]; !own {
			configs = append(configs, ref.ConfigName)
		}
	}
	return secrets, configs
}

// releaseConfigNames names the chart engine's release records.
//
// Matched by the engine's own exported label rather than by the
// "swarmcli.release.<release>.v<n>" name it happens to use, because that format
// is unexported and a rename there would silently stop protecting these. The
// label is part of the contract this repository already reads elsewhere —
// RemoveStack skips these configs by the same one.
func (b *Backend) releaseConfigNames(ctx context.Context) (map[string]struct{}, error) {
	list, err := b.api.ConfigList(ctx, swarm.ConfigListOptions{
		Filters: filters.NewArgs(filters.Arg("label", charts.LabelType+"="+charts.TypeRelease)),
	})
	if err != nil {
		return nil, fmt.Errorf("listing the release records to check a config reference against: %w", err)
	}
	out := make(map[string]struct{}, len(list))
	for _, c := range list {
		out[c.Spec.Name] = struct{}{}
	}
	return out, nil
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
	// Before any resource is created: a stack that reaches for one of the
	// controller's own secrets or configs is refused whole, not half-deployed.
	if err := b.rejectForbiddenMounts(ctx, stack); err != nil {
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
// Removal is idempotent, and it is decided by looking rather than by reading
// the error. A resource that has already gone between the list and the delete
// has reached the state this was asking for; a failure that leaves nothing
// behind is not a failure. That is not a rare race — Swarm garbage-collects an
// overlay network once the last task attached to it goes, which happens while
// the services removed a few lines above are still shutting down, so the
// network this listed is routinely gone before it is asked to remove it.
//
// Classifying the error is not enough on its own. A swarm-scoped network
// removal is proxied through swarmkit and its "already gone" reply does not
// reliably arrive as a not-found the client recognises, so every failed removal
// is followed by a re-check of what is actually left. The state is the answer;
// the error is only a hint about where to look.
//
// This is what makes the call safely repeatable, which prune's retry depends
// on: a pass that failed part-way is followed by another that re-lists and
// re-deletes, and treating the already-deleted half as an error would make that
// retry fail forever.
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

	// Failures are collected rather than returned at the first: the re-check
	// below can only say "nothing is left" if everything was attempted, and one
	// resource that will not go is no reason to abandon the rest of the stack.
	var errs []error

	services, err := b.api.ServiceList(ctx, swarm.ServiceListOptions{Filters: stackFilter(name)})
	if err != nil {
		return fmt.Errorf("listing the stack's services: %w", err)
	}
	for _, s := range services {
		if err := b.api.ServiceRemove(ctx, s.ID); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing service %q: %w", s.Spec.Name, err))
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
		if err := b.api.ConfigRemove(ctx, c.ID); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing config %q: %w", c.Spec.Name, err))
		}
	}

	secrets, err := b.api.SecretList(ctx, swarm.SecretListOptions{Filters: stackFilter(name)})
	if err != nil {
		return fmt.Errorf("listing the stack's secrets: %w", err)
	}
	for _, s := range secrets {
		if err := b.api.SecretRemove(ctx, s.ID); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing secret %q: %w", s.Spec.Name, err))
		}
	}

	networks, err := b.api.NetworkList(ctx, network.ListOptions{Filters: stackFilter(name)})
	if err != nil {
		return fmt.Errorf("listing the stack's networks: %w", err)
	}
	for _, n := range networks {
		if err := b.api.NetworkRemove(ctx, n.ID); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing network %q: %w", n.Name, err))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	// Something refused. Whether that matters is a question about the swarm,
	// not about the error: if nothing carrying this namespace is left, every
	// refusal was a resource that had already gone.
	left, err := b.stackRemains(ctx, name)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if !left {
		return nil
	}
	return errors.Join(errs...)
}

// stackRemains reports whether anything the stack owns is still on the swarm.
//
// Release-history configs do not count. RemoveStack deliberately leaves them —
// that is what keeps a release's history readable after it is uninstalled — so
// counting them would make every removal look like it had failed.
func (b *Backend) stackRemains(ctx context.Context, name string) (bool, error) {
	services, err := b.api.ServiceList(ctx, swarm.ServiceListOptions{Filters: stackFilter(name)})
	if err != nil {
		return false, fmt.Errorf("re-checking the stack's services: %w", err)
	}
	if len(services) > 0 {
		return true, nil
	}

	configs, err := b.api.ConfigList(ctx, swarm.ConfigListOptions{Filters: stackFilter(name)})
	if err != nil {
		return false, fmt.Errorf("re-checking the stack's configs: %w", err)
	}
	for _, c := range configs {
		if c.Spec.Labels[charts.LabelType] != charts.TypeRelease {
			return true, nil
		}
	}

	secrets, err := b.api.SecretList(ctx, swarm.SecretListOptions{Filters: stackFilter(name)})
	if err != nil {
		return false, fmt.Errorf("re-checking the stack's secrets: %w", err)
	}
	if len(secrets) > 0 {
		return true, nil
	}

	networks, err := b.api.NetworkList(ctx, network.ListOptions{Filters: stackFilter(name)})
	if err != nil {
		return false, fmt.Errorf("re-checking the stack's networks: %w", err)
	}
	return len(networks) > 0, nil
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
