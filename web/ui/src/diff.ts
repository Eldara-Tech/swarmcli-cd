// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The diff the controller sends, turned into rows a screen can draw.
//
// # It is not a unified diff
//
// drift.Diffs renders with CE's utils/textdiff, whose own doc comment says it
// "omits hunk headers". So there is no `---`/`+++` preamble, no `@@`, and — the
// consequence that matters here — no hunking at all: every diff carries the
// whole rendered manifest as context, however small the change. A one-line
// chart bump on a six-service stack arrives as several hundred unchanged lines
// around one pair. Folding is therefore what makes this screen usable rather
// than a nicety, which is why it lives in a tested module of its own instead of
// inside the component.
//
// # The prefix is two characters, and reading one is a real bug
//
// textdiff writes "- ", "+ " or "  " and then the line. A manifest is YAML, and
// YAML is full of lines that themselves begin "- " — every list item. So a
// reader that classified on the prefix and then trimmed, or that matched
// `startsWith('-')` against the whole line, would render an unchanged
// `  - name: web` as a deletion. The marker is character 0 and nothing else; the
// content starts at character 2, indentation included.
//
// No diff library: the dependency budget forbids one and
// scripts/check-npm-licences.sh gates additions. There is also nothing to gain —
// the LCS has already been computed at the other end.

/** How many unchanged lines are kept either side of a change. */
export const foldContext = 3

/**
 * One row of the rendered diff.
 *
 * `folded` is not a line the controller sent; it is the stand-in for a run of
 * unchanged ones this module dropped, carrying how many so the screen can say
 * so rather than silently hiding manifest.
 */
export type DiffRow =
  | { kind: 'added' | 'removed' | 'context'; text: string }
  | { kind: 'folded'; hidden: number }

/**
 * diffRows parses one release's diff and folds its unchanged runs.
 *
 * An empty diff is an empty list rather than one blank row: `textdiff.Lines`
 * returns "" when both manifests are empty, and a screen has something specific
 * to say about that which a row of nothing would not convey.
 */
export function diffRows(diff: string, context: number = foldContext): DiffRow[] {
  if (diff === '') return []
  // One trailing newline, because textdiff terminates every line with one.
  // Splitting without dropping it would add a final empty row to every diff.
  const lines = (diff.endsWith('\n') ? diff.slice(0, -1) : diff).split('\n').map(classify)

  const rows: DiffRow[] = []
  let i = 0
  while (i < lines.length) {
    if (lines[i].kind !== 'context') {
      rows.push(lines[i])
      i++
      continue
    }
    let end = i
    while (end < lines.length && lines[end].kind === 'context') end++
    const run = lines.slice(i, end)
    // Context is only worth keeping next to a change. A run at the very top of
    // the diff has nothing above it to give context to, and one at the very
    // bottom has nothing below — so those keep the tail and the head
    // respectively and no more. Without this an install, which is one addition
    // followed by nothing, would still print three lines of trailing manifest.
    const head = i === 0 ? 0 : context
    const tail = end === lines.length ? 0 : context
    if (run.length > head + tail + 1) {
      rows.push(...run.slice(0, head))
      rows.push({ kind: 'folded', hidden: run.length - head - tail })
      // slice(length) is [] when tail is 0, which is the run-at-the-end case.
      rows.push(...run.slice(run.length - tail))
    } else {
      rows.push(...run)
    }
    i = end
  }
  return rows
}

/**
 * classify reads one line's marker.
 *
 * A line matching none of the three is kept whole as context rather than having
 * two characters cut off it. Nothing the OSS renderer produces reaches that
 * branch; an alternative reconciler serving these endpoints could, and losing
 * the first two characters of somebody's manifest is a worse failure than
 * showing an unclassified line.
 */
function classify(line: string): DiffRow {
  switch (line[0]) {
    case '+':
      return { kind: 'added', text: line.slice(2) }
    case '-':
      return { kind: 'removed', text: line.slice(2) }
    case ' ':
      return { kind: 'context', text: line.slice(2) }
    default:
      return { kind: 'context', text: line }
  }
}
