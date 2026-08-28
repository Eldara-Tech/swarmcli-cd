// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The topology tab, which shipped as a route with no test at all.
//
// D19 asks for a component test per screen, and this is a screen: it has its own
// path, its own empty state and a tree that reads three levels of the detail
// document. What it must not do is invent a fourth — ServiceStatus carries
// counts and no task list, so the pips below are a rendering of "running of
// desired" and not a claim about containers the controller never named.

import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import type { View } from '../api/types'
import { setToken } from '../auth/session'
import { clone, controller, json, openStream } from '../test/fakeApi'
import viewFull from '../test/fixtures/view-full.json'

function view(mutate: (v: View) => void = () => {}): View {
  const v = clone(viewFull) as unknown as View
  v.spec.name = 'edge'
  delete v.status.error
  mutate(v)
  return v
}

function serve(body: View): void {
  controller({
    '/api/v1/applications/edge': () => json(200, body),
    '/api/v1/events': openStream,
  })
}

beforeEach(() => {
  sessionStorage.clear()
  queryClient.clear()
  window.history.pushState({}, '', '/applications/edge/topology')
  setToken('good')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('the topology tab', () => {
  it('draws application, release and service as three levels', async () => {
    serve(
      view((v) => {
        const releases = v.status.releases
        if (releases === undefined) throw new Error('the fixture declares no releases')
        releases[0].name = 'traefik'
        const services = releases[0].services
        if (services === undefined || services === null) throw new Error('the fixture declares no services')
        services[0].name = 'traefik-proxy'
      }),
    )
    render(<App />)

    expect(await screen.findByText('traefik')).toBeDefined()
    expect(screen.getByText('traefik-proxy')).toBeDefined()
    // The application node names the application, and is labelled as one — so a
    // release sharing its name is still distinguishable from it.
    const app = screen.getByText('application').closest('.topo-node')
    expect(app).not.toBeNull()
    expect(within(app as HTMLElement).getByText('edge')).toBeDefined()
  })

  it('says nothing about resources for an application not yet reconciled', async () => {
    // Absent releases on this endpoint mean the controller has not reported,
    // which is the one thing absence can mean here; the tree must not render an
    // empty application as though it had nothing deployed.
    serve(
      view((v) => {
        delete v.status.releases
      }),
    )
    render(<App />)

    expect(await screen.findByText(/Not reconciled yet/)).toBeDefined()
    expect(document.querySelector('.topo')).toBeNull()
  })

  it('does not draw an unhealthy application in the all-clear tone', async () => {
    // The node's tone comes from the shared fold, so this is the same reading
    // the list's dot and the diagnostics score make. The fixture is degraded.
    serve(view())
    render(<App />)

    await screen.findByText('application')
    const app = screen.getByText('application').closest('.topo-node')
    expect(app?.className).toContain('topo-bad')
    expect(app?.className).not.toContain('topo-ok')
  })

  it('reads an application whose axes are unknown as neither clear nor failing', async () => {
    serve(
      view((v) => {
        v.status.sync.state = 'unknown'
        v.status.health.state = 'unknown'
        delete v.status.drift
      }),
    )
    render(<App />)

    await screen.findByText('application')
    const app = screen.getByText('application').closest('.topo-node')
    expect(app?.className).toContain('topo-info')
    expect(app?.className).not.toContain('topo-ok')
    expect(app?.className).not.toContain('topo-bad')
  })
})
