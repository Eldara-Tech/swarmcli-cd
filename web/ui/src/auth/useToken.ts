// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useSyncExternalStore } from 'react'

import { getToken, subscribe } from './session'

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
