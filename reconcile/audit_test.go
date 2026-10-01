// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// auditingBackend answers UnpermittedNames per release from a table, failing or
// panicking for the releases named so.
type auditingBackend struct {
	stubBackend
	swarm  string
	needs  map[string]application.Allow
	errs   map[string]error
	panics map[string]bool
	asked  *[]capability.AllowRequest
	mu     *sync.Mutex
	onAsk  func()
}

func (b auditingBackend) UnpermittedNames(_ context.Context, req capability.AllowRequest) (application.Allow, error) {
	b.mu.Lock()
	*b.asked = append(*b.asked, req)
	b.mu.Unlock()
	if b.onAsk != nil {
		b.onAsk()
	}
	if b.panics[req.Name] {
		panic("conversion")
	}
	return b.needs[req.Name], b.errs[req.Name]
}

// listingEngine is a fakeEngine that also lists the swarm's releases.
//
// panics is atomic because a test flips it while the loops run, and every sync
// lists the releases to report their revisions.
type listingEngine struct {
	*fakeEngine
	releases []charts.Release
	err      error
	panics   atomic.Bool
}

func (e *listingEngine) List(context.Context) ([]charts.Release, error) {
	if e.panics.Load() {
		panic("decoding")
	}
	return e.releases, e.err
}

// swarmRegistry hands out one backend per swarm name, and fails for the rest.
type swarmRegistry map[string]charts.Backend

func (s swarmRegistry) Backend(_ context.Context, t swarms.Target) (charts.Backend, error) {
	if b, ok := s[t.Swarm]; ok {
		return b, nil
	}
	return nil, errors.New("no such swarm")
}

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

// lines is every logged line containing all of words.
func (s *syncBuffer) lines(words ...string) []string {
	var out []string
	for _, line := range strings.Split(s.String(), "\n") {
		match := line != ""
		for _, w := range words {
			match = match && strings.Contains(line, w)
		}
		if match {
			out = append(out, line)
		}
	}
	return out
}

const auditController = "ctl"

// owned is a release as controller auditController records it for app.
func owned(app, release, manifest string) charts.Release {
	return charts.Release{
		Name:     release,
		Owner:    charts.OwnerRef{ID: application.OwnerID(auditController, app), Kind: charts.OwnerKindRelease, Name: release}.String(),
		Manifest: manifest,
	}
}

// auditSetup is a reconciler over apps whose swarms are the registry's, each
// swarm's engine listing its own releases, logging into a buffer.
func auditSetup(apps []application.Spec, registry swarmRegistry, engines map[string]*listingEngine) (*Reconciler, *syncBuffer) {
	logged := &syncBuffer{}
	return New(apps, Options{
		Fetcher:      &fakeFetcher{revision: strings.Repeat("a", 40)},
		Builder:      &fakeBuilder{},
		Swarms:       registry,
		ControllerID: auditController,
		NewEngine: func(b charts.Backend) Engine {
			if ab, ok := b.(auditingBackend); ok {
				return engines[ab.swarm]
			}
			return &fakeEngine{}
		},
		Log: slog.New(slog.NewTextHandler(logged, nil)),
		Now: func() time.Time { return time.Unix(0, 0).UTC() },
	}), logged
}

func newAuditing(swarm string, asked *[]capability.AllowRequest) auditingBackend {
	return auditingBackend{swarm: swarm, asked: asked, mu: &sync.Mutex{},
		needs: map[string]application.Allow{}, errs: map[string]error{}, panics: map[string]bool{}}
}

