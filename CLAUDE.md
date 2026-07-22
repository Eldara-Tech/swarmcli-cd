# swarmcli-cd

GitOps controller for Docker Swarm — "Argo CD for Swarm". Public, Apache-2.0.
The entry point is the top-level `controller` package; `cmd/swarmcli-cd` is a
one-line `main` so that the private companion can build the same binary from a
main.go differing only by its blank imports.

**Read [issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1) before
designing anything here.** It is the source of truth for positioning, decisions
D1–D6, the Swarm constraints that shape the design, and the phase plan. Do not
re-derive them.

## Quick reference

```bash
go build -o swarmcli-cd ./cmd/swarmcli-cd
go test ./...
golangci-lint run ./...
./scripts/check-spdx.sh          # CI enforces this on every .go and .sh
```

## Conventions

- **Module path** is `github.com/Eldara-Tech/swarmcli-cd`. It depends on CE as a
  normal versioned module (`require github.com/Eldara-Tech/swarmcli v1.13.0-rc2`)
  — **no `replace`, no sibling checkout**. That is deliberate: swarmcli-be pays
  the sibling-checkout tax, and renaming CE's module (Eldara-Tech/swarmcli#476)
  existed precisely so this repo would not have to.
- **SPDX header** on every `.go` and `.sh` file, or `licence.yml` fails:
  ```go
  // SPDX-License-Identifier: Apache-2.0
  // Copyright © 2026 Eldara Tech
  ```
  `goheader` in `.golangci.yml` enforces the same thing at lint time.
- **PRs need one label from each of A/B/C** (`check_labels.yml`), mirroring the
  other code repos. Add all three in **one** REST call:
  ```
  gh api -X POST repos/Eldara-Tech/swarmcli-cd/issues/<n>/labels \
    -f 'labels[]=A3-technical' -f 'labels[]=B0-low-priority' -f 'labels[]=C0-breaks-nothing'
  ```
  Even so, an early run can evaluate an incomplete label set and **fail** (not
  merely get cancelled), stranding the PR at `mergeable_state: unstable`. The PAT
  cannot re-run jobs (403); re-fire the event by toggling one label off and on.
- **Push via `originToken`**, never `origin` (SSH, broken). Commit as
  `eldara-cruncher <hello@eldara.io>`, matching the other code repos.
- `gofmt` is the only formatter; import grouping is not linted.

## Layout

```
cmd/swarmcli-cd/   one-line main; the entry point is controller/
controller/        entry point, command dispatch, daemon wiring
application/       Application spec + status: the wire contract
scripts/           check-spdx.sh
docs/
.github/workflows/ ci.yml, check_labels.yml, licence.yml
```

Packages land with the code that needs them rather than as empty directories,
so this grows one issue at a time.

**Do not put them under `internal/`.** Issue #1 sketches an `internal/` tree
(`app`, `git`, `render`, `reconcile`, `health`, `swarms`, `store`, `api`,
`audit`), but that is incompatible with D6: the private `swarmcli-cd-be`
companion is a separate *module*, and Go's internal rule is per-module, not
per-repository — it could not import any of it. This is exactly why
`Eldara-Tech/swarmcli` has no `internal/` directory anywhere, and why
swarmcli-be imports `app`, `views`, `registry` and `docker` as top-level
packages. Anything the companion touches — the seams, the core types, the
reconciler, the API — is top-level here too.

## Design constraints worth not rediscovering

Both are established, with reproductions, in issue #1:

- **`docker stack deploy` cannot be the applier.** Its `--prune` removes services
  only and swallows its own list error, networks are silently never updated,
  there is no dry-run, `--detach=true` returns before convergence, and update
  order is Go map iteration. The applier is built on docker/cli's *exported*
  `cli/compose/{loader,schema,convert}` plus `github.com/moby/moby/client`.
- **Live drift is diffed at `ServiceSpec` level, not YAML.** `compose →
  ServiceSpec` is lossy and one-way, so diffing against
  `docker.ReconstructStackCompose` manufactures differences that do not exist.
  That function is for human-readable display only.

## Open-core seams

Per D6 the private `swarmcli-cd-be` companion is deferred to Phase 3, but the
**seams ship from day one**: `SwarmRegistry`, `Authorizer`, `Notifier` and
`SecretProvider`, each an interface with a working OSS default, replaced via Go
`init()` self-registration (the mechanism swarmcli-be already uses — see its
`docs/extensibility.md`). No build tags, no stubbed files in the public tree.
