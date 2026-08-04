// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { Link, NavLink, Outlet } from 'react-router'

import { useFeature } from './api/discovery'
import { useEventStream } from './api/useEventStream'
import { clearToken } from './auth/session'
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
        <button type="button" className="sign-out" onClick={clearToken}>
          Sign out
        </button>
      </header>
      <main className="shell-main">
        <Outlet />
      </main>
    </div>
  )
}
