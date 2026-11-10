// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package appset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/config"
	"github.com/Eldara-Tech/swarmcli-cd/git"
)

// The sourcer is what the git mode is specified to reuse, so the contract it
// has to satisfy is asserted at compile time rather than inferred from a test
// that happens to pass one in.
var _ Fetcher = (*git.Sourcer)(nil)

// setPath is where the app set lives in both modes: a path within a tree, not
// the tree's root, so the containment check has something to resolve.
const setPath = "apps/applications.yaml"

// setDir is the directory setPath is in, which is what a loader reading a
// directory of files is pointed at: the shared suite then runs over a directory
// holding that one file.
const setDir = "apps"

const oneApp = `applications:
  - name: edge
    source:
      repoURL: https://example.com/infra.git
      revision: main
      releaseFile: releases/edge.yaml
`

const twoApps = `applications:
  - name: edge
    source:
      repoURL: https://example.com/infra.git
      revision: main
      releaseFile: releases/edge.yaml
  - name: core
    source:
      repoURL: https://example.com/infra.git
      revision: main
      releaseFile: releases/core.yaml
`

// renamedApp is oneApp under a new application name, with the source and the
// release file untouched — the rename of #62, which is a departure and an
// arrival that both point at the same deployed release.
const renamedApp = `applications:
  - name: edge-eu
    source:
      repoURL: https://example.com/infra.git
      revision: main
      releaseFile: releases/edge.yaml
`

// invalidSets are the ways a commit breaks the set, one per rule the loader is
// required to enforce before swapping anything in.
var invalidSets = map[string]string{
	"unknown key": `applications:
  - name: edge
    sauce: https://example.com/infra.git
`,
	"duplicate name": `applications:
  - name: edge
    source:
      repoURL: https://example.com/infra.git
      revision: main
      releaseFile: releases/edge.yaml
  - name: edge
    source:
      repoURL: https://example.com/other.git
      revision: main
      releaseFile: releases/other.yaml
`,
	"invalid source": `applications:
  - name: edge
    source:
      revision: main
      releaseFile: releases/edge.yaml
`,
	// A file emptied, or cut short of its list, without saying so — what a
	// truncation looks like. Each is a validation error, so the running set is
	// kept rather than every application being stopped; `applications: []` is
	// how to declare none.
	"file emptied":          "",
	"applications key lost": "apiVersion: v1\n",
	"applications null":     "applications:\n",
}

// mode is one source implementation under test: a loader, and the ability to
// publish a new app set to whatever it reads from. The swap rule is asserted
// through this rather than twice, because "both modes behave identically" is
// the property that matters.
type mode struct {
	loader *Loader
	// publish makes content the app set the next load will see. Both
	// implementations publish atomically, which is what the path mode requires
	// of its writer and what a git checkout does anyway.
	publish func(content string)
}

// modes builds one of each on demand, so every subtest gets a tree of its own
// and a failure is reported against the subtest that caused it. Each mode runs
// twice, reading one file and reading the directory it is in, because a set
// split across files is held to every rule a single file is.
func modes(initial string) map[string]func(*testing.T) mode {
	return map[string]func(*testing.T) mode{
		"git":        func(t *testing.T) mode { return gitMode(t, initial) },
		"path":       func(t *testing.T) mode { return pathMode(t, initial) },
		"git files":  func(t *testing.T) mode { return gitFilesMode(t, initial) },
		"path files": func(t *testing.T) mode { return pathFilesMode(t, initial) },
	}
}

func gitMode(t *testing.T, initial string) mode {
	t.Helper()
	f := newFixture(t)
	f.commit(map[string]string{setPath: initial})
	sourcer := git.New(t.TempDir(), git.Auth{})
	return mode{
		loader:  NewGit(sourcer, GitConfig{RepoURL: f.dir, Revision: f.branch(), Path: setPath}),
		publish: func(content string) { f.commit(map[string]string{setPath: content}) },
	}
}

// gitFilesMode is gitMode read as a directory of files: the same repository,
// with the loader pointed at the directory the one file is in.
func gitFilesMode(t *testing.T, initial string) mode {
	t.Helper()
	m := gitMode(t, initial)
	r := m.loader.reader.(*gitReader)
	cfg := r.cfg
	cfg.Path, cfg.Files = setDir, true
	m.loader = NewGit(r.fetch, cfg)
	return m
}

func pathMode(t *testing.T, initial string) mode {
	t.Helper()
	dir := t.TempDir()
	write := func(content string) {
		t.Helper()
		// Write-then-rename, which is what the path mode documents as its
		// writer's obligation: the reader sees one whole version or the other.
		tmp := filepath.Join(dir, ".applications.yaml.tmp")
		if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, filepath.FromSlash(setPath))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(setPath))), 0o700); err != nil {
		t.Fatal(err)
	}
	write(initial)
	return mode{loader: NewPath(PathConfig{Dir: dir, Path: setPath}), publish: write}
}

