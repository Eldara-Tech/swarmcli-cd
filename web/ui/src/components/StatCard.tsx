// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import type { ReactNode } from 'react'

import { Icon, type IconName } from './Icon'

/**
 * The tone a stat's number is drawn in. `default` is the plain ink used for a
 * count that is neither good nor bad on its own — how many applications there
 * are — where a colour would imply a verdict the number does not carry.
 */
export type Tone = 'default' | 'ok' | 'warn' | 'bad' | 'accent'

const toneClass: Record<Tone, string> = {
  default: '',
  ok: 'tone-ok',
  warn: 'tone-warn',
  bad: 'tone-bad',
  accent: 'tone-accent',
}

/**
 * One KPI tile: a caps label, a large value, an optional unit and sub-line.
 *
 * No progress bar, deliberately: a data-driven width is an inline style, and the
 * CSP this UI serves under forbids the style attribute (see index.css). The
 * number and its "/ total" carry the same information a bar would.
 */
export function StatCard({
  label,
  value,
  unit,
  sub,
  tone = 'default',
  icon,
}: {
  label: string
  value: ReactNode
  unit?: string
  sub?: ReactNode
  tone?: Tone
  icon?: IconName
}) {
  return (
    <div className={`stat-card stat-${tone}`}>
      <div className="stat-top">
        <span className="stat-label label-caps">{label}</span>
        {icon !== undefined && <Icon name={icon} size={16} />}
      </div>
      <span className="stat-value">
        <span className={toneClass[tone]}>{value}</span>
        {unit !== undefined && <span className="stat-unit">{unit}</span>}
      </span>
      {sub !== undefined && <span className="muted">{sub}</span>}
    </div>
  )
}
