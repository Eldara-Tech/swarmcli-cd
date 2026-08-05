#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright © 2026 Eldara Tech
set -euo pipefail

# The application the browser smoke walks through: a git repository holding one
# chart, and the app-set file that points at it.
#
# A script rather than two heredocs in smoke.yml because it has a second caller.
# swarmcli-cd-be runs the same walkthrough against a licensed binary
# (swarmcli-cd-be#7), materialising this tree out of the module cache the way its
# release job already does — so a copy over there would be a copy of the exact
# fixture every assertion in e2e/smoke.spec.ts is written against, drifting the
# first time one of them changes. The controller start is deliberately *not*
# here: the two runs configure it differently, and that difference is the point
# of running it twice.
#
#   Usage: scripts/smoke-fixture.sh [directory]   (default: the current one)
#
# Leaves ./smoke-repo and ./applications.yaml in that directory. Needs `git`.

cd "${1:-.}"

# The chart is a busybox that sleeps: the smallest thing that reliably reaches
# running on any runner, with no healthcheck and a tag that is always pullable.
mkdir -p smoke-repo/charts/app/templates
(
  cd smoke-repo
  cat > swarmcli-release.yaml <<'YAML'
apiVersion: v1
releases:
  - name: smoke
    chart: ./charts/app
YAML
  printf 'apiVersion: v1\nname: app\nversion: 0.1.0\n' > charts/app/Chart.yaml
  cat > charts/app/templates/stack.yaml <<'YAML'
version: "3.9"
services:
  app:
    image: busybox:1.36
    command: ["sleep", "3600"]
    deploy:
      replicas: 1
YAML
  git init -q -b main
  git add -A
  git -c user.email=ci@example.com -c user.name=ci commit -q -m chart
)

# Cloned over a file path, exactly as the integration tests do it.
cat > applications.yaml <<YAML
applications:
  - name: smoke
    source:
      repoURL: $PWD/smoke-repo
      revision: main
      releaseFile: swarmcli-release.yaml
    syncPolicy:
      # Manual, and that is the whole reason this fixture works.
      #
      # Automated, the controller deploys the application about a second after
      # startup — long before Playwright has a browser — and then it is
      # converged. A sync triggered afterwards has nothing to do and raises no
      # events at all, so the assertion that one arrives at an already-watching
      # page could never pass. There is no replay to fall back on: api/stream.go
      # emits no \`id:\`, so a subscriber is only ever sent what happens while it
      # is attached.
      #
      # Manual, the startup pass plans, reports out-of-sync and stops. The
      # install is still waiting when the browser connects, so the sync the test
      # triggers is real work and raises sync-started and sync-succeeded at a
      # page that is already watching — which is exactly what the test claims to
      # prove.
      automated: false
      interval: 10s
    driftDetection: manifest
YAML