// pathFilesMode is pathMode read as a directory of files: the same tree, with
// the loader pointed at the directory the one file is in.
func pathFilesMode(t *testing.T, initial string) mode {
	t.Helper()
	m := pathMode(t, initial)
	m.loader = NewPath(PathConfig{Dir: currentDir(t, m), Path: setDir, Files: true})
	return m
}

// fixture is a real repository on disk, built through go-git so the tests need
// no git binary and no network. Package git has one of its own; it is
// unexported there, and the app set commits whole trees rather than single
// files, so this is a sibling rather than a shared helper.
type fixture struct {
	t   *testing.T
	dir string
	rep *gogit.Repository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	rep, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	return &fixture{t: t, dir: dir, rep: rep}
}

func (f *fixture) commit(files map[string]string) {
	f.t.Helper()
	tree, err := f.rep.Worktree()
	if err != nil {
		f.t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(f.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			f.t.Fatal(err)
		}
		if _, err := tree.Add(name); err != nil {
			f.t.Fatal(err)
		}
	}
	if _, err := tree.Commit("update", &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0).UTC()},
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) branch() string {
	f.t.Helper()
	head, err := f.rep.Head()
	if err != nil {
		f.t.Fatal(err)
	}
	return head.Name().Short()
}

// names is the application names a load returned, which is what every
// assertion about "which set is current" comes down to.
func names(t *testing.T, apps []application.Spec) []string {
	t.Helper()
	out := make([]string, 0, len(apps))
	for _, app := range apps {
		out = append(out, app.Name)
	}
	return out
}

func TestLoadParsesTheSet(t *testing.T) {
	for name, newMode := range modes(oneApp) {
		t.Run(name, func(t *testing.T) {
			m := newMode(t)

			file, changed, err := m.loader.Load(context.Background())
			if err != nil {
				t.Fatalf("Load = %v, want nil", err)
			}
			if !changed {
				t.Error("the first load reported no change, want changed: there was nothing before it")
			}
			if got := names(t, file.Applications); len(got) != 1 || got[0] != "edge" {
				t.Errorf("applications = %v, want [edge]", got)
			}
			if m.loader.Current() != file {
				t.Error("Current returned a different set from the one Load returned")
			}
		})
	}
}

// An unchanged set must not be re-parsed or re-reported: the loop above this
// diffs whatever comes back, and a change that is not one costs a full pass
// over every application.
func TestLoadReportsUnchanged(t *testing.T) {
	for name, newMode := range modes(oneApp) {
		t.Run(name, func(t *testing.T) {
			m := newMode(t)

			first, _, err := m.loader.Load(context.Background())
			if err != nil {
				t.Fatalf("Load = %v, want nil", err)
			}
			again, changed, err := m.loader.Load(context.Background())
			if err != nil {
				t.Fatalf("second Load = %v, want nil", err)
			}
			if changed {
				t.Error("the second load reported a change, want unchanged: nothing was published")
			}
			if again != first {
				t.Error("the second load returned a different set, want the one already current")
			}
		})
	}
}

func TestLoadPicksUpANewSet(t *testing.T) {
	for name, newMode := range modes(oneApp) {
		t.Run(name, func(t *testing.T) {
			m := newMode(t)
			if _, _, err := m.loader.Load(context.Background()); err != nil {
				t.Fatalf("Load = %v, want nil", err)
			}

			m.publish(twoApps)
			file, changed, err := m.loader.Load(context.Background())
			if err != nil {
				t.Fatalf("Load = %v, want nil", err)
			}
			if !changed {
				t.Error("a published set was not reported as a change")
			}
			if got := names(t, file.Applications); len(got) != 2 {
				t.Errorf("applications = %v, want both", got)
			}
			if m.loader.Current() != file {
				t.Error("Current did not swap to the set Load returned")
			}
		})
	}
}

// An explicitly empty set is a set: it loads and swaps in, in both modes, so the
// last application can be removed by a commit.
func TestAnExplicitlyEmptySetLoads(t *testing.T) {
	for name, newMode := range modes(oneApp) {
		t.Run(name, func(t *testing.T) {
			m := newMode(t)
			if _, _, err := m.loader.Load(context.Background()); err != nil {
				t.Fatalf("Load = %v, want nil", err)
			}

			m.publish("applications: []\n")
			file, changed, err := m.loader.Load(context.Background())
			if err != nil {
				t.Fatalf("Load = %v, want nil", err)
			}
			if !changed || len(file.Applications) != 0 {
				t.Errorf("changed=%v, applications=%v, want an empty set swapped in", changed, names(t, file.Applications))
			}
			if m.loader.Current() != file {
				t.Error("Current did not swap to the empty set")
			}
		})
	}
}

