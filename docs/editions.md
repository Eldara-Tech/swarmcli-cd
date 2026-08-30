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
| `swarmcli-cd_*_oss` archives, `eldaratech/swarmcli-cd:<version>-oss` | `cmd/swarmcli-cd` in this repository, nothing else | this repository, and nothing else | wholly Apache-2.0 |

Two artefacts start with `v1.1.0`. `v1.0.0` and the release candidates before it
published the plain names only, and a single `checksums.txt` — there was one set
of artefacts to verify — so those releases have no Apache-2.0 archive and no
`-oss` image to download.

The archive name in the table is the one from the release *after* `v1.1.0`
onwards. `v1.1.0` itself carries the qualifier at the front —
`swarmcli-cd-oss_Linux_x86_64.tar.gz` — which sorted every Apache-2.0 archive
above every merged one on the release page, since GitHub renders assets sorted
by `lower(name)` and gives no other control. Only the asset name moved; the
image tag is `<version>-oss` throughout.

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

A licence is installed as a Docker config and read at startup. There is no
secret form: the reader does one config inspect, by name and by label, so a
licence delivered any other way is one it never sees. It turns features on in
the merged build only — there is nothing in the OSS build for it to unlock, and
installing one there changes nothing and reports nothing.

**A licence installed into a running controller takes effect on its own. Only
its routes wait for a restart.** The licence config is re-read whenever
something asks the controller and the last read is more than five minutes old,
and reloaded only when it has actually changed — so the edition, the feature map
and [the badge](web-ui.md#the-licence-badge) all move with nothing done to the
process: within minutes on a controller anybody is using, and on the licence
watcher's own six-hourly pass on one nobody is. Route registration is the
exception, and it is read exactly once, when the HTTP mux is built, so a
companion's login and callback routes cannot appear in a process that has
already started serving. This is the same "registration is over" rule
[extensibility.md](extensibility.md) states for every seam, and the alternative
— a mux that can be rebuilt underneath live requests — is what that document
argues against on its own terms.

So: install the licence, then `docker service update --force` the controller (or
redeploy the stack) for anything that is a route — SSO is. The startup log's
`routes` lines are where to check that it took: an unlicensed build declares
**no** licensed routes at all, so `/auth/*` appearing there is the licence
having been read.

### A licence does not have to be bought

The **free** tier grants what Business Edition grants — every licensed feature
on this page, single sign-on included — on a swarm of up to **three nodes**. It
is a licence like any other: signed by the issuer, installed as the same Docker
config, read by the same reader, and turning on the same features.

Two things bound it, and neither of them is a feature switched off. A free
licence carries a **mandatory expiry** — one that never expired would be a
perpetual grant on every swarm it reached, so the term is finite by construction
and the issuer rolls it forward. And the three nodes are the issuer's own
judgement rather than this controller's.

### The node allowance

The issuer reports back what it believes this deployment's size to be, and the
controller passes that on unchanged: an `allowance` block on [the capability
document](api.md#discovery--get-uibootstrapjson-and-get-apiv1capabilities), a
line beside the badge, and a `WARN` in the logs.

**Nothing is switched off by it, and nothing may be.** That report is unsigned —
it is what a server said, not something this build verified against a key
compiled into it — and a controller that disabled a feature on it would be a
controller one firewall rule could disable. Enforcement is unchanged and stays
where it already is: in the signed token's own expiry and in the lease window,
judged offline.

What being over the allowance costs is the roll-forward. The issuer stops
extending the term, and the licence then expires on a date the warning has been
naming all along — which is why that date is in the line and not only the node
count. On a free-tier licence this is the whole of how three nodes is bounded,
and the log line is the only notice there is.

### A managed licence has to be kept activated

Most licences are a key and nothing else: install it and it works until it
expires. A **managed** licence adds a second artifact — an *activation lease*
for one swarm, with its own dates, typically 30 days to renewal and a further
30 days of grace during which everything keeps working while the renewal is
overdue.

**The merged build renews its own licence.** The deployment this page is
written for is a swarm running swarmcli-cd and nothing else — the chart is
standalone and does not require SwarmCLI's bootstrap stack, so the
licence-renewer service that keeps a bootstrapped swarm current is not there to
do it. The controller does it instead, on the same refresh that notices an
installed licence: it renews the lease when the lease is due, and collects a
re-signed token when one is owed. Both happen on that refresh's own goroutine,
off the request path, and a failure is a log line and never a deny — the
installed lease goes on granting to the end of its own window, because a
reconciler holding write access to a swarm must not stop converging because a
licensing call timed out.

It is on by default and talks to `https://swarmcli.io/api/v1`. The two variables
that point it at another deployment or switch it off entirely are in
[configuration § environment](configuration.md#environment), and they are the
only controls over the one request this controller makes on its own initiative.

A cron job on a manager is still the answer wherever that renewal is not
running — the `-oss` artefact, which links no licensed module and so renews
nothing, and a merged build with renewal switched off:

```cron
# on a swarm manager, daily
0 4 * * *  swarmcli license sync
```

`swarmcli license sync` renews this swarm's activation and exits non-zero if it
could not; `swarmcli license status` reports what the swarm holds and exits
non-zero when the licence is not granting. Both are in
[swarmcli's licence documentation](https://github.com/Eldara-Tech/swarmcli/blob/main/docs/license.md).

**A licence that is installed but not activated reads as `absent`, and `absent`
has two remedies.** The controller reports that status when there is no licence
at all *and* when a managed one is installed, verifies, and has not been
activated for this swarm — so "install a licence" is half of it, and the
operator it is not written for is holding the key already. The other half is
activation, in an online form and an offline one:

```bash
swarmcli license sync                  # on a manager with a route out
swarmcli license lease install <file>  # or :license lease install <file> in the
                                       # TUI — the air-gapped path
```

The second is not an edge case here. Activation is automatic exactly where the
licence service is reachable, so a swarm sitting in `absent` is
disproportionately one that cannot reach it, and the lease file it was sent is
the route it has. The badge and the controller's warning name both.

This matters more here than anywhere else, because the renewal a person
performs by opening the swarmcli TUI is a person nobody has: a controller runs
unattended for weeks, which is exactly the deployment where an activation
quietly runs out. A renewed lease reaches a running controller the way an
installed licence does — on the next refresh, with no restart unless what it
turns on is a route.

**The controller says so before it stops.** Two weeks before a licence expires
it starts logging a warning naming the date, the days left and the remedy, and
it keeps warning through the grace period and after; [the badge in the web
UI](web-ui.md#the-licence-badge) turns amber over the same window rather than
reading healthy until the morning the features stop. Neither can fail a
reconcile: the check runs on its own goroutine, only logs, and survives a
licence reporter that panics. A build with no licensed module says nothing at
all — there is no licence there to lapse.

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
# The archive — pick <version> from the releases page. This name is the one from
# the release after v1.1.0 onwards; on v1.1.0 itself the asset is
# swarmcli-cd-oss_Linux_x86_64.tar.gz:
curl -sSLO https://github.com/Eldara-Tech/swarmcli-cd/releases/download/v<version>/swarmcli-cd_Linux_x86_64_oss.tar.gz

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
