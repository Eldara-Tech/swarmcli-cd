// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The one status fold, tested where it lives.
//
// It was three copies in three screens before this, which is why the case below
// that matters most — `unknown` — read differently on each of them. Testing the
// function rather than three renderings is what keeps them from drifting apart
// again.

import { describe, expect, it } from 'vitest'

import { assess, healthToneOf, toneOf } from './severity'
import type { View } from './types'
import { clone } from '../test/fakeApi'
import viewFull from '../test/fixtures/view-full.json'

function view(mutate: (v: View) => void): View {
  const v = clone(viewFull) as unknown as View
  delete v.status.releases
  delete v.status.drift
  delete v.status.error
  mutate(v)
  return v
}

describe('assess', () => {
  it('reads a reconcile error as critical, over every other axis', () => {
    // Every other field on the row is then the last successful observation, so
    // a green sync beside a failed reconcile must not win.
    const v = view((v) => {
      v.status.error = 'clone failed'
      v.status.sync.state = 'synced'
      v.status.health.state = 'healthy'
    })
    expect(assess(v)).toEqual({ severity: 'crit', reason: 'Last reconcile failed: clone failed' })
  })

  it('reads degraded and missing health as critical', () => {
    for (const state of ['degraded', 'missing'] as const) {
      const v = view((v) => {
        v.status.health.state = state
      })
      expect(assess(v).severity).toBe('crit')
    }
  })

  it('reads drift, out-of-sync and a rollout as warnings', () => {
    const drifted = view((v) => {
      v.status.sync.state = 'synced'
      v.status.health.state = 'healthy'
      v.status.drift = { state: 'detected', services: 1 }
    })
    expect(assess(drifted).severity).toBe('warn')

    const behind = view((v) => {
      v.status.sync.state = 'out-of-sync'
      v.status.health.state = 'healthy'
    })
    expect(assess(behind).severity).toBe('warn')

    const rolling = view((v) => {
      v.status.sync.state = 'synced'
      v.status.health.state = 'progressing'
    })
    expect(assess(rolling).severity).toBe('warn')
  })

  it('reads an unreported application as unknown, and never as clear', () => {
    // application/enum.go makes Unknown the zero member of every enum, so this
    // is what an application the controller has accepted and not yet reconciled
    // actually looks like on the wire. Folded into `ok` it scored a controller
    // that had reconciled nothing at 100/100.
    const fresh = view((v) => {
      v.status.sync.state = 'unknown'
      v.status.health.state = 'unknown'
    })
    const got = assess(fresh)
    expect(got.severity).toBe('unknown')
    expect(got.severity).not.toBe('ok')
    expect(got.reason).toMatch(/Not yet reconciled/)
  })

  it('lets a real warning outrank not knowing', () => {
    const v = view((v) => {
      v.status.sync.state = 'out-of-sync'
      v.status.health.state = 'unknown'
    })
    expect(assess(v).severity).toBe('warn')
  })

  it('reads a state this build has never heard of as unknown, not as clear', () => {
    // A controller ahead of this one. decodeEnum maps the name to `unknown`
    // rather than matching no case and falling through to the all-clear.
    const v = view((v) => {
      v.status.sync.state = 'quantum-superposition' as never
      v.status.health.state = 'healthy'
    })
    expect(assess(v).severity).toBe('unknown')
  })

  it('does not read an unasked drift axis as a finding', () => {
    // Absent is manifest mode never having looked, which is not "no drift".
    const v = view((v) => {
      v.status.sync.state = 'synced'
      v.status.health.state = 'healthy'
    })
    expect(assess(v).severity).toBe('ok')
  })
})

describe('toneOf', () => {
  it('gives unknown its own tone rather than the all-clear one', () => {
    expect(toneOf('crit')).toBe('bad')
    expect(toneOf('warn')).toBe('warn')
    expect(toneOf('unknown')).toBe('info')
    expect(toneOf('ok')).toBe('ok')
    expect(toneOf('unknown')).not.toBe(toneOf('ok'))
  })
})

describe('healthToneOf', () => {
  it('maps each health state, and an unrecognised one to neutral', () => {
    expect(healthToneOf('healthy')).toBe('ok')
    expect(healthToneOf('progressing')).toBe('warn')
    expect(healthToneOf('degraded')).toBe('bad')
    expect(healthToneOf('missing')).toBe('bad')
    expect(healthToneOf('unknown')).toBe('info')
    expect(healthToneOf('something-newer' as never)).toBe('info')
  })
})
