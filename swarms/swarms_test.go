// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package swarms

import (
	"context"
	"strings"
	"testing"

	"github.com/Eldara-Tech/swarmcli/charts"
)

func TestDefaultIsRegistered(t *testing.T) {
	if got := Active(); got != "local" {
		t.Errorf("Active = %q, want local", got)
	}
	if Get() == nil {
		t.Error("Get returned nil")
	}
}

// The empty name is the swarm the controller runs in, and the same backend
// comes back every time: the reconcile loop asks on every tick.
func TestLocalResolvesTheAmbientSwarmOnce(t *testing.T) {
	l := &local{}

	first, err := l.Backend(context.Background(), "")
	if err != nil {
		t.Fatalf("Backend = %v, want nil", err)
	}
	if first == nil {
		t.Fatal("Backend returned nil")
	}

	second, err := l.Backend(context.Background(), "")
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
func TestLocalRejectsNamedSwarms(t *testing.T) {
	_, err := (&local{}).Backend(context.Background(), "production")
	if err == nil {
		t.Fatal("Backend = nil, want an error for a named swarm")
	}
	if !strings.Contains(err.Error(), "production") {
		t.Errorf("error %q does not name the swarm asked for", err)
	}
}

func TestRegisterReplaces(t *testing.T) {
	original, originalName := Get(), Active()
	t.Cleanup(func() { Register(originalName, original) })

	Register("companion", stubRegistry{})

	if Active() != "companion" {
		t.Errorf("Active = %q, want companion", Active())
	}
	if _, err := Get().Backend(context.Background(), "production"); err != nil {
		t.Errorf("the companion should resolve a named swarm, got %v", err)
	}
}

// stubRegistry stands in for the multi-swarm registry Phase 3 adds.
type stubRegistry struct{}

func (stubRegistry) Backend(context.Context, string) (charts.Backend, error) {
	return charts.NewDockerBackend(""), nil
}
