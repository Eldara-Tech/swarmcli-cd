// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The badge, across all five statuses of feature.Status.
//
// Rendered through <App /> rather than in isolation, because what the badge has
// to get right is not its own markup: it is that a build with no licence to
// report draws nothing at all, and that is a property of the header it sits in.

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import { setToken } from '../auth/session'
import {
  clone,
  communityCapabilities,
  communityDiscovery,
  controller,
  json,
  openStream,
} from '../test/fakeApi'

const okStatus = {
  appSet: { mode: 'static', loadedAt: '2026-07-22T09:41:10Z', stale: false },
  applications: 0,
}

/** An instant `days` before now, so an assertion about "4 days ago" needs no clock control. */
function daysBeforeNow(days: number): string {
  return new Date(Date.now() - days * 86_400_000).toISOString()
}

/**
 * The capability document of a licensed build whose licence is in `status`.
 *
 * A plain string rather than LicenceStatus, so the last test below can send a
 * status this build has never heard of — which is what a controller ahead of it
 * does.
 */
function licensed(status: string, expiresAt: string | null): unknown {
  const capabilities = clone(communityCapabilities) as Record<string, unknown>
  capabilities.edition = 'business'
  capabilities.licence = { tier: 'be', status, expiresAt }
  return capabilities
}

async function show(capabilities: unknown): Promise<void> {
  controller({
    ...communityDiscovery(),
    '/api/v1/capabilities': () => json(200, capabilities),
    '/api/v1/status': () => json(200, okStatus),
    '/api/v1/applications': () => json(200, { applications: [] }),
    '/api/v1/events': openStream,
  })
  render(<App />)
  await screen.findByTestId('application-count')
}

/** The badge's whole text, chip and detail together. */
async function badgeText(): Promise<string> {
  return (await screen.findByTestId('licence-badge')).textContent ?? ''
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

describe('the licence badge', () => {
  // The acceptance criterion, from the badge's own side: an Apache-2.0 build
  // has no licensed module and therefore no licence, and a chip reading
  // "community" would be a control that can never say anything else.
  it('renders nothing in a build with no licence to report', async () => {
    await show(communityCapabilities)

    expect(screen.queryByTestId('licence-badge')).toBeNull()
  })

  it('reads valid, with what expires and when', async () => {
    await show(licensed('valid', '2027-03-01T00:00:00Z'))

    const text = await badgeText()
    expect(text).toContain('licence valid')
    expect(text).toContain('be')
    expect(text).toContain('expires')
  })

  it('says perpetual rather than nothing for a licence with no expiry', async () => {
    await show(licensed('valid', null))

    expect(await badgeText()).toContain('perpetual')
  })

  // The one the fifth status exists for (D25). A licence in its grace period is
  // granting features and is not valid, so a badge that read healthy would stay
  // green until the morning everything stopped.
  it('does not read healthy in the grace period, and says how long ago it expired', async () => {
    await show(licensed('grace', daysBeforeNow(4)))

    const text = await badgeText()
    expect(text).toContain('grace period')
    expect(text).toContain('expired 4 days ago')
    expect(text).toContain('renew now')
    expect(text).not.toContain('valid')
    // Warn, not good: the features are still on, and there has to be somewhere
    // louder to go when they are not.
    expect(screen.getByText('grace period').className).toBe('chip chip-warn')
  })

  it('reads expired, and says the features are off', async () => {
    await show(licensed('expired', daysBeforeNow(1)))

    const text = await badgeText()
    expect(text).toContain('licence expired')
    expect(text).toContain('expired 1 day ago')
    expect(text).toContain('features are off')
    expect(screen.getByText('licence expired').className).toBe('chip chip-bad')
  })

  it('reads invalid, and points at the vendor rather than at the deployment', async () => {
    await show(licensed('invalid', null))

    expect(await badgeText()).toContain('licence invalid')
    expect(await badgeText()).toContain('ask for a replacement')
  })

  it('reads absent as something to install, not as something broken', async () => {
    await show(licensed('absent', null))

    const text = await badgeText()
    expect(text).toContain('no licence')
    expect(text).toContain('install one')
  })

  // A controller ahead of this build reports a sixth status. It has to render
  // as something: falling through every case would leave a header with a
  // licence the operator cannot see at all.
  it('renders a status this build has never heard of', async () => {
    await show(licensed('revoked', null))

    expect(await badgeText()).toContain('licence revoked')
  })
})
