// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package local is the OSS swarms.Registry: it resolves exactly one swarm, the
// one this controller runs in, over the docker.sock it is mounted with (D2).
//
// It is a package of its own rather than a default inside the seam, so that the
// seam stays a leaf. swarms is the contract a companion implements; building
// the local backend needs swarmcli-cd/backend, the whole Docker applier, and
// with the default living in the seam every importer of the contract linked the
// implementation. prune is the one that shows why that is wrong — it advertises
// itself as pure policy and could not build without the applier.
//
// The binary blank-imports this. cmd/swarmcli-cd/main.go is already the file
// whose entire purpose is blank imports, which is where the companion adds its
// own; see docs/extensibility.md.
package local

import (
	"context"
	"fmt"
	"sync"

	"github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/backend"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// A default may not overwrite a companion, and here that has to be checked
// rather than assumed.
//
// The other three seams keep their default in the package a companion must
// import, so Go's rule that an imported package initialises first makes the
// companion's init() necessarily later and the companion necessarily win. This
// one does not: swarms/local and a companion's registry are unrelated siblings,
// and gc initialises siblings in import-path order — under which
// "…/swarmcli-cd-be/swarms" sorts before "…/swarmcli-cd/swarms/local", because
// '-' precedes '/'. Registering unconditionally would therefore overwrite the
// companion, in a build that looks correct and whose only symptom is
// swarms.Active reporting "local".
//
// Declining to overwrite is order-independent, so the companion wins whichever
// way the paths happen to sort — which is a stronger guarantee than the one
// last-wins gave before the default moved out.
func init() { register() }

// register is init()'s body, separated so that a test can run it the way gc
// would if it ordered this package after a companion's — which is exactly the
// case that must not overwrite and exactly the one an init() cannot be made to
// reproduce.
func register() {
	if swarms.Active() == "none" {
		swarms.Register("local", &registry{})
	}
}

// registry resolves only the swarm this controller runs in.
//
// It implements neither swarms.Lister nor swarms.NodeReach, and both omissions
// are the honest answer rather than a gap. There is one swarm to enumerate and
// the caller is already on it; and a Swarm node does not expose its daemon to
// anything by default, so this controller — given one socket, its own — cannot
// reach another node whatever interface it declares.
type registry struct {
	once    sync.Once
	backend charts.Backend
	err     error
}

// Backend returns the ambient-context backend. charts.NewDockerBackend("")
// resolves the Docker context the process was started with, which for an
// in-swarm controller is the mounted docker.sock.
func (r *registry) Backend(_ context.Context, t swarms.Target) (charts.Backend, error) {
	if t.Swarm != "" {
		return nil, fmt.Errorf("unknown swarm '%s': this build resolves only the swarm the controller runs in", t.Swarm)
	}
	// The client is built once and reused: it holds a connection pool, and the
	// reconcile loop asks on every tick for every application.
	r.once.Do(func() {
		var cli *client.Client
		cli, r.err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if r.err != nil {
			r.err = fmt.Errorf("connecting to the local docker daemon: %w", r.err)
			return
		}
		r.backend = backend.New(cli, backend.Options{})
	})
	return r.backend, r.err
}
