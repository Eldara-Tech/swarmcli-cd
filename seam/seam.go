// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package seam holds the registration mechanism the open-core seams share.
//
// Per D6 the private companion repository replaces a seam's implementation by
// blank-importing a package whose init() registers its own. Go initialises an
// imported package before its importer, so the OSS default — registered in the
// seam package's own init() — is always in place before a companion's init()
// runs, and the companion always wins. No build tags, no stubbed files in the
// public tree, no ordering hacks. It is the mechanism swarmcli-be already uses.
//
// Two shapes cover every seam. Slot holds one implementation and registering
// replaces it: there is exactly one answer to "which swarm registry is in
// force". List holds every implementation and returns all of them: a companion
// adding a Slack notifier must not remove the log notifier or the API's event
// stream.
package seam

import "sync"

// Slot holds exactly one implementation of T. The zero Slot is ready to use
// and returns the zero T until something registers.
type Slot[T any] struct {
	mu   sync.RWMutex
	name string
	val  T
}

// Register replaces the current implementation. Name is what the controller
// logs at startup, so an operator can tell from the logs whether a companion's
// implementation actually loaded.
func (s *Slot[T]) Register(name string, v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name, s.val = name, v
}

// Get returns the registered implementation.
func (s *Slot[T]) Get() T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.val
}

// Name returns the name of the registered implementation, or "" if nothing has
// registered.
func (s *Slot[T]) Name() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

// List holds every registered implementation of T, in registration order. The
// zero List is ready to use.
type List[T any] struct {
	mu      sync.RWMutex
	names   []string
	entries []T
}

// Register appends an implementation. Unlike Slot.Register it removes nothing.
func (l *List[T]) Register(name string, v T) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
	l.entries = append(l.entries, v)
}

// All returns every registered implementation. The result is a copy, so a
// caller ranging over it cannot race a late registration.
func (l *List[T]) All() []T {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]T(nil), l.entries...)
}

// Names returns the names of every registered implementation, in the same
// order as All.
func (l *List[T]) Names() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]string(nil), l.names...)
}
