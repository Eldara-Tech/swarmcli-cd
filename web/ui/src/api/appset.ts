// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Which of the app set's shapes a controller is in.
//
// It lives beside the wire types rather than in the screen that draws it
// because two screens have to agree: the controller screen renders the shape,
// and the applications list renders a one-line pointer to it. A stale app set
// means every row in that list describes a set the controller is refusing, so a
// list with no way to see that is wrong in a way nothing on it can show.

import { isUnset } from '../format'
import type { ControllerStatus } from './types'

/**
 * The five states, four of which the screens draw differently.
 *
 * Three of them are failures and docs/api.md is explicit that they are
 * different failures — which is why this is an enumeration and not a boolean:
 *
 *  - `stale`: a newer set is being refused and what is running is the last one
 *    that validated. The applications are real; the file on disk is ahead.
 *  - `partial`: a set that loaded and could not be applied in full. The
 *    applications are real and one of them is not what the file says.
 *  - `never-loaded`: no set has ever loaded. The list is empty for a reason
 *    that has nothing to do with anybody's applications, and it is the loudest
 *    of the three.
 *
 * `unwired` is the fourth shape and is not a failure at all: api.go's status
 * handler answers with a zero ControllerStatus when it has no controller, so
 * `mode` is empty and every field below it is a zero value — including
 * `loadedAt`, which would otherwise read as never-loaded. It has to be checked
 * first, and checking it first is the whole reason this function exists rather
 * than three conditions in a component.
 */
export type AppSetShape = 'unwired' | 'never-loaded' | 'stale' | 'partial' | 'ok'

/** appSetShape classifies one controller status. */
export function appSetShape(status: ControllerStatus): AppSetShape {
  const set = status.appSet
  if (set.mode === '') return 'unwired'
  // Both halves, as docs/api.md states it: "with `applications` at zero and no
  // `loadedAt`". A set that loaded and legitimately declares nothing has a
  // loadedAt, and a controller mid-first-load has neither — the difference
  // between "nothing to do" and "nothing works".
  if (isUnset(set.loadedAt) && status.applications === 0) return 'never-loaded'
  if (set.stale) return 'stale'
  if (set.error !== undefined && set.error !== '') return 'partial'
  return 'ok'
}
