// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package capability names the optional interfaces a backend may implement.
//
// The reconciler and the sweep hold a charts.Backend, which is the smallest
// contract that can deploy a stack and little else. Everything beyond it that
// they ask a backend for — read a ServiceSpec, list what a stack declared, scope
// an image pull to one application's credential, count the swarm's nodes — is an
// upgrade they type-assert for, so a backend that cannot answer loses that one
// feature instead of failing. *backend.Backend implements every one of them, and
// asserts so at compile time; a Phase 3 remote backend reached through the same
// swarms seam implements whichever it can.
//
// They live here rather than beside the callers that assert for them because
// they are the contract such a backend is written against, and an unexported
// contract is not one. Go interfaces are structural, so a companion in another
// module *can* satisfy an interface it cannot name — but it cannot be
// compile-checked against it, and since every one of these is an optional
// upgrade that falls back silently when the assertion fails, the companion would
// learn about a changed signature by watching a feature quietly stop working
// rather than from a build error.
//
// A package of its own rather than twelve more exported names in reconcile and
// prune. This repository's exported surface *is* the companion contract, so
// where a name lives is most of what says whether it is one: these are, and the
// reconciler's own Fetcher, Builder and Engine — which it states for itself and
// nobody outside implements — are not. It also leaves both of those packages'
// public surfaces exactly as they were, and gives a backend one import rather
// than two.
//
// Nothing here has an implementation, and no fallback lives here either. What a
// caller does when an assertion fails is different for every one of these —
// unchanged backend, no live drift, no sweep, no purge — so it stays with the
// caller that has to choose.
//
// One value is not an interface: ErrOwnStack. It is here on the same test
// everything else is — it is part of what a backend is written against, and a
// caller matching the OSS applier's own sentinel instead would make every
// companion import that applier.
//
// Adding one is the same commitment as adding a seam method: the day the
// companion ships, every signature in this file is frozen, and a capability that
// needs to grow grows by taking a struct rather than by widening a parameter
// list.
package capability

import (
	"context"
	"errors"

	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/compose"
	"github.com/Eldara-Tech/swarmcli-cd/regauth"
)

// ErrOwnStack is what a backend's refusal to act on the stack this controller
// runs as wraps, so that a caller sweeping rather than deploying can tell it
// from the other reasons a removal fails.
//
// The only error value in a package of interfaces, and here for the same reason
// they are: it is part of what a backend is written against. A sweep that
// matched on the OSS applier's own sentinel would make every companion backend
// import that applier to be skipped correctly, which is the coupling this
// package exists to remove.
//
// One caller needs it. An operator who drops the self application from the app
// set leaves the controller's own stack stamped for an application nobody
// declares, and no sweep can remove it — deleting it would delete the
// controller, which every backend must refuse. Without a way to recognise that
// refusal the sweep reports the same failure every interval, for ever, about a
// controller that is working perfectly.
//
// A backend that does not wrap it is not broken. The sweep then does what it
// did before and reports the failure, which is degraded rather than wrong —
// the same silent-fallback bargain every interface here makes.
var ErrOwnStack = errors.New("it is the stack namespace this controller itself is deployed under")

// RegistryAuth is the optional interface a backend implements to authenticate
// image pulls with an application's credential. A backend reached through the
// swarms seam that authenticates its own way need not, and is left unchanged.
type RegistryAuth interface {
	WithRegistryAuth(regauth.Resolver) charts.Backend
}

// ForbidSecrets is the optional interface a backend implements to refuse a stack
// mounting the controller's own secrets.
type ForbidSecrets interface {
	WithForbiddenSecrets(map[string]struct{}) charts.Backend
}

// AllowedReferences is the optional interface a backend implements to take one
// application's allowlist of what its charts may reach outside their own
// releases.
//
// A backend that does not implement it enforces its own. The refusal belongs
// where the specs are written rather than in the reconciler, so that nothing can
// reach a swarm past a layer that did not look.
type AllowedReferences interface {
	WithAllowedReferences(application.Allow) charts.Backend
}

// SelfRelease is the optional interface a backend implements to deploy the
// stack this controller itself runs as — the one release whose upgrade replaces
// the process performing it.
//
// It takes the deferral rather than a bare flag because the two are one
// decision. What makes a self release safe to deploy at all is that the write
// replacing this controller is issued last, after everything the pass did has
// been recorded; a backend that cannot hand that write back cannot be asked for
// the exemptions either, and a nil hold therefore leaves the backend unchanged.
// That is the safe direction: a self release that deployed everything except
// the controller and never came back for it would report a successful sync and
// have changed nothing.
type SelfRelease interface {
	WithSelfRelease(hold DeferSelf) charts.Backend
}

