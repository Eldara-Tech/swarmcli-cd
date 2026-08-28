// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useMutation, useQuery } from '@tanstack/react-query'
import { Link, NavLink, Outlet, useParams } from 'react-router'

import { apiGet, apiPost, hasStatus } from '../api/client'
import { compatStates, decodeEnum, syncActions } from '../api/enums'
import { applicationKey } from '../api/queries'
import type { ReleaseStatus, View } from '../api/types'
import { DriftPanel } from '../components/DriftPanel'
import { Forbidden } from '../components/Forbidden'
import { Icon } from '../components/Icon'
import { Instant } from '../components/Instant'
import { CompatChip, DriftCell, HealthChip, SyncChip } from '../components/StateChip'
import { TopologyTree } from '../components/TopologyTree'
import { chartRef, destination, serviceCounts, shortRevision } from '../format'
import { ErrorState, Loading } from '../components/StateBlock'

/**
 * The application's identity, and the three tabs that read it.
 *
 * A layout route rather than three screens each drawing their own header: the
 * name and the three chips answer "which application am I looking at, and is it
 * healthy", and a tab that redrew them could disagree with the one beside it.
 * It also means the diff and history documents are fetched only while their tab
 * is open, which is the whole reason they have endpoints of their own — a
 * manifest must not be dragged along by a screen that is not showing it.
 *
 * The tabs are routes and not state, for the reason the applications list gives
 * about its filters: a tab is then a link somebody can paste.
 */
export function ApplicationDetail() {
  const { app = '' } = useParams()
  const detail = useApplication(app)
  // Above the branches below, because a hook cannot be called conditionally and
  // the button belongs to the header this component draws either way.
  const sync = useSync(app)

  if (detail.isPending) return <Loading />
  if (detail.isError) {
    return <ErrorState message={detail.error.message} />
  }

  const { spec, status } = detail.data

  return (
    <section className="screen">
      <p className="crumb">
        <Link to="/">← Applications</Link>
      </p>
      <header className="screen-header">
        <h1>{spec.name}</h1>
        <p className="header-chips">
          <SyncChip state={status.sync.state} />
          <HealthChip state={status.health.state} />
          <DriftCell drift={status.drift} />
        </p>
        {/* Disabled while the request is in flight, and that is the whole of
            what stops a double press becoming two syncs: React does not deliver
            a click to a disabled button, so the second press of a double-click
            lands on nothing. It does not need to hold for longer than that —
            the handler answers 202 in milliseconds, and a press after it has
            answered is coalesced by the controller onto the sync already
            queued rather than starting a second one. */}
        <button
          type="button"
          className="sync-button"
          disabled={sync.isPending}
          onClick={() => {
            sync.mutate()
          }}
        >
          Sync
        </button>
      </header>
      <SyncOutcome sync={sync} />

      {/* `.` with `end` is the index route, so "Overview" is current on
          /applications/edge and not on its children. */}
      <nav className="tabs">
        <NavLink to="." end className={tab}>
          Overview
        </NavLink>
        <NavLink to="topology" className={tab}>
          Topology
        </NavLink>
        <NavLink to="diff" className={tab}>
          Diff
        </NavLink>
        <NavLink to="history" className={tab}>
          History
        </NavLink>
      </nav>

      <Outlet />
    </section>
  )
}

function tab({ isActive }: { isActive: boolean }): string {
  return isActive ? 'tab tab-current' : 'tab'
}

/**
 * The detail document, read by the layout above and the overview below.
 *
 * One query key between them, so the header and the body it sits over cost one
 * request and can never be two different observations of the same application.
 */
function useApplication(app: string) {
  return useQuery({
    queryKey: applicationKey(app),
    queryFn: () => apiGet<View>(`/api/v1/applications/${encodeURIComponent(app)}`),
  })
}

/**
 * The 202 body api.go's sync handler writes.
 *
 * Not in api/types.ts: that file mirrors the Go structs a fixture is generated
 * from, and this one is a map literal built in the handler, so a generated
 * fixture could not police it.
 */
interface SyncAccepted {
  application: string
  accepted: boolean
  /**
   * Present, and true, when a sync was already queued and this request joined
   * it rather than adding another. Not an error and not a no-op — see
   * SyncOutcome.
   */
  coalesced?: boolean
}

