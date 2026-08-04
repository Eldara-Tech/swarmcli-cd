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