// DeferSelf is handed the one write that replaces this controller, for the
// caller to issue once the pass it belongs to is over.
//
// A sink rather than a return value because a deploy reaches a backend by two
// routes — the chart engine's apply, whose signature is charts.Backend's, and
// the reconciler's own drift correction, which calls DeployStack directly — and
// widening the one this repository does not own is not available. A sink is the
// same shape for both, and holding one for the whole pass rather than one per
// deploy is what stops a deferral taken during a correction from being dropped.
type DeferSelf func(apply func(context.Context) error)

// OutOfBand is the optional interface a backend implements to report a mutation
// that lost its compare-and-swap.
//
// Swarm gives the controller exactly one signal that something else is writing
// to a service — ?version= is mandatory, so a mutation that loses the race is
// refused — and only the reconciler knows which application a write belongs to,
// which is why the backend reports rather than notifies.
type OutOfBand interface {
	WithOutOfBandNotifier(func(service string)) charts.Backend
}

// StackServicesReader is the optional interface a backend implements to report a
// failed read rather than an empty stack.
//
// charts.Backend.StackServices has no error return, and for its CE caller that
// is right: it polls, so a daemon that could not be asked is "not converged yet"
// and the next poll asks again. A caller that instead publishes what it read
// turns the same nil into "deployed, but no services are present on the swarm",
// the loudest thing a health rollup can say — so one slow daemon flipped every
// release of an application from healthy to missing (#107).
type StackServicesReader interface {
	ReadStackServices(ctx context.Context, name string) ([]charts.ServiceState, error)
}

// StacksReader is the optional interface a backend implements to answer for
// several releases from one look at the swarm.
//
// The read behind StackServices is a whole-swarm snapshot — NodeList,
// ServiceList, TaskList and Info — which is then filtered by stack name. Asking
// it once per release fetches the entire swarm once per release and throws away
// all but one stack's worth each time.
//
// The contract is all-or-nothing: a nil error means every requested release is
// in the map, and a non-nil one means the swarm could not be read and none of
// them are. That is what one snapshot actually does, and it is what keeps a
// daemon that could not be asked from being published as a swarm with no
// services on it.
type StacksReader interface {
	ReadStacks(ctx context.Context, releases []string) (map[string][]charts.ServiceState, error)
}

// LiveDrift is the optional interface a backend implements to expose the two
// halves of a live drift comparison.
//
// It exists because charts.Backend carries no way to read a ServiceSpec — its
// StackServices returns a display projection with a running count and no spec —
// and no Docker client for the reconciler to convert a manifest with. A backend
// that cannot answer does not implement it, and its applications report no live
// drift rather than failing.
//
// Only the read half needs a capability. Correcting drift is DeployStack, which
// is on charts.Backend already.
type LiveDrift interface {
	DesiredServices(ctx context.Context, manifest, stack string) (*compose.Stack, error)
	LiveServices(ctx context.Context, stack string) (map[string]swarm.Service, error)
}

// NetworkNamer is the optional interface a backend implements to name the
// networks a service is attached to.
//
// A spec names them by id, because the daemon rewrites each target to one as it
// writes the service, so without this the attachments cannot be compared at all.
// Separate from LiveDrift for the reason ResourceLister is: a backend that can
// read services but not list networks should lose that one field and keep every
// other comparison.
//
// Not stack-scoped, unlike ResourceLister's three. A service may be attached to
// an external network or a predefined one, neither of which carries the stack's
// namespace label, and an attachment that could not be named is exactly the one
// worth reporting.
type NetworkNamer interface {
	LiveNetworkNames(ctx context.Context) (map[string]string, error)
}

// DeclaredLister is the optional interface a backend implements to answer what a
// manifest declares without needing any of it to exist.
//
// LiveDrift.DesiredServices cannot answer that question about a *stored*
// revision. Converting a service resolves every config and secret it mounts to
// the id Swarm addresses it by, so it asks today's swarm about yesterday's
// references — and a revision that mounted a config a previous sweep has since
// deleted no longer converts at all. The sweep then loses that revision's claims
// and leaves resources behind (#87). Nothing about proving ownership wants an
// id; the scoped name is the whole of it.
//
// Separate from LiveDrift rather than added to it, for the reason ResourceLister
// gives: a backend that cannot answer this should lose the sweep's history walk,
// not its live drift too. And separate from DesiredServices rather than
// replacing it, because live drift compares whole ServiceSpecs and must never
// start diffing against a placeholder id.
type DeclaredLister interface {
	DeclaredResources(ctx context.Context, manifest, stack string) (*compose.Stack, error)
}

