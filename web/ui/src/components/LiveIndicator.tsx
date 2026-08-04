// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import type { LiveStream } from '../api/useEventStream'

const labels = {
  connecting: 'connecting',
  live: 'live',
  reconnecting: 'reconnecting',
  stopped: 'not connected',
} as const

/**
 * LiveIndicator says whether this tab is receiving events, and what the last one
 * was.
 *
 * It exists because the stream has no keepalive: a controller with nothing to
 * report and a socket that died half an hour ago look identical on screen, and
 * an operator watching a sync needs to know which they are looking at before
 * they conclude that nothing is happening.
 *
 * role="status" rather than an aria-live region on the whole header: the state
 * changes on its own, without anybody having pressed anything, which is exactly
 * what a status role is for.
 */
export function LiveIndicator({ state, last }: LiveStream) {
  return (
    <p className={`live live-${state}`} role="status">
      <span className="live-dot" aria-hidden="true" />
      {/* The two test ids are for the browser smoke, which has no other way to
          tell a stream that connected from one that only rendered. */}
      <span className="live-label" data-testid="live-state">
        {labels[state]}
      </span>
      {last !== null && (
        <span className="live-last" data-testid="live-last">
          {last.type} · {last.application}
        </span>
      )}
    </p>
  )
}
