// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { afterEach, describe, expect, it, vi } from 'vitest'

import { getToken, setToken } from '../auth/session'
import { backoffFor, runEventStream, type ControllerEvent, type StreamState } from './events'

// Two frames, a proxy keepalive comment between them, and three characters that
// are not one byte each: an accented letter (2 bytes), an ellipsis (3) and an
// emoji (4). The free-text fields are exactly where a non-ASCII character turns
// up in real life — a commit message, an operator's display name — and a decoder
// used without { stream: true } turns a character split across two chunks into
// two replacement characters and the frame's JSON into a parse error.
const wire =
  'event: sync-started\n' +
  'data: {"application":"edge","swarm":"","type":"sync-started","revision":"9f3c1ab","at":"2026-07-22T09:41:10Z"}\n' +
  '\n' +
  ': keepalive\n' +
  '\n' +
  'event: sync-failed\n' +
  'data: {"application":"edge","swarm":"","type":"sync-failed","message":"rendering échoué… 🚀","actor":"alice","at":"2026-07-22T09:41:12Z"}\n' +
  '\n'

const expected: ControllerEvent[] = [
  {
    application: 'edge',
    swarm: '',
    type: 'sync-started',
    revision: '9f3c1ab',
    at: '2026-07-22T09:41:10Z',
  },
  {
    application: 'edge',
    swarm: '',
    type: 'sync-failed',
    message: 'rendering échoué… 🚀',
    actor: 'alice',
    at: '2026-07-22T09:41:12Z',
  },
]

/** streamOf answers one connection whose body is exactly these chunks. */
function streamOf(chunks: Uint8Array[]): Response {
  let next = 0
  const body = {
    getReader: () => ({
      read: () =>
        Promise.resolve(
          next < chunks.length
            ? { done: false, value: chunks[next++] }
            : { done: true, value: undefined },
        ),
    }),
  }
  return { ok: true, status: 200, body } as unknown as Response
}

/**
 * collect runs the transport against one connection carrying these chunks and
 * returns when it would reconnect. Aborting from the sleep is what ends the run:
 * the transport has no other stopping condition, because a stream that closed is
 * a stream to reopen.
 */
async function collect(chunks: Uint8Array[]): Promise<ControllerEvent[]> {
  const controller = new AbortController()
  const events: ControllerEvent[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(streamOf(chunks))),
  )
  await runEventStream({
    signal: controller.signal,
    onEvent: (event) => events.push(event),
    onState: () => {},
    now: () => 0,
    sleep: () => {
      controller.abort()
      return Promise.resolve()
    },
  })
  return events
}

afterEach(() => {
  vi.unstubAllGlobals()
  sessionStorage.clear()
})

describe('the frame parser', () => {
  it('produces the same events however the bytes are split', async () => {
    const bytes = new TextEncoder().encode(wire)

    // Every index, including 0 and the length: a chunk boundary can fall
    // between any two bytes, and the interesting ones are inside a line, inside
    // a frame, and inside a character. Enumerating them is cheaper than
    // reasoning about which ones matter.
    for (let split = 0; split <= bytes.length; split++) {
      const got = await collect([bytes.slice(0, split), bytes.slice(split)])
      expect(got, `split at byte ${split}`).toEqual(expected)
    }
  })

  it('produces the same events one byte at a time', async () => {
    const bytes = new TextEncoder().encode(wire)
    const singles = Array.from(bytes, (byte) => new Uint8Array([byte]))

    expect(await collect(singles)).toEqual(expected)
  })

  it('drops a malformed frame rather than the connection', async () => {
    const chunks = [new TextEncoder().encode('data: not json\n\n' + wire)]

    // Killing the read here would drop a healthy connection and enter a backoff
    // over something no reconnect can fix.
    expect(await collect(chunks)).toEqual(expected)
  })
})

describe('reconnection', () => {
  it('backs off exponentially and caps', () => {
    expect([0, 1, 2, 3, 4, 5, 6].map(backoffFor)).toEqual([
      1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000,
    ])
  })

  it('forgives the backoff only after a connection that stayed open', async () => {
    // A server that accepts and immediately closes is the case this rule exists
    // for: resetting on "we connected" would make every attempt look like a
    // success and turn the backoff into a hot loop.
    const lifetimes = [0, 0, 0, 6_000, 0]
    let clock = 0
    let connection = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(() => {
        const lifetime = lifetimes[connection++] ?? 0
        const body = {
          getReader: () => ({
            read: () => {
              clock += lifetime
              return Promise.resolve({ done: true, value: undefined })
            },
          }),
        }
        return Promise.resolve({ ok: true, status: 200, body } as unknown as Response)
      }),
    )

    const controller = new AbortController()
    const waits: number[] = []
    await runEventStream({
      signal: controller.signal,
      onEvent: () => {},
      onState: () => {},
      now: () => clock,
      sleep: (ms) => {
        waits.push(ms)
        if (waits.length === lifetimes.length) controller.abort()
        return Promise.resolve()
      },
    })

    // Three instant closes double the wait; the fourth connection lasted six
    // seconds, so the fifth attempt starts from the floor again.
    expect(waits).toEqual([1_000, 2_000, 4_000, 1_000, 2_000])
  })

  it('stops for good on a 401, having cleared the credential', async () => {
    setToken('stale')
    const fetched = vi.fn(() =>
      Promise.resolve(new Response(JSON.stringify({ error: 'unauthorized' }), { status: 401 })),
    )
    vi.stubGlobal('fetch', fetched)

    const states: StreamState[] = []
    await runEventStream({
      signal: new AbortController().signal,
      onEvent: () => {},
      onState: (state) => states.push(state),
      sleep: () => Promise.reject(new Error('a 401 must not be retried')),
    })

    expect(getToken()).toBeNull()
    expect(fetched).toHaveBeenCalledTimes(1)
    expect(states.at(-1)).toBe('stopped')
  })

  it('reports connecting, live and reconnecting in that order', async () => {
    const controller = new AbortController()
    const states: StreamState[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(streamOf([new TextEncoder().encode(wire)]))),
    )

    await runEventStream({
      signal: controller.signal,
      onEvent: () => {},
      onState: (state) => states.push(state),
      now: () => 0,
      sleep: () => {
        controller.abort()
        return Promise.resolve()
      },
    })

    expect(states).toEqual(['connecting', 'live', 'reconnecting', 'stopped'])
  })

  it('opens nothing once its signal is aborted', async () => {
    const fetched = vi.fn()
    vi.stubGlobal('fetch', fetched)
    const controller = new AbortController()
    controller.abort()

    await runEventStream({
      signal: controller.signal,
      onEvent: () => {},
      onState: () => {},
    })

    // What unmounting the shell has to do: no request, and no timer left behind.
    expect(fetched).not.toHaveBeenCalled()
  })
})
