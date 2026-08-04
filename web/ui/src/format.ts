// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The renderings every screen shares.
//
// Each one mirrors its counterpart in controller/app.go — short(), observed(),
// services(), chartRef(), destination() — so that `swarmcli-cd app get edge` and
// the detail screen say the same thing about the same document. An operator
// comparing the two should never have to work out whether "9f3c1ab" and a full
// SHA name the same commit.

import { zeroTimestamp, type ChartSource, type Destination, type ServiceCounts, type Timestamp } from './api/types'

/**
 * shortRevision abbreviates a commit the way git does.
 *
 * The full value stays in the DOM as a title, because the short form is for
 * reading and the long one is what somebody pastes into `git show`.
 */
export function shortRevision(revision: string): string {
  return revision.length <= 7 ? revision : revision.slice(0, 7)
}

/**
 * isUnset reports whether a timestamp was never set.
 *
 * The one guard on this file worth reading twice. Go's zero time marshals as
 * "0001-01-01T00:00:00Z" and `loadedAt` carries no omitempty, so it arrives on
 * every controller that has never loaded an app set — and Date.parse of it is a
 * large negative number rather than NaN. A screen guarding on NaN therefore
 * passes the check and renders the year 1, or "56 years ago" from a relative
 * formatter, for the loudest failure the controller has.
 *
 * A type predicate rather than a boolean so that the caller's other branch
 * narrows to a string it can format; there is exactly one rule here and no
 * caller may restate it.
 */
export function isUnset(at: Timestamp | undefined): at is undefined | '' | typeof zeroTimestamp {
  return at === undefined || at === '' || at === zeroTimestamp
}

/**
 * formatInstant renders a timestamp in the reader's own locale.
 *
 * A value that will not parse is returned verbatim rather than rendered as
 * "Invalid Date": whatever the controller actually sent is the useful thing to
 * put in front of whoever has to fix it.
 */
export function formatInstant(at: Timestamp): string {
  const parsed = Date.parse(at)
  return Number.isNaN(parsed) ? at : new Date(parsed).toLocaleString()
}

/** serviceCounts renders the "3/4" a row shows. */
export function serviceCounts(counts: ServiceCounts): string {
  return `${counts.healthy}/${counts.total}`
}

/** chartRef names a chart source the way the spec declares it: a path within the repository, or a pinned repository reference. */
export function chartRef(chart: ChartSource): string {
  if (chart.path !== undefined && chart.path !== '') return chart.path
  const ref = chart.ref ?? ''
  if (chart.version !== undefined && chart.version !== '') return `${ref} ${chart.version}`
  return ref
}

/** destination names where an application deploys. Empty is the swarm the controller runs in. */
export function destination(dest: Destination): string {
  return dest.swarm === undefined || dest.swarm === '' ? 'local swarm' : dest.swarm
}

/** plural is the "1 application" / "2 applications" the list header counts with. */
export function plural(count: number, singular: string): string {
  return `${count} ${singular}${count === 1 ? '' : 's'}`
}
