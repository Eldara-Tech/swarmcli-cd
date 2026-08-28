// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import (
	"testing"
	"time"
)

// Shape is the Go half of a rule the UI states in web/ui/src/api/appset.ts, and
// the two have to agree: three of these five are different failures, and one is
// not a failure at all. `unwired` has to be checked first — a zero
// ControllerStatus has a zero LoadedAt too, so any other order reads a
// controller with no reconcile loop as one whose set has never loaded.
func TestControllerStatusShape(t *testing.T) {
	loaded := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status ControllerStatus
		want   AppSetShape
	}{
		{"no controller at all", ControllerStatus{}, AppSetUnwired},
		{
			"a mode, but nothing ever loaded",
			ControllerStatus{AppSet: AppSetStatus{Mode: "git", Error: "connection refused"}},
			AppSetNeverLoaded,
		},
		{
			// The distinction never-loaded turns on: a set that loaded and
			// legitimately declares nothing has a LoadedAt, and a controller
			// mid-first-load has neither.
			"loaded, and declares nothing",
			ControllerStatus{AppSet: AppSetStatus{Mode: "git", LoadedAt: loaded}},
			AppSetOK,
		},
		{
			"a newer set being refused",
			ControllerStatus{AppSet: AppSetStatus{Mode: "git", LoadedAt: loaded, Stale: true, Error: "dup"}, Applications: 2},
			AppSetStale,
		},
		{
			"loaded, but part of it did not land",
			ControllerStatus{AppSet: AppSetStatus{Mode: "git", LoadedAt: loaded, Error: "edge: render failed"}, Applications: 2},
			AppSetPartial,
		},
		{
			"healthy",
			ControllerStatus{AppSet: AppSetStatus{Mode: "static", LoadedAt: loaded}, Applications: 2},
			AppSetOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.status.Shape(); got != tc.want {
				t.Errorf("Shape() = %q, want %q", got, tc.want)
			}
		})
	}
}
