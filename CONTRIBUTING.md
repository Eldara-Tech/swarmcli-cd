<!--
SPDX-License-Identifier: Apache-2.0
Copyright © 2026 Eldara Tech
-->

# Contributing

Thanks for your interest in swarmcli-cd. This page is what a change has to
satisfy; [docs/extensibility.md](docs/extensibility.md) is how the open-core
seams fit together, and
[issue #1](https://github.com/Eldara-Tech/swarmcli-cd/issues/1) is the source of
truth for positioning, the design decisions and the phase plan. **Read #1 before
proposing anything architectural** — the constraints Swarm imposes are
established there, with reproductions, and re-deriving them is the most common
way to spend a week on something that was already settled.

## Building and testing

Go 1.26 or newer.

```bash
go build -o swarmcli-cd ./cmd/swarmcli-cd
go test ./...
gofmt -l .
go vet ./...
golangci-lint run ./...
./scripts/check-spdx.sh
```

The integration tests are behind a build tag and need a Docker daemon that is a
swarm manager. They deploy real stacks, so point them at a swarm you do not mind
being written to:

```bash
go test -tags integration ./integration-tests/...
```

**A plain `go build` does not build the web UI**, and the resulting binary
serves a page saying so to every browser. That is deliberate — it is what keeps
this repository buildable, testable and `go install`-able with no JavaScript
toolchain — but it means a local binary is not what an image serves until you
run the two commands that produce the bundle:

```bash
npm --prefix web/ui ci
npm --prefix web/ui run build   # writes web/dist, which //go:embed compiles in
npm --prefix web/ui run dev     # the development loop; proxies to a controller on :8080
```

`npm --prefix web/ui run lint`, `typecheck` and `test` are what CI runs beside
the build. `./scripts/check-npm-licences.sh`, `check-npm-scripts.sh` and
`check-node-pins.sh` are the rest of the UI's gates.

## Two ways a test passes without reaching the code

**Build a rot-catching fixture by reflection, not by hand.** When a test exists to
catch *a field added later* — a deep-copy check, a round-trip check, an "every
field is serialised" check — walk the type with `reflect` rather than writing a
literal.

swarmcli-cd#137 and #143 are the counter-example, hours apart. #137 added
`Status.Clone`/`Spec.Clone` and a test that walked the value and failed on any
pointer or slice the clone still shared. Its fixture was hand-written, with a
comment defending the choice: *"built by hand rather than by a helper that would
drift with the type."* #135 added `Spec.Allow` — five slices — the same day.
`Clone` did not copy them, the fixture did not populate them, and an empty slice
is indistinguishable from a shared one. The guard was written for exactly that
class of bug and could not see it.

**The Docker API fakes ignore the filters they are handed.** `fakeAPI.ConfigList`
records the label filter for assertions and then returns the whole store; CE's
`fakeBackend` does not model server-side filtering either. So a filtered read
passes CI whether or not the filter is right, **and** a read that should be
filtered passes whether or not it is. Both directions are invisible.

That cost two bugs on one day: swarmcli-cd#63's release-record lookup returned an
unrelated config and failed a legitimate test, and swarmcli#510's proposed label
filter would have refused real installs while CI stayed green. Fix the fake
before the feature — `backend/service_test.go`'s `ConfigList` now honours the
filter, with a comment saying why.

## What every change needs

- **An SPDX header** on every `.go`, `.sh`, `.ts`, `.tsx` and `.css` file, or
  the licence workflow fails:

  ```go
  // SPDX-License-Identifier: Apache-2.0
  // Copyright © 2026 Eldara Tech
  ```

- **Tests for anything that can be wrong.** The suite runs with no daemon, so
  prefer a fake over an integration test where the behaviour allows it.
- **The docs updated in the same PR.** A new flag, environment variable,
  applications-file field or endpoint that is not in
  [docs/configuration.md](docs/configuration.md) or
  [docs/api.md](docs/api.md) is a change only its author can operate.
- **One concern per PR.** Keep the diff readable; a refactor that travels
  alongside a fix hides both.

## Pull requests

Fork, branch from `main`, and open the PR **against `main`**. That last part is
not a style preference: `ci.yml`, `licence.yml` and `check_labels.yml` all
trigger only on pull requests based on `main`, so a PR stacked on another branch
runs **no CI at all** and its green tick means nothing. Verify locally, and
retarget once the parent has merged.

Every PR needs **one label from each of three sets**, enforced by
`check_labels.yml`:

| Set | Labels |
|---|---|
| A — what kind of change | `A0-ui`, `A1-feature`, `A2-bugfix`, `A3-technical`, `D0-dependency` |
| B — priority | `B0-low-priority`, `B2-high-priority` |
| C — blast radius | `C0-breaks-nothing`, `C1-breaking-change` |

Add all three at once. The check reads the labels out of the event payload, so a
run that fired before they were applied evaluates the set as it was then — the
fix is to re-fire the event by pushing or by toggling a label, not to re-run the
job.

## Reporting

- **Bugs and features**: the
  [issue tracker](https://github.com/Eldara-Tech/swarmcli-cd/issues). For a bug,
  the controller's startup `seams` and `routes` lines and the output of
  `swarmcli-cd status -o json` say more than any description.
- **Security vulnerabilities**: not in a public issue. See
  [SECURITY.md](SECURITY.md).

By contributing you agree that your work is licensed under Apache-2.0, and that
it stays in the open half of the product: nothing in this repository is ever
moved behind a licence. See [docs/editions.md](docs/editions.md).
