// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

/** What each separately-grantable action carries, in the sentence below. */
const reads: Record<string, string> = {
  diff: 'the manifest change a sync would make',
  history: "its releases' recorded revisions",
  sync: 'the right to start one',
}

/**
 * A 403 is a permission, not a failure.
 *
 * authz.ActionDiff, ActionHistory and ActionSync are each granted separately
 * from ActionRead, and authz.go says why: a diff is the rendered manifest, a
 * history names what installed each revision, and a sync writes to the swarm —
 * so a token that may list applications may legitimately be refused all three.
 * Drawing any of them as an error would send an operator looking for a broken
 * controller; on the sync it would additionally read as a reconcile that failed,
 * when none was ever started.
 *
 * Nothing here has ever run against the OSS authorizer, which grants its admin
 * token everything. A licensed one is the first thing that will exercise it,
 * which is exactly why it is written now rather than when somebody reports a
 * red banner.
 */
export function Forbidden({ action }: { action: 'diff' | 'history' | 'sync' }) {
  return (
    <div className="notice notice-forbidden" role="status" data-testid="forbidden">
      <h2>You do not have permission</h2>
      <p>
        This token does not hold the <code>{action}</code> action. It is granted separately from{' '}
        <code>read</code>, so being able to see this application does not carry {reads[action]}.
      </p>
    </div>
  )
}
