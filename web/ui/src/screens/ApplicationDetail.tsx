// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'
import { Link, NavLink, Outlet, useParams } from 'react-router'

import { apiGet } from '../api/client'
import { compatStates, decodeEnum, syncActions } from '../api/enums'
import type { ReleaseStatus, View } from '../api/types'
import { DriftPanel } from '../components/DriftPanel'
import { Instant } from '../components/Instant'
import { CompatChip, DriftCell, HealthChip, SyncChip } from '../components/StateChip'
import { chartRef, destination, serviceCounts, shortRevision } from '../format'

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

  if (detail.isPending) return <p>Loading…</p>
  if (detail.isError) {
    return (
      <p className="error" role="alert">
        {detail.error.message}
      </p>
    )
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
      </header>

      {/* `.` with `end` is the index route, so "Overview" is current on
          /applications/edge and not on its children. */}
      <nav className="tabs">
        <NavLink to="." end className={tab}>
          Overview
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
    queryKey: ['applications', app],
    queryFn: () => apiGet<View>(`/api/v1/applications/${encodeURIComponent(app)}`),
  })
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

      <dl className="detail-fields">
        <dt>Source</dt>
        <dd className="wrap">
          {spec.source.repoURL} @ {spec.source.revision}
        </dd>
        {spec.source.chart === undefined ? (
          <>
            <dt>Release file</dt>
            <dd>{spec.source.releaseFile}</dd>
          </>
        ) : (
          <>
            <dt>Chart</dt>
            <dd>
              {chartRef(spec.source.chart)} as {spec.source.chart.release}
            </dd>
          </>
        )}
        <dt>Destination</dt>
        <dd>{destination(spec.destination)}</dd>
        <dt>Revision</dt>
        <dd>
          {status.sync.revision === undefined || status.sync.revision === '' ? (
            <span className="muted">–</span>
          ) : (
            <code title={status.sync.revision}>{shortRevision(status.sync.revision)}</code>
          )}
        </dd>
        <dt>Services</dt>
        <dd>{serviceCounts(status.health.services)}</dd>
        {status.health.message !== undefined && status.health.message !== '' && (
          <>
            <dt>Health message</dt>
            <dd className="wrap">{status.health.message}</dd>
          </>
        )}
        <dt>Last sync</dt>
        <dd>
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
        <dt>Observed</dt>
        <dd>
          <Instant at={status.observedAt} />
        </dd>
      </dl>

      <h2>Releases</h2>
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

      <DriftPanel releases={releases ?? []} />
    </>
  )
}

function ReleaseNode({ release }: { release: ReleaseStatus }) {
  const services = release.services ?? []
  return (
    <li className="tree-release">
      <div className="tree-head">
        <h3>{release.name}</h3>
        <code>
          {release.chart} {release.version}
        </code>
        <span className="muted">revision {release.revision}</span>
        <span className="chip chip-muted">{decodeEnum(release.action, syncActions)}</span>
        <SyncChip state={release.sync} />
        <HealthChip state={release.health.state} />
        {release.compat !== undefined && <CompatChip status={release.compat.status} />}
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
                <th scope="row">{service.name}</th>
                <td>{service.mode}</td>
                <td>
                  {service.running}/{service.desired}
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
                    service.updateState
                  )}
                </td>
                <td className="wrap">{service.message}</td>
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
