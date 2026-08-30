<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# The web UI

The controller serves a browser UI from the same process and the same port as
the API. There is no second service to deploy, no port to open beyond
`--listen`, and nothing is fetched from a CDN at runtime: the whole bundle is
compiled into the binary.

It is a client of the HTTP API and nothing more. Every screen is one request
from [the endpoint table](api.md#endpoints), so anything the UI can show, `curl`
and `swarmcli-cd -o json` can show too — which is what keeps the API the
product and the UI a view of it.

## Reaching it

The UI is on by default at `/`, and `stack.yml` still does not publish the port.
Reach it the way the CLI reaches the API — from inside the swarm, or over a
tunnel:

```bash
ssh -L 8080:127.0.0.1:8080 your-manager-host
```

Then open `http://127.0.0.1:8080` and sign in with the admin token, the same
value `SWARMCLI_CD_ADMIN_TOKEN` holds. A wrong token is rejected before anybody
is signed in — the login screen probes the status endpoint with what you typed
rather than storing it and letting the first 401 undo it — so "that token was
rejected" is a message you actually get.

`--ui=false` turns it off. It does not remove the two routes; it answers them
with `404`, so the exposure of a controller that serves no UI is the same as one
that does. See [configuration § flags](configuration.md#flags).

## Building it

The published image and the release archives already contain it. Nothing below
is needed to run swarmcli-cd; it is needed to build one from source.

**A plain `go build` does not build the UI.** The bundle is produced by Vite and
embedded at compile time, and a checkout that has never run it embeds one empty
sentinel file — so every UI route answers a plain-text page saying the binary
was built without its web UI, and naming the two commands that fix it. That is
deliberate, and the reason is the promise `go install …@latest` makes: `go build
./...`, `go test ./...` and `go vet ./...` all work with no Node installed.

With Node:

```bash
npm --prefix web/ui ci
npm --prefix web/ui run build   # writes web/dist, which //go:embed compiles in
go build -o swarmcli-cd ./cmd/swarmcli-cd
```

`npm --prefix web/ui run dev` is the development loop: Vite serves the UI and
proxies `/api` and `/healthz` to a controller on `http://127.0.0.1:8080`, so
development is same-origin exactly as production is and the controller needs no
development-only setting. `lint`, `typecheck` and `test` run beside `build` in
CI; `smoke` is the Playwright run, which needs a real controller on a real swarm
and lives in its own workflow so that its flakiness never blocks the Go suite.

Two things about `web/dist` are worth knowing before they surprise you. It holds
a committed `.gitkeep`, because `//go:embed all:dist` refuses an empty
directory, and a Vite plugin puts that file back after every build, which starts
by wiping the directory. Delete it from the repository and `go build` stops
working for everyone without Node. And the image builds the UI from the
repository's own sources rather than from your `web/dist`, which `.dockerignore`
excludes, so a stale local build can never become what a published image serves.
[RELEASING.md](../RELEASING.md) carries the check that an archive really shipped
a UI.

## What the controller serves

Two public routes, both in [the endpoint table](api.md#endpoints): `GET /` for
the document, `GET /assets/{path...}` for the hashed build output it asks for.
They are unauthenticated because a browser has no credential until the login
screen it is asking for has loaded, and what they serve is the build's own
bytes.

`GET /` is also the fallback for every path no other route claimed, because
those paths belong to the router in the browser rather than to the mux — which
is what makes a bookmark and a page reload work at all. The two consequences of
that, including why a mistyped endpoint under `/api/` is still a JSON `404`, are
in [api § what an unmatched path answers](api.md#what-an-unmatched-path-answers).

The index is served `Cache-Control: no-store` and the assets
`immutable`, which is the opposite of the usual instinct and is right in both
directions: the document names the hashed filenames of the build it came from,
so a copy cached across an upgrade would ask for files that no longer exist,
while a hashed filename's bytes never change. A request for a hashed name this
build does not have is a `404` rather than the index — otherwise a missing
script would arrive as a `200` that renders nothing — and `/assets/` itself is
not browsable, since a directory listing would enumerate the whole build to a
caller who has authenticated nothing.

## The licence badge

At the foot of the navigation rail, on every screen, reading the `licence` block
of [the capability
document](api.md#discovery--get-uibootstrapjson-and-get-apiv1capabilities). It
is the operator-facing half of what the controller also writes to its log, and
it answers two questions: what state the licence is in, and what to do about it.

**It renders nothing in the OSS build.** `licence` is `null` there — no licensed
module is linked — and a chip reading "community" in a build that has no licence
to report is a control that can never say anything else. It also renders nothing
while the capability request is in flight and after one that failed, which is
what makes a controller with no capability endpoint indistinguishable from a
free one.

In the merged build it has five statuses, of which `valid` renders two ways, and
a fallback for a sixth:

| | |
|---|---|
| **licence valid** | the tier, and the expiry date or "perpetual" |
| **licence expiring** | inside fourteen days of expiry: the days left and the date, in amber. It stops reading healthy *before* the licence lapses, because the first thing a lapse takes is SSO, and SSO is what the operators who would have noticed sign in through |
| **grace period** | past a deadline and still granting: how long ago it lapsed, and the day the features actually stop |
| **licence expired** | verified, past every grace it had, granting nothing |
| **licence invalid** | it did not verify — and the remedy is a replacement licence rather than a change to this deployment, which is what the badge says so that nobody spends an afternoon on the controller's configuration |
| **no active licence** | nothing installed, *or* a managed licence installed and not activated for this swarm. Both remedies are offered, and the offline one with them: install a licence, activate a managed one, or `:license lease install <file>` on an air-gapped swarm |
| *the status itself, muted* | a status this build does not know, from a controller ahead of this bundle. Drawn rather than guessed at |

The grace rendering says "lapsed" and not "expired" on purpose. One of the two
states behind that status is a managed licence that verifies and has not expired
at all — only its activation renewal is late — and its reader, told the licence
expired, goes looking for a replacement for a key already in their hand. The day
the features stop comes from the controller rather than from arithmetic here,
because the two states' windows are not the same length.

**The node allowance sits beside the status and never inside it.** When the
issuer reports the deployment over its allowance, a second line says so and
names the date the term stops being rolled forward — which is the point of
showing it at all, a cap judged at the issuer being otherwise invisible until a
term quietly fails to renew. It never colours the chip, hides a control or
changes what the build claims to grant: it is unsigned, and a build that
switched a feature on or off on it would be one that anyone able to answer a
request could switch. See [editions § the node
allowance](editions.md#the-node-allowance).

## The service log console

On the Monitor screen, behind the **Service Container Logs** tab. It is offered
only when `capabilities.logs` says the build can answer — a controller with no
log streamer reports `false`, and a tab that reported `501` after being clicked
would teach the operator that by wasting their time.

Each row is a line number, the container's own timestamp, **which replica wrote
it**, the stream tag and the message.

The replica cell says the least it can honestly say. A replicated service's task
shows as `.3`, the way `docker service ps` spells it. A **global** service's
tasks have no slot at all, so they show the node's hostname instead; so does a
task the controller could not name. Failing that there is the truncated task id.
The full task id and the hostname are on the cell's hover, and each task keeps
one colour for as long as the console is open — the colour is always a second
cue, never the only one, so the row still reads in monochrome and in Export.

**Scrollback** is four presets: the last 100 lines, or 15 minutes, an hour, or 6
hours. Each sends a `since` *and* the tail that bounds it — see
[api § choosing a window](api.md#choosing-a-window) for why one without the
other is a control that does nothing. Changing the preset is a new read, not
more of the current one: the daemon cannot extend a stream backwards, so the
buffer is replaced rather than added to.

The console holds up to 50,000 lines and **draws at most 2,000 of them**, the
newest, saying so above the rows when it is holding more. That is a bound on the
rendering and on nothing else — the search, the task filter, Copy and Export all
work on everything held. An operator reading six hours of a service is looking
for something, and the way to find it in twenty thousand lines was never to
scroll.

A line the controller wrote rather than the container — output dropped, a line
truncated — is drawn as a `NOTICE` and is never hidden by the task filter, since
it reports on the stream rather than on one replica.

## The security posture

**The browser holds the admin token, and that token is root-equivalent on the
swarm.** An injected script in this page is not a defaced page; it is a
compromised swarm. Everything in this section follows from that one sentence,
and it is the reason the UI is deliberately small.

Every UI response carries a Content-Security-Policy with `script-src 'self'`
and **no `'unsafe-inline'`**, plus `frame-ancestors 'none'`, `base-uri 'none'`
and `form-action 'none'`. That is a constraint on what the build may emit rather
than a wish: one inline `<script>` in the bundle produces a page that does not
run and one inline `<style>` a page that renders unstyled, so a Go test greps
the embedded index and fails if it finds either. The policy names no `font-src`,
so a font inherits
`default-src 'self'` and an inlined `data:` font would be refused by the browser
that asked for it — which is why the build inlines no assets at all. Responses
also carry `X-Content-Type-Options: nosniff`, and `Referrer-Policy: no-referrer`
because a URL in this UI names one of your applications and there is no reason
for that name to reach anybody else's host.

**The token lives in `sessionStorage`, not `localStorage`.** Per-tab lifetime
with no persistence across a browser restart is the cheapest real reduction in
exposure available without giving the controller a second credential type, and
there is deliberately no "remember me". The input is a password field with
autocomplete off for the same reason: a password manager writing the swarm's
root credential to disk would undo the point of it. Signing out and any `401` —
from a read, from the sync button, from the event stream — clear it through one
path, and the login screen is rendered in place of the application rather than
navigated to, so the credential never enters a URL, a history entry or a
`Referer` header.

What that choice buys is a controller that had to learn nothing new: no cookie,
no session endpoint, no CSRF token, no `SameSite` reasoning. The browser sends
`Authorization: Bearer …` on every request exactly as the CLI does, and the
server cannot tell them apart. What it costs is the sentence this section opens
with.

The bundle ships no source maps — a source map is the application's whole source
served unauthenticated beside it, and it would be embedded in the binary either
way. The UI is same-origin with the API by construction, so nothing here uses
CORS and nothing should start. The dependency list is short on purpose: every
package in it runs in a page holding the swarm's root credential, and each one
is something this project has to keep vetting.

One thing the UI does not add is identity. Signing in with the shared admin
token makes every operator the same subject, so the `actor` on a sync event
names that one subject and not a person; see
[api § authentication](api.md#authentication). Per-user login is a licensed
capability — [single sign-on](sso.md), which replaces the token box on this
login screen with a provider's — and the paths it serves are the ones reserved
in [extensibility § reserved paths](extensibility.md#reserved-paths). Everything
in this section is about a deployment that has not enabled it, which includes
every OSS build.

## Why the port is still not published by default

Because the reason it was never published is the token, and the UI does not
change it. The controller holds root-equivalent access to the swarm behind one
shared bearer credential; on a published plaintext port that credential is on
the wire in every request, and the login form is typed into a page anybody on
the path could have rewritten before it arrived. A CSP is an instruction to the
browser from a document that was itself delivered in the clear.

The routes are registered whether or not `--ui` is on, so a deployment that
never opens a browser has exactly the surface it had before the UI existed. And
a tunnel remains the shortest way to see it — one `ssh -L`, nothing deployed.

With TLS on, publishing becomes defensible rather than reckless: the credential
is encrypted in transit, and the token stays a Docker secret rather than
anything in the stack file. `stack.yml`'s commented publish block spells out
that configuration, and [configuration § TLS](configuration.md#tls) covers the
two things enabling TLS breaks — both of which look like something else entirely
when they happen.

## See also

- [Getting started](getting-started.md) — deploys a controller and opens this
  UI in a browser.
- [HTTP API](api.md) — every endpoint the UI is a client of.
- [Configuration](configuration.md) — `--ui`, `--listen` and TLS.
- [Extensibility](extensibility.md) — the seams, and the paths reserved for a
  licensed build's own screens.