// One warning per application whose releases need entries its allowlist does
// not name, merged across its releases and grouped by field; nothing for one
// that needs none, and no field with nothing in it. A name the release's own
// prefix used to hand it that is scoped under another application's release —
// web_a_db for release web beside release Web_a, compared without regard to case
// — is that release's, and is warned about apart, to review rather than to add,
// naming only the releases behind it. One scoped under another application's
// release but not under the reading release — tidy_token read by api — needed an
// entry before as well, and is one to add; so is one scoped under another of
// the application's own releases — web_b_db read by web, beside edge's web_b.
// Only releases actually read are counted. Each release is asked with its own
// manifest, files and application's allowlist; one that is not the set's, or is
// another controller's, is not asked; one that cannot be read, or panics, is said
// so and the rest are still read; and the manifest is never logged.
func TestTheAuditWarnsOfTheAllowEntriesEachApplicationNeeds(t *testing.T) {
	edge, clean := spec("edge", true), spec("clean", true)
	edge.Allow = application.Allow{Configs: []string{"shared-site"}}
	clean.Allow = application.Allow{Secrets: []string{"tidy_key"}}
	var asked []capability.AllowRequest
	backend := newAuditing("", &asked)
	backend.needs["web"] = application.Allow{Secrets: []string{"web_a_db", "web_b_db", "web_db"}, Networks: []string{"web_public"}}
	backend.needs["api"] = application.Allow{Secrets: []string{"web_db", "tidy_token"}, Configs: []string{"api_site"}, Volumes: []string{"api_data"}}
	backend.errs["broken"] = errors.New("parsing the manifest: bad")
	backend.panics["crashing"] = true
	files := map[string][]byte{"files/site.conf": []byte("x")}
	web := owned("edge", "web", "services: {app: {image: x, environment: {PASSWORD: hunter2}}}")
	web.Files = files
	theirs := owned("edge", "theirs", "theirs manifest")
	theirs.Owner = charts.OwnerRef{ID: application.OwnerID("other", "edge"), Kind: charts.OwnerKindRelease, Name: "theirs"}.String()
	engine := &listingEngine{fakeEngine: &fakeEngine{}, releases: []charts.Release{
		web, owned("edge", "api", "api manifest"), owned("edge", "web_b", "web_b manifest"),
		owned("clean", "tidy", "tidy manifest"), owned("clean", "broken", "broken manifest"),
		owned("clean", "crashing", "crashing manifest"), owned("gone", "Web_a", "Web_a manifest"),
		theirs, {Name: "handmade", Manifest: "handmade manifest"},
	}}
	r, logged := auditSetup([]application.Spec{edge, clean}, swarmRegistry{"": backend}, map[string]*listingEngine{"": engine})

	r.auditAllowlists(t.Context(), []application.Spec{edge, clean})

	adds := logged.lines("level=WARN", "and a deploy of them is refused")
	if len(adds) != 1 {
		t.Fatalf("warned %d times of entries to add, want once, for edge alone:\n%s", len(adds), logged.String())
	}
	for _, want := range []string{"application=edge", `releases="[api web]"`, `allow.secrets="[tidy_token web_b_db web_db]"`,
		"allow.configs=[api_site]", "allow.volumes=[api_data]", "allow.networks=[web_public]"} {
		if !strings.Contains(adds[0], want) {
			t.Errorf("warning %q does not carry %s", adds[0], want)
		}
	}
	reviews := logged.lines("level=WARN", "scoped under another release")
	if len(reviews) != 1 || !strings.Contains(reviews[0], "allow.secrets=[web_a_db]") || !strings.Contains(reviews[0], "releases=[web]") ||
		!strings.Contains(reviews[0], "scopedUnder=[Web_a]") || strings.Contains(adds[0], "web_a_db") ||
		strings.Contains(reviews[0], "allow.configs") {
		t.Errorf("warnings %q and %q, want web_a_db kept apart as web_a's", adds, reviews)
	}
	for _, want := range [][]string{
		{"level=WARN", "application=clean", "release=broken", "parsing the manifest"},
		{"level=WARN", "application=clean", "release=crashing", "panic"},
		{"level=INFO", "releases=4"},
	} {
		if len(logged.lines(want...)) != 1 {
			t.Errorf("log does not carry one line with %v:\n%s", want, logged.String())
		}
	}
	for _, rel := range engine.releases {
		if strings.Contains(logged.String(), rel.Manifest) || strings.Contains(logged.String(), "hunter2") {
			t.Errorf("log carries %s's manifest:\n%s", rel.Name, logged.String())
		}
	}
	allows := map[string]application.Allow{}
	var names []string
	for _, req := range asked {
		names = append(names, req.Name)
		allows[req.Name] = req.Allow
		if req.Name == "web" && (req.Manifest != web.Manifest || !reflect.DeepEqual(req.Files, files)) {
			t.Errorf("asked about web with %+v, want its own manifest and files", req)
		}
	}
	if want := []string{"web", "api", "web_b", "tidy", "broken", "crashing"}; !reflect.DeepEqual(names, want) {
		t.Errorf("asked about %v, want the set's releases %v", names, want)
	}
	if !reflect.DeepEqual(allows["web"], edge.Allow) || !reflect.DeepEqual(allows["tidy"], clean.Allow) {
		t.Errorf("asked with %+v, want each release under its own application's allowlist", allows)
	}
}

