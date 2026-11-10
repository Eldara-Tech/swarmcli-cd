// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package webhook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

type fakeReconciler struct {
	views []application.View

	mu        sync.Mutex
	refreshed []string
}

func (f *fakeReconciler) Views() []application.View { return f.views }

func (f *fakeReconciler) Refresh(app string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, app)
	return nil
}

func (f *fakeReconciler) refreshes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.refreshed)
}

type fakeAppSet struct{ n atomic.Int64 }

func (f *fakeAppSet) Refresh() { f.n.Add(1) }

func app(name, repo, revision string) application.Spec {
	return application.Spec{Name: name, Source: application.Source{RepoURL: repo, Revision: revision}}
}

func views(specs ...application.Spec) []application.View {
	out := make([]application.View, 0, len(specs))
	for _, s := range specs {
		out = append(out, application.View{Spec: s})
	}
	return out
}

// fixture is a handler over two applications on one repository and an app set
// followed from another, with its clock, its floor's timer and its log in the
// test's hands.
type fixture struct {
	h      *handler
	rec    *fakeReconciler
	appSet *fakeAppSet
	clock  *fakeClock
	log    *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	rec := &fakeReconciler{views: views(
		app("billing-api", "https://github.com/org/infra.git", "main"),
		app("billing-worker", "https://github.com/org/infra.git", "main"),
		app("billing-release", "https://github.com/org/infra.git", "v1.0.0"),
		app("unrelated", "https://github.com/org/other.git", "main"),
	)}
	appSet := &fakeAppSet{}
	log := &bytes.Buffer{}
	h := newHandler(Options{
		Secrets: []string{githubSecret, signingSecret}, Reconciler: rec,
		AppSet: appSet, AppSetRepo: "https://github.com/org/apps.git", AppSetRevision: "main",
		Log: slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	clock := newFakeClock()
	h.now, h.floor.now, h.floor.after = clock.now, clock.now, clock.after
	return &fixture{h: h, rec: rec, appSet: appSet, clock: clock, log: log}
}

func (f *fixture) do(method, path string, h http.Header, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header = h
	w := httptest.NewRecorder()
	f.h.routes().ServeHTTP(w, req)
	return w
}

func githubPush(ref, repo string) []byte {
	return []byte(`{"ref":"` + ref + `","repository":{"clone_url":"` + repo + `.git","html_url":"` + repo + `"},` +
		`"head_commit":{"message":"a body marker that must never be logged"}}`)
}

func githubHeaders(event string, body []byte) http.Header {
	return headers("Content-Type", "application/json", "X-GitHub-Event", event,
		"X-GitHub-Delivery", "d-123", "X-Hub-Signature-256", hubSignature(githubSecret, body))
}

func gitlabPush(ref, repo string) []byte {
	return []byte(`{"object_kind":"push","ref":"` + ref + `","project":{"git_http_url":"` + repo + `.git","web_url":"` + repo + `"}}`)
}

func gitlabSigned(event string, body []byte, now time.Time) http.Header {
	ts := strconv.FormatInt(now.Unix(), 10)
	return headers("Content-Type", "application/json", "X-Gitlab-Event", event,
		"Webhook-Id", "msg_1", "Webhook-Timestamp", ts, "Webhook-Signature", standardSignature(signingSecret, "msg_1", ts, body))
}

func TestResponses(t *testing.T) {
	infra := githubPush("refs/heads/main", "https://github.com/org/infra")
	garbage := []byte("{not json")
	oversized := bytes.Repeat([]byte(" "), maxBody+1)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		h      http.Header
		body   []byte
		want   int
	}{
		{"a push to a tracked branch", http.MethodPost, "/hooks/github", githubHeaders("push", infra), infra, http.StatusAccepted},
		{"a GitLab push to a tracked branch", http.MethodPost, "/hooks/gitlab",
			gitlabSigned("Push Hook", gitlabPush("refs/heads/main", "https://github.com/org/infra"), testNow),
			gitlabPush("refs/heads/main", "https://github.com/org/infra"), http.StatusAccepted},
		{"a GitLab tag push to a tracked tag", http.MethodPost, "/hooks/gitlab",
			gitlabSigned("Tag Push Hook", gitlabPush("refs/tags/v1.0.0", "https://github.com/org/infra"), testNow),
			gitlabPush("refs/tags/v1.0.0", "https://github.com/org/infra"), http.StatusAccepted},
		// The same 202 for everything authenticated that parses, whether it woke
		// anything or not: see TestEveryParsedDeliveryGetsTheSameAnswer.
		{"a ping", http.MethodPost, "/hooks/github", githubHeaders("ping", []byte(`{"zen":"x"}`)), []byte(`{"zen":"x"}`), http.StatusAccepted},
		{"an event that is not a push", http.MethodPost, "/hooks/github", githubHeaders("issues", infra), infra, http.StatusAccepted},
		{"a GitLab merge request", http.MethodPost, "/hooks/gitlab",
			gitlabSigned("Merge Request Hook", infra, testNow), infra, http.StatusAccepted},
		{"a push nothing tracks", http.MethodPost, "/hooks/github",
			githubHeaders("push", githubPush("refs/heads/feature", "https://github.com/org/infra")),
			githubPush("refs/heads/feature", "https://github.com/org/infra"), http.StatusAccepted},
		{"a bad signature", http.MethodPost, "/hooks/github",
			headers("Content-Type", "application/json", "X-GitHub-Event", "push", "X-Hub-Signature-256", hubSignature("guessed", infra)),
			infra, http.StatusUnauthorized},
		{"no signature", http.MethodPost, "/hooks/github", headers("Content-Type", "application/json", "X-GitHub-Event", "push"), infra, http.StatusUnauthorized},
		// Verified before parsed: garbage under a bad signature is a 401, so
		// nothing unauthenticated ever reaches the decoder.
		{"garbage under a bad signature", http.MethodPost, "/hooks/github",
			headers("Content-Type", "application/json", "X-GitHub-Event", "push", "X-Hub-Signature-256", hubSignature("guessed", garbage)),
			garbage, http.StatusUnauthorized},
		{"garbage under a good signature", http.MethodPost, "/hooks/github", githubHeaders("push", garbage), garbage, http.StatusBadRequest},
		// Bounded before authenticated: no signature at all, and still 413.
		{"an oversized body", http.MethodPost, "/hooks/github", headers("Content-Type", "application/json"), oversized, http.StatusRequestEntityTooLarge},
		{"a form-encoded body", http.MethodPost, "/hooks/github",
			headers("Content-Type", "application/x-www-form-urlencoded"), []byte("payload=%7B%7D"), http.StatusUnsupportedMediaType},
		{"a forge this build does not receive", http.MethodPost, "/hooks/bitbucket", githubHeaders("push", infra), infra, http.StatusNotFound},
		{"the API's paths", http.MethodPost, "/api/v1/applications/edge/sync", githubHeaders("push", infra), infra, http.StatusNotFound},
		{"GET", http.MethodGet, "/hooks/github", headers(), nil, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if got := f.do(tc.method, tc.path, tc.h, tc.body); got.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", got.Code, tc.want, got.Body)
			}
		})
	}
}

