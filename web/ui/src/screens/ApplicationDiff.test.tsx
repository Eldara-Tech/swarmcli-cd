// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The diff tab, and the pair of empty answers that mean opposite things.
//
// Rendered through <App /> against the faked controller rather than in
// isolation, so the route, the layout that draws the tabs and the fetch
// wrapper's error unwrapping are all really run — the 403 assertion below is
// about ApiError surviving react-query, which a mocked client would not prove.

import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import type { DiffResponse, View } from '../api/types'
import { setToken } from '../auth/session'
import { clone, controller, json, openStream } from '../test/fakeApi'
import diffConverged from '../test/fixtures/diff-converged.json'
import diffFull from '../test/fixtures/diff-full.json'
import diffNotPlanned from '../test/fixtures/diff-not-planned.json'
import viewFull from '../test/fixtures/view-full.json'

/** The detail document the tab's layout draws its header from. */
function view(): View {
  const v = clone(viewFull) as unknown as View
  v.spec.name = 'edge'
  return v
}

function response(from: unknown): DiffResponse {
  return clone(from) as DiffResponse
}

/** The fixture's diff is the placeholder string "diff"; a real one is a
 *  rendered manifest, so every test that looks at lines supplies its own. */
function withDiff(text: string): DiffResponse {
  const body = response(diffFull)
  const releases = body.releases
  if (releases === null || releases.length === 0) throw new Error('the fixture declares no release diff')
  releases[0].release = 'traefik'
  releases[0].diff = text
  return body
}

function serve(body: DiffResponse, status = 200): void {
  controller({
    // Longer than the detail path, so fakeApi's longest-prefix rule sends the
    // tab's request here and not to the document below it.
    '/api/v1/applications/edge/diff': () => json(status, body),
    '/api/v1/applications/edge': () => json(200, view()),
    '/api/v1/events': openStream,
  })
}

beforeEach(() => {
  sessionStorage.clear()
  queryClient.clear()
  window.history.pushState({}, '', '/applications/edge/diff')
  setToken('good')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('the two empty answers', () => {
  // Backwards from the guess, and the reason this endpoint answers two shapes
  // at all: drift.Diffs appends to a nil slice and skips unchanged releases, so
  // absence is what a converged application sends.
  it('reads null releases with planned true as nothing would change', async () => {
    serve(response(diffConverged))
    render(<App />)

    expect(await screen.findByText(/Nothing would change/)).toBeDefined()
    expect(screen.queryByText(/Not reconciled yet/)).toBeNull()
  })

  it('reads an empty list with planned false as not reconciled yet', async () => {
    serve(response(diffNotPlanned))
    render(<App />)

    expect(await screen.findByText(/Not reconciled yet/)).toBeDefined()
    expect(screen.queryByTestId('diff-converged')).toBeNull()
  })

  // planned is read first and the list second. A screen switching on the list
  // would collapse the two above into one answer, which is precisely what
  // api.go's ErrNotPlanned arm exists to keep apart.
  it('trusts planned over the list, so an unplanned application is never called converged', async () => {
    serve({ releases: null, planned: false })
    render(<App />)

    expect(await screen.findByText(/Not reconciled yet/)).toBeDefined()
  })
})

describe('the diff itself', () => {
  it('colours and glyphs each line, and folds the unchanged run between them', async () => {
    const manifest = [
      '- image: traefik:2.9',
      '+ image: traefik:3.0',
      ...Array.from({ length: 20 }, (_, i) => `  line ${i + 1}`),
      '+ replicas: 2',
    ]
    serve(withDiff(manifest.map((line) => `${line}\n`).join('')))
    render(<App />)

    const removed = await screen.findByText('image: traefik:2.9')
    // A glyph as well as a colour: colour alone fails for around 8% of male
    // readers, and `style-src 'self'` means the colour is a class either way.
    expect(removed.parentElement?.className).toContain('diff-removed')
    expect(within(removed.parentElement as HTMLElement).getByText('-')).toBeDefined()

    const added = screen.getByText('image: traefik:3.0')
    expect(added.parentElement?.className).toContain('diff-added')
    expect(within(added.parentElement as HTMLElement).getByText('+')).toBeDefined()

    // Required rather than nice: textdiff omits hunk headers, so every diff
    // carries the whole rendered manifest as context.
    expect(screen.getByText('14 unchanged lines')).toBeDefined()
    expect(screen.queryByText('line 10')).toBeNull()
    expect(screen.getByText('line 3')).toBeDefined()
  })

  it('says so when a planned release renders no manifest change at all', async () => {
    serve(withDiff('   \n'))
    render(<App />)

    expect(await screen.findByText('(no manifest change)')).toBeDefined()
  })

  // The list is shorter than the overview's: drift.Diffs skips every unchanged
  // release, so heading it as the application's releases would claim the others
  // had gone away.
  it('says the list is a subset rather than the whole release set', async () => {
    serve(withDiff('+ one\n'))
    render(<App />)

    expect(await screen.findByText(/fewer releases than the overview shows/)).toBeDefined()
  })

  it('names the release and the action a sync would take on it', async () => {
    serve(withDiff('+ one\n'))
    render(<App />)

    const panel = within(await screen.findByTestId('release-diff'))
    expect(panel.getByRole('heading', { name: /traefik/ })).toBeDefined()
    expect(panel.getByText('upgrade')).toBeDefined()
  })
})

// Not a failure. ActionDiff is granted separately from ActionRead, so a token
// that can see the application above may legitimately be refused this tab.
it('reads a 403 as a permission rather than as an error', async () => {
  serve({ error: 'forbidden' } as unknown as DiffResponse, 403)
  render(<App />)

  expect(await screen.findByTestId('forbidden')).toBeDefined()
  expect(screen.queryByRole('alert')).toBeNull()
})

it('reaches the tab from the detail screen, and leaves the header in place', async () => {
  serve(withDiff('+ one\n'))
  render(<App />)

  expect(await screen.findByRole('heading', { name: 'edge', level: 1 })).toBeDefined()
  expect(screen.getByRole('link', { name: 'Diff' }).className).toContain('tab-current')
  expect(screen.getByRole('link', { name: 'Overview' }).className).not.toContain('tab-current')
})
