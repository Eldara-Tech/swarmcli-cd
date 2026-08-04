// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { Link, Outlet } from 'react-router'

import { useEventStream } from './api/useEventStream'
import { clearToken } from './auth/session'
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

  return (
    <div className="shell">
      <header className="shell-header">
        <Link className="brand" to="/">
          swarmcli-cd
        </Link>
        <LiveIndicator {...live} />
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
