// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import (
	"reflect"
	"testing"
	"time"
)

// populated is a Status with every reference-typed field non-nil and non-empty,
// so that a walk over it reaches each one. A field left zero here is a field the
// walk cannot check, which is why this is built by hand rather than by a helper
// that would drift with the type.
func populated() Status {
	return Status{
		Sync: Sync{
			State:    SyncOutOfSync,
			Revision: "abc",
			LastSync: &SyncResult{Revision: "abc", StartedAt: time.Unix(0, 0), Succeeded: true},
		},
		Health: Health{State: HealthDegraded, Services: ServiceCounts{Healthy: 1, Total: 2}},
		Drift:  &Drift{State: DriftStateDetected, Services: 1, Resources: 2},
		Releases: []ReleaseStatus{{
			Name:     "whoami",
			Services: []ServiceStatus{{Name: "whoami_web", Running: 1, Desired: 1}},
			Compat:   &Compat{Status: CompatIncompatible, Required: ">=1.0.0"},
			Drift: &ReleaseDrift{
				State: DriftStateDetected,
				Services: []ServiceDrift{{
					Name:   "whoami_web",
					Fields: []FieldDrift{{Field: "image", Desired: "a", Live: "b"}},
				}},
				Resources: []ResourceDrift{{Kind: ResourceNetwork, Name: "whoami_net"}},
			},
		}},
		ObservedAt: time.Unix(0, 0),
	}
}

// The clone must share no memory with its original. This is the test that keeps
// Clone honest as the type grows: it walks the value rather than checking a
// list of fields somebody has to remember to update, so a pointer or slice
// added later and not cloned fails here rather than years later in whichever
// consumer first mutates one.
func TestStatusCloneSharesNothingWithItsOriginal(t *testing.T) {
	original := populated()
	clone := original.Clone()

	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("the clone is not equal to its original:\n got %+v\nwant %+v", clone, original)
	}
	assertDisjoint(t, reflect.ValueOf(original), reflect.ValueOf(clone), "Status")
}

// timeType is walked past rather than into. A time.Time carries a *Location,
// and every UTC time in the process points at the same one — it is an immutable
// global, so sharing it is neither avoidable nor a hazard. Strings are the same
// case and are skipped by having no reference kind the walk descends into.
var timeType = reflect.TypeOf(time.Time{})

// assertDisjoint fails on any pointer or slice the two values still share.
func assertDisjoint(t *testing.T, a, b reflect.Value, path string) {
	t.Helper()

	if a.Type() == timeType {
		return
	}

	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() {
			// A nil on one side and not the other is a difference DeepEqual
			// above has already caught.
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone points at the same value as the original", path)
			return
		}
		assertDisjoint(t, a.Elem(), b.Elem(), path+".*")

	case reflect.Slice:
		if a.IsNil() || a.Len() == 0 {
			return
		}
		if a.UnsafePointer() == b.UnsafePointer() {
			t.Errorf("%s: the clone shares the original's backing array", path)
			return
		}
		for i := range a.Len() {
			assertDisjoint(t, a.Index(i), b.Index(i), path+"[]")
		}

	case reflect.Struct:
		for i := range a.NumField() {
			assertDisjoint(t, a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)
		}

	case reflect.Map, reflect.Chan, reflect.Func:
		// None of these appear in Status today. Failing rather than passing
		// silently is the point: one added later needs a decision here, not a
		// gap in the walk.
		if !a.IsNil() {
			t.Errorf("%s: %s is not covered by Clone or by this walk", path, a.Kind())
		}
	}
}

// The same walk over Spec. It is handed out by the same two calls and carries
// the same shape of hazard: *ChartSource with two slices under it, and
// SyncPolicy's *int.
func TestSpecCloneSharesNothingWithItsOriginal(t *testing.T) {
	max := 5
	original := Spec{
		Name: "edge",
		Source: Source{
			RepoURL: "https://example.com/x.git",
			Chart: &ChartSource{
				Release:      "whoami",
				Values:       []string{"values.yaml", "prod.yaml"},
				Repositories: []RepositorySpec{{Name: "repo", URL: "https://example.com/charts"}},
			},
		},
		SyncPolicy: SyncPolicy{Automated: true, HistoryMax: &max},
	}

	clone := original.Clone()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("the clone is not equal to its original:\n got %+v\nwant %+v", clone, original)
	}
	assertDisjoint(t, reflect.ValueOf(original), reflect.ValueOf(clone), "Spec")
}

// The hazard in the words of the thing that would hit it: a caller sorting what
// it was given must not reorder what every other caller sees.
func TestSortingAViewDoesNotReorderTheStore(t *testing.T) {
	stored := Status{Releases: []ReleaseStatus{{Name: "b"}, {Name: "a"}}}

	handed := stored.Clone()
	handed.Releases[0], handed.Releases[1] = handed.Releases[1], handed.Releases[0]

	if stored.Releases[0].Name != "b" {
		t.Errorf("the stored order became %q; a caller reordered the store", stored.Releases[0].Name)
	}
}
