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

The application's identity. It becomes a URL path segment and half of the owner
stamp `cd/<name>`, so the charset is narrow: lowercase letters, digits, dot,
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
The controller passes its own owner, `cd/<name>`, so the file's `owner:` is
ignored. See [concepts § ownership](concepts.md#ownership).

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
`docker service inspect`. Nor can a reconciled stack reach it the other way: the
controller refuses to deploy a stack that mounts one of its own secrets (this
credential, the admin token, or the git token) as an `external` secret.

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
| `historyMax` | engine default | revisions kept per release (one Docker config each); older revisions are pruned |
| `prune` | `false` | delete the resources of a release this application no longer declares. Only ever its own releases — see [prune](#prune) |
| `pruneVolumes` | `false` | extend `prune` to the named volumes of what it deletes. Requires `prune`; set alone it is a config error |

(The `swarmcli-cd app sync --wait` *client* command has its own, separate
`--timeout`, defaulting to 5m — that bounds how long the CLI watches, not the
apply itself.)

Releases are applied in the order the release file lists them; use `wait: true`
if a later release needs an earlier one live first.

### `driftDetection` (optional)

```yaml
driftDetection: manifest
```

How drift is decided. Phase 1 has one mode, `manifest`: the rendered manifest is
compared against what was last applied. Omitting it defaults to `manifest`.
Comparing the desired `ServiceSpec` against the live one (`live`) is Phase 2 and
this build rejects it. See [concepts § drift](concepts.md#drift-sync-and-health).

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

Volumes are a second opt-in because they are the one part nothing can restore.
Everything else prune deletes is recreated from git the moment the application
comes back; the data in a volume is not. `--prune-volumes` without `--prune` is
a startup error rather than a guess in either direction.

**With prune on, removing an application from the app set is an outage, not a
pause.** Deleting the entry to "park" something deletes the deployment.

A release an application stops declaring is a separate case, and is opted into
per application rather than controller-wide, because the spec is still there to
carry it:

```yaml
applications:
  - name: edge
    syncPolicy:
      automated: true
      prune: true          # delete releases this application no longer declares
      pruneVolumes: false  # and their volumes; requires prune
```

Not to be confused with `historyMax`, which trims a release's revision *history*
and never touches anything deployed.

Releases are applied first and pruned afterwards, so a failed apply leaves the
old release running rather than having already deleted it. The cost is that
**renaming a release makes it briefly coexist with its old name**: the new name
is an install and the old one becomes an orphan, and if the two collide on an
ingress port or a network name the new release will not converge until the next
reconcile prunes the old one. It clears itself; it is not a failure to
investigate.

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
| `--data` | `/var/lib/swarmcli-cd` | repository clones and the chart cache, on a volume so a restart does not re-clone everything |
| `--appset-repo` | — | pull the app set from this repository. Selects **git** mode |
| `--appset-revision` | — | branch, tag or SHA to track. Required with `--appset-repo`; an unpinned app set would follow whatever the default branch happens to point at |
| `--appset-dir` | — | read the app set from a directory something else keeps current. Selects **path** mode |
| `--appset-path` | `applications.yaml` in path mode | the set's path within the repository or the directory. Required with `--appset-repo` |
| `--appset-interval` | `3m` | how often the app set is re-read |

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
