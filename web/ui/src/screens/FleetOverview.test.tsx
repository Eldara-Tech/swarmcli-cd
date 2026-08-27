// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import type { HealthState, SyncState } from '../api/enums'
import type { View } from '../api/types'
import { setToken } from '../auth/session'
import { clone, communityDiscovery, controller, json, openStream } from '../test/fakeApi'
import viewFull from '../test/fixtures/view-full.json'

const okStatus = {
  appSet: { mode: 'static', source: '/etc/applications.yaml', loadedAt: '2026-07-22T09:41:10Z', stale: false },
  applications: 2,
}

/** One list row: the generated fixture with its releases stripped and axes set. */
function row(name: string, sync: SyncState, health: HealthState, drift = false): View {
  const view = clone(viewFull) as unknown as View
  view.spec.name = name
  view.status.sync.state = sync
  view.status.health.state = health
  delete view.status.releases
  delete view.status.error
  if (drift) view.status.drift = { state: 'detected', services: 1 }
  else delete view.status.drift
  return view
}

function serve(apps: View[]): void {
  controller({
    ...communityDiscovery(),
    '/api/v1/status': () => json(200, okStatus),
    '/api/v1/applications': () => json(200, { applications: apps }),
    '/api/v1/events': openStream,
  })
}

/** Signs in on the list, then clicks through to the overview. */
async function openOverview(): Promise<void> {
  render(<App />)
  await screen.findByTestId('application-count')
  fireEvent.click(screen.getByRole('link', { name: 'Overview' }))
  await screen.findByRole('heading', { name: 'Overview' })
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

describe('the fleet overview', () => {
  it('reads all systems nominal when every application is synced and healthy', async () => {
    serve([row('edge', 'synced', 'healthy'), row('ingress', 'synced', 'healthy')])
    await openOverview()

    expect(screen.getByText('all systems nominal')).toBeDefined()
    expect(screen.getByText('In sync')).toBeDefined()
    expect(screen.getByText('Healthy')).toBeDefined()
  })

  it('flags attention and surfaces the drift and reconcile-error counts', async () => {
    const failing = row('broken', 'out-of-sync', 'degraded')
    failing.status.error = 'clone failed'
    serve([row('edge', 'synced', 'healthy'), row('drifter', 'synced', 'healthy', true), failing])
    await openOverview()

    expect(screen.getByText('needs attention')).toBeDefined()
    expect(screen.getByText('Drifted')).toBeDefined()
    expect(screen.getByText('Reconcile errors')).toBeDefined()
  })
})
