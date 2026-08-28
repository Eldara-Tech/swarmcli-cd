// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'
import { useParams } from 'react-router'

import { apiGet, hasStatus } from '../api/client'
import { decodeEnum, syncActions } from '../api/enums'
import { diffKey } from '../api/queries'
import type { DiffResponse, ReleaseDiff } from '../api/types'
import { DiffView } from '../components/DiffView'
import { Forbidden } from '../components/Forbidden'
import { Icon } from '../components/Icon'
import { ErrorState, Loading } from '../components/StateBlock'

/**
 * What a sync would change, release by release.
 *
 * # The two empty answers mean opposite things, and the null one is the good one
 *
 * drift.Diffs starts from a nil slice and appends only the releases a sync would
 * touch, so a converged application answers `{"planned":true,"releases":null}` —
 * absence as success. The other empty answer is written literally by api.go's
 * ErrNotPlanned arm as `{"planned":false,"releases":[]}`, and means the
 * controller has not planned this application at all, so there is nothing to
 * compare rather than nothing to change.
 *
 * `planned` is therefore read first and the list second. A screen that switched
 * on the list would report the second case as the first, which is the one
 * mistake this endpoint was shaped to make impossible: it distinguishes them
 * deliberately.
 *
 * # It is a subset
 *
 * An unchanged release is omitted entirely — its diff is empty by construction —
 * so this list is shorter than the Overview's and must never be headed as if it
 * were the whole set.
 */
export function ApplicationDiff() {
  const { app = '' } = useParams()
  const diff = useQuery({
    // Its own key rather than ['applications', app, 'diff']. Invalidation in
    // react-query matches on key prefixes, so nesting under the detail's key
    // would make every refresh of the document refresh this too — and the
    // invalidation map has to keep them apart in the other direction as well:
    // live-drift-detected must not invalidate the diff, because the swarm moved
    // and the rendered manifest did not. Taken from api/queries.ts rather than
    // written here, so the screen and the map cannot disagree about it.
    queryKey: diffKey(app),
    queryFn: () => apiGet<DiffResponse>(`/api/v1/applications/${encodeURIComponent(app)}/diff`),
  })

  if (diff.isPending) return <Loading />
  if (diff.isError) {
    if (hasStatus(diff.error, 403)) return <Forbidden action="diff" />
    return <ErrorState message={diff.error.message} />
  }

  const { planned, releases } = diff.data
  if (!planned) {
    return (
      <p className="empty">
        Not reconciled yet — the controller has no plan for this application, so there is nothing to compare
        against. That is not the same as nothing having to change.
      </p>
    )
  }
  // Null and empty are folded together only here, past the planned check: the
  // OSS controller only ever sends null, and an alternative reconciler serving
  // this endpoint with an allocated-but-empty slice means the same thing by it.
  const changed = releases ?? []
  if (changed.length === 0) {
    return (
      <div className="confirm-card">
        <div className="confirm-icon">
          <Icon name="check" size={24} />
        </div>
        <h2 className="confirm-title">Manifests in Sync</h2>
        <p className="confirm-desc empty" data-testid="diff-converged">
          Nothing would change — the controller has a plan, and no release in it would be touched by a sync.
        </p>
      </div>
    )
  }

  return (
    <section className="diff-screen">
      <p className="muted">
        The releases a sync would change. A release it would leave alone is not listed at all, so this is
        fewer releases than the overview shows, not all of them.
      </p>
      {changed.map((release) => (
        <ReleaseDiffPanel key={release.release} release={release} />
      ))}
    </section>
  )
}

function ReleaseDiffPanel({ release }: { release: ReleaseDiff }) {
  return (
    <section className="diff-release" data-testid="release-diff">
      <h2>
        {release.release} <span className="chip chip-muted">{decodeEnum(release.action, syncActions)}</span>
      </h2>
      <DiffView diff={release.diff} />
    </section>
  )
}
