// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The paths through the shell that nobody exercises by hand.
//
// The fake is installed at globalThis.fetch rather than at a mocked module, so
// the bearer header, the 401 rule and the event stream's own parser all sit
// under the seam and are really run. Mocking src/api/client.ts would test the
// screens against a client that cannot be wrong. It is shared with the screen
// suites; see src/test/fakeApi.ts.

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from './App'
import { getToken, setToken } from './auth/session'
import { clone, controller, json, openStream } from './test/fakeApi'
import viewFull from './test/fixtures/view-full.json'

/** Two applications, shaped as the generated fixture, named apart. */
function twoApplications(): unknown {
  const applications = ['edge', 'ingress'].map((name) => {
    const view = clone(viewFull)
    view.spec.name = name
    return view
  })
  return { applications }
}

/** A controller with an app set that loaded and is not stale. */
const healthyStatus = {
  appSet: { mode: 'static', loadedAt: '2026-07-22T09:41:10Z', stale: false },
  applications: 2,
}

beforeEach(() => {
  sessionStorage.clear()
  // The cache is one per tab, and a test process is one tab for every test. A
  // query answered from a previous test's data never reaches the faked
  // controller at all, so the 401 below would never be issued.
  queryClient.clear()
  window.history.pushState({}, '', '/')
})

afterEach(() => {
  // Explicit because vitest runs with globals off, and Testing Library only
  // registers its own afterEach when it finds a global one. Without this every
  // test after the first queries a document holding every earlier render.
  cleanup()
  vi.unstubAllGlobals()
})

describe('the shell', () => {
  it('asks for a token when there is none', () => {
    controller({})
    render(<App />)

    expect(screen.getByLabelText('Admin token')).toBeDefined()
  })

  it('signs in with a token the controller accepts and makes its one request', async () => {
    controller({
      '/api/v1/status': () => json(200, healthyStatus),
      '/api/v1/applications': () => json(200, twoApplications()),
      '/api/v1/events': openStream,
    })
    render(<App />)

    // fireEvent rather than setting .value and dispatching by hand: React
    // tracks the last value it wrote, so a direct assignment looks like no
    // change at all and the handler never runs.
    fireEvent.change(screen.getByLabelText('Admin token'), { target: { value: 'right' } })
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByTestId('application-count')).toBeDefined()
    expect(screen.getByTestId('application-count').textContent).toBe('2 applications')
    expect(getToken()).toBe('right')
  })

  it('says so and stays put when the controller rejects the token', async () => {
    controller({ '/api/v1/status': () => json(401, { error: 'unauthorized' }) })
    render(<App />)

    fireEvent.change(screen.getByLabelText('Admin token'), { target: { value: 'wrong' } })
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('alert')).toBeDefined()
    // Not signed in, and — the point of probing before storing — the message
    // survived, because the login screen was never unmounted and remounted.
    expect(getToken()).toBeNull()
  })

  // The one the issue exists for: a credential that stops working while the tab
  // is open must return to the login screen, not to an error page. It is the
  // path nobody exercises by hand, because it needs a token that was valid and
  // is not any more — a controller restarted with a new secret, most likely.
  it('returns to the login screen when a request 401s after signing in', async () => {
    setToken('was-valid')
    controller({
      '/api/v1/status': () => json(200, healthyStatus),
      '/api/v1/applications': () => json(401, { error: 'unauthorized' }),
      '/api/v1/events': openStream,
    })
    render(<App />)

    expect(await screen.findByLabelText('Admin token')).toBeDefined()
    expect(getToken()).toBeNull()
  })

  it('forgets the credential when the operator signs out', async () => {
    setToken('good')
    controller({
      '/api/v1/status': () => json(200, healthyStatus),
      '/api/v1/applications': () => json(200, { applications: [] }),
      '/api/v1/events': openStream,
    })
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Sign out' }))

    await waitFor(() => {
      expect(getToken()).toBeNull()
    })
    expect(screen.getByLabelText('Admin token')).toBeDefined()
  })

  it('never writes the credential to localStorage', async () => {
    setToken('good')
    controller({
      '/api/v1/status': () => json(200, healthyStatus),
      '/api/v1/applications': () => json(200, { applications: [] }),
      '/api/v1/events': openStream,
    })
    render(<App />)
    await screen.findByTestId('application-count')

    // The token is the swarm's root credential, so it must not outlive the tab.
    // A test rather than a comment because the two APIs differ by one word.
    expect(localStorage.length).toBe(0)
    expect(window.location.search).toBe('')
    expect(window.location.hash).toBe('')
  })
})