// The rule the whole issue exists for: a bad commit leaves the running set
// running.
func TestInvalidSetKeepsTheLastGood(t *testing.T) {
	for mode, newMode := range modes(oneApp) {
		for name, invalid := range invalidSets {
			t.Run(mode+"/"+name, func(t *testing.T) {
				m := newMode(t)
				good, _, err := m.loader.Load(context.Background())
				if err != nil {
					t.Fatalf("Load = %v, want nil", err)
				}

				m.publish(invalid)
				file, changed, err := m.loader.Load(context.Background())
				if err == nil {
					t.Fatal("Load = nil, want the validation error")
				}
				if file != nil {
					t.Error("a failed load returned a set; it must return none, so a stale one cannot be mistaken for a fresh one")
				}
				if changed {
					t.Error("a failed load reported a change")
				}
				if m.loader.Current() != good {
					t.Error("Current moved off the last good set")
				}

				// And the loader is not wedged: the next good commit lands.
				m.publish(twoApps)
				file, changed, err = m.loader.Load(context.Background())
				if err != nil {
					t.Fatalf("Load after the fix = %v, want nil", err)
				}
				if !changed || len(file.Applications) != 2 {
					t.Errorf("recovered load: changed=%v, applications=%v, want the fixed set", changed, names(t, file.Applications))
				}
			})
		}
	}
}

// Nothing to fall back on is the one case where a caller gets neither a set nor
// a last-good one. It must also keep reporting the error rather than falling
// silent once the bad document has been seen.
func TestFirstLoadInvalidLeavesNoCurrentSet(t *testing.T) {
	for name, newMode := range modes(invalidSets["duplicate name"]) {
		t.Run(name, func(t *testing.T) {
			m := newMode(t)

			for i := range 2 {
				file, changed, err := m.loader.Load(context.Background())
				if err == nil {
					t.Fatalf("load %d = nil, want the validation error every time", i)
				}
				if !strings.Contains(err.Error(), "duplicate application name") {
					t.Errorf("error %q does not name the problem", err)
				}
				if file != nil || changed {
					t.Errorf("load %d returned file=%v changed=%v, want none", i, file, changed)
				}
				if m.loader.Current() != nil {
					t.Error("Current returned a set, but none has ever loaded")
				}
			}
		})
	}
}

// Change is the app set changing, not the branch moving: a commit that does not
// touch the file leaves the desired set exactly as it was, and re-diffing every
// application over an unrelated commit is work with no possible outcome.
func TestGitUnrelatedCommitIsNotAChange(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{setPath: oneApp})
	loader := NewGit(git.New(t.TempDir(), git.Auth{}), GitConfig{RepoURL: f.dir, Revision: f.branch(), Path: setPath})

	first, _, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}

	f.commit(map[string]string{"README.md": "unrelated"})
	again, changed, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if changed {
		t.Error("a commit that does not touch the app-set file was reported as a change")
	}
	if again != first {
		t.Error("the set was replaced by an equal one; the running set should have been kept")
	}
}

// countingFetcher records what the loader asked the sourcer for, and delegates
// to a real one so the assertion is about the call and not about a stub.
type countingFetcher struct {
	inner *git.Sourcer
	keys  []string
}

func (c *countingFetcher) Fetch(ctx context.Context, key string, src application.Source) (git.Checkout, error) {
	c.keys = append(c.keys, key)
	return c.inner.Fetch(ctx, key, src)
}

// The app set is fetched by the sourcer the applications use, under one cache
// key of its own: no second git client, and no clone that could collide with an
// application's.
func TestGitFetchesThroughTheSourcerUnderOneKey(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{setPath: oneApp})

	root := t.TempDir()
	fetcher := &countingFetcher{inner: git.New(root, git.Auth{})}
	loader := NewGit(fetcher, GitConfig{RepoURL: f.dir, Revision: f.branch(), Path: setPath})

	for range 2 {
		if _, _, err := loader.Load(context.Background()); err != nil {
			t.Fatalf("Load = %v, want nil", err)
		}
	}

	if len(fetcher.keys) != 2 {
		t.Errorf("the sourcer was called %d times, want once per load", len(fetcher.keys))
	}
	for _, key := range fetcher.keys {
		if key != cacheKey {
			t.Errorf("fetched under %q, want the reserved key %q", key, cacheKey)
		}
	}
	if _, err := os.Stat(filepath.Join(root, cacheKey)); err != nil {
		t.Errorf("no clone under the cache key: %v", err)
	}
}

