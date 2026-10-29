// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { expect, it } from 'vitest'

import icon from './favicon.svg?raw'

// A browser parses the tab icon as XML, where one error discards the whole file
// for its default globe: `--` inside a comment, which HTML allows and XML does
// not, did exactly that (#328).
it('the tab icon is well-formed XML', () => {
  const doc = new DOMParser().parseFromString(icon, 'image/svg+xml')
  expect(doc.querySelector('parsererror')?.textContent).toBeUndefined()
  expect(doc.documentElement.nodeName).toBe('svg')
})
