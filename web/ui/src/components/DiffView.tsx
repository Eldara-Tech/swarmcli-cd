// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { diffRows, type DiffRow } from '../diff'
import { plural } from '../format'

/** The character in the gutter, which is the half of the encoding that is not colour. */
const glyphs: Record<DiffRow['kind'], string> = {
  added: '+',
  removed: '-',
  // A context line's gutter is empty and keeps its width from the stylesheet, so
  // the columns line up without a screen reader announcing a space per line.
  context: '',
  folded: '⋯',
}

/**
 * One release's diff.
 *
 * Every row carries a glyph as well as a colour. Around 8% of male readers
 * cannot separate the red from the green, and a diff whose only encoding is
 * colour tells them the manifest changed without telling them in which
 * direction — so the glyph is the signal and the colour is the emphasis, never
 * the other way round.
 *
 * The colour arrives as a class, never as a style prop: web.go sends
 * `style-src 'self'` with no 'unsafe-inline', so an inline style attribute is
 * refused by the browser and by eslint before that. See eslint.config.js.
 */
export function DiffView({ diff }: { diff: string }) {
  // Trimmed, not compared to "": a diff carrying only unchanged blank lines is
  // the same answer as an empty one, and both reach here. The engine rendered
  // the same manifest either side — a release can be planned for an upgrade
  // because its chart version moved and still render byte-identically — and
  // drawing nothing at all would read as a screen that failed to load.
  if (diff.trim() === '') return <p className="empty">(no manifest change)</p>

  const rows = diffRows(diff)
  return (
    // Spans and not divs: <pre> takes phrasing content, and a block element
    // inside it is invalid HTML that every browser happens to render. The row
    // is laid out by the stylesheet instead.
    <pre className="diff">
      {rows.map((row, i) => (
        <span className={`diff-line diff-${row.kind}`} key={i}>
          <span className="diff-gutter">{glyphs[row.kind]}</span>
          <span className="diff-content">
            {row.kind === 'folded' ? plural(row.hidden, 'unchanged line') : row.text}
          </span>
        </span>
      ))}
    </pre>
  )
}
