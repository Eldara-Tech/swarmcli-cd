// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package backend applies compose-derived Swarm specs to a swarm through the
// moby client. It is the half of charts.Backend that needs a daemon; turning a
// manifest into those specs is package compose.
package backend

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
)

// Image resolution modes, matching charts.InstallOptions.ResolveImage and the
// daemon's own query parameter.
const (
	// ResolveAlways asks the registry to resolve the tag to a digest on every
	// deploy. Docker's default.
	ResolveAlways = "always"
	// ResolveChanged resolves only when the manifest names a different image
	// than the last deploy did, which is what suits automation: an unchanged
	// tag does not become a redeploy just because the registry moved.
	ResolveChanged = "changed"
	// ResolveNever leaves the tag as written.
	ResolveNever = "never"
)

// maxConflictRetries bounds the read-modify-write loop. Swarm makes ?version=
// mandatory on every mutation, so a losing race is re-read and re-applied — but
// a service being rewritten faster than we can read it is a condition to report,
// not to spin on.
const maxConflictRetries = 3

// Backend applies specs to one swarm.
type Backend struct {
	api client.APIClient
	log *slog.Logger
	// onOutOfBandChange is called with a service name each time a mutation
	// loses the compare-and-swap, meaning something else changed that service
	// between the read and the write.
	//
	// It is a callback rather than a direct notify.Dispatch because a backend is
	// scoped to a swarm and a notification is scoped to an application: only the
	// caller knows which application's reconcile this write belongs to.
	onOutOfBandChange func(service string)
	// now stamps the swarmcli.created label; overridable in tests.
	now func() time.Time
}

// Options tune a Backend. Every field has a working default.
type Options struct {
	Log               *slog.Logger
	OnOutOfBandChange func(service string)
	Now               func() time.Time
}

// New returns a Backend applying to the swarm the client is connected to.
func New(api client.APIClient, o Options) *Backend {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.OnOutOfBandChange == nil {
		o.OnOutOfBandChange = func(string) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Backend{api: api, log: o.Log, onOutOfBandChange: o.OnOutOfBandChange, now: o.Now}
}

// ApplyServices creates the stack's services that do not exist and updates
// those that do, in the order the stack lists them.
//
// It deletes nothing. Phase 1 is explicitly no prune, and charts.Apply itself
// never deletes either — a service the manifest no longer declares is left
// alone, not reaped.
//
// It also cannot detect an out-of-band change. Swarm has no server-side apply,
// so `docker service update --replicas 10` produces no conflict signal at all:
// the next reconcile simply computes the same desired spec and writes it back,
// silently. The only conflict this can see is a write that races one of ours.
func (b *Backend) ApplyServices(ctx context.Context, stack *cdcompose.Stack, resolve string) error {
	existing, err := b.stackServices(ctx, stack.Namespace.Name())
	if err != nil {
		return err
	}

	for _, svc := range stack.Services {
		name := stack.Namespace.Scope(svc.Name)
		cur, ok := existing[name]
		if !ok {
			if err := b.createService(ctx, name, svc.Spec, resolve); err != nil {
				return err
			}
			continue
		}
		if err := b.updateService(ctx, name, cur, svc.Spec, resolve); err != nil {
			return err
		}
	}
	return nil
}

// stackServices returns the swarm's services for one namespace, by name.
//
// A stack is a name prefix plus this label and nothing more: there is no
// /stacks endpoint, no server-side desired state and no owner references, so
// this filter is the whole of "which services belong to this stack".
func (b *Backend) stackServices(ctx context.Context, namespace string) (map[string]swarm.Service, error) {
	services, err := b.api.ServiceList(ctx, swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", convert.LabelNamespace+"="+namespace)),
	})
	if err != nil {
		return nil, fmt.Errorf("listing the stack's services: %w", err)
	}
	out := make(map[string]swarm.Service, len(services))
	for _, s := range services {
		out[s.Spec.Name] = s
	}
	return out, nil
}

