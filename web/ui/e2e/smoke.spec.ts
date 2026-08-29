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
//
// # One walkthrough, two editions
//
// Every assertion this file made about the licensed UI used to be a negative
// one — no SSO button, no licence badge, no Projects item, no swarm column —
// because a community build is the only one this repository can start. Each has
// an inverse, and the inverse is what the paid product is; a merged build that
// lit none of them would have passed every test in both repositories
// (swarmcli-cd-be#7).
//
// So the walkthrough is parameterised rather than copied. SWARMCLI_CD_EDITION
// says which build is on the other end, and only the login and the capability
// assertions branch on it — everything from the application list down is one
// path, which is the whole argument for doing it this way. The licence badge
// branches on neither: see where it is asserted for why it is a third thing. A
// licensed spec in the private repository could not have shared it, and the two
// would have drifted the first time a screen changed.
//
// This repository's own smoke.yml sets nothing and gets `community`, unchanged.
// The licensed run lives in swarmcli-cd-be, which starts a licensed binary
// against a real Keycloak and sets `business`. Nothing here is a disclosure: D8
// puts every licensed *screen* in this tree already — screens/Projects.tsx,
// components/LicenceBadge.tsx, the swarm column in screens/Applications.tsx —
// and src/capabilities.test.tsx already names all four.

const editions = ['community', 'business'] as const
type Edition = (typeof editions)[number]

const edition = (process.env.SWARMCLI_CD_EDITION ?? 'community') as Edition

const adminToken = process.env.SWARMCLI_CD_ADMIN_TOKEN ?? ''

// The Keycloak account the licensed run authenticates as. Read here rather than
// inline so that a business run started without them fails on the assertion
// below instead of typing an empty string into a login form and reporting that
// the identity provider refused it.
const ssoUser = process.env.SWARMCLI_CD_SSO_USER ?? ''
const ssoPassword = process.env.SWARMCLI_CD_SSO_PASSWORD ?? ''

// The marker a reload would destroy.
//
// "Without a page reload" is the half of the criterion that has no natural
// assertion: every check below passes just as well on a page that reloaded
// itself between them, and a reload is exactly what a UI polling with
// location.reload() would do. So the run stamps the window once, after login,
// and reads it back at the end — client-side navigation between the tabs keeps
// it, and nothing else in this test can.
const marker = '__swarmcliCdSmokeLoadId'

