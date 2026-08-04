// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { defineConfig, devices } from '@playwright/test'

// The browser run, which is a different job from the component tests and stays
// that way: it needs a real controller against a real swarm, and its flakiness
// must never gate the Go suite (D19).
//
// No webServer here. The controller is a Go binary with the bundle embedded in
// it, started by .github/workflows/smoke.yml against a single-node swarm —
// which is the whole point of running this at all, since everything below a
// real browser passes whether or not the CSP, the bearer header and the
// streamed response work.
export default defineConfig({
  testDir: './e2e',
  // A smoke run that needs retries to pass is a smoke run that is not telling
  // anybody anything.
  retries: 0,
  reporter: 'list',
  use: {
    baseURL: 'http://127.0.0.1:8080',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
