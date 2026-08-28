// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { App, queryClient } from '../App'
import { logLimit } from '../api/useEventStream'
import type { View } from '../api/types'
import { setToken } from '../auth/session'
import {
  clone,
  communityDiscovery,
  controller,
  discoveryWith,
  json,
  openStream,
  pushLogStream,
  pushStream,
} from '../test/fakeApi'
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
      ...discoveryWith({ logs: true }),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow('edge-web')] }),
      '/api/v1/events': stream.open,
      // The document said this build streams logs and the endpoint says it
      // does not. That disagreement is no longer any build's ordinary state —
      // a controller with no streamer now reports the capability off and the
      // control is absent — but the viewer must still say so rather than fill
      // the console with lines, because the endpoint is the authority.
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
      ...discoveryWith({ logs: true }),
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
      ...discoveryWith({ logs: true }),
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

describe('a controller notice is not container output', () => {
  /** A Monitor already switched to the log console, tailing edge/edge-web. */
  async function openLogs(stream: ReturnType<typeof pushLogStream>) {
    controller({
      ...discoveryWith({ logs: true }),
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

  // The controller drops lines rather than buffering for a console that has
  // stopped reading, and says so in the stream. Such a line carries
  // stream: 'stderr' so the filter does not hide it from the operator looking
  // for trouble — which is exactly what makes it indistinguishable from a line
  // the container wrote unless it is labelled differently.
  it('labels a dropped-output notice as the controller talking', async () => {
    const stream = pushLogStream()
    await openLogs(stream)

    await deliver(() =>
      stream.push({
        service: 'edge-web',
        stream: 'stderr',
        message: '12 log lines were dropped because this client could not keep up',
        timestamp: '2026-08-28T06:00:00Z',
        notice: true,
      }),
    )

    const text = await screen.findByText(/12 log lines were dropped/)
    const row = text.closest('.console-line')
    expect(row?.className).toContain('line-notice')
    expect(row?.textContent).toContain('NOTICE')
    // And not the label a container line gets, which is the whole point.
    expect(row?.textContent).not.toContain('STDERR')
  })

  // It carries the stderr label so that an operator filtered to stderr — the
  // one debugging the incident the drop happened during — still sees it.
  it('keeps a notice visible under the stderr filter', async () => {
    const stream = pushLogStream()
    await openLogs(stream)

    await deliver(() =>
      stream.push({
        service: 'edge-web',
        stream: 'stderr',
        message: 'a log line longer than 1MiB was truncated',
        timestamp: '2026-08-28T06:00:00Z',
        notice: true,
      }),
    )
    fireEvent.click(screen.getByRole('button', { name: 'Stderr' }))

    expect(screen.getByText(/longer than 1MiB was truncated/)).toBeDefined()
  })
})


describe('the log console is gated on the build reporting a streamer', () => {
  async function openMonitor(): Promise<void> {
    render(<App />)
    await screen.findByTestId('application-count')
    fireEvent.click(screen.getByRole('link', { name: 'Monitor' }))
    await screen.findByRole('heading', { name: 'Monitor' })
  }

  // The stock build has no LogStreamer, so /logs answers 501 — and the console
  // offered the tab anyway, which meant the operator discovered that by
  // clicking it. The control is now absent, and the controller stream, which
  // every build serves, is what Monitor is.
  it('omits the control on a build that cannot stream logs', async () => {
    const asked = vi.fn(() => json(501, { error: 'this controller does not stream service logs' }))
    controller({
      ...discoveryWith({ logs: false }),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow('edge-web')] }),
      '/api/v1/events': openStream,
      '/api/v1/applications/edge/services/edge-web/logs': asked,
    })
    await openMonitor()

    expect(screen.queryByRole('button', { name: 'Service Container Logs' })).toBeNull()
    // The remaining control is still the one that is selected, so removing the
    // other one did not leave the segment showing nothing as active.
    expect(screen.getByRole('button', { name: 'Controller Events' }).className).toContain('active')
    // And no stream was opened for a service the build cannot tail.
    expect(asked).not.toHaveBeenCalled()
  })

  it('offers the control on a build that can', async () => {
    controller({
      ...discoveryWith({ logs: true }),
      '/api/v1/status': () => json(200, okStatus),
      '/api/v1/applications': () => json(200, { applications: [edgeRow('edge-web')] }),
      '/api/v1/events': openStream,
      '/api/v1/applications/edge/services/edge-web/logs': openStream,
    })
    await openMonitor()

    expect(screen.getByRole('button', { name: 'Service Container Logs' })).toBeDefined()
  })
})

// ---------------------------------------------------------------------------
// #266 — the console names which replica wrote a line
// ---------------------------------------------------------------------------

