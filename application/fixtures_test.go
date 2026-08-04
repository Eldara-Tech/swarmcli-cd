// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This file is the bridge between the wire types above and the TypeScript that
// renders them. It marshals a fully-populated and a zero value of every document
// the API serves into web/ui/src/test/fixtures, and fails when the committed
// files no longer match.
//
// It exists because "the TypeScript types mirror the Go types" is otherwise a
// claim somebody made in a review, and the part of it that goes wrong is not the
// field names — it is optionality. `releases: null` is the diff's *success*
// case; `revisions: null` is a release the plan would install; `loadedAt` has no
// omitempty, so a controller that has never loaded a set reports the zero time
// rather than nothing at all. Each of those is invisible in a hand-written
// mirror and obvious in a generated zero-value fixture.
//
// The full values are built by reflection rather than by hand, for the reason
// clone_test.go's filler gives: a fixture written out by a person only covers
// the fields that person remembered, which is exactly the failure a guard
// against rot must not have. A field added below appears in the fixture, in the
// diff, and in the TypeScript check, without anyone touching this file.

var updateFixtures = flag.Bool("update", false,
	"rewrite the wire fixtures under web/ui/src/test/fixtures")

// fixtureDir is where the browser UI's tests read these from. A path out of the
// package rather than a testdata directory: the fixtures have exactly one
// consumer and it is not Go, so a copy under application/testdata would be a
// second thing to keep current.
const fixtureDir = "../web/ui/src/test/fixtures"

func TestWireFixtures(t *testing.T) {
	// The full and zero value of every document, plus the three payloads whose
	// shape a zero value cannot reach and that the UI is most likely to crash
	// on. Naming them here is what makes them reviewable.
	for _, f := range []struct {
		name  string
		value any
	}{
		{"view-full", populated[View](t)},
		{"view-zero", View{}},
		{"controller-status-full", populated[ControllerStatus](t)},
		// A controller that has never managed to load a set: no mode, no
		// revision, and loadedAt at the zero time rather than absent.
		{"controller-status-zero", ControllerStatus{}},
		{"history-full", populated[History](t)},
		{"history-zero", History{}},
		// The state B4 renders: a release the repository declares and the plan
		// would install, so the engine has no record of it. Revisions is nil and
		// has no omitempty, so it reaches the browser as null — and .map() on
		// null throws.
		{"history-never-deployed", History{Releases: []ReleaseHistory{{Name: "traefik"}}}},
		{"diff-full", populated[DiffResponse](t)},
		// Planned, and nothing would change. The success case, and the null one.
		{"diff-converged", DiffResponse{Planned: true}},
		// Not reconciled yet: an empty list, which is the other thing entirely.
		{"diff-not-planned", DiffResponse{Releases: []ReleaseDiff{}}},
	} {
		t.Run(f.name, func(t *testing.T) {
			// Indented, because these files are read by whoever is deciding
			// whether a TypeScript field should be optional.
			got, err := json.MarshalIndent(f.value, "", "  ")
			if err != nil {
				t.Fatalf("marshalling the fixture: %v", err)
			}
			got = append(got, '\n')

			path := filepath.Join(fixtureDir, f.name+".json")
			if *updateFixtures {
				if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
					t.Fatalf("creating the fixture directory: %v", err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("writing the fixture: %v", err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading the fixture: %v\nrun: go test ./application -run TestWireFixtures -update", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s is out of date with the Go types.\nrun: go test ./application -run TestWireFixtures -update\n got: %s\nwant: %s",
					path, got, want)
			}
		})
	}
}

// wireExamples are the values the filler must not invent one for.
//
// Two kinds. A time or a duration filled with a generic value would either be
// unreadable or — worse, for a golden file — move on every run. An enum filled
// with anything but a member would put a string on the wire that the type does
// not permit, which is precisely what these fixtures exist to catch elsewhere.
//
// A named string type missing from this map is a test failure rather than a
// silently ugly fixture; see populate.
var wireExamples = map[reflect.Type]any{
	reflect.TypeOf(time.Time{}):        time.Date(2026, 7, 22, 9, 41, 10, 0, time.UTC),
	reflect.TypeOf(Duration(0)):        Duration(90 * time.Second),
	reflect.TypeOf(SyncState("")):      SyncOutOfSync,
	reflect.TypeOf(HealthState("")):    HealthDegraded,
	reflect.TypeOf(SyncAction("")):     ActionUpgrade,
	reflect.TypeOf(CompatState("")):    CompatIncompatible,
	reflect.TypeOf(DriftDetection("")): DriftLive,
	reflect.TypeOf(DriftState("")):     DriftStateDetected,
	reflect.TypeOf(DriftReason("")):    DriftModified,
	// The one enum with no unknown fallback: an open string the UI renders
	// verbatim, so that a kind a newer controller reports is not coerced away.
	reflect.TypeOf(ResourceKind("")): ResourceNetwork,
}

// populated returns a T with every field set to something that survives
// omitempty, so that the fixture carries every key the type can produce.
func populated[T any](t *testing.T) T {
	t.Helper()
	var v T
	populate(t, reflect.ValueOf(&v).Elem(), "value", map[reflect.Type]bool{})
	return v
}

// populate fills v, recursively.
//
// name is the JSON name of the field being filled, and is what a string is set
// to. A counter would renumber every value below an inserted field and turn a
// one-field change into a whole-file diff; the field's own name stays put.
//
// seen holds the types on the current path, so a type that contained itself —
// none does today — would terminate rather than recurse for ever.
func populate(t *testing.T, v reflect.Value, name string, seen map[reflect.Type]bool) {
	t.Helper()

	if example, ok := wireExamples[v.Type()]; ok {
		v.Set(reflect.ValueOf(example))
		return
	}
	if seen[v.Type()] {
		return
	}
	seen[v.Type()] = true
	defer delete(seen, v.Type())

	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		populate(t, v.Elem(), name, seen)

	case reflect.Slice:
		// One element, which is all a shape needs: the fixture answers what a
		// member looks like, not how many there can be.
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		populate(t, v.Index(0), name, seen)

	case reflect.String:
		if v.Type() != reflect.TypeOf("") {
			// A named string type is an enum until proved otherwise, and filling
			// one with a field name would write a value the wire contract does
			// not permit into a file that exists to state the contract.
			t.Fatalf("%s is a named string type with no entry in wireExamples: add one naming a member", v.Type())
		}
		v.SetString(name)

	case reflect.Bool:
		v.SetBool(true)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)

	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !v.Field(i).CanSet() {
				continue
			}
			populate(t, v.Field(i), jsonName(f), seen)
		}

	default:
		// A map, a float or an interface added to one of these types needs a
		// decision here and in the TypeScript, not a fixture that quietly leaves
		// the field at its zero value.
		t.Fatalf("populate does not know how to fill a %s (field %q)", v.Kind(), name)
	}
}

// jsonName is the key f marshals under, which is what the fixture's reader sees.
func jsonName(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if tag == "" {
		return f.Name
	}
	return tag
}