// Applications are read on the swarm each is destined for, one swarm at a time:
// a swarm that cannot be resolved, or whose releases cannot be listed, is said so
// and the others are still read.
func TestTheAuditReadsEachSwarmOnItsOwn(t *testing.T) {
	near, far, lost, stuck := spec("near", true), spec("far", true), spec("lost", true), spec("stuck", true)
	far.Destination.Swarm, lost.Destination.Swarm, stuck.Destination.Swarm = "b", "aa", "a"
	var asked []capability.AllowRequest
	a, b, local := newAuditing("a", &asked), newAuditing("b", &asked), newAuditing("", &asked)
	b.needs["far"] = application.Allow{Secrets: []string{"far_db"}}
	local.needs["near"] = application.Allow{Secrets: []string{"near_db"}}
	everything := []charts.Release{owned("near", "near", "m"), owned("far", "far", "m"), owned("stuck", "stuck", "m")}
	engines := map[string]*listingEngine{
		"":  {fakeEngine: &fakeEngine{}, releases: everything},
		"a": {fakeEngine: &fakeEngine{}, err: errors.New("daemon busy")},
		"b": {fakeEngine: &fakeEngine{}, releases: everything},
	}
	specs := []application.Spec{near, far, lost, stuck}
	r, logged := auditSetup(specs, swarmRegistry{"": local, "a": a, "b": b}, engines)

	r.auditAllowlists(t.Context(), specs)

	for _, want := range [][]string{
		{"level=WARN", "application=near", "allow.secrets=[near_db]"},
		{"level=WARN", "application=far", "allow.secrets=[far_db]"},
		{"level=WARN", "swarm=aa", "no such swarm"},
		{"level=WARN", "swarm=a", "daemon busy"},
	} {
		if len(logged.lines(want...)) != 1 {
			t.Errorf("log does not carry one line with %v:\n%s", want, logged.String())
		}
	}
	if len(asked) != 2 {
		t.Errorf("asked %d times, want once for near on its swarm and once for far on its own", len(asked))
	}
}

// A cancelled audit stops — before asking the swarm anything, or after the
// release it was reading — rather than reading every release and warning that
// each could not be read.
func TestACancelledAuditStops(t *testing.T) {
	var asked []capability.AllowRequest
	engine := &listingEngine{fakeEngine: &fakeEngine{}, releases: []charts.Release{owned("edge", "web", "m"), owned("edge", "api", "m")}}
	resolved := 0
	registry := countingRegistry{swarmRegistry{"": newAuditing("", &asked)}, &resolved}
	logged := &syncBuffer{}
	r := New([]application.Spec{spec("edge", true)}, Options{
		Fetcher: &fakeFetcher{}, Builder: &fakeBuilder{}, Swarms: registry, ControllerID: auditController,
		NewEngine: func(charts.Backend) Engine { return engine },
		Log:       slog.New(slog.NewTextHandler(logged, nil)),
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.auditAllowlists(ctx, []application.Spec{spec("edge", true)})
	if len(asked) != 0 || resolved != 0 || logged.String() != "" {
		t.Errorf("asked %d times, resolved %d swarms and logged %q, want nothing", len(asked), resolved, logged.String())
	}

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	cancelling := newAuditing("", &asked)
	cancelling.onAsk = cancel
	registry.swarmRegistry[""] = cancelling
	r.auditAllowlists(ctx, []application.Spec{spec("edge", true)})
	if len(asked) != 1 {
		t.Errorf("asked %d times, want the audit stopped after the release it was reading", len(asked))
	}
}

// countingRegistry counts the swarms resolved through it.
type countingRegistry struct {
	swarmRegistry
	calls *int
}

func (c countingRegistry) Backend(ctx context.Context, t swarms.Target) (charts.Backend, error) {
	*c.calls++
	return c.swarmRegistry.Backend(ctx, t)
}

// Run starts the audit beside the loops, an application added once the set is
// running is audited as it arrives, and a panic listing the releases is logged
// rather than taking the controller down.
func TestRunAuditsTheSetAndWhatJoinsIt(t *testing.T) {
	var asked []capability.AllowRequest
	backend := newAuditing("", &asked)
	backend.needs["web"] = application.Allow{Secrets: []string{"web_db"}}
	backend.needs["late"] = application.Allow{Secrets: []string{"late_db"}}
	engine := &listingEngine{fakeEngine: &fakeEngine{plans: []*charts.Plan{synced()}},
		releases: []charts.Release{owned("edge", "web", "m"), owned("late", "late", "m")}}
	r, logged := auditSetup([]application.Spec{spec("edge", false)}, swarmRegistry{"": backend}, map[string]*listingEngine{"": engine})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitFor := func(words ...string) bool {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if len(logged.lines(words...)) > 0 {
				return true
			}
		}
		return false
	}
	if !waitFor("application=edge", "allow.secrets=[web_db]") {
		t.Errorf("log %q, want Run to have warned of the entry edge needs", logged.String())
	}
	if err := r.Add(spec("late", false)); err != nil {
		t.Fatalf("Add = %v", err)
	}
	if !waitFor("application=late", "allow.secrets=[late_db]") {
		t.Errorf("log %q, want the application added later audited", logged.String())
	}
	engine.panics.Store(true)
	_ = r.Add(spec("third", false))
	if !waitFor("level=ERROR", "recovered a panic checking") {
		t.Errorf("log %q, want the panic recovered and logged", logged.String())
	}
	cancel()
	<-done
}