// A remote that has gone away is a failed load, not a reconcile against
// nothing: the set that is running stays running.
func TestGitUnreachableRemoteKeepsTheLastGood(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{setPath: oneApp})
	loader := NewGit(git.New(t.TempDir(), git.Auth{}), GitConfig{RepoURL: f.dir, Revision: f.branch(), Path: setPath})

	good, _, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}

	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatal(err)
	}
	file, _, err := loader.Load(context.Background())
	if err == nil {
		t.Fatal("Load = nil, want an error once the remote is gone")
	}
	if !strings.Contains(err.Error(), "fetching the app set") {
		t.Errorf("error %q does not say what failed", err)
	}
	if file != nil || loader.Current() != good {
		t.Error("the last good set was not kept")
	}
}

func TestGitMissingFileIsAFailedLoad(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{setPath: oneApp})
	loader := NewGit(git.New(t.TempDir(), git.Auth{}), GitConfig{RepoURL: f.dir, Revision: f.branch(), Path: "apps/absent.yaml"})

	if _, _, err := loader.Load(context.Background()); err == nil {
		t.Fatal("Load = nil, want an error for a path that is not in the repository")
	}
	if loader.Current() != nil {
		t.Error("Current returned a set for a load that never succeeded")
	}
}

// Which commit broke the set is the first thing an operator asks, so the parse
// error names it.
func TestGitErrorsNameTheCommit(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{setPath: invalidSets["duplicate name"]})
	head, err := f.rep.Head()
	if err != nil {
		t.Fatal(err)
	}
	loader := NewGit(git.New(t.TempDir(), git.Auth{}), GitConfig{RepoURL: f.dir, Revision: f.branch(), Path: setPath})

	_, _, err = loader.Load(context.Background())
	if err == nil {
		t.Fatal("Load = nil, want the validation error")
	}
	if want := setPath + "@" + head.Hash().String(); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
}

// A writer that rewrites the file in place instead of publishing atomically can
// be read mid-write: emptied by the O_TRUNC, or holding half a line. Both are
// failed loads and the running set is kept.
//
// What this cannot catch is a truncation that is still valid YAML and still a
// valid set — cutting this document on a line boundary produces one — which is
// why the writer's atomic write-then-rename is a documented obligation rather
// than something the reader makes up for.
func TestPathTornReadKeepsTheLastGood(t *testing.T) {
	for name, torn := range map[string]string{
		"emptied by the truncate": "",
		"half a line":             "applications:\n  - name: edge\n    source:\n      repoURL: https://example.com/infra.git\n      revis",
		// Valid YAML whose one entry is blank, which the decoder drops: it must
		// not read as `applications: []`.
		"cut after the dash": "applications:\n  - ",
	} {
		t.Run(name, func(t *testing.T) {
			m := pathMode(t, oneApp)
			good, _, err := m.loader.Load(context.Background())
			if err != nil {
				t.Fatalf("Load = %v, want nil", err)
			}

			if err := os.WriteFile(filepath.Join(currentDir(t, m), filepath.FromSlash(setPath)), []byte(torn), 0o600); err != nil {
				t.Fatal(err)
			}

			file, changed, err := m.loader.Load(context.Background())
			if err == nil {
				t.Fatal("Load = nil, want an error for a half-written file")
			}
			if file != nil || changed {
				t.Error("a torn read produced a set")
			}
			if m.loader.Current() != good {
				t.Error("a torn read replaced the running set")
			}
		})
	}
}

func TestPathRemovedFileKeepsTheLastGood(t *testing.T) {
	m := pathMode(t, oneApp)
	good, _, err := m.loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}

	if err := os.Remove(filepath.Join(currentDir(t, m), filepath.FromSlash(setPath))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.loader.Load(context.Background()); err == nil {
		t.Fatal("Load = nil, want an error for a file that is no longer there")
	}
	if m.loader.Current() != good {
		t.Error("a missing file replaced the running set")
	}
}

