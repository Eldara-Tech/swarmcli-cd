// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package extension

import (
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

// fake is an Extension that returns what it was built with. The seam itself
// does nothing with the routes — the core does — so a fake here needs no
// behaviour beyond being identifiable.
type fake struct {
	routes []Route
	public []Route
}

func (f fake) Routes() []Route       { return f.routes }
func (f fake) PublicRoutes() []Route { return f.public }

func route(pattern string) Route {
	return Route{
		Pattern: pattern,
		Action:  authz.ActionRead,
		Handler: func(http.ResponseWriter, *http.Request, authz.Subject) {},
	}
}

// patterns names what a list of extensions would serve, so that a test can
// assert on identity without comparing func values, which are not comparable.
func patterns(exts []Extension) []string {
	var out []string
	for _, e := range exts {
		for _, rt := range e.Routes() {
			out = append(out, rt.Pattern)
		}
	}
	return out
}

// atStart is the seam as the test binary found it. Package-level variables are
// initialised before any test runs, so this holds the OSS default whatever order
// the tests below execute in — which is the property being asserted: this
// package has no init() and registers nothing.
var atStart = struct {
	extensions []Extension
	names      []string
}{All(), Active()}

// The OSS default is the empty list, and it is the one seam that has no default
// to register: no route is the correct behaviour for a build with no companion
// loaded.
func TestNothingIsRegisteredByDefault(t *testing.T) {
	if len(atStart.extensions) != 0 {
		t.Errorf("All before any registration = %v, want empty", atStart.extensions)
	}
	if len(atStart.names) != 0 {
		t.Errorf("Active before any registration = %v, want empty", atStart.names)
	}
}

// Registering appends, the way notify does and unlike the Slot seams: two
// companions may each add routes, and the second must not remove the first's.
func TestRegisterAppendsRatherThanReplaces(t *testing.T) {
	mark := len(All())
	Register("sso", fake{routes: []Route{route("GET /login")}})
	Register("projects", fake{routes: []Route{route("GET /api/v1/projects")}})

	got := patterns(All()[mark:])
	want := []string{"GET /login", "GET /api/v1/projects"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("All = %v, want both registrations in order: %v", got, want)
	}
}

// Active is what the startup log line reports, so its order has to be the order
// things registered in — an operator reading it is matching names against the
// blank imports in their own main.go.
func TestActiveNamesThemInRegistrationOrder(t *testing.T) {
	mark := len(Active())
	Register("first", fake{})
	Register("second", fake{})

	got := Active()[mark:]
	want := []string{"first", "second"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Active = %v, want %v", got, want)
	}
}

// All returns a copy, so a caller ranging over the result cannot race a late
// registration — and this is a List, so a late registration is a thing that
// happens: it is not settled at the end of init().
func TestAllReturnsACopy(t *testing.T) {
	mark := len(All())
	Register("copy", fake{routes: []Route{route("GET /copy")}})

	got := All()
	got[mark] = fake{routes: []Route{route("GET /tampered")}}

	if p := patterns(All()[mark:]); len(p) == 0 || p[0] != "GET /copy" {
		t.Errorf("mutating the result of All changed the seam: %v", p)
	}
}

func TestConcurrentRegisterAndAll(t *testing.T) {
	mark := len(All())

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() { defer wg.Done(); Register("x", fake{}) }()
		go func() { defer wg.Done(); _, _ = All(), Active() }()
	}
	wg.Wait()

	if got := len(All()) - mark; got != 50 {
		t.Errorf("registered 50 extensions, All grew by %d", got)
	}
	if got := len(Active()) - mark; got != 50 {
		t.Errorf("registered 50 extensions, Active grew by %d", got)
	}
}

// api imports this package, so this package must not import api — the cycle a
// companion author creates by reaching for api's own write and fail helpers
// instead of writing four lines of JSON. Handler exists in the shape it does so
// that no import is needed to meet api's guarded type.
//
// Derived from the source rather than written down, for the reason
// api.registeredRoutes gives: a list kept beside the code is the list that stops
// being true.
func TestExtensionDoesNotImportApi(t *testing.T) {
	const forbidden = "github.com/Eldara-Tech/swarmcli-cd/api"

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing this package: %v", err)
	}

	parsed := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			parsed++
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: unquoting %s: %v", name, imp.Path.Value, err)
				}
				if path == forbidden {
					t.Errorf("%s imports %s, which imports this package", name, forbidden)
				}
			}
		}
	}
	// A parse that read nothing passes vacuously, which is the one way this test
	// could stop checking anything without saying so.
	if parsed == 0 {
		t.Fatal("no source files parsed; this test stopped checking anything")
	}
}
