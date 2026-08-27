// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'

import { apiGet } from '../api/client'
import { decodeEnum, driftStates, healthStates, syncStates } from '../api/enums'
import { controllerKey, listKey } from '../api/queries'
import type { ApplicationList, ControllerStatus, View } from '../api/types'
import { StatCard } from '../components/StatCard'
import { Loading } from '../components/StateBlock'
import { TerminalStream } from '../components/TerminalStream'
import { isUnset, formatInstant, serviceCounts } from '../format'
import { useLive } from '../live'

/**
 * The fleet at a glance: the rollup the list makes you read row by row, plus
 * the live event stream.
 *
 * It reads the same two documents the list and the controller screen already
 * hold — /api/v1/applications and /api/v1/status, under the shared query keys —
 * so opening it from either costs no request. Everything here is derived from
 * those; nothing is invented. Where the drift axis was never asked (manifest
 * mode) it is counted as "not checked" rather than as zero.
 */
export function FleetOverview() {
  const apps = useQuery({
    queryKey: listKey,
    queryFn: () => apiGet<ApplicationList>('/api/v1/applications'),
  })
  const controller = useQuery({
    queryKey: controllerKey,
    queryFn: () => apiGet<ControllerStatus>('/api/v1/status'),
  })
  const live = useLive()

  if (apps.isPending) return <Loading rows={4} />
  if (apps.isError) {
    return (
      <p className="error" role="alert">
        {apps.error.message}
      </p>
    )
  }

  const views = apps.data.applications
  const roll = rollup(views)
  const nominal = views.length > 0 && roll.synced === views.length && roll.healthy === views.length && roll.drifted === 0 && roll.failed === 0

  return (
    <section className="screen">
      <header className="screen-header">
        <h1>Overview</h1>
        <p className="header-chips">
          {views.length === 0 ? (
            <span className="chip chip-muted">no applications</span>
          ) : nominal ? (
            <span className="chip chip-good">all systems nominal</span>
          ) : (
            <span className="chip chip-warn">needs attention</span>
          )}
        </p>
      </header>

      <div className="stat-row">
        <StatCard label="Applications" value={views.length} icon="app" />
        <StatCard
          label="In sync"
          value={roll.synced}
          unit={`/ ${views.length}`}
          tone={roll.synced === views.length ? 'ok' : 'warn'}
          icon="sync"
        />
        <StatCard
          label="Healthy"
          value={roll.healthy}
          unit={`/ ${views.length}`}
          tone={roll.healthy === views.length ? 'ok' : 'warn'}
          icon="check"
        />
        <StatCard
          label="Drifted"
          value={roll.drifted}
          tone={roll.drifted > 0 ? 'warn' : 'ok'}
          sub={roll.driftUnchecked > 0 ? `${roll.driftUnchecked} not checked` : undefined}
          icon="layers"
        />
        <StatCard
          label="Reconcile errors"
          value={roll.failed}
          tone={roll.failed > 0 ? 'bad' : 'ok'}
          icon="warn"
        />
      </div>

      <div className="overview-grid">
        <div className="card-frame">
          <div className="card-frame-head">
            <h2>Application set</h2>
            {controller.data !== undefined && (
              <span className="chip chip-muted">{controller.data.appSet.mode || 'static'}</span>
            )}
          </div>
          <div className="card-frame-body">
            {controller.data === undefined ? (
              <p className="muted">Controller status unavailable.</p>
            ) : (
              <dl className="spec-grid">
                <div className="spec-item">
                  <dt className="spec-label">Source Mode & Path</dt>
                  <dd className="spec-value wrap">
                    {controller.data.appSet.mode === '' ? (
                      <span className="muted">none wired</span>
                    ) : (
                      <>
                        {controller.data.appSet.mode}
                        {controller.data.appSet.source !== undefined && controller.data.appSet.source !== '' && (
                          <> · {controller.data.appSet.source}</>
                        )}
                      </>
                    )}
                  </dd>
                </div>
                <div className="spec-item">
                  <dt className="spec-label">Loaded Revision</dt>
                  <dd className="spec-value">
                    {controller.data.appSet.revision === undefined || controller.data.appSet.revision === '' ? (
                      <span className="muted">–</span>
                    ) : (
                      <code>{controller.data.appSet.revision}</code>
                    )}
                  </dd>
                </div>
                <div className="spec-item">
                  <dt className="spec-label">Timestamp Loaded</dt>
                  <dd className="spec-value">
                    {isUnset(controller.data.appSet.loadedAt) ? (
                      <span className="muted">never</span>
                    ) : (
                      formatInstant(controller.data.appSet.loadedAt)
                    )}
                  </dd>
                </div>
                <div className="spec-item">
                  <dt className="spec-label">Running Services</dt>
                  <dd className="spec-value">{serviceCounts(roll.services)} healthy</dd>
                </div>
              </dl>
            )}
          </div>
        </div>

        <TerminalStream title="Controller stream" events={live.log} className="overview-terminal" />
      </div>
    </section>
  )
}

interface Rollup {
  synced: number
  healthy: number
  drifted: number
  driftUnchecked: number
  failed: number
  services: { healthy: number; total: number }
}

/**
 * rollup counts the fleet across the three status axes, decoding each enum so a
 * state a newer controller added is counted as itself rather than matching
 * nothing. `failed` is the reconcile-error count — the row the list marks stale
 * — which is orthogonal to sync and health and the one an overview must not bury.
 */
function rollup(views: View[]): Rollup {
  const roll: Rollup = {
    synced: 0,
    healthy: 0,
    drifted: 0,
    driftUnchecked: 0,
    failed: 0,
    services: { healthy: 0, total: 0 },
  }
  for (const view of views) {
    const { status } = view
    if (decodeEnum(status.sync.state, syncStates) === 'synced') roll.synced++
    if (decodeEnum(status.health.state, healthStates) === 'healthy') roll.healthy++
    if (status.drift === undefined) roll.driftUnchecked++
    else if (decodeEnum(status.drift.state, driftStates) === 'detected') roll.drifted++
    if (status.error !== undefined && status.error !== '') roll.failed++
    roll.services.healthy += status.health.services.healthy
    roll.services.total += status.health.services.total
  }
  return roll
}