// Change is decided on content, not on the clock. A writer that preserves
// timestamps — rsync -a, cp -p, several git-sync images — would otherwise
// publish a set that is never picked up, and one that rewrites the same bytes
// would trigger a diff of every application for nothing.
func TestPathChangeIsContentNotTimestamps(t *testing.T) {
	m := pathMode(t, oneApp)
	file := filepath.Join(currentDir(t, m), filepath.FromSlash(setPath))

	first, _, err := m.loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("same bytes, newer timestamp", func(t *testing.T) {
		m.publish(oneApp)
		later := before.ModTime().Add(time.Hour)
		if err := os.Chtimes(file, later, later); err != nil {
			t.Fatal(err)
		}
		again, changed, err := m.loader.Load(context.Background())
		if err != nil {
			t.Fatalf("Load = %v, want nil", err)
		}
		if changed || again != first {
			t.Error("a rewrite of the same set was reported as a change")
		}
	})

	t.Run("new bytes, preserved timestamp", func(t *testing.T) {
		m.publish(twoApps)
		if err := os.Chtimes(file, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
		got, changed, err := m.loader.Load(context.Background())
		if err != nil {
			t.Fatalf("Load = %v, want nil", err)
		}
		if !changed || len(got.Applications) != 2 {
			t.Errorf("changed=%v applications=%v, want the new set picked up despite the timestamp", changed, names(t, got.Applications))
		}
	})
}

// The tree is written by something other than the controller, so a symlink in
// it must not turn an app-set read into a read of whatever the controller can
// reach — which, for a process holding the docker socket, is everything.
func TestReadRefusesAPathLeavingTheTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(outside, []byte(oneApp), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, filepath.FromSlash(setPath))); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"symlink out of the tree": setPath,
		"traversal":               "../secret.yaml",
		"absolute":                outside,
	} {
		t.Run(name, func(t *testing.T) {
			loader := NewPath(PathConfig{Dir: dir, Path: path})
			if _, _, err := loader.Load(context.Background()); err == nil {
				t.Error("Load = nil, want a refusal to read outside the tree")
			}
		})
	}
}

// Load runs on the app-set loop; Current is read by whatever serves the
// controller's status. The race detector is the assertion.
func TestConcurrentLoadAndCurrent(t *testing.T) {
	m := pathMode(t, oneApp)
	if _, _, err := m.loader.Load(context.Background()); err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, _, err := m.loader.Load(context.Background()); err != nil {
					t.Errorf("Load = %v, want nil", err)
					return
				}
				if m.loader.Current() == nil {
					t.Error("Current returned nothing after a successful load")
					return
				}
			}
		}()
		if i == 0 {
			m.publish(twoApps)
		}
	}
	wg.Wait()

	if got := names(t, m.loader.Current().Applications); len(got) != 2 {
		t.Errorf("Current = %v, want the published set", got)
	}
}

// currentDir is the directory a path-mode loader reads from. The mode helper
// closes over it, and a couple of tests need to corrupt what is in it.
func currentDir(t *testing.T, m mode) string {
	t.Helper()
	r, ok := m.loader.reader.(*pathReader)
	if !ok {
		t.Fatalf("loader reads through %T, want the path reader", m.loader.reader)
	}
	return r.cfg.Dir
}

// The app-set file is content the controller did not write, read whole, every
// interval. os.ReadFile sized its buffer from the file, so a four-gigabyte
// applications.yaml exhausted the controller before the decoder saw a byte —
// yaml.v3's alias cap covers the billion-laughs shape of this and nothing else.
func TestAnOversizedAppSetIsRefused(t *testing.T) {
	for name, newMode := range modes(oneApp) {
		t.Run(name, func(t *testing.T) {
			m := newMode(t)
			if _, _, err := m.loader.Load(context.Background()); err != nil {
				t.Fatalf("Load = %v, want the seed set to load", err)
			}

			// Valid YAML throughout, so what refuses it is the bound rather than
			// the parser tripping over something else.
			m.publish(oneApp + strings.Repeat("# padding to the brim\n", (maxAppSetSize/22)+64))

			_, _, err := m.loader.Load(context.Background())
			if err == nil {
				t.Fatal("Load = nil, want an app-set file over the limit refused")
			}
			if !strings.Contains(err.Error(), "larger than the") {
				t.Errorf("error = %v, want it to name the limit", err)
			}
			// Last-good is kept, as it is for any other failed load: the bound is
			// a refusal, not a reason to stop reconciling what already validated.
			if cur := m.loader.Current(); cur == nil || len(cur.Applications) != 1 {
				t.Errorf("current set = %+v, want the last one that validated kept", cur)
			}
		})
	}
}

// And a file up to the limit still loads, so the bound cannot quietly become a
// smaller one than the message names.
func TestAnAppSetAtTheLimitLoads(t *testing.T) {
	m := pathMode(t, oneApp)
	padding := strings.Repeat("#", maxAppSetSize-len(oneApp)-1) + "\n"
	m.publish(oneApp + padding)

	file, _, err := m.loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want a file at the limit accepted", err)
	}
	if len(file.Applications) != 1 {
		t.Errorf("got %d applications, want 1", len(file.Applications))
	}
}

