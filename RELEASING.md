<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Release process

Tagging is the whole trigger. `.github/workflows/release.yml` then drafts the
notes from PR labels, publishes binary archives with GoReleaser, and pushes a
multi-arch image to Docker Hub as `eldaratech/swarmcli-cd`.

## Prerequisites

The `docker` job needs two repository secrets, and they are not something the
tooling can set for itself:

| Secret | |
|---|---|
| `DOCKERHUB_USERNAME` | Docker Hub account with push access to `eldaratech` |
| `DOCKERHUB_TOKEN` | an access token for it, not the password |

Without them the release still produces a GitHub release and binaries; only the
image push fails.

## Before tagging

**Check what the chart engine will be stamped as.** The image and the binaries
both stamp `charts.engineVersion` with whatever swarmcli release `go.mod` pins,
read at build time so it cannot drift from what is compiled in:

```bash
go list -m -f '{{.Version}}' github.com/Eldara-Tech/swarmcli
```

A pseudo-version (`v1.13.0-rc4.0.20260722094010-8b65cf951c7e`) works — chart
compatibility resolves it to its core version, and there is a test for that —
but it says "this was built against a commit, not a release" in every
`swarmcli-cd version`. Prefer pinning a released tag before a GA.

## Tagging

```bash
git tag -a v0.1.0 -m "Release v0.1.0"
git push originToken v0.1.0
```

Release candidates are tagged the same way (`v0.1.0-rc1`). GoReleaser marks
them prerelease automatically, and `docker/metadata-action` will not move
`:latest` to a prerelease — so an rc cannot become what `stack.yml` pulls.

## Verifying

```bash
gh release view v0.1.0 --repo Eldara-Tech/swarmcli-cd

docker pull eldaratech/swarmcli-cd:0.1.0
docker run --rm eldaratech/swarmcli-cd:0.1.0 version
# swarmcli-cd v0.1.0 (chart engine v1.13.0)

# Both architectures are in the manifest:
docker buildx imagetools inspect eldaratech/swarmcli-cd:0.1.0
```

An `unstamped` chart engine in that output means the ldflag did not take, and
every chart declaring a `swarmcliVersion` floor would be deployed unchecked —
treat it as a failed release rather than a cosmetic problem.

## Locally, without publishing

```bash
goreleaser release --snapshot --clean
docker build -t swarmcli-cd:dev .
```
