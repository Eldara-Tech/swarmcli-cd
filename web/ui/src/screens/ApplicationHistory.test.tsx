// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The history tab, and the null that throws.
//
// `revisions: null` is a real payload — reconcile.History builds one for every
// release the plan would install — and it is the most likely runtime crash on
// this screen, so it is the first thing asserted.

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import type { History, Revision, View } from '../api/types'
import { setToken } from '../auth/session'
import { clone, controller, json, openStream } from '../test/fakeApi'
import historyFull from '../test/fixtures/history-full.json'
import historyNeverDeployed from '../test/fixtures/history-never-deployed.json'
import historyZero from '../test/fixtures/history-zero.json'
import viewFull from '../test/fixtures/view-full.json'

/** The detail document the tab's layout draws its header from. */
function view(): View {
  const v = clone(viewFull) as unknown as View
  v.spec.name = 'edge'
  return v
}

function history(from: unknown): History {
  return clone(from) as History
}

/** The full fixture with its placeholder strings renamed; the shape stays the
 *  one application/fixtures_test.go marshalled from the Go type. */
function deployed(): History {
  const body = history(historyFull)
  const releases = body.releases
  if (releases === null || releases.length === 0) throw new Error('the fixture declares no release history')
  releases[0].name = 'traefik'
  return body
}

function revisions(body: History): Revision[] {
  const revs = body.releases?.[0].revisions
  if (revs === null || revs === undefined) throw new Error('the fixture declares no revisions')
  return revs
}

function serve(body: unknown, status = 200): void {
  controller({
    '/api/v1/applications/edge/history': () => json(status, body),
    '/api/v1/applications/edge': () => json(200, view()),
    '/api/v1/events': openStream,
  })
}

