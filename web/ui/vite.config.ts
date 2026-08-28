// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { readFileSync, existsSync } from 'node:fs'
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

// woff2Only strips the legacy .woff fallback out of the built stylesheet.
//
// Every @font-face @fontsource ships lists woff2 first and woff second. Nothing
// that can run React 19 lacks woff2 — it has been baseline since 2015 — so the
// fallback is never fetched by any browser that reaches this UI, and Vite emits
// it anyway: 197KB of files then embedded in every released binary by
// `//go:embed all:dist`. That is 56% of the font payload and 28% of the bundle.
//
// Done on the finished bundle rather than in a `transform` hook, because there
// is no module to hook: Vite's own CSS plugin resolves and inlines the
// `@import`s, so the @fontsource files never become modules of their own and a
// transform never sees a woff url. Here the stylesheet is one asset with the
// hashed names already in it, and the files it stops referencing are dropped in
// the same pass — leaving one behind would embed an asset nothing can reach.
const woff2Only = (): Plugin => ({
  name: 'swarmcli-cd:woff2-only',
  enforce: 'post',
  generateBundle(_options, bundle) {
    for (const [name, output] of Object.entries(bundle)) {
      if (output.type !== 'asset' || !name.endsWith('.css')) continue
      const css = output.source.toString()
      output.source = css.replace(/,\s*url\([^)]*\.woff\)\s*format\(["']woff["']\)/g, '')
    }
    for (const name of Object.keys(bundle)) {
      if (name.endsWith('.woff')) delete bundle[name]
    }
  },
})

/**
 * thirdPartyNotices emits dist/THIRD-PARTY-NOTICES.txt.
 *
 * The bundle is third-party code and third-party font bytes, and both licence
 * families it draws on — MIT/ISC for the JavaScript, OFL-1.1 for the fonts —
 * make retaining the copyright and permission notice a *condition* of
 * redistributing them. `//go:embed all:dist` redistributes them, inside every
 * released binary and every published image, so the notice has to be in dist or
 * it is nowhere.
 *
 * It throws rather than warning when a package has no readable licence file. A
 * notices file that silently omits a dependency is worse than none: it reads as
 * a statement that the dependency was checked. The lockfile is the source of
 * the package list — transitive runtime packages ship exactly as the direct ones
 * do, and `dev: true` is the only thing that means "not in the bundle".
 */
const thirdPartyNotices = (): Plugin => ({
  name: 'swarmcli-cd:third-party-notices',
  generateBundle() {
    const lock = JSON.parse(readFileSync(new URL('./package-lock.json', import.meta.url), 'utf8')) as {
      packages: Record<string, { version?: string; dev?: boolean }>
    }
    const names = Object.entries(lock.packages)
      .filter(([path, meta]) => path.startsWith('node_modules/') && meta.dev !== true)
      .map(([path]) => path.slice('node_modules/'.length))
      .sort()

    const sections = names.map((name) => {
      const root = new URL(`./node_modules/${name}/`, import.meta.url)
      const manifest = JSON.parse(readFileSync(new URL('package.json', root), 'utf8')) as {
        version: string
        license?: string
      }
      // The spellings npm packages actually use, in the order they are looked for.
      const file = ['LICENSE', 'LICENSE.md', 'LICENSE.txt', 'LICENCE', 'LICENCE.md', 'license']
        .map((candidate) => new URL(candidate, root))
        .find((url) => existsSync(url))
      if (file === undefined) {
        throw new Error(
          `${name} ships in the bundle and has no licence file to retain. ` +
            'Add its notice by hand here, or drop the dependency.',
        )
      }
      const header = `${name}@${manifest.version}${manifest.license === undefined ? '' : ` (${manifest.license})`}`
      return `${'='.repeat(78)}\n${header}\n${'='.repeat(78)}\n\n${readFileSync(file, 'utf8').trim()}\n`
    })

    this.emitFile({
      type: 'asset',
      fileName: 'THIRD-PARTY-NOTICES.txt',
      source:
        'swarmcli-cd bundles the third-party software below. Each package is\n' +
        'reproduced here with the copyright and permission notice its licence\n' +
        'requires to travel with it. swarmcli-cd itself is Apache-2.0; see LICENSE\n' +
        'in the repository.\n\n' +
        `${sections.join('\n')}`,
    })
  },
})

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
  plugins: [woff2Only(), react(), thirdPartyNotices(), keepSentinel()],
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
