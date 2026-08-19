// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/charts"
	"github.com/Eldara-Tech/swarmcli/docker"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
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

// And every optional capability its callers look for, asserted here rather than
// discovered at run time.
//
// Each one is reached through a type assertion that falls back silently when it
// fails, so a method whose signature drifted would not break a build: it would
// quietly turn a feature off — no live drift, no sweep, no per-application
// credential — and the first evidence would be a report that had stopped saying
// anything. That is the failure package capability exists to spare a companion
// backend, and this one is written against the same names on the same terms.
var (
	_ capability.RegistryAuth        = (*Backend)(nil)
	_ capability.ForbidSecrets       = (*Backend)(nil)
	_ capability.AllowedReferences   = (*Backend)(nil)
	_ capability.SelfRelease         = (*Backend)(nil)
	_ capability.OutOfBand           = (*Backend)(nil)
	_ capability.StackServicesReader = (*Backend)(nil)
	_ capability.StacksReader        = (*Backend)(nil)
	_ capability.LiveDrift           = (*Backend)(nil)
	_ capability.NetworkNamer        = (*Backend)(nil)
	_ capability.DeclaredLister      = (*Backend)(nil)
	_ capability.ResourceLister      = (*Backend)(nil)
	_ capability.ResourceRemover     = (*Backend)(nil)
	_ capability.SwarmSizer          = (*Backend)(nil)
)

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
// rejectForbiddenResources. Nothing an operator has to wire up can therefore be
// the difference between the guard being on and off.
func (b *Backend) WithForbiddenSecrets(names map[string]struct{}) charts.Backend {
	c := *b
	c.forbiddenSecrets = names
	return &c
}

// WithAllowedReferences returns a copy of the backend that permits one
// application's charts to reach what allow enumerates — host paths on the nodes,
// and the secrets, configs, volumes and networks of some other stack.
//
// Per application, unlike WithForbiddenSecrets, and applied through the same
// optional-interface upgrade as WithRegistryAuth so the reconciler need not
// depend on this concrete type. It is the only thing that carries an
// application's identity into this package: the guards here are name
// comparisons, and until now every set they compared against was the
// controller's own, which needed no caller to say who was deploying.
//
// A backend nobody scoped this way permits nothing beyond what the release being
// deployed owns. That is the safe direction and it is what the sweep in package
// prune resolves for itself — a sweep removes rather than deploys, so it reaches
// neither this value's readers, but if it ever did it would refuse rather than
// wave something through.
func (b *Backend) WithAllowedReferences(allow application.Allow) charts.Backend {
	c := *b
	c.allow = allow
	return &c
}

