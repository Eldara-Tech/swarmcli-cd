#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
# Copyright © 2026 Eldara Tech
set -eu

tmpfile=$(mktemp)
trap 'rm -f "$tmpfile"' EXIT

# Go's package walker skips only .-prefixed, _-prefixed and testdata
# directories, and neither of the UI's two is any of those — so this walk
# reaches them as well. A dependency tree is full of unheadered .sh, .ts and
# .css files, and without the first exclusion this script starts failing on any
# machine where somebody has run `npm ci`.
#
# site/ is mkdocs' output, gitignored and full of the theme's unheadered .css.
# Excluded for the same reason: a contributor who has run `mkdocs build` once
# would otherwise get a failure naming files that are not in the repository.
find . -type f \( -name '*.go' -o -name '*.sh' -o -name '*.ts' -o -name '*.tsx' -o -name '*.css' \) \
  -not -path './vendor/*' \
  -not -path './.git/*' \
  -not -path './web/ui/node_modules/*' \
  -not -path './web/dist/*' \
  -not -path './site/*' \
  > "$tmpfile"

fail=0
while IFS= read -r f; do
  if ! head -n 20 "$f" | grep -q "SPDX-License-Identifier: Apache-2.0"; then
    echo "Missing SPDX header: $f"
    fail=1
  fi
done < "$tmpfile"

exit "$fail"
