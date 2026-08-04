// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useEffect, useState } from 'react'

// reachability is what this page can say about the controller behind it.
type reachability = 'checking' | 'reachable' | 'unreachable'

// App is the whole of the UI in phase A: enough to prove that the controller
// serves this bundle and that the browser can reach the API it came from.
//
// /healthz is the only endpoint it can ask. Everything under /api/v1 needs the
// admin token, which the login screen collects in B1, and the version this page
// would rather show belongs to the public discovery document of C1. Asking for
// either now would mean an endpoint designed around one screen and frozen the
// day it shipped.
export function App() {
  const [controller, setController] = useState<reachability>('checking')

  useEffect(() => {
    // The component can unmount before the answer arrives — StrictMode runs
    // this twice in development — and setting state afterwards is a warning
    // nobody can act on.
    let mounted = true
    const settle = (state: reachability) => {
      if (mounted) setController(state)
    }
    fetch('/healthz')
      .then((response) => settle(response.ok ? 'reachable' : 'unreachable'))
      .catch(() => settle('unreachable'))
    return () => {
      mounted = false
    }
  }, [])

  return (
    <main>
      <h1>swarmcli-cd</h1>
      <p>GitOps continuous delivery for Docker Swarm.</p>
      <p>
        controller: <span className={`state state-${controller}`}>{controller}</span>
      </p>
    </main>
  )
}
