// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The acceptance criterion of #178, and the three shells it constrains.
//
// **With every feature false the UI must be what Phase B shipped.** The
// capability plumbing has to be invisible in the free build, or the free
// product has grown a set of dead controls — a nav item that leads nowhere, a
// column that says the same thing on every row, a badge that can only ever read
// one word.
//
// The first test proves it mechanically rather than by listing the controls
// somebody remembered to hide: it renders the whole application twice — once
// against the document a free controller serves, and once against a controller
// with no capability endpoint at all, which is exactly what every build before
// Phase C is — and compares the two DOMs. A control that leaked into the free
// build would have to leak into both renders identically to survive that, and
// a control gated on a feature cannot.
//
// It is a comparison against a controller rather than against Phase B's own
// components, and deliberately: the alternative is a frozen copy of every
// component this repository has, kept in the tree for ever so that a test can
// render it. That was checked by hand once when this landed — the login screen,
// the shell and the list all render byte-identical DOM to the ones at the B2
// commit — and what is kept here is the property that can still be true a year
// from now.

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from './App'
import { capabilitiesKey, type FeatureName } from './api/discovery'
import type { View } from './api/types'
import { setToken } from './auth/session'
import {
  clone,
  communityCapabilities,
  communityDiscovery,
  controller,
  json,
  openStream,
} from './test/fakeApi'
import viewFull from './test/fixtures/view-full.json'

const okStatus = {
  appSet: { mode: 'static', loadedAt: '2026-07-22T09:41:10Z', stale: false },
  applications: 1,
}

/** One list row, shaped as the list handler serves one: the view with releases stripped. */
function row(name: string): View {
  const view = clone(viewFull) as unknown as View
  view.spec.name = name
  view.status.sync.state = 'synced'
  view.status.health.state = 'healthy'
  delete view.status.releases
  delete view.status.drift
  delete view.status.error
  return view
}

/** The capability document of a build that grants `names` and nothing else. */
function granting(...names: FeatureName[]): unknown {
  const capabilities = clone(communityCapabilities)
  capabilities.edition = 'business'
  for (const name of names) capabilities.features[name] = true
  return capabilities
}

function serve(capabilities: () => Response): void {
  controller({
    ...communityDiscovery(),
    '/api/v1/capabilities': capabilities,
    '/api/v1/status': () => json(200, okStatus),
    '/api/v1/applications': () => json(200, { applications: [row('edge')] }),
    '/api/v1/events': openStream,
  })
}

/** Waits until nothing is in flight, so a comparison is of two settled screens. */
async function settled(): Promise<void> {
  await screen.findByTestId('application-count')
  await waitFor(() => {
    expect(queryClient.getQueryState(capabilitiesKey)?.status).not.toBe('pending')
  })
  await waitFor(() => {
    expect(queryClient.isFetching()).toBe(0)
  })
}

