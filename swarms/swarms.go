// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package swarms resolves an application's destination to something that can
// apply to it.
//
// This is the seam Phase 3 replaces with multi-swarm through
// swarmcli-rbac-proxy. Per D2 the OSS default resolves exactly one swarm: the
// one this controller runs in, reached over the docker.sock it is mounted with.
package swarms

import (
	"context"
	"fmt"
	"sync"

	"github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/backend"
	"github.com/Eldara-Tech/swarmcli-cd/seam"
)

// Registry resolves a swarm name to a backend.
//
// It hands back a charts.Backend rather than an interface of our own.
// swarmcli-cd is built on the chart engine — charts.NewEngineWith takes any
// Backend, which is what makes a controller-supplied applier possible at all —
// and a fourteen-method pass-through would buy nothing but a second definition
// to keep in step with CE's.
type Registry interface {
	// Backend returns the backend for the named swarm. The empty name means
	// the swarm this controller runs in.
	//
	// Implementations return the same backend for the same name rather than
	// reconnecting per call: the reconcile loop asks on every tick.
	Backend(ctx context.Context, swarm string) (charts.Backend, error)
}

var slot seam.Slot[Registry]

// Register installs r as the swarm registry, replacing whatever was there.
// Call it from an init().
func Register(name string, r Registry) { slot.Register(name, r) }

// Get returns the registry in force.
func Get() Registry { return slot.Get() }

// Active names the registry in force, for startup logging.
func Active() string { return slot.Name() }

func init() { Register("local", &local{}) }

// local resolves only the swarm this controller runs in.
type local struct {
	once    sync.Once
	backend charts.Backend
	err     error
}

// Backend returns the ambient-context backend. charts.NewDockerBackend("")
// resolves the Docker context the process was started with, which for an
// in-swarm controller is the mounted docker.sock.
func (l *local) Backend(_ context.Context, swarm string) (charts.Backend, error) {
	if swarm != "" {
		return nil, fmt.Errorf("unknown swarm %q: this build resolves only the swarm the controller runs in", swarm)
	}
	// The client is built once and reused: it holds a connection pool, and the
	// reconcile loop asks on every tick for every application.
	l.once.Do(func() {
		var cli *client.Client
		cli, l.err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if l.err != nil {
			l.err = fmt.Errorf("connecting to the local docker daemon: %w", l.err)
			return
		}
		l.backend = backend.New(cli, backend.Options{})
	})
	return l.backend, l.err
}
