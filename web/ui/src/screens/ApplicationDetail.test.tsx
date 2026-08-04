// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The application detail, rendered against the faked controller.
//
// The fixture's placeholder strings are all the word "name", which would make
// half the assertions below match something they did not mean. Every helper
// therefore renames — and renames only: the shape stays the one
// application/fixtures_test.go marshalled from the Go View, because the shape is
// what drifts.

import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import type { FieldDrift, ReleaseStatus, ResourceDrift, ServiceDrift, View } from '../api/types'
import { setToken } from '../auth/session'
import { clone, controller, json, openStream } from '../test/fakeApi'
import viewFull from '../test/fixtures/view-full.json'

function detail(): View {
  const view = clone(viewFull) as unknown as View
  view.spec.name = 'edge'
  // The fixture populates every field, including a reconcile error, which no
  // real document combines with a fresh plan. Cleared here so that a test
  // asserting on one banner is not reading another one's.
  delete view.status.error
  const release = firstRelease(view)
  release.name = 'traefik'
  release.chart = 'charts/traefik'
  release.version = '1.2.3'
  const services = release.services
  if (services !== undefined) services[0].name = 'traefik_web'
  const drift = release.drift
  if (drift?.services !== undefined) drift.services[0].name = 'traefik_web'
  if (drift?.resources !== undefined) drift.resources[0].name = 'traefik_default'
  return view
}

/** Throws rather than defaulting: a fixture with no releases would silently
 *  turn most of this suite into assertions about an empty screen. */
function firstRelease(view: View): ReleaseStatus {
  const releases = view.status.releases
  if (releases === undefined || releases.length === 0) throw new Error('the fixture declares no releases')
  return releases[0]
}

function serviceDrift(view: View): ServiceDrift[] {
  const services = firstRelease(view).drift?.services
  if (services === undefined) throw new Error('the fixture declares no service drift')
  return services
}

function fieldDrift(view: View): FieldDrift[] {
  const fields = serviceDrift(view)[0].fields
  if (fields === undefined) throw new Error('the fixture declares no field drift')
  return fields
}

function resourceDrift(view: View): ResourceDrift[] {
  const resources = firstRelease(view).drift?.resources
  if (resources === undefined) throw new Error('the fixture declares no resource drift')
  return resources
}

function ancestor(node: HTMLElement, selector: string): HTMLElement {
  const found = node.closest(selector)
  if (found === null) throw new Error(`no ${selector} above ${node.tagName}`)
  return found as HTMLElement
}

/** Queries scoped to the drift panel. Unscoped, "live" also matches the shell's
 *  own live indicator, and a release name matches both its tree node and its
 *  drift section. */
async function driftPanel() {
  return within(ancestor(await screen.findByRole('heading', { name: 'Drift' }), 'section'))
}

function serve(view: View): void {
  controller({
    '/api/v1/applications/edge': () => json(200, view),
    '/api/v1/events': openStream,
  })
}

