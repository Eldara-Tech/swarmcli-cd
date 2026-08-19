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
	"sort"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
	"github.com/Eldara-Tech/swarmcli-cd/regauth"
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
	// registryAuth resolves the encoded credential for a service's image, or is
	// nil when the application deploying through this backend declared none, in
	// which case pulls are anonymous. It is set per application by
	// WithRegistryAuth, never shared: a swarm's backend is reused across
	// applications, and one application must not pull with another's credential.
	registryAuth regauth.Resolver
	// forbiddenSecrets names the secrets mounted into this controller — its own
	// credentials — that a reconciled stack must not mount. Swarm secrets are
	// cluster-global and referenced by name, so without this a chart declaring
	// one of these as an `external` secret would read the controller's admin
	// token, git token or another application's registry credential. Set
	// controller-wide by WithForbiddenSecrets; empty disables the check.
	forbiddenSecrets map[string]struct{}
	// allow is what the application deploying through this backend may reach
	// outside the releases it installs: host paths on the nodes, and the names of
	// secrets, configs, volumes and networks some other stack owns. It is an
	// allowlist, so the zero value permits nothing beyond what a release owns —
	// which is what a backend nobody scoped to an application enforces, and the
	// safe direction for one. Set per application by WithAllowedReferences.
	allow application.Allow
	// selfRelease marks this copy as deploying the stack the controller itself
	// runs as, and holdSelf is where the write that replaces it is handed back.
	// Set together by WithSelfRelease, which refuses to set either alone — the
	// exemptions a self release gets are only safe because that write is issued
	// last, so a copy that could not hand it back must not have them.
	selfRelease bool
	holdSelf    capability.DeferSelf
	// self is what Swarm reports it has mounted into this controller, read from
	// its own service spec on the first deploy that needs it. It covers the
	// controller's configs, which have no /run/secrets equivalent to list, and
	// corrects its secrets, which that listing names by mount target rather than
	// by the name a reference uses. Shared by every With* copy.
	self *selfMountCache
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
	return &Backend{api: api, log: o.Log, onOutOfBandChange: o.OnOutOfBandChange, now: o.Now,
		self: &selfMountCache{}}
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
	if err := b.rejectForeignNamespace(ctx, stack.Namespace.Name(), existing); err != nil {
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

// rejectForeignNamespace refuses to write into a stack namespace that already
// has services and that this engine has no record of installing.
//
// The rule the sweep applies before it deletes a service is that the label says
// *where* a resource lives and the record says *whose* it is (#62, one scope
// down). The write path had no such rule at all: the services to update were
// whatever carried the namespace label, and a spec was written over each of them
// unread. Anything that can name a release can therefore name a stack — the
// namespace is a label anyone can carry, and a release name is chosen by whoever
// writes the release file — and the chart's spec lands on somebody else's
// services, with that chart's image, mounts and placement (#102).
//
// The record is the second signal here as it is there. A release the engine has
// installed has at least one release-history config, written by this controller
// through CreateConfig; a stack put on the swarm by `docker stack deploy`, by
// another tool, or by hand has none, whatever labels its services carry. So:
//
//   - nothing running under the namespace — an install into empty space — is not
//     a claim on anything and needs no record. This is the ordinary first deploy.
//   - services running and a record of ours: the release is one the engine
//     installed, and updating its services is what a sync is.
//   - services running and no record: they are not ours to write over, and the
//     deploy is refused before a single service is created or updated.
//
// The third case refuses two things that used to go through, and the second is
// the one worth stating. It refuses adopting a hand-deployed stack by naming a
// release after it — which was never a decision anybody made, it was the absence
// of this check, and the release would then also be the stack that a later
// uninstall or prune deletes. And it refuses installing *alongside* one, with no
// name collision at all, because a namespace is the unit RemoveStack and every
// sweep act on: sharing one with a stack this engine did not install means the
// first uninstall takes both. It also closes the two-commit version of the
// takeover, where a release is first installed into a foreign namespace under
// harmless service names and only then grows a service named like one already
// there.
//
// An operator who really means to bring an existing stack under GitOps removes
// it first and lets the controller install it — which is a decision they make,
// visible in what the deploy does, rather than a spec silently overwritten.
func (b *Backend) rejectForeignNamespace(ctx context.Context, namespace string, existing map[string]swarm.Service) error {
	if len(existing) == 0 {
		return nil
	}
	ours, err := b.releaseRecorded(ctx, namespace)
	if err != nil {
		return err
	}
	if ours {
		return nil
	}
	names := make([]string, 0, len(existing))
	for name := range existing {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("refusing to deploy release '%s': %s already run under its stack namespace and this "+
		"controller has no release record for it, so they were deployed by something else — a `docker stack "+
		"deploy`, another tool, or the controller's own stack — and this release would write its services "+
		"over them. Give the release a name of its own, or remove that stack first if it is meant to be "+
		"managed from here", namespace, strings.Join(names, ", "))
}

// releaseRecorded reports whether the chart engine holds a release record for
// this release — the proof that the stack under its namespace is one this
// controller deployed.
//
// Matched by the engine's exported labels rather than by the config name, for the
// reason releaseConfigNames gives: that name format is unexported, and a rename
// there would silently stop this proving anything.
//
// A record carrying a stack namespace is not a record. A chart can put any
// labels it likes on a config it declares, so these labels are forgeable — but a
// config created by a stack deploy is stamped with that stack's namespace on the
// way in (convert.AddStackLabel, which writes the label last and so cannot be
// talked out of it), while a genuine record is written through CreateConfig with
// com.swarmcli.* labels and no namespace at all. That absence is already
// load-bearing — it is what keeps RemoveStack from deleting a release's history —
// and it is what tells a record from a chart claiming to be one.
//
// Any owner counts, including none. The stamp answers a different question than
// this does: which of this controller's applications installed a release, so that
// a sweep does not delete another controller's work. Requiring it here would
// refuse the two handovers the engine and the docs describe as supported — a
// release installed from the command line and then adopted by an application, and
// every release on the swarm after --controller-id is changed, which is corrected
// precisely by redeploying each one once.
func (b *Backend) releaseRecorded(ctx context.Context, release string) (bool, error) {
	list, err := b.api.ConfigList(ctx, swarm.ConfigListOptions{
		Filters: filters.NewArgs(filters.Arg("label", charts.LabelRelease+"="+release)),
	})
	if err != nil {
		return false, fmt.Errorf("listing release '%s''s records to check whether this controller installed it: %w",
			release, err)
	}
	for _, c := range list {
		if c.Spec.Labels[charts.LabelType] != charts.TypeRelease {
			continue
		}
		if _, stacked := c.Spec.Labels[convert.LabelNamespace]; stacked {
			continue
		}
		return true, nil
	}
	return false, nil
}

func (b *Backend) createService(ctx context.Context, name string, spec swarm.ServiceSpec, resolve string) error {
	auth, err := b.encodedAuth(spec)
	if err != nil {
		return fmt.Errorf("creating service '%s': %w", name, err)
	}
	resp, err := b.api.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{
		QueryRegistry:       resolve == ResolveAlways || resolve == ResolveChanged,
		EncodedRegistryAuth: auth,
	})
	if err != nil {
		return fmt.Errorf("creating service '%s': %w", name, err)
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

		auth, err := b.encodedAuth(desired)
		if err != nil {
			return fmt.Errorf("updating service '%s': %w", name, err)
		}
		opts.EncodedRegistryAuth = auth

		resp, err := b.api.ServiceUpdate(ctx, cur.ID, cur.Version, desired, opts)
		if err == nil {
			b.log.Info("service updated", "service", name, "id", cur.ID)
			b.warn(name, resp.Warnings)
			return nil
		}
		if !isVersionConflict(err) {
			return fmt.Errorf("updating service '%s': %w", name, err)
		}

		b.log.Warn("service changed between read and write; re-reading and re-applying",
			"service", name, "attempt", attempt+1)
		b.onOutOfBandChange(name)

		fresh, _, ierr := b.api.ServiceInspectWithRaw(ctx, cur.ID, swarm.ServiceInspectOptions{})
		if ierr != nil {
			return fmt.Errorf("re-reading service '%s' after a version conflict: %w", name, ierr)
		}
		cur = fresh
	}
	return fmt.Errorf("service '%s' changed underneath us %d times; giving up so the next reconcile can plan "+
		"against whatever it settles on", name, maxConflictRetries)
}

// prepareUpdate applies the adjustments an update needs that a create does not.
// Each exists because leaving it out causes a spurious redeploy, and redeploying
// a healthy service is a real outage risk rather than a cosmetic problem.
func prepareUpdate(cur swarm.Service, spec swarm.ServiceSpec, resolve string) (swarm.ServiceSpec, swarm.ServiceUpdateOptions) {
	var opts swarm.ServiceUpdateOptions

	// The image the manifest asked for last time, exactly as it was written:
	// convert.Service copies the manifest's own string into the label and neither
	// tags nor familiarises it. The live spec's image has had both done to it —
	// the client's, on the way out, and the daemon's resolved digest on top — so
	// the two are only comparable through cdcompose.SameImage below.
	deployed := cur.Spec.Labels[convert.LabelImage]

	// Neither container spec is guaranteed. A swarm.TaskSpec carries a
	// PluginSpec and a NetworkAttachmentSpec alongside it, and only one of the
	// three is ever set — so a service running a different runtime has none.
	// That matters most for the live side: cur is whatever the daemon returned
	// for a service carrying this stack's namespace label, and anything that can
	// reach the socket this controller itself holds can create one under that
	// name with a non-container runtime. Dereferencing it took the whole process
	// down, since nothing here recovers.
	//
	// Nil means there is no image to reason about, so the reasoning below is
	// skipped and the desired spec is written as it stands — the same "flatten it
	// and carry on" drift.containerSpec takes. Refusing instead would be this
	// applier inventing a verdict about a runtime it does not model; letting the
	// update go leaves the daemon to accept or reject the change and to say why,
	// which is the answer an operator can act on. encodedAuth further down
	// guards the same expression for the same reason.
	want, live := spec.TaskTemplate.ContainerSpec, cur.Spec.TaskTemplate.ContainerSpec
	var wanted string
	if want != nil {
		wanted = want.Image
	}

	switch {
	case resolve == ResolveAlways:
		opts.QueryRegistry = true
	case resolve == ResolveChanged && wanted != deployed:
		opts.QueryRegistry = true
	case want != nil && live != nil && wanted == deployed && cdcompose.SameImage(live.Image, deployed):
		// Same tag as last time, so keep the digest the daemon resolved it to.
		// Writing the bare tag back would differ from the live spec and
		// redeploy every task for no reason.
		//
		// SameImage is what makes that reasoning true. Keeping the live image
		// is only right while it *is* the digest our tag resolved to, and an
		// out-of-band `docker service update --image` breaks that: it rewrites
		// ContainerSpec.Image and leaves the stack label alone, so the label
		// still names our tag and `wanted == deployed` still holds. Without the
		// check, correcting an image someone changed by hand would write their
		// image straight back — the one kind of drift a converge would silently
		// fail to undo.
		want.Image = live.Image
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

// encodedAuth resolves the credential for a service's image to the header value
// ServiceCreate and ServiceUpdate carry. A nil resolver — the application
// declared no registryAuth — sends nothing, which is an anonymous pull and the
// behaviour before this backend authenticated at all.
//
// It is set on both create and update because the credential is needed in two
// places: the manager contacts the registry to resolve a tag to a digest when
// QueryRegistry is set, and each node contacts it to pull. Sending it when
// neither happens is harmless — the daemon ignores a credential it does not use.
func (b *Backend) encodedAuth(spec swarm.ServiceSpec) (string, error) {
	if b.registryAuth == nil || spec.TaskTemplate.ContainerSpec == nil {
		return "", nil
	}
	return b.registryAuth(spec.TaskTemplate.ContainerSpec.Image)
}

func (b *Backend) warn(service string, warnings []string) {
	for _, w := range warnings {
		b.log.Warn("swarm warning", "service", service, "warning", w)
	}
}
