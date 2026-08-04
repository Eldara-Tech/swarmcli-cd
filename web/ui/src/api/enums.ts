// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The wire enums, and the one rule for reading them.
//
// Mirrored from application/enum.go, whose contract is stated there once and
// implemented by every member: an enum marshals as its lowercase name, and the
// Go decoder maps a name it does not recognise to the Unknown member rather
// than failing. A newer controller reporting a state this build has never heard
// of leaves the screen showing "unknown" instead of erroring out.
//
// The lists below therefore include "unknown", because it is a value that
// arrives on the wire — the Go zero member is the empty string and marshals as
// that name — and the empty string never does.

/** The name every enum falls back to; see decodeEnum. */
export const unknownMember = 'unknown'

export const syncStates = ['synced', 'out-of-sync', 'unknown'] as const
export type SyncState = (typeof syncStates)[number]

export const healthStates = ['healthy', 'progressing', 'degraded', 'missing', 'unknown'] as const
export type HealthState = (typeof healthStates)[number]

export const syncActions = ['unchanged', 'install', 'upgrade', 'unknown'] as const
export type SyncAction = (typeof syncActions)[number]

export const compatStates = ['ok', 'incompatible', 'unknown'] as const
export type CompatState = (typeof compatStates)[number]

export const driftDetections = ['manifest', 'live', 'unknown'] as const
export type DriftDetection = (typeof driftDetections)[number]

export const driftStates = ['none', 'detected', 'unknown'] as const
export type DriftState = (typeof driftStates)[number]

export const driftReasons = ['modified', 'missing', 'unexpected', 'rolled-back', 'unknown'] as const
export type DriftReason = (typeof driftReasons)[number]

/**
 * ResourceKind names the kind of a drifted resource: "network", "config" or
 * "secret" today.
 *
 * It is the one enum with no unknown fallback — application.go declares it as a
 * bare string with neither MarshalJSON nor UnmarshalJSON — so it is an open
 * string here too, and is rendered verbatim. Coercing an unrecognised value to
 * "unknown" would hide a kind a newer controller added, which is the one thing
 * an operator looking at a drift panel most needs to see.
 */
export type ResourceKind = string

/**
 * decodeEnum reads one enum value off the wire.
 *
 * It mirrors application/enum.go's unmarshalEnum exactly, including the
 * asymmetry that looks like an oversight and is not: a *name* it does not
 * recognise is not an error and decodes to "unknown", because a controller
 * ahead of this build is expected; a value that is not a string at all is an
 * error, because being sent a number is a broken producer rather than a newer
 * one, and quietly rendering "unknown" for it would hide that.
 *
 * The wire types describe these fields as their unions already, so this is not
 * a parse step every response goes through — it is what a screen calls on the
 * one field it is about to switch on, so that a value outside the union cannot
 * fall through every case and render nothing.
 */
export function decodeEnum<T extends string>(
  value: unknown,
  members: readonly T[],
): T | typeof unknownMember {
  if (typeof value !== 'string') {
    throw new TypeError(`expected a string enum member, got ${typeof value}`)
  }
  return (members as readonly string[]).includes(value) ? (value as T) : unknownMember
}
