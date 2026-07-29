// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"

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
