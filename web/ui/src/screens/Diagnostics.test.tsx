// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import type { HealthState, SyncState } from '../api/enums'
import type { View } from '../api/types'
import { setToken } from '../auth/session'
import { clone, communityDiscovery, controller, discoveryWith, json, openStream } from '../test/fakeApi'
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
  const isHealthy = apps.every((a) => a.status.health.state === 'healthy' && a.status.sync.state === 'synced')
  const defaultDiag = {
    score: isHealthy ? 100 : 50,
    tone: isHealthy ? 'ok' : 'bad',
    clearCount: apps.filter((a) => a.status.health.state === 'healthy').length,
    totalCount: 4,
    risks: isHealthy
      ? []
      : apps
          .filter((a) => a.status.health.state !== 'healthy')
          .map((a) => ({
            id: `health-${a.spec.name}`,
            severity: 'bad',
            application: a.spec.name,
            title: a.spec.name,
            summary: 'Health is degraded',
          })),
    checks: [
      { id: 'check-1', name: 'GitOps Manifest Convergence', passed: isHealthy, detail: 'Manifests in sync' },
    ],
  }

  const nodes = {
    swarm: 'local',
    nodes: [
      {
        id: 'node-01',
        hostname: 'swarm-mgr-1',
        role: 'manager',
        availability: 'active',
        status: 'ready',
        engineVersion: '27.5.1',
        addr: '127.0.0.1',
        leader: true,
        tasksRunning: 2,
        tasksDesired: 2,
      },
    ],
  }

  controller({
    ...communityDiscovery(),
    '/api/v1/status': () => json(200, okStatus),
    '/api/v1/applications': () => json(200, { applications: apps }),
    '/api/v1/diagnostics': () => json(200, defaultDiag),
    '/api/v1/nodes': () => json(200, nodes),
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
    serve([row('edge', 'synced', 'healthy'), row('ingress', 'out-of-sync', 'degraded')])
    await openDiagnostics()

    expect(screen.getByLabelText('Integrity score 50 out of 100')).toBeDefined()
    expect(screen.getByRole('heading', { name: 'ingress', level: 3 })).toBeDefined()
    expect(screen.getByText('Health is degraded')).toBeDefined()
    expect(screen.getByText('swarm-mgr-1')).toBeDefined()
  })

  it('reports no risks when the whole fleet is clear', async () => {
    serve([row('edge', 'synced', 'healthy'), row('ingress', 'synced', 'healthy')])
    await openDiagnostics()

    expect(screen.getByLabelText('Integrity score 100 out of 100')).toBeDefined()
    expect(screen.getByText('All Checks Passed')).toBeDefined()
    expect(screen.getByText('swarm-mgr-1')).toBeDefined()
  })

  it('does not call an unreported application a warning, or the fleet nominal', async () => {
    // The server's third severity. Rendered through the old two-way branch it
    // was labelled "warning" — a finding nobody made — and the chip came off a
    // tone that read ok at any score of 90 or more. Both now come from the risks.
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [row('edge', 'synced', 'healthy')] }),
      '/api/v1/diagnostics': () =>
        json(200, {
          score: 95,
          tone: 'warn',
          clearCount: 3,
          totalCount: 4,
          risks: [
            {
              id: 'health-edge',
              severity: 'unknown',
              application: 'edge',
              title: 'Application \'edge\' has not been reported on',
              summary: 'The controller has not reported a health state for this application yet.',
            },
          ],
          checks: [{ id: 'c1', name: 'GitOps Manifest Convergence', passed: true, detail: '1 of 1 in sync' }],
        }),
      '/api/v1/nodes': () => json(200, { swarm: 'local', nodes: [] }),
      '/api/v1/events': openStream,
    })
    await openDiagnostics()

    expect(screen.getByText('unknown')).toBeDefined()
    expect(screen.queryByText('warning')).toBeNull()
    expect(screen.queryByText('Nominal')).toBeNull()
    expect(screen.getByText('Attention Needed')).toBeDefined()
  })

  it('says the build does not report nodes rather than drawing an empty matrix', async () => {
    // /nodes answered 200 with one invented manager — swarm-manager-01 at
    // 127.0.0.1 on engine 27.5.1 — whenever the reconciler had no node lister,
    // which was then every build. Since #260 this build has one, so the state
    // under test is the other reachable 501: the capability document says the
    // seam is wired and the destination's backend still cannot answer.
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [row('edge', 'synced', 'healthy')] }),
      '/api/v1/diagnostics': () =>
        json(200, { score: 100, tone: 'ok', clearCount: 4, totalCount: 4, risks: [], checks: [] }),
      '/api/v1/nodes': () => json(501, { error: 'this controller does not report swarm node telemetry' }),
      '/api/v1/events': openStream,
    })
    await openDiagnostics()

    expect(screen.getByText(/does not report swarm node telemetry/)).toBeDefined()
    expect(screen.getByText('unavailable')).toBeDefined()
    for (const invented of ['swarm-manager-01', '27.5.1', '127.0.0.1']) {
      expect(screen.queryByText(invented)).toBeNull()
    }
  })
})

describe('the node matrix is gated on the build reporting one', () => {
  const diagnosticsOk = () =>
    json(200, { score: 100, tone: 'ok', clearCount: 4, totalCount: 4, risks: [], checks: [] })

  // The console used to draw the heading whatever the build could do, so an
  // operator read "Swarm Node Topology & Health Matrix" and then an apology
  // under it. A card that is only ever an apology is not a card.
  it('leaves the card out entirely when the build reports no roster', async () => {
    const asked = vi.fn(() => json(501, { error: 'this controller does not report swarm node telemetry' }))
    controller({
      ...discoveryWith({ nodes: false }),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [row('edge', 'synced', 'healthy')] }),
      '/api/v1/diagnostics': diagnosticsOk,
      '/api/v1/nodes': asked,
      '/api/v1/events': openStream,
    })
    await openDiagnostics()

    expect(screen.queryByText(/Swarm Node Topology/)).toBeNull()
    expect(screen.queryByText(/does not report swarm node telemetry/)).toBeNull()
    // And not merely hidden: a card that is not drawn should not have cost a
    // round trip to a controller that already said it cannot answer.
    expect(asked).not.toHaveBeenCalled()
  })

  it('draws the card when the build reports one', async () => {
    controller({
      ...discoveryWith({ nodes: true }),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [row('edge', 'synced', 'healthy')] }),
      '/api/v1/diagnostics': diagnosticsOk,
      '/api/v1/nodes': () =>
        json(200, {
          swarm: '',
          nodes: [
            {
              id: 'n1',
              hostname: 'mgr-1',
              role: 'manager',
              availability: 'active',
              status: 'ready',
              engineVersion: '28.5.2',
              addr: '10.0.0.1',
              leader: true,
              tasksRunning: 2,
              tasksDesired: 2,
            },
          ],
        }),
      '/api/v1/events': openStream,
    })
    await openDiagnostics()

    expect(await screen.findByText(/Swarm Node Topology/)).toBeDefined()
    expect(await screen.findByText('mgr-1')).toBeDefined()
  })
})