/** A Monitor already switched to the log console, tailing edge/edge-web. */
async function openConsole(stream: ReturnType<typeof pushLogStream>): Promise<void> {
  controller({
    ...discoveryWith({ logs: true }),
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

/** One line as the controller sends it, with whatever labels the test is about. */
function logLine(message: string, labels: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    service: 'edge-web',
    stream: 'stdout',
    message,
    timestamp: '2026-08-28T06:00:00Z',
    ...labels,
  }
}

function rowFor(message: string): Element | null {
  return screen.getByText(message).closest('.console-line')
}

describe('the console names which replica wrote a line', () => {
  // The daemon says which task and which node in ids, and an operator reads
  // `docker service ps`, which says `.1`. Showing the id would leave them
  // correlating by eye against another window, which is the whole of #266.
  it('draws the replica slot, with the long form on hover', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    await deliver(() =>
      stream.push(
        logLine('listening on :8080', {
          taskID: 'k9r2m1x8p4qzabcd',
          nodeID: 'node-a',
          nodeHostname: 'swarm-w1',
          slot: 3,
        }),
      ),
    )

    const cell = rowFor('listening on :8080')?.querySelector('.line-replica')
    expect(cell?.textContent).toBe('.3')
    // The column has no width for either, and a support conversation quotes
    // both, so they live on the hover rather than being dropped.
    expect(cell?.getAttribute('title')).toContain('k9r2m1x8p4qzabcd')
    expect(cell?.getAttribute('title')).toContain('swarm-w1')
  })

  // A global service's tasks have no slot — the daemon reports 0 and means it —
  // and neither does a task the controller could not name. Drawing `.0` would
  // name a replica that does not exist, so the node is the label instead.
  it('falls back to the hostname when there is no slot', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    await deliver(() =>
      stream.push(logLine('global task up', { taskID: 'task-g', nodeID: 'node-a', nodeHostname: 'swarm-w1' })),
    )

    const cell = rowFor('global task up')?.querySelector('.line-replica')
    expect(cell?.textContent).toBe('swarm-w1')
    expect(cell?.textContent).not.toContain('.0')
  })

  // Both lookups can fail — they are separate calls on purpose — and the line
  // still arrives. The id truncated the way the daemon's own tooling truncates
  // it is the last honest thing left to say.
  it('falls back to a truncated task id when the controller named neither', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    await deliver(() => stream.push(logLine('unnamed', { taskID: 'k9r2m1x8p4qzabcdefgh', nodeID: 'node-a' })))

    expect(rowFor('unnamed')?.querySelector('.line-replica')?.textContent).toBe('k9r2m1x8p4qz')
  })

  // The colour is the second cue, never the only one, and it has to be stable:
  // one task looking like itself is the entire point.
  it('gives two tasks two colours and keeps each one', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    await deliver(() => {
      stream.push(logLine('from one', { taskID: 'task-1', slot: 1 }))
      stream.push(logLine('from two', { taskID: 'task-2', slot: 2 }))
      stream.push(logLine('from one again', { taskID: 'task-1', slot: 1 }))
    })

    const first = rowFor('from one')?.querySelector('.line-replica')?.className
    const second = rowFor('from two')?.querySelector('.line-replica')?.className
    const again = rowFor('from one again')?.querySelector('.line-replica')?.className
    expect(first).not.toBe(second)
    expect(again).toBe(first)
  })

  // A notice is the controller talking about the stream, so no task wrote it
  // and none is claimed for it.
  it('claims no replica for a controller notice', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    await deliver(() =>
      stream.push(logLine('12 log lines were dropped', { stream: 'stderr', notice: true })),
    )

    expect(rowFor('12 log lines were dropped')?.querySelector('.line-replica')?.textContent).toBe('')
  })

  it('narrows to one task and back', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    await deliver(() => {
      stream.push(logLine('from one', { taskID: 'task-1', slot: 1 }))
      stream.push(logLine('from two', { taskID: 'task-2', slot: 2 }))
    })

    fireEvent.change(screen.getByLabelText('Filter by task'), { target: { value: 'task-1' } })
    expect(screen.getByText('from one')).toBeDefined()
    expect(screen.queryByText('from two')).toBeNull()

    fireEvent.change(screen.getByLabelText('Filter by task'), { target: { value: '' } })
    expect(screen.getByText('from two')).toBeDefined()
  })

  // A notice reports on the stream itself — output dropped, a line truncated —
  // so it is about every task, and narrowing to one must not hide the reason a
  // gap appeared in it.
  it('keeps a notice visible while narrowed to one task', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    await deliver(() => {
      stream.push(logLine('from one', { taskID: 'task-1', slot: 1 }))
      stream.push(logLine('12 log lines were dropped', { stream: 'stderr', notice: true }))
    })

    fireEvent.change(screen.getByLabelText('Filter by task'), { target: { value: 'task-1' } })

    expect(screen.getByText('12 log lines were dropped')).toBeDefined()
  })

  // Copy and Export are what leaves the browser and gets pasted into an
  // incident channel, so the label has to survive them.
  it('writes the replica into what Copy puts on the clipboard', async () => {
    const written: string[] = []
    vi.stubGlobal('navigator', {
      ...navigator,
      clipboard: { writeText: (t: string) => { written.push(t); return Promise.resolve() } },
    })
    const stream = pushLogStream()
    await openConsole(stream)

    await deliver(() => stream.push(logLine('served /', { taskID: 'task-1', slot: 3 })))
    fireEvent.click(screen.getByRole('button', { name: 'Copy' }))

    expect(written[0]).toBe('[06:00:00] [.3] [STDOUT] served /')
  })
})

