// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { commitURL, shortRevision } from '../format'

/**
 * Commit names a commit by its short SHA, linked to the forge's page for it
 * when the repository has one (#367).
 *
 * The full SHA stays in the title, which is what somebody pastes into
 * `git show`. A new tab, and rel="noreferrer" for the reason Login.tsx gives:
 * the forge never learns this controller's URL, nor reaches back into its tab.
 */
export function Commit({ repoURL, revision }: { repoURL: string; revision: string }) {
  const code = <code title={revision}>{shortRevision(revision)}</code>
  const href = commitURL(repoURL, revision)
  if (href === undefined) return code
  return (
    <a href={href} target="_blank" rel="noreferrer">
      {code}
    </a>
  )
}
