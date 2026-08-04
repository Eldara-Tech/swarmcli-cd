#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
# Copyright © 2026 Eldara Tech
set -eu

# Third-party lifecycle scripts stay off, in both of the two ways they can be
# turned off.
#
# `npm ci` runs preinstall/install/postinstall from every package it installs,
# as whoever ran it. In the release job that is a step carrying GITHUB_TOKEN
# with contents: write, and the bundle it writes is compiled into all four
# published binaries by `//go:embed all:dist` — so one postinstall would hold
# the release credential and the page an operator afterwards types a swarm-root
# bearer token into. Nothing in the tree declares one today, and nothing about a
# Dependabot bump keeps it that way.
#
# Two mechanisms because each covers what the other cannot. web/ui/.npmrc covers
# an invocation nobody remembered to flag, including a developer's own; the
# explicit flag covers the invocations that are --prefixed from another
# directory, since which .npmrc npm treats as the project's is a resolution rule
# and not something this repository controls.
#
# Run from anywhere: `./scripts/check-npm-scripts.sh`. Needs no npm.
cd "$(dirname "$0")/.."

tmpfile=$(mktemp)
trap 'rm -f "$tmpfile"' EXIT

fail=0

npmrc=web/ui/.npmrc
if ! grep -qE '^[[:space:]]*ignore-scripts[[:space:]]*=[[:space:]]*true[[:space:]]*$' "$npmrc" 2>/dev/null; then
  echo "$npmrc does not set ignore-scripts=true"
  fail=1
fi

# Only the file kinds that execute: a .md quoting `npm ci` is documentation, and
# under the .npmrc above a developer following it is covered anyway. The
# node_modules exclusion is check-spdx.sh's, for check-spdx.sh's reason — the
# dependency tree is full of both kinds of file, and without it this script
# starts failing on any machine where somebody has run `npm ci`.
find . -type f \( -name '*.yml' -o -name '*.yaml' -o -name '*.sh' -o -name 'Dockerfile*' \) \
  -not -path './.git/*' \
  -not -path './web/ui/node_modules/*' \
  -not -path './site/*' \
  > "$tmpfile"

while IFS= read -r f; do
  # An npm invocation whose subcommand installs something, ignoring comments.
  # `npx playwright install` is not one: npx is a different word, and playwright
  # is what is doing the installing.
  hits=$(grep -nE '(^|[^[:alnum:]._-])npm[[:space:]]' "$f" |
    grep -vE '^[0-9]+:[[:space:]]*#' |
    grep -E '[[:space:]](ci|install|add)([[:space:]]|$)' |
    grep -vE -- '--ignore-scripts' || true)
  [ -n "$hits" ] || continue
  echo "$hits" | sed "s|^|$f:|"
  fail=1
done < "$tmpfile"

if [ "$fail" -ne 0 ]; then
  echo
  # Worded around this script's own matcher rather than with it: writing the
  # rule down beside the thing it governs is what web_test.go's HTML-comment
  # strip exists for, and here it would have made the script fail on itself.
  echo "An install above runs third-party lifecycle scripts. Pass"
  echo "--ignore-scripts, and keep web/ui/.npmrc saying the same thing."
fi

exit "$fail"
