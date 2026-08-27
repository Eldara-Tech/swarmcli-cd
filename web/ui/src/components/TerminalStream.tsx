// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useEffect, useRef, useState } from 'react'

import type { ControllerEvent } from '../api/events'
import { Icon } from './Icon'

/**
 * The tone each event type is drawn in — the same three-way reading the chips
 * use. An outcome that landed is green, one that failed is red, a drift or a
 * start is amber or cyan. A type this build has never heard of falls to the
 * neutral info tone rather than vanishing.
 */
const toneFor: Record<string, string> = {
  'sync-succeeded': 'term-ok',
  'drift-converged': 'term-ok',
  'sync-failed': 'term-bad',
  'prune-failed': 'term-bad',
  'drift-detected': 'term-warn',
  'live-drift-detected': 'term-warn',
  'sync-started': 'term-info',
  'resources-pruned': 'term-info',
}

type LevelFilter = 'all' | 'ok' | 'warn' | 'bad' | 'info'

/** The wall clock an event arrived at, or a dash when its timestamp will not parse. */
function clockOf(at: string): string {
  const parsed = Date.parse(at)
  return Number.isNaN(parsed) ? '--:--:--' : new Date(parsed).toLocaleTimeString()
}

/**
 * A terminal that tails the controller's event stream.
 *
 * The frames come from the tab's one stream via useLive, not a connection of its
 * own — so this renders whatever the caller passes and holds no state but the
 * scroll position, which it pins to the bottom as new lines arrive.
 */
export function TerminalStream({
  title,
  events,
  className,
}: {
  title: string
  events: ControllerEvent[]
  className?: string
}) {
  const body = useRef<HTMLDivElement>(null)
  const [filter, setFilter] = useState<LevelFilter>('all')

  const filteredEvents =
    filter === 'all'
      ? events
      : events.filter((e) => {
          const tone = toneFor[e.type] ?? 'term-info'
          return tone === `term-${filter}`
        })

  // Follow the tail: a new frame scrolls the newest line into view. Keyed on the
  // count so it fires per frame and not on every render.
  useEffect(() => {
    const el = body.current
    if (el !== null) el.scrollTop = el.scrollHeight
  }, [filteredEvents.length])

  return (
    <div className={className === undefined ? 'terminal' : `terminal ${className}`}>
      <div className="terminal-head">
        <span className="label-caps">
          <Icon name="terminal" size={14} /> {title}
          <span className="term-count">{events.length} events</span>
        </span>
        <div className="terminal-controls">
          <button
            type="button"
            className={filter === 'all' ? 'term-filter-btn active' : 'term-filter-btn'}
            onClick={() => setFilter('all')}
          >
            all
          </button>
          <button
            type="button"
            className={filter === 'ok' ? 'term-filter-btn active' : 'term-filter-btn'}
            onClick={() => setFilter('ok')}
          >
            ok
          </button>
          <button
            type="button"
            className={filter === 'warn' ? 'term-filter-btn active' : 'term-filter-btn'}
            onClick={() => setFilter('warn')}
          >
            warn
          </button>
          <button
            type="button"
            className={filter === 'bad' ? 'term-filter-btn active' : 'term-filter-btn'}
            onClick={() => setFilter('bad')}
          >
            err
          </button>
        </div>
      </div>
      <div className="terminal-body" ref={body}>
        {filteredEvents.length === 0 && <p className="term-empty">Waiting for controller activity…</p>}
        {filteredEvents.map((event, index) => (
          <div
            className={index === filteredEvents.length - 1 ? 'term-line term-line-new' : 'term-line'}
            key={`${event.at}-${index}`}
          >
            <span className="term-time">{clockOf(event.at)}</span>
            <span className={`term-type ${toneFor[event.type] ?? 'term-info'}`}>{event.type}</span>
            <span className="term-app">{event.application}</span>
            {event.message !== undefined && event.message !== '' && (
              <span className="term-msg">{event.message}</span>
            )}
          </div>
        ))}
        {filteredEvents.length > 0 && (
          <span className="term-cursor" aria-hidden="true">
            ▋
          </span>
        )}
      </div>
    </div>
  )
}
