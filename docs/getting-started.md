<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Getting started

This walks a single application from a git repository to a service running on a
real swarm, reconciled continuously. It takes about ten minutes and leaves you
with a controller you can point at your own repositories.

## What you need

- A **Docker Swarm** you can reach a manager of. `docker swarm init` on a single
  host is enough.
- A **git repository** the controller can clone. The controller runs inside the
  swarm, so this must be a URL it can reach — a GitHub/GitLab repo, or anything
  else go-git accepts. A folder on your laptop will not do.
- The **swarmcli-cd binary** locally, to talk to the controller. Grab it from a
  [release](https://github.com/Eldara-Tech/swarmcli-cd/releases), or build it
  (`go build -o swarmcli-cd ./cmd/swarmcli-cd`).

## 1. Put a desired state in git

The repository holds what *should* be running. The quickstart repo in
[`../examples/quickstart-repo/`](../examples/quickstart-repo/) is a complete,
minimal one: a release file and a one-service chart.

```
swarmcli-release.yaml
charts/whoami/
  Chart.yaml
  values.yaml
  templates/stack.yaml
```

Copy those files into a repository of your own and push it:

```bash
cp -r examples/quickstart-repo/* /path/to/your/new/repo/
cd /path/to/your/new/repo
git init && git add . && git commit -m "quickstart"
git remote add origin https://github.com/your-org/quickstart.git
git push -u origin main
```

The release file names one release, `whoami`, from the local chart beside it:

```yaml
apiVersion: v1
releases:
  - name: whoami
    chart: ./charts/whoami
```

## 2. Declare the application

The controller reconciles an **applications file** — a list of applications, each
pointing at a repository. Create `applications.yaml`:

```yaml
apiVersion: v1
applications:
  - name: quickstart
    source:
      repoURL: https://github.com/your-org/quickstart.git
      revision: main
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      automated: true
      interval: 1m
      wait: true
      timeout: 5m
```

`automated: true` means the controller reconciles and applies on its own, every
minute. Every field is explained in the [configuration reference](configuration.md).

## 3. Deploy the controller

The controller runs **in the swarm, on a manager node**, and reaches the daemon
through the mounted socket. Its two inputs — the applications file and an admin
token — are delivered as a Docker config and a Docker secret, both immutable.

```bash
# The applications file, as a config:
docker config create swarmcli-cd-applications ./applications.yaml

# A random admin token, as a secret:
printf '%s' "$(openssl rand -hex 32)" | docker secret create swarmcli-cd-token -

# The stack. Copy stack.yml from the repository root.
docker stack deploy -c stack.yml swarmcli-cd
```

For a **private** repository, also create a git-token secret and uncomment the
`SWARMCLI_CD_GIT_*` lines in `stack.yml`:

```bash
printf '%s' "$YOUR_GIT_TOKEN" | docker secret create swarmcli-cd-git-token -
```

Confirm the controller is up:

```bash
docker service logs swarmcli-cd_controller
# A "seams" line reports which implementations loaded — for the OSS build,
#   msg=seams swarms=local authz=token notify=[log] secrets=plaintext
# followed by a "starting" line with the application count and listen address.
```

## 4. Reach the API

`stack.yml` **does not publish the API port** — the controller holds
root-equivalent access to the swarm behind one shared bearer token over plaintext
HTTP, so exposing it on a public node would put the swarm on the internet. Reach
it from inside the swarm, or tunnel over SSH:

```bash
ssh -L 8080:127.0.0.1:8080 your-manager-host
```

Then point the client at it and give it the token you generated:

```bash
export SWARMCLI_CD_SERVER=http://127.0.0.1:8080
export SWARMCLI_CD_ADMIN_TOKEN=<the token from step 3>
```

The token is read from the environment, never a flag: a token in `argv` is a
token in `ps` and in the shell history. For a file instead, set
`SWARMCLI_CD_ADMIN_TOKEN_FILE`.

## 5. Watch it reconcile

```bash
swarmcli-cd app list
# NAME        SYNC     HEALTH    SERVICES   REVISION
# quickstart  synced   healthy   1/1        a1b2c3d
```

Within a minute the application should be `synced` and `healthy`. Look closer:

```bash
swarmcli-cd app get quickstart      # releases and their services
swarmcli-cd app history quickstart  # each release's revisions
```

And confirm on the swarm itself:

```bash
docker service ls --filter label=com.swarmcli.release=whoami
```

## 6. Change something, and watch it converge

Edit the desired state and push:

```bash
# bump replicas: 1 -> 3 in charts/whoami/values.yaml
git commit -am "scale whoami to 3" && git push
```

On the next reconcile the controller notices, and — because this application is
automated — applies it. To see the plan before it does, or on a manual
application, diff and sync by hand:

```bash
swarmcli-cd app diff quickstart        # what a sync would change
swarmcli-cd app sync quickstart --wait # reconcile now; non-zero exit if it failed
```

`app sync --wait` blocks until the rollout converges and exits non-zero if it
did not — which is what makes it usable in a script or a smoke test.

## Where to go next

- **[Configuration reference](configuration.md)** — every field of the
  applications file, plus the controller's flags and environment.
- **[Concepts](concepts.md)** — sync versus health, drift, ownership, and why
  the applier is not `docker stack deploy`.
- **[HTTP API](api.md)** — the endpoints behind every command, for scripting and
  for building on.
- Add `-o json` to any read command for the controller's own response,
  unmodified — the form to script against.
