// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { getToken, setToken } from '../auth/session'
import { ApiError, apiGet, authorized, probePath, verify } from './client'

/** requests records what the faked fetch was asked for. */
let requests: { url: string; init: RequestInit | undefined }[] = []

function fakeFetch(answer: (url: string) => Response) {
  // Typed as a string because that is all this module ever passes: every call
  // goes through withBearer with a path. A Request object would stringify to
  // "[object Object]" and the assertions would quietly stop meaning anything.
  const spy = vi.fn((url: string, init?: RequestInit) => {
    requests.push({ url, init })
    return Promise.resolve(answer(url))
  })
  vi.stubGlobal('fetch', spy)
  return spy
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** bearer reads the header off a recorded request. */
function bearer(index = 0): string | null {
  return new Headers(requests[index].init?.headers).get('Authorization')
}

beforeEach(() => {
  requests = []
  sessionStorage.clear()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the fetch wrapper', () => {
  it('attaches the session credential to every request', async () => {
    setToken('s3cret')
    fakeFetch(() => json(200, { applications: [] }))

    await apiGet('/api/v1/applications')

    expect(bearer()).toBe('Bearer s3cret')
  })

  it('sends no header at all when there is no credential', async () => {
    fakeFetch(() => json(200, {}))

    await apiGet('/api/v1/status')

    // Not `Bearer `. An empty credential is a request the controller should
    // answer 401, which is what returns the tab to the login screen; a header
    // that is present and empty invites a proxy to treat it as authenticated.
    expect(bearer()).toBeNull()
  })

  it('clears the credential on a 401 and says why', async () => {
    setToken('stale')
    fakeFetch(() => json(401, { error: 'unauthorized' }))

    await expect(apiGet('/api/v1/applications')).rejects.toBeInstanceOf(ApiError)

    // The whole 401 contract: the store is empty, so useToken re-renders the
    // application with no credential and the login screen replaces the shell.
    // Nobody sees an error page.
    expect(getToken()).toBeNull()
  })

  it('leaves the credential alone on a 403, which is a different answer', async () => {
    setToken('narrow')
    fakeFetch(() => json(403, { error: 'forbidden' }))

    await expect(apiGet('/api/v1/applications/edge/diff')).rejects.toMatchObject({ status: 403 })

    // A token that may not do one thing is still a working token. Signing the
    // operator out over it would lose a credential that works everywhere else.
    expect(getToken()).toBe('narrow')
  })

  it('reports the controller error text rather than the status line', async () => {
    setToken('t')
    fakeFetch(() => json(404, { error: 'no such application' }))

    await expect(apiGet('/api/v1/applications/gone')).rejects.toThrow('no such application')
  })

  it('falls back to the status line when the body is not the controller JSON', async () => {
    setToken('t')
    // What a reverse proxy in front of a restarting controller answers.
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response('<html><body>502 Bad Gateway</body></html>', {
            status: 502,
            statusText: 'Bad Gateway',
          }),
        ),
      ),
    )

    await expect(apiGet('/api/v1/status')).rejects.toThrow('502 Bad Gateway')
  })

  it('hands a streaming response back without reading it', async () => {
    setToken('t')
    fakeFetch(() => new Response('event: sync-started\n', { status: 200 }))

    const response = await authorized('/api/v1/events', { headers: { Accept: 'text/event-stream' } })

    expect(response.ok).toBe(true)
    expect(new Headers(requests[0].init?.headers).get('Accept')).toBe('text/event-stream')
  })
})

describe('the login probe', () => {
  it('asks the status endpoint with the typed credential, not the stored one', async () => {
    setToken('the-old-one')
    fakeFetch(() => json(200, { applications: 0 }))

    expect(await verify('the-new-one')).toBe(true)
    expect(requests[0].url).toBe(probePath)
    expect(bearer()).toBe('Bearer the-new-one')
  })

  it('reports a rejected token without touching the session', async () => {
    setToken('still-good')
    fakeFetch(() => json(401, { error: 'unauthorized' }))

    expect(await verify('wrong')).toBe(false)
    // The reason verify exists: it must not sign anybody out — nor in — while
    // it is asking. A 401 here is an answer, not an authentication failure.
    expect(getToken()).toBe('still-good')
  })

  it('raises anything that is not an accept-or-reject', async () => {
    fakeFetch(() => json(502, { error: 'controller unreachable' }))

    await expect(verify('t')).rejects.toThrow('controller unreachable')
  })
})