/** Renders the signed-in application against one capability document and returns its DOM. */
async function snapshot(capabilities: () => Response): Promise<string> {
  queryClient.clear()
  serve(capabilities)
  render(<App />)
  await settled()
  const html = document.body.innerHTML
  cleanup()
  vi.unstubAllGlobals()
  return html
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

describe('the free build', () => {
  it('renders exactly what a controller with no capability endpoint renders', async () => {
    const free = await snapshot(() => json(200, communityCapabilities))
    // 404 rather than a document: this is a Phase B controller, where the route
    // did not exist. Every gate must therefore read the same as it does for an
    // all-false document, which is what makes the plumbing invisible.
    const phaseB = await snapshot(() => json(404, { error: 'not found' }))

    expect(free).toBe(phaseB)
    // Guards the guard: a comparison of two empty strings would pass and prove
    // nothing at all.
    expect(free).toContain('application-count')
  })

  it('carries no licensed control', async () => {
    serve(() => json(200, communityCapabilities))
    render(<App />)
    await settled()

    expect(screen.queryByRole('link', { name: 'Projects' })).toBeNull()
    expect(screen.queryByRole('columnheader', { name: 'Swarm' })).toBeNull()
    expect(screen.queryByTestId('licence-badge')).toBeNull()
  })

  it('offers the same login screen whether or not the controller can be asked', async () => {
    sessionStorage.clear()
    controller(communityDiscovery())
    render(<App />)
    await screen.findByLabelText('Admin token')
    const advertised = document.body.innerHTML
    cleanup()
    vi.unstubAllGlobals()

    queryClient.clear()
    controller({ '/ui/bootstrap.json': () => json(404, { error: 'not found' }) })
    render(<App />)
    await screen.findByLabelText('Admin token')

    expect(document.body.innerHTML).toBe(advertised)
  })
})

describe('the licensed shells', () => {
  it('adds the swarm column when multi-swarm is granted', async () => {
    serve(() => json(200, granting('multi-swarm')))
    render(<App />)
    await settled()

    expect(screen.getByRole('columnheader', { name: 'Swarm' })).toBeDefined()
    // The fixture's destination names a swarm; format.destination renders the
    // empty one as "local swarm", exactly as the CLI does.
    expect(within(rowFor('edge')).getByText('swarm')).toBeDefined()
  })

  it('adds the Projects nav item, and somewhere for it to go, when projects is granted', async () => {
    serve(() => json(200, granting('projects')))
    render(<App />)
    await settled()

    fireEvent.click(screen.getByRole('link', { name: 'Projects' }))

    // The nav item and the route are gated together on purpose: a NavLink to a
    // path no route claims matches nothing, which unmounts the shell with it and
    // presents as the whole UI disappearing.
    expect(screen.getByRole('heading', { name: 'Projects' })).toBeDefined()
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeDefined()
  })

  it('gates each shell on its own feature', async () => {
    serve(() => json(200, granting('projects')))
    render(<App />)
    await settled()

    expect(screen.getByRole('link', { name: 'Projects' })).toBeDefined()
    expect(screen.queryByRole('columnheader', { name: 'Swarm' })).toBeNull()
  })
})

describe('the capability document', () => {
  // One request for the tab, not one per screen: the shell, the route table and
  // the list all read it, and three requests for a document that cannot change
  // would be three chances to disagree about what this build is.
  it('is read once however many screens consume it', async () => {
    serve(() => json(200, granting('projects', 'multi-swarm')))
    render(<App />)
    await settled()

    fireEvent.click(screen.getByRole('link', { name: 'Controller' }))
    await screen.findByRole('heading', { name: 'Controller' })
    fireEvent.click(screen.getByRole('link', { name: 'Applications' }))
    await settled()

    expect(capabilityRequests()).toBe(1)
  })

  // staleTime: Infinity with no interval, proved by moving the clock past the
  // 30-second default the other queries poll on. The document changes only when
  // the controller restarts, and a restart drops the event stream this tab is
  // holding — so polling it buys nothing and costs a guarded request per tab
  // per half-minute.
  it('is never polled, while the list still is', async () => {
    // Installed before the render, not after: react-query schedules a query's
    // refetch interval when its observer mounts, and a timer created on the real
    // clock is one no amount of advancing a fake clock will ever fire — so the
    // whole test would pass by proving nothing.
    // shouldAdvanceTime, because everything below awaits a promise: a frozen
    // clock never lets testing-library's own polling finish, so the render
    // would time out before there was anything to advance past.
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      serve(() => json(200, communityCapabilities))
      render(<App />)
      await settled()
      const listRequests = requestsFor('/api/v1/applications')

      await vi.advanceTimersByTimeAsync(90_000)

      // The control: the clock really moved, because the list refetched on it.
      expect(requestsFor('/api/v1/applications')).toBeGreaterThan(listRequests)
      expect(capabilityRequests()).toBe(1)
    } finally {
      vi.useRealTimers()
    }
  })
})

/** The row an application's link sits in. */
function rowFor(name: string): HTMLElement {
  const tr = screen.getByRole('link', { name }).closest('tr')
  if (tr === null) throw new Error(`${name} is not in a table row`)
  return tr
}

function requestsFor(path: string): number {
  // Narrowed rather than stringified: fetch takes a Request or a URL too, and
  // src/api only ever passes a string — see api/client.test.ts.
  return vi.mocked(globalThis.fetch).mock.calls.filter(([url]) => typeof url === 'string' && url.startsWith(path))
    .length
}

function capabilityRequests(): number {
  return requestsFor('/api/v1/capabilities')
}