/**
 * The one request this UI makes that writes.
 *
 * A mutation rather than a query because it is not idempotent and must never be
 * retried: react-query retries a *query* on the operator's behalf, and a retried
 * POST here would be a second sync of a swarm. Mutations default to no retries,
 * which is the behaviour this wants and is therefore not restated.
 *
 * It invalidates nothing on success, deliberately. The response says a sync was
 * accepted and nothing whatever about its outcome — it runs detached from the
 * request, for as long as the rollout takes — so refetching the detail here
 * would refetch a document that has not moved yet. What moves it is the
 * sync-started the controller raises, through the same invalidation map every
 * other event goes through; there is no second path into the cache for the one
 * document an operator is watching hardest.
 */
function useSync(app: string) {
  return useMutation({
    mutationFn: () => apiPost<SyncAccepted>(`/api/v1/applications/${encodeURIComponent(app)}/sync`),
  })
}

/**
 * What pressing the button did, which is never what the sync did.
 *
 * Nothing here reports success or failure of a reconcile: the request is over
 * long before the sync is. The outcome arrives on the header's chips, refetched
 * when the controller says so.
 */
function SyncOutcome({ sync }: { sync: ReturnType<typeof useSync> }) {
  if (sync.isError) {
    // A 403 here is a permission and not a failed sync. authz grants ActionSync
    // separately from ActionRead — a token that may watch this application may
    // legitimately be refused the button — and drawing it in red as a failure
    // would send an operator to the controller's logs looking for a reconcile
    // that was never started.
    if (hasStatus(sync.error, 403)) return <Forbidden action="sync" />
    return (
      <p className="error" role="alert">
        {sync.error.message}
      </p>
    )
  }
  if (sync.data === undefined) return null

  return (
    <p className="sync-outcome" role="status" data-testid="sync-outcome">
      {sync.data.coalesced === true
        ? // Not an error, and not a no-op either: api.go coalesces onto a sync
          // that is queued and has not read the repository yet, so the state
          // this operator asked to have reconciled is the state that one will
          // reconcile. Saying "already running" instead would be the one thing
          // that is not true.
          'A sync was already queued for this application, so this one joined it rather than adding another. It has not read the repository yet, so it will reconcile what you asked for.'
        : 'Sync accepted. It runs detached from this request, so this is not its outcome — the chips above are, once the controller reports.'}
    </p>
  )
}

/**
 * What was declared, what was observed, and what no longer matches.
 *
 * The tree stops at services because that is where the API stops. ServiceStatus
 * carries mode, running, desired, completed, health, update state and message,
 * and there is no task or container level to descend into — so a screen that
 * offered one would be promising something no endpoint can answer.
 */
