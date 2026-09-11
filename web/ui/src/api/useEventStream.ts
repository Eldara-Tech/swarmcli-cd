// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { fetchRecentEvents, runEventStream, type ControllerEvent, type StreamState } from './events'
import { invalidatedBy } from './queries'

export interface LiveStream {
  state: StreamState
  /** The most recent frame, or null before one has arrived. */
  last: ControllerEvent | null
  /**
   * The controller's recent events, oldest first, capped at logLimit. It is a
   * scrollback for the Monitor and Overview terminals, not a source of truth:
   * the documents remain authoritative, and this is a convenience for reading
   * what happened. Bounded because a tab left open for a week must not grow
   * without limit.
   *
   * Seeded from /api/v1/events/recent before every connect and appended to from
   * the stream, so it is not only what this tab was delivered. The stream itself
   * still replays nothing — the document is what closes that gap, and it closes
   * it as far back as the controller's ring reaches and no further: a controller
   * that restarted has forgotten, which is why the seed is guarded rather than
   * trusted to overwrite.
   */
  log: ControllerEvent[]
}

/** How many past frames the scrollback keeps; older ones fall off the top. */
export const logLimit = 250

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
  const [log, setLog] = useState<ControllerEvent[]>([])
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
      // Before each connect, including the first. The stream itself still
      // replays nothing — this reads the document that does, so a terminal
      // opens with the controller's recent history instead of the empty box a
      // converged fleet would leave it as for ever (#292).
      //
      // It replaces the scrollback rather than merging into it, which is what
      // keeps a reconnect from drawing every line twice. The ring is deeper
      // than this cap and is appended before the fan-out, so what comes back is
      // a superset of what this tab was delivered — except in the one case
      // guarded below.
      beforeConnect: async () => {
        let seed: ControllerEvent[]
        try {
          seed = await fetchRecentEvents(logLimit)
        } catch {
          // A controller older than this bundle answers 404, and a read can
          // fail for every ordinary reason. Either way the seed is a
          // convenience: keep whatever the tab has and open the stream.
          return
        }
        // An empty document is the one case where the ring is not a superset: a
        // controller that has just restarted has forgotten what this tab is
        // still showing, and replacing would erase the operator's scrollback at
        // the exact moment something happened.
        if (seed.length === 0) return
        // The client's cap is the client's. The controller clamps to what was
        // asked for, but a companion or a future build need not.
        setLog(seed.slice(-logLimit))
      },
      onEvent: (event) => {
        setLast(event)
        // Appended for the terminals, capped so a long-lived tab does not grow
        // without bound. slice keeps the newest logLimit frames.
        setLog((prev) => {
          const next = prev.length >= logLimit ? prev.slice(prev.length - logLimit + 1) : prev
          return [...next, event]
        })
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
        // disconnected was delivered to nobody, and only a document can say what
        // it was. beforeConnect above has just re-read the one that answers that
        // for the terminal; this is the same move for every other screen.
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

  return { state, last, log }
}
