// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { formatInstant, isUnset } from '../format'
import type { Timestamp } from '../api/types'

/**
 * Instant says when something happened, or that it never did.
 *
 * A component rather than a call to formatInstant from six places, because the
 * thing it has to get right is not the formatting: an unset time arrives as
 * Go's zero value, and every naive guard renders that as the year 1. Keeping
 * the check on this side of the JSX means a screen cannot forget it.
 */
export function Instant({ at }: { at?: Timestamp }) {
  if (isUnset(at)) return <span className="muted">never</span>
  return (
    <time dateTime={at} title={at}>
      {formatInstant(at)}
    </time>
  )
}
