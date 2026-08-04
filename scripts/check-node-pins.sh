#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
# Copyright © 2026 Eldara Tech
set -eu

# Every Node major this repository pins is the same Node major.
#
# The CSP is a build constraint, not a wish: `script-src 'self'` with no
# 'unsafe-inline', enforced by TestTheBuiltIndexHasNoInlineScriptOrStyle
# against the bundle Vite actually emitted. Which Node emitted it is therefore
# part of what is being asserted — and the Node the image ships is pinned in
# the Dockerfile, while the Node that assertion runs on is pinned separately in
# a workflow.
#
# Dependabot bumps the Dockerfile's `FROM node:` on its own monthly schedule.
# Without this check, that bump ships a bundle built by one Node while the only
# job that inspects a bundle runs on another — green, and inspecting something
# else. Adding the Dockerfile to ci.yml's `ui` paths filter closes the half of
# that hole where the job does not run at all, and does nothing about this
# half.
#
# So the pins move together or they do not move. A Dependabot Dockerfile bump
# goes red here until the workflows follow, and once they do, the assertion is
# running on the Node that will build the released bundle.
#
# Run from anywhere: `./scripts/check-node-pins.sh`. Needs no Node.
cd "$(dirname "$0")/.."

tmpfile=$(mktemp)
trap 'rm -f "$tmpfile"' EXIT

# Discovered rather than listed. A new workflow that sets up Node is exactly
# the case a hand-maintained list of files would miss, and it is the case this
# check exists for.
{
  for f in Dockerfile*; do
    [ -f "$f" ] || continue
    sed -n 's#^[[:space:]]*FROM[[:space:]][[:space:]]*node:\([^[:space:]-]*\).*#\1|'"$f"'|FROM node:#p' "$f"
  done
  for f in .github/workflows/*.yml; do
    [ -f "$f" ] || continue
    # `node-version:` and not `node-version-file:`; the colon is what tells
    # them apart, and setup-node quotes the value more often than not.
    sed -n "s#^[[:space:]]*node-version:[[:space:]]*['\"]\{0,1\}\([^'\"[:space:]]*\).*#\1|$f|node-version:#p" "$f"
  done
} > "$tmpfile"

found=$(wc -l < "$tmpfile" | tr -d ' ')
# Two, because one is what this check reads as agreement when it has in fact
# stopped finding anything: a renamed key or a Dockerfile written another way
# would otherwise pass silently, which is the failure mode the whole issue is
# about.
if [ "$found" -lt 2 ]; then
  echo "found $found Node pin(s); expected the Dockerfile's and at least one workflow's."
  echo "The extraction below has stopped matching, so this check was asserting nothing."
  exit 1
fi

# A value with no leading digit — lts/*, latest, current — names a version that
# moves on its own, so two of them agreeing here would not mean two builds got
# the same Node. Refused rather than compared.
if [ -n "$(cut -d'|' -f1 "$tmpfile" | grep -vE '^[0-9]' || true)" ]; then
  echo "A Node pin does not name a version:"
  echo
  sort -t'|' -k2 "$tmpfile" | while IFS='|' read -r value file key; do
    case "$value" in
    [0-9]*) ;;
    *) echo "  $file: $key$value" ;;
    esac
  done
  echo
  echo "This compares majors, so a pin has to start with one."
  exit 1
fi

# The major, so that a Dockerfile pinning 26.1.0-alpine beside a workflow
# pinning 26 is agreement rather than a false refusal. It is the granularity
# the comment above claims and the one Dependabot moves.
majors=$(cut -d'|' -f1 "$tmpfile" | sed 's#[^0-9].*$##' | sort -u)
if [ "$(echo "$majors" | wc -l | tr -d ' ')" -ne 1 ]; then
  echo "The Node pins disagree:"
  echo
  sort -t'|' -k2 "$tmpfile" | while IFS='|' read -r value file key; do
    echo "  $file: $key$value"
  done
  echo
  echo "The bundle the release builds and the bundle the CSP check inspects have"
  echo "to come from the same Node. Move them together, or the check is green"
  echo "against a bundle nothing ships."
  exit 1
fi

echo "Node $majors, in $found place(s)."
