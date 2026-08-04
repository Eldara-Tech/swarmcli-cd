// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The event stream, read with fetch rather than EventSource.
//
// EventSource supports only `withCredentials`; it cannot set a header. Giving
// the server a second credential type — a cookie, with everything a cookie
// brings with it — to satisfy one browser API is a much larger change than
// parsing the frames ourselves, and the server's wire format then does not move:
// a curl and the future TUI view keep reading exactly what they read today.
//
// No React in this file. It is driven by useEventStream, and everything below is
// testable by feeding it bytes.
//
// # What the wire does not have
//
// No `id:`, so there is no Last-Event-ID and a reconnect is a refetch rather
// than a resume; none is invented here, because the documents are authoritative
// and the stream is a hint (see useEventStream).
//
// No `retry:`, so the backoff below is entirely ours.
//
// No keepalive, so a healthy quiet controller is indistinguishable from a dead
// socket until a write fails — and there is deliberately *no* idle timer here. A
// quiet controller is the normal case, so a timer would churn the connection all
// day; the 30-second refetch floor on the queries is the compensator.

import { ApiError, authorized } from './client'

export const eventsPath = '/api/v1/events'

/**
 * The eight event types this build knows. There are no others today —
 * docs/api.md states the set — but a controller ahead of this build may add a
 * ninth, so a frame's `type` is typed as a string and this list is what a
 * consumer narrows with.
 */
export const eventTypes = [
  'sync-started',
  'sync-succeeded',
  'sync-failed',
  'drift-detected',
  'live-drift-detected',
  'drift-converged',
  'resources-pruned',
  'prune-failed',
] as const

export type ControllerEventType = (typeof eventTypes)[number]

/** isEventType narrows a frame's type to one this build has a rule for. */
export function isEventType(value: string): value is ControllerEventType {
  return (eventTypes as readonly string[]).includes(value)
}

/** One frame's payload, mirrored from api/stream.go's `wire`. */
export interface ControllerEvent {
  application: string
  /**
   * Deliberately not optional, where revision, message and actor are — and that
   * asymmetry on one struct is the point rather than something to tidy up. The
   * question is never "is the string empty", it is "does empty mean something".
   *
   * Every event has a destination, and empty *is* the answer: the swarm the
   * controller runs in. api/stream.go therefore sends the key even when it is
   * empty, so that a client written against a single-swarm build has seen the
   * field before it is ever pointed at one that fills it. The other three are
   * genuine absences — the controller acted on its own, nothing resolved a
   * commit — and their keys are dropped rather than sent empty, which is what
   * makes them optional here.
   */
  swarm: string
  /** The wire type verbatim; narrow with isEventType before switching on it. */
  type: string
  revision?: string
  message?: string
  /** Who asked for the work. Absent when the controller acted on its own tick. */
  actor?: string
  at: string
}

/** Whether the tab is currently receiving events. */
export type StreamState = 'connecting' | 'live' | 'reconnecting' | 'stopped'

export interface EventStreamOptions {
  signal: AbortSignal
  onEvent: (event: ControllerEvent) => void
  onState: (state: StreamState) => void
  /** Seams for the tests, which must not spend real seconds proving a backoff. */
  now?: () => number
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>
}

/**
 * A connection has to have stayed open this long before the backoff is
 * forgiven.
 *
 * Resetting on "we connected" instead would turn a server that accepts and
 * immediately closes — a proxy with no upstream, a controller mid-restart — into
 * a hot reconnect loop, because every attempt would look like a success.
 */
const stableConnectionMs = 5_000

const firstBackoffMs = 1_000
const maxBackoffMs = 30_000

/** backoffFor is the wait before retry number `attempt`, counting from zero. */
export function backoffFor(attempt: number): number {
  return Math.min(maxBackoffMs, firstBackoffMs * 2 ** attempt)
}

/**
 * runEventStream keeps one connection to the event stream open until its signal
 * is aborted, reconnecting with a capped exponential backoff.
 *
 * It resolves rather than rejects: a stream that has stopped is a state the
 * caller renders, not an exception for it to handle.
 */
