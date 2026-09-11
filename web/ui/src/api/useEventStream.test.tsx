// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { controller, json } from '../test/fakeApi'
import type { ControllerEvent, EventStreamOptions, StreamState } from './events'
import { logLimit, useEventStream } from './useEventStream'

// The stream itself is tested in events.test.ts against real bytes. What is
// under test here is only what the hook does with the states it is handed, so
// the transport is replaced by something that hands them over on demand.
const states: ((s: StreamState) => void)[] = []
// The seed hook, captured rather than run: the fake transport never connects,
// so a test calls this itself and says exactly when the connect it precedes
// would have happened.
const seeds: (() => Promise<void>)[] = []
vi.mock('./events', async (original) => ({
  ...(await original<typeof import('./events')>()),
  runEventStream: (o: EventStreamOptions) => {
    states.push(o.onState)
    if (o.beforeConnect !== undefined) seeds.push(o.beforeConnect)
    return new Promise<void>(() => {})
  },
}))

// fetchRecentEvents is deliberately NOT mocked above — the spread keeps the real
// one — so these tests run the bearer header, the 401 rule and the document's
// own decoding rather than a stand-in for them, exactly as fakeApi argues.

function Harness() {
  useEventStream()
  return null
}

/** The log the hook is currently handing out, captured as it renders. */
const seen: ControllerEvent[][] = []

function LogHarness() {
  seen.push(useEventStream().log)
  return null
}

function event(revision: string): ControllerEvent {
  return {
    application: 'edge',
    swarm: '',
    type: 'sync-succeeded',
    revision,
    at: '2026-09-11T09:02:11Z',
  }
}

/** Renders the hook and returns the seed it registered. */
async function mounted(): Promise<() => Promise<void>> {
  states.length = 0
  seeds.length = 0
  seen.length = 0
  render(
    <QueryClientProvider client={new QueryClient()}>
      <LogHarness />
    </QueryClientProvider>,
  )
  await waitFor(() => expect(seeds.length).toBeGreaterThan(0))
  return seeds[seeds.length - 1]
}

/** The newest log the hook has rendered. */
function log(): ControllerEvent[] {
  return seen[seen.length - 1]
}

describe('the scrollback seed', () => {
  // The whole of #292: a converged controller raises nothing, so a terminal
  // that only ever showed what arrived after the tab loaded showed nothing at
  // all, for ever, on the healthiest fleet there is.
  it('opens the terminal with what the controller already published', async () => {
    controller({
      '/api/v1/events/recent': () => json(200, { events: [event('r1'), event('r2')] }),
    })
    const seed = await mounted()

    expect(log()).toHaveLength(0)

    await act(async () => {
      await seed()
    })

    expect(log().map((e) => e.revision)).toEqual(['r1', 'r2'])
  })

  // A controller older than this bundle has no such endpoint, and a read fails
  // for every ordinary reason besides. The seed is a convenience; a stream that
  // would not open without it would be a worse terminal than an empty one.
  it('opens anyway when the controller cannot serve the document', async () => {
    controller({
      '/api/v1/events/recent': () => json(404, { error: 'no such endpoint' }),
    })
    const seed = await mounted()

    await act(async () => {
      await expect(seed()).resolves.toBeUndefined()
    })

    expect(log()).toHaveLength(0)
  })

  // The one case where the ring is not a superset of what this tab holds. A
  // controller that restarted has forgotten what the operator is still reading,
  // and replacing would erase it at the moment something happened.
  it('keeps the scrollback when the document comes back empty', async () => {
    let events = [event('r1')]
    controller({
      '/api/v1/events/recent': () => json(200, { events }),
    })
    const seed = await mounted()

    await act(async () => {
      await seed()
    })
    expect(log()).toHaveLength(1)

    events = []
    await act(async () => {
      await seed()
    })

    expect(log().map((e) => e.revision)).toEqual(['r1'])
  })

  // Replaced, not merged. The seed runs before every connect, so a reconnect
  // that appended would draw every line the tab already held a second time.
  it('replaces the scrollback on a reconnect rather than appending to it', async () => {
    let events = [event('r1'), event('r2')]
    controller({
      '/api/v1/events/recent': () => json(200, { events }),
    })
    const seed = await mounted()

    await act(async () => {
      await seed()
    })

    events = [event('r2'), event('r3')]
    await act(async () => {
      await seed()
    })

    expect(log().map((e) => e.revision)).toEqual(['r2', 'r3'])
  })

  // The controller clamps to what was asked for; a companion or a future build
  // need not, and the cap is this client's to keep.
  it('keeps only the newest logLimit of an oversized document', async () => {
    const many = Array.from({ length: logLimit + 10 }, (_, i) => event(`r${i}`))
    controller({
      '/api/v1/events/recent': () => json(200, { events: many }),
    })
    const seed = await mounted()

    await act(async () => {
      await seed()
    })

    expect(log()).toHaveLength(logLimit)
    expect(log()[0].revision).toBe('r10')
    expect(log()[logLimit - 1].revision).toBe(`r${logLimit + 9}`)
  })
})

describe('a reconnect', () => {
  it('refetches everything, and the first connection does not', async () => {
    states.length = 0
    const client = new QueryClient()
    const invalidate = vi.spyOn(client, 'invalidateQueries')

    render(
      <QueryClientProvider client={client}>
        <Harness />
      </QueryClientProvider>,
    )
    await waitFor(() => expect(states.length).toBeGreaterThan(0))
    const onState = states[states.length - 1]

    // The first open is not a reconnect: nothing was missed, and every screen
    // is about to fetch on mount anyway. Invalidating here would double every
    // request the page makes on load.
    onState('live')
    expect(invalidate).not.toHaveBeenCalled()

    // Dropping and coming back is. Whatever the controller raised in between
    // was delivered to nobody, and there is no Last-Event-ID to replay it.
    onState('reconnecting')
    onState('live')
    await waitFor(() => expect(invalidate).toHaveBeenCalledTimes(1))
    // Everything, with no key: which documents moved is exactly what a
    // disconnected tab cannot know.
    expect(invalidate).toHaveBeenCalledWith()
  })
})