// The 415 says how to fix the one mistake everybody makes once.
func TestAFormEncodedHookIsToldWhatToChange(t *testing.T) {
	f := newFixture(t)
	w := f.do(http.MethodPost, "/hooks/github", headers("Content-Type", "application/x-www-form-urlencoded"), []byte("payload=%7B%7D"))
	if !strings.Contains(w.Body.String(), "application/json") {
		t.Errorf("body = %s, want it to name the content type to set", w.Body)
	}
}

// Past the bound a delivery is refused at once rather than queued, since each
// one in flight may hold a whole body in memory.
func TestDeliveriesBeyondTheBoundAreRefused(t *testing.T) {
	f := newFixture(t)
	for range maxDeliveries {
		f.h.slots <- struct{}{}
	}
	infra := githubPush("refs/heads/main", "https://github.com/org/infra")
	if w := f.do(http.MethodPost, "/hooks/github", githubHeaders("push", infra), infra); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	<-f.h.slots
	if w := f.do(http.MethodPost, "/hooks/github", githubHeaders("push", infra), infra); w.Code != http.StatusAccepted {
		t.Errorf("status = %d once a slot was free, want 202", w.Code)
	}
}

// A slot is held while the body arrives, and the body is unauthenticated until
// it has. So connections that send headers and then stall must each give their
// slot back once the body deadline passes, rather than at the listener's own
// thirty-second ReadTimeout — and a signed delivery then gets through. The
// server here has the controller's timeouts; only the body deadline is short.
func TestStalledBodiesGiveTheirSlotsBack(t *testing.T) {
	f := newFixture(t)
	f.h.bodyTimeout = 50 * time.Millisecond
	srv := httptest.NewUnstartedServer(f.h.routes())
	srv.Config.ReadHeaderTimeout = 10 * time.Second
	srv.Config.ReadTimeout = 30 * time.Second
	srv.Config.WriteTimeout = 10 * time.Second
	srv.Start()
	t.Cleanup(srv.Close)

	stalled := make([]net.Conn, 0, maxDeliveries)
	for range maxDeliveries {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		// Headers, no signature, and a body that never comes.
		if _, err := io.WriteString(c, "POST /hooks/github HTTP/1.1\r\nHost: x\r\n"+
			"Content-Type: application/json\r\nContent-Length: 1000\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		stalled = append(stalled, c)
	}

	// Each is answered once its deadline passes, and the handler gives its slot
	// back before the answer leaves. Five seconds is the test failing, not the
	// test waiting: without the deadline nothing answers for thirty.
	for i, c := range stalled {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("stalled body %d was never answered, so it still holds its slot: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("stalled body %d answered %d, want 400 for a body that did not arrive", i, resp.StatusCode)
		}
	}

	infra := githubPush("refs/heads/main", "https://github.com/org/infra")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/hooks/github", bytes.NewReader(infra))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = githubHeaders("push", infra)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("a signed delivery after the stalled ones = %d, want 202", resp.StatusCode)
	}
}