test('logs in, watches an application converge without reloading, and reads every screen', async ({
  page,
  baseURL,
}) => {
  // A typo in the variable is otherwise a run that quietly asserts the wrong
  // half — and the half it would fall back to is the one that passes against
  // any build, since every community assertion is an absence.
  expect(editions, `SWARMCLI_CD_EDITION is ${JSON.stringify(edition)}`).toContain(edition)

  if (edition === 'community') {
    expect(adminToken, 'SWARMCLI_CD_ADMIN_TOKEN must name the token the controller was started with')
      .not.toBe('')
  } else {
    expect(ssoUser, 'SWARMCLI_CD_SSO_USER must name the account registered with the identity provider').not.toBe('')
    expect(ssoPassword, 'SWARMCLI_CD_SSO_PASSWORD must be the password for that account').not.toBe('')
  }

  // A console listener, because a CSP refusal is reported there and nowhere
  // else: the page still renders, so every assertion below would pass while the
  // browser silently dropped a style or a script.
  //
  // Filtered by origin, because the licensed login leaves this application
  // entirely and authenticates against Keycloak. Whatever policy the identity
  // provider's own login page enforces is not this controller's to answer for,
  // and an unfiltered listener would fail the run on somebody else's page. A
  // message with no location is kept: losing one of ours costs more than an
  // occasional foreign line.
  const refusals: string[] = []
  page.on('console', (message) => {
    if (!/Content Security Policy/i.test(message.text())) return
    const from = message.location().url
    if (from !== '' && baseURL !== undefined && !from.startsWith(baseURL)) return
    refusals.push(message.text())
  })

  await page.goto('/')

  // The login screen is drawn from /ui/bootstrap.json, which a real browser
  // fetches with no credential — the one request in this application that must
  // not carry the bearer, and the only place that can be observed end to end.
  // What the document says is the whole difference between the two builds here:
  // a free controller advertises exactly one method and it is typed in, and a
  // licensed one advertises exactly one and it is a link.
  if (edition === 'community') {
    // An SSO button here would mean the public document had grown a method the
    // Apache-2.0 authorizer does not offer.
    await expect(page.getByLabel('Admin token')).toBeVisible()
    await expect(page.locator('.login-form input')).toHaveCount(1)
    await expect(page.locator('.login-start')).toHaveCount(0)

    await page.getByLabel('Admin token').fill(adminToken)
    await page.getByRole('button', { name: 'Sign in' }).click()
  } else {
    // And a box here would mean a deployment that has moved to SSO still
    // offering a browser the shared admin token.
    await expect(page.locator('.login-start')).toHaveCount(1)
    await expect(page.getByLabel('Admin token')).toHaveCount(0)

    // The single most valuable assertion in this file, and the only one that
    // cannot be faked anywhere: the redirect is followed to a real identity
    // provider, a real password is typed into it, and the browser comes back
    // holding a session this application can do nothing with except believe the
    // controller about. Everything below it in the licensed run is downstream
    // of that having worked.
    await page.getByRole('link', { name: 'Sign in with SSO' }).click()
    // Keycloak's own ids, which are stable across its themes and versions in a
    // way its localised labels are not. The client verify.sh registers sets no
    // consentRequired, so there is no consent screen between here and the
    // callback; one appearing would strand this on the password page rather
    // than failing obscurely somewhere later.
    await page.locator('#username').fill(ssoUser)
    await page.locator('#password').fill(ssoPassword)
    await page.locator('#kc-login').click()
  }

  // The shell, and which control it draws is itself the assertion. A token is
  // the tab's own to drop and signs out without a round trip; an SSO session is
  // a cookie only the controller can end, so it is a link to /auth/logout.
  if (edition === 'community') {
    await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()
  } else {
    await expect(page.getByRole('link', { name: 'Sign out' })).toBeVisible()
  }

  // Stamped here rather than before the login, because signing in is the last
  // thing on this path that legitimately could have replaced the document.
  await page.evaluate((key) => {
    const w = window as unknown as Record<string, unknown>
    w[key] = 'stamped'
  }, marker)

  // What the build says it is, asserted directly rather than only through what
  // it drew: a UI that hid everything for the wrong reason — a 404, a rejected
  // read — would look identical from the outside to one that hid it correctly.
  //
  // The bearer is attached only for the free run. The licensed one deliberately
  // sends nothing, because the browser's cookie is the credential and this is
  // the one place a test can prove the API accepts it: page.request shares the
  // context's cookie jar, so a licensed controller that authenticated the
  // *browser* and not its API calls fails here rather than in some screen that
  // renders empty.
  const capabilities = await page.request.get(
    '/api/v1/capabilities',
    edition === 'community' ? { headers: { Authorization: `Bearer ${adminToken}` } } : {},
  )
  expect(capabilities.ok(), `reading the capability document failed: ${capabilities.status()}`).toBe(true)
  const build = (await capabilities.json()) as {
    edition: string
    features: Record<string, boolean>
    licence: unknown
  }
  expect(build.edition).toBe(edition)

  if (edition === 'community') {
    // The free build draws none of the licensed shells.
    expect(Object.values(build.features).some(Boolean)).toBe(false)
    await expect(page.getByRole('link', { name: 'Projects' })).toHaveCount(0)
    await expect(page.getByRole('columnheader', { name: 'Swarm' })).toHaveCount(0)
  } else {
    // And the inverse, which is the thing swarmcli-cd-be sells and which
    // nothing anywhere asserted before. Named individually rather than "some
    // feature is on", because a licence that granted one of them and lit all
    // three would be the same bug as one that granted none.
    for (const granted of ['sso', 'projects', 'multi-swarm']) {
      expect(build.features[granted], `${granted} is not granted: ${JSON.stringify(build.features)}`).toBe(true)
    }
    await expect(page.getByRole('link', { name: 'Projects' })).toBeVisible()
    await expect(page.getByRole('columnheader', { name: 'Swarm' })).toBeVisible()
    // The claim out of the ID token, all the way through: the provider minted
    // it, the callback read it, the controller reported it on
    // /ui/bootstrap.json, and the header drew it. A shell that mounted without
    // this is one that believed a session it could not describe.
    await expect(page.locator('.session-name')).toHaveText(ssoUser)
  }

  // The badge is on a different axis from the edition, and running this against
  // a merged build is what made that concrete: it asks whether a licensed
  // *module* is linked, not what the licence currently grants.
  //
  // The Apache-2.0 binary reports `licence: null` and draws nothing, which is
  // #178's acceptance criterion — a chip that could only ever read one word is a
  // dead control the free product must not grow. The merged binary can never
  // report null, because under D20 the module is always linked; with no licence
  // installed it reports `absent`, and the badge says "no active licence,
  // install one, or activate a managed one, to turn the licensed features on;
  // an air-gapped swarm activates with :license lease install <file>".
  // Both are community builds. Both are right.
  //
  // So it is derived from the document rather than from the edition, which also
  // makes it the stronger assertion: the badge renders exactly when the
  // controller reports something to put in it.
  const badge = page.getByTestId('licence-badge')
  if (build.licence === null) {
    await expect(badge).toHaveCount(0)
  } else {
    await expect(badge).toBeVisible()
  }

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

  // Signing out, last of all because it is the one action here that deliberately
  // destroys the document — the marker above could not survive it, and nothing
  // after it would have a session.
  //
  // Only the licensed run does it, and only the licensed run needs to: a token
  // is dropped in the tab and Vitest proves that against a faked fetch, while
  // ending a cookie session is a route on a real controller that no fake can
  // stand in for. The failure it catches is a sign-out button linking to a 404,
  // which is what this looked like before swarmcli-cd-be served /auth/logout —
  // and which nothing below a browser can see.
  if (edition === 'business') {
    await page.getByRole('link', { name: 'Sign out' }).click()
    await expect(page.locator('.login-start')).toHaveCount(1)
    await expect(page.getByRole('link', { name: 'Sign out' })).toHaveCount(0)
  }

  expect(refusals, 'the browser refused something the policy in web.go forbids').toEqual([])
})