// WithSelfRelease returns a copy of the backend that deploys the stack this
// controller itself runs as, handing hold the one write that would replace it.
//
// The copy is what carries the answer to "is this release ours", and it is a
// copy for the same reason WithAllowedReferences is: a backend is built once per
// swarm and reused by every application, so a flag set on the shared one would
// make the next application's deploy the controller's own.
//
// A nil hold returns the backend unchanged rather than a self copy with nowhere
// to put the write. The exemptions a self release gets are only defensible
// because the write replacing this controller is issued last, after the pass has
// been recorded; a copy that could not hand it back would deploy everything
// except the controller, report a successful sync, and have upgraded nothing.
// Refusing to be self is the failure that says so.
func (b *Backend) WithSelfRelease(hold capability.DeferSelf) charts.Backend {
	if hold == nil {
		return b
	}
	c := *b
	c.selfRelease, c.holdSelf = true, hold
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

// What a reconciled stack may not touch, and why — the same answers whether it
// reached for the name or claimed it.
const (
	whatControllerSecret = "a credential belonging to the controller"
	whatControllerConfig = "this controller's own state; the application set names every " +
		"repository, revision and destination this controller applies"
	whatReleaseRecord = "a chart release record; those hold the rendered manifest of every " +
		"release on this swarm"
	whatControllerVolume = "this controller's own volume; it holds every application's git clone " +
		"and chart cache, so reading it is every watched repository's content and writing it is " +
		"what the next reconcile deploys under another application's name"
)

// rejectForbiddenResources refuses a stack that reaches outside itself for one of
// the controller's own secrets, configs, volumes or networks, for the chart
// engine's release history, or for anything else its application was not
// permitted — and a stack that declares one of those names as its own.
//
// Two rules in one pass, in an order that is the whole of what they mean.
//
// The controller's own is refused first, and no allowlist reaches it: permitting
// one is not an operator lending an application something of the operator's, it
// is handing whoever writes the chart the controller's credentials and with them
// the app set (see application.Allow). Everything else outside the release is
// refused unless the application's allowlist names it, which is the
// per-application half — #64.
//
// One release is not refused, and it is not an allowlist that spares it. A
// release the app set marked `self: true` is the stack this controller runs as,
// so what it mounts of the controller's it does not reach outside itself for at
// all — the release namespace is the controller's namespace, and the only reason
// scopedUnder cannot see it is that a Swarm secret, config or volume carries no
// namespace prefix on a cluster-global name. ownMount is that recognition, and
// its three limits are what keep it from being a hole: it is the referenced half
// only, it reads the set off the controller's own service spec rather than
// /run/secrets, and the release records are refused before it is consulted. The
// name of that release is not the app set's word either — rejectSelfMismatch has
// already established it is this controller's stack (#235). A name scoped to the release being
// deployed is the release's own and asks nobody: conversion produces
// "<release>_<name>" for everything a stack owns.
//
// The direction matters more than the sets do. It is an allowlist because the
// other way round leaves whatever nobody thought of permitted, and the family
// this guard grew out of — an external secret (#63), a renamed declaration (#86),
// a release record, a volume, a network, a socket bind (#103) — was exactly that
// in each case until somebody found it.
//
// Secrets, configs and volumes are cluster-global objects addressed by name, so
// each check is a name comparison. The declared half is not redundant, and #86 is
// the proof: a manifest can put any name it likes on a resource it declares,
//
//	secrets:
//	  x:
//	    name: swarmcli-cd-token
//	    driver: vault
//
// and conversion then resolves both the declaration and the reference to that
// name. So the stack's own set contains it, externalRefs correctly reports the
// reference as not reaching outside the stack — and applySecrets adopts the
// controller's real secret, because a stored secret's data cannot be read back
// and there is nothing to compare. The service is handed the real credential, and
// the secret is relabelled into the stack's namespace, where a later RemoveStack
// deletes it. Declaring a name is not the same as owning it.
//
// A volume needs no second pass for that, and a network needs no first one.
// Nothing pre-creates a volume — DeployStack says why — so a top-level `volumes:`
// entry does nothing at all until a service mounts it, and conversion has already
// folded `external: true`, a bare `name:` and plain namespace scoping into the one
// name the mount carries (convert.Volumes). A network is the mirror image: a
// service's attachment is only legible from the stack's network list, and both
// halves of that list are names a manifest chose, so both are compared.
//
// The cost is one listing of the release records per deploy, and only for a stack
// that references or declares a config at all. What this controller has mounted
// is read unconditionally and costs nothing extra: DeployStack has already made
// that read before reaching here, because rejectOwnNamespace compares the release
// name against the same answer, and it is memoised for the life of the process.
func (b *Backend) rejectForbiddenResources(ctx context.Context, stack *cdcompose.Stack) error {
	mine, err := b.mounts(ctx)
	if err != nil {
		return err
	}

	// The release records are the read worth conditioning on: a fresh listing of a
	// set that grows by one config per deploy, where the answer above is a cached
	// pair of round trips. Memoised for this call, because inside the loop it
	// would otherwise be read once per config reference.
	var history map[string]struct{}
	records := func() (map[string]struct{}, error) {
		if history == nil {
			var err error
			if history, err = b.releaseConfigNames(ctx); err != nil {
				return nil, err
			}
		}
		return history, nil
	}

	// The release's own namespace, which is what "outside the stack" is measured
	// against below.
	ns := stack.Namespace.Name()

	// What a service reaches outside the stack for.
	for _, svc := range stack.Services {
		secrets, configs := externalRefs(stack, svc)
		for _, name := range secrets {
			if b.ownMount(mine.secrets, name) {
				continue
			}
			_, wired := b.forbiddenSecrets[name]
			_, mounted := mine.secrets[name]
			if wired || mounted {
				return mountsForbidden(svc.Name, "secret", name, whatControllerSecret)
			}
			if !scopedUnder(ns, name) && !permits(b.allow.Secrets, name) {
				return mountsUnpermitted(svc.Name, "secret", name, "allow.secrets")
			}
		}
		for _, name := range configs {
			// The release records are checked before anything is recognised as
			// ours, because they are the one thing on this list that no release
			// owns — this controller's own included. Nothing in the chart that
			// deploys this controller mounts one, and a manifest that wants to is
			// not a self-update.
			known, err := records()
			if err != nil {
				return err
			}
			if _, forbidden := known[name]; forbidden {
				return mountsForbidden(svc.Name, "config", name, whatReleaseRecord)
			}
			if b.ownMount(mine.configs, name) {
				continue
			}
			if _, forbidden := mine.configs[name]; forbidden {
				return mountsForbidden(svc.Name, "config", name, whatControllerConfig)
			}
			if !scopedUnder(ns, name) && !permits(b.allow.Configs, name) {
				return mountsUnpermitted(svc.Name, "config", name, "allow.configs")
			}
		}
		for _, name := range volumeSources(svc) {
			if b.ownMount(mine.volumes, name) {
				continue
			}
			if _, forbidden := mine.volumes[name]; forbidden {
				return mountsForbidden(svc.Name, "volume", name, whatControllerVolume)
			}
			if !scopedUnder(ns, name) && !permits(b.allow.Volumes, name) {
				return mountsUnpermitted(svc.Name, "volume", name, "allow.volumes")
			}
		}
	}

	// What the stack claims as its own. Checked whether or not a service mounts
	// it: the adoption happens in applySecrets and applyConfigs, which run over
	// everything the manifest declares, so an unmounted declaration relabels the
	// controller's resource just the same.
	//
	// The self release gets no recognition here, deliberately, and the asymmetry
	// with the referenced half above is the point. Mounting a secret the
	// controller already has changes nothing about it; *declaring* one runs it
	// through applySecrets, which relabels the existing secret into the stack's
	// namespace and hands it to the services — an adoption, of a credential
	// nobody needed to adopt. `external: true` is how a chart references a
	// pre-created credential and is what the swarmcli-cd chart writes, so nothing
	// legitimate is refused; what is refused is the one release with the most to
	// gain from #86's trick, which is where that guard is worth keeping whole.
	for _, spec := range stack.Secrets {
		_, wired := b.forbiddenSecrets[spec.Name]
		_, mounted := mine.secrets[spec.Name]
		if wired || mounted {
			return declaresForbidden("secret", spec.Name, whatControllerSecret)
		}
		if !scopedUnder(ns, spec.Name) && !permits(b.allow.Secrets, spec.Name) {
			return declaresUnpermitted("secret", spec.Name, "allow.secrets")
		}
	}
	for _, spec := range stack.Configs {
		if _, forbidden := mine.configs[spec.Name]; forbidden {
			return declaresForbidden("config", spec.Name, whatControllerConfig)
		}
		known, err := records()
		if err != nil {
			return err
		}
		if _, forbidden := known[spec.Name]; forbidden {
			return declaresForbidden("config", spec.Name, whatReleaseRecord)
		}
		if !scopedUnder(ns, spec.Name) && !permits(b.allow.Configs, spec.Name) {
			return declaresUnpermitted("config", spec.Name, "allow.configs")
		}
	}

	// And the networks, which are neither reached for nor claimed so much as
	// joined. Both lists: an `external:` entry names a network the manifest
	// expects to find, and a declared one carries whatever `name:` said, which
	// applyNetworks then finds already there, leaves alone, and attaches the
	// stack's services to.
	for _, name := range stack.ExternalNetworks {
		if inControllersStack(mine.namespace, name) {
			if !b.selfRelease {
				return joinsForbidden(name, mine.namespace)
			}
			continue
		}
		if !scopedUnder(ns, name) && !permits(b.allow.Networks, name) {
			return joinsUnpermitted(name)
		}
	}
	for _, nw := range stack.Networks {
		if inControllersStack(mine.namespace, nw.Name) {
			if !b.selfRelease {
				return joinsForbidden(nw.Name, mine.namespace)
			}
			continue
		}
		if !scopedUnder(ns, nw.Name) && !permits(b.allow.Networks, nw.Name) {
			return joinsUnpermitted(nw.Name)
		}
	}
	return nil
}

// allowFor is the allowlist this deploy converts under: the application's own,
// and for the self release the host paths the controller is already bound to.
//
// The socket is why. compose.checkBindSources refuses a bind of anything the
// application's allow.hostPaths does not name, and it refuses it during
// conversion — before any of the guards below are reached. The chart that
// deploys this controller binds /var/run/docker.sock, because that is what the
// controller talks to the swarm through, so without this the self release is
// refused for re-declaring the mount it is already running with.
//
// It is the same recognition ownMount makes, in the one place a name is not what
// is being compared: this release is the controller, so the paths the controller
// holds are not paths it is reaching for. And it is bounded the same way — the
// set comes off the controller's own service spec, so a self chart can re-declare
// the socket and cannot reach / or /var/lib/docker. An operator who wants it to
// have more says so in allow.hostPaths, exactly as for any other application.
//
// Requiring allow.hostPaths for the socket instead was the alternative, and it
// is worse in the direction that matters: `self: true` already grants strictly
// more than the socket does, so the entry would carry no decision, and an
// operator who left it out would get a refusal about bind mounts for a mistake
// about self-management.
func (b *Backend) allowFor(ctx context.Context) (application.Allow, error) {
	if !b.selfRelease {
		return b.allow, nil
	}
	mine, err := b.mounts(ctx)
	if err != nil {
		return application.Allow{}, err
	}
	allow := b.allow
	allow.HostPaths = append(slices.Clone(allow.HostPaths), slices.Sorted(maps.Keys(mine.binds))...)
	return allow, nil
}

// ownMount reports whether this deploy may mount name because the controller
// already has it.
//
// True only for the self release, and only for a name in the set read off this
// controller's own service spec — which for that release is not a name reaching
// outside the stack at all. A self release *is* the controller's stack: the
// release namespace is the controller's namespace, and the only reason
// scopedUnder cannot see that is that a Swarm secret, config or volume is
// cluster-global and carries no namespace prefix on the reference.
//
// So this is not a permission granted to an application. It is the recognition
// that for one release the question the guard asks — "is this somebody else's" —
// has the answer no. Which is also why it takes the set read from the service
// spec rather than forbiddenSecrets: that one is derived from /run/secrets and
// names mount *targets*, so a target-renamed entry there is not the name any
// reference resolves by, and treating it as the controller's own would permit a
// different secret that happens to share the target's name.
func (b *Backend) ownMount(mine map[string]struct{}, name string) bool {
	if !b.selfRelease {
		return false
	}
	_, ok := mine[name]
	return ok
}

// permits reports whether an allowlist names name.
//
// Exact, unlike the containment Allow.PermitsPath does for a host path, because
// these are cluster-global names Swarm resolves by equality: there is no
// hierarchy among them for a looser match to follow, so anything looser would
// permit a name the operator did not write.
func permits(allowed []string, name string) bool {
	return slices.Contains(allowed, name)
}

// scopedUnder reports whether name belongs to the stack deployed as namespace.
//
// Conversion scopes everything a stack owns to "<namespace>_<name>"
// (convert.Namespace.Scope), so the prefix is the whole of "this is that stack's
// own" — for the release being deployed, whose resources need no permission, and
// for the controller's own stack, whose do not exist.
func scopedUnder(namespace, name string) bool {
	return namespace != "" && strings.HasPrefix(name, namespace+"_")
}

// inControllersStack reports whether a network name belongs to the stack this
// controller itself was deployed as — the namespace, or anything scoped under it.
//
// A namespace comparison and not a set of names, because the controller's own
// service spec names its networks by id (see selfMounts.volumes), and resolving
// those would take a third round trip inside the self-read for an answer that is
// worse: it would also refuse whatever shared network an operator deliberately
// attached this controller to, which is the operator's own topology rather than a
// tenant reaching for something. What is left uncovered is exactly that case — a
// tenant joining a network the operator put the controller on can reach its API,
// and so can everything else already there.
//
// A controller that is not a swarm service has no namespace and this is inert,
// the same answer rejectOwnNamespace gives, for the same reason: there is nothing
// of ours there to join.
func inControllersStack(namespace, name string) bool {
	return namespace != "" && (name == namespace || scopedUnder(namespace, name))
}

func mountsForbidden(service, kind, name, what string) error {
	return fmt.Errorf("service '%s' mounts %s '%s', which is %s; a reconciled stack may not mount it",
		service, kind, name, what)
}

func declaresForbidden(kind, name, what string) error {
	return fmt.Errorf("this stack declares %s '%s', which is %s; a reconciled stack may not declare "+
		"one of those names as its own, because Swarm addresses a %s by name — the existing one "+
		"would be handed to this stack's services and relabelled as this stack's", kind, name, what, kind)
}

// The refusals the application's own allowlist makes, as against the flat ones
// above. Each names the field to add the name to, because the operator reading
// this is the one who can, and because an allowlist whose message does not say
// where it lives is indistinguishable from a bug.
func mountsUnpermitted(service, kind, name, field string) error {
	return fmt.Errorf("service '%s' mounts %s '%s', which this release does not own and this application is "+
		"not permitted to reference. A %s is addressed by name with no namespace on the reference, so "+
		"naming another stack's is being handed it — add it to %s in the app set if that is what is meant",
		service, kind, name, kind, field)
}

func declaresUnpermitted(kind, name, field string) error {
	return fmt.Errorf("this stack declares %s '%s', which is not scoped to this release and which this "+
		"application is not permitted to reference. A declaration carrying a name that already exists is "+
		"handed the existing %s and relabels it as this stack's, so declaring one is not owning it — add "+
		"the name to %s in the app set if that is what is meant", kind, name, kind, field)
}

func joinsUnpermitted(name string) error {
	return fmt.Errorf("this stack joins network '%s', which is not scoped to this release and which this "+
		"application is not permitted to join. Everything already on a shared network is reachable from "+
		"it — add the name to allow.networks in the app set if that is what is meant", name)
}

func joinsForbidden(name, namespace string) error {
	return fmt.Errorf("this stack joins network '%s', which belongs to the stack this controller itself "+
		"is deployed as (%s); a reconciled stack may not join it. The controller's API is deliberately "+
		"unpublished and reachable only from inside the swarm, which is what makes a single bearer token "+
		"over plaintext HTTP an acceptable design — a stack on that network is on the inside. Give the "+
		"stack a network of its own", name, namespace)
}

// externalRefs names the secrets and configs one service references that the
// stack does not itself declare.
//
// Those are the references that resolve against the cluster-wide store rather
// than against something this deploy creates. A manifest's own declarations are
// usually namespace-scoped to "<stack>_<name>" on the way in, so a reference to
// an unscoped name usually came from an `external:` declaration — but "usually"
// is the whole of #86, and a declaration that named itself something else is why
// rejectForbiddenResources checks the declared set separately.
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

// volumeSources names the volumes one service mounts.
//
// All of them, unlike externalRefs, which drops the ones the stack declares.
// There is nothing to drop: a top-level `volumes:` entry creates nothing (Swarm
// makes a named volume on the node that first needs it, which is why DeployStack
// pre-creates none), so the entry contributes only the name the mount ends up
// carrying — namespace-scoped, or whatever `name:` said, for an `external: true`
// entry and a stack-owned one alike (convert.Volumes). That name is the whole of
// what the node will address, so it is the whole of what is worth comparing, and
// a declaration no service mounts reaches nothing.
//
// Binds are not here. A bind names a path rather than a cluster-wide name, so
// there is nothing for it to collide with, and the question it does raise — which
// paths on a node this application may reach at all — is answered one step
// earlier, in compose.checkBindSources, against the same allowlist.
func volumeSources(svc cdcompose.Service) []string {
	cs := svc.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return nil
	}
	out := make([]string, 0, len(cs.Mounts))
	for _, m := range cs.Mounts {
		if m.Type == mount.TypeVolume && m.Source != "" {
			out = append(out, m.Source)
		}
	}
	return out
}

