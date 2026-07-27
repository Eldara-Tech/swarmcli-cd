// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import "time"

// ControllerStatus is the controller itself as last observed, as distinct from
// the applications it reconciles.
//
// It exists because the app set became something that can fail on its own: once
// the set is pulled from git rather than mounted at deploy time, "every
// application looks fine" and "the controller has been refusing every commit
// for an hour" are both true at the same time, and nothing in the per-application
// views can say the second. This is where that is said.
//
// Applications counts what is actually being reconciled, which is not
// necessarily what the last-loaded file declares: an application the set added
// but that could not be started is in the file and not in this count.
type ControllerStatus struct {
	AppSet       AppSetStatus `json:"appSet"`
	Applications int          `json:"applications"`
}

// AppSetStatus is where the set of applications came from and how that is
// going.
type AppSetStatus struct {
	// Mode is how the set is sourced, as the deployment configured it —
	// "static", "git" or "path". It is a label passed in rather than something
	// the source infers about itself, so the bootstrap's own vocabulary is what
	// an operator sees reflected back.
	Mode string `json:"mode"`

	// Revision is the commit the running set was loaded from. Empty when the
	// set does not come from a repository.
	Revision string `json:"revision,omitempty"`

	// LoadedAt is when the running set last loaded successfully — not when it
	// was last checked. A load that changed nothing does not move it, so the
	// pair with Error answers "how old is what I am running".
	LoadedAt time.Time `json:"loadedAt"`

	// Error is why the last attempt failed: a load that was refused, or the
	// applications a diff could not apply. Cleared by the next attempt that
	// succeeds.
	Error string `json:"error,omitempty"`

	// Stale reports that the running set is a last-good one and a newer version
	// is being refused. It is the field a UI colours; Error is what it shows
	// beside it. An error with Stale false is a set that loaded but could not be
	// fully applied, which is a different problem.
	Stale bool `json:"stale"`

	// Orphaned names applications that left the set. Their loops are stopped and
	// their stacks are still deployed and no longer reconciled by anyone —
	// reported rather than removed, which is what pruning them is for. In
	// memory only: a restart forgets them, because nothing here has a store.
	Orphaned []string `json:"orphaned,omitempty"`
}
