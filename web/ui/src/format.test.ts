// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { describe, expect, it } from 'vitest'

import { zeroTimestamp } from './api/types'
import {
  ago,
  chartRef,
  commitURL,
  destination,
  formatInstant,
  isUnset,
  plural,
  serviceCounts,
  shortRevision,
  tasksUp,
} from './format'

describe('shortRevision', () => {
  it('abbreviates the way git does and leaves a short one alone', () => {
    expect(shortRevision('9f3c1abd4e5f6789')).toBe('9f3c1ab')
    expect(shortRevision('9f3c1ab')).toBe('9f3c1ab')
    expect(shortRevision('')).toBe('')
  })
})

describe('commitURL', () => {
  const sha = '2545466f1c3a9b0e7d4c2a1f0e9d8c7b6a5f4e3d'

  it.each([
    ['https://github.com/Eldara-Tech/swarmcli-cd.git', `https://github.com/Eldara-Tech/swarmcli-cd/commit/${sha}`],
    // The issue's own repository: a self-hosted GitLab, nested group, which
    // redirects /commit/ to /-/commit/.
    ['https://gitlab.esix.hu/best-validator/polkadot.git', `https://gitlab.esix.hu/best-validator/polkadot/commit/${sha}`],
    ['https://git.example.com:8443/team/app/', `https://git.example.com:8443/team/app/commit/${sha}`],
    ['https://bitbucket.org/team/app.git', `https://bitbucket.org/team/app/commits/${sha}`],
    // Bitbucket Data Center, with GÉANT's public instance's own clone URLs.
    [
      'https://bitbucket.software.geant.org/scm/cat/cat.git',
      `https://bitbucket.software.geant.org/projects/cat/repos/cat/commits/${sha}`,
    ],
    [
      'https://bitbucket.software.geant.org/scm/~jordi.ortiz_um.es/rare-nix.git',
      `https://bitbucket.software.geant.org/users/jordi.ortiz_um.es/repos/rare-nix/commits/${sha}`,
    ],
    [
      'https://atlassian.example.com/bitbucket/scm/ops/edge.git',
      `https://atlassian.example.com/bitbucket/projects/ops/repos/edge/commits/${sha}`,
    ],
    // Not Data Center's shape: three segments after scm.
    [
      'https://gitlab.example.com/scm/platform/tools/app.git',
      `https://gitlab.example.com/scm/platform/tools/app/commit/${sha}`,
    ],
  ])('links %s to its commit page', (repoURL, want) => {
    expect(commitURL(repoURL, sha)).toBe(want)
  })

  it('leaves credentials written into the repository URL out of the link', () => {
    expect(commitURL('https://deploy:s3cret@github.com/o/r.git?x=1#y', sha)).toBe(`https://github.com/o/r/commit/${sha}`)
  })

  it.each([['/srv/git/app'], ['http://github.com/o/r.git'], ['https://']])('has no link for %s', (repoURL) => {
    expect(commitURL(repoURL, sha)).toBeUndefined()
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

describe('tasksUp', () => {
  it('counts a one-shot that finished as up, so a done migration is not 0/1', () => {
    const migrate = { name: 'migrate', mode: 'replicated', running: 0, desired: 1, completed: 1, health: 'healthy' as const }
    expect(tasksUp(migrate)).toBe(1)
  })

  it('is the running count on a service with no completed tasks to add', () => {
    expect(tasksUp({ name: 'web', mode: 'replicated', running: 2, desired: 3, health: 'progressing' as const })).toBe(2)
  })

  it('counts only completions on a native job, so one still running is not 2/2', () => {
    expect(tasksUp({ name: 'seed', mode: 'replicated job', running: 2, desired: 2, health: 'progressing' as const })).toBe(0)
    expect(tasksUp({ name: 'seed', mode: 'replicated job', running: 1, desired: 2, completed: 1, health: 'progressing' as const })).toBe(1)
  })

  it('counts a global job by its completions too', () => {
    expect(tasksUp({ name: 'warm', mode: 'global job', running: 1, desired: 3, completed: 2, health: 'progressing' as const })).toBe(2)
  })
})

describe('ago', () => {
  const now = Date.parse('2026-11-16T12:00:00Z')
  const before = (ms: number) => new Date(now - ms).toISOString()

  it.each([
    [-3_600_000, 'just now'], // the controller's clock ahead of this one
    [30_000, 'just now'],
    [60_000, '1 minute ago'],
    [59 * 60_000, '59 minutes ago'],
    [3_600_000, '1 hour ago'],
    [47 * 3_600_000, '47 hours ago'],
    [48 * 3_600_000, '2 days ago'],
    [(5 * 24 + 3) * 3_600_000, '5 days ago'],
  ])('%i ms before now is %s', (ms, want) => {
    expect(ago(before(ms), now)).toBe(want)
  })

  it('says nothing about a time that was never set or will not parse', () => {
    expect(ago(undefined, now)).toBe('')
    expect(ago(zeroTimestamp, now)).toBe('')
    expect(ago('yesterday-ish', now)).toBe('')
  })
})
