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
// It grew one assertion per screen as the screens landed and is now the whole of
// acceptance criterion 5 (#176): log in, watch an application go from
// out-of-sync to synced with no page reload, read its diff, read its history,
// press sync, and see the event arrive. What it must not grow into is a second
// component suite: anything provable against a faked fetch belongs in Vitest,
// where it costs no swarm.

const adminToken = process.env.SWARMCLI_CD_ADMIN_TOKEN ?? ''

// The marker a reload would destroy.
//
// "Without a page reload" is the half of the criterion that has no natural
// assertion: every check below passes just as well on a page that reloaded
// itself between them, and a reload is exactly what a UI polling with
// location.reload() would do. So the run stamps the window once, after login,
// and reads it back at the end — client-side navigation between the tabs keeps
// it, and nothing else in this test can.
const marker = '__swarmcliCdSmokeLoadId'

test('logs in, watches an application converge without reloading, and reads every screen', async ({ page }) => {
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

  // The login screen is now drawn from /ui/bootstrap.json, which a real browser
  // fetches with no credential — the one request in this application that must
  // not carry the bearer, and the only place that can be observed end to end.
  // A free controller advertises exactly one method, so exactly one box and no
  // start link: an SSO button here would mean the public document had grown a
  // method the Apache-2.0 authorizer does not offer.
  await expect(page.getByLabel('Admin token')).toBeVisible()
  await expect(page.locator('.login-form input')).toHaveCount(1)
  await expect(page.locator('.login-start')).toHaveCount(0)

  await page.getByLabel('Admin token').fill(adminToken)
  await page.getByRole('button', { name: 'Sign in' }).click()

  // The shell.
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()

  // Stamped here rather than before the login, because signing in is the last
  // thing on this path that legitimately could have replaced the document.
  await page.evaluate((key) => {
    const w = window as unknown as Record<string, unknown>
    w[key] = 'stamped'
  }, marker)

  // The free build reads community, and draws none of the licensed shells for
  // it. The badge is the one to check by its absence rather than its text:
  // `licence` is null in a build with no licensed module, and a chip that
  // rendered anyway would be the dead control #178's acceptance criterion
  // forbids. The document is asserted directly because it is the thing the
  // shells are gated on, and a UI that hid everything for the wrong reason —
  // a 404, a rejected read — would look identical from the outside.
  const capabilities = await page.request.get('/api/v1/capabilities', {
    headers: { Authorization: `Bearer ${adminToken}` },
  })
  expect(capabilities.ok(), `reading the capability document failed: ${capabilities.status()}`).toBe(true)
  const build = (await capabilities.json()) as { edition: string; features: Record<string, boolean> }
  expect(build.edition).toBe('community')
  expect(Object.values(build.features).some(Boolean)).toBe(false)
  await expect(page.getByTestId('licence-badge')).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Projects' })).toHaveCount(0)
  await expect(page.getByRole('columnheader', { name: 'Swarm' })).toHaveCount(0)

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

  // The two read tabs, before the sync below and not after — which is the whole
  // reason smoke.yml declares `automated: false`. Un-deployed, the plan is an
  // install, so the diff is a real rendered manifest and the chart engine has no
  // revisions at all; deployed, both tabs go quiet and neither says anything a
  // fixture could not.
  await page.getByRole('link', { name: 'smoke' }).click()

  // The detail header, which is the thing the assertion at the bottom of this
  // test watches change. Read before the sync so that "went from out-of-sync to
  // synced" is two observations of the same element rather than one observation
  // and an assumption.
  const syncChip = page.locator('.header-chips .chip').first()
  await expect(syncChip).toHaveText('out-of-sync')

  // The engine's own rendering, reaching the colouriser. jsdom proves the
  // parser against strings this repository wrote; only this proves it against
  // the manifest the chart engine actually produces.
  await page.getByRole('link', { name: 'Diff' }).click()
  await expect(page.getByTestId('release-diff')).toContainText(/busybox/)

  // `"revisions": null` from the controller rather than from a fixture: the
  // payload .map() throws on, on the one screen that has to survive it.
  await page.getByRole('link', { name: 'History' }).click()
  await expect(page.getByTestId('release-history')).toContainText(/never deployed/)

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
  // Pressed rather than posted: this is the only place the button is exercised
  // against a controller that really starts a sync — the bearer header the
  // browser attaches to a write, the 202 body the screen reads, and the events
  // that write lands on the stream this same page is holding open. The fixture
  // is declared `automated: false` in smoke.yml precisely so this press has real
  // work to do; converged, it would raise nothing.
  await page.getByRole('button', { name: 'Sync' }).click()

  // Accepted, which is all the response can say: the sync runs detached from the
  // request and the outcome arrives below, on the stream.
  await expect(page.getByTestId('sync-outcome')).toContainText('accepted')

  await expect(page.getByTestId('live-last')).toContainText('smoke')

  // The criterion this test exists for, and the only assertion here that
  // exercises the whole chain end to end: the controller finishes the install,
  // raises sync-succeeded, the stream this page is holding open delivers it,
  // api/queries.ts turns it into an invalidation, react-query refetches the
  // detail, and the chip re-renders. Nothing was pressed and nothing was
  // reloaded — a break anywhere along it leaves this chip reading out-of-sync
  // for ever.
  //
  // Generously timed rather than retried. What is being waited for is a real
  // stack deploy on a real swarm; the default five seconds would make this
  // assert that a swarm is fast, and a smoke run that needs a retry to pass is
  // telling nobody anything.
  await expect(syncChip).toHaveText('synced', { timeout: 90_000 })

  // And the document is the one the login produced. Read last, because what it
  // has to cover is everything above it.
  const survived = await page.evaluate((key) => (window as unknown as Record<string, unknown>)[key], marker)
  expect(survived, 'the page reloaded: the convergence above was a fresh load, not a live update').toBe('stamped')

  expect(refusals, 'the browser refused something the policy in web.go forbids').toEqual([])
})
