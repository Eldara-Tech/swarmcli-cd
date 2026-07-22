// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Command swarmcli-cd is the SwarmCLI CD controller: it converges a Docker
// Swarm to the desired state declared in a Git repository, and serves the API
// that everything else observes it through.
//
// There is nothing here but the call. The entry point lives in the importable
// controller package so that the private swarmcli-cd-be companion can build the
// same binary from a main.go differing only by its blank imports — a main
// package cannot be imported. See docs/extensibility.md.
package main

import "github.com/Eldara-Tech/swarmcli-cd/controller"

func main() { controller.Main() }