// ---------------------------------------------------------------------------
// #267 — reading further back than the tail
// ---------------------------------------------------------------------------

describe('the console can read further back than its tail', () => {
  // A string, because that is all src/api ever passes; the fake asserts the
  // same thing in client.test.ts.
  function requestedUrls(): string[] {
    return vi.mocked(fetch).mock.calls.map((c) => c[0] as string)
  }

  // The default is what every build did before there was a window, and it
  // stays a bare request rather than one spelling out the old default.
  it('opens with no window at all until one is chosen', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    const opened = requestedUrls().filter((u) => u.includes('/logs'))
    expect(opened).toHaveLength(1)
    expect(opened[0]).toBe('/api/v1/applications/edge/services/edge-web/logs')
  })

  // The daemon applies the tail first and drops what is older than since
  // afterwards, so a preset that sent only `since` would ask for six hours and
  // be answered with a hundred lines. The pair is the control.
  it('asks for a window as both a since and the tail that bounds it', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '1h' }))
      await Promise.resolve()
    })

    const opened = requestedUrls().filter((u) => u.includes('/logs'))
    expect(opened[opened.length - 1]).toContain('since=1h')
    expect(opened[opened.length - 1]).toContain('tail=20000')
  })

  // A different window is a different read. The daemon cannot extend a stream
  // backwards, so the buffer is replaced rather than prepended to, and a
  // console still showing the old lines would be showing two windows at once.
  it('replaces the buffer rather than adding to it', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    await deliver(() => stream.push(logLine('from the tail read', { taskID: 'task-1', slot: 1 })))
    expect(screen.getByText('from the tail read')).toBeDefined()

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '6h' }))
      await Promise.resolve()
    })

    expect(screen.queryByText('from the tail read')).toBeNull()
  })

  // The buffer is far larger than the DOM can hold, and the gap is stated
  // rather than hidden: a console silently drawing a fraction of what it has
  // is a console that lies. Everything else — the filters, Copy, Export —
  // still works on all of it.
  it('draws a bounded number of rows and says how many it is holding', async () => {
    const stream = pushLogStream()
    await openConsole(stream)

    const many = Array.from({ length: 2500 }, (_, i) =>
      logLine(`line ${i}`, { taskID: 'task-1', slot: 1 }),
    )
    await deliver(() => stream.pushAll(many))

    const drawn = document.querySelectorAll('.console-line').length
    // Bounded on both sides. "fewer than everything" is also true of a console
    // that drew nothing, which is the failure this is between.
    expect(drawn).toBeGreaterThan(100)
    expect(drawn).toBeLessThan(2500)
    expect(screen.getByText(/Showing the newest/)).toBeDefined()
    // The newest, because a console is read from the bottom.
    expect(screen.getByText('line 2499')).toBeDefined()
    expect(screen.queryByText('line 0')).toBeNull()
  })

  // The cap is on the render and on nothing else. An operator who cannot scroll
  // to a line must still be able to find it, or the buffer is decorative.
  it('filters and exports the whole buffer and not only the drawn rows', async () => {
    const written: string[] = []
    vi.stubGlobal('navigator', {
      ...navigator,
      clipboard: { writeText: (t: string) => { written.push(t); return Promise.resolve() } },
    })
    const stream = pushLogStream()
    await openConsole(stream)

    const many = Array.from({ length: 2500 }, (_, i) =>
      logLine(i === 0 ? 'the needle' : `line ${i}`, { taskID: 'task-1', slot: 1 }),
    )
    await deliver(() => stream.pushAll(many))

    // Undrawn, and still on the clipboard.
    expect(screen.queryByText('the needle')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Copy' }))
    expect(written[0]).toContain('the needle')

    // And reachable, which is what the banner tells the operator to do.
    fireEvent.change(screen.getByPlaceholderText(/Grep logs/), { target: { value: 'needle' } })
    expect(screen.getByText('the needle')).toBeDefined()
  })
})
