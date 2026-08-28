// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import type { ReactNode } from 'react'

/**
 * The icon set, drawn inline.
 *
 * Not an icon font and not served files: a font is a font-src to inherit and a
 * whole alphabet to ship for a dozen glyphs, and a file per icon is a request
 * per icon. These are SVG paths in the bundle, taking `currentColor` so a
 * caller colours them with the same token its text uses. The stroke style — no
 * fill, round joins — matches the brand's terminal-prompt mark, so the set and
 * the logo beside it read as one hand.
 *
 * Paths are Lucide's, copied in rather than depended on: the set a console
 * needs is small and stable, and a package would be a tree to audit — and a
 * licence-gate entry — for the sake of fifteen `<path>`s.
 *
 * ISC requires the copyright and permission notice to travel with the copy, so
 * it is reproduced here in full rather than referred to. Naming a licence is
 * not retaining its notice:
 *
 *   Copyright (c) for portions of Lucide are held by Cole Bemis 2013-2022 as
 *   part of Feather (MIT). All other copyright (c) for Lucide are held by
 *   Lucide Contributors 2022.
 *
 *   Permission to use, copy, modify, and/or distribute this software for any
 *   purpose with or without fee is hereby granted, provided that the above
 *   copyright notice and this permission notice appear in all copies.
 *
 *   THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 *   WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 *   MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 *   ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 *   WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 *   ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR
 *   IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 *
 * No `style` prop anywhere — size is width/height attributes — because the CSP
 * this UI serves under forbids the inline style one would emit; see index.css.
 */

// `satisfies`, never an annotation. Written `Record<string, ReactNode>` the key
// type widened to `string`, so `keyof typeof paths` was `string | number` and
// IconName checked nothing at all: `<Icon name="typo" />` compiled and rendered
// an empty <svg> with no error anywhere. Icon.test.tsx guards it with a pair of
// expect-error directives, which stop being errors the moment it widens again.
const paths = {
  terminal: (
    <>
      <path d="m4 17 6-6-6-6" />
      <path d="M12 19h8" />
    </>
  ),
  search: (
    <>
      <circle cx="11" cy="11" r="8" />
      <path d="m21 21-4.3-4.3" />
    </>
  ),
  sync: (
    <>
      <path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8" />
      <path d="M21 3v5h-5" />
      <path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16" />
      <path d="M3 21v-5h5" />
    </>
  ),
  check: (
    <>
      <path d="M21.801 10A10 10 0 1 1 17 3.335" />
      <path d="m9 11 3 3L22 4" />
    </>
  ),
  warn: (
    <>
      <path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3" />
      <path d="M12 9v4" />
      <path d="M12 17h.01" />
    </>
  ),
  error: (
    <>
      <circle cx="12" cy="12" r="10" />
      <path d="m15 9-6 6" />
      <path d="m9 9 6 6" />
    </>
  ),
  app: (
    <>
      <path d="M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z" />
      <path d="m3.3 7 8.7 5 8.7-5" />
      <path d="M12 22V12" />
    </>
  ),
  service: (
    <>
      <rect width="20" height="8" x="2" y="2" rx="2" />
      <rect width="20" height="8" x="2" y="14" rx="2" />
      <path d="M6 6h.01" />
      <path d="M6 18h.01" />
    </>
  ),
  bell: (
    <>
      <path d="M10.268 21a2 2 0 0 0 3.464 0" />
      <path d="M3.262 15.326A1 1 0 0 0 4 17h16a1 1 0 0 0 .74-1.673C19.41 13.956 18 12.499 18 8A6 6 0 0 0 6 8c0 4.499-1.411 5.956-2.738 7.326" />
    </>
  ),
  chevronDown: <path d="m6 9 6 6 6-6" />,
  externalLink: (
    <>
      <path d="M15 3h6v6" />
      <path d="M10 14 21 3" />
      <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
    </>
  ),
  filter: <polygon points="22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3" />,
  trash: (
    <>
      <path d="M3 6h18" />
      <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6" />
      <path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
      <path d="M10 11v6" />
      <path d="M14 11v6" />
    </>
  ),
  more: (
    <>
      <circle cx="12" cy="12" r="1" />
      <circle cx="12" cy="5" r="1" />
      <circle cx="12" cy="19" r="1" />
    </>
  ),
  layers: (
    <>
      <path d="M12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83Z" />
      <path d="M2 12a1 1 0 0 0 .58.91l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9A1 1 0 0 0 22 12" />
      <path d="M2 17a1 1 0 0 0 .58.91l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9A1 1 0 0 0 22 17" />
    </>
  ),
  activity: (
    <path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2" />
  ),
  gauge: (
    <>
      <path d="m12 14 4-4" />
      <path d="M3.34 19a10 10 0 1 1 17.32 0" />
    </>
  ),
} satisfies Record<string, ReactNode>

export type IconName = keyof typeof paths

/**
 * One glyph from the set. `size` is the box in px; the drawing scales to it.
 *
 * Always decorative. Every icon in this console sits beside its own label — the
 * rail's nav text, a card's heading, a chip's word — so none of them is the
 * accessible name of anything, and an aria-hidden svg keeps a screen reader
 * from reading the same thing twice. It carried an optional `title` for the
 * case where an icon *is* the only label; no caller ever had that case, and the
 * one place it looked like it might — the rail collapsed to icons under 960px —
 * is solved by hiding the label visually rather than removing it.
 */
export function Icon({ name, size = 20 }: { name: IconName; size?: number }) {
  return (
    <svg
      aria-hidden="true"
      className="icon"
      fill="none"
      height={size}
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth="2"
      viewBox="0 0 24 24"
      width={size}
    >
      {paths[name]}
    </svg>
  )
}

/**
 * The brand mark: the terminal prompt (a chevron and an underscore) the wordmark
 * hangs off in the header. Drawn a touch heavier than the icon set and left to
 * take its colour from `.brand-mark` in the CSS, which is the one place the
 * brand blue is spent on a glyph.
 */
export function BrandMark() {
  return (
    <svg
      aria-hidden="true"
      className="brand-mark"
      fill="none"
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth="2.2"
      viewBox="0 0 24 24"
    >
      <path d="m5 8 4 4-4 4" />
      <path d="M12 16h7" />
    </svg>
  )
}
