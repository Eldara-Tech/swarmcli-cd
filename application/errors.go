// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import "errors"

// The sentinels a reconciler answers with that are not failures, and that
// the API turns into a particular response rather than into an error.
//
// They live here, beside the types those responses are built from, rather than
// in the package that produces them. api declares a Reconciler interface
// precisely so that an alternative reconciler can serve the same endpoints, and
// matching on a sentinel exported by the OSS applier would have made that
// interface decorative: any replacement would still have had to import the whole
// of reconcile — go-git, the chart engine, the moby client — for two error
// values. Errors are part of a contract in Go, so a contract stated as an
// interface has to state its errors in the same place.
var (
	// ErrNotPlanned reports that an application has not been reconciled yet, so
	// there is nothing to diff and no releases to read a history for.
	//
	// Not a missing application and not a failure: it exists, and the first
	// reconcile has simply not run. The API answers it with an empty result and
	// a flag saying so, which a UI renders as an empty panel rather than as an
	// error.
	ErrNotPlanned = errors.New("no plan yet")

	// ErrSyncPending reports that a manual sync was not started because one is
	// already running with another already queued behind it.
	//
	// Not an error the caller can do anything about, and deliberately not a
	// failure: the queued sync will read the same repository and deploy the same
	// state, so the request has been honoured by the time it matters. It exists
	// so the API can say which of the two happened rather than claiming to have
	// started something it did not.
	ErrSyncPending = errors.New("a sync is already queued for this application")

	// ErrUnsupported reports that the reconciler could reach the destination
	// and the destination cannot answer the question — not that the request
	// was wrong, and not that the read failed.
	//
	// It exists because an optional capability is resolved per request rather
	// than at construction. api reaches the swarm node roster through a
	// NodeLister the reconciler either is or is not, which is a compile-time
	// fact; but which backend serves a destination is a run-time one, and a
	// reconciler that implements the method still cannot answer for a backend
	// that does not implement the capability behind it. Without a sentinel that
	// case arrives as a failure, and an operator is told the roster could not
	// be read when nothing was ever going to read it.
	//
	// The API answers it exactly as it answers a reconciler that is not a
	// NodeLister at all: 501, and the same sentence. The two are the same fact
	// from the caller's side — this build does not report that — and a client
	// branching on the status must not have to tell them apart.
	ErrUnsupported = errors.New("this destination cannot answer that")
)
