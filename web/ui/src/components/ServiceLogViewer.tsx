// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'

import { authorized } from '../api/client'
import type { ServiceLogEvent } from '../api/types'
import { Icon } from './Icon'

/** What the stream is doing, which the footer reports rather than assuming. */
type StreamStatus = 'connecting' | 'live' | 'ended' | 'failed' | 'unsupported'

/**
 * How many lines the buffer keeps.
 *
 * At least the widest scrollback preset's tail, or a window the operator asked
 * for would be discarded from the top the moment it arrived — which is the same
 * outcome as never having asked. It used to be 1000, when 100 was the only
 * thing that could be asked for.
 */
const logBufferLimit = 50_000

/**
 * How many of the matching lines the console starts with, and how many more it
 * adds each time the operator reaches the top of them.
 *
 * The newest of them first, because a console is read from the bottom. Fifty
 * thousand rows of five spans each put in the DOM at once is a browser that
 * stops responding, so the buffer and the render are deliberately different
 * sizes — but the render is a starting size rather than a ceiling. Scrolling to
 * the top of the drawn rows extends the window backwards by this many more,
 * until the whole buffer is reachable.
 *
 * The growth is bounded by the operator's attention rather than by a number:
 * it happens only while they have left the bottom, and the window collapses
 * back to this size the moment they return to it. So the live stream — the path
 * that repaints on every arriving chunk — never draws more than this, however
 * far back somebody read a minute ago.
 *
 * The footer says how many are held and how many are shown, because a console
 * silently drawing a fraction of what it has is a console that lies.
 */
const logRenderLimit = 2_000

/**
 * How close to the top counts as reaching it.
 *
 * Pixels rather than rows: the row is not a fixed height, so there is no row
 * count that means the same thing on a console of wrapped lines as on one of
 * short ones. Wide enough that a fast scroll does not overshoot the trigger and
 * stop dead at the top with more to read.
 */
const growAtTop = 400

/**
 * How far back the console can read, and what it costs.
 *
 * `tail` is not a detail of `since` — it is the other half of it. The daemon
 * selects the last `tail` lines *first* and only then drops the ones older than
 * `since`, so a window asking for six hours with a hundred-line tail is
 * answered with a hundred lines. The pair is what makes the preset mean what it
 * says, and the API refuses one without the other rather than serving the
 * disappointment.
 *
 * So `since` is the intent and `tail` is the safety cap. A service quiet enough
 * that an hour fits in 20,000 lines gets the whole hour; a chatty one gets its
 * most recent 20,000 lines within that hour, which is the honest truncation and
 * what the daemon would have done anyway.
 */
interface Scrollback {
  id: string
  label: string
  title: string
  tail: number
  since: string
}

const scrollbacks: Scrollback[] = [
  { id: 'tail', label: 'Tail', title: 'The last 100 lines, then follow', tail: 100, since: '' },
  { id: '15m', label: '15m', title: 'The last 15 minutes, up to 5,000 lines', tail: 5_000, since: '15m' },
  { id: '1h', label: '1h', title: 'The last hour, up to 20,000 lines', tail: 20_000, since: '1h' },
  { id: '6h', label: '6h', title: 'The last 6 hours, up to 50,000 lines', tail: 50_000, since: '6h' },
]

/** How many task colours there are before they repeat. */
const taskColours = 8

/** One buffered line, with the sequence number the console numbers rows by. */
interface LogLine extends ServiceLogEvent {
  seq: number
}

/**
 * What a line is labelled with.
 *
 * A notice is the controller talking — output was dropped, a line was truncated
 * — and it carries `stream: 'stderr'` so the stream filter does not hide it
 * from the operator looking for trouble. That makes it indistinguishable from a
 * line the container wrote unless it is labelled differently, which is what
 * this is for, and it is also what Copy and Export write.
 */
function tagOf(log: ServiceLogEvent): string {
  return log.notice ? 'NOTICE' : log.stream.toUpperCase()
}

