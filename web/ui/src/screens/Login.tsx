// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useState, type FormEvent } from 'react'

import { verify } from '../api/client'
import { useBootstrap, type LoginOption } from '../api/discovery'
import { setToken } from '../auth/session'

/**
 * The login screen: whatever /ui/bootstrap.json says a browser may sign in with.
 *
 * A method carrying a `start` path is a link the browser follows — the SSO
 * button — and one without is a box to type into. **An authorizer that
 * advertises no method at all is taken at its word**: the screen says so rather
 * than falling back to a token box for a credential the deployment has just
 * said it does not accept, which is the failure the whole document exists to
 * prevent.
 *
 * It is not a route. Rendering it in place of the shell, rather than navigating
 * to /login, keeps the URL the operator asked for: a bookmarked deep link
 * survives being signed out and back in, and — the reason that matters more —
 * there is no navigation that could ever carry the credential into a URL, a
 * history entry or a Referer header.
 */
export function Login() {
  const bootstrap = useBootstrap()
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

  // Nothing at all while the document is in flight, rather than a box drawn
  // optimistically and corrected: an SSO-only deployment would flash a box for
  // a credential it does not issue, which is the exact thing this screen now
  // exists to stop.
  if (bootstrap.isPending) return null

  // A document that could not be read is not an authorizer saying "no methods".
  // It is a controller that could not be asked — an older one with no such
  // route, a proxy that does not forward /ui/ — and drawing nothing there would
  // leave an operator with no way in to a controller that is working. The box
  // every authorizer was presented as before LoginMethods existed is the right
  // guess, and a wrong guess costs one rejected sign-in.
  const methods = bootstrap.isError ? [tokenFallback] : bootstrap.data.login

  // The one credential this screen collects. A second typed-in method is not a
  // shape it has: two boxes above one Sign in button is a form nobody can use,
  // and no authorizer offers one — the core's returns exactly the token method,
  // and a companion that has moved to SSO returns only a link.
  const typed = methods.find(isTypedIn)

  const card = (
    <>
      <h1>swarmcli-cd</h1>
      {methods.length === 0 && (
        <p className="login-error" role="alert">
          This controller advertises no way to sign in from a browser.
        </p>
      )}
      {methods.map((method) => {
        if (!isTypedIn(method)) {
          return (
            <a key={method.id} className="login-start" href={method.start}>
              {method.label}
            </a>
          )
        }
        if (method !== typed) return null
        return (
          <TypedCredential key={method.id} method={method} value={candidate} onChange={setCandidate} />
        )
      })}
      {error !== null && (
        <p className="login-error" role="alert">
          {error}
        </p>
      )}
      {typed !== undefined && (
        <button type="submit" disabled={checking || candidate === ''}>
          {checking ? 'Checking…' : 'Sign in'}
        </button>
      )}
    </>
  )

  // A form only when there is something to submit. A <form> holding nothing but
  // a link has no submitter and no submit event, and leaving one there would
  // put a form-action the CSP forbids one keystroke away from mattering.
  if (typed === undefined) {
    return (
      <main className="login">
        <div className="login-form">{card}</div>
      </main>
    )
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
        {card}
      </form>
    </main>
  )
}

/** The method assumed for a controller that could not be asked; see above. */
const tokenFallback: LoginOption = { id: 'token', label: 'Admin token' }

/** A method with nowhere to send the browser first is one the operator types in. */
function isTypedIn(method: LoginOption): boolean {
  return method.start === undefined || method.start === ''
}

/**
 * The box, as a fragment rather than a wrapper element.
 *
 * The fragment is deliberate: it keeps the card's children exactly the sequence
 * Phase B rendered — heading, label, input, hint, error, button — so a free
 * build's login screen is the one that shipped, down to the DOM.
 */
function TypedCredential({
  method,
  value,
  onChange,
}: {
  method: LoginOption
  value: string
  onChange: (value: string) => void
}) {
  return (
    <>
      <label htmlFor={method.id}>{method.label}</label>
      <input
        id={method.id}
        // A password field, and with autocomplete off: this is the swarm's
        // root credential, and a password manager writing it to disk would
        // undo the whole reason it lives in sessionStorage.
        type="password"
        autoComplete="off"
        spellCheck={false}
        autoFocus
        value={value}
        onChange={(event) => {
          onChange(event.target.value)
        }}
      />
      <p className="login-hint">
        {/* Named only for the credential it is actually the name of. A
            companion's own typed-in method has nothing to do with this
            variable, and telling an operator to look at one would send them
            to the wrong place. */}
        {method.id === 'token' && (
          <>
            The value of <code>SWARMCLI_CD_ADMIN_TOKEN</code>.{' '}
          </>
        )}
        It is kept for this tab only and is forgotten when the tab closes.
      </p>
    </>
  )
}