// rejectSelfMismatch refuses a release declared as this controller's own that is
// not the stack this controller runs as.
//
// It is the half of the self declaration a file cannot be held to. `self: true`
// says "this application deploys me", and the only thing that makes that true is
// the release name: a release name *is* the stack namespace, so a self release
// named anything else deploys a second controller beside this one rather than
// upgrading it — with its own copy of the app set, reconciling the same
// applications on the same swarm, which is why the chart pins replicas to one.
// That is swarmcli-cd#234 as reported, and it got as far as it did because the
// name looked like a detail.
//
// The name to compare against is a label on this controller's own service, so it
// takes the daemon to read and cannot be checked by the config loader. Both
// failures name what the controller is actually deployed as, because that string
// is the fix.
//
// A controller with no namespace at all is refused rather than waved through,
// which is the opposite of what the guards built on the same read do. They ask
// "is this ours to protect", and nothing is: a development run has no stack of
// its own for a release to claim. This asks "is this release us", and for a
// process that is not a swarm service the answer is no — there is nothing here
// to upgrade, and deploying the controller's chart as an ordinary release is not
// what the app set asked for. A destination resolving to another swarm arrives
// the same way, because that swarm's daemon has never heard of this container.
func (b *Backend) rejectSelfMismatch(ctx context.Context, release string) error {
	if !b.selfRelease {
		return nil
	}
	mine, err := b.mounts(ctx)
	if err != nil {
		return err
	}
	switch {
	case mine.namespace == "":
		return fmt.Errorf("refusing release '%s': it is declared as this controller's own, but this controller is not "+
			"deployed as a stack on this swarm — a development run and an application destined for another swarm both "+
			"read this way, and neither has a stack of its own to upgrade", release)
	case mine.namespace != release:
		return fmt.Errorf("refusing release '%s': it is declared as this controller's own, but this controller runs as "+
			"the stack '%s'. A release name is the stack namespace, so this one would deploy a second controller beside "+
			"this one rather than upgrading it; name the release '%s'", release, mine.namespace, mine.namespace)
	}
	return nil
}

