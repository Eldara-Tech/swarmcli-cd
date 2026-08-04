// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The faked controller every screen test renders against.
//
// Installed at globalThis.fetch rather than at a mocked src/api/client.ts, so
// the bearer header, the 401 rule, the error unwrapping and the event stream's
// own parser all sit *under* the seam and are really run by every test here.
// Mocking the client would test the screens against a client that cannot be
// wrong.

import { vi } from 'vitest'

/** controller fakes the API: a response per path prefix. */
export function controller(routes: Record<string, () => Response>): void {
  // Longest prefix wins. The detail path extends the list path, so key order
  // would otherwise decide whether GET /api/v1/applications/edge was answered
  // by the detail route or by the list — silently, and differently per test.
  const patterns = Object.keys(routes).sort((a, b) => b.length - a.length)
  vi.stubGlobal(
    'fetch',
    // A string, because that is all src/api ever passes; see client.test.ts.
    vi.fn((url: string) => {
      const route = patterns.find((path) => url.startsWith(path))
      if (route === undefined) return Promise.reject(new Error(`nothing faked for ${url}`))
      return Promise.resolve(routes[route]())
    }),
  )
}

export function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status })
}

/** A stream that stays open and says nothing, which is a healthy controller. */
export function openStream(): Response {
  const body = {
    getReader: () => ({ read: () => new Promise<never>(() => {}) }),
  }
  return { ok: true, status: 200, body } as unknown as Response
}

/**
 * clone detaches a generated fixture so a test can vary it.
 *
 * The fixtures are marshalled from the Go types by application/fixtures_test.go
 * and are never edited by hand — a wire type that genuinely changed is
 * regenerated with the -update flag. A test that needs a different *value*
 * therefore starts from the fixture's shape and overrides, which is also what
 * keeps it failing when the shape moves.
 */
export function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T
}
