#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
# Copyright © 2026 Eldara Tech
set -eu

# The licence gate for web/ui's dependency tree.
#
# check-spdx.sh checks the headers we write and says nothing about the packages
# `npm ci` installs — a third-party licence obligation this repository did not
# have until the UI arrived. The dependency that matters is not the one somebody
# chose; it is the transitive one a routine version bump pulls in, which arrives
# in a Dependabot PR nobody reads a lockfile diff for.
#
# Run from anywhere: `./scripts/check-npm-licences.sh`. Needs web/ui's tree
# installed (`npm --prefix web/ui ci`) — `npm query` reads the packages on disk,
# because the lockfile records no licence.
cd "$(dirname "$0")/.."

tmpfile=$(mktemp)
trap 'rm -f "$tmpfile"' EXIT

# '*' is every node in the tree, dev dependencies and the root package included.
npm --prefix web/ui query '*' > "$tmpfile"

node - "$tmpfile" <<'EOF'
const fs = require("node:fs");

// What may end up in the bundle a released binary embeds. Permissive only:
// swarmcli-cd is Apache-2.0 and the private companion links against it, so a
// reciprocal licence reaching shipped code is a decision, never a bump.
const shipped = new Set([
  "0BSD",
  "Apache-2.0",
  "BlueOak-1.0.0",
  "BSD-2-Clause",
  "BSD-3-Clause",
  "CC0-1.0",
  "ISC",
  "MIT",
  "MIT-0",
  // The self-hosted UI fonts (@fontsource/inter, /plus-jakarta-sans,
  // /jetbrains-mono) are OFL-1.1: the SIL Open Font License, which permits
  // embedding and redistributing the font files — as `//go:embed all:dist`
  // does, inside every released binary — provided the copyright and licence
  // travel with them.
  //
  // They do not travel by themselves. Vite emits the .woff2 files into
  // /assets and emits nothing else from the package, so the first version of
  // this entry was allowing a permission whose one condition the build was
  // not meeting: the font bytes shipped and the OFL text did not. What makes
  // it true is thirdPartyNotices() in web/ui/vite.config.ts, which writes
  // every runtime package's notice into dist/THIRD-PARTY-NOTICES.txt and
  // fails the build on a package it cannot find one for — and web/web.go,
  // which serves it. The same mechanism covers the MIT notices the
  // JavaScript in the bundle has always required and never carried.
  //
  // The bytes are in the bundle, so this is a shipped permission, not a
  // build-time one.
  "OFL-1.1",
  "Python-2.0",
  "Unlicense",
]);

// What may merely build it. Wider by exactly two, and each deliberately.
//
// Vite's CSS transformer (lightningcss) is MPL-2.0. MPL is file-level copyleft
// on its own source, and a build tool contributes none of its source to the
// bundle — it transforms ours. Anything stronger still fails here, because a
// build tool is code we execute on the machine that signs a release.
//
// caniuse-lite is CC-BY-4.0, and it is browserslist's browser-support *database*
// rather than code: babel reads it to decide what to compile down to, and none
// of it is emitted. Attribution attaches to data nothing ships. A CC licence on
// something in the bundle stays a failure, which is why this is here and not
// above.
const build = new Set([...shipped, "MPL-2.0", "CC-BY-4.0"]);

// SPDX expressions: "(MIT OR CC0-1.0)" is a choice, so one permitted
// alternative is enough; "A AND B" binds us to both. Handled because npm is
// full of them, not because this tree has one today.
const permits = (set, expr) =>
  expr
    .replace(/[()]/g, "")
    .split(/\s+OR\s+/)
    .some((alt) => alt.split(/\s+AND\s+/).every((term) => set.has(term.trim())));

let failed = 0;
for (const pkg of JSON.parse(fs.readFileSync(process.argv[2], "utf8"))) {
  const where = pkg.dev ? "build-time" : "shipped";
  // A licence npm cannot state as a string is an object ({type, url}, the old
  // form), "UNLICENSED", or absent. None of them is a permission, so all three
  // fall through to the failure below rather than being skipped.
  const expr = typeof pkg.license === "string" ? pkg.license : "";
  if (expr !== "" && permits(pkg.dev ? build : shipped, expr)) continue;
  console.log(`${pkg.name}@${pkg.version}: ${expr || "no licence"} (${where})`);
  failed++;
}

if (failed > 0) {
  console.log(
    `\n${failed} package(s) outside the allowlist in scripts/check-npm-licences.sh.\n` +
      "Vet the licence and add it there, or drop the dependency.",
  );
  process.exit(1);
}
EOF
