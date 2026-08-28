// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

export type DotTone = 'ok' | 'warn' | 'bad' | 'info' | 'muted'

/**
 * A pre-attentive status dot for tables, trees, and badges.
 *
 * Always decorative, and therefore always aria-hidden: every dot in this console
 * sits beside the same state written as text, so naming it would make a screen
 * reader say everything twice. It carried an optional `role="status"` label for
 * the case where it did not — no caller ever had that case, and `status` is an
 * aria-live region, so the unreached branch would have announced every dot in a
 * table each time the list refetched.
 */
export function Dot({ tone = 'info', pulse = false }: { tone?: DotTone; pulse?: boolean }) {
  return <span className={`dot dot-${tone}${pulse ? ' dot-pulse' : ''}`} aria-hidden="true" />
}
