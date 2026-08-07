# SwarmCLI CD

GitOps continuous delivery for Docker Swarm — reconcile your swarm from Git, the
way Argo CD does for Kubernetes.

> **Status: stable, and shipping.** The pull loop works end to end — fetch,
> render, plan, diff, apply, prune, drift detection and health — and is
> exercised against a real swarm by the integration tests. `driftDetection:
> live` has landed: a `docker service update` made behind the controller's back
> is seen, and on an automated application corrected. So have app-of-apps, which
> puts the application set itself in git, and sync waves, which order the
> releases within an application. **The web UI shipped in `v1.1.0`**, served
> from the same binary and the same port as the API, and with it the first
> licensed capability — [single sign-on](docs/sso.md), compiled into the default
> artefact and inert until a licence verifies.
> [Webhook triggers](https://github.com/Eldara-Tech/swarmcli-cd/issues/65) are
> what Phase 2 still wants. Pin the tag you deploy rather than tracking
> `:latest`, so that upgrading is a moment you chose. The design, decisions and
> phase plan live in
> [issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1).
>
> **New here?** Start with the [getting-started guide](docs/getting-started.md).

## Why

[SwarmCLI](https://github.com/Eldara-Tech/swarmcli) already ships the hard half:
`swarmcli charts` is a Helm-analogue for Swarm — templated packages, values
schemas, repository indexes with digest verification, dependency pre-flight, and
revision history stored in Swarm's own Raft store. `swarmcli charts apply`
converges a swarm to a file you commit.

What is missing is the *pull* half: something that watches Git, reconciles
continuously, detects drift, prunes what left the repo, and shows what is
actually running versus what should be. Today that gap is filled by CI running
`charts apply` after a merge — which means CI holds cluster write credentials and
nothing corrects drift between deploys.

## What makes this different

GitOps for Swarm is not greenfield — see the survey in
[#1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1). Several tools deploy a
compose file from a Git repo, and do it well. The gaps nobody has closed for
Swarm are:

- a real diff between the compose-derived desired `ServiceSpec` and the live one,
  **shown** before it is applied
- sync and health status **per stack and per service**
- pruning **networks, configs and secrets** — not just services
- automatic rollback on failed convergence, using Swarm's own
  `update_config.failure_action: rollback` and `PreviousSpec`, which the platform
  gives away for free and every existing tool ignores
- charts as a first-class source, with the revision history and rollback that
  already exist

## Installing

Released binaries (Linux and macOS, amd64 and arm64) are attached to each
[release](https://github.com/Eldara-Tech/swarmcli-cd/releases); the controller
image is `eldaratech/swarmcli-cd`. The same binary is both the controller and
its client, so a laptop needs only the archive.

Each release carries **two** artefacts. `swarmcli-cd` is the default: this
repository plus licensed code that does nothing until a licence verifies.
`swarmcli-cd-oss` archives and `eldaratech/swarmcli-cd:<version>-oss` are built
from this repository alone and are wholly Apache-2.0. The command inside both is
`swarmcli-cd` and everything below applies to either.
[docs/editions.md](docs/editions.md) is what each contains and how to tell which
one you are running.

Building it instead:

```bash
npm --prefix web/ui ci && npm --prefix web/ui run build
go build -o swarmcli-cd ./cmd/swarmcli-cd
./swarmcli-cd version
```

Requires Go 1.26+, and Node for the UI. A plain `go build` leaves the
chart-engine version unstamped, which makes every chart compatibility check
report Unknown — fine for development, not for anything that deploys. See
[RELEASING.md](RELEASING.md).

Without the first line — and so with `go install …@latest` too — the build
succeeds and the binary serves the API normally, but every browser gets a page
saying it was built without its web UI. That is what keeps this repository
buildable with no JavaScript toolchain; it also means `go install` is not a way
to get the UI. The archives and the images have it compiled in.

## Using it

One binary runs the controller and talks to it. The controller reconciles and
serves the API; every command that inspects it is a client of that API, so
anything the CLI can show, the [web UI](docs/web-ui.md) shows through the same
endpoints.

```bash
# In the swarm, on a manager node, with docker.sock mounted:
export SWARMCLI_CD_ADMIN_TOKEN_FILE=/run/secrets/swarmcli-cd-token
swarmcli-cd controller --config /etc/swarmcli-cd/applications.yaml

# From anywhere that can reach it:
export SWARMCLI_CD_SERVER=http://controller:8080
export SWARMCLI_CD_ADMIN_TOKEN=...

swarmcli-cd app list                 # sync state and health, one row each
swarmcli-cd app get edge             # releases and their services
swarmcli-cd app diff edge            # what a sync would change
swarmcli-cd app history edge         # each release's revisions
swarmcli-cd app sync edge --wait     # reconcile now; non-zero if it failed
swarmcli-cd status                   # the controller itself: where its app set comes from

# No controller, no swarm — this one reads a file:
swarmcli-cd validate --file applications.yaml   # non-zero if the set is invalid
```

Add `-o json` to any read for the controller's own response, unmodified — that
is the form to script against. The admin token never comes from a flag: a token
in argv is a token in `ps` and in the shell history.

Run `swarmcli-cd controller --help` or `swarmcli-cd app help` for the rest.

## Documentation

- [Getting started](docs/getting-started.md) — from a git repository to a running
  service, end to end.
- [Configuration](docs/configuration.md) — every field of the applications file,
  plus the controller's flags and environment.
- [Concepts](docs/concepts.md) — sync versus health, drift, ownership, rollback,
  chart compatibility.
- [Running it in production](docs/operations.md) — upgrading, restarting, what
  to back up, and what to alert on.
- [Web UI](docs/web-ui.md) — reaching it, and what it means that a browser holds
  the admin token.
- [HTTP API](docs/api.md) — the endpoints behind every command.
- [Editions](docs/editions.md) — the two artefacts every release publishes, and
  which one you are running. [Single sign-on](docs/sso.md) is the licensed
  capability that ships today.
- [Extensibility](docs/extensibility.md) — the open-core seams, for contributors.
- Examples: a commented [`applications.yaml`](examples/applications.yaml), a
  ready-to-push [quickstart repository](examples/quickstart-repo/), and an
  [app-set repository](examples/appset-repo/) for the git-sourced layout.

## Deploying it

Per D2 the controller runs **in the swarm, on a manager node**, and reaches the
daemon through the mounted socket. The image carries no docker binary: the
applier is built on the moby client rather than shelling out to `docker stack
deploy`, which is also why it can diff, prune and roll back things that command
cannot.

```bash
# Start from examples/applications.yaml and edit it for your repositories.
docker config create swarmcli-cd-applications ./applications.yaml
printf '%s' "$(openssl rand -hex 32)" | docker secret create swarmcli-cd-token -
docker stack deploy -c stack.yml swarmcli-cd
```

Both a config and a secret are immutable in Swarm, so changing either means
creating a new one and updating `stack.yml`. That is right for the controller's
bootstrap, which rarely changes — and optional for the application set, which
can live in git instead:

```bash
swarmcli-cd controller \
  --appset-repo https://github.com/your-org/apps.git \
  --appset-revision main \
  --appset-path apps/applications.yaml
```

Then adding, removing or retuning an application is a commit rather than a
redeploy; a commit that does not validate leaves the running set untouched and
is reported by `swarmcli-cd status`. The mounted-config mode stays the default
and the air-gap fallback. See
[configuration § where the app set lives](docs/configuration.md#where-the-app-set-lives).

`stack.yml` **does not publish the API port.** The controller holds
root-equivalent access to the swarm behind one shared bearer token over
plaintext HTTP, so publishing it on a node with a public address puts the swarm
on the internet. Reach it from inside the swarm, or tunnel:

```bash
ssh -L 8080:127.0.0.1:8080 manager
```

### Configuration

The tables below are the quick reference; [docs/configuration.md](docs/configuration.md)
is the full one, including every field of the applications file.

The controller takes flags; credentials come from the environment, because they
arrive as Docker secrets and a flag would put them in `docker service inspect`
output and in argv.

| Flag | Default | |
|---|---|---|
| `--config` | `/etc/swarmcli-cd/applications.yaml` | the applications file, delivered as a Docker config |
| `--listen` | `:8080` | API listen address |
| `--data` | `/var/lib/swarmcli-cd` | repository clones and the chart cache, on a volume |
| `--ui` | on | serve the embedded web UI at `/`; `--ui=false` answers its routes with `404` |
| `--appset-repo` `--appset-revision` `--appset-path` | — | source the application set from a git repository instead |
| `--appset-dir` | — | source it from a directory a git-sync sidecar keeps current |
| `--appset-interval` | `3m` | how often the application set is re-read |
| `--prune` | off | delete the resources of an application that has left the application set, instead of leaving the stack running and reporting it as orphaned |
| `--prune-volumes` | off | extend `--prune` to named volumes, the one part nothing can restore; requires `--prune` |
| `--controller-id` | `default` | this controller's identity, stamped on every release it installs. Two controllers on one swarm **must** differ, or each treats the other's applications as departed |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `--log-format` | `text` | `text` or `json`; everything goes to stderr through one handler either way |

| Environment | |
|---|---|
| `SWARMCLI_CD_ADMIN_TOKEN_FILE` | API admin token, read from a file — the Docker-secret form |
| `SWARMCLI_CD_ADMIN_TOKEN` | API admin token, given directly |
| `SWARMCLI_CD_GIT_USERNAME` | git username; forges usually ignore it, GitHub wants it non-empty |
| `SWARMCLI_CD_GIT_TOKEN_FILE` | git password or token, read from a file |
| `SWARMCLI_CD_GIT_TOKEN` | git password or token, given directly |
| `SWARMCLI_CD_SERVER` | for the client commands: which controller to talk to |

The controller **refuses to start** when no admin token is configured. An
authorizer that merely rejected every request would be indistinguishable, from
the outside, from a wrong token.

### Chart compatibility

A chart may declare the engine it needs (`swarmcliVersion: ">= 1.13.0"` in
`Chart.yaml`). The controller **refuses to apply** a plan containing a release
this build's chart engine is too old for, and records why on the application's
status — releases that would be unchanged are exempt, since applying will not
touch them. There is no operator to ask, and the alternative is a failure
minutes later inside the render, naming whatever feature happened to be missing.

The engine version is stamped into the image from the swarmcli release this
module pins. A plain `go build` leaves it empty, and every compatibility check
then reports Unknown rather than blocking.

## Contributing

Bug reports, features and patches are welcome — [CONTRIBUTING.md](CONTRIBUTING.md)
is what a change has to satisfy, and
[docs/extensibility.md](docs/extensibility.md) is how the open-core seams fit
together. Read [issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1)
before proposing anything architectural.

## Licence

Apache-2.0. See [LICENSE](LICENSE).

Everything in this repository — including the web UI — stays Apache-2.0, and the
whole product is here: reconcile, diff, drift, health, prune, the API, the CLI
and the browser. What a licence adds is built in a separate private companion
and reaches you inside the default artefact, inert until it verifies.
[Single sign-on](docs/sso.md) is the first of those and ships today; multi-swarm,
projects/RBAC, notifications and managed secret rotation are planned.
[docs/editions.md](docs/editions.md) is what each artefact contains.

## Security

Please do not report vulnerabilities via public issues. See
[SECURITY.md](SECURITY.md).

## Related

- [swarmcli](https://github.com/Eldara-Tech/swarmcli) — the TUI and chart engine (Apache-2.0)
- [swarmcli-charts](https://github.com/Eldara-Tech/swarmcli-charts) — community charts
- [swarmcli-rbac-proxy](https://github.com/Eldara-Tech/swarmcli-rbac-proxy) — mTLS + RBAC in front of the Docker API
