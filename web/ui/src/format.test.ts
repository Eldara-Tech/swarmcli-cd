// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { describe, expect, it } from 'vitest'

import { zeroTimestamp } from './api/types'
import { chartRef, destination, formatInstant, isUnset, plural, serviceCounts, shortRevision } from './format'

describe('shortRevision', () => {
  it('abbreviates the way git does and leaves a short one alone', () => {
    expect(shortRevision('9f3c1abd4e5f6789')).toBe('9f3c1ab')
    expect(shortRevision('9f3c1ab')).toBe('9f3c1ab')
    expect(shortRevision('')).toBe('')
  })
})

describe('isUnset', () => {
  it('catches the zero time, which every NaN guard lets through', () => {
    expect(isUnset(zeroTimestamp)).toBe(true)
    expect(isUnset(undefined)).toBe(true)
    expect(isUnset('')).toBe(true)
    expect(isUnset('2026-07-22T09:41:10Z')).toBe(false)
    // Why the check cannot be Number.isNaN: the zero time parses fine.
    expect(Number.isNaN(Date.parse(zeroTimestamp))).toBe(false)
  })
})

describe('formatInstant', () => {
  it('returns an unparseable value verbatim rather than "Invalid Date"', () => {
    expect(formatInstant('not a time')).toBe('not a time')
  })

  it('renders a real timestamp as something other than itself', () => {
    // The locale decides the text, so the assertion is that it was formatted
    // at all — a test pinned to en-US would fail on a runner with a different
    // default and tell nobody anything.
    expect(formatInstant('2026-07-22T09:41:10Z')).not.toBe('2026-07-22T09:41:10Z')
  })
})

describe('the small renderings', () => {
  it('renders service counts, plurals, chart refs and destinations', () => {
    expect(serviceCounts({ healthy: 3, total: 4 })).toBe('3/4')
    expect(plural(1, 'application')).toBe('1 application')
    expect(plural(0, 'application')).toBe('0 applications')
    expect(chartRef({ release: 'traefik', path: './charts/traefik' })).toBe('./charts/traefik')
    expect(chartRef({ release: 'traefik', ref: 'oci://r/traefik', version: '1.2.3' })).toBe('oci://r/traefik 1.2.3')
    expect(chartRef({ release: 'traefik', ref: 'oci://r/traefik' })).toBe('oci://r/traefik')
    expect(destination({})).toBe('local swarm')
    expect(destination({ swarm: 'eu-1' })).toBe('eu-1')
  })
})