func (b *Backend) createService(ctx context.Context, name string, spec swarm.ServiceSpec, resolve string) error {
	resp, err := b.api.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{
		QueryRegistry: resolve == ResolveAlways || resolve == ResolveChanged,
	})
	if err != nil {
		return fmt.Errorf("creating service %q: %w", name, err)
	}
	b.log.Info("service created", "service", name, "id", resp.ID)
	b.warn(name, resp.Warnings)
	return nil
}

// updateService writes the desired spec over an existing service, re-reading
// and retrying when it loses the compare-and-swap.
//
// Retrying is right for a controller even though the conflict is a real signal:
// the desired spec is complete, so re-applying it *is* correcting drift, which
// is the job. Failing instead would turn a routine race — an operator scaling a
// service, two reconciles overlapping — into a failed sync that the next tick
// would have fixed anyway. What must not happen is the overwrite being silent,
// which is what onOutOfBandChange is for.
func (b *Backend) updateService(ctx context.Context, name string, cur swarm.Service, spec swarm.ServiceSpec, resolve string) error {
	for attempt := range maxConflictRetries {
		desired, opts := prepareUpdate(cur, spec, resolve)

		resp, err := b.api.ServiceUpdate(ctx, cur.ID, cur.Version, desired, opts)
		if err == nil {
			b.log.Info("service updated", "service", name, "id", cur.ID)
			b.warn(name, resp.Warnings)
			return nil
		}
		if !isVersionConflict(err) {
			return fmt.Errorf("updating service %q: %w", name, err)
		}

		b.log.Warn("service changed between read and write; re-reading and re-applying",
			"service", name, "attempt", attempt+1)
		b.onOutOfBandChange(name)

		fresh, _, ierr := b.api.ServiceInspectWithRaw(ctx, cur.ID, swarm.ServiceInspectOptions{})
		if ierr != nil {
			return fmt.Errorf("re-reading service %q after a version conflict: %w", name, ierr)
		}
		cur = fresh
	}
	return fmt.Errorf("service %q changed underneath us %d times; giving up so the next reconcile can plan "+
		"against whatever it settles on", name, maxConflictRetries)
}

// prepareUpdate applies the adjustments an update needs that a create does not.
// Each exists because leaving it out causes a spurious redeploy, and redeploying
// a healthy service is a real outage risk rather than a cosmetic problem.
func prepareUpdate(cur swarm.Service, spec swarm.ServiceSpec, resolve string) (swarm.ServiceSpec, swarm.ServiceUpdateOptions) {
	var opts swarm.ServiceUpdateOptions

	// The image the manifest asked for last time, before the daemon resolved it
	// to a digest. Comparing against the live spec's image would compare a tag
	// with a digest and never match.
	deployed := cur.Spec.Labels[convert.LabelImage]
	wanted := spec.TaskTemplate.ContainerSpec.Image

	switch {
	case resolve == ResolveAlways:
		opts.QueryRegistry = true
	case resolve == ResolveChanged && wanted != deployed:
		opts.QueryRegistry = true
	case wanted == deployed:
		// Same tag as last time, so keep the digest the daemon resolved it to.
		// Writing the bare tag back would differ from the live spec and
		// redeploy every task for no reason.
		spec.TaskTemplate.ContainerSpec.Image = cur.Spec.TaskTemplate.ContainerSpec.Image
	}

	// There is no --force here, and there should not be: carrying the existing
	// counter forward keeps an update that changes nothing from restarting
	// tasks.
	spec.TaskTemplate.ForceUpdate = cur.Spec.TaskTemplate.ForceUpdate

	return spec, opts
}

// isVersionConflict reports the "someone else wrote this first" failure.
//
// It has to match the message. Swarmkit returns ErrSequenceConflict as gRPC
// InvalidArgument, which the daemon renders as 400 Bad Request — not 409 — so
// errdefs.IsConflict does not see it. Docker's own integration helpers match
// the same string for the same reason. The errdefs check is kept first so that
// a daemon which starts classifying it properly is handled without a change
// here.
func isVersionConflict(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsConflict(err) {
		return true
	}
	return strings.Contains(err.Error(), "update out of sequence")
}

func (b *Backend) warn(service string, warnings []string) {
	for _, w := range warnings {
		b.log.Warn("swarm warning", "service", service, "warning", w)
	}
}
