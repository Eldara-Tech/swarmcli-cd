// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import js from '@eslint/js'
import react from 'eslint-plugin-react'
import reactHooks from 'eslint-plugin-react-hooks'
import tseslint from 'typescript-eslint'

// The rules that are not a matter of taste.
//
// The one this file exists for is the ban on the `style` prop. The policy
// web.go sends is `style-src 'self'` with no 'unsafe-inline', because this page
// holds the swarm's root credential — and React's style prop emits exactly the
// inline style attribute that policy refuses. It fails in a real browser and
// nowhere else: jsdom has no CSP, so every component test passes, and the only
// signal is an element that renders unstyled in production. A convention cannot
// catch that; a rule can.
export default tseslint.config(
  // web/dist is Vite's output, and it is above this project's root.
  { ignores: ['../dist/**'] },
  js.configs.recommended,
  {
    // Type-aware rules over the bundle's own sources, which are the files
    // tsconfig.json describes. Anything outside it has no program to consult.
    files: ['src/**/*.{ts,tsx}'],
    extends: [tseslint.configs.recommendedTypeChecked],
    languageOptions: {
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    settings: { react: { version: 'detect' } },
    plugins: { react, 'react-hooks': reactHooks },
    rules: {
      'react/forbid-dom-props': [
        'error',
        {
          forbid: [
            {
              propName: 'style',
              message:
                "style-src 'self' has no 'unsafe-inline', so an inline style attribute is refused by the browser and the element renders unstyled. Add a class in index.css.",
            },
          ],
        },
      ],
      // The same rule from the other side: a component that takes a style prop
      // and spreads it onto a DOM node is how the ban above gets routed around
      // without anybody deciding to.
      'react/forbid-component-props': [
        'error',
        {
          forbid: [
            {
              propName: 'style',
              message:
                "style-src 'self' has no 'unsafe-inline'; pass a className instead so the rule cannot be routed around.",
            },
          ],
        },
      ],
      'react-hooks/rules-of-hooks': 'error',
      // An error rather than a warning: a stale closure in the event stream's
      // effect is a tab that stops receiving events while still saying it is
      // live, which is the one failure this UI has no other way to notice.
      'react-hooks/exhaustive-deps': 'error',
    },
  },
  {
    // The build's own configuration and the Playwright specs. Both run in Node
    // rather than in the browser and neither is in tsconfig's program, so they
    // get the syntax-only rules.
    files: ['*.ts', '*.js', 'e2e/**/*.ts'],
    extends: [tseslint.configs.recommended],
  },
)
