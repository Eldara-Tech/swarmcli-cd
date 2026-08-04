// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { App } from './App'
import './index.css'

const root = document.getElementById('root')
if (!root) {
  // index.html is embedded in the binary beside this bundle, so the two cannot
  // drift apart at runtime — but a build that lost the mount point would
  // otherwise be a blank page with nothing in the console.
  throw new Error('#root is missing from index.html')
}

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
