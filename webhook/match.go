// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package webhook

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// push is everything a delivery is read for: which ref moved, in which
// repository. Nothing else in the payload is parsed, so nothing else in it can
// decide anything — least of all what is deployed.
type push struct {
	// delivery is the forge's id for this delivery, for the log line an
	// operator matches against the forge's delivery log.
	delivery string
	ref      string
	// repos are the repository's URLs as the forge spelled them: its clone URL
	// and its web URL, either of which an application may have copied.
	repos []string
}

// parseGitHub reads a push from GitHub, GitHub Enterprise, Gitea or Forgejo.
// The two forks send GitHub's event and delivery headers too, and their own
// only as a fallback. ok is false for any other event, ping included.
func parseGitHub(h http.Header, body []byte) (p push, ok bool, err error) {
	if first(h, "X-GitHub-Event", "X-Gitea-Event", "X-Forgejo-Event") != "push" {
		return push{}, false, nil
	}
	var payload struct {
		Ref        string `json:"ref"`
		Repository struct {
			CloneURL string `json:"clone_url"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return push{}, false, err
	}
	return push{
		delivery: first(h, "X-GitHub-Delivery", "X-Gitea-Delivery", "X-Forgejo-Delivery"),
		ref:      payload.Ref,
		repos:    []string{payload.Repository.CloneURL, payload.Repository.HTMLURL},
	}, true, nil
}

// parseGitLab reads a push or a tag push from GitLab.
func parseGitLab(h http.Header, body []byte) (p push, ok bool, err error) {
	if event := h.Get("X-Gitlab-Event"); event != "Push Hook" && event != "Tag Push Hook" {
		return push{}, false, nil
	}
	var payload struct {
		Ref     string `json:"ref"`
		Project struct {
			GitHTTPURL string `json:"git_http_url"`
			WebURL     string `json:"web_url"`
		} `json:"project"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return push{}, false, err
	}
	return push{
		delivery: first(h, "Webhook-Id", "X-Gitlab-Event-UUID"),
		ref:      payload.Ref,
		repos:    []string{payload.Project.GitHTTPURL, payload.Project.WebURL},
	}, true, nil
}

func first(h http.Header, names ...string) string {
	for _, name := range names {
		if v := h.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// match reports what a push wakes: the app set, when it is followed from the
// pushed repository at the pushed branch or tag, and every application whose
// source is. repo is the pushed repository as it was matched, for the log.
//
// Only what the controller already declares can match, and only exactly:
// a source.revision is a bare branch, tag or SHA, so the ref's refs/heads/ or
// refs/tags/ is dropped and the rest must equal it. An application pinned to a
// SHA never matches, which is right — no push moves it.
func (h *handler) match(p push) (repo string, appSet bool, apps []string) {
	revision, ok := strings.CutPrefix(p.ref, "refs/heads/")
	if !ok {
		if revision, ok = strings.CutPrefix(p.ref, "refs/tags/"); !ok {
			return "", false, nil
		}
	}
	pushed := make(map[string]bool, len(p.repos))
	for _, raw := range p.repos {
		if n, ok := normalise(raw); ok {
			pushed[n] = true
			if repo == "" {
				repo = n
			}
		}
	}

	appSet = pushed[h.appSetRepo] && h.appSetRevision == revision
	for _, view := range h.rec.Views() {
		src := view.Spec.Source
		if n, ok := normalise(src.RepoURL); ok && pushed[n] && src.Revision == revision {
			apps = append(apps, view.Spec.Name)
		}
	}
	return repo, appSet, apps
}

// normalise reduces a repository URL to what two spellings of one repository
// share: the host, lowercased and without userinfo or the default port, and the
// path without a trailing slash or .git. The path is case-folded too, because
// the forges treat it case-insensitively, and the cost of a false match is one
// extra refresh.
//
// Only https is accepted, because a spec can name nothing else that a forge
// pushes to: an ssh source is refused by the fetcher, and a local path is
// nothing a forge has heard of. ok is false otherwise, and the result is never
// empty when it is true — which is what lets an unset app-set repository match
// nothing.
func normalise(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	}
	path := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git")
	return host + strings.ToLower(path), true
}
