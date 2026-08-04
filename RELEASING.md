<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Release process

Tagging is the whole trigger. `.github/workflows/release.yml` then drafts the
notes from PR labels, publishes binary archives with GoReleaser, and pushes a
multi-arch image to Docker Hub.

**This repository publishes the `-oss` half of a release, not the whole of it.**
Each release carries two artefacts (D21, [docs/editions.md](docs/editions.md)):

| Artefact | Tagged in | Publishes |
|---|---|---|
| `swarmcli-cd` — archives, `eldaratech/swarmcli-cd:<v>`, `:latest` | the private `swarmcli-cd-be` wrapper | into **this** repository's releases, under `RELEASE_TOKEN` |
| `swarmcli-cd-oss` — archives, `eldaratech/swarmcli-cd:<v>-oss` | here | into this repository's releases |

So a release needs **two tags, one in each repository, carrying the same version
string** — and the public one first.

- There is no trigger from the public tag. The PAT cannot dispatch a workflow,
  so nothing starts the private pipeline but a tag pushed there by hand;
  assuming otherwise produces a release that silently never built its default
  artefact.
- The order is not a preference. GoReleaser publishes into a release *named
  after the tag it was invoked with*, so the public tag creating the release
  first is what gives the private run something to add to. A private `v1.1.1`
  against a public `v1.1.0` creates a second release and asks GitHub to invent a
  tag to hang it from.
- Nothing collides, and that is by construction rather than by luck: the
  archives here are `swarmcli-cd-oss_*`, the checksum file is
  `checksums-oss.txt`, and the image tags carry a `-oss` suffix. The merged
  pipeline writes `swarmcli-cd_*`, `checksums-merged.txt` and the unsuffixed
  image tags, and it refuses to run if this repository at the pinned tag has
  gone back to the plain archive names.

**A tag pushed here on its own no longer moves `:latest`.** `latest=false` in
the docker job: under D20 `:latest` is the merged artefact, which is what
`stack.yml` pulls, and two pipelines competing for that tag would leave an
operator with whichever finished last. A public-only tag therefore publishes the
OSS archives and the `-oss` image tags and leaves `:latest` where it was.

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

docker pull eldaratech/swarmcli-cd:0.1.0-oss
docker run --rm eldaratech/swarmcli-cd:0.1.0-oss version
# swarmcli-cd v0.1.0 (chart engine v1.13.0)

# Both architectures are in the manifest:
docker buildx imagetools inspect eldaratech/swarmcli-cd:0.1.0-oss
```

`version` prints the same string for both artefacts — one tag produces both, so
it cannot tell them apart. The `seams` line in the controller's startup log can:
`feature=community` is this build. See [docs/editions.md](docs/editions.md).

An `unstamped` chart engine in that output means the ldflag did not take, and
every chart declaring a `swarmcliVersion` floor would be deployed unchecked —
treat it as a failed release rather than a cosmetic problem.

**Check that an archived binary serves the UI**, which the image job proves for
itself and the archives do not. GoReleaser's `before.hooks` build the bundle
`//go:embed` compiles in; if they are ever dropped, `go build` still succeeds
against the committed sentinel and every archive ships the "built without its
web UI" page. Nothing else about such a release looks wrong:

```bash
SWARMCLI_CD_ADMIN_TOKEN=x ./swarmcli-cd controller --config applications.yaml &
curl -s localhost:8080/ | grep -q 'id="root"' || echo "this archive has no UI"
```

A binary out of an archive prints the same release **without** the leading `v`:
GoReleaser stamps `{{.Version}}`, which is the tag with its `v` stripped, while
the image is built with `github.ref_name` verbatim. So `swarmcli-cd version`
says `v0.1.0` from the image and `0.1.0` from an archive — one tag, two
renderings, not a sign that the wrong thing was built.

## Locally, without publishing

Needs Node as well as Go: the `before.hooks` run `npm --prefix web/ui ci` and
`npm --prefix web/ui run build`, and `ENGINE_VERSION` is what the ldflag reads.

```bash
export ENGINE_VERSION=$(go list -m -f '{{.Version}}' github.com/Eldara-Tech/swarmcli)
goreleaser release --snapshot --clean
docker build -t swarmcli-cd:dev .
```
