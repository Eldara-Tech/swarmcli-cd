// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package seam holds the registration mechanism the open-core seams share.
//
// Per D6 the private companion repository replaces a seam's implementation by
// blank-importing a package whose init() registers its own. Go initialises an
// imported package before its importer, so a default registered in the seam
// package's own init() is always in place before a companion's init() runs, and
// the companion always wins. No build tags, no stubbed files in the public tree.
// It is the mechanism swarmcli-be already uses.
//
// That argument holds only while the default lives in the package the companion
// imports. A default in a package of its own — swarms/local, which is there so
// that the seam does not drag the Docker applier in behind it — is an unrelated
// sibling of the companion's, and gc initialises siblings in import-path order,
// which nothing about the two implementations controls. Such a default must
// register only if nothing has, and swarms/local says why at its own init().
// The rule for anything added here: a default that is not in the seam package
// does not overwrite.
//
// Two shapes cover every seam. Slot holds one implementation and registering
// replaces it: there is exactly one answer to "which swarm registry is in
// force". List holds every implementation and returns all of them: a companion
// adding a Slack notifier must not remove the log notifier or the API's event
// stream.
//
// # When registration is over
//
// The two shapes settle at different times, and consumers may rely on it.
//
// A Slot is settled by the end of init(). Every replacement comes from a blank
// import, so by the time main runs there is nothing left to register, and a
// consumer may read Get once and keep the result — which api.New, reconcile.New
// and prune.New all do.
//
// A List is not. The API server registers itself as a notifier during wiring,
// long after every init() has run, so notify.Dispatch re-reads All on every
// event. A consumer that snapshotted a List at construction would silently drop
// whatever registered after it, and for notify that is the UI's live updates.
//
// # Entitlement gating is not a re-registration
//
// The consequence, for the licence package docs/extensibility.md sketches: an
// implementation whose entitlement lapses must start refusing, from inside
// itself. It must not be swapped out of a Slot at runtime. Half the consumers
// would never see the swap — they hold the old value — and the half that did
// would see it mid-flight: replacing a swarms.Registry under a running
// reconcile hands a half-finished sync a different daemon. The mutexes here
// make registration safe against a concurrent read; they do not make a live
// replacement meaningful.
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
