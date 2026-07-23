<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# swarmcli-cd documentation

GitOps continuous delivery for Docker Swarm — reconcile your swarm from git, the
way Argo CD does for Kubernetes. Start with the [README](../README.md) for what
it is and why; the pages here are how to use it.

## For operators

- **[Getting started](getting-started.md)** — from a git repository to a service
  running on a real swarm, end to end.
- **[Configuration](configuration.md)** — every field of the applications file,
  plus the controller's flags and environment.
- **[Concepts](concepts.md)** — sync versus health, drift, ownership, rollback,
  chart compatibility, and why the applier is not `docker stack deploy`.
- **[HTTP API](api.md)** — the endpoints behind every command, for scripting and
  for building on.

## Examples

- **[`../examples/applications.yaml`](../examples/applications.yaml)** — a
  commented applications file showing both source types.
- **[`../examples/quickstart-repo/`](../examples/quickstart-repo/)** — a minimal,
  ready-to-push repository the getting-started guide deploys.

## For contributors

- **[Extensibility](extensibility.md)** — the open-core seams and how the private
  companion replaces one.
- **[RELEASING.md](../RELEASING.md)** — tagging, the image, and the engine-version
  stamp.
- **[Issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1)** — positioning,
  decisions D1–D6, and the phase plan. The source of truth for the design.
