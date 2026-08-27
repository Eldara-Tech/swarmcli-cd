// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import type { View } from '../api/types'
import { setToken } from '../auth/session'
import { clone, communityDiscovery, controller, json, pushStream } from '../test/fakeApi'
import viewFull from '../test/fixtures/view-full.json'

const okStatus = {
  appSet: { mode: 'static', loadedAt: '2026-07-22T09:41:10Z', stale: false },
  applications: 1,
}

function edgeRow(): View {
  const view = clone(viewFull) as unknown as View
  view.spec.name = 'edge'
  view.status.sync.state = 'synced'
  view.status.health.state = 'healthy'
  delete view.status.releases
  delete view.status.drift
  delete view.status.error
  return view
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

/**
 * deliver pushes a frame and lets React settle inside one act. The async form
 * is what flushes the microtasks the pushed frame's parse and state update run
 * in; the synchronous form would return before the terminal had the line.
 */
async function deliver(work: () => void): Promise<void> {
  await act(async () => {
    work()
    await Promise.resolve()
  })
}

describe('the monitor', () => {
  it('tails the controller event stream, one line per frame', async () => {
    // A driven stream: it delivers a frame only when the test pushes one, so the
    // terminal is empty until then and the assertion is not racing the open.
    const stream = pushStream()
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow()] }),
      '/api/v1/events': stream.open,
    })
    render(<App />)
    await screen.findByTestId('application-count')

    fireEvent.click(screen.getByRole('link', { name: 'Monitor' }))
    await screen.findByRole('heading', { name: 'Monitor' })
    expect(screen.getByText(/Waiting for controller activity/)).toBeDefined()

    await deliver(() => {
      stream.push({
        application: 'edge',
        swarm: '',
        type: 'sync-succeeded',
        message: 'converged',
        at: '2026-07-22T09:41:10Z',
      })
    })

    expect(await screen.findByText('sync-succeeded')).toBeDefined()
    expect(screen.getByText('converged')).toBeDefined()
  })
})