export async function runEventStream(o: EventStreamOptions): Promise<void> {
  const now = o.now ?? Date.now
  const sleep = o.sleep ?? sleepUnlessAborted

  let attempt = 0
  while (!o.signal.aborted) {
    o.onState('connecting')
    // null rather than 0 for "never opened": a clock is allowed to read zero,
    // and a sentinel that a real value can equal is how the reset below silently
    // stops happening.
    const connection: { openedAt: number | null } = { openedAt: null }
    try {
      await readStream(o, connection, now)
    } catch (error) {
      if (o.signal.aborted) break
      if (error instanceof ApiError && error.status === 401) {
        // The token has already been cleared by the fetch wrapper and the tab is
        // on its way back to the login screen. Retrying would send a credential
        // that no longer exists, once per backoff, until the component unmounts.
        o.onState('stopped')
        return
      }
      // Everything else — a dropped connection, a 502 from a proxy, a
      // controller restarting — is what the backoff is for.
    }
    if (o.signal.aborted) break

    o.onState('reconnecting')
    if (connection.openedAt !== null && now() - connection.openedAt >= stableConnectionMs) {
      attempt = 0
    }
    await sleep(backoffFor(attempt), o.signal)
    attempt++
  }
  o.onState('stopped')
}

/**
 * readStream resolves when the server closes the stream and throws when the
 * connection fails.
 *
 * connection.openedAt is written when the response headers arrive rather than
 * when the attempt started, so a connect that hangs for a minute and then fails
 * does not count as a connection that stayed open.
 */
async function readStream(
  o: EventStreamOptions,
  connection: { openedAt: number | null },
  now: () => number,
): Promise<void> {
  const response = await authorized(eventsPath, {
    signal: o.signal,
    headers: { Accept: 'text/event-stream' },
  })
  if (!response.ok || response.body === null) {
    throw new ApiError(response.status, `the event stream answered ${response.status}`)
  }

  connection.openedAt = now()
  o.onState('live')

  const reader = response.body.getReader()
  // { stream: true } is the whole reason this is a decoder and not one call per
  // chunk: a chunk boundary can fall in the middle of a multi-byte character,
  // and the fields most likely to carry one are exactly the free-text ones — a
  // commit message, a chart name, an operator's display name in `actor`.
  // Without it each half decodes to U+FFFD and the JSON on that frame is
  // corrupt.
  const decoder = new TextDecoder()
  const consume = frameReader(o.onEvent)

  for (;;) {
    const { done, value } = await reader.read()
    if (done) return
    consume(decoder.decode(value, { stream: true }))
  }
}

/**
 * frameReader turns chunks of text into events.
 *
 * It holds two pieces of state across chunks, and both are the bug this parser
 * exists to not have: `tail` is the part of a line the previous chunk cut in
 * half, and `data` is the fields of a frame whose blank line has not arrived
 * yet. A parser that assumed a chunk was a whole number of frames would work
 * against every fast localhost controller and drop events against a slow link.
 */
function frameReader(onEvent: (event: ControllerEvent) => void): (chunk: string) => void {
  let tail = ''
  let data: string[] = []

  return (chunk: string) => {
    const lines = (tail + chunk).split('\n')
    // The last piece is either an incomplete line or the empty string after a
    // trailing separator; either way it is not ready to be read.
    tail = lines.pop() ?? ''

    for (const raw of lines) {
      // The controller writes bare \n. A proxy that rewrites line endings would
      // otherwise leave a \r on the end of every value, including inside JSON.
      const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw

      if (line === '') {
        if (data.length > 0) dispatch(data.join('\n'), onEvent)
        data = []
        continue
      }
      // A comment. The controller sends none; a proxy's keepalive is exactly
      // this, and treating it as a field would break the frame it landed in.
      if (line.startsWith(':')) continue

      const colon = line.indexOf(':')
      const field = colon === -1 ? line : line.slice(0, colon)
      let value = colon === -1 ? '' : line.slice(colon + 1)
      if (value.startsWith(' ')) value = value.slice(1)

      // `event:` is read and discarded on purpose: the payload repeats the type
      // in its own `type` field, and consuming the name as well would give a
      // consumer two sources for one fact that a proxy could make disagree.
      if (field === 'data') data.push(value)
    }
  }
}

function dispatch(payload: string, onEvent: (event: ControllerEvent) => void): void {
  let event: ControllerEvent
  try {
    event = JSON.parse(payload) as ControllerEvent
  } catch {
    // A frame this build cannot parse is one frame lost. Throwing here would
    // abort the read, drop the connection, and enter a backoff over something a
    // reconnect cannot fix.
    return
  }
  onEvent(event)
}

/** The default sleep: settles early when the caller aborts, rather than after. */
function sleepUnlessAborted(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve()
      return
    }
    const timer = setTimeout(done, ms)
    signal.addEventListener('abort', done, { once: true })
    function done(): void {
      clearTimeout(timer)
      signal.removeEventListener('abort', done)
      resolve()
    }
  })
}
