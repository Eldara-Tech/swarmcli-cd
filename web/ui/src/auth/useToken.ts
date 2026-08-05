// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useSyncExternalStore } from 'react'

import { cookieSessionRefused, getToken, subscribe } from './session'

/**
 * useToken re-renders whoever calls it when the credential appears or goes.
 *
 * useSyncExternalStore rather than a context holding state, because the store
 * has writers that are not components: a 401 anywhere — a query, a mutation, the
 * event stream — clears the token from inside the fetch wrapper, and a context
 * would need every one of those to have been handed a setter.
 *
 * getToken is a safe snapshot even though it re-reads sessionStorage on every
 * call: it returns a string or null, both of which React compares by value.
 */
export function useToken(): string | null {
  return useSyncExternalStore(subscribe, getToken)
}

/**
 * useCookieSessionRefused re-renders whoever calls it when the controller stops
 * accepting the cookie this tab was signed in with.
 *
 * The same store and the same subscription, because it is the same event: a 401
 * ends the sign-in, and which credential it ended decides which of these two
 * hooks notices. A boolean is a safe snapshot for the same reason a string is.
 */
export function useCookieSessionRefused(): boolean {
  return useSyncExternalStore(subscribe, cookieSessionRefused)
}
