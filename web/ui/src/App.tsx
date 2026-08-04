// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter, Route, Routes } from 'react-router'

import { useToken } from './auth/useToken'
import { Shell } from './Shell'
import { Applications } from './screens/Applications'
import { Login } from './screens/Login'

/**
 * One cache for the tab, exported so that a test can empty it between renders —
 * a browser gets a fresh one by loading the page, and a test process does not.
 */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // The compensator for a stream that died silently. There is no keepalive
      // on the wire and deliberately no idle timer in the client, so this floor
      // is what bounds how long a screen can be wrong: a tab left open against a
      // controller it can no longer reach is at most half a minute stale rather
      // than indefinitely.
      staleTime: 30_000,
      refetchInterval: 30_000,
      // No retries. The one failure worth retrying — a connection that dropped —
      // is covered by the refetch above, and the one that must not be is a 401:
      // the fetch wrapper has already cleared the token by the time the error
      // arrives here, so three more attempts would each be made with no
      // credential and each answer 401 before the login screen appeared.
      retry: false,
    },
  },
})

/**
 * The whole application: a credential, or the screen that collects one.
 *
 * The token is read through the store rather than held in state here, so that
 * the 401 path is one rule in one place. Anything that meets a 401 — a query, a
 * mutation, the event stream — clears the store, this re-renders with no
 * credential, and the shell is replaced by the login screen. There is no error
 * boundary, no redirect and no route to keep consistent with it.
 */
export function App() {
  const token = useToken()

  return (
    <QueryClientProvider client={queryClient}>
      {token === null ? (
        <Login />
      ) : (
        <BrowserRouter>
          <Routes>
            <Route element={<Shell />}>
              <Route index element={<Applications />} />
            </Route>
          </Routes>
        </BrowserRouter>
      )}
    </QueryClientProvider>
  )
}
