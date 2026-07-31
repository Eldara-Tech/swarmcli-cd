// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Command swarmcli-cd is the SwarmCLI CD controller: it converges a Docker
// Swarm to the desired state declared in a Git repository, and serves the API
// that everything else observes it through.
//
// There is nothing here but the call and the seam defaults. The entry point
// lives in the importable controller package so that the private swarmcli-cd-be
// companion can build the same binary from a main.go differing only by its
// blank imports — a main package cannot be imported. See docs/extensibility.md.
package main

import (
	"github.com/Eldara-Tech/swarmcli-cd/controller"

	// The OSS swarm registry (D2). It is a blank import rather than a default
	// inside the seam so that swarms stays the contract and nothing importing
	// it links the Docker applier — see swarms/local. Without it swarms.Active
	// reports "none" at startup and every resolve fails with a message saying
	// this import is what is missing.
	_ "github.com/Eldara-Tech/swarmcli-cd/swarms/local"
)

func main() { controller.Main() }