// ResourceLister is the optional interface a backend implements to read the
// other three kinds a manifest declares, by scoped name.
//
// Separate from LiveDrift because live drift compares services and nothing else:
// a backend that can answer one and not the other should lose only the half it
// cannot answer. A backend implementing neither prunes nothing, which is the
// degradation live drift already has.
//
// Name to id is all the sweep needs — it matches on the scoped name, the only
// key a manifest and a live resource share, and deletes by id.
type ResourceLister interface {
	LiveNetworks(ctx context.Context, stack string) (map[string]string, error)
	LiveConfigs(ctx context.Context, stack string) (map[string]string, error)
	LiveSecrets(ctx context.Context, stack string) (map[string]string, error)
}

// ResourceRemover is the optional interface a backend implements to delete a
// single resource of each kind.
//
// Separate from the readers rather than folded into them, so that a backend
// which can read the swarm but not write to it still reports what a sweep would
// remove instead of losing the report along with the removal.
type ResourceRemover interface {
	RemoveService(ctx context.Context, id string) error
	RemoveNetwork(ctx context.Context, id string) error
	RemoveConfig(ctx context.Context, id string) error
	RemoveSecret(ctx context.Context, id string) error
}

// SwarmSizer is the optional interface a backend implements to count the swarm's
// nodes.
//
// A backend that does not cannot say whether a node-local volume listing was the
// whole swarm's, which for a deletion has to mean the same as knowing it was not.
type SwarmSizer interface {
	SwarmNodes(ctx context.Context) (int, error)
}

// NodeRoster is the optional interface a backend implements to describe the
// swarm's nodes rather than only count them.
//
// Separate from SwarmSizer rather than a widening of it, for the reason the two
// resource interfaces are separate: the count answers whether a node-local
// listing was the whole swarm's, and is asked on the deletion path where a
// backend that cannot answer has to mean "assume not". The roster answers what
// the cluster looks like, is asked by the API on a read, and a backend that
// cannot answer it says so rather than being taken for a swarm with no nodes.
//
// It returns the wire type rather than the daemon's, as AllowedReferences takes
// one: a companion backend reached through the swarms seam must not have to
// depend on the moby client's node and task types to describe its own swarm,
// and mapping them is exactly the work this interface exists to have done
// already.
//
// An empty roster is not a valid answer. A swarm with no nodes is not a thing,
// so a backend that finds one — a locked swarm answers that way — returns an
// error instead, which is what keeps "could not read" distinguishable from
// "read, and there is nothing there".
type NodeRoster interface {
	SwarmNodeRoster(ctx context.Context) ([]application.SwarmNode, error)
}

// ServiceLogRequest names one tail of one service's container output.
//
// A struct rather than three parameters, for the reason this package's doc
// comment gives: a capability that needs to grow grows by taking a struct. The
// growth this one can already see coming is `since`, which the daemon supports
// and no caller asks for yet.
type ServiceLogRequest struct {
	// Service is the swarm service name, as the application's own status
	// reported it. It is not a name the reader may resolve for itself: the
	// caller authorised a subject for one application and matched this string
	// against that application's services, and a reader that looked it up
	// swarm-wide would answer for a service nobody was granted.
	Service string
	// Tail is how much scrollback a newly attached client is given.
	Tail int
	// Follow keeps the stream open for new lines. False reads the tail and
	// ends, which nothing asks for yet and which costs nothing to pass through.
	Follow bool
}

// ServiceLogReader is the optional interface a backend implements to tail a
// service's container output.
//
// It hands back a channel rather than an io.ReadCloser, and the events rather
// than bytes, because the two labels the wire type carries — which task, which
// node — exist per line and have nowhere to go in a byte stream. Demultiplexing
// Docker's framing is the one part of this job with rules rather than plumbing,
// and it needs the daemon in front of it, so it happens here rather than being
// re-encoded into lines for a caller to parse back.
//
// The implementation owns the channel and closes it exactly once, when the
// stream ends. Cancelling ctx is how a caller stops it; there is no Close to
// forget. Sends must not block on a caller that has stopped reading — a
// container in a crash loop outruns a browser — so an implementation drops and
// says so, which is what application.ServiceLogEvent.Notice is for.
type ServiceLogReader interface {
	ServiceLogs(ctx context.Context, req ServiceLogRequest) (<-chan application.ServiceLogEvent, error)
}