// appFile is an applications file declaring each name, every one with a release
// file of its own.
func appFile(names ...string) string {
	out := "applications:\n"
	for _, name := range names {
		out += "  - name: " + name + "\n    source: {repoURL: https://example.com/infra.git, revision: main, releaseFile: releases/" + name + ".yaml}\n"
	}
	return out
}

// writeFiles writes each file under dir, creating what it needs to.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// filesLoader is a path-mode loader over a directory of app-set files holding
// files, which have been loaded once. It returns the directory, for a test to
// change what is in it, and that first set.
func filesLoader(t *testing.T, files map[string]string) (*Loader, string, *config.File) {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, filepath.Join(root, setDir), files)
	loader := NewPath(PathConfig{Dir: root, Path: setDir, Files: true})
	first, _, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	return loader, filepath.Join(root, setDir), first
}

// The set is the files' applications in filename order, whatever order they
// were written in, so the same directory always loads as the same list.
func TestFilesAreConcatenatedInNameOrder(t *testing.T) {
	root := t.TempDir()
	for _, f := range []struct{ name, content string }{
		{"zulu.yaml", appFile("zulu")},
		{"alpha.yaml", appFile("alpha")},
		{"mike.yml", appFile("mike", "mike-2")},
	} {
		writeFiles(t, filepath.Join(root, setDir), map[string]string{f.name: f.content})
	}

	file, _, err := NewPath(PathConfig{Dir: root, Path: setDir, Files: true}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if got, want := strings.Join(names(t, file.Applications), " "), "alpha mike mike-2 zulu"; got != want {
		t.Errorf("applications = %s, want %s", got, want)
	}
}

// Any file changing is the set changing, a rename included; the same files
// rewritten are not.
func TestFilesChangeIsAnyFileChanging(t *testing.T) {
	start := map[string]string{"a.yaml": appFile("alpha"), "b.yaml": appFile("bravo")}
	for name, tc := range map[string]struct {
		change      func(t *testing.T, dir string)
		wantChanged bool
	}{
		"an edit": {func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"b.yaml": appFile("bravo", "charlie")})
		}, true},
		"an add": {func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"c.yaml": appFile("charlie")})
		}, true},
		"a removal": {func(t *testing.T, dir string) {
			mustRemove(t, filepath.Join(dir, "b.yaml"))
		}, true},
		"a rename": {func(t *testing.T, dir string) {
			mustRename(t, filepath.Join(dir, "b.yaml"), filepath.Join(dir, "c.yaml"))
		}, true},
		"a rewrite of the same bytes": {func(t *testing.T, dir string) {
			writeFiles(t, dir, start)
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			loader, dir, first := filesLoader(t, start)
			tc.change(t, dir)

			file, changed, err := loader.Load(context.Background())
			if err != nil {
				t.Fatalf("Load = %v, want nil", err)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if !tc.wantChanged && file != first {
				t.Error("the set was replaced by an equal one; the running set should have been kept")
			}
		})
	}
}

// Where one file ends and the next one's name begins must not be readable two
// ways. Without the length in the digest these two directories hash alike, and
// the second — which does not even parse — would be taken for the first.
func TestFilesDigestCannotConfuseTwoDirectories(t *testing.T) {
	loader, dir, _ := filesLoader(t, map[string]string{"a.yaml": appFile("alpha"), "b.yaml": appFile("bravo")})

	writeFiles(t, dir, map[string]string{"a.yaml": appFile("alpha") + "b.yaml\x00" + appFile("bravo")})
	mustRemove(t, filepath.Join(dir, "b.yaml"))

	if _, changed, err := loader.Load(context.Background()); err == nil && !changed {
		t.Error("a different directory was read as the unchanged set")
	}
}

// A commit that touches nothing the set is read from is not a change: not a
// file outside the directory, and not one inside it that is not an app-set file.
func TestGitFilesUnrelatedCommitIsNotAChange(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{setDir + "/a.yaml": appFile("alpha"), setDir + "/b.yaml": appFile("bravo")})
	loader := NewGit(git.New(t.TempDir(), git.Auth{}), GitConfig{RepoURL: f.dir, Revision: f.branch(), Path: setDir, Files: true})

	first, _, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}

	f.commit(map[string]string{"README.md": "unrelated", setDir + "/NOTES.md": "unrelated"})
	again, changed, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if changed || again != first {
		t.Error("a commit that touches no app-set file was reported as a change")
	}
}

