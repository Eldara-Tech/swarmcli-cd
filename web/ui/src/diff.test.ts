// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { describe, expect, it } from 'vitest'

import { diffRows, type DiffRow } from './diff'

/** A diff exactly as textdiff.Lines writes one: two-character prefixes, every
 *  line terminated. */
function wire(...lines: string[]): string {
  return lines.map((line) => `${line}\n`).join('')
}

/** n unchanged lines, numbered so a fold can be checked for which ones it kept. */
function context(n: number): string[] {
  return Array.from({ length: n }, (_, i) => `  line ${i + 1}`)
}

function texts(rows: DiffRow[]): string[] {
  return rows.map((row) => (row.kind === 'folded' ? `folded:${row.hidden}` : row.text))
}

describe('diffRows', () => {
  it('reads the marker from the first character and the content from the third', () => {
    // The prefix is two characters wide — "- ", "+ ", "  " — and slicing one
    // would leave a stray space at the head of every line.
    const rows = diffRows(wire('- image: nginx:1.0', '+ image: nginx:1.1'))
    expect(rows).toEqual([
      { kind: 'removed', text: 'image: nginx:1.0' },
      { kind: 'added', text: 'image: nginx:1.1' },
    ])
  })

  // The bug a one-character prefix, or a trim-then-classify, would produce. A
  // manifest is YAML and YAML is full of list items, so an unchanged
  // "  - name: web" begins with a space and then a hyphen.
  it('does not read an unchanged YAML list item as a deletion', () => {
    const rows = diffRows(wire('  - name: web', '  + not a marker', '+ replicas: 2'))
    expect(rows).toEqual([
      { kind: 'context', text: '- name: web' },
      { kind: 'context', text: '+ not a marker' },
      { kind: 'added', text: 'replicas: 2' },
    ])
  })

  it('keeps the manifest own indentation, which is inside the content', () => {
    expect(diffRows(wire('+     replicas: 2'))).toEqual([{ kind: 'added', text: '    replicas: 2' }])
  })

  it('is empty for an empty diff rather than one blank row', () => {
    expect(diffRows('')).toEqual([])
  })

  it('does not add a row for the trailing newline every line carries', () => {
    expect(diffRows(wire('+ one', '- two'))).toHaveLength(2)
  })

  describe('folding, which is required and not a nicety', () => {
    // textdiff omits hunk headers, so every diff carries the whole rendered
    // manifest. Without this a one-line chart bump ships a few hundred
    // unchanged lines to the browser for every release in the plan.
    it('replaces a long run between two changes with one row saying how many', () => {
      const rows = diffRows(wire('- gone', ...context(20), '+ new'))
      expect(texts(rows)).toEqual([
        'gone',
        'line 1',
        'line 2',
        'line 3',
        'folded:14',
        'line 18',
        'line 19',
        'line 20',
        'new',
      ])
    })

    it('keeps a run short enough to be worth showing', () => {
      // Seven lines between two changes: three either side plus one folded is
      // the same height as printing all seven, so it prints all seven.
      const rows = diffRows(wire('- gone', ...context(7), '+ new'))
      expect(rows.some((row) => row.kind === 'folded')).toBe(false)
      expect(rows).toHaveLength(9)
    })

    it('keeps no leading context at the top of the diff, where there is nothing above to give context to', () => {
      const rows = diffRows(wire(...context(20), '+ new'))
      expect(texts(rows)).toEqual(['folded:17', 'line 18', 'line 19', 'line 20', 'new'])
    })

    it('keeps no trailing context at the bottom, which is what an install looks like', () => {
      const rows = diffRows(wire('+ new', ...context(20)))
      expect(texts(rows)).toEqual(['new', 'line 1', 'line 2', 'line 3', 'folded:17'])
    })

    it('folds a diff that is entirely context into a single row', () => {
      expect(texts(diffRows(wire(...context(20))))).toEqual(['folded:20'])
    })
  })
})
