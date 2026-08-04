// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { writeFileSync } from 'node:fs'
import react from '@vitejs/plugin-react'
import type { Plugin } from 'vite'
// vitest/config rather than vite: it is the same defineConfig with the `test`
// key added. One file, so the tests are built exactly as the bundle is —
// separate configs would let the two drift, and the drift would show up as a
// test suite that passes against a transform the browser never sees.
import { defineConfig } from 'vitest/config'

// Node 26 exposes Web Storage as unflagged globals, and vitest's jsdom
// environment copies a window property onto globalThis only when there is not
// one there already — so from 26 on both storages come from the process rather
// than from the document under test. localStorage is then a getter returning
// undefined unless --localstorage-file was passed, which is what App.test.tsx's
// `localStorage.length` reads; and sessionStorage becomes one store shared by
// every test file, which is where auth/session.ts puts the admin token it
// documents as living no longer than a tab.
//
// Turning Node's off is what lets jsdom's own through, so the storage the
// sources reach is the document's, as it is in a browser. Set here rather than
// in the test script because the workers are what must inherit it and a shell
// prefix is not portable. Guarded on the globals being present at all: Node 24
// and below have nothing to disable and reject the flag outright.
if ('localStorage' in globalThis) {
  process.env.NODE_OPTIONS = `${process.env.NODE_OPTIONS ?? ''} --no-experimental-webstorage`.trim()
}

// keepSentinel puts web/dist/.gitkeep back after every build.
//
// emptyOutDir wipes the directory first, and that file is the whole reason
// `//go:embed all:dist` compiles on a checkout that has never run Vite. Without
// this, running the UI build once would delete it, and the next `go build` on a
// machine with no Node — or in CI, on somebody else's PR — would fail with
// "pattern all:dist: no matching files found".
const keepSentinel = (): Plugin => ({
  name: 'swarmcli-cd:keep-embed-sentinel',
  closeBundle() {
    writeFileSync(new URL('../dist/.gitkeep', import.meta.url), '')
  },
})

export default defineConfig({
  plugins: [react(), keepSentinel()],
  build: {
    // Outside this project's root, which is why emptyOutDir has to be said out
    // loud: Vite refuses to clear a directory above its root unless it is.
    outDir: '../dist',
    emptyOutDir: true,
    // No data: URIs. The CSP names no font-src, so a font inherits
    // default-src 'self' and an inlined one would be refused by the browser
    // that asked for it.
    assetsInlineLimit: 0,
    // A source map is the whole application's source, served unauthenticated
    // beside the bundle, and it would be embedded in the binary either way.
    sourcemap: false,
  },
  server: {
    // Development is same-origin too, so nothing here needs CORS and the API
    // needs no development-only setting. The controller is expected on its
    // default listen address.
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/healthz': 'http://127.0.0.1:8080',
    },
  },
  test: {
    // jsdom because the components under test read and write the DOM and
    // sessionStorage. What it deliberately does not give them is a CSP, which
    // is why the style-prop ban is an eslint rule; see eslint.config.js.
    environment: 'jsdom',
    // The Playwright specs are run by Playwright, against a real controller.
    exclude: ['e2e/**', 'node_modules/**'],
    // No globals: every test imports what it uses, so tsconfig's `types: []`
    // stays true and a test file reads like the modules it exercises.
    globals: false,
  },
})
