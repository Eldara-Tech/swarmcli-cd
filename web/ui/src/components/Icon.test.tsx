// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The icon set, and the type that has to stay a union.
//
// `const paths: Record<string, ReactNode>` widened the key type to `string`, so
// `keyof typeof paths` was `string | number` and IconName checked nothing:
// `<Icon name="typo" />` compiled and rendered an empty <svg> with no error
// anywhere — not at build, not at runtime, not in a test. The guard is
// therefore a *type* assertion, run by `npm run typecheck`: if IconName ever
// widens again, the @ts-expect-error below stops being an error and tsc fails
// on the unused directive.

import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { Icon, type IconName } from './Icon'

describe('IconName', () => {
  it('is the union of the drawn glyphs and not a bare string', () => {
    // @ts-expect-error a name nothing draws must not typecheck
    const missing: IconName = 'definitely-not-an-icon'
    // @ts-expect-error and neither must a number, which `keyof Record<string,…>` admits
    const numeric: IconName = 42
    // Referenced so the bindings are not dead; the assertion is that the two
    // lines above are errors at all.
    expect([missing, numeric]).toHaveLength(2)
  })

  it('draws something for every name in the union', () => {
    // A name that resolves to nothing renders an empty <svg>, which is what the
    // widened type made unreachable to a reviewer and invisible to a user.
    const names: IconName[] = [
      'terminal',
      'search',
      'sync',
      'check',
      'warn',
      'error',
      'app',
      'service',
      'bell',
      'chevronDown',
      'externalLink',
      'filter',
      'trash',
      'more',
      'layers',
      'activity',
      'gauge',
    ]
    for (const name of names) {
      const { container, unmount } = render(<Icon name={name} />)
      const svg = container.querySelector('svg')
      expect(svg, `${name} rendered no svg`).not.toBeNull()
      expect(svg?.children.length, `${name} drew an empty svg`).toBeGreaterThan(0)
      unmount()
    }
  })

  it('is decorative, so a screen reader does not read every label twice', () => {
    const { container } = render(<Icon name="app" />)
    expect(container.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true')
  })
})