/**
 * Which replica wrote this line, as short as it can honestly be said.
 *
 * Three steps, and each one is a different thing being unknown. A slot is what
 * `docker service ps` shows and what an operator says out loud. Without one the
 * hostname is the answer: that covers a global-mode service, whose tasks have
 * no slot at all, and a task the controller could not name because its listing
 * failed. Without either there is only the task id, truncated the way the
 * daemon's own tooling truncates it.
 *
 * Empty for a notice, which no task wrote.
 */
function replicaOf(log: ServiceLogEvent): string {
  if (log.notice) return ''
  if (log.slot) return `.${log.slot}`
  if (log.nodeHostname) return log.nodeHostname
  return log.taskID ? log.taskID.slice(0, 12) : ''
}

/** The long form, for the hover that the column has no width for. */
function replicaTitle(log: ServiceLogEvent): string {
  const parts = []
  if (log.taskID) parts.push(`task ${log.taskID}`)
  if (log.nodeHostname) parts.push(`on ${log.nodeHostname}`)
  else if (log.nodeID) parts.push(`on node ${log.nodeID}`)
  return parts.join(' ')
}

/** One line as Copy and Export write it. */
function asText(log: LogLine): string {
  const replica = replicaOf(log)
  const label = replica ? `[${replica}] ` : ''
  return `[${log.timestamp.slice(11, 19)}] ${label}[${tagOf(log)}] ${log.message}`
}

/** What the stream badge and the footer say, per state. */
const badge: Record<StreamStatus | 'paused', { dot: string; text: string }> = {
  connecting: { dot: 'pulse-warn', text: 'CONNECTING' },
  live: { dot: 'pulse-ok', text: 'LIVE' },
  paused: { dot: 'pulse-warn', text: 'PAUSED' },
  ended: { dot: 'pulse-warn', text: 'ENDED' },
  failed: { dot: 'pulse-bad', text: 'FAILED' },
  unsupported: { dot: 'pulse-warn', text: 'UNSUPPORTED' },
}

interface ServiceLogViewerProps {
  app: string
  service: string
  apps: { name: string; services: string[] }[]
  onSelectApp: (app: string) => void
  onSelectService: (svc: string) => void
}

