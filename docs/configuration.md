<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Configuration

swarmcli-cd is configured in two tiers. The **applications file** — the *app
set* — lists what to reconcile. The **bootstrap** — a handful of flags and
environment variables — says where that file lives, which address to listen on,
and which credentials to use. Everything an operator edits about the one thing
they think about lives in the app set; everything about the process lives in the
bootstrap.

The split matters because the two change at completely different rates. The
bootstrap is set once at deploy time and rarely touched. The app set can be
every day, which is why it can live in git.

Worked, copy-pasteable files: [`../examples/applications.yaml`](../examples/applications.yaml)
for the set itself, and [`../examples/appset-repo/`](../examples/appset-repo/)
for the git-sourced layout.

## The applications file

Declared in a file rather than a database on purpose: a GitOps controller whose
own desired state lived in mutable storage would have a bootstrap problem. The
file is the only source of truth and the API serves it read-only — in both
modes, because the way to change the set is to change the file.

```yaml
apiVersion: v1        # "v1", or absent meaning the same
applications:
  - name: edge
    source: { ... }
    destination: { ... }
    syncPolicy: { ... }
    driftDetection: manifest
```

Unknown keys are an error. A misspelled key that was quietly ignored would leave
a setting you believe you configured silently doing nothing.

### `name` (required)

The application's identity. It becomes a URL path segment and the last part of
the owner stamp `cd/<controller>/<name>`, so the charset is narrow: lowercase letters, digits, dot,
dash and underscore, starting with a letter or digit. No spaces, no colon.
Names must be unique within the file.

### `source` (required)

Where the desired state lives in git.

| Field | | |
|---|---|---|
| `repoURL` | required | the git repository to clone (`https://…`, or any URL go-git accepts) |
| `revision` | required | branch, tag or SHA — **pin it**; an unpinned source would deploy whatever the default branch happens to point at |

Then **exactly one** of two source types. Which field is present *is* the source
type — a separate discriminator would be a second thing to keep consistent.

**Which one?** The practical difference is where a version bump lives. A
`releaseFile` keeps the pinned chart versions in a `swarmcli-release.yaml`
committed to your repo, so bumping one is an ordinary git commit in *that* repo
— the controller fetches it on the next reconcile (Renovate can even open the
PR). A `chart` source keeps the version here in `applications.yaml`.

