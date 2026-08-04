// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { describe, expect, it } from 'vitest'

import { clone } from '../test/fakeApi'
import controllerStatusFull from '../test/fixtures/controller-status-full.json'
import controllerStatusZero from '../test/fixtures/controller-status-zero.json'
import { appSetShape } from './appset'
import type { ControllerStatus } from './types'

/** Both fixtures are shapes the Go types produced; only values are varied. */
function status(from: unknown): ControllerStatus {
  return clone(from) as ControllerStatus
}

describe('appSetShape', () => {
  it('reads the zero fixture as unwired, not as never-loaded', () => {
    // The trap this function exists for. api.go answers with a zero
    // ControllerStatus when it has no controller, so `loadedAt` is the zero
    // time and `applications` is 0 — every signal of the loudest failure there
    // is, on a controller where nothing is wrong.
    expect(appSetShape(status(controllerStatusZero))).toBe('unwired')
  })

  it('reads a wired controller that has never loaded as never-loaded', () => {
    const s = status(controllerStatusZero)
    s.appSet.mode = 'git'
    s.appSet.error = 'dial tcp: connection refused'
    expect(appSetShape(s)).toBe('never-loaded')
  })

  it('reads the full fixture as stale, because stale outranks its own error', () => {
    expect(appSetShape(status(controllerStatusFull))).toBe('stale')
  })

  it('reads an error with stale false as partial', () => {
    const s = status(controllerStatusFull)
    s.appSet.stale = false
    expect(appSetShape(s)).toBe('partial')
  })

  it('reads a loaded set with no error as ok', () => {
    const s = status(controllerStatusFull)
    s.appSet.stale = false
    delete s.appSet.error
    expect(appSetShape(s)).toBe('ok')
  })

  it('does not read a set that legitimately declares nothing as never-loaded', () => {
    // Zero applications is half the signal; the other half is that the set has
    // never loaded at all. An empty applications.yaml is a working controller.
    const s = status(controllerStatusFull)
    s.appSet.stale = false
    delete s.appSet.error
    s.applications = 0
    expect(appSetShape(s)).toBe('ok')
  })
})
