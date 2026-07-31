// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package reclaim deletes the on-disk caches an application leaves behind when
// it stops being reconciled: its clone under <data>/repos/<name> and its chart
// repository cache under <data>/charts/<name>.
//
// Nothing else did. Reconciler.Remove drops the map entry, the credential and
// the order entry, and the two directories stay for ever — so app-of-apps churn,
// a preview environment per pull request being the obvious case, leaves one full
// clone plus one chart cache per application name *ever* declared. The data
// directory is a local volume on a manager node, sharing a filesystem with
// /var/lib/docker and the raft log, which is what makes filling it worse than it
// sounds: a git fetch that fails for every application is recoverable, and a
// raft log that can no longer be written is not.
//
// # Why this is a sweep and not part of Remove
//
// A removal races an in-flight sync of the same application. Remove cancels that
// application's loop and waits for the goroutine, but a sync the API started
// runs on the request's context and is neither cancelled nor waited for
// (swarmcli-cd#106) — so at the moment Remove returns, something may still be
// reading the checkout. Deleting a working tree out from under a render does not
// fail cleanly; it renders half a repository and deploys the result.
//
// So the decision is never taken at the moment of removal. A directory is
// deleted only once its name has been missing from the set for a whole sweep
// interval: the first sweep that misses it records it, only a later one removes
// it, and a name that comes back in between clears the record. That is what makes
// this safe while #106 is still open, rather than safe on the condition that it
// is fixed.
//
// # Why it needs no opt-in
//
// Everything here is a cache. An application whose clone was reclaimed and that
// returns to the set is re-cloned on its first fetch and its chart repositories
// re-fetched, so the cost of being wrong is one slow reconcile. Package prune
// deletes what is deployed and is opt-in for exactly the reason this is not.
package reclaim

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// Options configures a Sweeper.
type Options struct {
	// Roots are the directories holding one subdirectory per application. The
	// controller passes <data>/repos and <data>/charts; the app set's own clone
	// lives under neither, which is what keeps the bootstrap tier out of reach
	// of a sweep driven by the set it bootstraps.
	Roots []string
	Log   *slog.Logger
}

// Sweeper deletes the per-application directories under a set of roots.
//
// Not safe for concurrent use, and it does not need to be: the app-set loop is
// its only caller and calls it from its own goroutine, on the same pass that
// decides which applications there are.
type Sweeper struct {
	roots []string
	log   *slog.Logger
	// absent holds the paths that were already candidates on the previous sweep.
	// It is the whole of the grace period — a name has to be missing twice — so
	// a sweep can never delete a directory it has only just noticed.
	absent map[string]struct{}
}

// New returns a Sweeper over roots.
func New(o Options) *Sweeper {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Sweeper{roots: o.Roots, log: o.Log, absent: map[string]struct{}{}}
}

// Sweep deletes the directories under each root whose name is absent from keep,
// and was absent from it on the previous sweep too.
//
// A root that does not exist yet is not an error: the controller creates both at
// startup, so an absent one is a run that has cached nothing under it.
//
// Failures are collected rather than returned at the first, for the reason the
// prune sweep collects them: one directory that will not go is no reason to keep
// every later one. A failed removal stays a candidate, so the next sweep tries
// again immediately rather than starting its grace period over.
func (s *Sweeper) Sweep(keep []string) error {
	wanted := make(map[string]struct{}, len(keep))
	for _, name := range keep {
		wanted[name] = struct{}{}
	}

	candidates := make(map[string]struct{})
	var errs []error
	for _, root := range s.roots {
		entries, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("reading %s: %w", root, err))
			continue
		}

		for _, entry := range entries {
			// Directories only. Everything this reclaims was created as one, and
			// deleting what it does not recognise is not a sweep's job — a
			// symlink reports false here for the same reason.
			if !entry.IsDir() {
				continue
			}
			if _, ok := wanted[entry.Name()]; ok {
				continue
			}

			path := filepath.Join(root, entry.Name())
			if _, seen := s.absent[path]; !seen {
				// The first sweep to miss it: recorded, not removed.
				candidates[path] = struct{}{}
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, fmt.Errorf("reclaiming %s: %w", path, err))
				candidates[path] = struct{}{}
				continue
			}
			s.log.Info("reclaimed the cache of an application that has left the set",
				"application", entry.Name(), "path", path)
		}
	}

	// Replaced rather than merged: a name that came back must start its grace
	// period again, and one whose directory has gone must not be remembered for
	// ever.
	s.absent = candidates
	return errors.Join(errs...)
}
