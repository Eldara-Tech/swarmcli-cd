// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The query keys, and which of them an event says to re-read.
//
// Both halves live here on purpose. react-query matches a key by *prefix*, so
// the keys are themselves the policy: what a screen registers decides what an
// event can invalidate without dragging its neighbours along. Keeping the keys
// in the screens and the map in the hook is how the two silently stop agreeing —
// a renamed key is not a compile error, it is a screen that quietly never
// updates again, which is the whole failure mode this module exists to not have.
//
// # An event says look again; the document is the answer
//
// Nothing here returns data, and nothing anywhere may call setQueryData from an
// event. api/stream.go drops frames for a subscriber that is not keeping up —
// subscriberBuffer is 64, and publish selects with a `default:` that logs and
// discards — so state accumulated from the event sequence diverges silently, and
// diverges most precisely when the controller is busiest, which is exactly when
// the screen matters. The map hands out things to invalidate; the refetch that
// follows reads the authoritative document. eslint.config.js bans the write
// outright rather than leaving it to a convention.
//
// # Why the diff and the history are not nested under the application
//
// `['diff', app]` rather than `['applications', app, 'diff']`, decided in #202
// and load-bearing here: prefix matching means a nested key could not be left
// out of an invalidation of the application it belongs to, and two rows below
// have to leave exactly that out. Do not tidy them into a nest.

import { isEventType, type ControllerEventType } from './events'

/**
 * One thing to invalidate, shaped as react-query's filter.
 *
 * Stated locally rather than imported so the table stays plain data: it is
 * policy, and a test of it should not need a QueryClient to read.
 */
export interface Invalidation {
  queryKey: readonly unknown[]
  /** See listKey: true means this key only, not everything under it. */
  exact?: boolean
}

/** Every application in one document; read by the list screen. */
export const listKey = ['applications'] as const

/** The controller's own state, as distinct from any application's. */
export const controllerKey = ['status'] as const

/** One application's detail document, read by the layout and the overview. */
export function applicationKey(app: string) {
  return ['applications', app] as const
}

/** What a sync would change. */
export function diffKey(app: string) {
  return ['diff', app] as const
}

/** Every declared release's recorded revisions. */
export function historyKey(app: string) {
  return ['history', app] as const
}

/** The documents this UI holds. Named, so the table below reads as policy. */
type Document = 'detail' | 'list' | 'diff' | 'history' | 'controller'

/**
 * What each event type says to re-read.
 *
 * Every row invalidates the detail and the list, and neither is a judgement
 * call: each of these events is raised by a reconcile that has just written the
 * application's status, and the list is that same document a row at a time.
 * What a row actually decides is the other three.
 */
const table: Record<ControllerEventType, readonly Document[]> = {
  // The diff, because the plan is already recorded by the time this is raised:
  // reconcile records the freshly rendered plan and only then calls apply, which
  // dispatches this. Not the history — nothing has been applied yet, so the
  // chart engine has written nothing.
  'sync-started': ['detail', 'list', 'diff'],

  // Everything about the application: the releases were applied, so the engine
  // wrote revisions, and the re-plan that follows an apply is what the diff
  // endpoint reads.
  'sync-succeeded': ['detail', 'list', 'diff', 'history'],

  // The same set, and that is the point rather than copy-paste: a sync fails
  // part-way. An apply that got through two of three releases wrote two
  // revisions and left a diff naming the third, so reading failure as "nothing
  // happened" is how the history tab stays a release behind reality.
  'sync-failed': ['detail', 'list', 'diff', 'history'],

  // The repository moved. reconcile gates this event on the plan having installs
  // or upgrades, which is exactly the set drift.Diffs reports, so the diff has
  // moved by construction. Nothing was deployed, so the history has not.
  'drift-detected': ['detail', 'list', 'diff'],

  // Not the diff. The swarm moved and the rendered manifest did not — reconcile
  // raises this off the live ServiceSpec comparison, for a release whose plan
  // says unchanged. That distinction is the entire reason live drift is its own
  // event type rather than a drift-detected with different prose, and folding
  // the diff back in here would throw the distinction away.
  'live-drift-detected': ['detail', 'list'],

  // Not the history. notify.go states it outright — no chart revision is
  // written, because the desired state did not change — and reconcile.converge
  // is why: a live-drifted release is planned as unchanged by construction,
  // Engine.Apply skips those, so the correction deploys the manifest directly
  // and records nothing.
  //
  // Not the diff either, from the other side of the same fact: converge only
  // ever touches a release the plan marked unchanged, so the set drift.Diffs
  // reports cannot have moved. Where a correction did coincide with an apply,
  // converge runs inside it and the sync-succeeded that follows covers both.
  'drift-converged': ['detail', 'list'],

  // Everything, including the controller's own status. This is the only event
  // that reports something destroyed, and what went may be a whole release —
  // uninstalled through the engine, so its revisions went with it — or the
  // deployed resources of a whole application that has left the app set, which
  // is a different application count than the status document last reported.
  'resources-pruned': ['detail', 'list', 'diff', 'history', 'controller'],

  // Nothing was deleted: what this names is still deployed and still unmanaged,
  // and the next reconcile tries again. No manifest and no revision moved.
  'prune-failed': ['detail', 'list'],
}

/**
 * What an event type this build does not know invalidates.
 *
 * The application's whole subtree and both collections. A controller ahead of
 * this build is the case — it added a ninth type and this one has no rule for it
 * — and over-invalidating costs a refetch, where silently dropping the signal
 * costs a screen that never updates and says nothing about it.
 */
const unrecognised: readonly Document[] = ['detail', 'list', 'diff', 'history', 'controller']

/**
 * invalidatedBy is the map: what to re-read now that this happened to this
 * application.
 *
 * It takes the wire type as a string rather than a narrowed one, because that is
 * what arrives — see events.ts — and the unrecognised case is a row of this
 * table rather than something a caller has to remember to handle.
 */
export function invalidatedBy(type: string, app: string): Invalidation[] {
  const documents = isEventType(type) ? table[type] : unrecognised
  return documents.map((document) => filterFor(document, app))
}

function filterFor(document: Document, app: string): Invalidation {
  switch (document) {
    case 'detail':
      return { queryKey: applicationKey(app) }
    // exact, where nothing else here is: ['applications'] is a prefix of every
    // ['applications', app], so a plain invalidation of the list would also
    // invalidate every application's detail — including the one on screen, which
    // would then refetch over an event that said nothing about it.
    case 'list':
      return { queryKey: listKey, exact: true }
    case 'diff':
      return { queryKey: diffKey(app) }
    case 'history':
      return { queryKey: historyKey(app) }
    case 'controller':
      return { queryKey: controllerKey }
  }
}