beforeEach(() => {
  sessionStorage.clear()
  queryClient.clear()
  window.history.pushState({}, '', '/applications/edge')
  setToken('good')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('the header', () => {
  it('carries sync, health, services and the last sync', async () => {
    serve(detail())
    render(<App />)

    const title = await screen.findByRole('heading', { name: 'edge', level: 1 })
    const header = within(ancestor(title, 'header'))
    expect(header.getByText('out-of-sync')).toBeDefined()
    expect(header.getByText('degraded')).toBeDefined()
    expect(screen.getAllByText('1/1').length).toBeGreaterThan(0)
    expect(screen.getByText(/succeeded at/)).toBeDefined()
  })

  it('says the whole document is stale when the last reconcile failed', async () => {
    const view = detail()
    view.status.error = 'clone https://git.example/apps: connection refused'
    serve(view)
    render(<App />)

    expect(await screen.findByText('The last reconcile failed')).toBeDefined()
    expect(screen.getByText(/last successful observation/)).toBeDefined()
    expect(screen.getByText(/connection refused/)).toBeDefined()
  })
})

describe('the release tree', () => {
  // Absent is "not requested", never "none": the engine rejects a release file
  // declaring no releases, so on this endpoint the only thing absence can mean
  // is that the controller has not got there yet.
  it('reads absent releases as not reconciled, not as no releases', async () => {
    const view = detail()
    delete view.status.releases
    serve(view)
    render(<App />)

    expect(await screen.findByText(/Not reconciled yet/)).toBeDefined()
    expect(screen.queryByText(/no releases/i)).toBeNull()
  })

  it('renders a release with no services as a release with no services', async () => {
    const view = detail()
    delete firstRelease(view).services
    serve(view)
    render(<App />)

    const node = within(ancestor(await screen.findByText('No services.'), 'li'))
    expect(node.getByRole('heading', { name: 'traefik', level: 3 })).toBeDefined()
  })

  it('stops at services, which is where the API stops', async () => {
    serve(detail())
    render(<App />)

    expect(await screen.findByRole('rowheader', { name: 'traefik_web' })).toBeDefined()
    expect(screen.getByRole('columnheader', { name: 'Replicas' })).toBeDefined()
  })
})

describe('the compatibility gate', () => {
  // Blocking, and above everything: reconcile.checkCompat refuses the whole
  // plan rather than the incompatible part of it, so nothing below is going to
  // be deployed until the engine moves.
  it('puts an incompatible release at the top of the screen with the two versions', async () => {
    serve(detail())
    render(<App />)

    expect(await screen.findByText('This plan will not be applied')).toBeDefined()
    const notice = within(screen.getByRole('alert'))
    expect(notice.getByText('required')).toBeDefined()
    expect(notice.getByText('engine')).toBeDefined()
  })

  // checkCompat exempts a release the plan would leave alone: it is already
  // deployed and applying will not touch it. Claiming the plan is refused would
  // be a wrong answer to the only question the banner exists to answer.
  it('does not claim the plan is refused when the incompatible release is unchanged', async () => {
    const view = detail()
    firstRelease(view).action = 'unchanged'
    serve(view)
    render(<App />)

    // Still reported, because it is still a finding — just not a blocking one.
    expect(await screen.findByText('incompatible')).toBeDefined()
    expect(screen.queryByText('This plan will not be applied')).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

describe('the drift panel', () => {
  it('goes down to field, desired and live, and counts what it could not list', async () => {
    serve(detail())
    render(<App />)

    const panel = await driftPanel()
    expect(panel.getByText('field')).toBeDefined()
    expect(panel.getByText('desired')).toBeDefined()
    expect(panel.getByText('live')).toBeDefined()
    expect(panel.getByText('(and 1 more)')).toBeDefined()
  })

  it('marks an orphaned service apart from a bare unexpected one', async () => {
    serve(detail())
    render(<App />)

    await screen.findByRole('heading', { name: 'Drift' })
    expect(screen.getAllByText('modified (orphaned)').length).toBeGreaterThan(0)
    expect(screen.getByText(/go on the next sync/)).toBeDefined()
  })

  // No fields is not "nothing differs": the service itself is the finding.
  it('renders a service with no fields as the finding', async () => {
    const view = detail()
    const services = serviceDrift(view)
    delete services[0].fields
    delete services[0].truncated
    services[0].reason = 'missing'
    services[0].orphaned = false
    serve(view)
    render(<App />)

    await screen.findByRole('heading', { name: 'Drift' })
    const finding = within(ancestor(screen.getByRole('cell', { name: 'traefik_web' }), 'tr'))
    expect(finding.getByText('missing')).toBeDefined()
    expect(screen.queryByText('(and 1 more)')).toBeNull()
  })

  // The one reason a sync will not put it right: Swarm reverted the spec the
  // repository asks for, so the remedy is a commit and not a button.
  it('says a rolled-back service will not be re-applied by a sync', async () => {
    const view = detail()
    const services = serviceDrift(view)
    services[0].reason = 'rolled-back'
    services[0].message = 'update paused due to failure or early termination of task abc123'
    serve(view)
    render(<App />)

    await screen.findByRole('heading', { name: 'Drift' })
    expect(screen.getByText(/a sync will not re-apply it/)).toBeDefined()
    expect(screen.getByText(/the remedy is a commit/)).toBeDefined()
    expect(screen.getByText(/early termination of task abc123/)).toBeDefined()
  })

  // The API reduces an environment difference to set/absent/changed before it
  // is served, because this view is readable by anyone holding read scope. The
  // screen's job is to render what arrived and add nothing back.
  it('shows an environment difference without its value, and says why', async () => {
    const view = detail()
    fieldDrift(view)[0] = { field: 'env[DATABASE_URL]', desired: 'set', live: 'changed' }
    serve(view)
    render(<App />)

    await screen.findByRole('heading', { name: 'Drift' })
    expect(screen.getByText('env[DATABASE_URL]')).toBeDefined()
    expect(screen.getByText('changed')).toBeDefined()
    expect(screen.getByText(/value is never sent/)).toBeDefined()
  })

  it('renders a resource kind verbatim, because ResourceKind has no unknown fallback', async () => {
    const view = detail()
    resourceDrift(view)[0].kind = 'volume-claim'
    serve(view)
    render(<App />)

    await screen.findByRole('heading', { name: 'Drift' })
    expect(screen.getByText('volume-claim')).toBeDefined()
  })

  it('renders nothing at all for a release with nothing to report', async () => {
    const view = detail()
    delete firstRelease(view).drift
    serve(view)
    render(<App />)

    expect(await screen.findByRole('heading', { name: 'traefik', level: 3 })).toBeDefined()
    expect(screen.queryByRole('heading', { name: 'Drift' })).toBeNull()
  })
})
