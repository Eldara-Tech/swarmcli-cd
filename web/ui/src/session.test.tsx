// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The credential this tab did not collect (#217).
//
// A token is the tab's own — the login screen took it, auth/session.ts keeps it,
// and every assertion about it can be made from here. A companion's SSO session
// is a cookie the browser will not show to script, so the *only* evidence a
// browser has that it is signed in is what /ui/bootstrap.json says. Everything
// below is about that document being believed: the shell rendering at all, the
// capability document being read, and signing out reaching the controller
// rather than clearing something that was never in the tab.
//
// The lapse — a cookie the controller has stopped accepting — is in
// session-lapse.test.tsx, for the reason its header gives.

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from './App'
import { signOutPath } from './api/discovery'
import { setToken } from './auth/session'
import { clone, communityCapabilities, controller, json, openStream } from './test/fakeApi'
import viewFull from './test/fixtures/view-full.json'
import type { View } from './api/types'

/** What a licensed controller serves a browser that has completed an SSO login. */
const ssoSession = {
  login: [{ id: 'sso', label: 'Sign in with SSO', start: '/auth/login' }],
  session: { name: 'alice' },
}

/** The same controller before the login: the button, and no session. */
const ssoAnonymous = { login: ssoSession.login }

function licensed(): unknown {
  const capabilities = clone(communityCapabilities)
  capabilities.edition = 'business'
  capabilities.features.projects = true
  capabilities.features.sso = true
  return capabilities
}

function row(): View {
  const view = clone(viewFull) as unknown as View
  view.status.sync.state = 'synced'
  view.status.health.state = 'healthy'
  delete view.status.releases
  delete view.status.drift
  return view
}

/** A licensed controller, serving `bootstrap` to a browser with no bearer. */
function serve(bootstrap: unknown): void {
  controller({
    '/ui/bootstrap.json': () => json(200, bootstrap),
    '/api/v1/capabilities': () => json(200, licensed()),
    '/api/v1/status': () => json(200, { appSet: { mode: 'static', loadedAt: '2026-08-05T09:00:00Z', stale: false }, applications: 1 }),
    '/api/v1/applications': () => json(200, { applications: [row()] }),
    '/api/v1/events': openStream,
  })
}

beforeEach(() => {
  sessionStorage.clear()
  queryClient.clear()
  window.history.pushState({}, '', '/')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('a session the browser cannot see', () => {
  // The defect itself. Before #217 this rendered the login screen the operator
  // had just come back from, for ever: they pressed the button, authenticated
  // against their provider, and arrived at the same screen with nothing to say
  // what had gone wrong.
  it('signs the tab in with no token anywhere', async () => {
    serve(ssoSession)
    render(<App />)

    // The application list, which is where the acceptance criterion says a
    // completed login has to arrive — not merely a shell that mounted.
    expect(await screen.findByTestId('application-count')).toBeDefined()
    expect(sessionStorage.length).toBe(0)
  })

  // The second half of the same defect, and the one that would have survived a
  // fix to the first: the capability document was fetched only when a *token*
  // was present, so a browser signed in by cookie reached the shell and then
  // found every licensed control missing from the build that grants them.
  it('reads the capability document, so the licensed shells render', async () => {
    serve(ssoSession)
    render(<App />)

    expect(await screen.findByRole('link', { name: 'Projects' })).toBeDefined()
  })

  it('names the operator the identity provider returned', async () => {
    serve(ssoSession)
    render(<App />)

    expect(await screen.findByText('alice')).toBeDefined()
  })

  // Signing out of a cookie is not something this tab can do. Clearing the
  // store would leave the credential on the browser and the operator signed in
  // for another twelve hours, having been told they were not — so the control
  // is a link to the controller rather than a button.
  it('signs out through the controller', async () => {
    serve(ssoSession)
    render(<App />)

    const out = await screen.findByRole('link', { name: 'Sign out' })
    expect(out.getAttribute('href')).toBe(signOutPath)
    expect(screen.queryByRole('button', { name: 'Sign out' })).toBeNull()
  })

  // And the free build's control does not move. `session` is absent from every
  // document a build with no companion serves, so this is still the button it
  // has always been — the acceptance criterion of #178 applied to the header.
  it('leaves the token build signing out in the tab', async () => {
    setToken('good')
    serve({ login: [{ id: 'token', label: 'Admin token' }] })
    render(<App />)

    expect(await screen.findByRole('button', { name: 'Sign out' })).toBeDefined()
    expect(screen.queryByRole('link', { name: 'Sign out' })).toBeNull()
  })

  // A controller that advertises SSO and has not been logged into yet is the
  // ordinary before state, and it must still draw the button rather than a
  // shell: `session` absent means nobody is signed in, not "assume they are".
  it('draws the login screen when the document reports no session', async () => {
    serve(ssoAnonymous)
    render(<App />)

    expect(await screen.findByRole('link', { name: 'Sign in with SSO' })).toBeDefined()
    expect(screen.queryByRole('button', { name: 'Sign out' })).toBeNull()
  })
})

// The gate has to settle. It is asked before every render of the whole
// application, and it consults a query — so a gate that unmounts what it just
// mounted spins that query for ever and draws nothing at all, which is what
// gating on "a request is in flight" rather than "the controller has answered"
// did. Counted rather than asserted about a screen, because the screen looked
// fine in the case that worked and blank in the case that did not.
describe('the gate', () => {
  it('settles on a controller that cannot be asked', async () => {
    controller({ '/ui/bootstrap.json': () => json(404, { error: 'not found' }) })
    render(<App />)

    // The fallback the login screen makes for a controller it could not ask.
    expect(await screen.findByLabelText('Admin token')).toBeDefined()
    // Bounded rather than exact: an errored query refetches once for the
    // observer the login screen brings with it, and that is a settled two. What
    // this guards is the difference between a small number and an unbounded
    // one — the gate that unmounted the screen it had just mounted reached nine
    // hundred requests in under a second and drew nothing.
    expect(requestsFor('/ui/bootstrap.json')).toBeLessThan(5)
  })
})

function requestsFor(path: string): number {
  return vi.mocked(globalThis.fetch).mock.calls.filter(([url]) => typeof url === 'string' && url.startsWith(path)).length
}