export function ApplicationOverview() {
  const { app = '' } = useParams()
  const detail = useApplication(app)

  // No pending or error branch. The layout has already resolved this exact
  // query — same key, so the cache answers — and would have rendered neither
  // the tabs nor this outlet if it had not.
  if (detail.data === undefined) return null

  const { spec, status } = detail.data
  const releases = status.releases
  const blocked = (releases ?? []).filter(blocking)

  return (
    <>
      {/* Above everything, because it is not a property of a row: the reconciler
          refuses the whole plan rather than applying the compatible part of it,
          so nothing below is going to be deployed until the engine moves. */}
      {blocked.length > 0 && (
        <div className="notice notice-blocking" role="alert">
          <h2>This plan will not be applied</h2>
          <p>
            The controller refuses a plan containing a release its chart engine is too old for, and refuses
            all of it rather than half of it.
          </p>
          <ul>
            {blocked.map((release) => (
              <li key={release.name}>
                <strong>{release.name}</strong> requires <code>{release.compat?.required ?? 'unknown'}</code>;
                this engine is <code>{release.compat?.engine ?? 'unknown'}</code>.
                {release.compat?.reason !== undefined && release.compat.reason !== '' && (
                  <> {release.compat.reason}</>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* Second, and phrased as staleness rather than as an error: nothing on
          the status moves when a reconcile fails before it observes, so every
          field below is the last one that worked. */}
      {status.error !== undefined && status.error !== '' && (
        <div className="notice notice-reconcile" role="alert">
          <h2>The last reconcile failed</h2>
          <p>Everything below is the last successful observation, not the current state.</p>
          <p className="wrap">{status.error}</p>
        </div>
      )}

      <div className="bento-grid">
        <div className="bento-col-6">
          <div className="card-frame spec-card-frame">
            <div className="card-frame-head">
              <h2>
                <Icon name="app" size={16} />
                GitOps Specification
              </h2>
            </div>
            <div className="card-frame-body">
              <dl className="spec-grid">
                <div className="spec-item">
                  <dt className="spec-label">Source</dt>
                  <dd className="spec-value wrap">
                    {spec.source.repoURL} @ {spec.source.revision}
                  </dd>
                </div>
                {spec.source.chart === undefined ? (
                  <div className="spec-item">
                    <dt className="spec-label">Release file</dt>
                    <dd className="spec-value font-mono">{spec.source.releaseFile}</dd>
                  </div>
                ) : (
                  <div className="spec-item">
                    <dt className="spec-label">Chart</dt>
                    <dd className="spec-value">
                      {chartRef(spec.source.chart)} as {spec.source.chart.release}
                    </dd>
                  </div>
                )}
                <div className="spec-item">
                  <dt className="spec-label">Destination</dt>
                  <dd className="spec-value">{destination(spec.destination)}</dd>
                </div>
                <div className="spec-item">
                  <dt className="spec-label">Revision</dt>
                  <dd className="spec-value">
                    {status.sync.revision === undefined || status.sync.revision === '' ? (
                      <span className="muted">–</span>
                    ) : (
                      <code title={status.sync.revision}>{shortRevision(status.sync.revision)}</code>
                    )}
                  </dd>
                </div>
              </dl>
            </div>
          </div>
        </div>

        <div className="bento-col-6">
          <div className="card-frame spec-card-frame">
            <div className="card-frame-head">
              <h2>
                <Icon name="service" size={16} />
                Swarm Runtime &amp; Reconcile
              </h2>
            </div>
            <div className="card-frame-body">
              <dl className="spec-grid">
                <div className="spec-item">
                  <dt className="spec-label">Services</dt>
                  <dd className="spec-value">{serviceCounts(status.health.services)}</dd>
                </div>
                <div className="spec-item">
                  <dt className="spec-label">Drift State</dt>
                  <dd className="spec-value">
                    {status.drift === undefined || status.drift.state === 'none' ? (
                      <span className="text-ok font-medium">Invariance Maintained</span>
                    ) : (
                      <span className="chip chip-bad">{status.drift.state}</span>
                    )}
                  </dd>
                </div>
                <div className="spec-item">
                  <dt className="spec-label">Last sync</dt>
                  <dd className="spec-value">
                    {status.sync.lastSync === undefined ? (
                      <span className="muted">never</span>
                    ) : (
                      <>
                        {status.sync.lastSync.succeeded ? 'succeeded' : 'failed'} at{' '}
                        <Instant at={status.sync.lastSync.finishedAt} />{' '}
                        <code title={status.sync.lastSync.revision}>{shortRevision(status.sync.lastSync.revision)}</code>
                        {status.sync.lastSync.error !== undefined && status.sync.lastSync.error !== '' && (
                          <span className="error wrap"> {status.sync.lastSync.error}</span>
                        )}
                      </>
                    )}
                  </dd>
                </div>
                <div className="spec-item">
                  <dt className="spec-label">Observed</dt>
                  <dd className="spec-value">
                    <Instant at={status.observedAt} />
                  </dd>
                </div>
                {status.health.message !== undefined && status.health.message !== '' && (
                  <div className="spec-item full-spec-item">
                    <dt className="spec-label">Health Message</dt>
                    <dd className="spec-value wrap">{status.health.message}</dd>
                  </div>
                )}
              </dl>
            </div>
          </div>
        </div>

        <div className="bento-col-12">
          <div className="card-frame">
            <div className="card-frame-head">
              <h2>
                <Icon name="layers" size={16} />
                Releases
              </h2>
            </div>
            <div className="card-frame-body">
              {releases === undefined ? (
                // Absent is "not requested", never "none". The engine rejects a release
                // file that declares no releases, so a reconciled application always has
                // at least one — and the list endpoint strips the key on purpose. On
                // this endpoint the only thing absence can mean is that the controller
                // has not reconciled the application yet.
                <p className="empty">Not reconciled yet — the controller has not reported this application&apos;s releases.</p>
              ) : (
                <ul className="tree">
                  {releases.map((release) => (
                    <ReleaseNode key={release.name} release={release} />
                  ))}
                </ul>
              )}
            </div>
          </div>
        </div>
      </div>

      <DriftPanel releases={releases ?? []} />
    </>
  )
}

/**
 * The same application as a map rather than a table: application → releases →
 * services, on connector rails.
 *
 * It reads the layout's already-resolved query — same key, so the cache answers
 * and no request is made — and shares the overview's honesty about depth: the
 * tree stops at services because ServiceStatus is where the API stops.
 */
export function ApplicationTopology() {
  const { app = '' } = useParams()
  const detail = useApplication(app)

  if (detail.data === undefined) return null

  const releases = detail.data.status.releases
  if (releases === undefined) {
    return (
      <p className="empty">Not reconciled yet — the controller has not reported this application&apos;s resources.</p>
    )
  }
  return <TopologyTree view={detail.data} />
}

function ReleaseNode({ release }: { release: ReleaseStatus }) {
  const services = release.services ?? []
  return (
    <li className="tree-release">
      <div className="release-card-header">
        <div className="release-title-group">
          <Icon name="layers" size={16} />
          <h3 className="release-name">{release.name}</h3>
          <span className="release-chart-badge font-mono">
            {release.chart} {release.version}
          </span>
          <span className="release-rev-chip font-mono">rev {release.revision}</span>
        </div>
        <div className="release-chips-group">
          <span className="chip chip-muted">{decodeEnum(release.action, syncActions)}</span>
          <SyncChip state={release.sync} />
          <HealthChip state={release.health.state} />
          {release.compat !== undefined && <CompatChip status={release.compat.status} />}
        </div>
      </div>
      {services.length === 0 ? (
        // A release with no services, which is a release with no services — a
        // chart may install nothing but a network or a config. Rendering the
        // release and saying so is the difference between that and an empty
        // tree, which would read as "this release is missing".
        <p className="empty">No services.</p>
      ) : (
        <table className="service-table">
          <thead>
            <tr>
              <th scope="col">Service</th>
              <th scope="col">Mode</th>
              <th scope="col">Replicas</th>
              <th scope="col">Health</th>
              <th scope="col">Update</th>
              <th scope="col">Message</th>
            </tr>
          </thead>
          <tbody>
            {services.map((service) => (
              <tr key={service.name}>
                <th scope="row" className="service-name-cell font-mono">
                  {service.name}
                </th>
                <td>
                  <span className="chip chip-muted font-mono">{service.mode}</span>
                </td>
                <td>
                  <span className="replicas-badge font-mono">
                    {service.running}/{service.desired}
                  </span>
                </td>
                <td>
                  <HealthChip state={service.health} />
                </td>
                {/* Empty means never updated, not finished, so a dash rather
                    than a blank cell that reads as missing data. */}
                <td>
                  {service.updateState === undefined || service.updateState === '' ? (
                    <span className="muted">–</span>
                  ) : (
                    <span className="font-mono">{service.updateState}</span>
                  )}
                </td>
                <td className="wrap muted">{service.message || '–'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </li>
  )
}

/**
 * blocking reports whether one release's compatibility verdict stops the plan.
 *
 * Not every incompatible release does. reconcile.checkCompat exempts a release
 * whose action is `unchanged` — it is already deployed and applying will not
 * touch it — so treating the verdict alone as blocking would tell an operator
 * their plan is refused when it is not. The chip on the release row still
 * reports the finding either way.
 */
function blocking(release: ReleaseStatus): boolean {
  if (release.compat === undefined) return false
  if (decodeEnum(release.compat.status, compatStates) !== 'incompatible') return false
  return decodeEnum(release.action, syncActions) !== 'unchanged'
}
