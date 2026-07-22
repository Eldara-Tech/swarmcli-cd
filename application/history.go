// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

// History is one application's release history: every release it declares, with
// the revisions recorded for each.
//
// It is served by its own endpoint rather than carried on Status for the same
// reason ReleaseDiff is — a list view rendering twenty applications must not
// drag their histories along.
type History struct {
	Releases []ReleaseHistory `json:"releases"`
}

// ReleaseHistory is one release's revisions, newest first.
//
// A release the repository declares but that has never been deployed has an
// empty Revisions rather than being absent: a history view shows the release
// with nothing under it, which is the honest answer and a different one from
// "no such release".
type ReleaseHistory struct {
	Name      string     `json:"name"`
	Revisions []Revision `json:"revisions"`
}

// Revision is one recorded revision of one release.
//
// Deliberately not charts.Release. That type carries the rendered manifest and
// the merged values of every revision, and a history response covering six
// releases at ten revisions each would then be sixty manifests — for a view
// that renders a table of numbers, dates and chart versions. The manifest of a
// specific revision is a different request, if it is ever wanted.
type Revision struct {
	Revision int    `json:"revision"`
	Chart    string `json:"chart"`
	Version  string `json:"version"`
	// Status is the engine's derived status: the highest revision keeps its
	// stored status and every lower deployed one reads "superseded".
	Status string `json:"status"`
	// Created is RFC3339, as the engine recorded it.
	Created string `json:"created,omitempty"`
	// Owner is the stamp naming what produced the revision, empty when
	// unclaimed. A history view showing a revision this controller did not
	// install is telling the operator something worth knowing.
	Owner string `json:"owner,omitempty"`
}
