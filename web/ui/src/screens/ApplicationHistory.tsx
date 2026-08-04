// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'
import { useParams } from 'react-router'

import { apiGet, hasStatus } from '../api/client'
import type { History, ReleaseHistory } from '../api/types'
import { Forbidden } from '../components/Forbidden'
import { Instant } from '../components/Instant'

/**
 * Every declared release, with the revisions the chart engine recorded for it.
 *
 * # `revisions: null` is a real answer
 *
 * reconcile.History returns `ReleaseHistory{Name: …}` with a nil slice — and no
 * omitempty on the field — for a release the repository declares and the plan
 * would install. That is the honest answer and a different one from "no such
 * release", so it is rendered as the release with nothing under it. It is also
 * the single most likely runtime crash on this screen: `.map()` on null throws,
 * and it is the first payload a manual application serves.
 *
 * # Newest first, and left alone
 *
 * reconcile.revisions reverses the engine's ascending order before serving it,
 * with the stated reason that every client should then not have to. Sorting here
 * would be a second opinion on an order the API has already formed.
 */
export function ApplicationHistory() {
  const { app = '' } = useParams()
  const history = useQuery({
    // Its own key, for the reason ApplicationDiff's gives — and for one more of
    // its own: notify.go writes no chart revision when a drift converges, so
    // drift-converged must not invalidate this. A key nested under the
    // application's would make that impossible to express.
    queryKey: ['history', app],
    queryFn: () => apiGet<History>(`/api/v1/applications/${encodeURIComponent(app)}/history`),
  })

  if (history.isPending) return <p>Loading…</p>
  if (history.isError) {
    if (hasStatus(history.error, 403)) return <Forbidden action="history" />
    // The only endpoint in the API that answers 502, and it means something
    // specific: the controller is fine and reachable, and could not read the
    // release records off the swarm. A generic error panel would send an
    // operator to the controller's logs when the thing to look at is the
    // daemon — and it is transient often enough to be worth a button rather
    // than a page reload, which would cost the token and the event stream.
    if (hasStatus(history.error, 502)) {
      return (
        <div className="notice notice-unreachable" role="alert" data-testid="history-unreachable">
          <h2>The swarm would not answer</h2>
          <p>
            The controller could not read this application&apos;s release history from the swarm. Nothing is
            wrong with the application or with what is deployed; the records could not be read just now.
          </p>
          <p className="wrap notice-reason">{history.error.message}</p>
          <button type="button" className="retry" onClick={() => void history.refetch()}>
            Try again
          </button>
        </div>
      )
    }
    return (
      <p className="error" role="alert">
        {history.error.message}
      </p>
    )
  }

  // Null as well as empty: api.go's ErrNotPlanned arm writes an empty list, and
  // History{} marshals the field as null because it carries no omitempty. Both
  // say the controller has no plan to enumerate releases from.
  const releases = history.data.releases ?? []
  if (releases.length === 0) {
    return (
      <p className="empty">
        Not reconciled yet — the controller has no plan for this application, so it has no releases to read a
        history for.
      </p>
    )
  }

  return (
    <section className="history-screen">
      {/* Stated once, at the top, because the alternative is a tooltip on a
          column nobody hovers. The API exposes no controller id anywhere, so
          this screen genuinely cannot tell an operator whether a stamp is its
          own controller's — and guessing at a prefix rule would be wrong on the
          first swarm that runs two controllers, which is the case the id exists
          for. */}
      <p className="muted">
        A revision&apos;s owner is the stamp naming what installed it, written{' '}
        <code>&lt;id&gt;:release/&lt;release&gt;</code> — so{' '}
        <code>cd/&lt;controller&gt;/&lt;application&gt;:release/&lt;release&gt;</code> for a controller and{' '}
        <code>apply/&lt;name&gt;:release/&lt;release&gt;</code> for a release deployed by hand. It is reported
        here exactly as it was recorded: the API names no controller, so this screen cannot say which stamps
        are this controller&apos;s.
      </p>
      {releases.map((release) => (
        <ReleaseHistoryPanel key={release.name} release={release} />
      ))}
    </section>
  )
}

function ReleaseHistoryPanel({ release }: { release: ReleaseHistory }) {
  // The null the module comment is about, and the reason this is a `??` and not
  // a `.map()` two lines below.
  const revisions = release.revisions ?? []
  return (
    <section className="history-release" data-testid="release-history">
      <h2>{release.name}</h2>
      {revisions.length === 0 ? (
        <p className="empty">
          Declared, never deployed — the plan would install this release, so the chart engine has no revisions
          recorded for it yet.
        </p>
      ) : (
        <table>
          <thead>
            <tr>
              <th scope="col">Revision</th>
              <th scope="col">Chart</th>
              <th scope="col">Version</th>
              <th scope="col">Status</th>
              <th scope="col">Created</th>
              <th scope="col">Owner</th>
            </tr>
          </thead>
          <tbody>
            {/* Newest first, exactly as served. */}
            {revisions.map((revision) => (
              <tr key={revision.revision}>
                <th scope="row">{revision.revision}</th>
                <td>{revision.chart}</td>
                <td>{revision.version}</td>
                <td>{revision.status}</td>
                {/* A dash rather than Instant's "never": the field is optional
                    because the engine may not have recorded one, and a revision
                    that exists was plainly created at some point. */}
                <td>
                  {revision.created === undefined || revision.created === '' ? (
                    <span className="muted">–</span>
                  ) : (
                    <Instant at={revision.created} />
                  )}
                </td>
                <td className="wrap">
                  {revision.owner === undefined || revision.owner === '' ? (
                    <span className="muted">unclaimed</span>
                  ) : (
                    <code>{revision.owner}</code>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}
