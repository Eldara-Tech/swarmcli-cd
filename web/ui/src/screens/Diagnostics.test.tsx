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
  appSet: { mode: 'static', loadedAt: '2026-07-22T09:41:10Z', stale: false },
  applications: 2,
}

function row(name: string, sync: SyncState, health: HealthState): View {
  const view = clone(viewFull) as unknown as View
  view.spec.name = name
  view.status.sync.state = sync
  view.status.health.state = health
  delete view.status.releases
  delete view.status.drift
  delete view.status.error
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

async function openDiagnostics(): Promise<void> {
  render(<App />)
  await screen.findByTestId('application-count')
  fireEvent.click(screen.getByRole('link', { name: 'Diagnostics' }))
  await screen.findByRole('heading', { name: 'Diagnostics' })
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

describe('the diagnostics screen', () => {
  it('scores integrity and lists the risk behind the score', async () => {
    // One of two applications is clear, so the score is 50, and the degraded one
    // is a critical risk named for its worst axis.
    serve([row('edge', 'synced', 'healthy'), row('ingress', 'out-of-sync', 'degraded')])
    await openDiagnostics()

    expect(screen.getByLabelText('Integrity score 50 out of 100')).toBeDefined()
    expect(screen.getByRole('heading', { name: 'ingress', level: 3 })).toBeDefined()
    expect(screen.getByText('Health is degraded')).toBeDefined()
  })

  it('reports no risks when the whole fleet is clear', async () => {
    serve([row('edge', 'synced', 'healthy'), row('ingress', 'synced', 'healthy')])
    await openDiagnostics()

    expect(screen.getByLabelText('Integrity score 100 out of 100')).toBeDefined()
    expect(screen.getByText('No risks detected.')).toBeDefined()
    expect(screen.getByText('Nominal')).toBeDefined()
    // The three all-clear lines are rendered from their own predicates, so an
    // all-clear fleet is the only thing that draws all three.
    expect(screen.getAllByText(/OK$/).map((el) => el.textContent)).toEqual(['SYNC OK', 'HEALTH OK', 'ENGINE OK'])
  })

  it('does not read an unreconciled application as clear', async () => {
    // Both axes at their zero member is what an application the controller has
    // accepted and not yet reconciled looks like. Counted as clear it scored a
    // controller that had reconciled nothing at 100/100, over a green ring, with
    // three ticks claiming its stacks matched their manifests.
    serve([row('edge', 'synced', 'healthy'), row('ingress', 'unknown', 'unknown')])
    await openDiagnostics()

    expect(screen.getByLabelText('Integrity score 50 out of 100')).toBeDefined()
    expect(screen.queryByText('Nominal')).toBeNull()
    expect(screen.getByText('Attention Needed')).toBeDefined()
    expect(screen.getByRole('heading', { name: 'ingress', level: 3 })).toBeDefined()
    expect(screen.getByText(/Not yet reconciled/)).toBeDefined()
    expect(screen.getByText('unknown')).toBeDefined()
    expect(screen.queryByText('No risks detected.')).toBeNull()
  })

  it('never says Nominal beside a critical risk', async () => {
    // The chip came off a score threshold: nine clear applications in ten put it
    // at 90, which read "Nominal" next to a red "1 Detected" for a degraded one.
    const views = Array.from({ length: 9 }, (_, i) => row(`ok-${i}`, 'synced', 'healthy'))
    views.push(row('broken', 'synced', 'degraded'))
    serve(views)
    await openDiagnostics()

    expect(screen.getByLabelText('Integrity score 90 out of 100')).toBeDefined()
    expect(screen.queryByText('Nominal')).toBeNull()
    expect(screen.getByText('Attention Needed')).toBeDefined()
    expect(screen.getByText('1 Detected')).toBeDefined()
  })

  it('claims nothing about a fleet with no applications in it', async () => {
    // Three ticks over an empty fleet were three claims about nothing.
    serve([])
    await openDiagnostics()

    expect(screen.getByText('No risks detected.')).toBeDefined()
    expect(screen.queryByText(/OK$/)).toBeNull()
  })
})
