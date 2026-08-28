// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package web

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// built is a bundle shaped like Vite's output: an index naming a hashed script,
// the script itself, and a font — the three things §4.3 of the design has a
// different rule for.
func built() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                &fstest.MapFile{Data: []byte(`<!doctype html><script type="module" src="/assets/app-a1b2c3.js"></script>`)},
		"assets/app-a1b2c3.js":      &fstest.MapFile{Data: []byte("console.log(1)\n")},
		"assets/inter-d4e5f6.woff2": &fstest.MapFile{Data: []byte("not really a font")},
		"THIRD-PARTY-NOTICES.txt":   &fstest.MapFile{Data: []byte("SIL OPEN FONT LICENSE Version 1.1")},
	}
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
	return rr
}

// The path that keeps `go install …@latest` working: a tree that has never run
// Vite embeds one sentinel file, and every UI route has to say so rather than
// answer a blank 200 or 404 that a contributor cannot explain.
func TestABinaryBuiltWithoutTheUiSaysSo(t *testing.T) {
	h := Handler(fstest.MapFS{".gitkeep": &fstest.MapFile{}}, Options{})

	rr := get(t, h, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if !strings.Contains(rr.Body.String(), "npm --prefix web/ui run build") {
		t.Errorf("the not-built page does not say how to build it:\n%s", rr.Body.String())
	}
}

// "Not built" is the absence of an index, not an empty filesystem. A bundle
// with assets and no index is exactly as unserveable, and reporting it as built
// would answer every route with a blank page.
func TestABundleWithNoIndexIsNotBuilt(t *testing.T) {
	fsys := built()
	delete(fsys, "index.html")

	rr := get(t, Handler(fsys, Options{}), "/")
	if !strings.Contains(rr.Body.String(), "built without its web UI") {
		t.Errorf("a bundle with no index served:\n%s", rr.Body.String())
	}
}

// Every unmatched path is the SPA, because every unmatched path is a route
// belonging to the router in the browser. A bookmarked application URL is the
// case this exists for.
func TestAClientRouteServesTheIndex(t *testing.T) {
	h := Handler(built(), Options{})

	for _, target := range []string{"/", "/applications", "/applications/edge"} {
		rr := get(t, h, target)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", target, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "<!doctype html>") {
			t.Errorf("GET %s did not serve the index: %s", target, rr.Body.String())
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store: the index names the hashed filenames of one build", target, got)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q, want text/html", target, ct)
		}
	}
}

func TestAHashedAssetIsServedImmutable(t *testing.T) {
	rr := get(t, Handler(built(), Options{}), "/assets/app-a1b2c3.js")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != immutable {
		t.Errorf("Cache-Control = %q, want %q", got, immutable)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a javascript type", ct)
	}
}

// Go's MIME table has no font types and the runtime image has no
// /etc/mime.types, so without the local table this goes out as the sniffed
// octet-stream — which the nosniff header then makes the browser refuse.
func TestAFontCarriesAFontType(t *testing.T) {
	rr := get(t, Handler(built(), Options{}), "/assets/inter-d4e5f6.woff2")

	if got := rr.Header().Get("Content-Type"); got != "font/woff2" {
		t.Errorf("Content-Type = %q, want font/woff2", got)
	}
}

// ServeMux cleans the escaped path before routing, so %2e%2e reaches a handler
// as "..". embed.FS would refuse the result, but Handler serves whatever fs.FS
// it was given and must refuse it itself.
func TestATraversalOutOfTheBundleServesTheIndex(t *testing.T) {
	fsys := built()
	fsys["private.txt"] = &fstest.MapFile{Data: []byte("SENSITIVE")}
	h := Handler(fsys, Options{})

	for _, target := range []string{
		"/assets/../private.txt",
		"/assets/%2e%2e/private.txt",
		"/assets/%2e%2e/%2e%2e/etc/passwd",
	} {
		rr := get(t, h, target)
		if strings.Contains(rr.Body.String(), "SENSITIVE") {
			t.Errorf("GET %s escaped the assets directory", target)
		}
		if !strings.Contains(rr.Body.String(), "<!doctype html>") {
			t.Errorf("GET %s = %q, want the index", target, rr.Body.String())
		}
	}
}

// http.FileServerFS would render a listing here, which enumerates every hashed
// filename in the build to a caller that has authenticated nothing.
func TestTheAssetsDirectoryIsNotListed(t *testing.T) {
	fsys := built()
	rr := get(t, Handler(fsys, Options{}), "/assets/")

	if got, want := rr.Body.String(), string(fsys["index.html"].Data); got != want {
		t.Errorf("GET /assets/ served something other than the index:\n%s", got)
	}
	if strings.Contains(rr.Body.String(), "inter-d4e5f6.woff2") {
		t.Errorf("GET /assets/ enumerated the bundle:\n%s", rr.Body.String())
	}
}

// A hashed name this build does not have is a stale document or a probe.
// Answering it with the SPA would turn a missing script into a 200 that renders
// nothing at all.
func TestAMissingAssetIsNotTheIndex(t *testing.T) {
	rr := get(t, Handler(built(), Options{}), "/assets/gone-000000.js")

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body %q", rr.Code, rr.Body.String())
	}
}

