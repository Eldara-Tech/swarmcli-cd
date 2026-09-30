// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
)

// auditingBackend answers UnpermittedNames per release from a table, and fails
// for the releases named in errs.
type auditingBackend struct {
	stubBackend
	needs map[string]application.Allow
	errs  map[string]error
	asked *[]capability.AllowRequest
}

func (b auditingBackend) UnpermittedNames(_ context.Context, req capability.AllowRequest) (application.Allow, error) {
	*b.asked = append(*b.asked, req)
	return b.needs[req.Name], b.errs[req.Name]
}

// listingEngine is a fakeEngine that also lists the swarm's releases.
type listingEngine struct {
	*fakeEngine
	releases []charts.Release
	err      error
}

func (e *listingEngine) List(context.Context) ([]charts.Release, error) { return e.releases, e.err }

// syncBuffer is a log sink the audit goroutine and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// owned is a release as this controller records it for app.
func owned(app, release, manifest string) charts.Release {
	return charts.Release{
		Name:     release,
		Owner:    charts.OwnerRef{ID: application.OwnerID("", app), Kind: charts.OwnerKindRelease, Name: release}.String(),
		Manifest: manifest,
	}
}

func auditTest(t *testing.T, apps []application.Spec, backend auditingBackend, engine *listingEngine) (*Reconciler, *syncBuffer) {
	t.Helper()
	logged := &syncBuffer{}
	r := New(apps, Options{
		Fetcher:   &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder:   &fakeBuilder{},
		Swarms:    fakeRegistry{backend: backend},
		NewEngine: func(charts.Backend) Engine { return engine },
		Log:       slog.New(slog.NewTextHandler(logged, nil)),
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
	})
	return r, logged
}

// One warning per application whose releases need entries its allowlist does
// not name, with those entries merged across its releases and grouped by the
// field they go in, and nothing for an application that needs none. A release
// that is not one of the set's — another controller's, or one installed by
// hand — is not asked about. A release that cannot be read is said so, and the
// rest are still read. Each is asked with its own manifest and files and its
// application's allowlist; the manifest itself is never logged.
func TestTheAuditWarnsOfTheAllowEntriesEachApplicationNeeds(t *testing.T) {
	edge := spec("edge", true)
	edge.Allow = application.Allow{Configs: []string{"shared-site"}}
	var asked []capability.AllowRequest
	backend := auditingBackend{
		needs: map[string]application.Allow{
			"web": {Secrets: []string{"web_db"}, Networks: []string{"web_public"}},
			"api": {Secrets: []string{"api_key", "web_db"}, Volumes: []string{"api_data"}},
		},
		errs:  map[string]error{"broken": errors.New("parsing the manifest: bad")},
		asked: &asked,
	}
	files := map[string][]byte{"files/site.conf": []byte("x")}
	web := owned("edge", "web", "services: {app: {image: x, environment: {PASSWORD: hunter2}}}")
	web.Files = files
	engine := &listingEngine{fakeEngine: &fakeEngine{}, releases: []charts.Release{
		web,
		owned("edge", "api", "api manifest"),
		owned("clean", "tidy", "tidy manifest"),
		owned("clean", "broken", "broken manifest"),
		owned("gone", "left", "left manifest"),
		{Name: "theirs", Owner: charts.OwnerRef{ID: application.OwnerID("other", "edge"), Kind: charts.OwnerKindRelease, Name: "theirs"}.String(), Manifest: "theirs manifest"},
		{Name: "handmade", Manifest: "handmade manifest"},
	}}
	r, logged := auditTest(t, []application.Spec{edge, spec("clean", true)}, backend, engine)

	r.auditAllowlists(t.Context())

	out := logged.String()
	var warned []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "allowlist does not permit") {
			warned = append(warned, line)
		}
	}
	if len(warned) != 1 {
		t.Fatalf("warned %d times, want once, for edge alone:\n%s", len(warned), out)
	}
	for _, want := range []string{
		"application=edge", `releases="[web api]"`, `allow.secrets="[api_key web_db]"`,
		"allow.volumes=[api_data]", "allow.networks=[web_public]",
	} {
		if !strings.Contains(warned[0], want) {
			t.Errorf("warning %q does not carry %s", warned[0], want)
		}
	}
	if strings.Contains(warned[0], "allow.configs") {
		t.Errorf("warning %q names a field with nothing to add", warned[0])
	}
	if !strings.Contains(out, "release=broken") || !strings.Contains(out, "application=clean") {
		t.Errorf("log %q does not say which release could not be read", out)
	}
	for _, rel := range engine.releases {
		if strings.Contains(out, rel.Manifest) || strings.Contains(out, "hunter2") {
			t.Errorf("log %q carries %s's manifest", out, rel.Name)
		}
	}
	var names []string
	for _, req := range asked {
		names = append(names, req.Name)
		if req.Name == "web" && (req.Manifest != web.Manifest || len(req.Files) != 1 || len(req.Allow.Configs) != 1) {
			t.Errorf("asked about web with %+v, want its own manifest, files and allowlist", req)
		}
	}
	if want := []string{"web", "api", "tidy", "broken"}; strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("asked about %v, want the set's releases %v", names, want)
	}
}

// Advisory, and quiet when it cannot answer: a swarm whose releases cannot be
// listed is said so once, and a backend that cannot audit is skipped.
func TestTheAuditSaysSoWhenItCannotListReleases(t *testing.T) {
	var asked []capability.AllowRequest
	engine := &listingEngine{fakeEngine: &fakeEngine{}, err: errors.New("daemon busy")}
	r, logged := auditTest(t, []application.Spec{spec("edge", true)}, auditingBackend{asked: &asked}, engine)
	r.auditAllowlists(t.Context())
	if !strings.Contains(logged.String(), "daemon busy") {
		t.Errorf("log %q does not say the releases could not be listed", logged.String())
	}

	quiet := &syncBuffer{}
	r = New([]application.Spec{spec("edge", true)}, Options{
		Fetcher: &fakeFetcher{}, Builder: &fakeBuilder{}, Swarms: fakeRegistry{},
		NewEngine: func(charts.Backend) Engine { return engine },
		Log:       slog.New(slog.NewTextHandler(quiet, nil)),
	})
	r.auditAllowlists(t.Context())
	if quiet.String() != "" {
		t.Errorf("log %q, want nothing from a backend that cannot audit", quiet.String())
	}
}

// Run starts the audit beside the loops.
func TestRunAuditsTheAllowlists(t *testing.T) {
	var asked []capability.AllowRequest
	backend := auditingBackend{needs: map[string]application.Allow{"web": {Secrets: []string{"web_db"}}}, asked: &asked}
	engine := &listingEngine{fakeEngine: &fakeEngine{plans: []*charts.Plan{synced()}}, releases: []charts.Release{owned("edge", "web", "m")}}
	r, logged := auditTest(t, []application.Spec{spec("edge", false)}, backend, engine)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logged.String(), "allow.secrets=[web_db]") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(logged.String(), "allow.secrets=[web_db]") {
		t.Errorf("log %q, want Run to have warned of the entry edge needs", logged.String())
	}
}
