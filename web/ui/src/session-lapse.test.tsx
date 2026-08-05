// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// A cookie session the controller has stopped accepting (#217).
//
// Its own file, and the reason is the thing under test. auth/session.ts records
// a refused cookie in module state rather than anywhere persistent, because the
// refusal describes this document: reaching /auth/logout, or completing a
// login, is a navigation, and a navigation starts a new document with the
// record back at false — which is exactly when it should be. Nothing resets it
// short of that, so the only honest way to run this beside the tests in
// session.test.tsx is a fresh module registry, which is what a separate file
// is. A reset exported for the tests would be a production API that exists
// because of them.
//
// What it must not be is a reload. The tab has to return to the login screen
// without one, for the same reason the token path does: a reload is the one
// remedy that can loop, and a login screen that reloads itself is worse than a
// shell that does nothing.

import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { App, queryClient } from './App'
import { communityCapabilities, clone, controller, json, openStream } from './test/fakeApi'

beforeEach(() => {
  sessionStorage.clear()
  queryClient.clear()
  window.history.pushState({}, '', '/')
})

afterEach(() => {
  vi.unstubAllGlobals()
})

it('returns the tab to the login screen when the cookie stops working', async () => {
  const licensed = clone(communityCapabilities)
  licensed.edition = 'business'
  licensed.features.sso = true

  // The document still names a session — which is the case worth testing. A
  // controller that had already stopped reporting one would close the gate on
  // its own, and the tab would never have to notice the 401 at all.
  controller({
    '/ui/bootstrap.json': () => json(200, { login: [{ id: 'sso', label: 'Sign in with SSO', start: '/auth/login' }], session: { name: 'alice' } }),
    // The lapse: the cookie was good enough for the public document and is not
    // good enough for a guarded read. In a real controller that is an SSO
    // entitlement that expired under a running process — the authorizer stops
    // honouring the cookie and falls back to a bearer this browser has never
    // held.
    '/api/v1/capabilities': () => json(401, { error: 'unauthorized' }),
    '/api/v1/status': () => json(401, { error: 'unauthorized' }),
    '/api/v1/applications': () => json(401, { error: 'unauthorized' }),
    '/api/v1/events': openStream,
  })

  render(<App />)

  // Not the shell, and not a blank page: the screen that offers a way back in.
  expect(await screen.findByRole('link', { name: 'Sign in with SSO' })).toBeDefined()
  expect(screen.queryByRole('button', { name: 'Sign out' })).toBeNull()
  expect(screen.queryByRole('link', { name: 'Sign out' })).toBeNull()
})
