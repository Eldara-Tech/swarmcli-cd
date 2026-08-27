// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useEffect, useMemo, useRef, useState } from 'react'

import { authorized } from '../api/client'
import type { ServiceLogEvent } from '../api/types'
import { Icon } from './Icon'

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
  const terminalBodyRef = useRef<HTMLDivElement>(null)

  // Live SSE stream fetch
  useEffect(() => {
    setLogs([])
    if (!app || !service) return

    const controller = new AbortController()
    const url = `/api/v1/applications/${encodeURIComponent(app)}/services/${encodeURIComponent(service)}/logs`

    async function startStream() {
      try {
        const response = await authorized(url, { signal: controller.signal })
        if (!response.ok || !response.body) {
          setLogs([
            {
              service,
              stream: 'stderr',
              message: `Failed to connect to service logs (${response.status})`,
              timestamp: new Date().toISOString(),
            },
          ])
          return
        }

        const reader = response.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''

        while (true) {
          const { done, value } = await reader.read()
          if (done) break

          buffer += decoder.decode(value, { stream: true })
          const lines = buffer.split('\n\n')
          buffer = lines.pop() ?? ''

          for (const block of lines) {
            for (const line of block.split('\n')) {
              if (line.startsWith('data: ')) {
                try {
                  const event = JSON.parse(line.slice(6)) as ServiceLogEvent
                  setLogs((prev) => {
                    if (isPaused) return prev
                    const next = [...prev, event]
                    return next.length > 1000 ? next.slice(next.length - 1000) : next
                  })
                } catch {
                  // Ignore corrupted frames
                }
              }
            }
          }
        }
      } catch (err) {
        if (!controller.signal.aborted) {
          setLogs((prev) => [
            ...prev,
            {
              service,
              stream: 'stderr',
              message: `Stream disconnected: ${err instanceof Error ? err.message : String(err)}`,
              timestamp: new Date().toISOString(),
            },
          ])
        }
      }
    }

    void startStream()
    return () => {
      controller.abort()
    }
  }, [app, service, isPaused])

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

  // Clean log message by stripping duplicated timestamp prefixes
  const sanitizeMessage = (msg: string) => {
    return msg.replace(/^\[\d{4}-\d{2}-\d{2}T[^\]]+\]\s*/, '').replace(/^\[\d{2}:\d{2}:\d{2}\]\s*/, '')
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
      .map((l) => `[${l.timestamp.slice(11, 19)}] [${l.stream.toUpperCase()}] ${sanitizeMessage(l.message)}`)
      .join('\n')
    void navigator.clipboard.writeText(text)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const exportLogs = () => {
    const text = filteredLogs
      .map((l) => `[${l.timestamp.slice(11, 19)}] [${l.stream.toUpperCase()}] ${sanitizeMessage(l.message)}`)
      .join('\n')
    const blob = new Blob([text], { type: 'text/plain;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${app}_${service}_logs.txt`
    a.click()
    URL.revokeObjectURL(url)
  }

  const currentAppObj = apps.find((a) => a.name === app)
  const availableServices = currentAppObj?.services ?? (app ? [`${app}_web`] : [])

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
            <span className={`pulse-dot ${isPaused ? 'pulse-warn' : 'pulse-ok'}`} />
            <span className="stream-badge-text">{isPaused ? 'PAUSED' : 'LIVE'}</span>
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
              {logs.length === 0
                ? `Connecting to container stream for ${app}/${service}...`
                : 'No lines match active filter.'}
            </p>
          </div>
        ) : (
          filteredLogs.map((log, idx) => (
            <div
              key={`${log.timestamp}-${idx}`}
              className={`console-line ${log.stream === 'stderr' ? 'line-stderr' : 'line-stdout'}`}
            >
              <span className="line-num">{String(idx + 1).padStart(3, '0')}</span>
              <span className="line-time">{log.timestamp.slice(11, 19)}</span>
              <span className={`line-tag line-tag-${log.stream}`}>{log.stream.toUpperCase()}</span>
              <span className="line-content">{sanitizeMessage(log.message)}</span>
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
          <span className="live-status-text">SSE 2.0 PROTOCOL</span>
        </div>
      </div>
    </div>
  )
}
