<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Configuration

swarmcli-cd is configured by two things and nothing else: the **applications
file**, which lists what to reconcile, and a handful of **environment variables**
for the listen address, the admin token and git credentials. Everything an
operator edits about the one thing they think about — which applications exist —
lives in the file; everything about the process lives in the environment.

A worked, copy-pasteable file is in [`../examples/applications.yaml`](../examples/applications.yaml).

## The applications file

Declared in a file rather than a database on purpose: a GitOps controller whose
own desired state lived in mutable storage would have a bootstrap problem. In
Phase 1 the file is the only source of truth and the API serves it read-only.

It is delivered to the controller as a **Docker config**, which is immutable —
so changing it means creating a new config object and updating the stack, which
restarts the container. There is no hot reload because the process that would
notice a change does not outlive it. Restart is not a limitation here; it is the
only thing that can happen.

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
committed to your repo, so bumping one is an ordinary git commit — the controller
fetches it on the next reconcile, with no redeploy (Renovate can even open the
PR). A `chart` source keeps the version here in `applications.yaml`, which is
delivered as an immutable Docker config, so changing it means creating a new
config and redeploying the controller. A chart whose version changes often
therefore belongs in a release file; the `chart` source is the convenience form
for a single, rarely-changing chart.

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

## The controller

The controller takes a few flags; credentials come from the environment, because
they arrive as Docker secrets and a flag would put them in `docker service
inspect` output and in `argv`.

### Flags

| Flag | Default | |
|---|---|---|
| `--config` | `/etc/swarmcli-cd/applications.yaml` | the applications file, delivered as a Docker config |
| `--listen` | `:8080` | API listen address |
| `--data` | `/var/lib/swarmcli-cd` | repository clones and the chart cache, on a volume so a restart does not re-clone everything |

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
set `SWARMCLI_CD_GIT_USERNAME` and a token via `SWARMCLI_CD_GIT_TOKEN_FILE`.

## See also

- [Getting started](getting-started.md) — the end-to-end walkthrough
- [Concepts](concepts.md) — sync vs health, drift, ownership, chart compatibility
- [Deploying](../README.md#deploying-it) and [`stack.yml`](../stack.yml)
