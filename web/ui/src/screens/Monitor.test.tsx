// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import { logLimit } from '../api/useEventStream'
import type { View } from '../api/types'
import { setToken } from '../auth/session'
import { clone, communityDiscovery, controller, json, openStream, pushLogStream, pushStream } from '../test/fakeApi'
import viewFull from '../test/fixtures/view-full.json'

const okStatus = {
  appSet: { mode: 'static', loadedAt: '2026-07-22T09:41:10Z', stale: false },
  applications: 1,
}

/** The list row, optionally declaring services the log console can offer. */
function edgeRow(...services: string[]): View {
  const view = clone(viewFull) as unknown as View
  view.spec.name = 'edge'
  view.status.sync.state = 'synced'
  view.status.health.state = 'healthy'
  delete view.status.drift
  delete view.status.error
  if (services.length === 0) delete view.status.releases
  else {
    view.status.releases = [
      {
        name: 'rel',
        chart: 'c',
        version: '1',
        revision: 1,
        action: 'unchanged',
        sync: 'synced',
        health: { state: 'healthy', services: { healthy: 1, total: 1 } },
        services: services.map((name) => ({ name, mode: 'replicated', running: 1, desired: 1, completed: 0, health: 'healthy' })),
      },
    ] as View['status']['releases']
  }
  return view
}

beforeEach(() => {
  sessionStorage.clear()
  queryClient.clear()
  window.history.pushState({}, '', '/')
  setToken('good')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

/**
 * deliver pushes a frame and lets React settle inside one act. The async form
 * is what flushes the microtasks the pushed frame's parse and state update run
 * in; the synchronous form would return before the terminal had the line.
 */
async function deliver(work: () => void): Promise<void> {
  await act(async () => {
    work()
    await Promise.resolve()
  })
}

describe('the monitor', () => {
  it('tails the controller event stream, one line per frame', async () => {
    // A driven stream: it delivers a frame only when the test pushes one, so the
    // terminal is empty until then and the assertion is not racing the open.
    const stream = pushStream()
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow()] }),
      '/api/v1/events': stream.open,
    })
    render(<App />)
    await screen.findByTestId('application-count')

    fireEvent.click(screen.getByRole('link', { name: 'Monitor' }))
    await screen.findByRole('heading', { name: 'Monitor' })
    expect(screen.getByText(/Waiting for controller activity/)).toBeDefined()

    await deliver(() => {
      stream.push({
        application: 'edge',
        swarm: '',
        type: 'sync-succeeded',
        message: 'converged',
        at: '2026-07-22T09:41:10Z',
      })
    })

    expect(await screen.findByText('sync-succeeded')).toBeDefined()
    expect(screen.getByText('converged')).toBeDefined()

  })

  it('offers the log console for the services an application actually reports', async () => {
    const stream = pushStream()
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow('edge-web')] }),
      '/api/v1/events': stream.open,
      // 501 is what an open-source build answers; the viewer must say so
      // rather than fill the console with lines.
      '/api/v1/applications/edge/services/edge-web/logs': () =>
        json(501, { error: 'this controller does not stream service logs' }),
    })
    render(<App />)
    await screen.findByTestId('application-count')
    fireEvent.click(screen.getByRole('link', { name: 'Monitor' }))
    await screen.findByRole('heading', { name: 'Monitor' })

    fireEvent.click(screen.getByRole('button', { name: 'Service Container Logs' }))
    expect(screen.getByText('APP')).toBeDefined()
    expect(screen.getByText('SVC')).toBeDefined()
    // The service the document reported, and no other.
    const svc = screen.getAllByRole('combobox')[1]
    expect([...svc.querySelectorAll('option')].map((o) => o.textContent)).toEqual(['edge-web'])
    expect(await screen.findByText(/does not stream service logs/)).toBeDefined()
  })

  it('does not invent a service for an application that reports none', async () => {
    // `${app}_web` was put in the picker and opened as a stream, so the console
    // tailed a service name nothing had ever reported.
    const stream = pushStream()
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow()] }),
      '/api/v1/events': stream.open,
    })
    render(<App />)
    await screen.findByTestId('application-count')
    fireEvent.click(screen.getByRole('link', { name: 'Monitor' }))
    await screen.findByRole('heading', { name: 'Monitor' })

    fireEvent.click(screen.getByRole('button', { name: 'Service Container Logs' }))
    expect(screen.getByText(/reports no services yet/)).toBeDefined()
    expect(screen.queryByText('edge_web')).toBeNull()
    expect(screen.queryByText('SVC')).toBeNull()
  })
})

