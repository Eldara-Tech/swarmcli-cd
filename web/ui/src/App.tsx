// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter, Route, Routes } from 'react-router'

import { useBootstrap, useFeature, useSignedIn } from './api/discovery'
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
 * The gate is a child rather than this component, because deciding it now needs
 * a query — see Authenticated — and a hook reading one has to run inside the
 * provider rather than in the component that renders it. That is the same move
 * SignedIn already made when the route table became capability-driven.
 */
export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <Authenticated />
    </QueryClientProvider>
  )
}

/**
 * Whether this tab is signed in, and with which of the two credentials.
 *
 * The token is read through the store rather than held in state here, so that
 * the 401 path is one rule in one place. Anything that meets a 401 — a query, a
 * mutation, the event stream — ends the session in the store, this re-renders
 * with no credential, and the shell is replaced by the login screen. There is no
 * error boundary, no redirect and no route to keep consistent with it.
 *
 * A token needs nothing confirmed and is answered first, which keeps the free
 * build's path exactly what it was: no document is waited for, and a tab that
 * has one renders the shell immediately.
 *
 * A cookie is the other case and the reason this is not one line. The browser
 * will not show it to script, so the only way to know it is there is that the
 * controller says so — and it says so on the document the login screen was
 * going to block on anyway, which is why waiting here costs nothing. Rendering
 * nothing meanwhile rather than the login screen: drawing one and taking it
 * away is how an operator who is already signed in gets shown a box asking them
 * to sign in.
 */
function Authenticated() {
  const signedIn = useSignedIn()
  const bootstrap = useBootstrap()

  // First, and so a token never waits: useSignedIn answers true on one the
  // moment the store has it, whatever the document is doing.
  if (signedIn) return <SignedIn />

  // "Has the controller ever answered", not "is a request in flight", and the
  // difference is a loop rather than a nicety. A query that has failed refetches
  // when a new observer mounts, and Login is a new observer — so gating on
  // isPending unmounts the screen that had just mounted, which settles the
  // query, which mounts it again. It spun bootstrap.json for ever without
  // drawing anything.
  if (!bootstrap.isFetched) return null

  // A document that could not be read is not a controller saying "no session".
  // It lands here, on Login, which has its own answer for that case and offers
  // the token box rather than a screen with no way in.
  return <Login />
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
