// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package swarms resolves an application's destination to something that can
// apply to it.
//
// This is the seam Phase 3 replaces with multi-swarm through
// swarmcli-rbac-proxy. Per D2 the OSS default resolves exactly one swarm: the
// one this controller runs in, reached over the docker.sock it is mounted with.
// It lives in swarms/local and the binary blank-imports it, so that this
// package — the contract a companion implements — carries no implementation of
// its own and nothing importing it links the Docker applier.
//
// # What a registry may be asked
//
// Registry is the whole of what a companion must implement. Two capabilities
// beyond it are optional interfaces rather than methods, which is the shape
// this repository uses everywhere a backend may or may not be able to answer:
// Lister enumerates swarms, NodeReach reaches the individual nodes of one. A
// registry that cannot do either simply does not implement them, and the
// callers degrade to what the OSS build has always done rather than failing.
//
// The alternative — putting them on Registry — would mean every companion,
// including one that only ever wanted to point at a second swarm, had to write
// four methods it has no answer for.
package swarms

import (
	"context"
	"errors"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/seam"
)

// Target names what a backend is wanted for.
//
// It is a struct rather than a parameter list for the reason secrets.Request
// states: an implementation receives the struct, so a field added later costs
// it nothing, whereas widening a parameter list is a breaking change to an
// interface implemented outside this repository. Registry is the only seam
// method whose parameters this repository can still choose freely — after the
// companion ships, it cannot.
type Target struct {
	// Swarm is the destination's swarm, as application.Spec.Destination names
	// it. Empty means the swarm this controller runs in, which for the OSS
	// build is the only one there is.
	Swarm string
}

// Registry resolves a destination to a backend.
//
// It hands back a charts.Backend rather than an interface of our own.
// swarmcli-cd is built on the chart engine — charts.NewEngineWith takes any
// Backend, which is what makes a controller-supplied applier possible at all —
// and a fourteen-method pass-through would buy nothing but a second definition
// to keep in step with CE's.
type Registry interface {
	// Backend returns the backend for a target.
	//
	// Implementations return the same backend for the same target rather than
	// reconnecting per call: the reconcile loop asks on every tick.
	Backend(ctx context.Context, t Target) (charts.Backend, error)
}

// Lister is the optional interface a Registry implements when it can enumerate
// the swarms it resolves.
//
// It exists for the sweep. prune finds a departed application's releases by
// asking a swarm what it still holds, so it can only find them on a swarm it
// can name — and after an application leaves the app set, nothing names its
// swarm any more. A registry resolving one swarm has no gap to close and does
// not implement this; a multi-swarm one does, and the sweep can then cover
// every swarm rather than the one the controller happens to run in.
//
// A consumer must swarm-qualify what it holds before acting on the result. The
// sweep spares any release the current applications still declare, and it
// matches those by release name — which is unique within a swarm and not
// across them. Comparing an unqualified list against several swarms' releases
// would spare a release on swarm B because a different one shares its name on
// swarm A, and, worse, delete one on B that nothing on B declares. Nothing
// consumes this yet for exactly that reason: the qualification is a change to
// what the app-set loop hands the sweep, not to the sweep.
type Lister interface {
	// Swarms names every swarm this registry resolves, including the empty
	// target if it resolves the local one.
	//
	// Targets rather than names, so that this survives Target growing a second
	// field. Handing back bare names would make a consumer re-wrap them, and
	// the day a target is more than a swarm name — a tenant, a context, a
	// credential — that re-wrapping would have nothing to put in it and this
	// signature would have to break. It has no consumer yet, so it costs
	// nothing to get right now and cannot be got right later.
	Swarms(ctx context.Context) ([]Target, error)
}

// Node is one node of a swarm, as a registry can reach it.
//
// A struct for the same reason Target is: it is handed to NodeBackend, so it
// can grow — an address, a role, a reachability state — without breaking an
// implementation outside this repository.
type Node struct {
	// ID is the node's Swarm id, which is what the daemon's node list keys on.
	ID string
	// Hostname is what an operator recognises the node by, and is what the
	// controller logs. It is not unique and is not a key.
	Hostname string
}

