// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// fixture is a real repository on disk, built through go-git so the tests need
// no git binary and no network.
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
		t.Fatalf("init: %v", err)
	}
	return &fixture{t: t, dir: dir, rep: rep}
}

// commit writes a file and commits it, returning the commit hash.
func (f *fixture) commit(name, content string) plumbing.Hash {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	tree, err := f.rep.Worktree()
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := tree.Add(name); err != nil {
		f.t.Fatal(err)
	}
	hash, err := tree.Commit("add "+name, &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0).UTC()},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return hash
}

// tag creates an annotated tag, which is the case that needs dereferencing.
func (f *fixture) tag(name string, at plumbing.Hash) {
	f.t.Helper()
	_, err := f.rep.CreateTag(name, at, &gogit.CreateTagOptions{
		Message: name,
		Tagger:  &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0).UTC()},
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) source(revision string) application.Source {
	return application.Source{RepoURL: f.dir, Revision: revision, ReleaseFile: "r.yaml"}
}

func TestFetchResolvesBranchTagAndCommit(t *testing.T) {
	f := newFixture(t)
	first := f.commit("a.txt", "one")
	f.tag("v1.0.0", first)
	second := f.commit("b.txt", "two")

	branch, err := f.rep.Head()
	if err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir(), Auth{})

	for name, tc := range map[string]struct {
		revision string
		want     plumbing.Hash
	}{
		"branch":      {branch.Name().Short(), second},
		"tag":         {"v1.0.0", first},
		"commit hash": {first.String(), first},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := s.Fetch(context.Background(), "edge", f.source(tc.revision))
			if err != nil {
				t.Fatalf("Fetch = %v, want nil", err)
			}
			if got.Revision != tc.want.String() {
				t.Errorf("resolved %s, want %s", got.Revision, tc.want)
			}
			if len(got.Revision) != 40 {
				t.Errorf("revision %q is not a full hash", got.Revision)
			}
		})
	}
}

