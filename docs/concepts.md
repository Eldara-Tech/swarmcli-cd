<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Concepts

The ideas the CLI and API surface, and why they are shaped the way they are. If
you have used Argo CD, most of this will rhyme; the differences are where Swarm
differs from Kubernetes.

## The pull loop

swarmcli-cd is a controller that runs *in* the swarm and pulls. On a schedule,
for each application, it fetches the repository at the pinned revision, renders
the desired state, plans it against what is actually running, and — when the
sync policy says so — applies it.

This is the half that CI running `swarmcli charts apply` after a merge does not
give you: CI holds cluster write credentials, runs only on a push, and corrects
nothing that drifts between deploys. A controller holds the credentials itself,
inside the swarm, and reconciles continuously.

Each application runs on its own schedule in its own goroutine. One application
whose repository is unreachable backs off and retries without stalling the
others. The default interval is three minutes; an application that needs to be
quicker sets its own `syncPolicy.interval`.

## Drift, sync, and health

Two questions, deliberately kept apart.

**Sync** answers *does the swarm match git?* It is `synced` or `out-of-sync`. The
out-of-sync case carries a summary of the plan that made it so, counted by action
— how many releases would be installed, upgraded, or left unchanged.

**Health** answers *is what is running actually working?* It is `healthy`,
`progressing`, `degraded`, `missing`, or `unknown`. `missing` — declared but not
present — is deliberately distinct from `degraded` — present but unhealthy; an
operator and a UI both need to tell those apart. `unknown` is the swarm not
having been readable at all, which is distinct again: an unreachable daemon is
not a stack that has gone away, and reporting it as one would raise the loudest
alarm this axis has about something that is very likely fine.

They are independent axes. A stack can be perfectly synced and badly degraded at
the same time, and collapsing the two would lose the distinction that makes the
view useful. Every list row shows both, plus a `3/4` service count, without
having to open the application.

Both are the last *successful* observation, so a reconcile that never reached one
leaves them saying what they said before. When that happens the list grows a
`RECONCILE` column marking the affected rows, and prints the reason under the
table; `app get` and the JSON carry the full error either way. Without the
column, an application whose repository has been unreachable for a week reports
last week's verdict beside a timestamp of seconds ago.

**Drift** is what the sync axis reports between deploys. On a manual application,
the controller reconciles, sees the swarm no longer matches git, and records
`out-of-sync` — but does not apply it. The drift is observed; acting on it waits
for an explicit `app sync`. On an automated application the same reconcile
applies the change.

### Two ways to be out of sync

`driftDetection` decides which of them the controller looks for.

**`manifest`** (the default) compares the rendered manifest against what was last
applied. It catches a changed chart version, changed values and a changed
template — everything that happens because *git moved*.

It cannot catch anything that happened to the swarm afterwards. Swarm has no
server-side apply, so `docker service update --replicas 10` produces no conflict
signal at all: the next reconcile computes the same desired spec, finds it
unchanged, and does nothing. The hand-scaled service stays hand-scaled until some
unrelated commit triggers a deploy, and is then silently overwritten.

**`live`** additionally compares the running `ServiceSpec` of each settled
release against the one the repository renders to. That is the only thing that
catches *the swarm moving*, and it makes the controller report — and, with an
automated policy, correct — a change nobody committed.

The two are separate axes in the status for the same reason sync and health are.
`sync` goes `out-of-sync` either way, so the sync button and anything alerting on
that field work unchanged; `drift` beside it says the reason was the swarm rather
than git, which is a different thing for an operator to do something about. A
commit is something to review; a change nobody recorded is something to ask
about.

