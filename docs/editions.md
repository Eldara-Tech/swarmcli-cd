<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Editions: the two artefacts, and which one you have

Every release of swarmcli-cd publishes **two** artefacts from one tag. They are
built from different trees, they are named differently, and one of them can be
unlocked by a licence. This page is what each of them is.

| | Built from | Contains | Licence |
|---|---|---|---|
| `swarmcli-cd` archives, `eldaratech/swarmcli-cd:<version>`, `:latest` | a private build wrapper around this repository | this repository, plus licensed code that is **inert** without a licence | this repository's code is Apache-2.0; the licensed code is proprietary |
| `swarmcli-cd-oss` archives, `eldaratech/swarmcli-cd:<version>-oss` | `cmd/swarmcli-cd` in this repository, nothing else | this repository, and nothing else | wholly Apache-2.0 |

This starts with `v1.1.0`. `v1.0.0` and the release candidates before it
published the plain names only, and a single `checksums.txt` — there was one set
of artefacts to verify — so those releases have no `-oss` archive and no `-oss`
image to download.

The command inside both archives is `swarmcli-cd`. Every invocation in these
docs, `stack.yml`'s entrypoint and the container healthcheck are identical for
the two, deliberately: the difference between them is what the build contains,
not how it is driven.

## Why there are two

The default artefact carries the licensed code because a single binary is the
only arrangement in which "install a licence" is not "download a different
product". Nothing is hidden by that — the code is compiled in and does nothing
until a licence verifies.

But a released binary that contains proprietary code cannot honestly be called
open source, and a project whose only download is that binary has no answer when
somebody says so. `swarmcli-cd-oss` is the answer, and it only works if it ships
**from the first merged release**, not after somebody complains. It is also the
artefact a distribution packager, an air-gapped compliance review and a
contributor verifying what they built actually need.

The OSS build is not a subset with features removed. It is this repository,
which is the whole product: reconcile, diff, drift, health, prune, the HTTP API,
the CLI and the web UI. `syncPolicy.automated` and `driftDetection: live` shipped
Apache-2.0 in v1.0.0 and are not reclaimable; the free line is drawn there or
wider, never narrower.

## Which one am I running

`swarmcli-cd version` reports the release and the chart engine and says nothing
about this — one tag produces both artefacts, so the version cannot tell them
apart. Two things can.

**The startup log.** Every controller logs a `seams` line naming the
implementation in force behind each seam. `feature` is the one to read:

```
level=INFO msg=seams swarms=local authz=token secrets=plaintext feature=community
```

`feature=community` is the OSS build. The merged build always links its licensed
reporter, so that field names the reporter **whether or not a licence verified**
— which is exactly what tells "the module is missing" apart from "the licence is
missing", and neither the edition nor the absence of features answers that
alone.

**The capability document**, for an operator with the admin token and not the
logs:

```bash
curl -H "Authorization: Bearer $SWARMCLI_CD_ADMIN_TOKEN" \
     http://127.0.0.1:8080/api/v1/capabilities
```

`seams.feature` is that same field. `edition` reads `community` on the OSS build
*and* on a merged build with no valid licence, which is why it is the weaker of
the two signals. `seams` is reported only to a caller the authorizer lets read
about the controller itself; the admin token always does. See
[the API's authentication section](api.md#authentication).

## What a licence changes, and when

A licence is installed as a Docker config or secret and read at startup. It
turns features on in the merged build only — there is nothing in the OSS build
for it to unlock, and installing one there changes nothing and reports nothing.

**A licence installed into a running controller does not take effect until the
controller restarts.** Route registration is read exactly once, when the HTTP
mux is built, so a companion's login and callback routes cannot appear in a
process that has already started serving. This is the same "registration is
over" rule [extensibility.md](extensibility.md) states for every seam, and the
alternative — a mux that can be rebuilt underneath live requests — is what that
document argues against on its own terms.

So: install the licence, then `docker service update --force` the controller (or
redeploy the stack). The startup log's `routes` lines are where to check that it
took: an unlicensed build declares **no** licensed routes at all, so `/auth/*`
appearing there is the licence having been read.

What a licence grants today is [single sign-on](sso.md), which is where that
`/auth/*` comes from. Multi-swarm, projects and RBAC, notifications and managed
secret rotation are the rest of the plan; the seams they will arrive through are
in [extensibility.md](extensibility.md).

## The rule this model depends on

**Nothing AGPL may ever be linked into either artefact.** `swarmcli-rbac-proxy`
is AGPL-3.0 and is reached over the network, never imported — that is what keeps
the combination lawful, and it is a fact about the module graph rather than a
convention. Linking an AGPL package into a binary that also carries proprietary
code is a licence violation, not an inconvenience.

Both halves are checked rather than remembered. The merged build scans its own
linked module graph and refuses to publish on AGPL or GPL;
`scripts/check-oss-artefact.sh` runs on each published `swarmcli-cd-oss` binary
and on every change in CI, and fails if the main module is not this repository
or if anything private is linked.

## Getting the OSS build

```bash
# The archive — pick <version> from the releases page (v1.1.0 or later):
curl -sSLO https://github.com/Eldara-Tech/swarmcli-cd/releases/download/v<version>/swarmcli-cd-oss_Linux_x86_64.tar.gz

# Or the image:
docker pull eldaratech/swarmcli-cd:<version>-oss
```

There is deliberately no moving `:oss` tag. A deployment that wants the
verifiably-Apache-2.0 artefact pins a version, which is what an air-gapped or
compliance-reviewed deployment does anyway — and a moving tag on the artefact
whose whole point is being checkable is a way to be surprised by what is
running. `checksums-oss.txt` in each release covers these artefacts;
`checksums-merged.txt` covers the merged ones. Neither is named plain
`checksums.txt`: a release carries two sets of artefacts, and an unqualified
name would not say which set it verified.

Building it yourself is the same thing, with one caveat:

```bash
npm --prefix web/ui ci && npm --prefix web/ui run build   # or the UI is missing
go build -o swarmcli-cd ./cmd/swarmcli-cd
```

`go build` and `go install …@latest` both succeed with no Node installed and
produce a binary that serves the API normally and answers every browser with a
page saying it was built without its web UI. That is deliberate — it is what
keeps this repository buildable without a JavaScript toolchain — but it means
**`go install` is not a way to get the web UI.** The release archives and the
images have it compiled in; see [the web UI page](web-ui.md).
