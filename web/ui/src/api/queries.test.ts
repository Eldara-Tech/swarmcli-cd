// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The invalidation map, which is pure policy and whose errors are silent.
//
// A screen that quietly never updates again produces no exception, no failed
// request and no log line — so the table is asserted here row by row rather than
// left to a reviewer noticing a missing entry. It is deliberately a
// change-detector: editing the table without editing this file is exactly the
// change that has to be argued for.

import { describe, expect, it } from 'vitest'

import { eventTypes } from './events'
import { applicationKey, controllerKey, diffKey, historyKey, invalidatedBy, listKey } from './queries'

/** The keys, resolved for one application, so a row reads as a set of names. */
const named: Record<string, unknown> = {
  detail: applicationKey('edge'),
  list: listKey,
  diff: diffKey('edge'),
  history: historyKey('edge'),
  controller: controllerKey,
}

/** documents names what invalidatedBy asked for, in the vocabulary above. */
function documents(type: string): string[] {
  return invalidatedBy(type, 'edge').map((invalidation) => {
    const name = Object.keys(named).find(
      (key) => JSON.stringify(named[key]) === JSON.stringify(invalidation.queryKey),
    )
    if (name === undefined) throw new Error(`no screen registers ${JSON.stringify(invalidation.queryKey)}`)
    return name
  })
}

const expected: Record<string, string[]> = {
  'sync-started': ['detail', 'list', 'diff'],
  'sync-succeeded': ['detail', 'list', 'diff', 'history'],
  'sync-failed': ['detail', 'list', 'diff', 'history'],
  'drift-detected': ['detail', 'list', 'diff'],
  'live-drift-detected': ['detail', 'list'],
  'drift-converged': ['detail', 'list'],
  'resources-pruned': ['detail', 'list', 'diff', 'history', 'controller'],
  'prune-failed': ['detail', 'list'],
}

describe('the invalidation map', () => {
  for (const type of eventTypes) {
    it(`invalidates ${expected[type].join(', ')} for ${type}`, () => {
      expect(documents(type)).toEqual(expected[type])
    })
  }

  // The two rows that are counter-intuitive, stated again as behaviour rather
  // than as a row of a table, because a reader who disagrees with either will
  // reach for the table and not for the list above.

  // notify.go: no chart revision is written for a correction, because the
  // desired state did not change. Invalidating the history here would refetch a
  // document that cannot have moved, on every correction, for ever.
  it('does not invalidate the history when a drift converges', () => {
    expect(documents('drift-converged')).not.toContain('history')
  })

  // The swarm moved; the rendered manifest did not. That is the entire reason
  // live drift is its own event type rather than a drift-detected carrying
  // different prose.
  it('does not invalidate the diff when live drift is detected', () => {
    expect(documents('live-drift-detected')).not.toContain('diff')
  })

  // Conservative beats silently dropping a signal: a refetch costs a request,
  // and a rule that was never written costs a screen that never updates.
  it('invalidates everything for a type a newer controller added', () => {
    expect(documents('licence-expired')).toEqual(['detail', 'list', 'diff', 'history', 'controller'])
  })

  // Every event is about one application, and every row therefore says
  // something about that application's own document and about the list it
  // appears in. A row that said neither would be an event with nothing to
  // report.
  it('always invalidates the application and the list', () => {
    for (const type of eventTypes) {
      expect(documents(type), type).toEqual(expect.arrayContaining(['detail', 'list']))
    }
  })
})

describe('the keys', () => {
  // #202's decision, and the thing the two rows above are spent on: react-query
  // matches by prefix, so a diff key nested under the application's could not be
  // left out of an invalidation of the application. Asserted rather than
  // commented, because nesting them would look like tidying up.
  it('keeps the diff and the history out of the application subtree', () => {
    for (const key of [diffKey('edge'), historyKey('edge')]) {
      expect(key[0]).not.toBe(listKey[0])
    }
  })

  // The other half of the same fact, from the direction that bites: the list's
  // key is a prefix of every detail key, so invalidating it non-exactly would
  // refetch the detail on screen over an event about some other application.
  it('invalidates the list exactly, because it is a prefix of every detail key', () => {
    expect(applicationKey('edge')[0]).toBe(listKey[0])
    const list = invalidatedBy('sync-started', 'edge').find((i) => i.queryKey === listKey)
    expect(list?.exact).toBe(true)
  })

  // An application name is a path segment on the wire but a key element here, so
  // nothing escapes it and nothing has to: two applications whose encoded names
  // collide would otherwise share a cache entry.
  it('keys on the application name verbatim', () => {
    expect(applicationKey('a/b')).toEqual(['applications', 'a/b'])
    expect(diffKey('a/b')).toEqual(['diff', 'a/b'])
  })
})