How much that costs depends on where your app set lives. Mounted as a Docker
config it is the awkward option: the config is immutable, so bumping the version
means a new config object and a controller redeploy, and a chart whose version
moves often belongs in a release file instead. [Sourced from
git](#where-the-app-set-lives), both are commits — one to the application's
repository, one to the app-set repository — and the `chart` source stops being a
compromise.

#### `source.releaseFile` — a release file in the repo

A path, within the repository, to a `swarmcli-release.yaml`: the same declarative
release file [`swarmcli charts apply -f`](https://github.com/Eldara-Tech/swarmcli/blob/main/charts/README.md#declarative-releases-gitops)
takes. The controller renders it, plans it against the swarm, and applies it.

```yaml
source:
  repoURL: https://github.com/your-org/infra.git
  revision: main
  releaseFile: swarm/prod/swarmcli-release.yaml
```

The release-file format — `releases`, `repositories`, per-release `chart`,
`version`, `values`, the "pin a `repo/chart`, omit a version for a local path"
rule — is documented once, on the CE engine that reads it:
[swarmcli charts README](https://github.com/Eldara-Tech/swarmcli/blob/main/charts/README.md#declarative-releases-gitops).

One difference when swarmcli-cd is the consumer: **omit `owner:`** from the file.
The controller passes its own owner, `cd/<controller>/<name>`, so the file's
`owner:` is ignored. See [concepts § ownership](concepts.md#ownership).

#### `source.chart` — one chart, no release file

The "one application, one chart" case that does not deserve a release file. The
controller synthesises the release file it implies and hands it to the engine's
own parser, so a synthesised file obeys exactly the rules a committed one does.

```yaml
source:
  repoURL: https://github.com/your-org/infra.git
  revision: v1.2.0
  chart:
    release: hello                  # the release name to install as (required)
    ref: swarmcli-charts/whoami     # repository/chart
    version: "0.1.8"                 # required WITH a ref
    values: [values/hello.yaml]      # optional; paths within the repo
    repositories:                    # required WITH a ref, to resolve it
      - name: swarmcli-charts
        url: https://eldara-tech.github.io/swarmcli-charts
```

`version` and `values` are different axes: `version` selects *which chart
package* to pull, and the `values` files configure *how it renders* — replica
counts, the app's own image tag, and so on. Values are consumed by the chart, so
they cannot select its version, which is why a `ref` carries its own `version`.

| Field | | |
|---|---|---|
| `release` | required | the release name |
| `path` | one of path/ref | a chart directory within the repo (`./charts/mine`); `version` must be **omitted** — the chart's `Chart.yaml` is its version |
| `ref` | one of path/ref | `repository/chart`; needs `version` and `repositories` |
| `version` | with `ref` | the pinned chart version — **required** with a ref, because a floating pin would silently upgrade production on the next reconcile |
| `values` | optional | values files, as paths within the repo |
| `repositories` | with `ref` | `name` + `url` for each chart repository the ref resolves against |

**Path safety.** `releaseFile`, `chart.path` and every `values` entry must be
relative and stay inside the checkout: an absolute path, or one that escapes with
`../`, is rejected at load. A path that resolves outside the repository through a
symlink is refused again at render time — repository content is not trusted the
way your own configuration is, and a values file pointing at `/run/secrets` would
otherwise be read and merged into a manifest.

### `registryAuth` (optional)

```yaml
registryAuth: swarmcli-cd-regauth-edge
```

Names a Docker secret holding a docker `config.json` (`{"auths": {…}}`) — the
same file `docker login` writes — for the private registries this application's
images come from. Without it, the controller pulls anonymously and a private
image fails as a convergence timeout with a pull error.

It is **per application, and that is the isolation**: the controller uses only
the secret an application names, so one application cannot pull another's private
images even though every application's credential is mounted in the same
controller. Two applications that share a registry may name the same secret.

The value is a **secret name**, not a path: the secret must be mounted at the
default `/run/secrets/<name>`, which the short form `secrets: [<name>]` in
[stack.yml](../stack.yml) does. Create it from a login and mount it:

```sh
docker --config /tmp/rc login ghcr.io -u <user> -p <pat>
docker secret create swarmcli-cd-regauth-edge /tmp/rc/config.json
# then add swarmcli-cd-regauth-edge to the controller's secrets in stack.yml
```

The credential reaches the swarm only for the pull: it is stored encrypted in
the raft log and is never injected into a deployed container or returned by
`docker service inspect`. Nor can a reconciled stack reach it the other way — see
[what a reconciled stack may not reference](#what-a-reconciled-stack-may-not-reference).

#### What a reconciled stack may not reference

Swarm secrets and configs are **cluster-global objects referenced by name**.
There is no namespace on that reference: a stack declaring one as `external:`
gets whatever exists under that name, whoever created it. So the controller
refuses to deploy a stack that reaches for anything of its own, and refuses it
whole — before any network, config, secret or service is created, so nothing is
left half-deployed.

Reaching for a name is not the only way to get at it. A manifest can also put
that name on a resource it declares as **its own**, which is not an `external:`
reference at all:

```yaml
secrets:
  x:
    name: swarmcli-cd-token     # not this stack's to name
    driver: vault
```

A secret that already exists is not created again, so the stack would simply be
handed the real one — and its labels rewritten into the stack's namespace, where
uninstalling that release would delete it. Declared names are therefore checked
against the same three sets as referenced ones, whether or not any service mounts
them (swarmcli-cd#86). A chart's own config or secret is unaffected: its name is
namespace-scoped to `<release>_<name>`, so it is nobody else's.

Three things are off limits:

| | |
|---|---|
| the controller's **secrets** | the admin token, the git token, every `registryAuth` credential |
| the controller's **configs** | the application set, which names every repository, revision, destination and policy this controller applies |
| the engine's **release records** | one Docker config per release revision (`com.swarmcli.type=release`), each holding a rendered manifest |

The first two are read from the controller's **own service spec**, not from the
filesystem. A secret arrives at `/run/secrets/<name>` so a directory listing
almost answers it — but that yields the mount *target*, and a `stack.yml` written
in compose's long form can rename it, which would leave the guard comparing the
wrong name and silently matching nothing. A config has no such path at all: it
lands wherever `target:` puts it, with nothing tying that back to its name. The
service spec has both under the names a reference actually resolves by.

The release records cannot come from a set read once at startup, because a new
one is written on every deploy. They are matched by their label at deploy time.

Nothing here needs configuring, and there is no flag to forget. A controller that
is **not** running as a Swarm service — a development run — has nothing mounted
by Swarm and nothing of its own to protect, so this is inert rather than broken.
If that read does not get through — the daemon unreachable, or reachable and
answering with an error — the deploy **fails** rather than proceeding unguarded,
and the next one tries again. A failure is never remembered as "nothing is
mounted": only an answer is.

An ordinary `external:` reference to a config or secret an operator created for
their own stacks is unaffected, and so is a chart declaring and mounting its own.
Only the controller's own and the engine's own are refused.

**Static credentials only.** The controller image ships no docker credential
helpers, so a `config.json` using `credsStore` or `credHelpers` is refused at
startup. Registries with static credentials (Docker Hub, GHCR, Harbor, GitLab,
self-hosted) work; short-lived cloud-registry tokens (ECR, GCR) are not
refreshed and must be rotated out of band for now.

### `destination` (optional)

```yaml
destination:
  swarm: ""     # "" or omitted = the swarm the controller runs in
```

Names the swarm to deploy to, resolved through the swarm-registry seam. The OSS
build resolves exactly one — the swarm the controller runs in — so any non-empty
name is unreachable and the sync fails naming it. Multi-swarm is a licensed
capability (Phase 3); see [extensibility.md](extensibility.md).

### `syncPolicy` (optional)

When and how a plan is applied.

| Field | Default | |
|---|---|---|
| `automated` | `false` | `true` reconciles **and applies** on a schedule. `false` is manual: the controller still reconciles and reports drift, but applies only on an explicit `swarmcli-cd app sync`. "Manual" means "not on a schedule", not "never". |
| `interval` | controller default (3m) | how often to reconcile this application, as a duration string (`90s`, `5m`) |
| `wait` | `false` | block each release until its services converge, and — when the service declares `update_config.failure_action: rollback` — let Swarm roll it back on a failed rollout |
| `timeout` | engine default | how long `wait` waits for a rollout before giving up |
| `historyMax` | `10` | revisions kept per release (one Docker config each); older revisions are pruned after a deploy that succeeded. An explicit `0` keeps every revision, which is what the chart engine has always read the number as — and, on a controller deploying on a timer, a slow leak into the manager's raft log |
| `prune` | `false` | delete the resources of a release this application no longer declares. Only ever its own releases — see [prune](#prune) |
| `pruneVolumes` | `false` | extend `prune` to the named volumes of what it deletes. Requires `prune`; set alone it is a config error |
| `pruneResources` | `false` | delete a service, network, config or secret this application's chart used to declare and no longer does. Stands alone — it does not require `prune`; see [prune](#prune) |
| `pruneFirst` | `false` | delete before installing rather than after, so a replaced release never overlaps its replacement. Requires `prune` or `pruneResources`; see [ordering](#ordering-and-workloads-that-must-never-run-twice) |

(The `swarmcli-cd app sync --wait` *client* command has its own, separate
`--timeout`, defaulting to 5m — that bounds how long the CLI watches, not the
apply itself.)

Releases are applied in the order the release file lists them; use `wait: true`
if a later release needs an earlier one live first.

#### A chart that does not render the same twice

Every sync re-plans once the apply has landed, to confirm it. A release that the
confirming re-plan **still** calls out of date, moments after being deployed, has
a chart whose render is not reproducible — `{{ now }}` stamped into a label,
`uuidv4`, `randAlphaNum`. Nothing about that converges: every reconcile would
rewrite every service in the release, roll every task, and store another revision,
on every interval for ever.

So the controller deploys it once and then holds the application at that
revision, saying so on the status and in the log. The hold lifts when something
could actually change the answer — a new commit, an edit to the application, or
`swarmcli-cd app sync`, which is never subject to it. The remedy is to take the
non-deterministic value out of the render, or to pin it in a values file so that
changing it is a commit.

### `driftDetection` (optional)

```yaml
driftDetection: manifest
```

How drift is decided. Omitting it defaults to `manifest`.

| Mode | Compares | Catches |
|---|---|---|
| `manifest` | the rendered manifest against what was last applied | git moved: a changed chart version, values or template |
| `live` | the above, **and** each settled release's running `ServiceSpec` against the one the repository renders to | the swarm moved: `docker service update`, a deleted service, a spec Swarm rolled back |

`live` costs one service list and one manifest conversion per settled release per
interval, plus one network list per application per interval to name the networks
its services are attached to. It is the mode that lets the controller notice — and
correct — something nobody committed. See
[concepts § drift](concepts.md#drift-sync-and-health) for how it reads.

#### What `live` does about it

With `syncPolicy.automated: true`, it **redeploys** the drifted release, putting
the running services back to what the repository declares. With a manual policy
it reports and waits, and `swarmcli-cd app sync <app>` corrects it — the same
rule the rest of the controller follows, where manual means "not on a schedule"
rather than "never".

The correction writes no new chart revision. The desired state did not change;
the swarm was put back to it. `app get` shows the outcome and a
`drift-converged` event records that it happened.

A correction is a deploy, so `syncPolicy.wait` governs it too: with `wait: true`
the sync blocks until the corrected release converges or `timeout` expires, and
a correction that deploys but never settles fails the sync rather than reporting
success. Releases are corrected in the order the release file lists them, so
`wait` also gives the same ordering guarantee here that it gives an ordinary
apply.

A release whose live state could not be read reports `unknown` and is **not**
corrected: the controller does not rewrite a service on the strength of a read
it could not make. The same applies when the manifest it would be compared
*against* cannot be converted — a config the release mounts having been deleted by
hand, say. That costs the comparison only: `pruneResources` reads what a release
declares, which needs nothing it mounts to still exist, so a sweep is unaffected.

#### A service Swarm rolled back

One difference is reported and deliberately never corrected. When a service
declares `update_config.failure_action: rollback` and the spec this controller
wrote will not converge, Swarm reverts it — so the running spec stops matching the
repository through no act of anybody's. That reads exactly like a hand edit, and it
wants the opposite response: re-applying the manifest re-applies the spec the
platform has already rejected, which fails the same way, for ever.

Such a service reports reason `rolled-back` carrying the daemon's own explanation,
and it **vetoes the correction of its whole release** — including a sibling service
drifting in the ordinary way, because an apply writes every service a release
declares and there is no way to rewrite the sibling without re-writing the rejected
spec beside it. A manual `app sync` does not override it either; the remedy is a
commit, which arrives as an upgrade rather than as drift and clears the state with
it.

#### Which releases are compared

Only those the plan calls **unchanged**. A release that would be installed has
nothing running to compare against, and one that would be upgraded is about to
have its spec overwritten anyway — the sync axis already says to act on it.

#### What is compared

Diffing is at `ServiceSpec` level and never at YAML level: `compose →
ServiceSpec` is lossy and one-way, so reconstructing compose from a live service
manufactures differences that do not exist.

The comparison is a **named list of fields**, not the whole spec. A spec read
back from the daemon is not the one that was written — swarmkit defaults fields
the manifest never mentioned, resolves images to digests, and returns empty maps
where nil was sent — so a whole-spec comparison reports permanent drift on
services nobody has touched. The boundary is what `docker service update` can
change, which is the surface an out-of-band change actually uses:

| Reported as | Corresponds to |
|---|---|
| `mode` | a service recreated in a different mode |
| `replicas` | `--replicas` |
| `image` | `--image` |
| `resources.limits.nanoCPUs`, `resources.limits.memoryBytes` | `--limit-cpu`, `--limit-memory` |
| `resources.reservations.nanoCPUs`, `resources.reservations.memoryBytes` | `--reserve-cpu`, `--reserve-memory` |
| `env[NAME]` | `--env-add`, `--env-rm` |
| `constraints` | `--constraint-add`, `--constraint-rm` |
| `labels[NAME]` | `--label-add`, `--label-rm` |
| `ports[PUBLISHED:TARGET/PROTOCOL/MODE]` | `--publish-add`, `--publish-rm` |
| `endpointMode` | `--endpoint-mode` |
| `networks` | `--network-add`, `--network-rm` |
| `mounts[TARGET]` | `--mount-add`, `--mount-rm` |
| `secrets[NAME]`, `configs[NAME]` | `--secret-add`, `--secret-rm`, `--config-add`, `--config-rm` |
| `healthcheck`, `healthcheck.*` | `--health-*`, `--no-healthcheck` |
| `updateConfig.*`, `rollbackConfig.*` | `--update-*`, `--rollback-*` |
| `restartPolicy.*` | `--restart-*` |
| `placement.preferences`, `placement.maxReplicas` | `--placement-pref-add`, `--placement-pref-rm`, `--replicas-max-per-node` |
| `command`, `args` | `--entrypoint`, `--args` |
| `user`, `workingDir`, `hostname` | `--user`, `--workdir`, `--hostname` |
| `stopSignal`, `stopGracePeriod` | `--stop-signal`, `--stop-grace-period` |
| `readOnly`, `tty`, `init` | `--read-only`, `--tty`, `--init` |
| `hosts`, `groups` | `--host-add`, `--host-rm`, `--group-add`, `--group-rm` |
| `capabilityAdd`, `capabilityDrop` | `--cap-add`, `--cap-drop` |
| `sysctls[NAME]`, `ulimits[NAME]` | `--sysctl-add`, `--sysctl-rm`, `--ulimit-add`, `--ulimit-rm` |
| `dns`, `dns.*` | `--dns-add`, `--dns-search-add`, `--dns-option-add` and their `-rm` pairs |
| `logDriver`, `logDriver.*` | `--log-driver`, `--log-opt` |
| `containerLabels[NAME]` | `--container-label-add`, `--container-label-rm` |
| `isolation`, `oomScoreAdj`, `openStdin` | `--isolation`, `--oom-score-adj` |
| `resources.limits.pids` | `--limit-pids` |
| `resources.reservations.genericResources` | `--generic-resource-add`, `--generic-resource-rm` |
| `privileges.*` | `--credential-spec`, and the API for the rest |
| `maxConcurrent`, `totalCompletions` | `--max-concurrent`, `--replicas` on a job |

A published port is reported as a whole entry being `set` or `absent` rather than
as a value that changed, because a port has no stable key: two publishes can share
a target, a protocol and a mode and differ only in the published port. Moving one
therefore reads as one entry gone and one arrived — which is what `--publish-rm`
plus `--publish-add` did. A port the manifest published without naming one reads
as `dynamic`.

Attached **networks** are the one field that needs a second read: a service's spec
names them by the id the daemon assigned, where the manifest named a network, so
comparing them means naming the ids first. That costs one network listing per
application per interval. If it fails, or the swarm's backend cannot answer it,
attachments are not compared and everything else still is.

A **mount** is compared by what it mounts and where — its type, source, target and
read-only flag — and never by the options inside it. `BindOptions`, `VolumeOptions`
and `TmpfsOptions` are not comparable: the daemon drops a bind's whole options
struct when its propagation is unset, and `consistency` has no field in Swarm at
all, so a comparison would report a difference on a stack nobody has touched. None
of them can be changed without replacing the mount, which *is* reported.

A **secret or config reference** is compared by name and by where it lands. The
ids are not compared even though both sides carry a real one: a config's content is
hashed into its name, so a change to the content is already a manifest-level
difference, and comparing ids would report drift on a resource recreated with
identical content. `uid`, `gid` and `mode` are not compared either, because
`docker service update` cannot change one without removing and re-adding the
reference.

At most 20 differences are listed per service; beyond that the report carries a
count of how many more there were. A service whose every environment variable was
replaced would otherwise put an unbounded list into a payload served on every poll.

Plus two findings about the service rather than a field: a service the manifest
declares that is **missing** from the swarm, and one running under the stack's
namespace that the manifest does not declare (**unexpected**). An unexpected
service is never *converged* — a redeploy would leave it exactly where it is,
since applying deletes nothing. Removing it is a different switch:
[`syncPolicy.pruneResources`](#what-a-chart-stops-declaring), which deletes
only what the controller can prove it installed.

Environment **values are never reported** — only whether a variable is `set`,
`absent` or `changed`. What is running is whatever was set out of band, this is
served to anyone with read access, and an environment variable is exactly where
a credential would be.

The **healthcheck**, **update** and **rollback config** and **restart policy** are
each a struct a manifest may declare or leave out entirely. One present on exactly
one side is reported once, as `healthcheck` or `updateConfig` rather than as every
field inside it. Where both sides have one, the fields the manifest left unstated
are compared against the daemon's default for them rather than against nothing —
an update config that names only a parallelism is running with a failure action of
`pause` and an order of `stop-first`, and neither is a change anybody made.

`command` is compose's `entrypoint` and `args` is its `command` — the naming Swarm
uses, which is the opposite of what those field names suggest. `hosts`, `groups`
and the two capability lists are compared as **sets**: their order carries no
meaning, `--host-add` appends wherever it likes, and a chart template that
reorders one has not changed what runs. `init` reports `(unset)` where a manifest
stated nothing, kept apart from `false` because an unstated value means the
daemon's own default, which is configurable.

Two of these can only ever drift in one direction, because the compose v3.9 schema
cannot express them: `groups` has no `group_add`, and `dns.options` is never
filled by the conversion. A `--group-add` or `--dns-option-add` therefore always
shows up against an empty desired side, which is exactly the point.

`containerLabels` are the container's own, which are not the service's: two
different sets, two different flags, and only the container's reach the running
process. A **replicated job** counts with `maxConcurrent` and `totalCompletions`
where a service counts with `replicas`, and the mode branch is exclusive, so a job
is never reported as a replica change. Most of `privileges` has no
`docker service update` flag and can only be set through the API — which is the
reason to report it rather than a reason not to, since a no-new-privileges flag
cleared behind the controller's back is a security change nothing else here shows.

**What is left, and why.** The list above is now the whole of a `ServiceSpec` that
an out-of-band change can reach. What remains uncompared is uncompared for a
reason rather than for want of attention:

| | |
|---|---|
| `ForceUpdate`, `Runtime`, `Placement.Platforms` | the daemon's own bookkeeping, not desired state |
| `PluginSpec`, `NetworkAttachmentSpec` | other task runtimes; a service using one has no container spec at all |
| mount options, network aliases and driver options, a reference's `uid`/`gid`/`mode` | not independently mutable — changing one means replacing the thing that holds it, which *is* reported |
| `Endpoint` (as distinct from `EndpointSpec`) | runtime state: virtual IPs and the ports Swarm actually allocated |
| the service's own name | the key the two sides are matched on |

Networks, configs
and secrets are not compared as *resources* either — Swarm cannot update a
network in place, and configs and secrets are immutable with their content
hashed into the name, so a change to either is already a manifest-level
difference. What *is* reported for those three is the one finding that is not a
comparison at all: one the repository has stopped declaring, which
[`syncPolicy.pruneResources`](#what-a-chart-stops-declaring) will delete.

## Where the app set lives

Three modes. The default is the file mounted at deploy time, so an existing
deployment keeps working untouched; the other two make the set itself
GitOps-managed.

| Mode | Selected by | The set changes when |
|---|---|---|
| **static** | nothing — the default | you create a new Docker config and redeploy |
| **git** | `--appset-repo` | you commit to the tracked branch |
| **path** | `--appset-dir` | something else writes the file on a volume |

Which selector you pass *is* the mode; there is no separate mode flag to keep in
agreement with it. Passing both selectors, or either one alongside an explicit
`--config`, is refused at startup rather than resolved by a precedence rule — a
controller silently reconciling a set nobody pointed it at is worse than one that
will not start.

### static — a file mounted at deploy time

```bash
swarmcli-cd controller --config /etc/swarmcli-cd/applications.yaml
```

Delivered as a **Docker config**, which is immutable: changing it means creating
a new config object and updating the stack, which replaces the container. There
is nothing to hot-reload into and nothing that can change under the process, and
for an air-gapped or tightly-controlled deployment that is the feature. This is
what `stack.yml` does out of the box.

### git — the controller pulls the set itself

```bash
swarmcli-cd controller \
  --appset-repo https://github.com/your-org/apps.git \
  --appset-revision main \
  --appset-path apps/applications.yaml \
  --appset-interval 1m
```

The controller clones the repository — with the same `SWARMCLI_CD_GIT_*`
credential it uses for the applications, into its own directory under `--data` —
and re-reads the file every `--appset-interval`. A commit that adds an
application starts reconciling it; one that removes it stops; one that retunes it
swaps the spec without restarting anything else.

A layout and a copy-pasteable file are in
[`../examples/appset-repo/`](../examples/appset-repo/).

### path — something else keeps the set current

```bash
swarmcli-cd controller --appset-dir /var/lib/swarmcli-cd/appset
```

For a git-sync sidecar, or the Flux source-controller pattern: the controller
reads a directory and something else is responsible for what is in it.
`--appset-path` names the file within that directory and defaults to
`applications.yaml`.

**The writer must publish atomically** — write a temporary file and `rename` it
over the target. Rename is atomic, so a reader sees one whole version or the
other. A writer that rewrites the file in place can be read half-written; that
read fails to parse, which is safe, but a truncation that happens to still be
valid YAML is indistinguishable from a set you meant to shrink.

### What a bad commit does — nothing

In every mode the controller parses and **fully validates** before it swaps
anything in: apiVersion, unknown keys, duplicate names, every per-application
rule. On any failure — an unreachable repository, a missing file, malformed
YAML, a duplicate name — the last set that validated keeps running and the
reason is reported. The set is never partially applied.

`swarmcli-cd status` is where you see it:

```
Mode          git
Source        https://github.com/your-org/apps.git @ main (apps/applications.yaml)
Revision      a1b2c3d
Loaded        2026-07-27T09:12:04Z
Applications  4
Stale         yes — the running set is the last one that validated
Error         apps/applications.yaml@d4e5f6: applications[1]: duplicate application name "edge"
```

`Stale` means exactly that: what is running is last-good and a newer version is
being refused. Fix the commit and the next poll clears it. An `Error` with no
`Stale` line and `Applications 0` is the louder problem — nothing has ever
loaded, so nothing is running; see [startup, below](#startup-when-the-set-is-not-there-yet).

**An application removed from the set stops being reconciled; its stack stays
deployed.** It is reported as orphaned rather than deleted — pruning a
deployment because somebody edited a file is not something this controller does
by accident. [Prune](#prune) turns that into a deletion, deliberately.

### Catching it before the commit

Refusing a bad set is a safety net, not a workflow. The controller only finds
out once the commit is on the branch, and until somebody fixes it what is
running is a stale set. `swarmcli-cd validate` is the same parse and the same
rules, run against a file on disk:

```bash
swarmcli-cd validate --file applications.yaml
# OK: applications.yaml (4 applications)
```

It exits 0 on a valid file, 1 on an invalid one with the reason on stderr, and
2 on a misuse — so it is a one-line CI job on the app-set repository, run before
the branch the controller tracks ever moves. The image is the same binary, if
you would rather not install one:

```bash
docker run --rm -v "$PWD:/w" eldaratech/swarmcli-cd:latest validate --file /w/applications.yaml
```

It reads the file and nothing else. A set that validates can still fail to
deploy: whether a `releaseFile` or chart path exists in its repository, whether
a `destination` names a swarm this controller knows, and whether a
`registryAuth` secret is mounted are all answered at reconcile time, against
things this command cannot see.

### Prune

By default an application that leaves the app set leaves its stack behind,
running and unmanaged. Prune deletes it instead. It is off by default and turned
on controller-wide, because a departed application's spec is gone — there is
nothing left to carry a per-application setting:

```yaml
command: ["controller",
          "--appset-repo", "https://github.com/your-org/apps.git",
          "--appset-revision", "main",
          "--appset-path", "apps/applications.yaml",
          "--prune"]
```

| flag | effect |
|---|---|
| `--prune` | delete the services, networks, configs, secrets and release history of an application that has left the set |
| `--prune-volumes` | also delete its named volumes; requires `--prune` |
| `--controller-id` | this controller's identity, stamped on every release it installs (default `default`) |

#### Two controllers on one swarm

**If more than one swarmcli-cd runs against the same swarm, each must be given
its own `--controller-id`.** Every release carries the stamp
`cd/<controller>/<application>`, and the sweep only considers releases stamped
for itself. Two controllers sharing an id — including two both left on the
default — each see the other's applications as departed, and with `--prune` on
they delete each other's deployments. The controller cannot detect this from the
inside: it has no way to tell "my own releases from before a restart" from
"another controller using my id".

Changing the id later is safe but not free: releases stamped with the old one
stop being recognised, so they read as unmanaged and prune ignores them until
they are re-stamped. **Every release is redeployed once to correct its stamp**,
because the owner a release is planned under is part of what decides whether it
needs deploying — an automated application does that on its next pass, a manual
one when it is next asked. The redeploy hands Swarm the manifest it is already
running, so it writes a revision and moves the stamp without restarting tasks.
Until then the old stamps are stale but safe (see
[renaming an application](#renaming-an-application), below). A release belonging
to an application that was removed at the same time is never re-stamped, because
nothing plans it any more; that one has to be cleaned up by hand.

Volumes are a second opt-in because they are the one part nothing can restore.
Everything else prune deletes is recreated from git the moment the application
comes back; the data in a volume is not. `--prune-volumes` without `--prune` is
a startup error rather than a guess in either direction.

**`--prune-volumes` only reaches the node the controller talks to.** Docker's
`GET /volumes` answers from the *node-local* volume store — only CSI cluster
volumes come from swarm, and only on a manager — and an ordinary named volume is
created by whichever engine runs the task that mounts it. So on a swarm of more
than one node, this deletes the departed release's volumes on the controller's
node and cannot see, let alone remove, the ones its tasks left elsewhere. There
is no swarm-wide listing of named volumes to ask for instead.

The controller does not pretend otherwise. On a multi-node swarm every purge
logs what it actually removed and says the listing was node-local, with the
filter that finds the rest:

```
docker volume ls --filter label=com.docker.stack.namespace=<release>
```

run on each node. It does **not** stop and it does not hold the release records
back: a later sweep on the same node would read the same node-local listing and
be no better off, so refusing would only leave an application that can never
finish being pruned — for the great majority of releases, which declare no named
volume at all. **On a multi-node swarm, treat `--prune-volumes` as a
best-effort convenience and the log line as the real inventory.**

**With prune on, removing an application from the app set is an outage, not a
pause.** Deleting the entry to "park" something deletes the deployment.

#### Renaming an application

Renaming an application — changing its `name`, leaving its source and release
file alone — is a handover. The stack keeps running, keeps its volumes and keeps
its release history; the new name simply takes over reconciling it.

Two things make that work, and both are worth knowing about because they are
visible.

**The rename redeploys each of the application's releases once.** The stamp is
part of what a plan compares, so a release still carrying the old name's stamp
is not "unchanged" — it is deployed again under the new name, which writes one
revision and moves the stamp. Swarm is handed the manifest it is already
running, so no task is restarted. Between the rename and that first reconcile
the stamp is stale, and `swarmcli-cd status` may show a release owned by a name
that is no longer in the app set; a manual-policy application stays that way
until it is asked to sync. It is harmless either way, because prune does not go
by the stamp alone: it never deletes a release that an application still in the
set declares.

And the sweep waits. An application that has just joined the set has not fetched
its repository yet, so it has not yet said which releases it declares — and a
sweep that ran before it did would read the silence as a departure and delete
the stack the rename was handing over. Until every application has reconciled at
least once, nothing is pruned and `status` says what it is waiting for:

```
Prune held    waiting for edge-eu
```

That line is normal for a few seconds after a rename or a restart. If it
persists, an application cannot reconcile at all — a bad repository URL or an
unreadable chart — and prune stays held for the whole controller until it is
fixed or removed from the set. Waiting is deliberate: the alternative is
deleting a running stack on the strength of an application that has not managed
to speak yet.

A release an application stops declaring is a separate case, and is opted into
per application rather than controller-wide, because the spec is still there to
carry it:

```yaml
applications:
  - name: edge
    syncPolicy:
      automated: true
      prune: true           # delete releases this application no longer declares
      pruneVolumes: false   # and their volumes; requires prune
      pruneResources: true  # delete what its charts no longer declare
```

Not to be confused with `historyMax`, which trims a release's revision *history*
and never touches anything deployed.

#### What a chart stops declaring

`prune` above is about a whole release. A service, network, config or secret
**inside** a release that is still declared is a different case and has its own
switch, because applying deletes nothing: edit a chart to drop any of them and
the next sync deploys what is left and never mentions the one that went. It stays
on the swarm, and every later reconcile honestly reports the release unchanged,
because the rendered manifest really does match the last applied one.

`pruneResources` deletes it. It does **not** require `prune`: that decides what
happens to a release this application stopped declaring, this decides what
happens inside one it still declares, and wanting the second without the first
is a reasonable position rather than half a config.

One switch covers all four kinds because it is one statement — *this chart is
authoritative about what exists in its own release*. **Configs and secrets are
the ones that accumulate fastest.** They are immutable, so a content change has
to arrive as a new name; the applier refuses one and tells you to hash the
content into the name. A chart that does as it is told therefore strands its
previous copy on **every value change**, not only when you remove a declaration.

A resource is deleted only when the swarm, git and this controller's own records
all agree:

1. it carries the release's `com.docker.stack.namespace` label;
2. the freshly rendered manifest does not declare it;
3. a stored revision **stamped by this controller for this application**
   declared it;
4. for a config or a secret, **this controller created it**, rather than finding
   it already there.

The third clause is what makes the deletion safe, and note what it is not: the
namespace label. A stack is a name prefix plus that label and nothing more — no
`/stacks` endpoint, no owner references — and anything can carry it, so a sidecar
somebody attached by hand reads exactly like a service the controller installed.
A stored revision does not: this controller wrote it, it names the owner, and it
never changes afterwards. The label says where a resource lives; the record says
whose it is.

The fourth exists because a revision proves the repository asked for a *name*,
which is not the same as this controller having made the *object*. Applying a
chart that declares a config or secret which already exists **adopts** it — the
applier relabels it into the stack's namespace, exactly as `docker stack deploy`
does — and that label is what makes it sweepable. So the pre-seed pattern, an
operator running `docker secret create web_db_password` before a chart declares
`db_password`, would end with the controller deleting material it never held a
copy of. Configs and secrets it creates carry a `com.swarmcli.cd.created` label;
adopted ones never gain one, and it is carried across updates rather than
re-derived.

**It applies to those two kinds and not to networks, because only those two
adopt.** The applier leaves an existing network entirely alone rather than
relabelling it, so a network can only be a candidate if it already carried the
release's namespace label — which means either this controller created it or
somebody labelled it as that release's on purpose. There is no adoption to
catch, and requiring a marker there would instead stop the sweep touching every
network created before the label existed.

**For configs and secrets it is retroactive in the safe direction.** One created
before this controller learned to mark them has no marker and cannot gain one —
from here it is indistinguishable from one somebody else made — so it is
reported and never deleted, the same answer clause 3 gives a resource older than
the retained history. Since [#99](https://github.com/Eldara-Tech/swarmcli-cd/issues/99)
a chart cannot own either kind anyway — `file:` reads the controller's own
filesystem and is refused, and an `external:` declaration is a reference this
controller did not create — so in practice clause 4 is a guard against the day
that changes rather than a narrowing of what is swept today.

The four kinds are proved separately, so a config never inherits a same-named
service's evidence — Swarm scopes all four into one namespace of names, and
`web_app` can legitimately be both.

A resource failing the third clause is **reported and never deleted**. A service
shows as `unexpected` without the `(orphaned)` marker, which is also how one
older than the release's retained history reads — with the default `historyMax`
of 10 that means a resource declared only by a revision more than ten deploys
ago, and setting `historyMax: 0` removes even that case at the price of an
unbounded revision store.

It works in either drift mode. Removing something from a template is git moving,
not the swarm moving, so coupling it to `driftDetection: live` would make it a
no-op under the default. Enabling it costs four list calls per deployed release
per reconcile, and reads a release's history only when that release actually has
a candidate — one read answering all four kinds.

**A resource still in use is not deleted, and that is not an error.** Swarm
refuses to remove a config, secret or network that anything still references, and
one whose service was removed a moment ago is still referenced while its tasks
shut down. The refusal is logged and the next reconcile tries again; nothing is
lost by waiting, because the evidence is a stored revision that is still there.
A network with tasks still draining routinely takes a second pass.

**Some refusals never clear, and after three passes the controller stops
trying.** A chart that drops a network another stack is still attached to gets
the same refusal on every reconcile for ever, and it is indistinguishable from a
slow drain except by how long it has been going on. Retrying indefinitely is not
free: it costs a history walk, a deploy of an unchanged plan, an event and an
application permanently reporting out of sync, every interval, over something
that will never happen. So the resource is **left in place, warned about on
every pass that still finds it, and dropped from the sync axis** — the
application goes back to `synced`, because nothing it will do is outstanding.
Removing the resource is then yours:

```
WARN resources the release no longer declares could not be removed and will not
     be tried again; remove them by hand once whatever still references them is
     gone  application=edge release=whoami resources="[network whoami_gone]"
```

The count is per resource, is cleared by a removal that works, and is held in
memory — so restarting the controller is also how you ask it to try again after
clearing whatever was holding it.

Volumes are **left in place** and reported, not deleted, even with `pruneVolumes`
on. That flag means "when a whole release goes, its data goes with it"; something
leaving a template is a far more frequent event, and nothing here can prove
another stack does not mount the same volume. The external networks swarmcli
auto-created for a release are treated the same way, and resources a chart
declares as `external:` are never candidates at all — they are not ours to
create, so they are not ours to delete.

`swarmcli-cd app get` marks what a sync will remove, so a manual-policy
application shows it before anything acts on it:

```
KIND     NAME             REASON                  FIELD  DESIRED  LIVE
service  web_sidecar      unexpected (orphaned)
service  web_stranger     unexpected
config   web_conf-a1b2c3  unexpected (orphaned)
network  web_internal     unexpected (orphaned)
```

#### Ordering, and workloads that must never run twice

Releases are applied first and pruned afterwards, so a failed apply leaves the
old release running rather than having already deleted it. The cost is that
**renaming a release makes it briefly coexist with its old name**: the new name
is an install and the old one becomes an orphan, and between the two steps both
are deployed. If they collide on an ingress port or a network name the new
release will not converge until the next reconcile prunes the old one — that
clears itself and is not a failure to investigate.

For most workloads a moment of overlap is harmless and an outage is not, which
is why that is the default. For some it is the other way round: a blockchain
validator that double-signs is slashed, a job runner that starts twice processes
its queue twice. `pruneFirst` inverts the order for those — the departing
release is deleted before its replacement is installed, and a failed apply
leaves a gap instead of an overlap.

```yaml
    syncPolicy:
      automated: true
      prune: true
      pruneFirst: true   # never overlap; prefer a gap
```

**This bounds the overlap the controller creates deliberately. It is not a
distributed lock.** Nothing here can prevent two instances during a network
partition, a node rejoining with stale state, or a manual `docker service`
command. A workload that cannot tolerate two instances *at all* needs a guard
outside the orchestrator — for a validator, a remote signer with an
anti-slashing record, or a hardware signer that refuses a conflicting height and
round. What this controller can promise is "never deliberately runs two", not
"never runs two".

#### What prune will not do

Prune only ever deletes what this controller installed, identified by the owner
stamp on the release rather than by name. A release deployed with
`swarmcli charts apply`, one belonging to another application, or one another
tool created is unmanaged here and is never a candidate — the same distinction
`swarmcli charts` draws between orphaned and unmanaged releases.

Three further guards exist because the alternative is a controller that empties
a swarm over a transient failure:

- a pass whose app set failed to load prunes nothing — the last-good set keeps
  running and no departure is inferred from a file nobody could read;
- a pass whose apply failed prunes nothing, because the running state is not yet
  the declared one;
- an app set that declares **no applications at all** prunes nothing. An empty
  set and a truncated one are indistinguishable from here. To remove every
  application, remove them one commit at a time.

A controller that has never successfully loaded a set therefore prunes nothing,
indefinitely — which is the correct reading of "I have no idea what should be
running".

Networks that swarmcli auto-created for a release are **left in place** and
reported in the log, not deleted: they may be shared with another stack. The
same is true of anything prune could not remove — the failure is logged, the
release keeps its owner stamp, and the next sweep tries again.

One case `pruneResources` cannot defend against: a network a chart declares with
an explicit `name:`, which suppresses the namespace prefix but not the namespace
label. Two stacks can both declare the same name and both label it as theirs, so
dropping it from one chart makes it a candidate. Swarm refuses to remove a
network anything is attached to, which covers the case that would hurt; a shared
network with no live tasks on it is not covered.

### Startup, when the set is not there yet

A **static** set that cannot be read is fatal, as it always has been: the config
was not mounted, nothing will change under the controller's feet, and the restart
is your notification.

A **git** or **path** set that cannot be read at startup is not. Both are
produced by something else that may simply not be ready — a repository briefly
unreachable, a sidecar that has not finished its first sync — and exiting would
restart-loop the controller under Swarm and take down the API that could have
explained it. So the controller starts with **no applications**, logs the reason,
reports it on `swarmcli-cd status`, and retries every interval until the set
arrives.

The cost is worth stating plainly: a permanently wrong `--appset-repo` presents
as a healthy container reconciling nothing. `/healthz` stays a liveness probe —
tying it to the app set would have Swarm kill and restart the task, which is the
restart loop this avoids — so **`swarmcli-cd status` is the thing to alert on**,
not the healthcheck.

### The trust boundary

The app-set repository is **root-equivalent**. Whoever can commit to it defines
every application, destination and sync policy the controller applies with its
docker socket. Treat it exactly as you treat access to the swarm itself.

The bootstrap is the anchor that makes that boundary meaningful: it is the sole
authority on *which* repository, branch, path and credentials are authoritative,
and **nothing in the app set can repoint it**. There is no field in
`applications.yaml` that changes where `applications.yaml` comes from.

In the OSS build, protecting that repository is the deployment's responsibility:

- branch protection on the tracked branch, with review required;
- signed commits, if your forge can enforce them;
- a repository separate from the applications' own, so that being able to change
  an app's chart is not the same permission as being able to add an app.

Per-user identity and RBAC over who may change what are a licensed capability;
see [extensibility.md](extensibility.md).

One thing a commit **cannot** do: deliver a new secret. An application that names
a `registryAuth:` needs that Docker secret mounted into the controller in
`stack.yml`, so introducing a new private-registry credential is still a
deploy-time act. Adding an application that uses an already-mounted one is just a
commit.

### Moving an existing deployment to git

Nothing forces this move, and the static mode is not deprecated. If you want it:

1. **Commit the file you already have.** Put today's `applications.yaml` into a
   repository — its own, not an application's — say at `apps/applications.yaml`.
   The format is identical; nothing in the file changes.
2. **Point the controller at it.** In `stack.yml`, replace the `--config`
   argument with the git selectors:

   ```yaml
   command: ["controller",
             "--appset-repo", "https://github.com/your-org/apps.git",
             "--appset-revision", "main",
             "--appset-path", "apps/applications.yaml",
             "--appset-interval", "1m"]
   ```
3. **Drop the `configs:` block** from the service and the top-level `configs:`
   entry. Leaving it mounted is harmless but leaves a stale copy on disk that
   nothing reads.
4. **Keep the secrets.** The admin token and every `registryAuth` secret stay
   exactly as they are — see the note above.
5. **Redeploy once**, and check `swarmcli-cd status`: `Mode` should read `git`
   and `Revision` the commit you pushed. That is the last redeploy the app set
   costs you.

To go back, restore the `--config` argument and the `configs:` block. The two
modes read the same file, so there is nothing to convert in either direction.

## The controller

The controller takes flags; credentials come from the environment, because they
arrive as Docker secrets and a flag would put them in `docker service inspect`
output and in `argv`. The app-set selectors are flags rather than environment
variables for the same reason inverted: they are not secrets, and a `command:`
line that shows exactly what the controller is following is worth having in
`docker service inspect`.

### Flags

| Flag | Default | |
|---|---|---|
| `--config` | `/etc/swarmcli-cd/applications.yaml` | the applications file, delivered as a Docker config. Static mode |
| `--listen` | `:8080` | API listen address |
| `--data` | `/var/lib/swarmcli-cd` | repository clones and the chart cache, on a volume so a restart does not re-clone everything. An application that leaves the set has both reclaimed, one app-set interval after it goes |
| `--appset-repo` | — | pull the app set from this repository. Selects **git** mode |
| `--appset-revision` | — | branch, tag or SHA to track. Required with `--appset-repo`; an unpinned app set would follow whatever the default branch happens to point at |
| `--appset-dir` | — | read the app set from a directory something else keeps current. Selects **path** mode |
| `--appset-path` | `applications.yaml` in path mode | the set's path within the repository or the directory. Required with `--appset-repo` |
| `--appset-interval` | `3m` | how often the app set is re-read |
| `--prune` | off | delete the resources of an application that has left the app set instead of leaving its stack running and reporting it as orphaned. See [prune](#prune) |
| `--prune-volumes` | off | extend `--prune` to named volumes, the one part nothing can restore — and only on the controller's own node; see [prune](#prune). Requires `--prune` |
| `--controller-id` | `default` | this controller's identity, stamped on every release it installs. Two controllers on one swarm must be given different ones; see [two controllers on one swarm](#two-controllers-on-one-swarm) |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `--log-format` | `text` | `text` or `json` |

### Logs

Everything the controller writes goes to **stderr**, through one handler, in one
format — the reconcile events, the applier's per-resource lines and the seam
report at startup alike. `docker service logs swarmcli-cd_controller` is the
whole of it — including what the CE libraries the controller is built on write,
which goes through the same handler rather than a log file of their own.

`text` is [logfmt](https://brandur.org/logfmt), which is what an operator
reading those logs wants. It quotes any value containing a space and escapes the
quotes inside it, so an error that names things in quotes arrives looking like
this:

```
time=2026-07-28T13:42:30.788Z level=ERROR msg="reconcile failed" application=swarm-cronjob failures=2 error="planning: release \"swarm-cronjob\": chart \"swarm-cronjob\" version \"0.1.2\" not found"
```

That is the format working, not a fault: the backslashes are what keep the value
one field. `--log-format json` is the option for anything that parses rather
than reads — a log shipper will unescape it back.

### Environment

| Variable | |
|---|---|
| `SWARMCLI_CD_ADMIN_TOKEN_FILE` | API admin token, read from a file — the Docker-secret form |
| `SWARMCLI_CD_ADMIN_TOKEN` | API admin token, given directly |
| `SWARMCLI_CD_GIT_USERNAME` | git username; forges often ignore it, GitHub wants it non-empty (`x-access-token`) |
| `SWARMCLI_CD_GIT_TOKEN_FILE` | git password or token, read from a file — the Docker-secret form |
| `SWARMCLI_CD_GIT_TOKEN` | git password or token, given directly |
| `SWARMCLI_CD_SERVER` | *client only* — which controller the `app` commands talk to (default `http://127.0.0.1:8080`) |

The controller **refuses to start** with no admin token configured: an authorizer
that merely rejected every request would be indistinguishable, from the outside,
from a wrong token. The admin token never comes from a flag — a token in `argv`
is a token in `ps` and in the shell history.

For a public repository, the git variables are unnecessary. For a private one,
set `SWARMCLI_CD_GIT_USERNAME` and a token via `SWARMCLI_CD_GIT_TOKEN_FILE`. The
same credential is used for the app-set repository in git mode — one credential
for the controller, not one per repository.

## See also

- [Getting started](getting-started.md) — the end-to-end walkthrough
- [Concepts](concepts.md) — sync vs health, drift, ownership, chart compatibility
- [HTTP API](api.md#status--get-apiv1status) — the status endpoint behind `swarmcli-cd status`
- [Deploying](../README.md#deploying-it) and [`stack.yml`](../stack.yml)
