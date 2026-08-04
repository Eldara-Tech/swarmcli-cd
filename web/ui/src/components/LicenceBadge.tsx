// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import type { ReactNode } from 'react'

import { licenceStatuses, useCapabilities, type Licence, type LicenceStatus } from '../api/discovery'
import { decodeEnum } from '../api/enums'
import { plural } from '../format'
import { Instant } from './Instant'

/**
 * The licence badge: what state this build's licence is in, and what to do
 * about it.
 *
 * # It renders nothing in the free build
 *
 * `licence` is null in a build with no licensed module linked — the Apache-2.0
 * one — and nothing is drawn for it. That is the acceptance criterion of #178
 * rather than a shortcut: a chip reading "community" in a build that has no
 * licence to report is a control that can never say anything else, and the free
 * product must not grow one. The state that *is* worth a badge in a build with
 * everything switched off is a licensed one whose licence lapsed, and that one
 * carries a licence object saying so.
 *
 * # grace is why the five statuses are five
 *
 * A licence in its grace period is granting features and is not valid, so a
 * badge that folded it into either would be wrong in the direction that matters:
 * it reads healthy right up to the morning the features stop. What is rendered
 * for it is how long ago it expired, in the loudest treatment short of a
 * failure.
 *
 * What it cannot render is the day the grace ends, because that is not on the
 * wire: the length of the grace period is the licensed module's, feature.Licence
 * carries only ExpiresAt, and a badge that named a deadline it had derived from
 * a guessed period would be a badge that lies about a date. "expired 4 days ago"
 * is what the document supports, and it is said out loud here so that the next
 * reader adds the field rather than the arithmetic.
 */
export function LicenceBadge() {
  const licence = useCapabilities().data?.licence
  // undefined while the request is in flight or after it failed, null in a
  // build with no licensed module. Both render nothing, and that is what makes
  // a controller with no capability endpoint indistinguishable from a free one.
  if (licence === undefined || licence === null) return null

  // Decoded rather than switched on raw, for the reason api/enums.ts gives: a
  // controller ahead of this build reports a status the union does not have,
  // and it has to render as something rather than falling through every case.
  const status = decodeEnum(licence.status, licenceStatuses)
  const { tone, label, detail } = describe(licence, status)

  return (
    <p className="licence" role="status" data-testid="licence-badge">
      <span className={`chip chip-${tone}`}>{label}</span>
      {detail !== null && <span className="licence-detail">{detail}</span>}
    </p>
  )
}

interface Rendering {
  /** The chip's colour, from the four index.css already defines. */
  tone: 'good' | 'warn' | 'bad' | 'muted'
  label: string
  detail: ReactNode
}

function describe(licence: Licence, status: LicenceStatus | 'unknown'): Rendering {
  switch (status) {
    case 'valid':
      return {
        tone: 'good',
        label: 'licence valid',
        detail:
          licence.expiresAt === null ? (
            `${licence.tier}, perpetual`
          ) : (
            <>
              {licence.tier}, expires <Instant at={licence.expiresAt} />
            </>
          ),
      }
    case 'grace':
      // Warn rather than bad: the features are still on, and colouring this the
      // same as a build that has already lost them would leave nothing to
      // escalate to when it does.
      return {
        tone: 'warn',
        label: 'grace period',
        detail: `${expiredWhen(licence.expiresAt)}, still granting features — renew now`,
      }
    case 'expired':
      return { tone: 'bad', label: 'licence expired', detail: `${expiredWhen(licence.expiresAt)}, features are off` }
    case 'invalid':
      // The remedy is a licence from the vendor rather than a change to this
      // deployment, and the badge says which so that nobody spends an afternoon
      // on the controller's configuration.
      return { tone: 'bad', label: 'licence invalid', detail: 'it did not verify — ask for a replacement' }
    case 'absent':
      // Nothing is wrong: the module is linked and no licence is installed.
      return { tone: 'warn', label: 'no licence', detail: 'install one to turn the licensed features on' }
    default:
      return { tone: 'muted', label: `licence ${licence.status}`, detail: 'a status this build does not know' }
  }
}

/**
 * expiredWhen says how long ago, in whole days.
 *
 * Whole days rather than an exact instant because the reader's question is how
 * much time is left, not when the certificate was minted; the exact value is a
 * click away in the capability document. A licence with no expiry that reports
 * one of the expired statuses is a contradiction the controller sent, so it
 * says only that it expired rather than inventing a day for it.
 */
function expiredWhen(expiresAt: string | null): string {
  if (expiresAt === null) return 'expired'
  const parsed = Date.parse(expiresAt)
  if (Number.isNaN(parsed)) return 'expired'
  const days = Math.floor((Date.now() - parsed) / 86_400_000)
  return days <= 0 ? 'expired today' : `expired ${plural(days, 'day')} ago`
}
