// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package web serves the single-page UI the controller binary was built with.
//
// It is a root package rather than part of api for two reasons: api stays
// data-only, so nothing importing it links an embedded asset tree, and the
// private companion imports api's neighbours freely. api never imports this
// package — the wiring lives where all wiring lives, in controller.serve,
// which builds a Handler and passes it in as api.Options.UI.
//
// A binary built without running Vite embeds nothing, and every UI route then
// answers a plain-text page saying so. That is what keeps `go build ./...`,
// `go test ./...` and `go install …@latest` working with no Node installed,
// which is what the getting-started guide promises today.
package web

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// Options tune a Handler. Every field has a working default.
type Options struct {
	// Log reports an assets filesystem that could not be read at all — which is
	// not the same thing as a build with no UI, and is otherwise a page an
	// operator cannot explain with nothing anywhere saying why.
	Log *slog.Logger
}

const (
	// indexFile is the SPA's entry document, and the file whose absence is what
	// "this binary was built without a UI" means.
	indexFile = "index.html"

	// assetsPrefix is the one directory served as files. Everything else is the
	// index, because everything else is a route belonging to the router in the
	// browser.
	assetsPrefix = "assets/"

	// csp is the policy every UI response carries. `script-src 'self'` with no
	// 'unsafe-inline' is a constraint on what Vite may emit rather than a wish:
	// the browser holds the admin token, which is the swarm's root credential,
	// so an injected script here is a compromised swarm. Fonts and images have
	// no source of their own and inherit default-src, which is why the build
	// inlines nothing.
	csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

	// immutable is safe only because Vite hashes every asset filename: the bytes
	// behind one name never change, and a new build asks for a new name.
	immutable = "public, max-age=31536000, immutable"
)

// notBuilt is what every UI route answers on a binary compiled without a
// bundle. Plain text, and it names the two commands that fix it: the state is
// normal for a contributor who has never run Vite, and a blank page with a 200
// would leave them nothing to search for.
const notBuilt = `swarmcli-cd was built without its web UI.

The API is serving normally; only the browser UI is missing. It is built by
Vite and embedded at compile time, which "go build" on its own does not do:

    npm --prefix web/ui ci
    npm --prefix web/ui run build
    go build ./cmd/swarmcli-cd

The published image is built with both halves. See docs/design/web-ui.md.
`

// mimeTypes are the types this bundle can contain that Go's table does not
// have.
//
// mime.TypeByExtension answers from a built-in list plus the system's, and the
// runtime image is alpine with no /etc/mime.types — so a font would go out as
// the sniffed application/octet-stream, which a browser refuses to use once
// secure has sent nosniff. The failure is a page in the wrong font with nothing
// in any log.
var mimeTypes = map[string]string{
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".ico":   "image/x-icon",
}

// Handler serves the UI out of assets: index.html and assets/ at its top level.
//
// Whether a UI was built at all is decided once, here, by reading the index
// rather than by asking whether the filesystem is empty. A bundle carrying
// assets and no index is exactly as unserveable as an empty one, and would
// otherwise answer every route with a blank 200 instead of saying what is
// wrong.
func Handler(assets fs.FS, o Options) http.Handler {
	if o.Log == nil {
		o.Log = slog.Default()
	}

	index, err := fs.ReadFile(assets, indexFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// A build with no UI is the normal state and the page below says so.
			// A filesystem that failed for any other reason is not, and nothing
			// else in the process would ever mention it.
			o.Log.Error("the embedded UI could not be read; serving the not-built page", "error", err)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secure(w)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			// No name: ServeContent uses it only to detect a type, and this
			// response has one. ServeContent rather than Write so that HEAD and
			// a range request are answered the same way as everywhere else.
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(notBuilt))
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure(w)

		// path.Clean of the request path, and not r.PathValue: ServeMux cleans
		// the *escaped* path before routing, so %2e%2e survives it, and a
		// wildcard's value is handed over unescaped — which makes PathValue on
		// /assets/%2e%2e/%2e%2e/etc/passwd exactly "../../etc/passwd". embed.FS
		// refuses that through fs.ValidPath, but this Handler serves whatever
		// fs.FS its caller passed and must not depend on which one it got.
		// Cleaned first and then required to still be under assets/, an escape
		// leaves the prefix behind and is answered with the index.
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if !strings.HasPrefix(name, assetsPrefix) {
			// Every client-side route lands here — /applications/edge belongs to
			// the router in the browser, not to the mux — which is what makes a
			// bookmarked or reloaded URL work at all.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// no-store rather than a validator: this document names the hashed
			// asset filenames of the build it came from, so a copy cached across
			// an upgrade asks for files that no longer exist.
			w.Header().Set("Cache-Control", "no-store")
			http.ServeContent(w, r, indexFile, time.Time{}, bytes.NewReader(index))
			return
		}

		// Read rather than served with http.FileServerFS, which renders a
		// directory listing for /assets/ — enumerating every hashed filename in
		// the build to a caller that has not authenticated anything.
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			// Not the index. A request for a hashed name this build does not
			// have is a stale document or a probe, and answering it with the SPA
			// would turn a missing script into a 200 that renders nothing.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if ct := contentType(name); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", immutable)
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
}

// contentType is mime.TypeByExtension with the gaps above filled first.
func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := mimeTypes[ext]; ok {
		return ct
	}
	return mime.TypeByExtension(ext)
}

// secure writes the headers every UI response carries, built or not.
func secure(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	// What makes the MIME table above worth having: with nosniff a browser uses
	// the type it was given, and refuses an asset whose type does not fit where
	// the document asked for it, rather than guessing one.
	h.Set("X-Content-Type-Options", "nosniff")
	// A URL in this UI names an application. Without this every image or link
	// out of the page would carry that name to whatever host served it.
	h.Set("Referrer-Policy", "no-referrer")
}