beforeEach(() => {
  sessionStorage.clear()
  queryClient.clear()
  window.history.pushState({}, '', '/applications/edge/history')
  setToken('good')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('a release with no revisions', () => {
  // ReleaseHistory.Revisions is a nil slice with no omitempty, so it reaches the
  // browser as null. `.map()` on it throws, which is why this test renders the
  // generated fixture rather than a hand-written one.
  it('renders null revisions as the release with nothing under it', async () => {
    serve(history(historyNeverDeployed))
    render(<App />)

    const panel = within(await screen.findByTestId('release-history'))
    expect(panel.getByRole('heading', { name: 'traefik' })).toBeDefined()
    expect(panel.getByText(/Declared, never deployed/)).toBeDefined()
  })

  // A different answer from "no such release", which is what an absent entry
  // would be — so the release keeps its heading.
  it('does not read it as the application having no releases', async () => {
    serve(history(historyNeverDeployed))
    render(<App />)

    await screen.findByTestId('release-history')
    expect(screen.queryByText(/Not reconciled yet/)).toBeNull()
  })
})

describe('the revision table', () => {
  it('leaves the order it was sent in alone, because it is already newest first', async () => {
    const body = deployed()
    const one = revisions(body)[0]
    revisions(body).unshift({ ...one, revision: 4 }, { ...one, revision: 3 })
    serve(body)
    render(<App />)

    await screen.findByTestId('release-history')
    // reconcile.revisions reverses the engine's ascending order "so every client
    // does not"; re-sorting here would be a second opinion on it.
    const rows = screen.getAllByRole('rowheader').map((cell) => cell.textContent)
    expect(rows).toEqual(['4', '3', '1'])
  })

  it('renders an owner stamp verbatim and explains the form it takes', async () => {
    const body = deployed()
    revisions(body)[0].owner = 'cd/default/edge:release/traefik'
    serve(body)
    render(<App />)

    expect(await screen.findByText('cd/default/edge:release/traefik')).toBeDefined()
    // The API exposes no controller id, so the screen cannot say whose stamp it
    // is and must not guess at a prefix rule.
    expect(screen.getByText(/cannot say which stamps are this controller/)).toBeDefined()
  })

  it('reads an absent owner as unclaimed', async () => {
    const body = deployed()
    delete revisions(body)[0].owner
    serve(body)
    render(<App />)

    expect(await screen.findByText('unclaimed')).toBeDefined()
  })
})

describe('the answers that are not a table', () => {
  it('reads no releases as not reconciled yet', async () => {
    // api.go's ErrNotPlanned arm writes an empty list; History{} marshals the
    // field as null. Both mean there is no plan to enumerate releases from.
    serve(history(historyZero))
    render(<App />)

    expect(await screen.findByText(/Not reconciled yet/)).toBeDefined()
  })

  // The only endpoint in the API that answers 502, and it means the swarm would
  // not be read — not that the controller is broken.
  it('gives a 502 its own panel and a way to try again', async () => {
    serve({ error: 'could not read release history from the swarm' }, 502)
    render(<App />)

    const panel = within(await screen.findByTestId('history-unreachable'))
    expect(panel.getByText(/The swarm would not answer/)).toBeDefined()
    expect(panel.getByText(/could not read release history from the swarm/)).toBeDefined()
    expect(panel.getByRole('button', { name: 'Try again' })).toBeDefined()
  })

  it('re-asks when the retry is pressed, and shows the history the swarm then gave', async () => {
    serve({ error: 'could not read release history from the swarm' }, 502)
    render(<App />)

    const retry = await screen.findByRole('button', { name: 'Try again' })
    // The daemon is answering again. A button rather than telling the operator
    // to reload: a reload costs the session token and the event stream, for a
    // failure that is usually over by the time it is read.
    serve(deployed())
    fireEvent.click(retry)

    expect(await screen.findByRole('heading', { name: 'traefik' })).toBeDefined()
    expect(screen.queryByTestId('history-unreachable')).toBeNull()
  })

  // ActionHistory is granted separately from ActionRead, and authz.go gives a
  // reason of its own: a history names what installed each revision.
  it('reads a 403 as a permission rather than as an error', async () => {
    serve({ error: 'forbidden' }, 403)
    render(<App />)

    expect(await screen.findByTestId('forbidden')).toBeDefined()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

describe('the status a recorded revision is drawn in', () => {
  // charts/types.go declares five, and every one of them was drawn with
  // chip-good — so the view an operator opens to find out which revision broke
  // reported the broken one as a success.
  const chipOf = (status: string): string => {
    const cell = screen.getByText(status)
    return cell.className
  }

  it('does not draw a failed revision as a success', async () => {
    const body = deployed()
    revisions(body)[0].status = 'failed'
    serve(body)
    render(<App />)

    expect(await screen.findByText('failed')).toBeDefined()
    expect(chipOf('failed')).toContain('chip-bad')
    expect(chipOf('failed')).not.toContain('chip-good')
  })

  it('keeps chip-good for the one status that is good', async () => {
    const body = deployed()
    revisions(body)[0].status = 'deployed'
    serve(body)
    render(<App />)

    expect(await screen.findByText('deployed')).toBeDefined()
    expect(chipOf('deployed')).toContain('chip-good')
  })

  it('draws superseded and uninstalled as neither good nor bad', async () => {
    for (const status of ['superseded', 'uninstalled']) {
      const body = deployed()
      revisions(body)[0].status = status
      serve(body)
      render(<App />)

      expect(await screen.findByText(status)).toBeDefined()
      expect(chipOf(status)).toContain('chip-muted')
      cleanup()
      queryClient.clear()
    }
  })

  it('renders a status a newer engine added verbatim, and not as a success', async () => {
    // application/history.go declares Status as a bare string with no
    // marshaller, so a sixth value is a thing that can arrive.
    const body = deployed()
    revisions(body)[0].status = 'pending-rollback'
    serve(body)
    render(<App />)

    expect(await screen.findByText('pending-rollback')).toBeDefined()
    expect(chipOf('pending-rollback')).not.toContain('chip-good')
  })
})
