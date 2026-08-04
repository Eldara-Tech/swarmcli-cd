// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The applications list, rendered against the faked controller.
//
// Every body below starts from src/test/fixtures/view-full.json, which
// application/fixtures_test.go marshalled from the Go View. A hand-written body
// would let this suite keep passing against a document the controller stopped
// sending.

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import type { SyncState } from '../api/enums'
import type { View } from '../api/types'
import { setToken } from '../auth/session'
import { clone, controller, json, openStream } from '../test/fakeApi'
import controllerStatusFull from '../test/fixtures/controller-status-full.json'
import viewFull from '../test/fixtures/view-full.json'

/**
 * One list row, shaped as GET /api/v1/applications actually serves one: the
 * view with releases stripped, which is what api.go's list handler does to
 * every element before writing it. Nothing on this screen may branch on the
 * key, and building the body the way the handler does is what proves it.
 */
function row(name: string): View {
  const view = clone(viewFull) as unknown as View
  view.spec.name = name
  delete view.status.releases
  return view
}

/**
 * A row with nothing wrong. The fixture's placeholders are the opposite of
 * that: a degraded, out-of-sync, drifted application whose last reconcile
 * failed.
 */
function healthy(name: string): View {
  const view = row(name)
  view.status.sync.state = 'synced'
  view.status.health.state = 'healthy'
  delete view.status.drift
  delete view.status.error
  return view
}

const okStatus = {
  appSet: { mode: 'static', loadedAt: '2026-07-22T09:41:10Z', stale: false },
  applications: 2,
}

function serve(views: View[], status: unknown = okStatus): void {
  controller({
    '/api/v1/status': () => json(200, status),
    '/api/v1/applications': () => json(200, { applications: views }),
    '/api/v1/events': openStream,
  })
}

/** The row an application's link sits in. Found through the link rather than by
 *  the row's accessible name, which is a concatenation of every cell. */
function rowFor(name: string): HTMLElement {
  const tr = screen.getByRole('link', { name }).closest('tr')
  if (tr === null) throw new Error(`${name} is not in a table row`)
  return tr
}