// The headers are on every UI response, including the not-built page: a build
// with no UI is still a page reachable from a browser.
func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	handlers := map[string]http.Handler{
		"built":     Handler(built(), Options{}),
		"not built": Handler(fstest.MapFS{}, Options{}),
	}
	for name, h := range handlers {
		for _, target := range []string{"/", "/applications/edge", "/assets/app-a1b2c3.js", "/assets/missing.js"} {
			rr := get(t, h, target)
			if got := rr.Header().Get("Content-Security-Policy"); got != csp {
				t.Errorf("%s GET %s CSP = %q, want %q", name, target, got, csp)
			}
			if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("%s GET %s nosniff = %q", name, target, got)
			}
			if got := rr.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("%s GET %s Referrer-Policy = %q", name, target, got)
			}
		}
	}
}

// A GET pattern on the mux matches HEAD too, so two methods reach this handler
// where the route table names one. ServeContent is what makes that harmless,
// and this is the test rather than the discovery.
func TestHeadIsAnsweredWithoutABody(t *testing.T) {
	rr := httptest.NewRecorder()
	Handler(built(), Options{}).ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD returned a body: %q", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// Two things nothing else here can see, and both of them are compile-time:
// Assets is rooted at the bundle rather than at dist/, and the sentinel that
// makes `//go:embed all:dist` compile on a tree which has never run Vite is
// actually in the binary. This is the only check on either that runs where
// there is no Node.
func TestTheEmbedIsRootedAtTheBundle(t *testing.T) {
	if _, err := fs.ReadFile(Assets, ".gitkeep"); err != nil {
		t.Errorf("reading the embed sentinel: %v", err)
	}
}

var (
	scriptTag = regexp.MustCompile(`(?is)<script([^>]*)>`)
	// Comments are stripped before the scan. A browser executes nothing in one,
	// and index.html's own header talks about the tags this test looks for —
	// which is a failure caused by writing the rule down next to the thing it
	// governs.
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// The CSP names `script-src 'self'` with no 'unsafe-inline', which is a
// constraint on what Vite may emit and not a wish — the browser holds the
// swarm's root credential. Vite can start inlining a small chunk after a config
// or version change, and the symptom would be a blank page and a console error
// nobody sees until a release.
//
// It can only run where a real bundle exists, which is why ci.yml's ui job has
// Go as well as Node: the test job has no Node and always skips here.
//
// requireBundle is what stops the skip from being the answer everywhere. Where
// there is no Node, skipping is correct and is the only reason `go test ./...`
// works on a fresh checkout. In the one job that built a bundle a skip means
// something else entirely — that the build produced nothing — and a green run
// that asserted nothing is indistinguishable from a green run that asserted
// this. So that job sets the variable, and there the absence is a failure.
const requireBundle = "SWARMCLI_CD_REQUIRE_UI_BUNDLE"

func TestTheBuiltIndexHasNoInlineScriptOrStyle(t *testing.T) {
	index, err := fs.ReadFile(Assets, indexFile)
	if errors.Is(err, fs.ErrNotExist) {
		if os.Getenv(requireBundle) != "" {
			t.Fatalf("%s is set and no UI is embedded: this is the job that builds one, so this check ran nowhere", requireBundle)
		}
		t.Skip("no UI is embedded in this build; ci.yml's ui job is where this runs")
	}
	if err != nil {
		t.Fatalf("reading the embedded index: %v", err)
	}

	markup := htmlComment.ReplaceAllString(string(index), "")
	for _, m := range scriptTag.FindAllStringSubmatch(markup, -1) {
		if !strings.Contains(strings.ToLower(m[1]), "src=") {
			t.Errorf("the embedded index carries an inline <script%s>; the CSP has no 'unsafe-inline' and the page will not run", m[1])
		}
	}
	if strings.Contains(strings.ToLower(markup), "<style") {
		t.Error("the embedded index carries an inline <style>; the CSP has no 'unsafe-inline' and the page will render unstyled")
	}
}

// The bundle carries third-party code and third-party font bytes, and MIT, ISC
// and OFL-1.1 each make retaining the notice a condition of redistributing
// them. //go:embed all:dist redistributes the lot inside every released binary,
// so the notices have to be reachable — and reachable without a credential,
// because a licence notice behind one has not been provided to anybody.
func TestTheThirdPartyNoticesAreServedAsText(t *testing.T) {
	rr := get(t, Handler(built(), Options{}), "/THIRD-PARTY-NOTICES.txt")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "SIL OPEN FONT LICENSE") {
		t.Errorf("body = %q, want the notices rather than the index", rr.Body.String())
	}
	// Not the SPA fallback, which is what every other root path gets and which
	// would answer 200 with an HTML document that says nothing about licences.
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	// The name carries no build hash, so a cached copy would outlive the
	// dependency set it describes.
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q, want no-store", cc)
	}
}

// A tree built before the notices existed still has to serve a working UI: the
// path falls through to the SPA rather than 404ing on the one file whose
// absence is a build-age question and not a broken request.
func TestABundleWithoutNoticesFallsThroughToTheIndex(t *testing.T) {
	fsys := built()
	delete(fsys, "THIRD-PARTY-NOTICES.txt")
	rr := get(t, Handler(fsys, Options{}), "/THIRD-PARTY-NOTICES.txt")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "<!doctype html>") {
		t.Errorf("body = %q, want the index", rr.Body.String())
	}
}
