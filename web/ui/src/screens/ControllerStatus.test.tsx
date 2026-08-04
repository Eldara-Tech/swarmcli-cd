// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The controller screen, and the four shapes it has to keep apart.
//
// Three of them are failures and docs/api.md says they are different failures.
// The fourth is not a failure at all and arrives looking exactly like the worst
// of the three, because api.go answers with a zero ControllerStatus when it has
// no controller to ask.

import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import { zeroTimestamp, type ControllerStatus } from '../api/types'
import { setToken } from '../auth/session'
import { clone, controller, json, openStream } from '../test/fakeApi'
import controllerStatusFull from '../test/fixtures/controller-status-full.json'
import controllerStatusZero from '../test/fixtures/controller-status-zero.json'

function status(from: unknown): ControllerStatus {
  return clone(from) as ControllerStatus
}

/** A loaded, healthy set: the full fixture with the two failure fields off. */
function loaded(): ControllerStatus {
  const s = status(controllerStatusFull)
  s.appSet.mode = 'git'
  s.appSet.stale = false
  delete s.appSet.error
  delete s.appSet.orphaned
  delete s.appSet.pruned
  delete s.appSet.pruneHeldBy
  return s
}

function serve(body: ControllerStatus): void {
  controller({
    '/api/v1/status': () => json(200, body),
    '/api/v1/events': openStream,
  })
}

/** The notice, whichever of the four it turned out to be. */
function notice(): HTMLElement {
  return screen.getByTestId('app-set-notice')
}

beforeEach(() => {
  sessionStorage.clear()
  queryClient.clear()
  window.history.pushState({}, '', '/status')
  setToken('good')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('the three app-set failure shapes', () => {
  it('draws a stale set as a set that is running the wrong version', async () => {
    const s = loaded()
    s.appSet.stale = true
    s.appSet.error = 'applications[1]: duplicate application name "edge"'
    serve(s)
    render(<App />)

    expect(await screen.findByText('The running application set is stale')).toBeDefined()
    expect(notice().className).toContain('notice-stale')
    expect(within(notice()).getByText(/duplicate application name/)).toBeDefined()
  })

  it('draws an error with stale false as a load that did not land in full', async () => {
    const s = loaded()
    s.appSet.error = 'edge: chart render failed'
    serve(s)
    render(<App />)

    expect(await screen.findByText('The last load reported an error')).toBeDefined()
    expect(notice().className).toContain('notice-partial')
    expect(notice().className).not.toContain('notice-stale')
  })

  // The loudest of the three, and the one with no per-row signal anywhere: no
  // application is failing, because there are no applications.
  it('draws a set that has never loaded as the loudest of the three', async () => {
    const s = loaded()
    s.appSet.loadedAt = zeroTimestamp
    s.appSet.error = 'dial tcp 10.0.0.1:22: connect: connection refused'
    s.applications = 0
    serve(s)
    render(<App />)

    expect(await screen.findByText('No application set has ever loaded')).toBeDefined()
    expect(notice().className).toContain('notice-never-loaded')
    // Distinct from both of the others, which is the whole acceptance
    // criterion: the same class on two shapes would pass every assertion above
    // and still be the bug.
    expect(notice().className).not.toContain('notice-stale')
    expect(notice().className).not.toContain('notice-partial')
  })

  // Date.parse of the Go zero time is a large negative number rather than NaN,
  // so a relative-time renderer guarding only on NaN says "56 years ago" here.
  it('renders a never-loaded timestamp as never, not as the year 1', async () => {
    const s = loaded()
    s.appSet.loadedAt = zeroTimestamp
    s.applications = 0
    serve(s)
    render(<App />)

    expect((await screen.findByTestId('app-set-loaded')).textContent).toBe('never')
    expect(screen.queryByText(/0001/)).toBeNull()
  })
})

describe('the fourth shape, which is not a failure', () => {
  // The zero fixture is api.go's s.controller == nil arm verbatim: mode empty,
  // loadedAt the zero time, applications zero. Every signal of the loudest
  // failure there is, on a controller where nothing is wrong.
  it('reads an empty mode as no source wired and suppresses the failure logic', async () => {
    serve(status(controllerStatusZero))
    render(<App />)

    expect(await screen.findByText('No application set source is wired')).toBeDefined()
    expect(screen.queryByTestId('app-set-notice')).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.getByTestId('app-set-applications').textContent).toBe('0')
  })
})

describe('the rest of the document', () => {
  it('says nothing at all when the app set is healthy', async () => {
    serve(loaded())
    render(<App />)

    expect((await screen.findByTestId('app-set-applications')).textContent).toBe('1')
    expect(screen.queryByTestId('app-set-notice')).toBeNull()
    expect(screen.queryByText('No application set source is wired')).toBeNull()
  })

  // The three lists that only exist when something happened. A "Pruned: none"
  // row on every healthy controller trains an operator to stop reading.
  it('omits the orphaned, pruned and prune-held lists when there are none', async () => {
    serve(loaded())
    render(<App />)

    await screen.findByTestId('app-set-applications')
    expect(screen.queryByRole('heading', { name: 'Orphaned' })).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Pruned' })).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Prune held' })).toBeNull()
  })

  it('lists them when there are some', async () => {
    serve(status(controllerStatusFull))
    render(<App />)

    expect(await screen.findByRole('heading', { name: 'Orphaned' })).toBeDefined()
    expect(screen.getByRole('heading', { name: 'Pruned' })).toBeDefined()
    expect(screen.getByRole('heading', { name: 'Prune held' })).toBeDefined()
  })
})
