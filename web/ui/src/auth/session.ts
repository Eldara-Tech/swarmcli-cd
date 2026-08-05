// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Whether this tab is signed in, and — when the credential is one it holds
// itself — what with.
//
// There are two, and only one of them is here. The admin token is the tab's own:
// the login screen collected it, this module keeps it, and every request
// attaches it. An SSO session is a cookie the browser will not show to script,
// so nothing in this file can see it and nothing here can drop it; what this
// module records about that one is only that the controller has stopped
// accepting it. Who to ask about the other direction is api/discovery.ts, which
// reads it off /ui/bootstrap.json.
//
// sessionStorage and not localStorage: the token is the swarm's root credential
// — the same one SWARMCLI_CD_ADMIN_TOKEN holds and the CLI already sends — and a
// per-tab lifetime with no persistence across a browser restart is the cheapest
// real reduction in exposure available without giving the server a second
// credential type. There is deliberately no "remember me".
//
// No React here. The store is what api/client.ts clears on a 401, and a module
// that a non-React caller has to import must not drag a renderer in with it;
// auth/useToken.ts is the three lines that make it a hook.

const storageKey = 'swarmcli-cd.token'

// listeners are the React subscriptions; see useToken. A Set rather than the
// `storage` event, which sessionStorage never fires for the tab that wrote it —
// and a tab is the whole scope of this store.
const listeners = new Set<() => void>()

export function getToken(): string | null {
  return sessionStorage.getItem(storageKey)
}

export function setToken(token: string): void {
  sessionStorage.setItem(storageKey, token)
  announce()
}

/**
 * clearSession ends this tab's sign-in, whichever credential it was resting on.
 *
 * Called from three places that must not be able to disagree: the sign-out
 * control, any 401 the fetch wrapper meets, and the event stream's own 401. It
 * was `clearToken` while a token was the only thing it could be.
 *
 * Both halves, unconditionally, rather than branching on which credential is in
 * play: a tab can hold each at once — a token typed into a licensed deployment
 * that also set a cookie — and a sign-out that ended one of them would leave the
 * operator signed in by the other, having been told they were not.
 *
 * Ending the cookie at the *controller* is a separate act, and one only a
 * navigation can perform: see Shell's sign-out.
 */
export function clearSession(): void {
  sessionStorage.removeItem(storageKey)
  cookieRefused = true
  announce()
}

// Whether the controller has refused the cookie this tab was relying on.
//
// A module-level flag rather than anything persisted, because it describes this
// document rather than this tab: reaching the controller's own /auth/logout, or
// completing a login, is a navigation, and a navigation starts a new document
// with this back at false — which is exactly when it should be.
let cookieRefused = false

/**
 * cookieSessionRefused reports whether a cookie session has been refused since
 * this document loaded.
 *
 * It is what keeps a lapsed cookie from stranding the tab. `clearSession` can
 * drop a token and the gate closes on the next render; it cannot drop a cookie,
 * so without this the shell would stay mounted with every request answering 401
 * and no way back to the login screen short of a reload — and a reload is the
 * one remedy that can loop.
 */
export function cookieSessionRefused(): boolean {
  return cookieRefused
}

/** subscribe registers fn until the returned function is called. */
export function subscribe(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

function announce(): void {
  for (const fn of listeners) fn()
}
