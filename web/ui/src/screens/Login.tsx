// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { Fragment, useState, type FormEvent } from 'react'

import { verify } from '../api/client'
import { useBootstrap, type LoginOption } from '../api/discovery'
import { setToken } from '../auth/session'
import { BrandMark } from '../components/Icon'
import { PageTitle } from '../components/PageTitle'

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
  const [username, setUsername] = useState('')
  const [candidate, setCandidate] = useState('')
  const [checking, setChecking] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // name is the username to sign in with, or undefined for a method that asks
  // for none.
  async function submit(event: FormEvent, name: string | undefined) {
    // No action and no method on the form, so this is the only thing that can
    // submit it — but preventDefault is what stops a browser from ever falling
    // back to a GET that would put the token in the address bar.
    event.preventDefault()
    setChecking(true)
    setError(null)
    try {
      if (await verify(candidate, name)) {
        // Stored only now that the controller has accepted it, so a wrong token
        // never signs anybody in for the length of a request; see client.verify.
        setToken(candidate, name)
        return
      }
      setError(
        name === undefined ? 'The controller rejected that token.' : 'The controller rejected that username and token.',
      )
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
  // shape it has: two credentials above one Sign in button is a form nobody can
  // use, and no authorizer offers one — the core's returns exactly the token
  // method, and a companion with SSO live returns its link before it.
  const typed = methods.find(isTypedIn)
  // The username belongs to the admin token, SWARMCLI_CD_ADMIN_USERNAME, so
  // only that method asks for one.
  const asksUsername = typed?.id === 'token'

  const card = (
    <>
      <PageTitle page="Sign in" />
      <div className="login-brand">
        <BrandMark />
        <div className="login-title">
          <h1>swarmcli-cd</h1>
          <p className="login-tagline">Continuous delivery for Docker Swarm</p>
        </div>
      </div>
      {methods.length === 0 && (
        <p className="login-error" role="alert">
          This controller advertises no way to sign in from a browser.
        </p>
      )}
      {methods.map((method, index) => {
        if (!isTypedIn(method)) {
          return (
            <a key={method.id} className="login-start" href={method.start}>
              {method.label}
            </a>
          )
        }
        if (method !== typed) return null
        return (
          <Fragment key={method.id}>
            {/* Between a link above and the form below, so the two read as
                alternatives rather than as steps. */}
            {methods.slice(0, index).some((m) => !isTypedIn(m)) && <p className="login-or">or</p>}
            {asksUsername && <Username value={username} onChange={setUsername} />}
            <TypedCredential method={method} value={candidate} focus={!asksUsername} onChange={setCandidate} />
          </Fragment>
        )
      })}
      {error !== null && (
        <p className="login-error" role="alert">
          {error}
        </p>
      )}
      {typed !== undefined && (
        <button type="submit" disabled={checking || candidate === '' || (asksUsername && username === '')}>
          {checking ? 'Checking…' : 'Sign in'}
        </button>
      )}
      {/* Two links out of the credential page. rel="noreferrer" so the tab each
          opens cannot read back through window.opener, and the referrer — this
          URL — never leaves with it.

          The notices are same-origin and served by web.go out of the bundle, so
          they are reachable on a controller with no route to the internet;
          swarmcli.io is not, which is why only one of them is the obligation. */}
      <p className="login-foot">
        <a href="https://swarmcli.io" target="_blank" rel="noreferrer">
          swarmcli.io
        </a>
        <a href="/THIRD-PARTY-NOTICES.txt" target="_blank" rel="noreferrer">
          Third-party notices
        </a>
      </p>
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
          void submit(event, asksUsername ? username : undefined)
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

/** The username the admin token is signed in with; see the hint below the token. */
function Username({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  return (
    <>
      <label htmlFor="admin-username">Username</label>
      <input
        // Not "username": that is the id Keycloak's own page gives its field,
        // which the licensed smoke run fills right after leaving this one.
        id="admin-username"
        type="text"
        // Off for the reason the token's is: a password manager offering to
        // save the pair would write the token to disk with it.
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        autoFocus
        value={value}
        onChange={(event) => {
          onChange(event.target.value)
        }}
      />
    </>
  )
}

/**
 * The box, as a fragment rather than a wrapper element.
 *
 * The fragment is deliberate: it keeps the card's children the flat sequence
 * Phase B rendered — heading, label, input, hint, error, button — with the
 * username's label and input now ahead of the token's.
 */
function TypedCredential({
  method,
  value,
  focus,
  onChange,
}: {
  method: LoginOption
  value: string
  /** False when a username field above it takes the focus instead. */
  focus: boolean
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
        autoFocus={focus}
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
            The username is <code>SWARMCLI_CD_ADMIN_USERNAME</code>, <code>admin</code> unless set, and the token
            is the value of <code>SWARMCLI_CD_ADMIN_TOKEN</code>.{' '}
          </>
        )}
        It is kept for this tab only and is forgotten when the tab closes.
      </p>
    </>
  )
}
