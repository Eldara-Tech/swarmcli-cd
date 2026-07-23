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
`progressing`, `degraded`, or `missing`. `missing` — declared but not present —
is deliberately distinct from `degraded` — present but unhealthy; an operator and
a UI both need to tell those apart.

They are independent axes. A stack can be perfectly synced and badly degraded at
the same time, and collapsing the two would lose the distinction that makes the
view useful. Every list row shows both, plus a `3/4` service count, without
having to open the application.

**Drift** is what the sync axis reports between deploys. On a manual application,
the controller reconciles, sees the swarm no longer matches git, and records
`out-of-sync` — but does not apply it. The drift is observed; acting on it waits
for an explicit `app sync`. On an automated application the same reconcile
applies the change. In Phase 1 drift is decided at the **manifest** level: the
rendered manifest is compared against what was last applied. (Comparing the
desired `ServiceSpec` against the live one — catching a change made directly with
`docker service update` — is Phase 2.)

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

Every release swarmcli-cd installs is stamped with an owner, `cd/<application>`,
recorded in the release-history config and in the stored record. The stamp is
what lets a later reconcile tell a release *this application* installed from one
it has never seen — the prerequisite for ever pruning safely, because it
separates "this is obsolete" from "I do not recognise this".

This is the same ownership mechanism CE's `charts apply` uses, with one
consequence worth stating plainly: when your release file is consumed by
swarmcli-cd, **its own `owner:` field is ignored** — the controller substitutes
`cd/<application>`. Set the application's identity in `applications.yaml`, not in
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

## Read-only applications, and no hot reload

In Phase 1 the applications file is the only source of truth, and the API serves
it read-only. This is not an unfinished CRUD API: the file is delivered as a
Docker config, Docker configs are immutable, and changing one replaces the
container. A file-watcher would never fire, because the process that would notice
the change does not outlive it. The API paths are nouns (`/applications/{app}`)
so that write operations can be added later without any of them moving.

## The open-core seams

Four behaviours are pluggable through interfaces with working OSS defaults —
swarm registry, authorizer, notifier, secret provider — replaced by a private
companion in Phase 3. The public build is a complete product; the seams just mark
where the licensed edition swaps multi-swarm, SSO/RBAC, Slack notifications and
SOPS decryption in. This is developer-facing detail; it lives in
[extensibility.md](extensibility.md).

## See also

- [Getting started](getting-started.md) · [Configuration](configuration.md) · [HTTP API](api.md)
- [Issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1) — positioning, the decisions D1–D6, and the phase plan
