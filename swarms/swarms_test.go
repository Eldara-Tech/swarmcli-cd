// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package swarms

import (
	"context"
	"strings"
	"testing"

	"github.com/Eldara-Tech/swarmcli/v2/charts"
)

// This package is the contract, not an implementation: the OSS default lives in
// swarms/local and arrives by blank import, so a build that has not taken it
// resolves nothing. What must not happen is that failing as a nil dereference
// somewhere inside a reconcile goroutine.
func TestUnconfiguredIsRegistered(t *testing.T) {
	if got := Active(); got != "none" {
		t.Errorf("Active = %q, want none", got)
	}
	if Get() == nil {
		t.Fatal("Get returned nil")
	}

	_, err := Get().Backend(context.Background(), Target{})
	if err == nil {
		t.Fatal("Backend = nil, want an error naming the missing registration")
	}
	// The message has to name the import, because the mistake is a missing
	// blank import in a main package and nothing else in the failure says so.
	if !strings.Contains(err.Error(), "swarmcli-cd/swarms/local") {
		t.Errorf("error %q does not name the import that is missing", err)
	}
}

func TestRegisterReplaces(t *testing.T) {
	original, originalName := Get(), Active()
	t.Cleanup(func() { Register(originalName, original) })

	Register("companion", stubRegistry{})

	if Active() != "companion" {
		t.Errorf("Active = %q, want companion", Active())
	}
	if _, err := Get().Backend(context.Background(), Target{Swarm: "production"}); err != nil {
		t.Errorf("the companion should resolve a named swarm, got %v", err)
	}
}

// The two capabilities beyond Registry are optional interfaces, so a registry
// implementing only Registry must not accidentally satisfy them — that is what
// lets a caller degrade rather than fail.
func TestPlainRegistryImplementsNeitherOptionalInterface(t *testing.T) {
	var r Registry = stubRegistry{}
	if _, ok := r.(Lister); ok {
		t.Error("a plain Registry satisfies Lister")
	}
	if _, ok := r.(NodeReach); ok {
		t.Error("a plain Registry satisfies NodeReach")
	}
}

// And a registry that does implement them is recognised through the same
// assertion the callers make.
func TestCapableRegistryIsRecognised(t *testing.T) {
	var r Registry = capableRegistry{}
	if _, ok := r.(Lister); !ok {
		t.Error("a registry with Swarms does not satisfy Lister")
	}
	if _, ok := r.(NodeReach); !ok {
		t.Error("a registry with Nodes and NodeBackend does not satisfy NodeReach")
	}
}

// stubRegistry stands in for the multi-swarm registry Phase 3 adds.
type stubRegistry struct{}

func (stubRegistry) Backend(context.Context, Target) (charts.Backend, error) {
	return charts.NewDockerBackend(""), nil
}

// capableRegistry is a stubRegistry that has also solved enumeration and node
// reach, which is what the companion is expected to look like.
type capableRegistry struct{ stubRegistry }

func (capableRegistry) Swarms(context.Context) ([]Target, error) { return []Target{{}}, nil }

func (capableRegistry) Nodes(context.Context, Target) ([]Node, error) {
	return []Node{{ID: "n1", Hostname: "manager-1"}}, nil
}

func (capableRegistry) NodeBackend(context.Context, Target, Node) (NodeBackend, error) {
	return nil, nil
}
