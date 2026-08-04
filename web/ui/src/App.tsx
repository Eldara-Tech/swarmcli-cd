// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter, Route, Routes } from 'react-router'

import { useFeature } from './api/discovery'
import { useToken } from './auth/useToken'
import { Shell } from './Shell'
import { ApplicationDetail, ApplicationOverview } from './screens/ApplicationDetail'
import { ApplicationDiff } from './screens/ApplicationDiff'
import { ApplicationHistory } from './screens/ApplicationHistory'
import { Applications } from './screens/Applications'
import { ControllerStatusScreen } from './screens/ControllerStatus'
import { Login } from './screens/Login'
import { Projects } from './screens/Projects'

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
    <QueryClientProvider client={queryClient}>{token === null ? <Login /> : <SignedIn />}</QueryClientProvider>
  )
}

/**
 * The router, in its own component because the route table is now capability-
 * driven and a hook reading the capability document has to run *inside* the
 * provider above rather than in the component that renders it.
 */
function SignedIn() {
  // False in a build with no capability endpoint, so the free build's route
  // table is exactly the one Phase B shipped — no route, and no nav item in the
  // shell that could reach it.
  const projects = useFeature('projects')

  return (
    <BrowserRouter>
      <Routes>
        <Route element={<Shell />}>
          <Route index element={<Applications />} />
          {/* Encoded by the screens that link here, and decoded by
              useParams, so an application whose name needs escaping is
              still one route rather than a special case.

              The three tabs are children rather than state, so each is a
              link somebody can paste — and so the diff and the history are
              requested only while their own tab is open. Each carries a
              whole rendered manifest or a whole revision table, which is
              why they are separate endpoints in the first place. */}
          <Route path="applications/:app" element={<ApplicationDetail />}>
            <Route index element={<ApplicationOverview />} />
            <Route path="diff" element={<ApplicationDiff />} />
            <Route path="history" element={<ApplicationHistory />} />
          </Route>
          <Route path="status" element={<ControllerStatusScreen />} />
          {projects && <Route path="projects" element={<Projects />} />}
        </Route>
      </Routes>
    </BrowserRouter>
  )
}