// The error a set split across files raises names the commit, the file and the
// one it collides with — which of many files to open is the question.
func TestGitFilesErrorsNameTheCommitAndBothFiles(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{setDir + "/team-a.yaml": appFile("edge"), setDir + "/team-b.yaml": appFile("edge")})
	head, err := f.rep.Head()
	if err != nil {
		t.Fatal(err)
	}
	loader := NewGit(git.New(t.TempDir(), git.Auth{}), GitConfig{RepoURL: f.dir, Revision: f.branch(), Path: setDir, Files: true})

	_, _, err = loader.Load(context.Background())
	want := setDir + "@" + head.Hash().String() + ": team-b.yaml: applications[0]: duplicate application name 'edge', also declared in team-a.yaml"
	if err == nil || err.Error() != want {
		t.Errorf("Load = %v, want %q", err, want)
	}
}

// One file failing fails the whole load, and the good files beside it are not
// applied on their own: the set is never partially applied.
func TestFilesOneBadFileKeepsTheLastGood(t *testing.T) {
	loader, dir, good := filesLoader(t, map[string]string{"a.yaml": appFile("alpha"), "b.yaml": appFile("bravo")})

	writeFiles(t, dir, map[string]string{"a.yaml": appFile("alpha", "charlie"), "b.yaml": invalidSets["unknown key"]})
	file, changed, err := loader.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "b.yaml") {
		t.Fatalf("Load = %v, want the error naming b.yaml", err)
	}
	if file != nil || changed {
		t.Error("a failed load returned a set")
	}
	if loader.Current() != good {
		t.Error("Current moved off the last good set")
	}
}

// A directory with nothing to read — emptied, never there, or holding nothing
// that counts — is a failed load, not a set declaring none: an emptied directory
// must not stop every application, and with prune delete them.
func TestFilesWithNothingToReadIsAFailedLoad(t *testing.T) {
	for name, empty := range map[string]func(t *testing.T, dir string){
		"emptied": func(t *testing.T, dir string) {
			mustRemove(t, filepath.Join(dir, "a.yaml"))
		},
		"missing": func(t *testing.T, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		},
		"nothing that counts": func(t *testing.T, dir string) {
			mustRemove(t, filepath.Join(dir, "a.yaml"))
			writeFiles(t, dir, map[string]string{"README.md": "x", ".a.yaml": appFile("alpha")})
		},
	} {
		t.Run(name, func(t *testing.T) {
			loader, dir, good := filesLoader(t, map[string]string{"a.yaml": appFile("alpha")})
			empty(t, dir)

			_, _, err := loader.Load(context.Background())
			if err == nil || !strings.Contains(err.Error(), "no app-set files") || !strings.Contains(err.Error(), "applications: []") {
				t.Fatalf("Load = %v, want the refusal saying how to declare none", err)
			}
			if loader.Current() != good {
				t.Error("Current moved off the last good set")
			}
		})
	}
}

// Files that each declare none are how a directory says none, and that loads.
func TestFilesDeclaringNoneIsAnEmptySet(t *testing.T) {
	loader, dir, _ := filesLoader(t, map[string]string{"a.yaml": appFile("alpha"), "b.yaml": appFile("bravo")})

	writeFiles(t, dir, map[string]string{"a.yaml": "applications: []\n", "b.yaml": "applications: []\n"})
	file, changed, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if !changed || len(file.Applications) != 0 || loader.Current() != file {
		t.Errorf("changed=%v applications=%v, want an empty set swapped in", changed, names(t, file.Applications))
	}
}

// Only .yaml and .yml files directly in the directory are read. What else is
// there is not, so none of it can fail the load.
func TestFilesReadsOnlyAppSetFiles(t *testing.T) {
	_, _, file := filesLoader(t, map[string]string{
		"a.yaml":        appFile("alpha"),
		"b.yml":         appFile("bravo"),
		".hidden.yaml":  "not: [yaml",
		"notes.txt":     "not: [yaml",
		"a.yaml.bak":    "not: [yaml",
		"sub/c.yaml":    "not: [yaml",
		"d.yaml/e.yaml": "not: [yaml",
	})
	if got := strings.Join(names(t, file.Applications), " "); got != "alpha bravo" {
		t.Errorf("applications = %s, want alpha bravo", got)
	}
}

// Every file is held inside the directory, and the directory inside the tree,
// for the reason a single file is.
func TestFilesRefusesAPathLeavingTheTree(t *testing.T) {
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"a.yaml": appFile("alpha")})

	for name, link := range map[string]func(root string){
		"a file linked out": func(root string) {
			writeFiles(t, filepath.Join(root, setDir), map[string]string{"b.yaml": appFile("bravo")})
			mustSymlink(t, filepath.Join(outside, "a.yaml"), filepath.Join(root, setDir, "a.yaml"))
		},
		"the directory linked out": func(root string) {
			mustSymlink(t, outside, filepath.Join(root, setDir))
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			link(root)
			if _, _, err := NewPath(PathConfig{Dir: root, Path: setDir, Files: true}).Load(context.Background()); err == nil || !strings.Contains(err.Error(), "outside") {
				t.Errorf("Load = %v, want a refusal to read outside the tree", err)
			}
		})
	}
}