export function ServiceLogViewer({
  app,
  service,
  apps,
  onSelectApp,
  onSelectService,
}: ServiceLogViewerProps) {
  const [logs, setLogs] = useState<LogLine[]>([])
  const [filter, setFilter] = useState('')
  const [streamFilter, setStreamFilter] = useState<'all' | 'stdout' | 'stderr'>('all')
  const [taskFilter, setTaskFilter] = useState('')
  const [scrollback, setScrollback] = useState('tail')
  const [isPaused, setIsPaused] = useState(false)
  const [autoScroll, setAutoScroll] = useState(true)
  /**
   * The oldest line the console is currently drawing, by sequence number, or
   * null while it is pinned to the newest `logRenderLimit`.
   *
   * A sequence number and not an index, because both things it has to survive
   * move indices under it: the buffer drops its oldest line at 50,000, and a
   * search narrows what is being indexed into. A number that means "the line
   * the operator is looking at" keeps meaning that through both.
   *
   * Setting it is also what stops the drawn rows sliding. Pinned to the newest
   * N, every arriving line drops the oldest drawn one — invisible at the bottom
   * of the console, where the pin holds the view, and a drift upward of
   * everything under the eye of somebody who has scrolled away from it. The pin
   * is released the moment they do.
   */
  const [windowStart, setWindowStart] = useState<number | null>(null)
  /**
   * The scroll height from just before the window last grew, or null.
   *
   * Growing puts rows *above* the ones being read, so the browser's scrollTop —
   * a distance from the top — now names different content. The layout effect
   * adds back exactly what was inserted, before the paint, so the rows do not
   * move. Doubles as the guard against a second growth being started while the
   * first has not been paid for yet.
   */
  const grownFrom = useRef<number | null>(null)
  const [copied, setCopied] = useState(false)
  const [status, setStatus] = useState<StreamStatus>('connecting')
  const [error, setError] = useState('')
  const terminalBodyRef = useRef<HTMLDivElement>(null)
  // Read by the stream reader, which must not re-subscribe when it changes.
  const pausedRef = useRef(false)
  useEffect(() => {
    pausedRef.current = isPaused
  }, [isPaused])

  /**
   * Which colour each task was given, in order of first appearance.
   *
   * A ref and not derived from `logs`, because it has to survive the buffer
   * scrolling: a colour recomputed from what is currently held would change
   * under the operator the moment the first line of a task fell off the top,
   * and the whole point of the colour is that one task looks like itself for as
   * long as the console is open.
   */
  const taskColour = useRef(new Map<string, number>())

  /**
   * The stream, opened once per app, service and scrollback window.
   *
   * `isPaused` is deliberately NOT a dependency. It was, and the effect body
   * begins by emptying the buffer — so pressing Pause dropped the connection
   * and wiped the scrollback, and pressing Resume wiped it again. Pause holds
   * the view; it does not restart the stream. The ref is what lets the reader
   * see the current value without the effect re-running.
   *
   * The scrollback window IS a dependency, and emptying the buffer is the right
   * behaviour there: a different window is a different read, not more of the
   * current one, and the daemon has no way to extend a stream backwards.
   */
  useEffect(() => {
    setLogs([])
    setWindowStart(null)
    setStatus('connecting')
    taskColour.current = new Map()
    if (!app || !service) return

    const controller = new AbortController()
    const window = scrollbacks.find((s) => s.id === scrollback) ?? scrollbacks[0]
    const params = new URLSearchParams()
    if (window.since) {
      // Both, always. One without the other is a window bounded by the wrong
      // thing, and the controller refuses it rather than serving it.
      params.set('tail', String(window.tail))
      params.set('since', window.since)
    }
    const query = params.size > 0 ? `?${params}` : ''
    const url = `/api/v1/applications/${encodeURIComponent(app)}/services/${encodeURIComponent(service)}/logs${query}`

    // Monotonic for the life of this stream, so a row's number and its React
    // key stay the same when the buffer scrolls. This used to be
    // `logs.indexOf(log)` inside the render map — a linear scan per row per
    // paint, which at this buffer size is not a cost anybody would choose, and
    // a key that changed for every row whenever one fell off the top.
    let seq = 0

    async function startStream() {
      try {
        const response = await authorized(url, {
          signal: controller.signal,
          headers: { Accept: 'text/event-stream' },
        })
        if (!response.ok || !response.body) {
          // 501 is the ordinary state of an open-source build: the controller
          // has no log streamer wired. It is not an error to report in red, and
          // it is certainly not something to fill with invented lines.
          setStatus(response.status === 501 ? 'unsupported' : 'failed')
          setError(
            response.status === 501
              ? 'This controller does not stream service logs.'
              : response.status === 404
                ? 'This application does not report a service by that name.'
                : response.status === 400
                  ? 'The controller could not read this scrollback window.'
                  : `The controller answered ${response.status}.`,
          )
          return
        }

        setStatus('live')
        const reader = response.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''

        for (;;) {
          const { done, value } = await reader.read()
          if (done) break

          buffer += decoder.decode(value, { stream: true })
          const blocks = buffer.split('\n\n')
          buffer = blocks.pop() ?? ''

          // Everything this read brought, appended in one state update.
          //
          // Not one per line, which is what it used to be. Each append copies
          // the whole buffer, so a per-line update over a 50,000-line history
          // read is fifty thousand copies of an array growing to fifty
          // thousand — quadratic, and a browser that stops responding on the
          // one request this console was widened for. It was survivable at a
          // thousand lines and is not at fifty.
          const batch: LogLine[] = []
          for (const block of blocks) {
            for (const raw of block.split('\n')) {
              // A proxy that rewrites line endings leaves a \r on every value,
              // including inside the JSON. api/events.ts strips it for the same
              // reason and this parser has the same problem.
              const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw
              if (!line.startsWith('data: ')) continue
              try {
                const event = JSON.parse(line.slice(6)) as ServiceLogEvent
                if (pausedRef.current) continue
                if (event.taskID && !taskColour.current.has(event.taskID)) {
                  taskColour.current.set(event.taskID, taskColour.current.size % taskColours)
                }
                batch.push({ ...event, seq: seq++ })
              } catch {
                // One frame this build cannot parse is one line lost.
              }
            }
          }
          if (batch.length > 0) {
            setLogs((prev) => {
              const grown = prev.length === 0 ? batch : [...prev, ...batch]
              return grown.length > logBufferLimit
                ? grown.slice(grown.length - logBufferLimit)
                : grown
            })
          }
        }
        // The server closed. Said rather than swallowed: the loop used to exit
        // here and leave a console that looked live and received nothing.
        if (!controller.signal.aborted) {
          setStatus('ended')
          setError('The controller closed the log stream.')
        }
      } catch (err) {
        if (!controller.signal.aborted) {
          setStatus('failed')
          setError(err instanceof Error ? err.message : String(err))
        }
      }
    }

    void startStream()
    return () => {
      controller.abort()
    }
  }, [app, service, scrollback])

  // Autoscroll to bottom
  useEffect(() => {
    if (autoScroll && terminalBodyRef.current) {
      terminalBodyRef.current.scrollTop = terminalBodyRef.current.scrollHeight
    }
  }, [logs, autoScroll])

  /**
   * The three things a scroll can mean here.
   *
   * Back at the bottom: the console is following again and the window collapses
   * to the newest `logRenderLimit`, which is what keeps the live path cheap
   * however far back this operator just read.
   *
   * Away from the bottom, for the first time: pin the window where it is, so
   * that arriving lines are added below rather than trading the oldest drawn
   * row for a new one under a reader who is not looking at the bottom.
   *
   * At the top of the drawn rows, with older ones held: draw more of them. This
   * is the whole of #270 — the buffer was always reachable by search and by
   * Export, and now it is reachable by the thing an operator reaches for first.
   */
  const handleScroll = () => {
    const body = terminalBodyRef.current
    if (!body) return
    const { scrollTop, scrollHeight, clientHeight } = body
    const atBottom = scrollHeight - (scrollTop + clientHeight) < 30
    setAutoScroll(atBottom)

    if (atBottom) {
      setWindowStart(null)
      return
    }
    if (windowStart === null) {
      setWindowStart(visible[0]?.seq ?? null)
      return
    }
    // One growth at a time: until the layout effect has corrected scrollTop it
    // is still reporting the top, and every scroll event would start another.
    if (scrollTop > growAtTop || grownFrom.current !== null) return
    const at = filteredLogs.findIndex((log) => log.seq === visible[0]?.seq)
    if (at <= 0) return
    grownFrom.current = scrollHeight
    setWindowStart(filteredLogs[Math.max(0, at - logRenderLimit)].seq)
  }

  // Before the paint, never after: an effect that ran afterwards would let the
  // browser show one frame of the rows in their new places, which is the jump
  // this exists to prevent.
  useLayoutEffect(() => {
    const body = terminalBodyRef.current
    const before = grownFrom.current
    grownFrom.current = null
    if (!body || before === null) return
    body.scrollTop += body.scrollHeight - before
  }, [windowStart])

  /**
   * The tasks seen so far, in the order they first wrote something.
   *
   * A select and not a button segment, because the set is not fixed: a rollout
   * adds members while the operator is reading, and a segment that reflows as
   * tasks come and go is a control that moves under the cursor.
   */
  const tasks = useMemo(() => {
    const seen = new Map<string, string>()
    for (const log of logs) {
      if (!log.taskID || log.notice || seen.has(log.taskID)) continue
      seen.set(log.taskID, replicaOf(log))
    }
    return [...seen].map(([id, label]) => ({ id, label }))
  }, [logs])

  const filteredLogs = useMemo(() => {
    const needle = filter.toLowerCase()
    return logs.filter((log) => {
      if (streamFilter !== 'all' && log.stream !== streamFilter) return false
      // A notice is the controller reporting on the stream itself — dropped
      // output, a truncated line — so it is about every task and is never
      // narrowed away by picking one.
      if (taskFilter && !log.notice && log.taskID !== taskFilter) return false
      if (needle && !log.message.toLowerCase().includes(needle)) return false
      return true
    })
  }, [logs, streamFilter, taskFilter, filter])

  /**
   * The matching lines that are actually drawn.
   *
   * The newest of them, which is what a console shows, until the operator
   * scrolls back — then everything from where they have reached to the end,
   * which grows as they keep going. Copy, Export and both filters above
   * deliberately work on filteredLogs and not on this.
   *
   * `>=` rather than `===` because the anchor line need not still match: a
   * search typed while scrolled back narrows what is here, and the honest
   * answer is the first surviving line at or after where the operator was, not
   * a jump to the bottom. When nothing survives at or after it — every match is
   * older than where they stood — there is no position left to keep and the
   * newest matches are the answer.
   */
  const visible = useMemo(() => {
    if (windowStart === null) {
      return filteredLogs.length > logRenderLimit
        ? filteredLogs.slice(filteredLogs.length - logRenderLimit)
        : filteredLogs
    }
    const at = filteredLogs.findIndex((log) => log.seq >= windowStart)
    if (at < 0) {
      return filteredLogs.length > logRenderLimit
        ? filteredLogs.slice(filteredLogs.length - logRenderLimit)
        : filteredLogs
    }
    return filteredLogs.slice(at)
  }, [filteredLogs, windowStart])

  const copyLogs = () => {
    void navigator.clipboard.writeText(filteredLogs.map(asText).join('\n'))
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const exportLogs = () => {
    const blob = new Blob([filteredLogs.map(asText).join('\n')], {
      type: 'text/plain;charset=utf-8',
    })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${app}_${service}_logs.txt`
    a.click()
    // Not in this tick: some browsers have not started the download yet, and
    // revoking underneath it cancels the file.
    setTimeout(() => URL.revokeObjectURL(url), 0)
  }

  const availableServices = apps.find((a) => a.name === app)?.services ?? []

  return (
    <div className="terminal-console">
      {/* Sleek Integrated Toolbar */}
      <div className="terminal-console-toolbar">
        <div className="toolbar-group">
          <div className="toolbar-select-wrap">
            <span className="toolbar-label">APP</span>
            <select
              className="toolbar-select"
              value={app}
              onChange={(e) => onSelectApp(e.target.value)}
            >
              {apps.map((a) => (
                <option key={a.name} value={a.name}>
                  {a.name}
                </option>
              ))}
            </select>
          </div>

          <div className="toolbar-select-wrap">
            <span className="toolbar-label">SVC</span>
            <select
              className="toolbar-select"
              value={service}
              onChange={(e) => onSelectService(e.target.value)}
            >
              {availableServices.map((svc) => (
                <option key={svc} value={svc}>
                  {svc}
                </option>
              ))}
            </select>
          </div>

          <div className="toolbar-select-wrap">
            <span className="toolbar-label">TASK</span>
            <select
              className="toolbar-select"
              value={taskFilter}
              aria-label="Filter by task"
              onChange={(e) => setTaskFilter(e.target.value)}
            >
              <option value="">All tasks</option>
              {tasks.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.label}
                </option>
              ))}
            </select>
          </div>

          <div className="stream-badge-wrap">
            <span className={`pulse-dot ${badge[isPaused ? 'paused' : status].dot}`} />
            <span className="stream-badge-text">{badge[isPaused ? 'paused' : status].text}</span>
          </div>
        </div>

        <div className="toolbar-group toolbar-controls">
          <div className="search-wrap">
            <Icon name="search" size={13} />
            <input
              type="text"
              className="terminal-search-input"
              placeholder="Grep logs (text / regex)..."
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
            {filter && (
              <button type="button" className="clear-search-btn" onClick={() => setFilter('')}>
                ×
              </button>
            )}
          </div>

          <div className="btn-segment" role="group" aria-label="Scrollback window">
            {scrollbacks.map((s) => (
              <button
                key={s.id}
                type="button"
                className={`btn-seg ${scrollback === s.id ? 'active' : ''}`}
                title={`${s.title}. Changing this is a new read, so the current buffer is replaced.`}
                onClick={() => setScrollback(s.id)}
              >
                {s.label}
              </button>
            ))}
          </div>

          <div className="btn-segment">
            <button
              type="button"
              className={`btn-seg ${streamFilter === 'all' ? 'active' : ''}`}
              onClick={() => setStreamFilter('all')}
            >
              All
            </button>
            <button
              type="button"
              className={`btn-seg ${streamFilter === 'stdout' ? 'active' : ''}`}
              onClick={() => setStreamFilter('stdout')}
            >
              Stdout
            </button>
            <button
              type="button"
              className={`btn-seg ${streamFilter === 'stderr' ? 'active' : ''}`}
              onClick={() => setStreamFilter('stderr')}
            >
              Stderr
            </button>
          </div>

          <button
            type="button"
            className="toolbar-btn"
            onClick={() => setIsPaused((p) => !p)}
            title={isPaused ? 'Resume live streaming' : 'Pause incoming stream'}
          >
            {isPaused ? 'Resume' : 'Pause'}
          </button>

          <button
            type="button"
            className="toolbar-btn"
            onClick={() => {
              setLogs([])
              setWindowStart(null)
            }}
            title="Clear current log buffer"
          >
            Clear
          </button>

          <button
            type="button"
            className="toolbar-btn"
            onClick={copyLogs}
            title="Copy logs to clipboard"
          >
            {copied ? 'Copied!' : 'Copy'}
          </button>

          <button
            type="button"
            className="toolbar-btn"
            onClick={exportLogs}
            title="Download log output as file"
          >
            Export
          </button>
        </div>
      </div>

      {/* Terminal Viewport */}
      <div className="terminal-console-viewport" ref={terminalBodyRef} onScroll={handleScroll}>
        {filteredLogs.length === 0 ? (
          <div className="terminal-empty-state">
            <Icon name="activity" size={24} />
            <p>
              {logs.length > 0
                ? 'No lines match active filter.'
                : status === 'connecting'
                  ? `Connecting to container stream for ${app}/${service}…`
                  : status === 'live'
                    ? `Attached to ${app}/${service}. No output yet.`
                    : error}
            </p>
          </div>
        ) : (
          <>
            {visible.length < filteredLogs.length && (
              <div className="console-truncated">
                {(filteredLogs.length - visible.length).toLocaleString()} older matching lines are
                held above these. Keep scrolling to draw them; the search, the task filter, Copy
                and Export already work on all {filteredLogs.length.toLocaleString()}.
              </div>
            )}
            {visible.map((log) => {
              const replica = replicaOf(log)
              return (
                <div
                  key={log.seq}
                  className={`console-line ${log.notice ? 'line-notice' : log.stream === 'stderr' ? 'line-stderr' : 'line-stdout'}`}
                >
                  <span className="line-num">{String(log.seq + 1).padStart(3, '0')}</span>
                  <span className="line-time">{log.timestamp.slice(11, 19)}</span>
                  <span
                    className={`line-replica${log.taskID ? ` line-replica-${taskColour.current.get(log.taskID) ?? 0}` : ''}`}
                    title={replicaTitle(log)}
                  >
                    {replica}
                  </span>
                  <span className={`line-tag line-tag-${log.notice ? 'notice' : log.stream}`}>
                    {tagOf(log)}
                  </span>
                  <span className="line-content">{log.message}</span>
                </div>
              )
            })}
          </>
        )}
      </div>

      {/* Floating Jump Button */}
      {!autoScroll && (
        <button
          type="button"
          className="jump-bottom-btn"
          onClick={() => {
            setAutoScroll(true)
            setWindowStart(null)
            if (terminalBodyRef.current) {
              terminalBodyRef.current.scrollTop = terminalBodyRef.current.scrollHeight
            }
          }}
        >
          ↓ Auto-scroll Paused (Jump to Bottom)
        </button>
      )}

      {/* Footer Telemetry Bar */}
      <div className="terminal-console-footer">
        <div className="footer-meta">
          <span>
            TARGET: <strong className="font-mono">{app}/{service}</strong>
          </span>
          <span className="footer-sep">|</span>
          <span>
            LINES: <strong>{visible.length.toLocaleString()}</strong> shown /{' '}
            {filteredLogs.length.toLocaleString()} matched / {logs.length.toLocaleString()} held
          </span>
          <span className="footer-sep">|</span>
          <span>
            AUTOSCROLL: <strong>{autoScroll ? 'ENABLED' : 'LOCKED'}</strong>
          </span>
        </div>
        <div className="footer-feed">
          <span className="live-status-text">{badge[isPaused ? 'paused' : status].text}</span>
        </div>
      </div>
    </div>
  )
}
