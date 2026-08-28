// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useEffect, useMemo, useRef, useState } from 'react'

import { authorized } from '../api/client'
import type { ServiceLogEvent } from '../api/types'
import { Icon } from './Icon'

/** What the stream is doing, which the footer reports rather than assuming. */
type StreamStatus = 'connecting' | 'live' | 'ended' | 'failed' | 'unsupported'

/** How many lines the buffer keeps. A console left open overnight is bounded. */
const logBufferLimit = 1000

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
  const [logs, setLogs] = useState<ServiceLogEvent[]>([])
  const [filter, setFilter] = useState('')
  const [streamFilter, setStreamFilter] = useState<'all' | 'stdout' | 'stderr'>('all')
  const [isPaused, setIsPaused] = useState(false)
  const [autoScroll, setAutoScroll] = useState(true)
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
   * The stream, opened once per app/service.
   *
   * `isPaused` is deliberately NOT a dependency. It was, and the effect body
   * begins by emptying the buffer — so pressing Pause dropped the connection
   * and wiped the scrollback, and pressing Resume wiped it again. Pause holds
   * the view; it does not restart the stream. The ref is what lets the reader
   * see the current value without the effect re-running.
   */
  useEffect(() => {
    setLogs([])
    setStatus('connecting')
    if (!app || !service) return

    const controller = new AbortController()
    const url = `/api/v1/applications/${encodeURIComponent(app)}/services/${encodeURIComponent(service)}/logs`

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
                setLogs((prev) => {
                  const next = [...prev, event]
                  return next.length > logBufferLimit ? next.slice(next.length - logBufferLimit) : next
                })
              } catch {
                // One frame this build cannot parse is one line lost.
              }
            }
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
  }, [app, service])

  // Autoscroll to bottom
  useEffect(() => {
    if (autoScroll && terminalBodyRef.current) {
      terminalBodyRef.current.scrollTop = terminalBodyRef.current.scrollHeight
    }
  }, [logs, autoScroll])

  const handleScroll = () => {
    if (!terminalBodyRef.current) return
    const { scrollTop, scrollHeight, clientHeight } = terminalBodyRef.current
    const atBottom = scrollHeight - (scrollTop + clientHeight) < 30
    setAutoScroll(atBottom)
  }

  const filteredLogs = useMemo(() => {
    return logs.filter((log) => {
      if (streamFilter !== 'all' && log.stream !== streamFilter) return false
      if (filter && !log.message.toLowerCase().includes(filter.toLowerCase())) return false
      return true
    })
  }, [logs, streamFilter, filter])

  const copyLogs = () => {
    const text = filteredLogs
      .map((l) => `[${l.timestamp.slice(11, 19)}] [${l.stream.toUpperCase()}] ${l.message}`)
      .join('\n')
    void navigator.clipboard.writeText(text)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const exportLogs = () => {
    const text = filteredLogs
      .map((l) => `[${l.timestamp.slice(11, 19)}] [${l.stream.toUpperCase()}] ${l.message}`)
      .join('\n')
    const blob = new Blob([text], { type: 'text/plain;charset=utf-8' })
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
            onClick={() => setLogs([])}
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
          filteredLogs.map((log) => (
            <div
              key={`${log.timestamp}-${logs.indexOf(log)}`}
              className={`console-line ${log.stream === 'stderr' ? 'line-stderr' : 'line-stdout'}`}
            >
              <span className="line-num">{String(logs.indexOf(log) + 1).padStart(3, '0')}</span>
              <span className="line-time">{log.timestamp.slice(11, 19)}</span>
              <span className={`line-tag line-tag-${log.stream}`}>{log.stream.toUpperCase()}</span>
              <span className="line-content">{log.message}</span>
            </div>
          ))
        )}
      </div>

      {/* Floating Jump Button */}
      {!autoScroll && (
        <button
          type="button"
          className="jump-bottom-btn"
          onClick={() => {
            setAutoScroll(true)
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
            LINES: <strong>{filteredLogs.length}</strong> / {logs.length}
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
