// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { createContext, useContext } from 'react'

import type { LiveStream } from './api/useEventStream'

/**
 * The tab's one event stream, shared down the tree.
 *
 * useEventStream opens exactly one connection and it is opened in the Shell —
 * a hook called per screen would open a connection per mounted screen against a
 * browser's six-per-origin limit, and reconnect on every navigation. The
 * Overview and Monitor terminals still need the frames, so the Shell publishes
 * what it already has here rather than every consumer opening its own.
 *
 * Null is the default only so that useLive can name the one misuse — a consumer
 * rendered outside the Shell — rather than handing back a stream that is quietly
 * always empty.
 */
const LiveContext = createContext<LiveStream | null>(null)

export const LiveProvider = LiveContext.Provider

/** useLive reads the shared event stream. It must be called under the Shell. */
export function useLive(): LiveStream {
  const live = useContext(LiveContext)
  if (live === null) {
    throw new Error('useLive was called outside the Shell that opens the stream')
  }
  return live
}
