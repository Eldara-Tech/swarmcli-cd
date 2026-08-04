// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { expect, test } from '@playwright/test'

// The one path nothing else covers: a real browser, a real credential, a real
// streamed response.
//
// Every browser-only failure mode this UI has works perfectly in jsdom — the
// CSP that would refuse an inline style, fetch's ReadableStream, the bearer
// header on a cross-route request. Landing this as a skeleton now rather than
// at the end of Phase B is deliberate: discovering any of them later means
// bisecting four merged pull requests.
//
// It grows one assertion per screen as the screens land. What it must not grow
// into is a second component suite: anything provable against a faked fetch
// belongs in Vitest, where it costs no swarm.

const adminToken = process.env.SWARMCLI_CD_ADMIN_TOKEN ?? ''

test('logs in, renders the shell, reads the API and receives an event', async ({ page }) => {
  expect(adminToken, 'SWARMCLI_CD_ADMIN_TOKEN must name the token the controller was started with')
    .not.toBe('')

  // A console listener, because a CSP refusal is reported there and nowhere
  // else: the page still renders, so every assertion below would pass while the
  // browser silently dropped a style or a script.
  const refusals: string[] = []
  page.on('console', (message) => {
    if (/Content Security Policy/i.test(message.text())) refusals.push(message.text())
  })

  await page.goto('/')

  await page.getByLabel('Admin token').fill(adminToken)
  await page.getByRole('button', { name: 'Sign in' }).click()

  // The shell.
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()

  // One API call, with the bearer header the browser attached, against a
  // guarded endpoint that would answer 401 without it.
  await expect(page.getByTestId('application-count')).toHaveText(/application/)

  // The list itself, not just its count: a row for the fixture application,
  // carrying the link the detail screen hangs off. It reads out-of-sync because
  // smoke.yml declares syncPolicy.automated: false — the startup pass plans the
  // install and stops, which is also what leaves the sync below with real work
  // to raise an event about.
  await expect(
    page.getByRole('row').filter({ has: page.getByRole('link', { name: 'smoke' }) }),
  ).toContainText('out-of-sync')

  // The stream: a real connection, held open, parsed off a real ReadableStream.
  await expect(page.getByTestId('live-state')).toHaveText('live')

  // One event, and the stream has to be open before it is raised — that is the
  // whole assertion. There is no replay: api/stream.go emits no `id:`, so there
  // is no Last-Event-ID and a subscriber is only ever sent what happens while it
  // is attached.
  //
  // So the sync is triggered here rather than waited for. An earlier version
  // waited for the controller's own tick, on the reasoning that it reconciles
  // the fixture application unprompted — and it does, about a second after
  // startup, long before Playwright has a browser. After that the application is
  // converged, and a tick with nothing to do raises nothing at all, so the wait
  // could only ever time out.
  //
  // Through page.request rather than a button: the sync control is a later
  // issue's, and this assertion is about the transport, not the screen. When the
  // button lands, this becomes a click and the rest of the test is unchanged.
  const sync = await page.request.post('/api/v1/applications/smoke/sync', {
    headers: { Authorization: `Bearer ${adminToken}` },
  })
  expect(sync.ok(), `triggering the sync failed: ${sync.status()}`).toBe(true)

  await expect(page.getByTestId('live-last')).toContainText('smoke')

  expect(refusals, 'the browser refused something the policy in web.go forbids').toEqual([])
})
