// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reclaim

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// roots builds a repos/charts pair with one directory per name in each, the way
// the controller lays its data directory out.
func roots(t *testing.T, names ...string) (repos, charts string) {
	t.Helper()
	dir := t.TempDir()
	repos, charts = filepath.Join(dir, "repos"), filepath.Join(dir, "charts")
	for _, root := range []string{repos, charts} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			// A file inside, so a test can tell an emptied directory from a
			// removed one.
			if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, name, "HEAD"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return repos, charts
}

func newTest(t *testing.T, roots ...string) *Sweeper {
	t.Helper()
	return New(Options{Roots: roots, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func exists(t *testing.T, path ...string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(path...))
	return err == nil
}

// The grace period is the whole of the safety argument: a name missing once is
// recorded and a name missing twice is removed, so nothing can be deleted within
// a sweep interval of the removal that a sync may still be racing (#106).
func TestSweepDeletesOnlyAfterAWholeInterval(t *testing.T) {
	repos, charts := roots(t, "edge", "preview-42")
	s := newTest(t, repos, charts)

	if err := s.Sweep([]string{"edge"}); err != nil {
		t.Fatalf("first Sweep = %v, want nil", err)
	}
	if !exists(t, repos, "preview-42") || !exists(t, charts, "preview-42") {
		t.Fatal("a departed application was reclaimed by the sweep that first noticed it")
	}

	if err := s.Sweep([]string{"edge"}); err != nil {
		t.Fatalf("second Sweep = %v, want nil", err)
	}
	if exists(t, repos, "preview-42") {
		t.Error("the clone of a departed application was not reclaimed")
	}
	if exists(t, charts, "preview-42") {
		t.Error("the chart cache of a departed application was not reclaimed")
	}
}

// The other half, and the one that matters more: what is still declared is never
// touched, however many times the sweep runs.
func TestSweepKeepsWhatIsStillInTheSet(t *testing.T) {
	repos, charts := roots(t, "edge", "core")
	s := newTest(t, repos, charts)

	for range 5 {
		if err := s.Sweep([]string{"edge", "core"}); err != nil {
			t.Fatalf("Sweep = %v, want nil", err)
		}
	}

	for _, name := range []string{"edge", "core"} {
		for _, root := range []string{repos, charts} {
			if !exists(t, root, name, "HEAD") {
				t.Errorf("%s under %s was reclaimed while still in the set", name, root)
			}
		}
	}
}

// A name that leaves and comes back within the grace period starts it again.
// Without this a removal followed by a re-add — which is how a rename, or a
// failed then successful load, arrives — would delete the clone of an
// application that is reconciling.
func TestSweepGraceRestartsWhenANameReturns(t *testing.T) {
	repos, _ := roots(t, "edge", "preview-42")
	s := newTest(t, repos)

	if err := s.Sweep([]string{"edge"}); err != nil {
		t.Fatalf("Sweep = %v, want nil", err)
	}
	if err := s.Sweep([]string{"edge", "preview-42"}); err != nil {
		t.Fatalf("Sweep = %v, want nil", err)
	}
	if err := s.Sweep([]string{"edge"}); err != nil {
		t.Fatalf("Sweep = %v, want nil", err)
	}

	if !exists(t, repos, "preview-42", "HEAD") {
		t.Error("a name that returned to the set was reclaimed on the next sweep that missed it")
	}
}

// Directories only. A file under a root was not put there by this controller,
// and a sweep that deletes what it does not recognise is a worse bargain than
// one that leaves a stray byte behind.
func TestSweepLeavesFilesAlone(t *testing.T) {
	repos, _ := roots(t)
	if err := os.WriteFile(filepath.Join(repos, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTest(t, repos)

	for range 3 {
		if err := s.Sweep(nil); err != nil {
			t.Fatalf("Sweep = %v, want nil", err)
		}
	}

	if !exists(t, repos, "notes.txt") {
		t.Error("a file under a root was reclaimed")
	}
}

// A root the controller has not created yet — a development run, or the chart
// cache before the first build — is not a failure to report every interval.
func TestSweepIgnoresAnAbsentRoot(t *testing.T) {
	s := newTest(t, filepath.Join(t.TempDir(), "never-created"))
	if err := s.Sweep([]string{"edge"}); err != nil {
		t.Errorf("Sweep = %v, want nil for a root that does not exist", err)
	}
}
