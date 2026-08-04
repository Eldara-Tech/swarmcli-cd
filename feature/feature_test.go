// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package feature

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The free build's answer, which is the one CI ever exercises end to end: the
// community edition, nothing granted, and no licence — not an absent one. A
// licence object with StatusAbsent means a licensed module is linked and has
// nothing installed, which is a different thing to say and a different badge to
// draw.
//
// Read inside the test rather than into a package-level variable, which is what
// extension_test.go can do and this cannot: a package's variables are
// initialised before its init() functions, so a var here would capture the slot
// as it was before the default registered. Every test below that registers
// restores in a t.Cleanup, so this sees the default whatever order they run in.
func TestTheDefaultReportsCommunity(t *testing.T) {
	if name := Active(); name != EditionCommunity {
		t.Errorf("Active = %q, want %q", name, EditionCommunity)
	}
	reporter := Get()
	if reporter == nil {
		t.Fatal("nothing is registered by default; a consumer would call a nil Reporter")
	}

	got := reporter.Report(context.Background())
	if got.Edition != EditionCommunity {
		t.Errorf("Edition = %q, want %q", got.Edition, EditionCommunity)
	}
	if got.Licence != nil {
		t.Errorf("Licence = %+v, want nil: this build has no licensed module in it", *got.Licence)
	}
	for _, name := range All() {
		if got.Features[name] {
			t.Errorf("the community build reports %q granted", name)
		}
	}
}

// All is what decides the capability document's key set, so a feature declared
// here and left out of it would be a key the document never carries — and a UI
// hiding a control on a missing key hides it rather than greying it out.
//
// Derived from the source rather than written down: a list kept beside the
// constants is the list that stops being true, and the constant added six
// months from now is exactly the one nobody remembers to add twice.
func TestAllNamesEveryDeclaredFeature(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing this package: %v", err)
	}

	var declared []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					// Only the constants of type Name. Status's five are
					// declared the same way and are not features.
					if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Name" {
						continue
					}
					for _, v := range vs.Values {
						lit, ok := v.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							t.Fatalf("a Name constant is not a string literal; this test cannot read it")
						}
						value, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquoting %s: %v", lit.Value, err)
						}
						declared = append(declared, value)
					}
				}
			}
		}
	}

	// A scan that read no constants would agree with any All at all.
	if len(declared) == 0 {
		t.Fatal("no Name constants found in this package's source; this test stopped checking anything")
	}

	var listed []string
	for _, name := range All() {
		listed = append(listed, string(name))
	}
	slices.Sort(declared)
	slices.Sort(listed)
	if !slices.Equal(declared, listed) {
		t.Errorf("All names %v, but this package declares %v", listed, declared)
	}
}

// stub reports whatever it was built with.
type stub struct{ report Report }

func (s stub) Report(context.Context) Report { return s.report }

// A Slot, so registering replaces. The property that matters is not the
// replacement itself but that it can go back: a licensed reporter whose licence
// lapsed has to be able to report community again, which a List could not
// express — it has no removal, and OR-ing two reports would let one module turn
// on another's feature.
func TestRegisterReplacesTheReporter(t *testing.T) {
	before, name := Get(), Active()
	t.Cleanup(func() { Register(name, before) })

	expires := time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)
	licensed := Report{
		Edition:  "business",
		Features: Set{SSO: true},
		Licence:  &Licence{Tier: "be", Status: StatusGrace, ExpiresAt: &expires},
	}
	Register("licence", stub{report: licensed})

	if Active() != "licence" {
		t.Errorf("Active = %q, want the reporter that registered last", Active())
	}
	got := Get().Report(context.Background())
	if got.Edition != "business" || !got.Features[SSO] || got.Licence == nil || got.Licence.Status != StatusGrace {
		t.Errorf("Report = %+v, want the registered reporter's answer", got)
	}

	// And back, which is the case a List cannot do at all.
	Register(EditionCommunity, community{})
	if got := Get().Report(context.Background()); got.Edition != EditionCommunity || got.Licence != nil {
		t.Errorf("Report after re-registering the default = %+v, want community", got)
	}
}

// A nil Set is a build that grants nothing rather than a reporter that answered
// wrongly, so nothing may require a reporter to build a map.
func TestANilSetReadsFalse(t *testing.T) {
	var s Set
	for _, name := range All() {
		if s[name] {
			t.Errorf("a nil Set reports %q granted", name)
		}
	}
}

// This seam is a report and never an enforcement point, and the doc comment
// saying so is what somebody reads once. This is what fails the day somebody
// does not: a Reporter is supplied by the module a gate would be constraining,
// so a core that gated on it would be asking the licensed module for permission
// to limit it.
//
// api is the only permitted consumer — it serves the report, which is the whole
// purpose. Active is not covered: naming the reporter in a log line or in the
// capability document decides nothing.
func TestGetIsNotCalledOutsideApi(t *testing.T) {
	const self = "github.com/Eldara-Tech/swarmcli-cd/feature"

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}
	allowed := filepath.Join(root, "api")

	fset := token.NewFileSet()
	importers := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			// .git and the in-repo build caches; the UI's dependency tree and
			// build output; mkdocs' output. None hold Go this repository
			// compiles, and all of them are large.
			case strings.HasPrefix(d.Name(), ".") && path != root,
				d.Name() == "node_modules", d.Name() == "dist", d.Name() == "site",
				path == allowed:
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}

		// What this package is called in that file, which an alias may change.
		local := ""
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || p != self {
				continue
			}
			local = "feature"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		if local == "" {
			return nil
		}
		importers++

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Get" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}
				t.Errorf("%s calls %s.Get: this seam is a report, not an enforcement point, "+
					"and a gate on it asks the licensed module for permission to limit it", rel, local)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	// Nothing importing this package at all means the walk found the wrong
	// tree, or the import path changed — either way the scan above agreed with
	// everything. controller imports it for the startup log line.
	if importers == 0 {
		t.Fatal("no file outside api imports this package; this test stopped checking anything")
	}
}
