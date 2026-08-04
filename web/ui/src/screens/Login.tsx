// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useState, type FormEvent } from 'react'

import { verify } from '../api/client'
import { setToken } from '../auth/session'

/**
 * The login screen: one box, for the admin token.
 *
 * Unconditional for now. C2 makes it conditional on the login methods
 * /ui/bootstrap.json advertises, so that a deployment that has moved to SSO
 * stops offering a box for a credential it no longer issues — but that document
 * does not exist yet, and a box hidden behind a flag nothing sets would be a
 * dead control.
 *
 * It is not a route. Rendering it in place of the shell, rather than navigating
 * to /login, keeps the URL the operator asked for: a bookmarked deep link
 * survives being signed out and back in, and — the reason that matters more —
 * there is no navigation that could ever carry the credential into a URL, a
 * history entry or a Referer header.
 */
export function Login() {
  const [candidate, setCandidate] = useState('')
  const [checking, setChecking] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function submit(event: FormEvent) {
    // No action and no method on the form, so this is the only thing that can
    // submit it — but preventDefault is what stops a browser from ever falling
    // back to a GET that would put the token in the address bar.
    event.preventDefault()
    setChecking(true)
    setError(null)
    try {
      if (await verify(candidate)) {
        // Stored only now that the controller has accepted it, so a wrong token
        // never signs anybody in for the length of a request; see client.verify.
        setToken(candidate)
        return
      }
      setError('The controller rejected that token.')
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : 'The controller could not be reached.')
    } finally {
      setChecking(false)
    }
  }

  return (
    <main className="login">
      <form
        className="login-form"
        // Voided rather than handed over as an async handler: a submit listener
        // that returns a promise is one nothing awaits, so a rejection inside it
        // would be an unhandled rejection rather than the message below.
        onSubmit={(event) => {
          void submit(event)
        }}
      >
        <h1>swarmcli-cd</h1>
        <label htmlFor="token">Admin token</label>
        <input
          id="token"
          // A password field, and with autocomplete off: this is the swarm's
          // root credential, and a password manager writing it to disk would
          // undo the whole reason it lives in sessionStorage.
          type="password"
          autoComplete="off"
          spellCheck={false}
          autoFocus
          value={candidate}
          onChange={(event) => {
            setCandidate(event.target.value)
          }}
        />
        <p className="login-hint">
          The value of <code>SWARMCLI_CD_ADMIN_TOKEN</code>. It is kept for this tab only and is
          forgotten when the tab closes.
        </p>
        {error !== null && (
          <p className="login-error" role="alert">
            {error}
          </p>
        )}
        <button type="submit" disabled={checking || candidate === ''}>
          {checking ? 'Checking…' : 'Sign in'}
        </button>
      </form>
    </main>
  )
}
