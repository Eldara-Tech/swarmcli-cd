// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package secrets resolves secret material read from an application's source
// tree — a values file committed encrypted, for instance.
//
// It does not supply the controller's own credentials. Git tokens, SSH keys and
// registry authentication arrive as Docker secrets: encrypted at rest in the
// raft log and delivered in memory, which is what swarmcli-rbac-proxy already
// does for its TLS material. Those are configuration, not a seam.
package secrets

import (
	"context"

	"github.com/Eldara-Tech/swarmcli-cd/seam"
)

// Request is one piece of material to resolve.
//
// It is a struct rather than a parameter list so that the seam can grow
// without breaking the companion module that implements it: an implementation
// receives the struct, so a field added later — a reference to fetch rather
// than bytes to decrypt, an application scope — costs it nothing. Widening a
// parameter list would be a breaking change to an interface implemented
// outside this repository.
type Request struct {
	// Path is where the material was read from, relative to the repository
	// root. An implementation may decide by name or extension.
	Path string
	// Data is the material as it was read.
	Data []byte
}

// Provider resolves secret material to plaintext.
//
// A provider that does not recognise the material returns Data unchanged and
// no error, so an unencrypted file passes cleanly through any provider. It
// errors only when it recognises the material and cannot resolve it — a
// corrupt ciphertext, a key it does not hold. Refusing to deploy is the right
// answer there; rendering a stack from a values file that is still ciphertext
// is not.
type Provider interface {
	Resolve(ctx context.Context, req Request) ([]byte, error)
}

var slot seam.Slot[Provider]

// Register installs p as the provider, replacing whatever was there. Call it
// from an init().
func Register(name string, p Provider) { slot.Register(name, p) }

// Get returns the provider in force.
func Get() Provider { return slot.Get() }

// Active names the provider in force, for startup logging.
func Active() string { return slot.Name() }

func init() { Register("plaintext", plaintext{}) }

// plaintext recognises nothing, so everything passes through. That is the
// correct behaviour for a repository whose values files are not encrypted,
// which is every repository until someone encrypts one.
type plaintext struct{}

// Resolve implements Provider.
func (plaintext) Resolve(_ context.Context, req Request) ([]byte, error) { return req.Data, nil }
