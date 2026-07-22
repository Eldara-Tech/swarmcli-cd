# SwarmCLI CD

GitOps continuous delivery for Docker Swarm — reconcile your swarm from Git, the
way Argo CD does for Kubernetes.

> **Status: scaffold.** The reconcile loop is not implemented yet. The design,
> decisions and phase plan live in [issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1).
> Nothing here is usable in production, and there is no release.

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

## Building

```bash
go build -o swarmcli-cd ./cmd/swarmcli-cd
./swarmcli-cd version
./swarmcli-cd controller --help
```

Requires Go 1.26+.

## Licence

Apache-2.0. See [LICENSE](LICENSE).

Multi-swarm, projects/RBAC, SSO, notifications and managed secret rotation are
planned as licensed capabilities in a separate private companion; everything in
this repository — including the web UI — stays Apache-2.0.

## Security

Please do not report vulnerabilities via public issues. See
[SECURITY.md](SECURITY.md).

## Related

- [swarmcli](https://github.com/Eldara-Tech/swarmcli) — the TUI and chart engine (Apache-2.0)
- [swarmcli-charts](https://github.com/Eldara-Tech/swarmcli-charts) — community charts
- [swarmcli-rbac-proxy](https://github.com/Eldara-Tech/swarmcli-rbac-proxy) — mTLS + RBAC in front of the Docker API
