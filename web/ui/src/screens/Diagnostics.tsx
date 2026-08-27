// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'

import { apiGet } from '../api/client'
import { decodeEnum, driftStates, healthStates, syncStates } from '../api/enums'
import { listKey } from '../api/queries'
import type { ApplicationList, View } from '../api/types'
import { Icon } from '../components/Icon'
import { Loading } from '../components/StateBlock'

/**
 * Diagnostics: an integrity score and the risks behind it, derived from the
 * status the controller already reports.
 *
 * Everything here is computed from /api/v1/applications — the sync, health and
 * drift axes — and nothing is fabricated. The deeper scans the mockup sketches
 * (a per-node health matrix, an auto-resolver) need controller endpoints that
 * are a later phase, and the note at the foot says so rather than filling the
 * space with numbers this build cannot stand behind.
 */
export function Diagnostics() {
  const apps = useQuery({
    queryKey: listKey,
    queryFn: () => apiGet<ApplicationList>('/api/v1/applications'),
  })

  if (apps.isPending) return <Loading />
  if (apps.isError) {
    return (
      <p className="error" role="alert">
        {apps.error.message}
      </p>
    )
  }

  const views = apps.data.applications
  const assessed = views.map((view) => ({ name: view.spec.name, ...assess(view) }))
  const clear = assessed.filter((a) => a.severity === 'ok').length
  const risks = assessed.filter((a) => a.severity !== 'ok')
  const score = views.length === 0 ? 100 : Math.round((100 * clear) / views.length)
  const tone = score >= 90 ? 'ok' : score >= 60 ? 'warn' : 'bad'

  // The arc, drawn as SVG geometry attributes — stroke-dasharray and -dashoffset
  // are presentation attributes, not the inline style the CSP forbids. The
  // colour is a class, because a CSS var() does not resolve inside a presentation
  // attribute; see .score-arc in index.css.
  const radius = 42
  const circumference = 2 * Math.PI * radius
  const offset = circumference * (1 - score / 100)

  return (
    <section className="screen">
      <header className="screen-header">
        <h1>Diagnostics</h1>
        <p className="muted">Derived from the current application status.</p>
      </header>

      <div className="overview-grid">
        <div className="card-frame">
          <div className="card-frame-head">
            <h2>
              <Icon name="gauge" size={16} />
              System integrity
            </h2>
            <span className={`chip chip-${tone === 'ok' ? 'good' : tone === 'warn' ? 'warn' : 'bad'}`}>
              {tone === 'ok' ? 'Nominal' : 'Attention Needed'}
            </span>
          </div>
          <div className="card-frame-body">
            <div className="score">
              <svg
                className="score-ring"
                width="110"
                height="110"
                viewBox="0 0 110 110"
                role="img"
                aria-label={`Integrity score ${score} out of 100`}
              >
                <circle className="score-track" cx="55" cy="55" r={radius} />
                <circle
                  className={`score-arc score-arc-${tone}`}
                  cx="55"
                  cy="55"
                  r={radius}
                  strokeDasharray={circumference}
                  strokeDashoffset={offset}
                  transform="rotate(-90 55 55)"
                />
              </svg>
              <div>
                <div className={`score-value tone-${tone}`}>
                  {score}
                  <span className="stat-unit"> / 100</span>
                </div>
                <p className="muted">
                  {clear} of {views.length} applications synced &amp; healthy
                </p>
              </div>
            </div>
          </div>
        </div>

        <div className="card-frame">
          <div className="card-frame-head">
            <h2>
              <Icon name="warn" size={16} />
              Risks &amp; Anomalies
            </h2>
            <span className={`chip ${risks.length === 0 ? 'chip-good' : 'chip-bad'}`}>
              {risks.length} Detected
            </span>
          </div>
          <div className="card-frame-body">
            {risks.length === 0 ? (
              <div className="checklist">
                <p className="muted">No risks detected.</p>
                <div className="checklist-item checklist-item-ok">
                  <Icon name="check" size={16} />
                  <span className="checklist-text">All deployed stacks matching declared manifests</span>
                  <span className="checklist-tag">SYNC OK</span>
                </div>
                <div className="checklist-item checklist-item-ok">
                  <Icon name="check" size={16} />
                  <span className="checklist-text">Zero container task restart loops or missing services</span>
                  <span className="checklist-tag">HEALTH OK</span>
                </div>
                <div className="checklist-item checklist-item-ok">
                  <Icon name="check" size={16} />
                  <span className="checklist-text">Reconcile loop converged with zero daemon errors</span>
                  <span className="checklist-tag">ENGINE OK</span>
                </div>
              </div>
            ) : (
              <div className="risk-list">
                {risks.map((risk) => (
                  <div key={risk.name} className={`risk-card risk-${risk.severity === 'crit' ? 'crit' : 'warn'}`}>
                    <div className="risk-card-top">
                      <div className="risk-card-header">
                        <Icon name={risk.severity === 'crit' ? 'error' : 'warn'} size={16} />
                        <h3>{risk.name}</h3>
                      </div>
                      <span className={`risk-badge risk-badge-${risk.severity === 'crit' ? 'crit' : 'warn'}`}>
                        {risk.severity === 'crit' ? 'critical' : 'warning'}
                      </span>
                    </div>
                    <p>{risk.reason}</p>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      </div>

      <p className="phase2-note">
        A per-node health matrix and an auto-resolver need controller endpoints that are a later phase. This score
        is derived from the sync, health and drift the controller already reports.
      </p>
    </section>
  )
}

type Severity = 'crit' | 'warn' | 'ok'

/**
 * assess reduces one application's three axes to a single worst finding.
 *
 * A failed reconcile outranks everything, because every other field on that row
 * is then the last successful observation and not the current one (the list
 * marks the same row stale for the same reason). A dead health is next; drift
 * and being out of sync are warnings; progressing is the mildest. Each enum is
 * decoded so a state a newer controller added is read as itself.
 */
function assess(view: View): { severity: Severity; reason: string } {
  const { status } = view
  if (status.error !== undefined && status.error !== '') {
    return { severity: 'crit', reason: `Last reconcile failed: ${status.error}` }
  }
  const health = decodeEnum(status.health.state, healthStates)
  if (health === 'degraded' || health === 'missing') {
    return { severity: 'crit', reason: `Health is ${health}` }
  }
  if (status.drift !== undefined && decodeEnum(status.drift.state, driftStates) === 'detected') {
    return { severity: 'warn', reason: 'Live drift detected against the running swarm' }
  }
  if (decodeEnum(status.sync.state, syncStates) === 'out-of-sync') {
    return { severity: 'warn', reason: 'Out of sync with the repository' }
  }
  if (health === 'progressing') {
    return { severity: 'warn', reason: 'A rollout is still progressing' }
  }
  return { severity: 'ok', reason: 'Synced and healthy' }
}