// NodeReach is the optional interface a Registry implements when it can reach
// the individual nodes of a swarm rather than only the swarm's manager API.
//
// It exists because one question the controller has to answer is node-local
// and nothing else in the design is. The daemon's volume list answers from the
// node's own store — only CSI cluster volumes come from swarm, and only on a
// manager — while an ordinary named volume is created by the engine running
// the task that mounts it. So a stack's volumes live on whichever nodes
// scheduled its tasks, and the manager API cannot see them, let alone delete
// them.
//
// **The OSS build does not implement this**, and that is a deployment fact
// rather than an omission: a Swarm node does not expose its daemon to anything
// by default, and this controller is given one socket, its own. Reaching the
// others means operator-provisioned endpoints on every node, a per-node agent,
// or a privileged global-mode service — each of which is a decision about what
// the controller is allowed to be, not a function somebody forgot to write.
//
// So prune keeps deleting what its own node can see and saying plainly that
// that was not all of them, and a companion that has solved the reach properly
// makes the same sweep complete. See Eldara-Tech/swarmcli-cd#108.
type NodeReach interface {
	// Nodes names the nodes of a target's swarm that this registry can reach.
	//
	// Reachable, not merely present. A node this cannot connect to should be
	// left out rather than listed and then failed on: an omission is the
	// recoverable answer — the caller covers what it was given and reports the
	// rest as uncovered — while a node that is listed and then unreachable
	// fails the caller's whole operation, which for a node that is permanently
	// gone means an operation that can never complete.
	//
	// Omitting is safe because no caller may assume this is every node. prune
	// checks the count against the swarm's own before it claims to have covered
	// the swarm, precisely so that "a node was down" degrades to an honest
	// partial rather than to a silent one.
	Nodes(ctx context.Context, t Target) ([]Node, error)

	// NodeBackend returns a handle to one node's own daemon.
	NodeBackend(ctx context.Context, t Target, node Node) (NodeBackend, error)
}

// NodeBackend is what node-local work needs, and deliberately nothing more.
//
// It is not a charts.Backend. A worker node's daemon cannot deploy a stack,
// remove one or read a service spec — those are manager operations — so
// handing back something with DeployStack on it would promise what the node
// cannot do and invite a caller to try. The two methods here are the two
// questions that are genuinely node-local.
//
// A NodeBackend is only ever asked about a release the caller has already
// proved it owns and may delete. It is not a second gate, and an implementation
// should not try to be one: it has none of the evidence — no owner stamp, no
// app set — that the decision was made from.
type NodeBackend interface {
	// StackVolumes names the node's volumes labelled as belonging to a stack.
	StackVolumes(ctx context.Context, stack string) ([]string, error)
	// RemoveVolume deletes one of them by name. A volume that is already gone
	// is not an error: a caller retrying a partial sweep must not be failed by
	// the part that worked.
	RemoveVolume(ctx context.Context, name string) error
}

var slot seam.Slot[Registry]

// Register installs r as the swarm registry, replacing whatever was there.
// Call it from an init().
func Register(name string, r Registry) { slot.Register(name, r) }

// Get returns the registry in force.
func Get() Registry { return slot.Get() }

// Active names the registry in force, for startup logging.
func Active() string { return slot.Name() }

func init() { Register("none", unconfigured{}) }

// unconfigured is what is in force when nothing else registered.
//
// It exists so that the failure has a name. The default implementation lives in
// swarms/local and arrives by blank import, which means a binary can be built
// without it — and a nil Registry would surface as a nil dereference inside a
// reconcile goroutine, a panic naming a line that has nothing to do with the
// mistake. This returns an error saying what is missing, and Active reports
// "none" in the seams line the controller logs at startup, before anything has
// reconciled at all.
type unconfigured struct{}

// errNoRegistry names the missing blank import rather than the symptom.
var errNoRegistry = errors.New("no swarm registry is registered: the binary must import " +
	"github.com/Eldara-Tech/swarmcli-cd/swarms/local for the default, or a companion package that registers its own")

// Backend implements Registry.
func (unconfigured) Backend(context.Context, Target) (charts.Backend, error) {
	return nil, errNoRegistry
}
