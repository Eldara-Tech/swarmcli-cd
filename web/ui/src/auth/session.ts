// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Where the admin token lives for as long as this tab does.
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
 * clearToken forgets the credential, which returns the tab to the login screen.
 *
 * Called from three places that must not be able to disagree: the sign-out
 * button, any 401 the fetch wrapper meets, and the event stream's own 401.
 */
export function clearToken(): void {
  sessionStorage.removeItem(storageKey)
  announce()
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
