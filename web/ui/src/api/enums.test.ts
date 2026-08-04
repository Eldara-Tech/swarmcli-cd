// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { describe, expect, it } from 'vitest'

import { decodeEnum, healthStates, syncStates } from './enums'

describe('decodeEnum mirrors unmarshalEnum', () => {
  it('accepts a member', () => {
    expect(decodeEnum('out-of-sync', syncStates)).toBe('out-of-sync')
    expect(decodeEnum('progressing', healthStates)).toBe('progressing')
  })

  it('reads a name it does not know as unknown, so a newer controller does not break this one', () => {
    expect(decodeEnum('quiesced', syncStates)).toBe('unknown')
    // The empty string is not a wire value — Go's zero member marshals as
    // "unknown" — but it is not a member either, so it lands in the same place.
    expect(decodeEnum('', syncStates)).toBe('unknown')
  })

  it('refuses a value that is not a string at all', () => {
    // The asymmetry that mirrors the Go decoder: an unrecognised name is a
    // newer controller and is expected; a number is a broken producer, and
    // rendering "unknown" for it would hide that.
    expect(() => decodeEnum(3, syncStates)).toThrow(TypeError)
    expect(() => decodeEnum(null, syncStates)).toThrow(TypeError)
  })
})
