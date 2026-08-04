// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The login screen, against the document that decides what it draws.
//
// The fake is installed at globalThis.fetch, so the request this suite cares
// about most — the one made with no credential — is really issued and can be
// read back off the mock rather than asserted about a mocked client.

import { QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import { bootstrapPath } from '../api/discovery'
import { setToken } from '../auth/session'
import { communityDiscovery, controller, json } from '../test/fakeApi'
import { Login } from './Login'

const sso = { id: 'sso', label: 'Sign in with SSO', start: '/auth/login' }
const token = { id: 'token', label: 'Admin token' }

function advertising(...login: unknown[]): void {
  controller({ [bootstrapPath]: () => json(200, { login }) })
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

describe('the login screen', () => {
  it('draws the box for a credential that is typed in', async () => {
    advertising(token)
    render(<App />)

    expect(await screen.findByLabelText('Admin token')).toBeDefined()
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeDefined()
    expect(screen.queryByRole('link')).toBeNull()
  })

  it('draws a link for a method that carries a start path', async () => {
    advertising(token, sso)
    render(<App />)

    const start = await screen.findByRole('link', { name: 'Sign in with SSO' })
    // An anchor rather than a button: the browser has to leave this page for
    // the authorizer's own route, and the credential comes back through it.
    expect(start.getAttribute('href')).toBe('/auth/login')
    expect(screen.getByLabelText('Admin token')).toBeDefined()
  })

  // The point of the whole document: a deployment that has moved to SSO must
  // stop offering a box for a credential it no longer issues. A UI that kept
  // the box would be asking operators to keep a shared admin token alive.
  it('offers no box when the authorizer advertises only SSO', async () => {
    advertising(sso)
    render(<App />)

    expect(await screen.findByRole('link', { name: 'Sign in with SSO' })).toBeDefined()
    expect(screen.queryByLabelText('Admin token')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Sign in' })).toBeNull()
  })

  // Taken at its word, rather than papered over with a token box the authorizer
  // has just said it does not accept: a box that can only ever be rejected is a
  // worse answer than saying there is no way in from here.
  it('says so when the authorizer advertises no method at all', async () => {
    advertising()
    render(<App />)

    expect(await screen.findByRole('alert')).toBeDefined()
    expect(screen.queryByLabelText('Admin token')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Sign in' })).toBeNull()
  })

  // A document that could not be read is not an authorizer refusing every
  // method — it is one that could not be asked, and the operator of a working
  // controller behind a proxy that does not forward /ui/ must still get in.
  it('falls back to the box when the document cannot be read', async () => {
    controller({ [bootstrapPath]: () => json(404, { error: 'not found' }) })
    render(<App />)

    expect(await screen.findByLabelText('Admin token')).toBeDefined()
  })

  // The rule the endpoint exists for. bootstrap.json is what a browser reads
  // *before* it has a credential, so it must be read without one — and the
  // check is made with a token in the session precisely because a login screen
  // that has none would pass it by accident.
  it('reads the public document with no credential attached', async () => {
    setToken('good')
    controller(communityDiscovery())
    render(
      <QueryClientProvider client={queryClient}>
        <Login />
      </QueryClientProvider>,
    )
    await screen.findByLabelText('Admin token')

    // Narrowed rather than stringified: fetch takes a Request or a URL too, and
    // src/api only ever passes a string — see api/client.test.ts.
    const call = vi.mocked(globalThis.fetch).mock.calls.find(([url]) => url === bootstrapPath)
    expect(call).toBeDefined()
    // No init at all, which is stronger than an absent header: there is nowhere
    // for a bearer to have been put.
    expect(call?.[1]).toBeUndefined()
  })
})
