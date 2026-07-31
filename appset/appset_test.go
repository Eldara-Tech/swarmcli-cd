// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package appset

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/git"
)

// The sourcer is what the git mode is specified to reuse, so the contract it
// has to satisfy is asserted at compile time rather than inferred from a test
// that happens to pass one in.
var _ Fetcher = (*git.Sourcer)(nil)

// setPath is where the app set lives in both modes: a path within a tree, not
// the tree's root, so the containment check has something to resolve.
const setPath = "apps/applications.yaml"

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

// invalidSets are the four ways a commit breaks the set, one per rule the
// loader is required to enforce before swapping anything in.
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
	// An operator removing the last application. It is a validation error, so
	// the running set is kept rather than every application being stopped.
	"no applications": "applications: []\n",
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
// and a failure is reported against the subtest that caused it.
func modes(initial string) map[string]func(*testing.T) mode {
	return map[string]func(*testing.T) mode{
		"git":  func(t *testing.T) mode { return gitMode(t, initial) },
		"path": func(t *testing.T) mode { return pathMode(t, initial) },
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
