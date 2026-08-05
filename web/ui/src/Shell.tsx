// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { Link, NavLink, Outlet } from 'react-router'

import { signOutPath, useFeature, useSession } from './api/discovery'
import { useEventStream } from './api/useEventStream'
import { clearSession } from './auth/session'
import { LicenceBadge } from './components/LicenceBadge'
import { LiveIndicator } from './components/LiveIndicator'

/**
 * The shell every screen hangs off: the header, the live indicator, and the
 * outlet the router renders into.
 *
 * The event stream is opened here, once, for the reason useEventStream gives —
 * one connection per tab, never one per screen. That makes this component the
 * thing that must not unmount on navigation, which is why it is the router's
 * layout route rather than something a screen renders.
 */
export function Shell() {
  const live = useEventStream()
  // False in a build with no capability endpoint, so this header is the one
  // Phase B shipped until a companion says otherwise; see api/discovery.ts.
  const projects = useFeature('projects')

  return (
    <div className="shell">
      <header className="shell-header">
        <Link className="brand" to="/">
          swarmcli-cd
        </Link>
        {/* The controller screen is reachable from here as well as from the
            list's banner: an operator who suspects the app set has to be able
            to look without a failing application to click through from. */}
        <nav className="shell-nav">
          <NavLink to="/" end className={({ isActive }) => (isActive ? 'nav-current' : undefined)}>
            Applications
          </NavLink>
          {projects && (
            <NavLink to="/projects" className={({ isActive }) => (isActive ? 'nav-current' : undefined)}>
              Projects
            </NavLink>
          )}
          <NavLink to="/status" className={({ isActive }) => (isActive ? 'nav-current' : undefined)}>
            Controller
          </NavLink>
        </nav>
        <LiveIndicator {...live} />
        {/* Renders nothing at all in a build with no licence to report, which
            is what keeps the free header identical to Phase B's. */}
        <LicenceBadge />
        <SignOut />
      </header>
      <main className="shell-main">
        <Outlet />
      </main>
    </div>
  )
}

/**
 * Signing out, which is two different acts depending on what signed you in.
 *
 * A token is the tab's own: dropping it is enough, it happens without a round
 * trip, and this is the control the free build has always had — rendered
 * identically, because `session` is absent in every build that issues no cookie.
 *
 * A cookie is the controller's, and no amount of clearing anything here ends it:
 * the credential is `HttpOnly` and outlives the click by twelve hours. So this
 * is a link rather than a button, to the path docs/extensibility.md reserves for
 * exactly this, and the store is cleared on the way out as well — a tab can hold
 * both at once, and a sign-out that ended one of them would leave the operator
 * signed in by the other having been told they were not.
 */
function SignOut() {
  const session = useSession()

  if (session === undefined) {
    return (
      <button type="button" className="sign-out" onClick={clearSession}>
        Sign out
      </button>
    )
  }
  return (
    <>
      {/* Who the identity provider said this is. The controller logs the same
          name against every sync this session asks for, and an operator who
          signed in through their company's provider and landed on a shell that
          could not say who they were would have been signed in as nobody. */}
      <span className="session-name">{session.name}</span>
      <a className="sign-out" href={signOutPath} onClick={clearSession}>
        Sign out
      </a>
    </>
  )
}
