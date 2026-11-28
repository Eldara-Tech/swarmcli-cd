// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The renderings every screen shares.
//
// Each one mirrors its counterpart in controller/app.go — short(), observed(),
// services(), chartRef(), destination() — so that `swarmcli-cd app get edge` and
// the detail screen say the same thing about the same document. An operator
// comparing the two should never have to work out whether "9f3c1ab" and a full
// SHA name the same commit.

import {
  zeroTimestamp,
  type ChartSource,
  type Destination,
  type ServiceCounts,
  type ServiceStatus,
  type Timestamp,
} from './api/types'

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
 * commitURL is the forge's page for a commit, or undefined for a repository
 * that has none: the controller takes https:// or an absolute path (supportedURL
 * in git/git.go), and only the first is served by anything with a browser.
 *
 * `/commit/` is what GitHub, Gitea and Forgejo serve and GitLab redirects to its
 * own `/-/commit/`, so one rule covers a self-hosted forge whose host names no
 * product; Bitbucket Cloud alone says `/commits/` (#367). Built from origin and
 * path, so credentials written into the URL stay out of the link.
 */
export function commitURL(repoURL: string, revision: string): string | undefined {
  let url: URL
  try {
    url = new URL(repoURL)
  } catch {
    return undefined
  }
  if (url.protocol !== 'https:') return undefined
  // The trim webhook/match.go's normalise applies to the same field.
  const repo = url.pathname.replace(/\/+$/, '').replace(/\.git$/, '')
  const commit = url.hostname === 'bitbucket.org' ? 'commits' : 'commit'
  return `${url.origin}${repo}/${commit}/${revision}`
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

/**
 * tasksUp is how many of a service's tasks have done what was asked of them.
 *
 * Running alone is not the answer for a one-shot. A migration that exited 0
 * leaves nothing running, so a ratio built from `running` reads 0/1 on a
 * release that finished — next to a health chip saying healthy, which is the
 * pair an operator reads as a failure (#290). `completed` is the controller's
 * count of the tasks that ended cleanly, absent on every service that is not a
 * job, and the chart engine's own replica column adds it the same way.
 *
 * Swarm's own job modes are the exception. Their `desired` is a number of
 * completions, so a task still running is not done yet: counting it would read
 * 2/2 on a progressing service whose message says 0/2 completed (#300). There
 * only `completed` counts, as in the engine's column and `docker service ls`,
 * whose mode names these are.
 */
export function tasksUp(service: ServiceStatus): number {
  if (service.mode === 'replicated job' || service.mode === 'global job') return service.completed ?? 0
  return service.running + (service.completed ?? 0)
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

/**
 * ago says how long before `now` something happened, in the one unit a reader
 * judges staleness by — "4 days ago" — and mirrors `ago` in controller/app.go,
 * so the CLI and the screen give the same answer.
 *
 * Empty for a time that was never set or will not parse: an age is a gloss on a
 * time shown beside it, and that time already says "never" or shows the raw
 * value (see Instant and formatInstant). A time ahead of now, which is the
 * controller's clock disagreeing with this one, reads as just now.
 */
export function ago(at: Timestamp | undefined, now: number = Date.now()): string {
  if (isUnset(at)) return ''
  const then = Date.parse(at)
  if (Number.isNaN(then)) return ''
  const minutes = Math.floor((now - then) / 60_000)
  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${plural(minutes, 'minute')} ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 48) return `${plural(hours, 'hour')} ago`
  return `${plural(Math.floor(hours / 24), 'day')} ago`
}