// An accepted delivery refreshes exactly what tracks the push, and says nothing
// about it in the answer.
func TestAnAcceptedDeliveryRefreshesWhatTracksThePush(t *testing.T) {
	f := newFixture(t)
	infra := githubPush("refs/heads/main", "https://github.com/org/infra")
	w := f.do(http.MethodPost, "/hooks/github", githubHeaders("push", infra), infra)

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %s: %v", w.Body, err)
	}
	if want := map[string]any{"accepted": true}; w.Code != http.StatusAccepted || !maps.Equal(got, want) {
		t.Errorf("response = %d %v, want 202 %v", w.Code, got, want)
	}
	if r := f.rec.refreshes(); !slices.Equal(r, []string{"billing-api", "billing-worker"}) {
		t.Errorf("refreshed %v, want the two applications on that repository and branch", r)
	}
	if n := f.appSet.n.Load(); n != 0 {
		t.Errorf("the app set was woken %d times by a push to another repository", n)
	}

	apps := githubPush("refs/heads/main", "https://github.com/org/apps")
	if w := f.do(http.MethodPost, "/hooks/github", githubHeaders("push", apps), apps); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d for a push to the app set's repository, want 202", w.Code)
	}
	if n := f.appSet.n.Load(); n != 1 {
		t.Errorf("the app set was woken %d times by a push to its own repository, want 1", n)
	}
}

// Every configured secret is good for every repository, on either route — a
// GitLab hook's signing token signs a GitHub-shaped delivery as well as its own.
// So the answer is the same for everything authenticated that parses: a push to
// an application's repository, to the app set's, to a ref nothing tracks, a
// ping, another event. If it differed, any one secret's holder could map what
// this controller follows by asking.
func TestEveryParsedDeliveryGetsTheSameAnswer(t *testing.T) {
	f := newFixture(t)
	asGitHub := func(event string, body []byte) http.Header {
		return headers("Content-Type", "application/json", "X-GitHub-Event", event,
			"X-Hub-Signature-256", hubSignature(signingSecret, body))
	}
	type delivery struct {
		name, path string
		h          http.Header
		body       []byte
	}
	var deliveries []delivery
	for _, d := range []struct{ name, ref, repo string }{
		{"an application's repository", "refs/heads/main", "https://github.com/org/infra"},
		{"the app set's repository", "refs/heads/main", "https://github.com/org/apps"},
		{"a ref nothing tracks", "refs/heads/nope", "https://github.com/org/infra"},
		{"a repository nothing tracks", "refs/heads/main", "https://github.com/org/elsewhere"},
	} {
		gh := githubPush(d.ref, d.repo)
		gl := gitlabPush(d.ref, d.repo)
		deliveries = append(deliveries,
			delivery{"github: " + d.name, "/hooks/github", asGitHub("push", gh), gh},
			delivery{"gitlab: " + d.name, "/hooks/gitlab", gitlabSigned("Push Hook", gl, testNow), gl})
	}
	ping := []byte(`{"zen":"x"}`)
	deliveries = append(deliveries,
		delivery{"a ping", "/hooks/github", asGitHub("ping", ping), ping},
		delivery{"another event", "/hooks/github", asGitHub("issues", ping), ping})

	var first string
	for _, d := range deliveries {
		w := f.do(http.MethodPost, d.path, d.h, d.body)
		// Headers too: an answer can differ in more places than its body.
		got := fmt.Sprintf("%d %v %s", w.Code, w.Header(), w.Body)
		if first == "" {
			first = got
		}
		if got != first {
			t.Errorf("%s answered %q, want %q like every other authenticated delivery", d.name, got, first)
		}
	}
	// And they did not all answer alike because nothing worked.
	if len(f.rec.refreshes()) == 0 || f.appSet.n.Load() == 0 {
		t.Errorf("refreshed %v and the app set %d times, want the tracked pushes to have woken both", f.rec.refreshes(), f.appSet.n.Load())
	}
}

