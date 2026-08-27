// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

export type DotTone = 'ok' | 'warn' | 'bad' | 'info' | 'muted'

/**
 * A pre-attentive status dot for tables, trees, and badges.
 *
 * Provides instant visual scanning before reading text. Renders as an aria-hidden
 * span by default, or with an aria-label if a label is supplied.
 */
export function Dot({
  tone = 'info',
  pulse = false,
  label,
}: {
  tone?: DotTone
  pulse?: boolean
  label?: string
}) {
  const className = `dot dot-${tone}${pulse ? ' dot-pulse' : ''}`
  if (label !== undefined && label !== '') {
    return <span className={className} role="status" aria-label={label} />
  }
  return <span className={className} aria-hidden="true" />
}
