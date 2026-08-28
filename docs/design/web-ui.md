# swarmcli-cd web UI — architecture, and the open/licensed split

Against `swarmcli-cd` `main` @ `e08df8a` (v1.0.0 + 5 commits), `swarmcli-be`
`main`, and `swarmcli` v1.13.0. Every claim in §3 was re-read from those trees,
not taken from issue text or from memory.

Status: **accepted.** The epic is [#168](https://github.com/Eldara-Tech/swarmcli-cd/issues/168),
and this document is its decision record — where an issue's own plan corrects
something here, the issue is the one written against the code.

---

## Contents

1. [Executive summary](#1-executive-summary)
2. [Decisions taken](#2-decisions-taken)
3. [What is true today](#3-what-is-true-today)
4. [Architecture](#4-architecture)
   - 4.1 [Where the UI lives](#41-where-the-ui-lives)
   - 4.2 [Build and embed](#42-build-and-embed)
   - 4.3 [Serving: routes, fallback, headers](#43-serving-routes-fallback-headers)
   - 4.4 [Browser authentication](#44-browser-authentication)
   - 4.5 [Live updates without EventSource](#45-live-updates-without-eventsource)
   - 4.6 [The two discovery documents](#46-the-two-discovery-documents)
   - 4.7 [Screens, and the endpoint behind each](#47-screens-and-the-endpoint-behind-each)
   - 4.8 [TLS, and the two things it breaks](#48-tls-and-the-two-things-it-breaks)
5. [The open / licensed split](#5-the-open--licensed-split)
   - 5.1 [Everything renders in the public tree](#51-everything-renders-in-the-public-tree)
   - 5.2 [The `feature` seam](#52-the-feature-seam)
   - 5.3 [Reserved paths, and what that costs](#53-reserved-paths-and-what-that-costs)
   - 5.4 [Tier packaging against the existing licence](#54-tier-packaging-against-the-existing-licence)
   - 5.5 [`swarmcli-cd-be`, and the SSO slice](#55-swarmcli-cd-be-and-the-sso-slice)
   - 5.6 [One binary, two artefacts](#56-one-binary-two-artefacts)
6. [Core changes, package by package](#6-core-changes-package-by-package)
7. [What this deliberately does not do](#7-what-this-deliberately-does-not-do)
8. [Risks and dependencies](#8-risks-and-dependencies)
9. [Decisions settled at review](#9-decisions-settled-at-review--2026-08-04)
10. [Acceptance criteria](#10-acceptance-criteria)
11. [Shape of the epic](#11-shape-of-the-epic)

---

## 1. Executive summary

The controller's HTTP API was designed UI-first in Phase 1 and the debt has
already been paid: eight routes, one per screen, with wire types in
`application` that carry sync state, health rollups, per-service detail, drift
fields, manifest diffs and release history. **A read-only Argo-CD-shaped UI
needs no new read endpoint.** What it needs is a way to be served, a way to
authenticate from a browser, and a way to find out which licensed features the
build it is talking to actually has.

Three things are genuinely new.

**Serving a browser makes the public surface real.** Today the only
unauthenticated route is `/healthz`, and `stack.yml` deliberately does not
publish port 8080 — the API is "root-equivalent access behind one shared token
over plaintext HTTP". A UI means an asset tree that must be reachable before
login, and it means operators will publish the port. So the design carries a
strict CSP, an explicit JSON-404 discipline so the SPA fallback cannot swallow
API typos, and TLS on the controller itself (§4.8) — which turns out to break
the container healthcheck and the CLI unless both are handled with it.

**`EventSource` cannot send an `Authorization` header.** The event stream is
guarded, so a browser gets the feed either through a cookie session or through
`fetch` + `ReadableStream`. We chose fetch: the SSE wire format on the server
does not change, no cookie type enters the core, and no CSRF surface is created.

**Licensed features must be discoverable, not guessed.** All UI code stays
Apache-2.0, so the free build contains screens the free build cannot populate.
A new `feature` seam — a `Slot` whose OSS default reports "nothing granted, no
licence" — lets the companion report what it grants, and a new guarded
`/api/v1/capabilities` projects that to the UI. Login methods come from an
optional interface beside `authz`, served publicly and minimally, because the
login screen has to know whether to offer a token box or an SSO button before
anyone is authenticated.

**And there is one binary, not two** (§5.6). The default `swarmcli-cd` artefact
carries the licensed code inert, lit by a licence rather than by a different
download, with a wholly Apache-2.0 build published beside it. This costs less
than it sounds: BE binaries already ship publicly through four channels under a
README calling them "a strict superset of CE — same binary name", and
`swarmcli-be`'s release workflow already builds privately and publishes outward.
What it does change is that the licence check becomes the only boundary there
is, which makes removing the `SWARMCLI_LICENSE_PUBKEY` override a prerequisite
rather than a cleanup.

The licensed side is proved end to end with **one** slice: SSO. It is the
feature that exercises every new mechanism at once — a companion `authz`
implementation, `extension.PublicRoutes` for the callback, the `feature` seam,
the public login document and a UI shell. The licence itself is already solved:
`swarmcli-be` stores it as a Docker **swarm config** (`swarmcli-license`), and
the controller runs on a manager with the socket — so one key installed once
lights up both the TUI and the controller, with no second key registry and no
new installation flow.

---

## 2. Decisions taken

Recorded in the numbering of `swarmcli-cd#1`, continuing from D6.

| # | Decision |
| --- | --- |
| **D7** | **React + TypeScript + Vite**, built to a static bundle and `go:embed`ed into the controller binary. |
| **D8** | **All UI is Apache-2.0, in `swarmcli-cd`.** The companion ships no frontend code. Licensed screens exist as shells and render only when the build reports the feature. |
| **D9** | **Browser auth is the bearer token**, held in the tab, sent on every call including the event stream, which the UI reads with `fetch` rather than `EventSource`. No cookies, no CSRF token, no CORS. |
| **D10** | **v1 is read-only parity plus the sync button.** Every screen maps to an endpoint that exists today; `POST /sync` is the only write. |
| **D11** | **CD features extend the existing `be`/`trial` tiers** in `swarmcli-be/license`. One licence, one key registry, one expiry story, read from the swarm config the TUI already installs. |
| **D12** | **Four licensed CD capabilities**: multi-swarm fleet view, SSO + projects/RBAC, audit log, Slack/webhook notifications. They become **five** flags (§5.2), because SSO and projects are separately grantable — a deployment can want single sign-on without multi-tenancy, and an authorizer that implements one and not the other must be able to say so. |
| **D13** | **`swarmcli-cd-be` is created in this epic**, proving the licensed path end to end with SSO. It is a **private build wrapper, not a second product** — see D20. |
| **D14** | **The controller gains `--tls-cert` / `--tls-key`** (§4.8). Both or neither; one alone is a startup error. |
| **D15** | **The UI is served by default**, with `--ui=false` to disable. |
| **D16** | **`swarmcli-cd-be` imports `swarmcli-be/license` directly.** No extracted module, no second verifier. |
| **D17** | **The SPDX gate extends to `*.ts`, `*.tsx` and `*.css`.** |
| **D18** | **`notify.Event.Actor` lands in this epic**, not with audit. |
| **D19** | **Vitest component tests for every screen, plus one Playwright smoke run** against the built image. |
| **D20** | **One public-facing binary** (§5.6). The default `swarmcli-cd` artefact is built from the private wrapper and carries the licensed code, inert without a licence. swarmcli-cd adopts this first; the TUI migrates later. |
| **D21** | **A wholly Apache-2.0 artefact is published alongside it**, clearly named, so "the released binary is not open source" has a rebuttal. |
| **D22** | **An unlicensed build declares no licensed routes.** `Routes()`/`PublicRoutes()` return nil without a valid licence, so a free deployment's mux is byte-for-byte today's. Installing a licence needs a restart. |
| **D23** | **HTTP/2 stays on** where TLS enables it. The drain test is extended to a table over {plaintext, TLS} and the decision stands on that evidence (§4.8). |
| **D24** | **`SWARMCLI_CD_CA_CERT`** is read by `app` and `status` — flag wins, then env — so setting `SWARMCLI_CD_SERVER` for the healthcheck does not break a `docker exec`. |
| **D25** | **`feature.Licence.Status` has five values**: `valid`, `grace`, `expired`, `invalid`, `absent`. `WrongCluster` stays folded into `invalid`. |
| **D26** | **Phase B is resequenced** (§11): foundations into B1, controller status into B2, the browser smoke run starts early, and no hidden `swarm` column is pre-reserved. |

Two things D12 explicitly does **not** cover, because they shipped Apache-2.0
in v1.0.0 and cannot be reclaimed: `syncPolicy.automated` and
`driftDetection: live`. The free line is drawn at v1.0.0's feature set or
wider, never narrower.

---

## 3. What is true today

Verified against the trees named at the top.

**The API is eight routes** (`api/api.go`, `coreRoutes`): `GET /healthz`
(public), and under `/api/v1/` — `status`, `applications`,
`applications/{app}`, `…/diff`, `…/history`, `POST …/sync`, `events`. All but
`/healthz` are guarded, with four actions: `read`, `diff`, `history`, `sync`.

**The wire types already carry a UI's worth of state.** `application.View` is
`Spec` + `Status`; `Status` has `Sync` (state, revision, summary, last result),
`Health` (state, message, service counts), an optional `Drift`, and
`[]ReleaseStatus` — each with chart, version, revision number, planned action,
sync state, health, wave, compat, per-service `ServiceStatus` (mode, running,
desired, completed, health, update state) and per-release `ReleaseDrift` down
to `FieldDrift{Field, Desired, Live}`. Enums marshal lowercase and decode
unknown names to `"unknown"`, so a newer controller never breaks an older
client.

**`/api/v1/events` is SSE, guarded, and authorised per event.** Eight event
types. Every frame carries `application`, `swarm`, `type`, `at`. Slow
subscribers are dropped, not blocked — `subscriberBuffer` is 64 and the doc is
explicit that a client that missed events re-reads the authoritative status.
The server is itself a registered notifier, which is why `notify` appends.

**There is no CORS, cookie, CSP or session code anywhere in the repo**, and no
`web/` directory. Nothing in the ecosystem has a frontend: no `package.json`,
no `.tsx`, and the only `go:embed`s are two small text templates in
`swarmcli-be`.

**Five seams** (`swarms`, `authz`, `notify`, `secrets`, `extension`), three
replacing and two appending, plus twelve backend `capability` interfaces.
`extension` lets a companion *declare* routes that the core registers behind
its own guard, with a separate `PublicRoutes()` method for the one shape that
cannot be authenticated — an SSO callback. `authz.Authorizer.Authenticate`
takes the whole `*http.Request`, so a companion can already read a cookie.
**Licensed SSO therefore needs no new core auth surface.**

**`stack.yml` does not publish the port**, and says why: root-equivalent access,
one shared token, plaintext HTTP. `--listen` defaults to `:8080` and there are
no TLS flags.

**The licence lives on the swarm.** `swarmcli-be/license/load.go` stores the key
in a Docker config named `swarmcli-license` (`SwarmConfigName`), read with
`ConfigInspectWithRaw`. Tiers are `be` and `trial`, granting an identical
seven-feature set; `TierFeatures` is documented as extendable without reissuing
keys. `license` imports only stdlib, `swarmcli/utils/log`, `swarmcli/docker` and
the moby client — all of which `swarmcli-cd` already depends on transitively.

**CI**: `ci.yml` (fmt / lint / vet+tidy / test / image) and `licence.yml` (SPDX
over `*.go` and `*.sh`), both gated on `dorny/paths-filter` outputs so
doc-only PRs keep a "skipped" status that satisfies branch protection.
`release.yml` runs release-drafter, then goreleaser and a multi-arch Docker
build in parallel. `Dockerfile` is a two-stage Go build onto alpine with no
docker binary.

---

## 4. Architecture

### 4.1 Where the UI lives

```
web/
  ui/                 the npm project — src/, index.html, vite.config.ts, package.json
  dist/               build output; gitignored except .gitkeep
  embed.go            //go:embed all:dist  →  var Assets embed.FS
  web.go              package web: Handler(fs.FS, Options) http.Handler
  web_test.go
```

`web` is a root package, not a subpackage of `api`, for two reasons. `api` stays
data-only — nothing importing it should link an embedded asset tree — and the
companion imports `api`'s neighbours freely, so keeping the embed off that path
matters. Nothing lives under `internal/`, per the standing rule.

The wiring stays where all wiring is: `controller.serve` builds the handler and
passes it to `api.New` as an `http.Handler` in `Options`. `api` never imports
`web`.

### 4.2 Build and embed

Vite builds `web/ui` into `web/dist` — `build.outDir: '../dist'` with
`emptyOutDir: true`, which Vite requires explicitly for an output directory
outside its root.

*Corrected 2026-08-04 — an earlier draft claimed the split keeps `node_modules`
"out of the path any recursive Go tooling walks". It does not.* Go's package
walker skips only `.`-prefixed, `_`-prefixed and `testdata` directories
(`cmd/go/internal/search/search.go`), and `node_modules` is none of those. So
`go build ./...`, `go vet ./...`, `gofmt -w .` and `scripts/check-spdx.sh` all
descend into it. The SPDX script will start failing locally the moment anyone
runs `npm ci`, because a dependency tree is full of unheadered `.sh` and `.css`
files. Both `check-spdx.sh` and any recursive tooling need explicit exclusions
for `./web/ui/node_modules/*` and `./web/dist/*`. The structural alternative —
naming it `web/_ui`, which Go skips outright — is worse for JS tooling and is
not recommended, but the exclusions are not optional.

**`.gitignore` has to be fixed in the same PR.** Its existing `dist/` rule is
unanchored, so it matches `web/dist` too, and git will not descend into an
excluded directory — which means `!web/dist/.gitkeep` cannot rescue the
sentinel. Verified with `git check-ignore -v web/dist/.gitkeep`, which reports
`.gitignore:7:dist/`. Anchor the existing rule to `/dist/` and add
`web/dist/*` plus `!web/dist/.gitkeep`. Without this the sentinel is never
committed and the first CI run on a machine with no Node fails with
`pattern all:dist: no matching files found`.

`//go:embed all:dist` requires the directory to be non-empty, so
`web/dist/.gitkeep` is committed — the `all:` prefix is what makes a dotfile
count — and `.gitignore` carries `web/dist/*` plus `!web/dist/.gitkeep`. A binary built
without running Vite therefore embeds nothing, and `web.Handler` answers every
UI route with a plain-text page naming `RELEASING.md`. That keeps
`go build ./...`, `go test ./...` and `go install …@latest` working with no
Node installed — which is what the getting-started guide promises today.

| Where | What is added |
| --- | --- |
| `ci.yml` | a `ui` job: `npm ci`, `npm run lint`, `npm test`, `npm run build`, gated on a new `ui` paths filter for `web/**`. The Go jobs do **not** build the UI. |
| `ci.yml` `image` job | builds the Docker image, which now includes the UI stage — that is where the two halves are proved to compose. Its paths filter gains `web/**`, or a UI change would skip the only job that builds both halves. |
| `Dockerfile` | a `node:22-alpine AS ui` stage running `npm ci && npm run build`, copied into the Go stage before `go build`. |
| `.goreleaser.yml` | `before.hooks` gain `npm --prefix web/ui ci` and `npm --prefix web/ui run build`. Hooks run once; all four targets embed the same bytes. |
| `release.yml` | the `goreleaser` job gains `actions/setup-node` — the hooks above need a Node the runner does not otherwise have, and the failure would be a release that shipped the not-built page. |
| `licence.yml` | the SPDX filter and `check-spdx.sh` extend to `*.ts`, `*.tsx` and `*.css` (D17). |
| new: `smoke.yml` | one Playwright run against the built image (D19), in its own job so its flakiness never gates the Go suite. It is what satisfies acceptance criterion 5. |
| new: `deps.yml` | an npm licence allowlist gate. A JS dependency tree is a new third-party licence obligation this repository has never had, and the existing `licence.yml` only checks our own headers. |

Dependency budget, deliberately small: `react`, `react-dom`, `react-router`,
`@tanstack/react-query`, the three `@fontsource` font packages, and dev-only
`vite`, `typescript`, `vitest`, `@testing-library/react`, `eslint`. **No
component library, no icon package and no diff library** — the components are
~15 small ones we own, the icons are fifteen vendored `<path>`s carrying
Lucide's ISC notice in the file, and the diff is already a unified text diff
that a 60-line colouriser renders. Every dependency here is one we would have to
vet and keep vetting on a process holding the Docker socket.

The fonts are the one entry that is *data* rather than code, and they came with
an obligation the rest of the budget always had and had never met. MIT, ISC and
OFL-1.1 all make retaining the copyright and permission notice a condition of
redistribution, and `//go:embed all:dist` redistributes the whole bundle inside
every released binary. `thirdPartyNotices()` in `web/ui/vite.config.ts` writes
every runtime package's notice into `dist/THIRD-PARTY-NOTICES.txt` and **fails
the build** on one it cannot find a licence file for; `web/web.go` serves it at
the root, unauthenticated, because a licence notice behind a credential has not
been provided to anybody. `check-npm-licences.sh` vets which licences may appear
there; this is what makes its `OFL-1.1` entry true rather than merely allowed.

`woff2Only()` in the same file drops the legacy `.woff` fallback from the built
stylesheet and its files from the bundle: nothing that runs React 19 lacks
woff2, so those 197KB were embedded in every binary and fetched by nobody.

### 4.3 Serving: routes, fallback, headers

Four routes are added to `api.coreRoutes`:

| Route | Guard | |
| --- | --- | --- |
| `GET /` | public | the SPA index, and the fallback for client-side routes |
| `GET /THIRD-PARTY-NOTICES.txt` | public | the bundle's retained third-party notices (§4.2), served from the root so the address does not move with a build hash |
| `GET /assets/{path...}` | public | hashed, immutable build output |
| `GET /ui/bootstrap.json` | public | login methods only — see §4.6 |
| `GET /api/v1/capabilities` | `read` | seams, features, licence summary |

They are registered **unconditionally**, even when `--ui=false` or when nothing
was embedded; only the response differs. That keeps `coreRoutes` a static list
of string literals, which is what `api_test.go`'s source-scanning
`registeredRoutes` check and `TestZeroExtensionsChangesNothing` both depend on,
and it keeps an extension from ever claiming `GET /`.

**Three of the four are public, and that quietly weakens an existing test.**

*Corrected 2026-08-04 — an earlier draft of this section said `/healthz` escapes
the check because it is registered with `mux.HandleFunc`. That is not what the
source does.* `registeredRoutes` matches **both** `Handle` and `HandleFunc`
(`api_test.go`: `sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc"`), and
`/healthz` escapes through a hand-written exemption — `var openRoutes =
map[string]bool{"GET /healthz": true}` — which the scan then *requires* to be
present, so a vanished route fails loudly. The registration form is irrelevant.

That changes what the fix is. Three more public routes would mean **three more
hand-written entries in `openRoutes`**, which is precisely the weakening this
section exists to prevent: the exemption list is written down, so it is the thing
that stops being true. So the check is **extended, in the same PR**, in two
steps: it starts classifying on the *second* argument of each registration — a
route is guarded iff that argument is a call to `guard` — and `openRoutes` is
**derived from `coreRoutes`' `Public: true` entries** rather than written. The
assertion then becomes a set equality in both directions, which also catches the
inverse mistake nothing catches today: a route declared `Public: true` while
actually registered behind the guard, which would make `Routes()` lie to the
startup log and make `TestEveryApiEndpointIsGuarded` skip a route it should be
testing.

**`GET /` matches everything unmatched, and that is a trap.** An unregistered
`GET /api/v1/typo` would fall through to the SPA and answer `200 text/html`,
which turns a client's mistake into a parse error somewhere far away — today it
is a plain 404 from the mux. The root handler therefore checks the path first:
anything under `/api/` that reaches it gets a 404, as JSON, matching the rest of
the API.

*Corrected 2026-08-04 — an earlier draft said requests with another method
"match no pattern at all and keep the mux's own 404, exactly as today". They do
not.* `ServeMux.findHandler` calls `matchingMethods` when nothing matched and
returns **405 with an `Allow:` header** whenever some other method would have
matched (`net/http/server.go`). Registering `GET /` means every path matches
under GET, so `POST /api/v1/typo` becomes `405 Allow: GET, HEAD` rather than a
404. Accept it, pin it with a test, and say so in `api.md`: the alternative —
additionally registering a methodless `/api/` pattern — would make that one case
a JSON 404 but would *lose* the informative 405 on a wrong-method request to a
real route, turning `GET …/sync` into a 404. That is a worse trade for the CLI
and for anyone scripting the API.

`GET /` also serves `HEAD`, since `GET` patterns match it. Harmless with
`http.ServeContent`, but two more methods reach the UI handler than the route
table suggests, so it gets a test rather than a discovery.

Headers, on the UI routes only:

- `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`
- `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`
- index: `Cache-Control: no-store`. Assets: `public, max-age=31536000, immutable` — safe because Vite hashes filenames.

`script-src 'self'` with no `'unsafe-inline'` is a **build constraint**, not a
wish: Vite must emit no inline script or style. A Go test greps the embedded
`index.html` for `<script>` without `src` and fails the build if one appears.
This matters more here than on an ordinary site, because §4.4 puts a
swarm-root credential in the browser.

### 4.4 Browser authentication

The login screen takes the admin token — the same one `SWARMCLI_CD_ADMIN_TOKEN`
holds and the CLI already sends. It is kept in `sessionStorage` and sent as
`Authorization: Bearer …` on every request. A 401 clears it and returns to the
login screen.

`sessionStorage` rather than `localStorage`: the token is the swarm's root
credential, and per-tab lifetime with no persistence across a browser restart is
the cheapest real reduction in exposure. There is no "remember me" in v1.

What this buys is a **zero-delta core**: no cookie type, no session endpoint, no
CSRF token, no `SameSite` reasoning, no coupling of login to TLS. What it costs
is that an XSS in the UI is a swarm-root compromise — which is why §4.3's CSP
is a hard constraint and why the dependency budget is small.

**No CORS.** The UI is same-origin with the API by construction. The Vite dev
server proxies `/api`, `/ui` and `/healthz` to a running controller, so
development needs no server-side change either. A cross-origin UI would be a
separate decision with its own threat model; nothing here anticipates one.

The licensed path grows from here, and exactly one thing moves. An SSO
authorizer sets and reads its own cookie inside `Authenticate(r *http.Request)`,
serves its login and callback through `extension.PublicRoutes`, and advertises
itself through §4.6's public document; core learns nothing about OIDC. But it
does have to learn that a request *authenticated*, because the paragraph above
is what the UI's own signed-in gate rests on — the token is in `sessionStorage`
and the UI is the thing that put it there — and a cookie the UI cannot read
takes that away. `HttpOnly` is not negotiable here, so the server is the only
side that can see both credentials and it has to say which arrived.

So §4.6's public document reports `session` when the request it arrives on
already authenticates, and `/auth/logout` joins §5.3's reserved paths so that
signing out of a cookie session has somewhere to go. No cookie type enters the
core, and no session store: it asks the authorizer a question it could already
answer.

*Corrected 2026-08-05.* This paragraph used to end at "Core learns nothing about
OIDC", which is true of the server and was read as settling the feature. It
settles half of it. SSO shipped in v1.1.0 with a login that completed and a UI
that never noticed, and pressing the button returned the browser to the login
screen for ever — [#217](https://github.com/Eldara-Tech/swarmcli-cd/issues/217).

### 4.5 Live updates without EventSource

`EventSource` supports only `withCredentials`; it cannot set a header. Rather
than give the server a second credential type, the UI reads
`GET /api/v1/events` with `fetch`, sets the bearer header, and parses the SSE
frames off the `ReadableStream` — about 40 lines, including reconnection with
capped exponential backoff. **The server's wire format does not change**, so
anything already consuming the stream — a `curl`, the future TUI view — keeps
working unchanged, and the browser is the one that adapts.

The client does not reconstruct state from events. The stream drops frames for
slow subscribers by design, so an event is a *hint*: it invalidates the query for
the application it names (or the list) and the UI refetches the authoritative
document. A 30-second background refetch covers a stream that died silently. This
is the model the API docs already prescribe — "a client that missed some re-reads
the status endpoint, which is authoritative" — implemented rather than
reinterpreted.

The stream carries no `id:` field, so there is no `Last-Event-ID` resume and none
is invented; a reconnect is a refetch.

### 4.6 The two discovery documents

The login screen must know whether to offer a token box or an SSO button
**before** anyone is authenticated. The full capability report must not be
public. So there are two documents, deliberately asymmetric.

**`GET /ui/bootstrap.json` — public, and as small as it can be.**

Under `/ui/` rather than `/api/v1/` on purpose: `api.md` states that every
`/api/v1/…` endpoint requires the admin token, and that invariant is worth more
than the tidiness of one path. A public route under the API prefix would make an
operator check each one before believing it.

```json
{ "login": [ { "id": "token", "label": "Admin token" },
             { "id": "sso",   "label": "Sign in with SSO", "start": "/auth/login" } ] }
```

Nothing else — except, for a request that *already* authenticates, who it
belongs to:

```json
{ "login": [ … ], "session": { "name": "alice" } }
```

The key is absent otherwise, which is every request a browser makes to it in a
build whose only credential is the admin token: the UI reads this through
`publicGet` and attaches nothing, so the document a free deployment serves does
not move. It is here rather than behind the token because the question it
answers — "am I already signed in?" — is asked by a tab that does not yet know,
and §4.4 has the rest of the argument.

In particular **no version string**: an unauthenticated caller does not need the
build number, and handing one out is free CVE matching. The version is in the
guarded document. Two costs come with the session that did not come with the
login list: this endpoint now answers differently per request, and a bearer sent
to it gets a valid-or-not answer. The second is an oracle `client.verify`
already offers on `/api/v1/status`, unthrottled, so it is a second door rather
than a new room — but it should be counted as a door.

It is fed by an optional interface beside `authz`, which is where "how does one
authenticate here" belongs:

```go
// LoginMethod is one way to obtain a credential this authorizer accepts.
type LoginMethod struct {
    ID    string // "token", "sso"
    Label string // shown on the login screen
    Start string // where the browser goes to begin; empty for a credential typed in
}

// LoginMethods is the optional interface an Authorizer implements to say how a
// browser may authenticate. An authorizer that does not implement it is
// presented as a token box, which is what every authorizer before this one was.
type LoginMethods interface{ LoginMethods() []LoginMethod }
```

The token authorizer implements it and returns the token method. An SSO-only
companion returns only its own, so the token box disappears — which is the point:
a deployment that has moved to SSO should not show a box for a credential it no
longer issues.

**`GET /api/v1/capabilities` — guarded with `read`.**

```json
{
  "version": "1.2.0",
  "edition": "community",
  "features": { "multi-swarm": false, "sso": false, "projects": false,
                "audit": false, "notifications": false },
  "licence": null,
  "seams": { "swarms": "local", "authz": "token",
             "notify": ["log", "api"], "secrets": "plaintext", "extension": [] }
}
```

`seams` is what `controller.serve` already logs at startup, made readable by an
operator who has the token but not the logs. `features` and `licence` come from
the new seam.

### 4.7 Screens, and the endpoint behind each

| Screen | Endpoint | New work |
| --- | --- | --- |
| Login | `GET /ui/bootstrap.json` | new (§4.6) |
| Overview — the fleet rollup across the three axes, the app-set card, and the live terminal | `GET /api/v1/applications`, `GET /api/v1/status` | none |
| Applications list — cards and table, filters on sync / health / drift, text search | `GET /api/v1/applications` | none |
| Application detail — header, and the tree: application → release → service | `GET /api/v1/applications/{app}` | none |
| Application topology — the same three levels as node cards on connector rails | same document | none |
| Monitor — the controller's own reconcile events, full height | `GET /api/v1/events` | none |
| Diagnostics — an integrity score and the risks behind it | `GET /api/v1/applications` | none |
| Drift panel, down to `field / desired / live` | same document (`status.releases[].drift`) | none |
| Diff | `GET /api/v1/applications/{app}/diff` | none |
| History — per release, per revision, with owner stamps | `GET /api/v1/applications/{app}/history` | none |
| Sync button | `POST /api/v1/applications/{app}/sync` | none |
| Controller status — app-set mode, source, revision, `stale`, `orphaned`, `pruned`, `pruneHeldBy` | `GET /api/v1/status` | none |
| Live event feed | `GET /api/v1/events` | none |
| Licence badge | `GET /api/v1/capabilities` | new (§4.6) |

The tree stops at services because that is where the API stops: `ServiceStatus`
has mode, running, desired, completed, health, update state and message, and
there is no task or container level (§7). The topology tab stops there for the
same reason, and Monitor is the *controller's* event stream and not container
output — per-service logs would need an endpoint that does not exist.

Overview and Diagnostics add no endpoint: both read documents the list and the
controller screen already hold, under the same query keys, so reaching them from
either costs no request. Nothing on either is asserted rather than derived —
Diagnostics' all-clear lines are each rendered from their own predicate, and the
engine card on the controller screen is drawn from the same `AppSetShape` its
notice uses. A console that hard-codes a green chip has told an operator the one
thing they cannot check.

**One fold, not one per screen** (`api/severity.ts`). The list's dot, the
topology node and the integrity score are the same question asked three times,
and written three times they disagreed on `unknown` — the *zero* member of every
enum in `application/enum.go`, and therefore what an application the controller
has accepted and not yet reconciled actually reports. Two of the three read it
as an all-clear. It is its own severity: excluded from the clear count, drawn
neutral, never green.

The `swarm` column exists in the table from day one, hidden while
`features["multi-swarm"]` is false. Every event already carries `swarm`, and
`destination.swarm` is already on every spec — reserving the column costs one
conditional and means a multi-swarm build needs no list-view change at all.

### 4.8 TLS, and the two things it breaks

Per D14 the controller takes `--tls-cert` and `--tls-key`. Both or neither: one
alone is a startup error, because the failure it otherwise produces is a
listener that came up in plaintext when the operator believed it had not.
`runUntilStopped` calls `ListenAndServeTLS` instead of `ListenAndServe`, and
that is the whole of the server change.

Enabling it breaks two things that are not obvious from the flag, and both fail
in ways that look like something else.

**The container healthcheck restart-loops.** `stack.yml` runs
`/swarmcli-cd healthcheck`, which probes `client.DefaultServer` —
`http://127.0.0.1:8080`, hard-coded. Against a TLS listener that probe fails,
so Swarm marks the task unhealthy and restarts it, forever, with the controller
itself working perfectly. Nothing in the logs says "TLS": the operator sees a
task that will not stay up.

The resolution keeps the flag self-contained rather than making the healthcheck
learn the controller's configuration, which it cannot — it is a separate
process invocation. The probe **derives the scheme**: given an `https://` URL
whose host is loopback, it skips certificate verification. That is narrow and
defensible on its own terms — `/healthz` discloses nothing, the connection never
leaves the host, and the probe is asserting liveness, not identity. A
non-loopback `https://` verifies normally. `stack.yml` sets
`SWARMCLI_CD_SERVER=https://127.0.0.1:8080` in the TLS example, and that is the
only thing the deployment has to remember.

**The CLI stops working.** `DefaultServer` is plaintext and `--server
https://…` then meets whatever certificate the operator installed. A cert from
a real CA just works. A self-signed one does not, and the client has no way to
be told about it — so `client.Options` gains a `CACert` path, surfaced as
`--ca-cert` on `app` and `status`. **Not `--insecure`**: a flag that disables
verification on the client that reads diffs and triggers syncs is a worse thing
to add than one extra argument, and the healthcheck's loopback rule above is
already the narrow exception that covers the case people would reach for it for.

**Certificate reload is out of scope for v1.** The pair is read once at startup,
so a renewed certificate needs a controller restart. Stated rather than
discovered, and filed as a follow-up: the fix is a `tls.Config.GetCertificate`
that re-reads on handshake, which is small but is not what this epic is about.

#### Three more, found while planning #171 and verified against the Go source

**TLS turns HTTP/2 on.** `Server.ServeTLS` calls `setupHTTP2_ServeTLS`, so a
browser negotiates h2 against the API and **the SSE stream runs over it**.
Graceful shutdown of h2 connections goes through the bundled http2 server's
GOAWAY hook rather than `closeIdleConns`, and an h2 connection is idle only when
it has no open streams — which is exactly what `srv.Drain` closes. It should
compose, but it is not what today's drain test exercises, so the TLS variant of
`TestTheListenerIsDrainedBeforeTheReconcilerStops` is the proof. If it does not
hold, restricting `Server.Protocols` to HTTP/1.1 is the escape hatch — and it
must be a decision, not a default. **Settled: D23 (§9.7) — h2 stays on**, and
the drain test's TLS row passes, so `Protocols` is left alone. The companion
check is `TestEventStreamDeliversWhatTheNotifierIsGiven`, now a table over
HTTP/1.1 and h2 over TLS, both asserting the protocol they claim to test.

**`SWARMCLI_CD_SERVER` is shared by all four commands.** `resolveServer` serves
`app`, `status` and `healthcheck` alike, so the moment `stack.yml` sets it to
`https://127.0.0.1:8080` to keep the healthcheck alive, a
`docker exec … swarmcli-cd app list` inside the controller — the use case
`client.DefaultServer`'s own doc comment names — starts failing with a
certificate error, because `app` verifies and the certificate is self-signed.
The design's own resolution reintroduces a failure through another door.
**Settled: D24 (§9.8) — `SWARMCLI_CD_CA_CERT`**, read by `app` and `status`,
flag then environment then unset, exactly as `resolveServer` resolves the
server. `stack.yml`'s TLS block sets it beside `SWARMCLI_CD_SERVER`.

**`client.message` swallows the one sentence that explains the restart loop.**
Go's server answers a plaintext request to a TLS listener with `400` and a body
reading *"Client sent an HTTP request to an HTTPS server."* — but `message`
falls back to the status line for any non-JSON body, deliberately, because a
proxy's HTML error page is the common case. So the operator sees
`400 bad request` and nothing else, which in `docker inspect` output is all they
get. This is the concrete mechanism behind "nothing in the logs says TLS". The
fix is a targeted hint on the healthcheck's error path, not a widening of
`message`.

And one that is only good news: **`ReadHeaderTimeout: 10s` silently becomes the
TLS handshake timeout**, since `Server.tlsHandshakeTimeout` returns the minimum
positive of `ReadHeaderTimeout`, `ReadTimeout` and `WriteTimeout`. It needs a
clause on that field's comment so a later tidy-up does not unbound the
handshake.

---

## 5. The open / licensed split

### 5.1 Everything renders in the public tree

Per D8 there is one UI, Apache-2.0, in `swarmcli-cd`. The companion contributes
no HTML, no JavaScript and no build step. Licensed features change what the UI
*renders*, never where the rendering code lives.

The honest consequence, stated rather than discovered: **the public tree
contains the shapes of licensed features.** A "Projects" nav item, an audit
table's columns, a swarm selector. That is the same disclosure the `notify` seam
already makes by being Slack-shaped, and it is the price of not maintaining a
second frontend build in a private repository. What stays licensed is every
implementation: the OIDC exchange, the policy engine, the audit store, the
multi-swarm registry.

For v1 (D10) only two of those shells actually ship — the SSO login button and
the licence badge. Projects, audit and notification screens are designed here
and built when their features are.

### 5.2 The `feature` seam

A sixth seam. It clears the bar `docs/extensibility.md` sets — a behaviour that
genuinely differs between editions and cannot be expressed by an existing seam.
`authz` is about deciding a request, `notify` about an event; neither can answer
"what does this build grant".

```go
// Package feature reports what a build grants, for display. It is never an
// enforcement point: a licensed implementation refuses from inside itself, and
// nothing in this repository reads a Set to decide whether to do something.
package feature

type Name string

const (
    MultiSwarm    Name = "multi-swarm"
    SSO           Name = "sso"
    Projects      Name = "projects"
    Audit         Name = "audit"
    Notifications Name = "notifications"
)

type Set map[Name]bool

type Licence struct {
    Tier      string     // "be", "trial"
    Status    Status     // valid | grace | expired | invalid | absent (D25)
    ExpiresAt *time.Time // nil when perpetual or absent
}

type Report struct {
    Edition  string   // "community", "business"
    Features Set
    Licence  *Licence // nil in a build with no licensed module
}

type Reporter interface {
    // Report is called per request, not cached: an entitlement that lapses
    // must stop being reported without a restart.
    Report(ctx context.Context) Report
}

func Register(name string, r Reporter) // from an init()
func Get() Reporter
func Active() string
```

A `Slot`, not a `List`: there is exactly one answer to "what does this build
grant", and OR-ing two modules' entitlement sets would let one module turn on
another's feature. The OSS default registers from `feature`'s own `init()` and
reports `{Edition: "community"}` — everything false, no licence — so the seam
never hands a consumer a nil to call, and the ordering argument in `seam`'s doc
holds without the `swarms/local` exception.

**The doc comment matters as much as the type.** The `feature` seam is a
*report*. Someone will eventually try to gate on it; the comment, and a test
asserting nothing outside `api` reads `feature.Get`, are what stop that.

### 5.3 Reserved paths, and what that costs

Because the UI is public and the data is not, the core must **specify** the
paths and shapes the companion serves, so the public UI can be written against
them:

| Path | Public | Serves |
| --- | --- | --- |
| `GET /auth/login`, `GET /auth/callback` | yes | SSO start and callback |
| `GET /auth/logout` | yes | where a session ends — see below |
| `GET /api/v1/licence` | no | the full licence record, beyond the badge |
| `GET /api/v1/projects` | no | project list and membership |
| `GET /api/v1/audit` | no | the audit log, paged |
| `GET /api/v1/notifications` | no | configured destinations |

They are documented in `docs/extensibility.md` as **reserved**, and reserved
means documented — nothing enforces it, and nothing may. **A reserved path must
never be added to `coreRoutes`**: the collision check refuses an extension
pattern that equals a core route, so listing `/api/v1/projects` there would make
the core reject the one module that is supposed to serve it. The contract is a
document the companion is written against and the public UI is written against,
which is exactly as strong as the seam contracts already are.

Reserving them now is cheap and reserving them later is not. `extension`'s
collision check is exact-string, so the day a companion ships `/api/v1/audit-log`
that name is frozen — and the public UI would have to be changed to match a
private module's spelling.

Only the first two are needed for v1.

### 5.4 Tier packaging against the existing licence

Per D11, CD features become entries in `swarmcli-be/license`:

```go
// swarmcli-be/license/scheme.go
const (
    FeatureCDMultiSwarm    = "cd-multi-swarm"
    FeatureCDSSO           = "cd-sso"
    FeatureCDProjects      = "cd-projects"
    FeatureCDAudit         = "cd-audit"
    FeatureCDNotifications = "cd-notifications"
)
```

added to both `TierFeatures` rows. The `cd-` prefix is not decoration: the file
states that feature ids match the TUI's `view.RegisterGatedAction` names, so an
unprefixed `notifications` would collide with that vocabulary. The mapping from
`cd-sso` to `feature.SSO` lives in `swarmcli-cd-be`; the public tree never sees
a `cd-` name.

`TierFeatures` is documented as extendable without reissuing keys, so **every
outstanding `be` and `trial` licence gains the CD features the day the map
changes.** That is the intended behaviour under D11 and it is worth being
explicit about, because it is also irreversible in the field.

The delivery mechanism is already solved and this is the part worth not
re-deriving: the licence is a Docker config named `swarmcli-license` on the
swarm, installed by the TUI's `:license` view. The controller runs on a manager
with the socket mounted, so it reads the same config. **One key, installed once,
lights up the TUI and the controller.** No CD-specific installation flow, no
second key registry, no expiry story to keep in sync.

Per-repo change: adding the constants is a `swarmcli-be` PR with A/B/C labels,
never a direct push to main.

### 5.5 `swarmcli-cd-be`, and the SSO slice

```
github.com/Eldara-Tech/swarmcli-cd-be     private
  go.mod        require swarmcli-cd, swarmcli-be   (Go 1.26)
  authz/        OIDC authorizer + LoginMethods; extension.PublicRoutes for /auth/*
  licence/      reads the swarm config, verifies via swarmcli-be/license,
                maps cd-* → feature.Name, registers the feature.Reporter
  cmd/swarmcli-cd/main.go      blank imports + controller.Main()
```

The command directory is `cmd/swarmcli-cd`, not `…-be`: per D20 what this
wrapper builds *is* the published `swarmcli-cd`, so the binary name, the archive
names and the image tag are the public ones. The repository is private; the
product it emits is not a separate one.

`main.go` is the only file that differs between the builds, exactly as
`docs/extensibility.md` specifies, and routes add no line to it — `authz`
registers its own through `extension` from the same `init()`.

The SSO slice is chosen because it is the only single feature that exercises
every new mechanism: a companion `authz.Authorizer` (replacing a `Slot`), the
new `authz.LoginMethods` optional interface, `extension.PublicRoutes` for the
callback, the `feature` seam, the public bootstrap document, the guarded
capabilities document, and a UI shell that changes what the login screen offers.
Multi-swarm would exercise one seam; audit would need storage the controller
does not have; notifications would exercise a seam that already works.

Keycloak is the first provider, since it is already in the infrastructure.

### 5.6 One binary, two artefacts

Per D20 there is **one public-facing `swarmcli-cd`**, built from the wrapper
above and carrying the licensed code inert. There is no `swarmcli-cd-be`
download, no second image, no second Homebrew formula.

**The secrecy argument was already spent, which is what makes this cheap.** BE
binaries ship today through the Homebrew tap, the Scoop bucket,
`eldaratech/swarmcli-be` on Docker Hub and public releases on
`swarmcli-be-releases`, whose README calls the product "a strict superset of CE
— same binary name". Anyone who wants the proprietary code can already download
it and patch the check out. Merging changes who holds it by default, not whether
it can be held.

**The pipeline was already spent too.** `swarmcli-be/.github/workflows/release.yml`
already builds privately and publishes outward to four public destinations under
`RELEASE_TOKEN`. This is consolidating two pipelines into one, not inventing one.

**The constraint that fixes the shape is Go's, not policy.** The public module
may never `require` the private one: `go install …@latest`, a contributor's
`go build ./...` and every fork would fail on an unreachable module. So the
public source stays clean and complete, and the *release* is built from the
private wrapper and published under the public name — exactly what BE does now.

Two artefacts are published from one tag (D21):

| Artefact | Built from | Contains |
| --- | --- | --- |
| `swarmcli-cd` — archives, `eldaratech/swarmcli-cd` | the private wrapper | core + licensed code, inert without a licence |
| `swarmcli-cd-oss` — archives, `eldaratech/swarmcli-cd:<v>-oss` | `cmd/swarmcli-cd` in the public repo | core only, wholly Apache-2.0 |

The second exists because without it "the released binary is not open source" is
a fair criticism with no rebuttal, and because distro packagers, air-gapped
compliance reviews and contributors verifying what they build all have a real
need for it. It costs one goreleaser target and one image tag.

**An unlicensed build serves exactly what today's build serves** (D22). The
licensed module's `Routes()` and `PublicRoutes()` return nil when there is no
valid licence, so `/auth/login` and `/auth/callback` never reach the mux, and
the public-route `WARN` never fires on a free install. That last part is not
cosmetic: a warning that appears on every deployment of a free product is how an
operator learns to ignore the warning that matters — which is the exact argument
`api.Routes` already makes for not warning about `/healthz`.

The cost is stated rather than hidden: `extension` is read once, when the mux is
built, so **a licence installed into a running controller does not serve SSO
until it restarts.** That is the same "registration is over" rule the seam
already documents, and the alternative — a hot-swappable mux — is what
`docs/extensibility.md` argues against on its own terms.

This also makes `feature.Reporter` unconditional: the licensed reporter is
always linked, and reports `community` with everything false when no licence
verifies. "Is the companion present" stops being a question, and only "is there
a licence" remains.

**Two standing rules this model depends on.**

*Nothing AGPL may ever be linked.* `swarmcli-rbac-proxy` is AGPL-3.0, and BE
today neither imports it nor carries it in `go.mod` — it is reached over the
network, which is what keeps the combination lawful. Linking an AGPL package
into a binary that also carries proprietary code is not awkward, it is a licence
violation, and it would end this model. Multi-swarm reaches rbac-proxy as an
HTTP client and must stay that way.

*The licence gate must be real before the merge, not beside it.* In a
two-artefact world the boundary is possession of the code; here the boundary is
the check. `SWARMCLI_ENV=dev` + `SWARMCLI_LICENSE_PUBKEY` currently replaces the
verification key in release builds — bad today, and in this model it hands the
whole product away with two environment variables. `swarmcli-be#273` and
`swarmcli-agent#83` are hard prerequisites (§8).

**Scope.** swarmcli-cd only, for now. It has no installed base, no second brew
formula and no migration to run, so the model gets proved on the product being
built rather than on a shipped one. The TUI migrates afterwards, with a
deprecation window publishing `swarmcli-be` artefacts as aliases; that is its own
plan and not this epic.

---

## 6. Core changes, package by package

| Package | Change |
| --- | --- |
| `web` (new) | `embed.go`, `Handler(fs.FS, Options) http.Handler`, the JSON-404 discipline for `/api/`, security headers, cache policy, the not-built fallback page. |
| `feature` (new) | The seam of §5.2, its `community` default, `Register`/`Get`/`Active`. |
| `authz` | `LoginMethod` struct and the `LoginMethods` optional interface; `token` implements it. No change to `Authorizer`. |
| `api` | `Options` gains `UI http.Handler` and `Version string`. Four entries in `coreRoutes`; four `mux.Handle` lines. Two new handlers: `bootstrap` (public) and `capabilities` (guarded, `ActionRead`). |
| `controller` | A `--ui` flag (default on, D15) and `--tls-cert` / `--tls-key` (D14, §4.8), validated as a pair in `options.validate`. Build `web.Handler`, pass it and `version` into `api.New`. `runUntilStopped` picks `ListenAndServeTLS` when the pair is set. Log the `feature` seam beside the other five. |
| `controller` (healthcheck) | The probe derives its scheme: loopback `https://` skips verification, anything else verifies (§4.8). Without this, enabling TLS restart-loops the task. |
| `client` | `Options` gains `CACert`, surfaced as `--ca-cert` on `app` and `status`. `DefaultServer` is unchanged — plaintext loopback stays the default because the default deployment is unchanged. |
| `notify` | `Event` gains `Actor string` — additive, and the thing an audit log will need. Filled by the API's sync handler from the authenticated subject, empty for controller-initiated events. On the SSE wire as `actor,omitempty`: an absent actor is a genuine absence — the controller acted on its own — which puts it with `revision` and `message` rather than with `swarm`. |
| `docs` | `extensibility.md`: the sixth seam, the optional interface, the reserved paths, and that a companion returning **zero** routes is legitimate — it is how entitlement gating reaches the mux (D22). `api.md`: the four routes. New `docs/web-ui.md`: building, serving, the security posture, why the port is still not published by default. New `docs/editions.md`: the two artefacts of §5.6, what each contains, and how a licence changes one of them. |
| `stack.yml` | The publish-the-port block gains the UI's context, and a commented TLS example carrying the certificate secrets, the two flags and `SWARMCLI_CD_SERVER=https://127.0.0.1:8080` for the healthcheck (§4.8). |

`notify.Event.Actor` is the one item here with no v1 consumer beyond the event
feed. It is included now because it is additive, because the UI is what makes
"who pressed sync" a question anyone asks, and because adding it later means
touching every notifier at a point when a companion implements one.

---

## 7. What this deliberately does not do

Each of these is a real Argo CD screen and each is out of v1 under D10.

- **Rollback.** Swarm's `PreviousSpec` and the chart engine's revision history
  both support it; there is no endpoint, and adding one means a new write route
  and a new `authz.Action` frozen the day it ships.
- **Sync options** — prune, dry-run, per-release selection. `POST /sync` takes
  no body today.
- **Service logs and task/container detail.** Needs new backend capabilities and
  reopens the disclosure question that `ActionDiff` was split out for.
- **Rendered manifest tab.** The diff endpoint carries a diff, not a manifest.
- **Editing applications.** The API is read-only in both tiers by design: the
  app set is a Docker config or a git commit.
- **Multi-swarm implementation.** Deprioritised in favour of this work; the UI
  reserves the column and the capability.
- **Audit, projects, notification screens.** Designed here, built with their
  features.
- **Cross-origin UI, cookie sessions, CSRF tokens.** Not needed by D9; each
  would be its own decision.

---

## 8. Risks and dependencies

**The token is swarm-root, and it now lives in a browser.** Mitigated by CSP
with no `'unsafe-inline'`, `sessionStorage` rather than `localStorage`, a small
audited dependency tree, and a test that fails the build on an inline script.
Not eliminated. This is the single largest new exposure in the design and the
`/e-cso-cd` audit should run against the epic before it merges.

**Publishing the port becomes normal.** `stack.yml` currently argues against it
and a UI argues for it. D14 gives the file something to argue *for*, but the
documentation still has to change from "do not publish this" to a defensible
published configuration — TLS on, port published, token in a secret — or the
first thing every operator does will be the thing the file warns about.

**npm is a new supply chain** on a process holding the Docker socket. Mitigated
by the dependency budget, a licence allowlist gate, and lockfile-only installs
(`npm ci`). Dependabot or Renovate should cover `web/ui/package-lock.json` from
the first commit.

**`SWARMCLI_ENV=dev` + `SWARMCLI_LICENSE_PUBKEY` replaces the verification key**
in release builds of the TUI and the agent-manager, and `swarmcli-cd-be`
inherits it by reusing `swarmcli-be/license` (D16). Under D20 this stops being a
bug and becomes the whole boundary: with one artefact carrying the licensed
code, the check *is* the product's paywall, and two environment variables
disable it. `swarmcli-be#273` and `swarmcli-agent#83` compile the override out —
they are **hard prerequisites for the merged artefact**, not parallel work.
Until they land, the licensed build must not be published.

**The released binary stops being wholly Apache-2.0.** That is a positioning
cost with a real precedent — Elastic took sustained criticism for exactly this
change. D21's OSS artefact is the rebuttal, and it only works if it is published
from the first release rather than added after somebody complains.

**Nothing AGPL may be linked, ever** (§5.6). `swarmcli-rbac-proxy` is AGPL-3.0;
BE neither imports it nor lists it in `go.mod` today, and multi-swarm must keep
reaching it as an HTTP client. A licence-scanning step in the release job is the
cheap way to keep this true rather than remembered.

**Depending on `swarmcli-be` couples two private modules.** The `license`
package's own imports are light (stdlib, `swarmcli/utils/log`,
`swarmcli/docker`, moby client), but the module's `go.mod` requirements enter
MVS and the two private modules' release cadences couple. Accepted knowingly as
D16; the alternatives are recorded in §9.3.

**CI ordering.** The `image` job is the only place the Node and Go halves are
proved to compose; a Go-only change that breaks the embed would otherwise pass.
The paths filter must not exclude it when `web/**` changes.

**The PAT cannot dispatch workflows or re-run failed jobs.** Releases go through
the documented tag-push fallback; a red CI job needs the user to re-run it from
the Actions UI.

---

## 9. Decisions settled at review — 2026-08-04

Items 1–6 are closed and are D14–D19 in §2; the reasoning is kept so it is not
re-derived. Items 7 and 8 were **opened** by planning A3 (§4.8) and are settled
below, in the second round.

1. **TLS: add `--tls-cert` / `--tls-key`** (D14). The alternative was a
   documented reverse proxy, which is what a real deployment should still do —
   but the first-run path to seeing the UI would then run through deploying a
   second service, and the fallback is a swarm-root bearer token over plaintext.
   The flags are ~30 lines. What they cost is owning a TLS surface: the
   consequences are §4.8, and certificate reload is explicitly deferred.
2. **UI served by default**, `--ui=false` to disable (D15). The routes register
   either way, so the exposure delta for an unchanged deployment is nil — the
   stack file still does not publish the port — and the alternative makes "I
   deployed it and there is no UI" a support question.
3. **`swarmcli-cd-be` imports `swarmcli-be/license`** (D16). One `require`
   line. Extracting a shared module now would move work to the front of the
   epic for a second consumer that does not exist; reimplementing the verifier
   would put two copies of a security-critical parser under an obligation to
   agree about schema versions, expiry and tier names forever. The coupling is
   a named risk in §8, not a hidden one.
4. **SPDX extends to TypeScript** (D17). Existing practice, and an exception is
   a thing to remember forever.
5. **`notify.Event.Actor` lands now** (D18), per §6.
6. **Vitest components plus one Playwright smoke** (D19). Component tests
   against a faked API cover every screen; one browser run covers the path
   nothing else does — real token, real stream, real swarm. It goes in its own
   workflow so its flakiness never gates the Go suite.

**Settled in a second round, after the planning agents reported:**

7. **HTTP/2 stays on** (D23). It multiplexes, so one connection serves the event
   stream *and* every API call — which matters for a UI that holds a stream open
   against a browser's six-per-origin limit. The risk is real but bounded: h2
   graceful shutdown goes through the GOAWAY hook rather than `closeIdleConns`,
   and an h2 connection is idle only when it has no open streams — exactly what
   `srv.Drain` closes. So the drain test becomes a table over {plaintext, TLS}
   and the decision stands on that evidence rather than on the reading. If it
   fails, `Server.Protocols` restricted to HTTP/1.1 is one line.
8. **`SWARMCLI_CD_CA_CERT`** (D24). Flag wins, then env, exactly as
   `resolveServer` already resolves the server — so the stack file sets it beside
   `SWARMCLI_CD_SERVER` and a `docker exec … swarmcli-cd app list` keeps working.
   The alternative was documenting `--ca-cert /run/secrets/…`, which works
   because a self-signed leaf is its own authority; it was rejected because an
   operator who does not read the comment meets a certificate error in the one
   place the docs call the normal way to reach the API.
9. **`feature.Licence.Status` gains `grace`** (D25), for five values.
   `GracePeriod` grants features but is not `Valid`, and collapsing it to
   `valid` means the badge cannot say "expired four days ago, stops working
   tomorrow" — the single most useful thing a badge can say. `WrongCluster`
   stays folded into `invalid`: its remedy does differ, but a sixth value the
   Apache-2.0 build can never produce is a worse cost than the ambiguity, and
   the companion can say so in prose.
10. **All four Phase B resequencing changes are applied** (D26); see §11.

---

## 10. Acceptance criteria

The epic is done when all of these hold.

1. `go build ./...`, `go test ./...` and `go vet ./...` pass with **no Node
   installed**, and the resulting binary serves the not-built page on `GET /`.
2. `docker build .` produces an image whose `GET /` serves the real UI, and the
   existing image checks — no docker binary, both versions stamped, refuses to
   start with no admin token — still pass.
3. A Go test asserts the embedded `index.html` contains no inline `<script>` or
   `<style>`, and that every UI response carries the CSP and `nosniff` headers.
4. `GET /api/v1/typo` returns a JSON 404, not the SPA.
5. The Playwright smoke run, against a real swarm: log in with the admin token,
   see an application go from out-of-sync to synced **without a page reload**,
   read its diff, read its history, press sync and watch the event arrive.
6. `GET /ui/bootstrap.json` on the free build offers exactly one login method
   and discloses no version.
7. `GET /api/v1/capabilities` on the free build reports `community`, every
   feature false and `licence: null`.
8. **The same published image**, with a valid `be` licence in the swarm config,
   reports `business`, `sso: true`, offers the SSO login method, completes a
   Keycloak login and reaches the application list. Remove the licence config,
   restart, and it is criterion 7 again — same bytes, both times.
9. With no licence or an expired one, that image declares **no** `/auth/*`
   routes (D22): the startup route log is identical to the OSS artefact's, and
   no public-route `WARN` is emitted.

   *Corrected 2026-08-04.* An earlier draft also required `GET /auth/login` to
   be a **404**. Once A1 registers `GET /` as the SPA fallback, every
   unregistered GET path answers `200` with the index — so that clause was
   unsatisfiable the moment the UI shipped, and the conflict was decided in A1
   and would have been paid in Phase D. The check is the route log and the
   absence of the `WARN`; the alternative, teaching the fallback a set of
   client-route prefixes, couples the Go handler to the React router and is not
   worth it.
10. The `swarmcli-cd-oss` artefact contains no licensed code — a symbol or
    module scan of the binary finds nothing from the private module — and passes
    criteria 1–7 on its own.
11. Startup logs name the sixth seam, and every public route, unchanged in form
    from today's `seams` and `routes` lines.
12. `api_test.go` asserts the unguarded route set equals `coreRoutes`' public
    entries, and fails when a route is registered unguarded without being
    declared public (§4.3).
13. `--tls-cert` without `--tls-key` refuses to start, naming the missing half.
14. With both set, the stack file's healthcheck **stays healthy across three
    intervals** — the criterion that would have caught the restart loop of
    §4.8 — and `swarmcli-cd app list --server https://… --ca-cert …` answers.

Criterion 8 is the one that proves D8 and D20 together: the same bytes — of the
UI and now of the whole binary — behave differently purely because a licence
config appeared on the swarm.

---

## 11. Shape of the epic

Planned properly after this document is accepted; sketched here only so the
size is visible.

- **Phase A — serve something.** `web` package, embed, stub fallback, routes,
  headers, JSON-404, `--ui` flag, TLS flags with the healthcheck and `--ca-cert`
  consequences (§4.8), CI/Dockerfile/goreleaser wiring. Ends with an empty React
  app served by the controller and proved in the image job.
- **Phase B — the read-only UI.** Login, list, detail with the tree, drift, diff,
  history, status, event stream over `fetch`, sync button.
- **Phase C — discovery.** `feature` seam, `authz.LoginMethods`, the two
  documents, the capability-driven shells, reserved paths documented.
- **Phase D — the licensed slice.** `swarmcli-cd-be` created as the build
  wrapper; `cd-*` features added to `swarmcli-be/license`; OIDC authorizer,
  callback, licence reader, feature reporter; the zero-routes-when-unlicensed
  rule (D22); criteria 8 and 9 met.
- **Phase E — one binary, two artefacts.** The release moves to the wrapper and
  publishes both `swarmcli-cd` and `swarmcli-cd-oss` (§5.6); `docs/editions.md`;
  the AGPL scan in the release job. Gated on the licence-gate prerequisites.
- **Phase F — hardening and docs.** `/e-cso-cd` audit, `docs/web-ui.md`,
  `stack.yml` published-port and TLS guidance, npm licence gate, dependency
  automation.

Phases A–C are `swarmcli-cd` only. Phase D touches three repos, and Phases D–E
are gated on `swarmcli-be#273` / `swarmcli-agent#83` — the merged artefact must
not be published while two environment variables can replace the verification
key.
