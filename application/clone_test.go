// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import (
	"reflect"
	"testing"
	"time"
)

// The clones must share no memory with their originals.
//
// The value under test is populated by reflection rather than by hand, and that
// is the whole point. The first version of this test built a fixture by hand and
// said so — "built by hand rather than by a helper that would drift with the
// type" — and Spec.Allow, five slices, was added the same day and went
// unchecked. A hand-written fixture only covers the fields somebody remembered
// to write into it, which is exactly the failure mode a guard against rot must
// not have. Filling by reflection means a field added later is populated,
// walked, and caught without anyone touching this file.
func TestCloneSharesNothingWithItsOriginal(t *testing.T) {
	t.Run("Status", func(t *testing.T) {
		original := filled[Status](t)
		assertClone(t, original, original.Clone(), "Status")
	})

	t.Run("Spec", func(t *testing.T) {
		original := filled[Spec](t)
		assertClone(t, original, original.Clone(), "Spec")
	})
}

func assertClone[T any](t *testing.T, original, clone T, name string) {
	t.Helper()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("the clone is not equal to its original:\n got %+v\nwant %+v", clone, original)
	}
	assertDisjoint(t, reflect.ValueOf(original), reflect.ValueOf(clone), name)
}

// filled returns a T with every reference-typed field reachable from it
// populated, so the walk below has something to compare at each one. An empty
// slice or a nil pointer is indistinguishable between a deep copy and a shallow
// one, so a field left zero is a field the walk cannot check.
func filled[T any](t *testing.T) T {
	t.Helper()
	var v T
	fill(reflect.ValueOf(&v).Elem(), map[reflect.Type]bool{})
	return v
}

// fill populates v's reference-typed fields, recursively.
//
// seen holds the types on the current path, so a type that contains itself —
// none does today — terminates instead of recursing for ever.
func fill(v reflect.Value, seen map[reflect.Type]bool) {
	if v.Type() == timeType || seen[v.Type()] {
		return
	}
	seen[v.Type()] = true
	defer delete(seen, v.Type())

	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fill(v.Elem(), seen)

	case reflect.Slice:
		if v.Len() == 0 {
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		}
		for i := range v.Len() {
			fill(v.Index(i), seen)
		}

	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Field(i); f.CanSet() {
				fill(f, seen)
			}
		}
	}
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
			// Unreachable while fill runs first, and left as a guard rather than
			// an assertion because a nil on one side only is a difference the
			// equality check above has already caught.
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone points at the same value as the original", path)
			return
		}
		assertDisjoint(t, a.Elem(), b.Elem(), path+".*")

	case reflect.Slice:
		if a.IsNil() || a.Len() == 0 {
			// fill populates every slice, so an empty one here means the walk
			// and the filler disagree about the shape — and a slice nobody
			// populated is a slice nobody is checking.
			t.Errorf("%s: empty, so this field is not actually being checked", path)
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
		// None of these appears in Spec or Status today. Failing rather than
		// passing silently is the point: one added later needs a decision in
		// Clone and in fill, not a gap in the walk.
		if !a.IsNil() {
			t.Errorf("%s: %s is not covered by Clone or by this walk", path, a.Kind())
		}
	}
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

// And the case this was written after. An application's allowlist is what the
// controller will let its charts reach outside their own releases, so a caller
// writing through a slice it was handed must not be able to widen it.
func TestWritingToAHandedAllowlistDoesNotWidenTheStore(t *testing.T) {
	stored := Spec{Allow: Allow{Networks: []string{"shared"}}}

	handed := stored.Clone()
	handed.Allow.Networks[0] = "anything"

	if stored.Allow.Networks[0] != "shared" {
		t.Errorf("the stored allowlist became %q; a caller widened it", stored.Allow.Networks[0])
	}
}