The comparison is at `ServiceSpec` level, never at YAML level, and is a named
list of fields rather than everything — see
[configuration § driftDetection](configuration.md#driftdetection-optional) for
what is compared and what is not.

## Revision, and last sync

The sync state carries two revisions, and they answer different questions. The
assessment's `revision` is the commit the swarm was *compared against* — the tip
of the pinned branch, right now. The `lastSync.revision` is the commit that was
actually *deployed*.

When they differ, there is a newer commit that has not been applied yet — which
is a different condition from being out-of-sync against the commit you are on,
and a UI shows both. A manual application that has drifted, and an automated one
mid-way between its interval ticks, are both this case.

## Automated versus manual

`syncPolicy.automated` is the difference between a controller that deploys for
you and one that only tells you what it would deploy.

- **Automated** — reconcile *and apply* on the schedule. This is GitOps in the
  usual sense: merge to the branch, and the swarm converges.
- **Manual** — reconcile and *report*, but apply only when you run `app sync`.
  "Manual" means "not on a schedule", not "never": an explicit sync still
  deploys. This is the mode for a production swarm where a human approves each
  rollout after reading the diff.

## Ownership

Every release swarmcli-cd installs is stamped with an owner,
`cd/<controller>/<application>`, recorded in the release-history config and in the stored record. The stamp is
what lets a later reconcile tell a release *this application* installed from one
it has never seen — the prerequisite for pruning safely, because it separates
"this is obsolete" from "I do not recognise this".

The stamp names the controller as well as the application —
`cd/<controller>/<application>` — because prune acts on the difference between
"mine" and "not mine". Without the controller half, a second swarmcli-cd on the
same swarm would read the first one's applications as departed and delete them.
Two controllers sharing a swarm must therefore have distinct `--controller-id`
values; see [configuration § two controllers on one swarm](configuration.md#two-controllers-on-one-swarm).

That stamp lives on the swarm, not in the controller's memory, which is what
makes [prune](configuration.md#prune) survive a restart: a controller that comes
up and finds a release stamped for one of its own applications that its app set
no longer declares knows the application departed, even though it never watched
it leave. It is also why prune needs no database — the swarm is the record.

A stamp records who *installed* a release, and it is only rewritten by a
reconcile that deploys. A stamp naming an application that no longer exists —
exactly what renaming one produces — is therefore corrected rather than
permanent: the owner is part of what the plan compares, so the release is
redeployed once under the new name. But it is corrected no sooner than the next
reconcile that gets that far, and a prune sweep can run first. That is why prune
asks a second question before it deletes anything: not only "whose stamp is
this" but "is any application still declaring this release". Only a release that
fails both is left behind; see
[configuration § renaming an application](configuration.md#renaming-an-application).

The same question is asked one scope down, and for all four kinds a manifest
declares. A *service, network, config or secret* inside a release carries only
the stack's `com.docker.stack.namespace` label, which says where it lives and not
who put it there — anything can set it. So before deleting one its chart has
stopped declaring, the controller looks for a stored revision of that release,
stamped by it for this application, that declared it. One that passes is
**orphaned**: provably installed from this repository and provably no longer
wanted. One that does not is **unmanaged**, reported and never touched. It is the
release-level orphan/unmanaged distinction applied to what is inside a release;
see
[configuration § what a chart stops declaring](configuration.md#what-a-chart-stops-declaring).

One rule and one ownership model, applied once per kind rather than over a merged
list — Swarm scopes all four into a single namespace of names, so a name is only
ever evidence about its own kind.

### What a release name may claim

A release name *is* the stack namespace, so choosing one is a claim on everything
already carrying that label. Two claims are refused.

A release may not be named after the stack the controller itself runs as — the
`swarmcli-cd` in `docker stack deploy -c stack.yml swarmcli-cd`. Deploying one
would write that chart's services over the controller's own, and removing one
would delete the controller along with the volume holding every application's
clone and chart cache. The controller reads that name off its own service, so a
development run that is not a swarm service has no name to protect and nothing to
refuse.

And a release may not deploy into a namespace whose services this controller has
no release record for. Those services were put there by something else — a
`docker stack deploy`, another tool, a shell — and the namespace label they carry
says where they live, not who deployed them. An existing stack is brought under
GitOps by removing it and letting the controller install it, not by naming a
release after it: the release is also the unit an uninstall and a prune act on,
so sharing a namespace with a stack the controller did not install means sharing
that too.

This is the same ownership mechanism CE's `charts apply` uses, with one
consequence worth stating plainly: when your release file is consumed by
swarmcli-cd, **its own `owner:` field is ignored** — the controller substitutes
its own stamp. Set the application's identity in `applications.yaml`, not in
the release file. The full ownership model, including the orphan-versus-unmanaged
distinction, is documented on the engine:
[swarmcli charts README § ownership](https://github.com/Eldara-Tech/swarmcli/blob/main/charts/README.md#ownership).

## Rollback comes from Swarm

When `syncPolicy.wait` is set, a release does not just get applied and forgotten:
the controller waits for its services to converge. And when a service declares
`update_config.failure_action: rollback`, a rollout that fails to converge is
rolled back to its previous spec — not something swarmcli-cd builds, but Swarm's
own `failure_action` and the `PreviousSpec` the platform keeps for every service.
The applier uses what the platform gives away for free, which is also why it can
diff, prune and roll back things `docker stack deploy` cannot.

What the controller does add is knowing that it happened. A rollback leaves the
service running something the repository does not ask for, which under
`driftDetection: live` is indistinguishable from somebody editing it by hand — and
the two want opposite responses. So a service Swarm reverted is reported under its
own reason and never redeployed, because the platform has already judged that spec.
See [configuration § a service Swarm rolled back](configuration.md#a-service-swarm-rolled-back).

## Why the applier is not `docker stack deploy`

The obvious way to apply a compose file is to shell out to `docker stack deploy`.
It cannot be the applier here, for reasons established with reproductions in
[issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1): its `--prune`
removes services only — never networks, configs or secrets — and swallows its own
list errors; it has no dry-run, so nothing can be shown before it is applied; and
`--detach=true` returns before convergence. The applier is built directly on
docker/cli's exported loader and the moby client instead, which is what lets it
diff, prune and roll back at all. The image carries no docker binary because it
needs none.

## Chart compatibility

A chart may declare the engine it needs (`swarmcliVersion: ">= 1.13.0"` in its
`Chart.yaml`). The controller embeds one chart-engine version — whichever
swarmcli release this build pinned — and **refuses to apply** a plan containing a
release that engine is too old for, recording why on the application's status.
Releases that would be unchanged are exempt, since applying will not touch them.

The refusal matters because there is no operator standing by to ask: the
alternative is a failure minutes later inside the render, naming whatever feature
happened to be missing. A build with an *unstamped* engine (a plain `go build`)
reports every compatibility check as `unknown` rather than blocking — fine for
development, not for anything that deploys. See
[RELEASING.md](../RELEASING.md).

## Read-only applications, and the two tiers

The applications file is the only source of truth, and the API serves it
read-only. This is not an unfinished CRUD API: the way to change what the
controller runs is to change that file, and the API's job is to show you the
result. The paths are nouns (`/applications/{app}`) so that write operations can
be added later without any of them moving.

Where the file lives is a deployment choice, and the two options behave
differently on purpose. Mounted as a **Docker config** it cannot change under the
process at all: configs are immutable, changing one replaces the container, and a
file-watcher would never fire because the process that would notice does not
outlive the change. Sourced from **git** — or from a directory something else
keeps current — it is re-read on an interval, validated in full, and swapped in
only if it is valid, so adding an application is a commit rather than a redeploy.

The controller's own bootstrap stays in the first category either way. That is
what makes it an anchor: it says which repository is authoritative, and nothing
in that repository can say otherwise. See
[configuration § where the app set lives](configuration.md#where-the-app-set-lives).

## The open-core seams

Four behaviours are pluggable through interfaces with working OSS defaults —
swarm registry, authorizer, notifier, secret provider — replaced by a private
companion in Phase 3, and a fifth seam through which that companion adds HTTP
routes rather than replacing anything. The public build is a complete product;
the seams just mark where the licensed edition swaps multi-swarm, SSO/RBAC,
Slack notifications and SOPS decryption in, and where it adds the endpoints
those need. This is developer-facing detail; it lives in
[extensibility.md](extensibility.md).

## See also

- [Getting started](getting-started.md) · [Configuration](configuration.md) · [HTTP API](api.md)
- [Issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1) — positioning, the decisions D1–D6, and the phase plan