// rejectOwnNamespace refuses a release whose name is the stack namespace this
// controller itself was deployed under.
//
// A release name is chosen by whoever writes the release file — CE validates its
// characters and nothing else — and it becomes the stack namespace, which is the
// whole of "which resources are this release's". The controller is deployed as a
// stack too, with `docker stack deploy -c stack.yml swarmcli-cd`, so its own
// service carries com.docker.stack.namespace=swarmcli-cd. A release of that name
// therefore scopes its services onto the controller's: ApplyServices finds the
// controller's service under the namespace it was told to converge, and writes
// the chart's spec over it — any image, the docker.sock the controller is
// mounted with, a manager placement. RemoveStack is the same collision pointed
// the other way and needs no chart at all: it removes everything carrying the
// label, which is the controller and, with prune-volumes on, the volume holding
// every application's clone and chart cache. Nothing reconverges after that,
// because the thing that would have is gone (#102).
//
// So both verbs are guarded, and the guard is here rather than at the reconciler
// because these two methods are what every path acting on a release name goes
// through — the engine's install, upgrade and uninstall, the drift correction,
// and the sweep of a departed application, which resolves its own backend and
// wears none of the reconciler's per-application clothing.
//
// A controller that is not a swarm service has no namespace and this is inert: a
// `go run` during development deploys and removes exactly as before. That is the
// same answer selfMounts gives about the mounts, for the same reason — there is
// nothing of ours there to claim. A daemon that could not be asked is not that
// answer and fails the call, which for a removal means the next sweep tries
// again.
func (b *Backend) rejectOwnNamespace(ctx context.Context, release string) error {
	mine, err := b.mounts(ctx)
	if err != nil {
		return err
	}
	if mine.namespace == "" || mine.namespace != release {
		return nil
	}
	return fmt.Errorf("refusing to act on release '%s': it is the stack namespace this controller itself "+
		"is deployed under, so deploying it would write this release's services over the controller's own "+
		"and removing it would delete the controller. Give the release a name of its own", release)
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
// The manifest is converted twice, and the first conversion is thrown away. It
// is what makes that order compatible with refusing a stack whole:
// converting a service resolves every config and secret it mounts to the id
// Swarm addresses it by, so the conversion that is applied cannot run until
// those exist — while the guard has to read what a service mounts before
// anything has been created (swarmcli-cd#84 for the first, #63 for the second).
// cdcompose.ConvertUnresolved answers that first read without needing anything
// to exist, and produces the same names for the guard to compare; the second
// conversion is the one whose specs are applied.
//
// Nothing is deleted. Phase 1 is explicitly no prune.
//
// req.Files is ignored, and that is a decision rather than an oversight. A
// manifest reaching here can never name a file: this controller converts it
// in-process, with no checkout for a relative path to resolve against
// (cdcompose.Convert sets WorkingDir: "/"), and cdcompose.checkFileSources
// refuses configs.*.file, secrets.*.file and services.*.env_file outright
// before the loader can read one — swarmcli-cd#99, because the only filesystem
// those paths could name is the one holding the Docker socket, the application
// set and /run/secrets. The chart engine fills the map from exactly those keys,
// so what arrives here is always empty.
//
// Making the field mean something is a separate change with its own threat
// argument, not a line added to this method: it means materialising the files
// to a temp directory, pointing WorkingDir at that directory, and relaxing
// checkFileSources from "refuse the key" to "refuse a path that escapes" —
// reopening half of #99's guard on a process holding the socket. That is #528's
// CE-side PR 4 and a follow-up issue here.
func (b *Backend) DeployStack(ctx context.Context, req charts.DeployRequest) error {
	// Before the manifest is even converted: a release claiming the controller's
	// own stack is refused whatever it declares, because the collision is the
	// name and not anything in the chart — and a release declaring itself the
	// controller's own and named anything else is refused first, because that
	// one deploys a second controller instead of colliding with this one.
	if err := b.rejectSelfMismatch(ctx, req.Name); err != nil {
		return err
	}
	// And for every release that is not this controller's own, the collision
	// that name would cause. The self release is exempt here and nowhere else:
	// deploying onto the controller's own services is what upgrading it *is*,
	// and the check above has already established the name is the controller's
	// rather than the app set's claim that it is. Removal is not exempt and must
	// not be — see RemoveStack and StackVolumes.
	if !b.selfRelease {
		if err := b.rejectOwnNamespace(ctx, req.Name); err != nil {
			return err
		}
	}

	allow, err := b.allowFor(ctx)
	if err != nil {
		return err
	}
	unresolved, err := cdcompose.ConvertUnresolved(ctx, req.Manifest, req.Name, b.api, allow)
	if err != nil {
		return err
	}
	// Before any resource is created: a stack that reaches for one of the
	// controller's own secrets or configs is refused whole, not half-deployed.
	// So is a manifest that will not convert at all, because the pass above is
	// that same conversion. The one thing it cannot catch is an `external:`
	// reference to something that does not exist, since assuming it does is how
	// it works — the chart engine pre-flights those before it calls this, and
	// says so for the same reason.
	if err := b.rejectForbiddenResources(ctx, unresolved); err != nil {
		return err
	}
	// The networks, secrets and configs are read from the unresolved pass
	// because the daemon plays no part in deriving them: they are the same specs
	// the conversion below would produce, and creating them is what lets that
	// conversion run at all.
	if err := b.applyNetworks(ctx, unresolved); err != nil {
		return err
	}
	if err := b.applySecrets(ctx, unresolved.Secrets); err != nil {
		return err
	}
	if err := b.applyConfigs(ctx, unresolved.Configs); err != nil {
		return err
	}

	stack, err := cdcompose.Convert(ctx, req.Manifest, req.Name, b.api, allow)
	if err != nil {
		return err
	}
	return b.ApplyServices(ctx, stack, req.Resolve)
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
func (b *Backend) RemoveStack(ctx context.Context, name string) error {
	// A destructive call with no stack to scope to would build the filter
	// "com.docker.stack.namespace=", and what that matches is the daemon's
	// business rather than something to find out here. The engine validates
	// release names, so this is unreachable today; it costs one line to keep it
	// that way.
	if name == "" {
		return fmt.Errorf("refusing to remove a stack with no name")
	}

	// And the name that is worse than none: this controller's own. Nothing below
	// checks who owns what — that is what `docker stack rm` does and what this
	// deliberately copies — so the only thing standing between a release name and
	// the controller's own services, network and volumes is that the name is not
	// the controller's (#102).
	if err := b.rejectOwnNamespace(ctx, name); err != nil {
		return err
	}

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
			errs = append(errs, fmt.Errorf("removing service '%s': %w", s.Spec.Name, err))
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
			errs = append(errs, fmt.Errorf("removing config '%s': %w", c.Spec.Name, err))
		}
	}

	secrets, err := b.api.SecretList(ctx, swarm.SecretListOptions{Filters: stackFilter(name)})
	if err != nil {
		return fmt.Errorf("listing the stack's secrets: %w", err)
	}
	for _, s := range secrets {
		if err := b.api.SecretRemove(ctx, s.ID); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing secret '%s': %w", s.Spec.Name, err))
		}
	}

	networks, err := b.api.NetworkList(ctx, network.ListOptions{Filters: stackFilter(name)})
	if err != nil {
		return fmt.Errorf("listing the stack's networks: %w", err)
	}
	for _, n := range networks {
		if err := b.api.NetworkRemove(ctx, n.ID); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing network '%s': %w", n.Name, err))
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
func (b *Backend) RefreshSnapshot(context.Context) error { return nil }

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
// failure to report. That is true of awaitConverged and false of everything
// that asks once and then takes a view — see ReadStackServices.
func (b *Backend) StackServices(ctx context.Context, name string) []charts.ServiceState {
	states, _ := b.ReadStackServices(ctx, name)
	return states
}

// ReadStackServices is the same read with the failure kept, for the caller that
// has to tell "the swarm has no services under this release" from "the swarm
// could not be asked".
//
// Those are one answer to StackServices and opposite findings to a health
// rollup, which reads an empty list as the positive assertion "deployed, but no
// services are present on the swarm". One slow daemon therefore flipped every
// release of an application from healthy to missing — the loudest state the
// rollup has, and the one anything alerting is watching for (#107).
//
// It is a method beside StackServices rather than its signature because
// charts.Backend is CE's interface: widening it would change every
// implementation CE has, for a distinction only this repository's caller needs.
// The reconciler reaches this one through an optional-interface upgrade, the
// same way it reaches WithRegistryAuth, so a backend that cannot answer reads
// exactly as it did before.
func (b *Backend) ReadStackServices(ctx context.Context, name string) ([]charts.ServiceState, error) {
	snap, err := docker.SnapshotWith(ctx, b.api)
	if err != nil {
		b.log.Warn("reading the swarm snapshot failed", "stack", name, "error", err)
		return nil, fmt.Errorf("reading the swarm snapshot: %w", err)
	}
	return charts.ServiceStatesFrom(snap, name), nil
}

// ReadStacks answers for several releases from one look at the swarm.
//
// It exists because the read above is not scoped to a release at all: the
// snapshot is the whole swarm — NodeList, ServiceList, TaskList and Info — and
// ServiceStatesFrom filters it afterwards. Asking per release therefore fetched
// the entire swarm per release and discarded all but one stack's worth of it
// each time, so an application declaring ten releases spent ten identical round
// trips on every reconcile, against the manager this controller runs on.
//
// All-or-nothing, which is the same answer the single read gives: either the
// snapshot arrived and every release is in the map, or it did not and none are.
// An empty slice for a release therefore means the swarm really has no services
// under that name, which is precisely the distinction #107 turned on.
func (b *Backend) ReadStacks(ctx context.Context, releases []string) (map[string][]charts.ServiceState, error) {
	snap, err := docker.SnapshotWith(ctx, b.api)
	if err != nil {
		b.log.Warn("reading the swarm snapshot failed", "stacks", releases, "error", err)
		return nil, fmt.Errorf("reading the swarm snapshot: %w", err)
	}

	out := make(map[string][]charts.ServiceState, len(releases))
	for _, release := range releases {
		out[release] = charts.ServiceStatesFrom(snap, release)
	}
	return out, nil
}