// The working tree is what the source-type layer reads, so the commit's files
// have to actually be on disk — and files from another revision must not be.
func TestFetchMaterialisesTheWorkingTree(t *testing.T) {
	f := newFixture(t)
	first := f.commit("a.txt", "one")
	f.commit("b.txt", "two")

	s := New(t.TempDir(), Auth{})
	head, err := f.rep.Head()
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Fetch(context.Background(), "edge", f.source(head.Name().Short()))
	if err != nil {
		t.Fatalf("Fetch = %v, want nil", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(got.Dir, name)); err != nil {
			t.Errorf("%s missing from the working tree: %v", name, err)
		}
	}

	// Going back to the earlier commit must remove the later commit's file.
	got, err = s.Fetch(context.Background(), "edge", f.source(first.String()))
	if err != nil {
		t.Fatalf("Fetch = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "b.txt")); !os.IsNotExist(err) {
		t.Error("b.txt survived a checkout of a commit that predates it")
	}
}

// A second fetch reuses the clone and still picks up new commits.
func TestFetchReusesTheCloneAndSeesNewCommits(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt", "one")
	head, err := f.rep.Head()
	if err != nil {
		t.Fatal(err)
	}
	branch := head.Name().Short()

	s := New(t.TempDir(), Auth{})
	first, err := s.Fetch(context.Background(), "edge", f.source(branch))
	if err != nil {
		t.Fatalf("Fetch = %v, want nil", err)
	}

	next := f.commit("b.txt", "two")
	second, err := s.Fetch(context.Background(), "edge", f.source(branch))
	if err != nil {
		t.Fatalf("Fetch = %v, want nil", err)
	}

	if second.Dir != first.Dir {
		t.Errorf("second fetch used %s, want the cached clone %s", second.Dir, first.Dir)
	}
	if second.Revision != next.String() {
		t.Errorf("second fetch resolved %s, want the new commit %s", second.Revision, next)
	}
	if first.Revision == second.Revision {
		t.Error("the new commit was not picked up")
	}
}

// Repointing an application at a different repository must not keep deploying
// the old one. Nothing downstream would catch it: every later step succeeds
// against a stale clone.
func TestFetchDiscardsACloneOfADifferentRepository(t *testing.T) {
	oldRepo, newRepo := newFixture(t), newFixture(t)
	oldRepo.commit("a.txt", "old")
	wanted := newRepo.commit("a.txt", "new")

	s := New(t.TempDir(), Auth{})

	oldHead, err := oldRepo.rep.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(context.Background(), "edge", oldRepo.source(oldHead.Name().Short())); err != nil {
		t.Fatalf("Fetch = %v, want nil", err)
	}

	newHead, err := newRepo.rep.Head()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Fetch(context.Background(), "edge", newRepo.source(newHead.Name().Short()))
	if err != nil {
		t.Fatalf("Fetch = %v, want nil", err)
	}
	if got.Revision != wanted.String() {
		t.Errorf("resolved %s, want the new repository's commit %s", got.Revision, wanted)
	}
	if content, err := os.ReadFile(filepath.Join(got.Dir, "a.txt")); err != nil || string(content) != "new" {
		t.Errorf("working tree still holds the old repository's content: %q, %v", content, err)
	}
}

func TestFetchRejects(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt", "one")
	s := New(t.TempDir(), Auth{})

	for name, tc := range map[string]struct {
		app, url, revision, want string
	}{
		"unknown revision": {"edge", f.dir, "no-such-branch", "cannot resolve revision"},
		"no revision":      {"edge", f.dir, "", "no revision"},
		"plaintext http":   {"edge", "http://git.example.com/x.git", "main", "plaintext remote"},
		"ssh url":          {"edge", "ssh://git@github.com/x/y.git", "main", "ssh remotes are not supported"},
		"scp-style ssh":    {"edge", "git@github.com:x/y.git", "main", "ssh remotes are not supported"},
		"relative url":     {"edge", "github.com/x/y", "main", "unsupported repository URL"},
		"no url":           {"edge", "", "main", "no repository URL"},
		"escaping app":     {"../elsewhere", f.dir, "main", "invalid application name"},
		"dotdot app":       {"..", f.dir, "main", "invalid application name"},
		"empty app":        {"", f.dir, "main", "no application name"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Fetch(context.Background(), tc.app, application.Source{RepoURL: tc.url, Revision: tc.revision})
			if err == nil {
				t.Fatalf("Fetch = nil, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// A failed clone must not leave a directory that would be opened as a valid
// repository on the next tick.
func TestFailedCloneLeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	s := New(root, Auth{})

	absent := filepath.Join(t.TempDir(), "not-a-repository")
	_, err := s.Fetch(context.Background(), "edge", application.Source{RepoURL: absent, Revision: "main"})
	if err == nil {
		t.Fatal("Fetch = nil, want an error")
	}
	if _, err := os.Stat(filepath.Join(root, "edge")); !os.IsNotExist(err) {
		t.Errorf("a partial clone was left behind: %v", err)
	}
}

// A cache directory that is not our clone — left by an earlier version, or by
// an operator poking about — is replaced rather than fetched into.
func TestFetchReplacesADirectoryThatIsNotOurClone(t *testing.T) {
	f := newFixture(t)
	wanted := f.commit("a.txt", "one")

	root := t.TempDir()
	stray := filepath.Join(root, "edge")
	if _, err := gogit.PlainInit(stray, false); err != nil {
		t.Fatal(err)
	}

	got, err := New(root, Auth{}).Fetch(context.Background(), "edge", f.source(wanted.String()))
	if err != nil {
		t.Fatalf("Fetch = %v, want nil", err)
	}
	if got.Revision != wanted.String() {
		t.Errorf("resolved %s, want %s", got.Revision, wanted)
	}
}

// A directory that cannot be opened as a repository at all is an error rather
// than something to silently clone over.
func TestFetchReportsAnUnopenableCache(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt", "one")

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "edge", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "edge", ".git", "config"), []byte("not ini\x00"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := New(root, Auth{}).Fetch(context.Background(), "edge", f.source("main"))
	if err == nil {
		t.Fatal("Fetch = nil, want an error")
	}
}

// A remote that has gone away is an error on the next tick, not a silent
// reconcile against whatever the clone happens to still hold.
func TestFetchReportsAnUnreachableRemote(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt", "one")
	head, err := f.rep.Head()
	if err != nil {
		t.Fatal(err)
	}
	src := f.source(head.Name().Short())

	s := New(t.TempDir(), Auth{})
	if _, err := s.Fetch(context.Background(), "edge", src); err != nil {
		t.Fatalf("Fetch = %v, want nil", err)
	}

	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(context.Background(), "edge", src); err == nil {
		t.Error("Fetch = nil, want an error once the remote is gone")
	}
}

func TestSupportedURL(t *testing.T) {
	for name, tc := range map[string]struct {
		url     string
		wantErr string
	}{
		"https":          {"https://github.com/acme/infra.git", ""},
		"absolute path":  {"/srv/git/infra.git", ""},
		"http":           {"http://git.example.com/x.git", "plaintext remote"},
		"ssh scheme":     {"ssh://git@github.com/x/y.git", "ssh remotes are not supported"},
		"scp style":      {"git@github.com:x/y.git", "ssh remotes are not supported"},
		"schemeless":     {"github.com/x/y", "unsupported repository URL"},
		"empty":          {"", "no repository URL"},
		"file scheme":    {"file:///srv/git/x.git", "unsupported repository URL"},
		"relative dotty": {"./infra", "unsupported repository URL"},
	} {
		t.Run(name, func(t *testing.T) {
			err := supportedURL(tc.url)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("supportedURL(%q) = %v, want nil", tc.url, err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("supportedURL(%q) = nil, want %q", tc.url, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("supportedURL(%q) = %v, want %q", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestAuthMethod(t *testing.T) {
	if got := (Auth{}).method(); got != nil {
		t.Errorf("an empty Auth produced %v, want nil so the remote is anonymous", got)
	}

	got := Auth{Password: "tok"}.method()
	if got == nil || !strings.Contains(got.String(), "x-access-token") {
		t.Errorf("method = %v, want the documented default username", got)
	}
	if got := (Auth{Username: "oauth2", Password: "tok"}).method(); got == nil || !strings.Contains(got.String(), "oauth2") {
		t.Errorf("method = %v, want the configured username", got)
	}
}

func TestAuthFromEnv(t *testing.T) {
	env := func(pairs map[string]string) func(string) string {
		return func(k string) string { return pairs[k] }
	}
	files := func(pairs map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			v, ok := pairs[p]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(v), nil
		}
	}

	t.Run("none is not an error", func(t *testing.T) {
		got, err := AuthFromEnv(env(nil), files(nil))
		if err != nil {
			t.Fatalf("AuthFromEnv = %v, want nil: a public repository needs no credential", err)
		}
		if got.method() != nil {
			t.Error("want an anonymous auth method")
		}
	})

	t.Run("token", func(t *testing.T) {
		got, err := AuthFromEnv(env(map[string]string{EnvToken: " tok ", EnvUsername: "oauth2"}), files(nil))
		if err != nil {
			t.Fatalf("AuthFromEnv = %v, want nil", err)
		}
		if got.Password != "tok" || got.Username != "oauth2" {
			t.Errorf("got %+v, want the trimmed token and the username", got)
		}
	})

	t.Run("file wins", func(t *testing.T) {
		got, err := AuthFromEnv(
			env(map[string]string{EnvTokenFile: "/run/secrets/git", EnvToken: "ignored"}),
			files(map[string]string{"/run/secrets/git": "from-file\n"}),
		)
		if err != nil {
			t.Fatalf("AuthFromEnv = %v, want nil", err)
		}
		if got.Password != "from-file" {
			t.Errorf("password = %q, want the file's contents", got.Password)
		}
	})

	for name, tc := range map[string]struct {
		env, files map[string]string
	}{
		"unreadable file": {map[string]string{EnvTokenFile: "/run/secrets/absent"}, nil},
		"empty file":      {map[string]string{EnvTokenFile: "/run/secrets/blank"}, map[string]string{"/run/secrets/blank": " \n"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := AuthFromEnv(env(tc.env), files(tc.files)); err == nil {
				t.Error("AuthFromEnv = nil, want an error")
			}
		})
	}
}