// Redeliveries and bursts go through the floor: still accepted, still one
// refresh per target per floor, and the deferred one arrives once it passes.
func TestDeliveriesInsideTheFloorAreDeferredNotDropped(t *testing.T) {
	f := newFixture(t)
	infra := githubPush("refs/heads/main", "https://github.com/org/infra")
	apps := githubPush("refs/heads/main", "https://github.com/org/apps")
	for range 3 {
		for _, body := range [][]byte{infra, apps} {
			if w := f.do(http.MethodPost, "/hooks/github", githubHeaders("push", body), body); w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 for a delivery the floor defers", w.Code)
			}
		}
	}
	if r, n := f.rec.refreshes(), f.appSet.n.Load(); len(r) != 2 || n != 1 {
		t.Fatalf("refreshed %v and the app set %d times straight away, want each once", r, n)
	}
	if !strings.Contains(f.log.String(), "deferred=2") {
		t.Errorf("the log does not count the deferred refreshes:\n%s", f.log)
	}
	f.clock.advance(application.MinInterval)
	if r, n := f.rec.refreshes(), f.appSet.n.Load(); len(r) != 4 || n != 2 {
		t.Errorf("refreshed %v and the app set %d times after the floor, want the deferred refresh of each", r, n)
	}
}

// What the log may carry. A rejection is anybody on the internet, so it is
// debug only; and no line, at any level, carries a secret, a signature or the
// body, all three of which an operator's log shipper would otherwise hold.
func TestTheLogNeverCarriesSecretsSignaturesOrBodies(t *testing.T) {
	f := newFixture(t)
	infra := githubPush("refs/heads/main", "https://github.com/org/infra")
	bad := headers("Content-Type", "application/json", "X-GitHub-Event", "push", "X-Hub-Signature-256", hubSignature("guessed", infra))
	good := githubHeaders("push", infra)
	tokenHeaders := headers("Content-Type", "application/json", "X-Gitlab-Event", "Push Hook", "X-Gitlab-Token", "a-wrong-token")

	f.do(http.MethodPost, "/hooks/github", bad, infra)
	f.do(http.MethodPost, "/hooks/gitlab", tokenHeaders, infra)
	f.do(http.MethodPost, "/hooks/github", githubHeaders("push", []byte("{not json")), []byte("{not json"))
	f.do(http.MethodPost, "/hooks/github", good, infra)

	out := f.log.String()
	for _, leak := range []string{
		githubSecret, signingSecret, "a-wrong-token",
		strings.TrimPrefix(bad.Get("X-Hub-Signature-256"), "sha256="),
		strings.TrimPrefix(good.Get("X-Hub-Signature-256"), "sha256="),
		"body marker", "{not json",
		// The decoder's own error quotes the byte it choked on, which is body.
		"invalid character",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("the log carries %q:\n%s", leak, out)
		}
	}

	var info []string
	for line := range strings.Lines(out) {
		if !strings.Contains(line, "level=DEBUG") {
			info = append(info, line)
		}
	}
	if len(info) != 1 || !strings.Contains(info[0], "webhook delivery accepted") {
		t.Fatalf("lines above debug = %q, want the one accepted delivery", info)
	}
	for _, want := range []string{"forge=github", "delivery=d-123", "repository=github.com/org/infra", "ref=refs/heads/main",
		"appSet=false", "applications=\"[billing-api billing-worker]\"", "deferred=0"} {
		if !strings.Contains(info[0], want) {
			t.Errorf("the accepted line lacks %s: %s", want, info[0])
		}
	}
	if strings.Count(out, "webhook delivery rejected") != 3 {
		t.Errorf("want the three rejections at debug:\n%s", out)
	}
}

// A stale GitLab signature is refused on the controller's clock, which is the
// handler's own and not the test's wall clock.
func TestAStaleGitLabDeliveryIsRefused(t *testing.T) {
	f := newFixture(t)
	body := gitlabPush("refs/heads/main", "https://github.com/org/infra")
	h := gitlabSigned("Push Hook", body, testNow.Add(-10*time.Minute))
	if w := f.do(http.MethodPost, "/hooks/gitlab", h, body); w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a delivery signed ten minutes ago", w.Code)
	}
}
