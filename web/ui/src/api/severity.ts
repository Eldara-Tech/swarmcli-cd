// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// How bad one application is, folded from its three axes.
//
// It lives beside appset.ts for the same reason that file gives: more than one
// screen has to agree. The list draws a dot, the topology draws a node, and
// diagnostics draws a score, and all three are answering the same question —
// "how is this application doing" — off the same document. Written three times
// they disagreed, and they disagreed precisely on the state nothing had thought
// about: an application reporting `unknown` read as a green dot on two screens
// and a neutral node on the third.

import { decodeEnum, driftStates, healthStates, syncStates } from './enums'
import type { HealthState } from './enums'
import type { View } from './types'

/**
 * The four readings, worst first.
 *
 * `unknown` is the one worth explaining. application/enum.go makes every enum's
 * Unknown member the *zero* value, marshalled as the name "unknown" — so an
 * application the controller has accepted but not yet reconciled arrives with
 * both axes reading unknown, and so does one whose state a newer controller
 * describes in a word this build has never heard. Neither is a fault and
 * neither is an all-clear, so it is its own tier rather than being folded into
 * `ok` (which is what let a controller twenty seconds into its first start
 * report 100/100) or into `warn` (which would paint a whole fleet amber for
 * being young).
 */
export type Severity = 'crit' | 'warn' | 'unknown' | 'ok'

/** The dot/chip tone each reading wears. The console's palette has four. */
export type Tone = 'bad' | 'warn' | 'info' | 'ok'

const tones: Record<Severity, Tone> = {
  crit: 'bad',
  warn: 'warn',
  unknown: 'info',
  ok: 'ok',
}

/** toneOf maps a reading to the tone every screen draws it in. */
export function toneOf(severity: Severity): Tone {
  return tones[severity]
}

/** One application's worst finding, and the sentence that names it. */
export interface Assessment {
  severity: Severity
  reason: string
}

/**
 * assess reduces one application's three axes to a single worst finding.
 *
 * A failed reconcile outranks everything, because every other field on that row
 * is then the last successful observation and not the current one (the list
 * marks the same row stale for the same reason). A dead health is next; drift,
 * being out of sync and a rollout in flight are warnings. Only then is a state
 * nobody has reported on read as unknown — a real warning outranks not knowing,
 * so an application that is both out of sync and unhealthy-in-an-unknown-way is
 * reported as the thing it is rather than as a shrug.
 *
 * Each enum is decoded rather than compared, so a state a newer controller
 * added falls to `unknown` instead of matching no case and silently reading as
 * an all-clear.
 */
export function assess(view: View): Assessment {
  const { status } = view

  if (status.error !== undefined && status.error !== '') {
    return { severity: 'crit', reason: `Last reconcile failed: ${status.error}` }
  }

  const health = decodeEnum(status.health.state, healthStates)
  if (health === 'degraded' || health === 'missing') {
    return { severity: 'crit', reason: `Health is ${health}` }
  }

  // Absent is "not asked" — manifest mode never checks — and is not a finding.
  // Only a drift the controller looked for and found is.
  if (status.drift !== undefined && decodeEnum(status.drift.state, driftStates) === 'detected') {
    return { severity: 'warn', reason: 'Live drift detected against the running swarm' }
  }

  const sync = decodeEnum(status.sync.state, syncStates)
  if (sync === 'out-of-sync') {
    return { severity: 'warn', reason: 'Out of sync with the repository' }
  }
  if (health === 'progressing') {
    return { severity: 'warn', reason: 'A rollout is still progressing' }
  }

  if (health === 'unknown' || sync === 'unknown') {
    return {
      severity: 'unknown',
      reason: 'Not yet reconciled — the controller has not reported on this application',
    }
  }

  return { severity: 'ok', reason: 'Synced and healthy' }
}

/**
 * healthToneOf is the same reading for a release or a service, which carry a
 * health state and nothing else.
 *
 * Separate from assess because they are not applications: there is no reconcile
 * error to outrank the axis and no sync or drift to fold in, so passing them
 * through the function above would mean inventing the two fields it reads.
 */
export function healthToneOf(state: HealthState): Tone {
  switch (decodeEnum(state, healthStates)) {
    case 'healthy':
      return 'ok'
    case 'degraded':
    case 'missing':
      return 'bad'
    case 'progressing':
      return 'warn'
    default:
      return 'info'
  }
}
