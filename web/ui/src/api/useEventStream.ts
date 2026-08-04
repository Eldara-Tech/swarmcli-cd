// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useEffect, useState } from 'react'

import { runEventStream, type ControllerEvent, type StreamState } from './events'

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
 * refetched; the document is authoritative. B4 (#174) adds that invalidation map
 * on top of this hook.
 */
export function useEventStream(): LiveStream {
  const [state, setState] = useState<StreamState>('connecting')
  const [last, setLast] = useState<ControllerEvent | null>(null)

  useEffect(() => {
    // Aborted on unmount, which is also what StrictMode's second mount in
    // development does to the first stream: one connection at a time either way.
    const controller = new AbortController()
    void runEventStream({
      signal: controller.signal,
      onEvent: (event) => {
        setLast(event)
      },
      onState: setState,
    })
    return () => {
      controller.abort()
    }
  }, [])

  return { state, last }
}
