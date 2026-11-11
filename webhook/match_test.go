// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package webhook

import (
	"slices"
	"testing"
)

func TestNormalise(t *testing.T) {
	const want = "github.com/org/repo"
	for _, tc := range []struct {
		name, raw, want string
		ok              bool
	}{
		{"the clone URL", "https://github.com/org/repo.git", want, true},
		{"the web URL", "https://github.com/org/repo", want, true},
		{"a trailing slash", "https://github.com/org/repo/", want, true},
		{"both", "https://github.com/org/repo.git/", want, true},
		// Forges treat both halves case-insensitively; a false match costs one
		// extra refresh, and a missed one costs the push.
		{"the case of either half", "https://GitHub.com/Org/Repo.git", want, true},
		{"userinfo, which a spec may carry and a payload does not", "https://x-access-token@github.com/org/repo.git", want, true},
		{"the default port", "https://github.com:443/org/repo.git", want, true},
		{"another port is kept", "https://git.example.com:8443/org/repo.git", "git.example.com:8443/org/repo", true},
		// A spec can only fetch https, so nothing else can be what it names.
		{"ssh", "ssh://git@github.com/org/repo.git", "", false},
		{"scp-style ssh", "git@github.com:org/repo.git", "", false},
		{"plain http", "http://github.com/org/repo.git", "", false},
		{"a local path", "/srv/git/repo.git", "", false},
		{"empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalise(tc.raw)
			if got != tc.want || ok != tc.ok {
				t.Errorf("normalise(%q) = %q, %v; want %q, %v", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// What a push wakes is decided by what the controller already declares, and
// matched exactly: the pushed repository, and the branch or tag with its
// refs/heads/ or refs/tags/ dropped, equal to source.revision as written.
func TestMatch(t *testing.T) {
	rec := &fakeReconciler{views: views(
		app("on-main", "https://github.com/org/repo.git", "main"),
		app("web-url", "https://github.com/Org/Repo", "main"),
		app("on-tag", "https://github.com/org/repo.git", "v1.2.0"),
		app("on-other-branch", "https://github.com/org/repo.git", "develop"),
		app("other-repo", "https://github.com/org/other.git", "main"),
		app("pinned", "https://github.com/org/repo.git", "0123456789abcdef0123456789abcdef01234567"),
		app("local", "/srv/git/repo.git", "main"),
	)}
	h := newHandler(Options{Reconciler: rec, AppSet: &fakeAppSet{},
		AppSetRepo: "https://github.com/org/repo.git", AppSetRevision: "main"})
	repos := []string{"https://github.com/org/repo.git", "https://github.com/org/repo"}

	for _, tc := range []struct {
		name       string
		ref        string
		wantAppSet bool
		wantApps   []string
	}{
		{"a branch", "refs/heads/main", true, []string{"on-main", "web-url"}},
		{"a tag", "refs/tags/v1.2.0", false, []string{"on-tag"}},
		{"another branch", "refs/heads/develop", false, []string{"on-other-branch"}},
		// A ref that is neither is nothing a revision can name.
		{"a pull request ref", "refs/pull/7/head", false, nil},
		{"a bare name", "main", false, nil},
		{"a tag that names a branch", "refs/tags/main", true, []string{"on-main", "web-url"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, appSet, apps := h.match(push{ref: tc.ref, repos: repos})
			if appSet != tc.wantAppSet || !slices.Equal(apps, tc.wantApps) {
				t.Errorf("match = %v, %v; want %v, %v", appSet, apps, tc.wantAppSet, tc.wantApps)
			}
			if tc.wantApps != nil && repo != "github.com/org/repo" {
				t.Errorf("repo = %q, want the normalised pushed repository", repo)
			}
		})
	}
}

// The app set is woken only in git mode. Outside it the controller passes no
// repository, and an empty one must match nothing — including a payload whose
// URLs are all unusable, which would otherwise leave an empty string on both
// sides.
func TestTheAppSetIsWokenOnlyFromItsRepository(t *testing.T) {
	rec := &fakeReconciler{}
	for _, tc := range []struct {
		name, repo, revision string
		pushed               []string
		want                 bool
	}{
		{"git mode, its repository and branch", "https://github.com/org/apps.git", "main", []string{"https://github.com/org/apps.git"}, true},
		{"git mode, another branch", "https://github.com/org/apps.git", "release", []string{"https://github.com/org/apps.git"}, false},
		{"git mode, another repository", "https://github.com/org/apps.git", "main", []string{"https://github.com/org/infra.git"}, false},
		{"git mode from a local path", "/srv/git/apps.git", "main", []string{"https://github.com/org/apps.git"}, false},
		// The revision is set in these two although validate never lets it be
		// without a repository, so that only the repository can be what refuses.
		{"no repository", "", "main", []string{"https://github.com/org/apps.git"}, false},
		{"no repository, and a payload with no usable URL", "", "main", []string{"", "ssh://git@github.com/org/apps.git"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(Options{Reconciler: rec, AppSet: &fakeAppSet{}, AppSetRepo: tc.repo, AppSetRevision: tc.revision})
			if _, got, _ := h.match(push{ref: "refs/heads/main", repos: tc.pushed}); got != tc.want {
				t.Errorf("app set woken = %v, want %v", got, tc.want)
			}
		})
	}
}
