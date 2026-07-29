// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/swarm"

	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
)

// The two halves of a live drift comparison, exported only because the caller
// that needs them cannot reach the daemon.
//
// The reconciler holds a charts.Backend, which carries no Docker client and no
// way to read a ServiceSpec — its StackServices returns charts.ServiceState, a
// display projection with a running count and no spec in it. Both methods here
// are one line over something that already exists; what they add is b.api. The
// reconciler reaches them through an optional-interface upgrade, the same way
// it reaches WithRegistryAuth, so a backend that cannot answer (a Phase 3
// remote one) simply does not implement them.
//
// Comparing the two is deliberately not here. That part needs no daemon, and
// keeping it in package drift is what lets it be tested exhaustively against
// hand-built spec pairs — which is the only way the normalisation rules can be
// got right.

// DesiredServices converts a rendered manifest to the Swarm specs it declares.
//
// It is the same conversion DeployStack does before applying, so what drift
// compares against is what a sync would write, rather than a second reading of
// the manifest that could disagree with it.
func (b *Backend) DesiredServices(ctx context.Context, manifest, stack string) (*cdcompose.Stack, error) {
	return cdcompose.Convert(ctx, manifest, stack, b.api)
}

// LiveServices returns the stack's running services with their full specs, by
// scoped name.
//
// Named for what it returns rather than StackServices, which is taken by the
// charts.Backend method above it and answers a different question.
func (b *Backend) LiveServices(ctx context.Context, stack string) (map[string]swarm.Service, error) {
	return b.stackServices(ctx, stack)
}

// RemoveService deletes one service by id.
//
// By id and not by name, because the caller has already read the service it
// means: resolving a name again here would open a window in which the name
// pointed at something else. A service already gone is not an error — a sweep
// that raced another deletion has still got the outcome it wanted.
//
// It is the whole of the applier's delete surface, and deliberately narrow.
// RemoveStack takes a namespace and removes everything under it; this takes one
// service the caller has proved it may delete, and the proof lives in the
// reconciler rather than here.
func (b *Backend) RemoveService(ctx context.Context, id string) error {
	if err := b.api.ServiceRemove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing service %q: %w", id, err)
	}
	return nil
}
