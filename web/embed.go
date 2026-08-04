// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package web

import (
	"embed"
	"io/fs"
)

// bundle is what Vite wrote into web/dist, compiled into the binary.
//
// The `all:` prefix is load-bearing rather than decorative. A tree that has
// never run Vite holds one file, web/dist/.gitkeep, and without `all:` the
// embed skips dotfiles, finds nothing and fails to compile with "pattern
// dist: contains no embeddable files" — which would make `go build ./...` and
// `go install …@latest` require a Node toolchain that this repository promises
// they do not.
//
//go:embed all:dist
var bundle embed.FS

// Assets is the built UI, rooted at the bundle itself: index.html and assets/
// are at its top level, not under dist/.
//
// Rooted here rather than in Handler so that the fs.FS Handler takes is the
// thing it documents — a caller handing it a test filesystem writes the paths
// a browser asks for, and no caller has to know where the embed happened to
// put them.
var Assets = sub(bundle, "dist")

// sub is fs.Sub without the error, which cannot happen for a path literal:
// fs.Sub fails only on a name fs.ValidPath rejects. Panicking rather than
// returning a nil FS keeps a mistake here at process start instead of as a nil
// dereference on the first request a browser makes.
func sub(fsys fs.FS, dir string) fs.FS {
	out, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("web: embedding " + dir + ": " + err.Error())
	}
	return out
}
