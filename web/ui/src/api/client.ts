// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Every request the UI makes goes through this file.
//
// It is one seam on purpose. The bearer header, the 401 rule and the unwrapping
// of the API's error body are each a thing that has to be true of every request
// including the ones nobody has written yet, and a screen that called fetch
// directly would be a screen where one of them silently is not. Tests fake
// globalThis.fetch, which puts all three *under* the seam and exercises them
// from every screen test rather than only from this file's own.

import { clearToken, getToken } from '../auth/session'

/** The endpoint the login screen probes; see verify. */
export const probePath = '/api/v1/status'

/** An HTTP failure carrying the status, so a caller can tell 401 from 502. */
export class ApiError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

/**
 * hasStatus reports whether a failed request answered with one status.
 *
 * react-query hands a screen an Error, by which point ApiError's type is gone,
 * so a screen that has something specific to say about one code has to ask. Two
 * do: a 403 on the diff or history tab is a token without a separately-grantable
 * action rather than a failure, and a 502 on the history is the swarm refusing
 * to be read rather than the controller being broken.
 */
export function hasStatus(error: unknown, status: number): boolean {
  return error instanceof ApiError && error.status === status
}

/**
 * authorized issues a request with the session's credential attached.
 *
 * A 401 clears the token before throwing, which is what returns the tab to the
 * login screen: useToken is subscribed to the store, so every component
 * re-renders with no credential and the shell is replaced. Doing it here rather
 * than in a query's error handler is what makes it true of the event stream and
 * of the sync button as well as of the reads.
 *
 * Nothing else is treated as an authentication failure. A 403 is a token that is
 * genuinely not permitted this action — an authorizer implementing projects —
 * and signing the operator out over it would lose a working credential.
 */
export async function authorized(path: string, init?: RequestInit): Promise<Response> {
  const response = await fetch(path, withBearer(init, getToken()))
  if (response.status === 401) {
    clearToken()
    throw new ApiError(401, 'the controller rejected the token')
  }
  return response
}

/** apiGet reads one JSON document. */
export async function apiGet<T>(path: string): Promise<T> {
  const response = await authorized(path)
  if (!response.ok) {
    throw new ApiError(response.status, await failureMessage(response))
  }
  return (await response.json()) as T
}

/** apiPost triggers work. The only endpoint that takes one today is sync. */
export async function apiPost<T>(path: string): Promise<T> {
  const response = await authorized(path, { method: 'POST' })
  if (!response.ok) {
    throw new ApiError(response.status, await failureMessage(response))
  }
  return (await response.json()) as T
}

/**
 * verify reports whether the controller accepts a token the operator has just
 * typed, without storing it first.
 *
 * The one request made with a credential that is not the session's, and the
 * reason it exists: storing the token first and letting the ordinary 401 path
 * clear it would unmount the login screen for as long as the request was in
 * flight and remount it with no idea why it was back — so a wrong token would
 * present as a form that cleared itself. The message a person needs is "that
 * token was rejected", and only a probe that has not yet signed anybody in can
 * say it.
 *
 * It probes the status endpoint because that is the cheapest guarded read: it
 * needs no application to exist, so it answers the same on a controller whose
 * app set has not loaded, and it is behind the `read` action every token that
 * can see anything already holds.
 */
export async function verify(candidate: string): Promise<boolean> {
  const response = await fetch(probePath, withBearer(undefined, candidate))
  if (response.status === 401) return false
  if (!response.ok) {
    throw new ApiError(response.status, await failureMessage(response))
  }
  return true
}

function withBearer(init: RequestInit | undefined, token: string | null): RequestInit {
  const headers = new Headers(init?.headers)
  if (token !== null) headers.set('Authorization', `Bearer ${token}`)
  return { ...init, headers }
}

/**
 * failureMessage prefers the API's own sentence to the status line.
 *
 * Read as text and then parsed, rather than with response.json(): the body is
 * only ours when the controller wrote it, and a reverse proxy in front of it
 * answers a 502 with an HTML page. json() on that throws inside the error path
 * and replaces a useful status with a parse error, which is the same failure Go
 * client.message documents from the other end of the same connection.
 */
async function failureMessage(response: Response): Promise<string> {
  const body = await response.text().catch(() => '')
  try {
    const parsed: unknown = JSON.parse(body)
    if (parsed !== null && typeof parsed === 'object' && 'error' in parsed) {
      const message: unknown = parsed.error
      if (typeof message === 'string' && message !== '') return message
    }
  } catch {
    // Not the controller's JSON. The status line below is all there is.
  }
  return `${response.status} ${response.statusText}`.trim()
}