// The set is what is in its directory. A file linked to somewhere else in the
// same tree is refused too, and said to leave the directory rather than the
// repository, which it does not.
func TestFilesRefusesALinkOutOfTheDirectory(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"shared/edge.yaml": appFile("edge"), setDir + "/core.yaml": appFile("core")})
	mustSymlink(t, filepath.Join("..", "shared", "edge.yaml"), filepath.Join(root, setDir, "edge.yaml"))

	_, _, err := NewPath(PathConfig{Dir: root, Path: setDir, Files: true}).Load(context.Background())
	want := "the app-set file " + filepath.Join(root, setDir, "edge.yaml") + " resolves outside the app-set directory " + filepath.Join(root, setDir)
	if err == nil || err.Error() != want {
		t.Errorf("Load = %v, want %q", err, want)
	}
}

// git-sync publishes by swapping a symlink to the whole directory and then
// deletes the version it swapped away from, so a load that straddles a swap may
// have read a directory that was being deleted under it. It is a failed load,
// not whatever it managed to read, and the next pass reads the new version.
//
// The swap is made to land mid-read by making the first file a FIFO: opening it
// blocks until a writer opens it too, by which point the directory has been
// listed, and the writer swaps the link before it lets the read finish.
func TestFilesReadAcrossASwapIsAFailedLoad(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"v0/a.yaml": appFile("alpha-0"),
		"v0/b.yaml": appFile("bravo-0"),
		"v1/b.yaml": appFile("bravo-1"),
		"v2/a.yaml": appFile("alpha-2"),
		"v2/b.yaml": appFile("bravo-2"),
	})
	fifo := filepath.Join(root, "v1", "a.yaml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// The swap git-sync makes: a new link renamed over the old one.
	swap := func(version string) error {
		next := filepath.Join(root, "next")
		if err := os.Symlink(version, next); err != nil {
			return err
		}
		return os.Rename(next, filepath.Join(root, "current"))
	}
	if err := swap("v0"); err != nil {
		t.Fatal(err)
	}
	loader := NewPath(PathConfig{Dir: root, Path: "current", Files: true})
	good, _, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if err := swap("v1"); err != nil {
		t.Fatal(err)
	}

	swapped := make(chan error, 1)
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			swapped <- err
			return
		}
		err = swap("v2")
		if err == nil {
			_, err = w.WriteString(appFile("alpha-1"))
		}
		swapped <- errors.Join(err, w.Close())
	}()

	_, _, err = loader.Load(context.Background())
	select {
	case werr := <-swapped:
		if werr != nil {
			t.Fatal(werr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the swap never happened, so the first file was never opened (Load = %v)", err)
	}
	if err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("Load = %v, want a failed load for a directory that moved while it was read", err)
	}
	if loader.Current() != good {
		t.Error("Current moved off the last good set")
	}

	// And the next pass reads the version the link moved to.
	file, _, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("next Load = %v, want nil", err)
	}
	if got := strings.Join(names(t, file.Applications), " "); got != "alpha-2 bravo-2" {
		t.Errorf("applications = %s, want alpha-2 bravo-2", got)
	}
}

// The size bound is over the files together, so splitting a set across many
// does not multiply it; and a set exactly at it still loads.
func TestFilesAreBoundedTogether(t *testing.T) {
	half := maxAppSetSize / 2
	pad := func(content string, size int) string {
		return content + strings.Repeat("#", size-len(content)-1) + "\n"
	}

	t.Run("over", func(t *testing.T) {
		loader, dir, good := filesLoader(t, map[string]string{"a.yaml": appFile("alpha")})
		writeFiles(t, dir, map[string]string{"a.yaml": pad(appFile("alpha"), half), "b.yaml": pad(appFile("bravo"), half+1)})

		_, _, err := loader.Load(context.Background())
		if err == nil || !strings.Contains(err.Error(), "together") || !strings.Contains(err.Error(), "b.yaml") {
			t.Fatalf("Load = %v, want the bound refusing them together, naming b.yaml", err)
		}
		if loader.Current() != good {
			t.Error("Current moved off the last good set")
		}
	})

	t.Run("at", func(t *testing.T) {
		_, _, file := filesLoader(t, map[string]string{"a.yaml": pad(appFile("alpha"), half), "b.yaml": pad(appFile("bravo"), maxAppSetSize-half)})
		if len(file.Applications) != 2 {
			t.Errorf("got %d applications, want 2", len(file.Applications))
		}
	})
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
