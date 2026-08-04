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

  // The stream: a real connection, held open, parsed off a real ReadableStream.
  await expect(page.getByTestId('live-state')).toHaveText('live')

  // One event. The controller reconciles the fixture application on its own
  // tick, so nothing here presses anything — the frame arrives at a page that
  // was already watching, which is what "without a page reload" means.
  await expect(page.getByTestId('live-last')).toContainText('smoke', { timeout: 180_000 })

  expect(refusals, 'the browser refused something the policy in web.go forbids').toEqual([])
})