describe('the scrollback bound, and what it broke', () => {
  it('keeps the newest logLimit frames and drops the oldest', async () => {
    const stream = pushStream()
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow()] }),
      '/api/v1/events': stream.open,
    })
    render(<App />)
    await screen.findByTestId('application-count')
    fireEvent.click(screen.getByRole('link', { name: 'Monitor' }))
    await screen.findByRole('heading', { name: 'Monitor' })

    // One past the cap, so exactly one frame must have fallen off the top.
    await deliver(() => {
      for (let i = 0; i <= logLimit; i++) {
        stream.push({
          application: `app-${i}`,
          swarm: '',
          type: 'sync-succeeded',
          message: `frame ${i}`,
          at: '2026-07-22T09:41:10Z',
        })
      }
    })

    expect(screen.queryByText('frame 0')).toBeNull()
    expect(screen.getByText('frame 1')).toBeDefined()
    expect(screen.getByText(`frame ${logLimit}`)).toBeDefined()
    expect(screen.getByText(`${logLimit} events`)).toBeDefined()
  })

  it('still follows the tail once the scrollback is full', async () => {
    // The effect was keyed on the frame *count*, which stops changing the moment
    // the cap is reached — so from frame 250 on the terminal silently stopped
    // scrolling, while lines still fell off the top under a pinned scrollTop,
    // dragging the view backwards. jsdom does no layout, so the geometry is
    // stubbed and what is asserted is that the component wrote scrollTop at all.
    const stream = pushStream()
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow()] }),
      '/api/v1/events': stream.open,
    })
    render(<App />)
    await screen.findByTestId('application-count')
    fireEvent.click(screen.getByRole('link', { name: 'Monitor' }))
    await screen.findByRole('heading', { name: 'Monitor' })

    const body = document.querySelector('.terminal-body') as HTMLElement
    Object.defineProperty(body, 'scrollHeight', { value: 10_000, configurable: true })
    Object.defineProperty(body, 'clientHeight', { value: 500, configurable: true })

    const push = (message: string) =>
      deliver(() => {
        stream.push({ application: 'edge', swarm: '', type: 'sync-succeeded', message, at: '2026-07-22T09:41:10Z' })
      })

    // Fill the scrollback to its cap, so the frame count can no longer move.
    for (let i = 0; i < logLimit; i++) await push(`frame ${i}`)
    expect(document.querySelectorAll('.term-line').length).toBe(logLimit)

    // Park the scroll somewhere it did not put us, then deliver one more frame.
    body.scrollTop = 0
    await push('after the cap')

    expect(body.scrollTop).toBe(10_000)
    const lines = document.querySelectorAll('.term-line')
    expect(lines[lines.length - 1].textContent).toContain('after the cap')
  })

  it('stops following once the reader has scrolled up', async () => {
    // An operator reading a failure was yanked to the bottom by the next frame,
    // and on a busy controller that is every second.
    const stream = pushStream()
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow()] }),
      '/api/v1/events': stream.open,
    })
    render(<App />)
    await screen.findByTestId('application-count')
    fireEvent.click(screen.getByRole('link', { name: 'Monitor' }))
    await screen.findByRole('heading', { name: 'Monitor' })

    const body = document.querySelector('.terminal-body') as HTMLElement
    Object.defineProperty(body, 'scrollHeight', { value: 10_000, configurable: true })
    Object.defineProperty(body, 'clientHeight', { value: 500, configurable: true })

    await deliver(() => {
      stream.push({ application: 'edge', swarm: '', type: 'sync-succeeded', message: 'first', at: '2026-07-22T09:41:10Z' })
    })

    // The reader scrolls away from the bottom, and the component is told.
    body.scrollTop = 200
    fireEvent.scroll(body)
    await deliver(() => {
      stream.push({ application: 'edge', swarm: '', type: 'sync-succeeded', message: 'second', at: '2026-07-22T09:41:10Z' })
    })

    expect(body.scrollTop).toBe(200)
    // The line still arrived; it just did not drag the view with it.
    expect(screen.getByText('second')).toBeDefined()
  })
})

describe('pausing the service log console', () => {
  /** A Monitor already switched to the log console, tailing edge/edge-web. */
  async function openLogs(stream: ReturnType<typeof pushLogStream>) {
    controller({
      ...communityDiscovery(),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow('edge-web')] }),
      '/api/v1/events': openStream,
      '/api/v1/applications/edge/services/edge-web/logs': stream.open,
    })
    render(<App />)
    await screen.findByTestId('application-count')
    fireEvent.click(screen.getByRole('link', { name: 'Monitor' }))
    await screen.findByRole('heading', { name: 'Monitor' })
    fireEvent.click(screen.getByRole('button', { name: 'Service Container Logs' }))
    await screen.findByText('APP')
  }

  const line = (message: string) => ({
    service: 'edge-web',
    stream: 'stdout',
    message,
    timestamp: '2026-08-28T06:00:00Z',
  })

  it('holds the view without dropping the connection or emptying the buffer', async () => {
    // `isPaused` was an effect dependency, and the effect body begins with
    // setLogs([]) — so Pause reopened the stream and wiped the scrollback, and
    // Resume wiped it again. The opposite of what a pause button does.
    const stream = pushLogStream()
    await openLogs(stream)

    await deliver(() => stream.push(line('before the pause')))
    expect(await screen.findByText('before the pause')).toBeDefined()

    fireEvent.click(screen.getByRole('button', { name: 'Pause' }))

    // Still there: pausing must not clear what has already been read.
    expect(screen.getByText('before the pause')).toBeDefined()

    await deliver(() => stream.push(line('while paused')))
    expect(screen.queryByText('while paused')).toBeNull()
    expect(screen.getByText('before the pause')).toBeDefined()

    fireEvent.click(screen.getByRole('button', { name: 'Resume' }))
    expect(screen.getByText('before the pause')).toBeDefined()

    await deliver(() => stream.push(line('after resuming')))
    expect(await screen.findByText('after resuming')).toBeDefined()
    // The buffer survived both transitions, which it cannot do if the effect
    // re-ran.
    expect(screen.getByText('before the pause')).toBeDefined()
  })
})