beforeEach(() => {
  sessionStorage.clear()
  queryClient.clear()
  window.history.pushState({}, '', '/')
  setToken('good')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('the applications list', () => {
  it('renders one row per application, each linking to its detail screen', async () => {
    serve([healthy('edge'), healthy('ingress')])
    render(<App />)

    expect((await screen.findByTestId('application-count')).textContent).toBe('2 applications')
    expect(screen.getByRole('link', { name: 'edge' }).getAttribute('href')).toBe('/applications/edge')
    expect(screen.getByRole('link', { name: 'ingress' })).toBeDefined()
  })

  // The issue's first acceptance criterion. Twenty is the size at which the CLI
  // starts hiding columns, and the list has to stay one table and one pass —
  // nothing here may render per row what it can decide once.
  it('renders twenty applications', async () => {
    serve(Array.from({ length: 20 }, (_, i) => healthy(`app-${String(i)}`)))
    render(<App />)

    expect((await screen.findByTestId('application-count')).textContent).toBe('20 applications')
    expect(screen.getAllByRole('row')).toHaveLength(21)
  })

  it('says so rather than showing an empty table when there are no applications', async () => {
    serve([])
    render(<App />)

    expect(await screen.findByText('No applications.')).toBeDefined()
  })

  // The contract application/enum.go implements at the other end: a controller
  // newer than this tab reports a state this build has never heard of, and the
  // row has to render rather than throw. The cast is the whole point — the wire
  // offers no such guarantee, and the union in types.ts describes only what
  // this build knows.
  it('renders a sync state this build has never heard of as unknown', async () => {
    const ahead = healthy('edge')
    ahead.status.sync.state = 'quiesced' as unknown as SyncState
    serve([ahead])
    render(<App />)

    await screen.findByRole('link', { name: 'edge' })
    expect(within(rowFor('edge')).getByText('unknown')).toBeDefined()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  // The question was never asked, which is a different thing from asked and
  // unanswerable — and only the second one gets to be called "unknown".
  it('renders a dash, not unknown, for an application in manifest mode', async () => {
    serve([row('edge'), healthy('ingress')])
    render(<App />)

    await screen.findByRole('link', { name: 'ingress' })
    const manifest = within(rowFor('ingress'))
    expect(manifest.getByTitle(/never checked/).textContent).toBe('–')
    expect(manifest.queryByText('unknown')).toBeNull()
  })

  it('drops the drift column entirely when nothing was asked', async () => {
    serve([healthy('edge')])
    render(<App />)

    await screen.findByRole('link', { name: 'edge' })
    expect(screen.queryByRole('columnheader', { name: 'Drift' })).toBeNull()
  })

  // #107: nothing on a status moves when a reconcile fails before it observes,
  // so an application whose repository has been unreachable for a week reads
  // green under a fresh observedAt. The mark is the only thing that stops the
  // rest of the row being believed.
  it('marks the row whose every other cell is a stale observation, and names why', async () => {
    const failing = healthy('edge')
    failing.status.error = 'dial tcp 10.0.0.1:22: connect: connection refused'
    serve([failing, healthy('ingress')])
    render(<App />)

    await screen.findByRole('link', { name: 'edge' })
    expect(rowFor('edge').className).toContain('row-stale')
    expect(within(rowFor('edge')).getByText('failed')).toBeDefined()
    expect(rowFor('ingress').className).not.toContain('row-stale')
    expect(screen.getByText(/connection refused/)).toBeDefined()
    expect(screen.getByText(/last successful observation/)).toBeDefined()
  })
})

describe('the filters, which are a link and not a preference', () => {
  it('applies what the URL already carries, so a pasted link opens the same rows', async () => {
    window.history.pushState({}, '', '/?sync=synced')
    const drifting = healthy('edge')
    drifting.status.sync.state = 'out-of-sync'
    serve([drifting, healthy('ingress')])
    render(<App />)

    expect((await screen.findByTestId('application-count')).textContent).toBe('1 of 2 applications')
    expect(screen.queryByRole('link', { name: 'edge' })).toBeNull()
    expect(screen.getByRole('link', { name: 'ingress' })).toBeDefined()
  })

  it('filters on the text of a name, a repository or a revision', async () => {
    serve([healthy('edge'), healthy('ingress')])
    render(<App />)
    await screen.findByRole('link', { name: 'edge' })

    fireEvent.change(screen.getByLabelText('Search'), { target: { value: 'ingr' } })

    await waitFor(() => {
      expect(window.location.search).toBe('?q=ingr')
    })
    expect(screen.queryByRole('link', { name: 'edge' })).toBeNull()
  })

  it('offers "not checked" as its own drift option rather than as a fifth state', async () => {
    window.history.pushState({}, '', '/?drift=not-checked')
    serve([row('edge'), healthy('ingress')])
    render(<App />)

    await screen.findByRole('link', { name: 'ingress' })
    expect(screen.queryByRole('link', { name: 'edge' })).toBeNull()
  })

  it('keeps the card/table choice in the URL and out of storage', async () => {
    serve([healthy('edge')])
    render(<App />)
    await screen.findByRole('link', { name: 'edge' })
    expect(screen.getByRole('table')).toBeDefined()

    fireEvent.click(screen.getByRole('button', { name: 'Card view' }))

    await waitFor(() => {
      expect(window.location.search).toBe('?view=cards')
    })
    expect(screen.queryByRole('table')).toBeNull()
    expect(screen.getByRole('link', { name: 'edge' })).toBeDefined()
    expect(localStorage.length).toBe(0)
    // Only the credential, which auth/session.ts put there.
    expect(sessionStorage.length).toBe(1)
  })

  it('says the filters are what emptied the list, not the controller', async () => {
    window.history.pushState({}, '', '/?q=nothing-matches-this')
    serve([healthy('edge')])
    render(<App />)

    expect(await screen.findByText('No application matches these filters.')).toBeDefined()
  })
})

describe('the app set behind the list', () => {
  // Why the controller screen shipped with this one: a refused set makes every
  // row below describe something other than what the repository says, and no
  // row can say so about itself.
  it('points at the controller status when the running set is stale', async () => {
    serve([healthy('edge')], controllerStatusFull)
    render(<App />)

    expect(await screen.findByText(/running application set is stale/)).toBeDefined()
    expect(screen.getByRole('link', { name: 'Controller status' }).getAttribute('href')).toBe('/status')
  })

  it('says nothing when the app set is healthy', async () => {
    serve([healthy('edge')])
    render(<App />)

    await screen.findByRole('link', { name: 'edge' })
    expect(screen.queryByRole('link', { name: 'Controller status' })).toBeNull()
  })
})
