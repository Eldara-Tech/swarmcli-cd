// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { runEventStream, type ControllerEvent, type StreamState } from './events'
import { invalidatedBy } from './queries'

export interface LiveStream {
  state: StreamState
  /** The most recent frame, or null before one has arrived. */
  last: ControllerEvent | null
}

/**
 * useEventStream opens the tab's event stream and reports what it is doing.
 *
 * Called once, from the shell, and never from a screen. A hook called per screen
 * would open one connection per mounted component against a browser's
 * six-per-origin limit, and would reconnect on every navigation.
 *
 * Nothing here writes into the query cache. The stream drops frames for slow
 * subscribers by design — api/stream.go says so and logs when it does — so state
 * accumulated from the sequence diverges silently, and diverges most precisely
 * when the controller is busiest. An event is a hint that a document should be
 * refetched; the document is authoritative. The map deciding which documents is
 * api/queries.ts, and it is the only thing this hook does with an event beyond
 * showing it.
 */
export function useEventStream(): LiveStream {
  const [state, setState] = useState<StreamState>('connecting')
  const [last, setLast] = useState<ControllerEvent | null>(null)
  const client = useQueryClient()
  // Whether this tab has ever had a live stream. A ref, not state: it must not
  // reset when the effect re-runs, and nothing renders from it.
  const opened = useRef(false)

  useEffect(() => {
    // Aborted on unmount, which is also what StrictMode's second mount in
    // development does to the first stream: one connection at a time either way.
    const controller = new AbortController()
    void runEventStream({
      signal: controller.signal,
      onEvent: (event) => {
        setLast(event)
        // invalidateQueries and never setQueryData: the frame is a hint that
        // something moved, not a copy of what it moved to. Its own fields are
        // not the document — an event carries a revision and a sentence, where
        // the screen needs releases, health and a plan — and a stream that drops
        // frames could not be reassembled into one anyway.
        //
        // The promise is dropped deliberately. It settles when the refetches it
        // triggered have finished, and nothing here waits for that: the screens
        // are already subscribed to the queries and re-render on their own, and
        // awaiting would serialise the next frame behind this one's requests.
        for (const invalidation of invalidatedBy(event.type, event.application)) {
          void client.invalidateQueries(invalidation)
        }
      },
      onState: (next) => {
        // Every open after the first is a reconnect, and a reconnect is a
        // refetch. There is no `id:` on the wire, so no Last-Event-ID and no
        // replay: whatever the controller raised while this tab was
        // disconnected was delivered to nobody and is not coming again. The
        // documents are the only way to find out what it was.
        //
        // Without this the screen is stale until the 30-second refetch floor
        // catches it — and the moment a stream drops is the moment a controller
        // is restarting or a proxy is cycling, which is exactly when something
        // changed. The floor is the backstop for a stream that died without
        // saying so; this is the case where it said so.
        if (next === 'live' && opened.current) {
          void client.invalidateQueries()
        }
        if (next === 'live') {
          opened.current = true
        }
        setState(next)
      },
    })
    return () => {
      controller.abort()
    }
  }, [client])

  return { state, last }
}
