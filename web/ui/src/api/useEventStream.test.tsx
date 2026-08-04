// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { EventStreamOptions, StreamState } from './events'
import { useEventStream } from './useEventStream'

// The stream itself is tested in events.test.ts against real bytes. What is
// under test here is only what the hook does with the states it is handed, so
// the transport is replaced by something that hands them over on demand.
const states: ((s: StreamState) => void)[] = []
vi.mock('./events', async (original) => ({
  ...(await original<typeof import('./events')>()),
  runEventStream: (o: EventStreamOptions) => {
    states.push(o.onState)
    return new Promise<void>(() => {})
  },
}))

function Harness() {
  useEventStream()
  return null
}

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
