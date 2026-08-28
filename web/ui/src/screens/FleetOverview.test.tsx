// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import type { HealthState, SyncState } from '../api/enums'
import type { View } from '../api/types'
import { zeroTimestamp } from '../api/types'
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

/**
 * The number a named stat card is showing.
 *
 * Reached through the card rather than by text, because "2" appears all over
 * this screen and a bare getByText would assert that *something* renders it.
 */
function statValue(label: string): string {
  // Scoped to the stat row: "Applications" is also the rail's nav link, and a
  // document-wide getByText finds both.
  const row = document.querySelector('.stat-row')
  if (row === null) throw new Error('the overview rendered no stat row')
  const card = within(row as HTMLElement)
    .getByText(label)
    .closest('.stat-card')
  if (card === null) throw new Error(`no stat card labelled ${label}`)
  const value = card.querySelector('.stat-value')
  if (value === null) throw new Error(`stat card ${label} has no value`)
  // The unit ("/ 2") is a child span; the count is the first text node.
  return value.firstElementChild?.textContent ?? ''
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
    // The numbers, not the labels. Asserting "In sync" and "Healthy" asserted
    // two string literals in the JSX: both counters could render a hard-coded 0
    // and this suite stayed green.
    expect(statValue('In sync')).toBe('2')
    expect(statValue('Healthy')).toBe('2')
    expect(statValue('Applications')).toBe('2')
    expect(statValue('Drifted')).toBe('0')
    expect(statValue('Reconcile errors')).toBe('0')
  })

  it('flags attention and surfaces the drift and reconcile-error counts', async () => {
    const failing = row('broken', 'out-of-sync', 'degraded')
    failing.status.error = 'clone failed'
    serve([row('edge', 'synced', 'healthy'), row('drifter', 'synced', 'healthy', true), failing])
    await openOverview()

    expect(screen.getByText('needs attention')).toBeDefined()
    expect(statValue('Applications')).toBe('3')
    expect(statValue('In sync')).toBe('2')
    expect(statValue('Healthy')).toBe('2')
    expect(statValue('Drifted')).toBe('1')
    expect(statValue('Reconcile errors')).toBe('1')
  })

  it('counts an unchecked drift axis as unchecked rather than as zero', () => {
    // Manifest mode never asks, so `drift` is absent. Counting that as "no
    // drift" would report a fleet nobody looked at as a fleet that is clean.
    serve([row('edge', 'synced', 'healthy'), row('ingress', 'synced', 'healthy')])
    return openOverview().then(() => {
      expect(statValue('Drifted')).toBe('0')
      expect(screen.getByText('2 not checked')).toBeDefined()
    })
  })

  it('names an unwired application set as unwired, never as static', () => {
    // `mode || 'static'` reported a real mode value for a controller with no
    // source wired, contradicting the "none wired" the same card renders below.
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, { appSet: { mode: '', loadedAt: zeroTimestamp, stale: false }, applications: 0 }),
      '/api/v1/applications': () => json(200, { applications: [row('edge', 'synced', 'healthy')] }),
      '/api/v1/events': openStream,
    })
    return openOverview().then(() => {
      expect(screen.queryByText('static')).toBeNull()
      expect(screen.getAllByText('none wired').length).toBeGreaterThan(0)
    })
  })

  it('refuses "all systems nominal" while the application set is stale', () => {
    // Every application below can be green and still describe a set the
    // controller is refusing: AppSetStatus.Stale is the field a UI colours, and
    // this screen was not reading it at all.
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () =>
        json(200, {
          appSet: { ...okStatus.appSet, stale: true, error: 'applications.yaml: line 4: unknown field' },
          applications: 2,
        }),
      '/api/v1/applications': () => json(200, { applications: [row('edge', 'synced', 'healthy'), row('ingress', 'synced', 'healthy')] }),
      '/api/v1/events': openStream,
    })
    return openOverview().then(() => {
      expect(screen.queryByText('all systems nominal')).toBeNull()
      expect(screen.getByText('needs attention')).toBeDefined()
      expect(screen.getByText('application set: stale')).toBeDefined()
    })
  })
})
