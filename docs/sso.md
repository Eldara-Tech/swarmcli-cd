<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Single sign-on

Signing in to the web UI with an OpenID Connect provider instead of the shared
admin token. This is a **licensed** capability: it is compiled into the default
`swarmcli-cd` artefact and does nothing until a licence verifies, and it is not
in the `swarmcli-cd-oss` artefact at all. See [editions](editions.md) for which
one you are running.

Keycloak is the provider this is written and tested against, because it is
already in the infrastructure. Nothing here is Keycloak-specific except the
spellings in [§ the provider](#1-register-a-client-with-the-provider).

## What it changes, and what it does not

It replaces the **token box on the login screen** with a "Sign in with SSO"
button. That is the whole of the user-visible change, and the distinction is
worth holding on to, because two things people expect to follow do not.

**The admin token is still required.** The controller refuses to start without
one, licensed or not. It is what the CLI, every script and the container
healthcheck send — none of which can complete a browser login — and it is what
the deployment falls back to if the licence lapses. Without it, a lapse would be
either an outage nobody can sign in through or an unauthenticated API on a
process holding the docker socket. What SSO removes is the box, not the
credential.

**Every subject it authenticates is an administrator.** Per-user identity is
what SSO establishes; per-user *authorisation* — projects, and RBAC scoping an
action to one of them — is the next slice and is not here yet. A login names who
asked for a sync in the log and on the event stream; it does not yet decide what
they may sync.

Group membership is carried from the ID token into the subject and nothing reads
it yet, deliberately: it is what projects will scope against, and a mapper
configured now costs nothing and saves a second round of provider changes later.

## What you need

- The **default artefact** — `swarmcli-cd` archives, or
  `eldaratech/swarmcli-cd:<version>` / `:latest`. The `-oss` artefact does not
  contain this code.
- A **licence** installed on the swarm. It is a docker config named
  `swarmcli-license` in the swarm's raft store, installed once through the
  SwarmCLI TUI's `:license` view. The controller runs on a manager with the same
  socket mounted and reads the same config — there is no CD-specific
  installation flow, and one key covers both products.
- An **OpenID Connect provider** you can register a client with.
- A **stable external URL** for the controller. The provider redirects a browser
  back to it, so it has to be an address the operator's browser can reach and
  that you can register in advance.

## 1. Register a client with the provider

In Keycloak, create a client in the realm you will authenticate against:

- **Standard flow** enabled. This is the authorization code flow; nothing here
  uses the implicit or direct-grant ones.
- **Valid redirect URI**: the controller's external URL plus `/auth/callback`,
  and nothing wider. It must match the spelling you configure below exactly —
  the provider redirects to what was registered with it, and this controller
  serves exactly one callback path.
- **Confidential or public.** A confidential client has a secret, which you pass
  as a docker secret. A public client has none and is protected by PKCE, which
  this controller always sends.
- Optionally, a **Group Membership** mapper named `groups` with *Add to ID
  token* on. Nothing reads it today; see above.

The controller asks for the `openid`, `profile` and `email` scopes and reads
three claims from the ID token: `preferred_username`, `email` and `groups`. The
displayed name is the first of `preferred_username`, `email` and `sub` that is
present, so a realm that maps none of them still produces a usable subject
rather than an empty one.

## 2. Configure the controller

Five environment variables, and nothing else. There are no flags for any of
this: like every other credential the controller takes, these arrive as docker
secrets, and a flag would put them in `docker service inspect` output and in
`argv`.

| Variable | |
|---|---|
| `SWARMCLI_CD_OIDC_ISSUER` | the provider's issuer URL — for Keycloak, `https://host/realms/<realm>`. A trailing slash is trimmed for you, because it is compared byte for byte against the `iss` claim |
| `SWARMCLI_CD_OIDC_CLIENT_ID` | the client you registered above |
| `SWARMCLI_CD_OIDC_CLIENT_SECRET_FILE` | the client secret, read from a file — the docker-secret form |
| `SWARMCLI_CD_OIDC_CLIENT_SECRET` | the same secret given directly. The file form is preferred: a secret arrives as a file, encrypted at rest in the raft log and delivered in memory, and the string form gives that up |
| `SWARMCLI_CD_OIDC_REDIRECT_URL` | where the provider sends the browser back — the controller's external URL plus `/auth/callback`. It **must** end in that path |

Both secret forms may be omitted for a public client.

**Set some but not all of them and the controller refuses to start**, naming
what is missing:

```
SSO is half-configured: SWARMCLI_CD_OIDC_REDIRECT_URL must be set too, or none
of the SWARMCLI_CD_OIDC_* variables at all
```

An identity provider configured with one variable missing would otherwise reject
every login and look, to an operator, exactly like a wrong password. Setting
none of them is the other valid state, and is what every unlicensed deployment
and every licensed one before the provider exists looks like.

In `stack.yml`, that is one secret and four lines:

```yaml
services:
  controller:
    environment:
      - SWARMCLI_CD_ADMIN_TOKEN_FILE=/run/secrets/swarmcli-cd-token
      - SWARMCLI_CD_OIDC_ISSUER=https://sso.example.com/realms/ops
      - SWARMCLI_CD_OIDC_CLIENT_ID=swarmcli-cd
      - SWARMCLI_CD_OIDC_CLIENT_SECRET_FILE=/run/secrets/swarmcli-cd-oidc-secret
      - SWARMCLI_CD_OIDC_REDIRECT_URL=https://cd.example.com/auth/callback
    secrets:
      - swarmcli-cd-token
      - swarmcli-cd-oidc-secret

secrets:
  swarmcli-cd-oidc-secret:
    external: true
```

```bash
printf '%s' "$THE_CLIENT_SECRET" | docker secret create swarmcli-cd-oidc-secret -
```

A redirect URL reachable from a browser means the port is published, which means
[TLS](configuration.md#tls) — see [§ over plain HTTP](#over-plain-http) for what
happens if it is not.

## 3. Restart the controller

**A licence installed into a running controller does not serve SSO until the
process restarts.** The route table is built exactly once, when the HTTP mux is
built, so the login and callback routes cannot appear in a process that is
already serving. The badge on the UI changes on the next request; the routes do
not.

A build that is configured for SSO and finds no licence granting it says so
once, at startup:

```
WARN sso: configured, but no licence grants it — /auth/login, /auth/callback and
     /auth/logout are not served. Install a licence and restart the controller:
     the route table is built once, at startup.
```

So: install the licence, then `docker service update --force
swarmcli-cd_controller`, or redeploy the stack.

## 4. Check that it took

Three signals, in increasing order of how much they tell you.

**The startup route log.** A licensed controller logs `/auth/login`,
`/auth/callback` and `/auth/logout`, each on its own `WARN` line naming the
pattern and the module that registered it, because they are public routes on a
process holding the docker socket and that is worth one line each. An unlicensed
build declares no route at all and logs none of them — its route table is the
Apache-2.0 build's, string for string.

**The login screen's own document**, which needs no credential:

```bash
curl http://127.0.0.1:8080/ui/bootstrap.json
{"login":[{"id":"sso","label":"Sign in with SSO","start":"/auth/login"}]}
```

Exactly that, and nothing else, is SSO being live: a deployment that has moved to
SSO does not offer a browser a box for the shared admin token. If it still names
the token method, the licence was not read or the controller has not restarted.

**The capability document**, with the admin token:

```bash
curl -H "Authorization: Bearer $SWARMCLI_CD_ADMIN_TOKEN" \
     http://127.0.0.1:8080/api/v1/capabilities
```

`"edition": "business"` with `"sso": true` is the licence having verified. See
[api § discovery](api.md#discovery--get-uibootstrapjson-and-get-apiv1capabilities).

## Sessions

A completed login issues an opaque session held **in the controller's memory**
and named by an `HttpOnly` cookie. What follows from that is worth knowing before
it surprises somebody.

- **A session lasts twelve hours** — a working day. There is no refresh token
  and no revocation list, so its own expiry is the only thing that ends it short
  of a restart or a sign-out.
- **A restart signs everybody out.** Sessions are not on the swarm. This is the
  same restart a licence already requires, and a session that survived a restart
  would have to be a signed cookie the controller could not revoke.
- **A login has ten minutes** to come back from the provider — enough for a
  password, a second factor and a consent screen. Longer than that and the
  callback reports that the login has expired.
- **Both stores are bounded**, at 1024 logins in flight and 4096 sessions, and a
  full store refuses a new entry rather than evicting a live one. Refusing means
  somebody retries; evicting would mean signing an operator out because a
  stranger asked for a login URL.
- The cookie is `HttpOnly` and `SameSite=Lax`, and carries `Secure` when the
  redirect URL is `https`. The one endpoint that writes anything is a `POST`,
  which `Lax` withholds from cross-site requests entirely.

A browser holding a stale session cookie is not rejected for it — it falls
through to the token, which is how the CLI has always authenticated and what a
lapsed licence degrades to.

## Signing out

`GET /auth/logout` drops the session on this controller and expires the
browser's cookie. The public UI's sign-out button clears a credential the tab
holds, and an SSO session is not one — without this route an operator would stay
signed in for the rest of the twelve hours having been told they were not.

**It does not sign them out of the identity provider.** The browser keeps its
provider session, so pressing "Sign in with SSO" again returns them to the
application list without a password. On a personal machine that is convenient;
on a shared one it is not what "sign out" is usually taken to mean, so tell
operators who ask rather than letting them find out.

RP-initiated logout — a redirect to the provider's `end_session_endpoint` — is
the fix, and it is deferred rather than overlooked: it needs a post-logout
redirect URI registered with the provider, which is a sixth configuration step
and a confusing failure when it is missed.

Signing out is also a `GET`, so a link on another site can log an operator out.
The whole effect of that forgery is that they sign in again.

## Over plain HTTP

Nothing refuses to start. A controller reached over plain HTTP on a private
network is a real deployment, and refusing would overrule a decision you have
already made with your provider. The controller says what it costs instead, once
at startup:

```
WARN sso: the redirect URL is not https, so the session cookie is sent without
     the Secure attribute and a network that can see the traffic can take a
     session redirect_url=http://cd.internal:8080/auth/callback
```

The `Secure` attribute follows the redirect URL's scheme rather than being
configured separately, so there is no second setting for the two to disagree
about — and the disagreement a browser acts on is the one that silently drops
the cookie. If the controller is reachable from anything you would not hand a
session to, put [TLS](configuration.md#tls) in front of it.

## When something is wrong

| What you see | What it is |
|---|---|
| The controller refuses to start, naming a variable | SSO is half-configured. Set the named variable, or none of the five |
| `SWARMCLI_CD_OIDC_REDIRECT_URL must end in /auth/callback` | The controller serves one callback path. A mismatch would end a login on a 404 with the authorization code in the operator's browser history |
| The login screen still shows a token box | The licence was not read, or the controller has not restarted since it was installed. Check `bootstrap.json` and the startup route log |
| `/auth/login` renders the UI instead of redirecting | No route is registered, so `GET /` — the single-page app's fallback — answered it. That is exactly what the Apache-2.0 build does. With `--ui=false` it is a plain `404` |
| `403 SSO is not licensed on this controller` | The routes are on the mux from a licence that has since lapsed or expired. The token still works |
| `502 the identity provider could not be reached` | Discovery or the code exchange failed. The controller logs the issuer and the underlying error; it does not cache a failure, so a provider that comes back needs no restart |
| `400 no login is in flight for this browser` | The login cookie did not come back with the callback — a different browser or tab completed it, or something stripped the cookie |
| `400 this login has expired or has already been completed` | More than ten minutes elapsed, or the callback URL was replayed. An authorization code is redeemable once, on purpose |
| `401 the login could not be completed` | The ID token did not verify, or belonged to a different login. The controller logs which |
| `503 too many logins in flight` / `too many sessions` | A bounded store is full. It refuses rather than evicting somebody live |

Everything the flow rejects is logged with a reason and answered to the browser
without one: every value in a callback's query string is written by whoever sent
the browser there, and a response body is a thing a browser renders.

## See also

- [Editions](editions.md) — the two artefacts, and which one you are running
- [The web UI](web-ui.md) — what the browser holds when SSO is *not* configured
- [Configuration § TLS](configuration.md#tls) — serving the API over https
- [HTTP API § authentication](api.md#authentication) — the actions an authorizer
  decides, and why they are five rather than two
- [Extensibility](extensibility.md) — the seams a licensed build replaces, and
  the paths reserved for it
