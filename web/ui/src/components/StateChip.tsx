// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import {
  compatStates,
  decodeEnum,
  driftStates,
  healthStates,
  syncStates,
  type CompatState,
  type HealthState,
  type SyncState,
} from '../api/enums'
import type { Drift } from '../api/types'

/**
 * The tone each enum member is drawn in.
 *
 * One table across four enums rather than one per chip. Their members are
 * disjoint apart from "unknown", and a single table is the only arrangement in
 * which the same word cannot come out two different colours on two screens.
 *
 * "unknown" is muted and not red on purpose. It is what an enum decodes to when
 * this build has never heard of the name — a controller newer than the tab,
 * which application/enum.go exists to make survivable — so colouring it as a
 * failure would make every upgrade look like an outage.
 */
const tones: Record<string, string | undefined> = {
  synced: 'good',
  healthy: 'good',
  none: 'good',
  ok: 'good',
  progressing: 'busy',
  'out-of-sync': 'warn',
  detected: 'warn',
  missing: 'warn',
  degraded: 'bad',
  incompatible: 'bad',
  unknown: 'muted',
}

function Chip({ value }: { value: string }) {
  return <span className={`chip chip-${tones[value] ?? 'muted'}`}>{value}</span>
}

/**
 * Every chip decodes before it renders, and that is not the belt-and-braces it
 * looks like.
 *
 * src/api/types.ts types these fields as their unions, but a union describes
 * what the controller *this build knows about* sends. A newer one sends a name
 * outside it, and TypeScript has erased itself by then. decodeEnum turns that
 * into "unknown" — which is precisely the contract application/enum.go
 * implements at the other end — instead of letting an unrecognised, untoned
 * word through.
 */
export function SyncChip({ state }: { state: SyncState }) {
  return <Chip value={decodeEnum(state, syncStates)} />
}

export function HealthChip({ state }: { state: HealthState }) {
  return <Chip value={decodeEnum(state, healthStates)} />
}

export function CompatChip({ status }: { status: CompatState }) {
  return <Chip value={decodeEnum(status, compatStates)} />
}

/**
 * DriftCell renders the live axis for one application, dash and all.
 *
 * The dash is the point. An absent `drift` means driftDetection is `manifest`
 * and the question was never asked, while "unknown" means it was asked and
 * could not be answered — a genuinely alarming state that has to keep the word
 * to itself. Same distinction, same reason, as controller/app.go's driftColumn.
 */
export function DriftCell({ drift }: { drift?: Drift }) {
  if (drift === undefined) {
    return (
      <span className="not-asked" title="driftDetection is manifest, so live drift was never checked">
        –
      </span>
    )
  }
  return <Chip value={decodeEnum(drift.state, driftStates)} />
}
