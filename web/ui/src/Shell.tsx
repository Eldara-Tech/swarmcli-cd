// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { Link, NavLink, Outlet } from 'react-router'

import { signOutPath, useFeature, useSession } from './api/discovery'
import { useEventStream } from './api/useEventStream'
import { clearSession } from './auth/session'
import { BrandMark, Icon, type IconName } from './components/Icon'
import { LicenceBadge } from './components/LicenceBadge'
import { LiveIndicator } from './components/LiveIndicator'
import { LiveProvider } from './live'

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
  // False in a build with no capability endpoint, so this shell is the one
  // Phase B shipped until a companion says otherwise; see api/discovery.ts.
  const projects = useFeature('projects')

  return (
    // The tab's one event stream, published to the Overview and Monitor
    // terminals below rather than re-opened by each; see live.tsx.
    <LiveProvider value={live}>
      <div className="app">
        <a href="#main-content" className="skip-link">
          Skip to content
        </a>
        <aside className="app-rail">
          <div className="rail-brand">
            <Link className="brand" to="/">
              <BrandMark />
              <span className="brand-text">swarmcli-cd</span>
            </Link>
          </div>
          {/* Controller is reachable from here as well as from the list's
              banner: an operator who suspects the app set has to be able to
              look without a failing application to click through from. */}
          <nav className="app-nav">
            <RailLink to="/overview" icon="layers" label="Overview" />
            <RailLink to="/" end icon="app" label="Applications" />
            <RailLink to="/monitor" icon="activity" label="Monitor" />
            <RailLink to="/diagnostics" icon="gauge" label="Diagnostics" />
            <RailLink to="/status" icon="service" label="Controller" />
            {/* Gated on its feature together with its route: a NavLink to a path
                no route claims matches nothing, unmounting the shell with it. */}
            {projects && <RailLink to="/projects" icon="app" label="Projects" />}
          </nav>
          {/* Renders nothing at all in a build with no licence to report, which
              is what keeps the free shell identical to Phase B's. */}
          <div className="rail-foot">
            <OperatorBadge />
            <LicenceBadge />
          </div>
        </aside>
        <div className="app-col">
          <header className="app-top">
            <LiveIndicator {...live} />
            <SignOut />
          </header>
          <main className="app-main" id="main-content">
            <Outlet />
          </main>
        </div>
      </div>
    </LiveProvider>
  )
}

/** One rail item: an icon and a label, current when its route is active. */
function RailLink({ to, end, icon, label }: { to: string; end?: boolean; icon: IconName; label: string }) {
  return (
    <NavLink to={to} end={end} className={({ isActive }) => (isActive ? 'nav-current' : undefined)}>
      <Icon name={icon} size={18} />
      <span className="nav-label">{label}</span>
    </NavLink>
  )
}

/**
 * Who is signed in, at the foot of the rail.
 *
 * A cookie session carries a name; the token build has none — it is the single
 * admin the token is — so it is labelled as that rather than left blank. Both
 * cases render the same element, which is what keeps the free shell's DOM
 * identical whether or not a capability document was read.
 */
function OperatorBadge() {
  const session = useSession()
  return (
    <div className="rail-user">
      <div className="rail-avatar">
        <Icon name="terminal" size={16} />
      </div>
      <div className="rail-user-meta">
        <span className="rail-user-name">{session?.name ?? 'Administrator'}</span>
        <span className="rail-user-sub label-caps">{session === undefined ? 'Token auth' : 'SSO session'}</span>
      </div>
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
  // Who the identity provider said this is now lives in the rail's OperatorBadge,
  // so it is not repeated here; this is only the control that ends the session.
  // `session` is still read above because a cookie session is the case where
  // signing out is a link to the controller rather than a button.
  return (
    <a className="sign-out" href={signOutPath} onClick={clearSession}>
      Sign out
    </a>
  )
}
