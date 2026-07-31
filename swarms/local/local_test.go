// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package local

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// Importing this package is what installs the default, so the import doing its
// job is the thing to assert: a build that took it resolves the local swarm,
// and swarms.Active says "local" rather than "none" in the startup line.
func TestImportRegistersTheDefault(t *testing.T) {
	if got := swarms.Active(); got != "local" {
		t.Errorf("swarms.Active = %q, want local", got)
	}
	if swarms.Get() == nil {
		t.Fatal("swarms.Get returned nil")
	}
}

// The empty target is the swarm the controller runs in, and the same backend
// comes back every time: the reconcile loop asks on every tick.
func TestResolvesTheAmbientSwarmOnce(t *testing.T) {
	r := &registry{}

	first, err := r.Backend(context.Background(), swarms.Target{})
	if err != nil {
		t.Fatalf("Backend = %v, want nil", err)
	}
	if first == nil {
		t.Fatal("Backend returned nil")
	}

	second, err := r.Backend(context.Background(), swarms.Target{})
	if err != nil {
		t.Fatalf("Backend = %v, want nil", err)
	}
	if first != second {
		t.Error("Backend reconnected instead of returning the same backend")
	}
}

// Naming another swarm has to fail, and the message has to say what this build
// can do — a controller that silently deployed to the wrong swarm is the
// failure mode this seam exists to prevent.
func TestRejectsNamedSwarms(t *testing.T) {
	_, err := (&registry{}).Backend(context.Background(), swarms.Target{Swarm: "production"})
	if err == nil {
		t.Fatal("Backend = nil, want an error for a named swarm")
	}
	if !strings.Contains(err.Error(), "production") {
		t.Errorf("error %q does not name the swarm asked for", err)
	}
}

// The OSS build cannot enumerate swarms and cannot reach another node, and both
// have to be visible to a caller as "does not implement" rather than as an
// error at the point of use — that assertion is what keeps prune reporting
// honestly instead of claiming a swarm-wide purge (#108).
func TestImplementsNeitherOptionalInterface(t *testing.T) {
	var r swarms.Registry = &registry{}
	if _, ok := r.(swarms.Lister); ok {
		t.Error("the local registry satisfies swarms.Lister; it resolves one swarm and cannot enumerate")
	}
	if _, ok := r.(swarms.NodeReach); ok {
		t.Error("the local registry satisfies swarms.NodeReach; it holds one socket and cannot reach another node")
	}
}

// The case the import-path ordering actually produces. gc initialises unrelated
// sibling packages in import-path order, and "…/swarmcli-cd-be/swarms" sorts
// before "…/swarmcli-cd/swarms/local" because '-' precedes '/' — so a companion
// registers first and a default that registered unconditionally would overwrite
// it, in a build that looks correct and whose only symptom is Active reporting
// "local".
func TestTheDefaultDoesNotOverwriteACompanion(t *testing.T) {
	original, originalName := swarms.Get(), swarms.Active()
	t.Cleanup(func() { swarms.Register(originalName, original) })

	swarms.Register("companion", stubRegistry{})
	register() // as it runs when gc orders this package second

	if got := swarms.Active(); got != "companion" {
		t.Errorf("swarms.Active = %q after the default ran second, want companion", got)
	}
	if _, ok := swarms.Get().(stubRegistry); !ok {
		t.Errorf("swarms.Get returned %T, want the companion's registry", swarms.Get())
	}
}

// And the other order still installs it, or a build with no companion resolves
// nothing at all.
func TestTheDefaultInstallsWhenNothingElseHas(t *testing.T) {
	original, originalName := swarms.Get(), swarms.Active()
	t.Cleanup(func() { swarms.Register(originalName, original) })

	swarms.Register("none", unregistered{})
	register()

	if got := swarms.Active(); got != "local" {
		t.Errorf("swarms.Active = %q with nothing else registered, want local", got)
	}
}

// stubRegistry stands in for the companion's multi-swarm registry.
type stubRegistry struct{}

func (stubRegistry) Backend(context.Context, swarms.Target) (charts.Backend, error) {
	return nil, nil
}

// unregistered stands in for the seam's own "none", which is what Active
// reports before anything has registered.
type unregistered struct{}

func (unregistered) Backend(context.Context, swarms.Target) (charts.Backend, error) {
	return nil, errors.New("nothing registered")
}
