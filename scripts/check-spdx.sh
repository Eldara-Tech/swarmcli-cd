#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
# Copyright © 2026 Eldara Tech
set -eu

tmpfile=$(mktemp)
trap 'rm -f "$tmpfile"' EXIT

# Go's package walker skips only .-prefixed, _-prefixed and testdata
# directories, and neither of the UI's two is any of those — so this walk
# reaches them as well. A dependency tree is full of unheadered .sh files, and
# without the first exclusion this script starts failing on any machine where
# somebody has run `npm ci`.
find . -type f \( -name '*.go' -o -name '*.sh' \) \
  -not -path './vendor/*' \
  -not -path './.git/*' \
  -not -path './web/ui/node_modules/*' \
  -not -path './web/dist/*' \
  > "$tmpfile"

fail=0
while IFS= read -r f; do
  if ! head -n 20 "$f" | grep -q "SPDX-License-Identifier: Apache-2.0"; then
    echo "Missing SPDX header: $f"
    fail=1
  fi
done < "$tmpfile"

exit "$fail"
