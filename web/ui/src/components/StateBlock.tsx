// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import type { ReactNode } from 'react'

import { Icon, type IconName } from './Icon'

/**
 * The three states a document can be in before it is a screen — loading, empty,
 * failed — as designed moments rather than a bare paragraph.
 *
 * Each keeps its message in a single text node so a caller's exact wording (and
 * the tests that assert it) survives being wrapped: `<Empty>No applications.</Empty>`
 * still reads as the text "No applications." to a screen reader and a query.
 */

/** A skeleton in the shape of the data that is coming. `rows` sizes it. */
export function Loading({ rows = 3 }: { rows?: number }) {
  return (
    <div className="state-loading" role="status" aria-label="Loading">
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="skeleton" />
      ))}
    </div>
  )
}

/** An empty result: a glyph and the message. */
export function Empty({ icon = 'app', children }: { icon?: IconName; children?: ReactNode }) {
  return (
    <div className="state-empty">
      <Icon name={icon} size={28} />
      {children !== undefined && <p className="empty">{children}</p>}
    </div>
  )
}

/** A read that failed. role="alert" so it is announced, since it replaces content. */
export function ErrorState({ message }: { message: string }) {
  return (
    <div className="state-error" role="alert">
      <Icon name="error" size={28} />
      <p className="error">{message}</p>
    </div>
  )
}
