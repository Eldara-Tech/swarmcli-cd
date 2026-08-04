// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { writeFileSync } from 'node:fs'
import react from '@vitejs/plugin-react'
import { defineConfig, type Plugin } from 'vite'

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
})
