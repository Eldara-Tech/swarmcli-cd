// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'

import { apiGet } from '../api/client'
import { decodeEnum, healthStates, syncStates } from '../api/enums'
import { listKey } from '../api/queries'
import { assess, type Severity } from '../api/severity'
import type { ApplicationList, View } from '../api/types'
import { Icon } from '../components/Icon'
import { ErrorState, Loading } from '../components/StateBlock'

/**
 * Diagnostics: an integrity score and the risks behind it, derived from the
 * status the controller already reports.
 *
 * Everything here is computed from /api/v1/applications — the sync, health and
 * drift axes, folded by api/severity.ts — and nothing is fabricated. That is a
 * constraint on the checks below and not a boast: the three all-clear lines are
 * each rendered from the predicate beside them, so a line can only appear when
 * the documents say it is true. The deeper scans the mockup sketches (a
 * per-node health matrix, an auto-resolver) need controller endpoints that are
 * a later phase, and the note at the foot says so rather than filling the space
 * with claims this build cannot stand behind.
 */
export function Diagnostics() {
  const apps = useQuery({
    queryKey: listKey,
    queryFn: () => apiGet<ApplicationList>('/api/v1/applications'),
  })

  if (apps.isPending) return <Loading />
  if (apps.isError) {
    return <ErrorState message={apps.error.message} />
  }

  const views = apps.data.applications
  const assessed = views.map((view) => ({ name: view.spec.name, ...assess(view) }))
  // Clear is `ok` and only `ok`. An application whose axes read unknown has not
  // been reported on, which is not the same as having been reported clear, and
  // counting it as clear is what let a controller that had reconciled nothing
  // score 100.
  const clear = assessed.filter((a) => a.severity === 'ok').length
  const risks = assessed.filter((a) => a.severity !== 'ok')
  const score = views.length === 0 ? 100 : Math.round((100 * clear) / views.length)

  // The tone is taken from the risks and not from the score, so the ring, the
  // chip and the list cannot disagree. A threshold on the score said "Nominal"
  // beside a critical risk as soon as nine applications in ten were clear, and
  // Math.round said 100/100 beside one from two hundred.
  const tone = worst(risks.map((r) => r.severity))

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
            <span className={`chip ${chipClass(tone)}`}>{tone === 'ok' ? 'Nominal' : 'Attention Needed'}</span>
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
            <span className={`chip ${chipClass(tone)}`}>{risks.length} Detected</span>
          </div>
          <div className="card-frame-body">
            {risks.length === 0 ? <Checks views={views} /> : <RiskList risks={risks} />}
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

/** The chip class for a tone. `unknown` reads amber: it is not an all-clear. */
function chipClass(tone: Severity): string {
  if (tone === 'ok') return 'chip-good'
  if (tone === 'crit') return 'chip-bad'
  return 'chip-warn'
}

/** worst is the loudest severity in a set, or `ok` for an empty one. */
function worst(severities: Severity[]): Severity {
  if (severities.includes('crit')) return 'crit'
  if (severities.includes('warn')) return 'warn'
  if (severities.includes('unknown')) return 'unknown'
  return 'ok'
}

/**
 * The all-clear list, one line per predicate that actually held.
 *
 * Each line is rendered from the check beside it rather than asserted, which is
 * the difference between a checklist and a picture of one. The middle line says
 * "missing services" and not "restart loops" because HealthState is what the
 * API reports: there is no task-level document, so a restart loop is not a
 * thing this build can claim to have looked for.
 *
 * With no applications at all there is nothing that held, so the list is empty
 * and only the sentence above it remains — three ticks over an empty fleet were
 * three claims about nothing.
 */
function Checks({ views }: { views: View[] }) {
  const every = (predicate: (view: View) => boolean) => views.length > 0 && views.every(predicate)
  const checks = [
    {
      tag: 'SYNC OK',
      text: 'All deployed stacks matching declared manifests',
      held: every((v) => decodeEnum(v.status.sync.state, syncStates) === 'synced'),
    },
    {
      tag: 'HEALTH OK',
      text: 'No service reported degraded or missing',
      held: every((v) => {
        const health = decodeEnum(v.status.health.state, healthStates)
        return health !== 'degraded' && health !== 'missing'
      }),
    },
    {
      tag: 'ENGINE OK',
      text: 'Reconcile loop converged with zero daemon errors',
      held: every((v) => v.status.error === undefined || v.status.error === ''),
    },
  ].filter((check) => check.held)

  return (
    <div className="checklist">
      <p className="muted">No risks detected.</p>
      {checks.map((check) => (
        <div key={check.tag} className="checklist-item checklist-item-ok">
          <Icon name="check" size={16} />
          <span className="checklist-text">{check.text}</span>
          <span className="checklist-tag">{check.tag}</span>
        </div>
      ))}
    </div>
  )
}

function RiskList({ risks }: { risks: { name: string; severity: Severity; reason: string }[] }) {
  return (
    <div className="risk-list">
      {risks.map((risk) => (
        <div key={risk.name} className={`risk-card risk-${risk.severity}`}>
          <div className="risk-card-top">
            <div className="risk-card-header">
              <Icon name={badge[risk.severity].icon} size={16} />
              <h3>{risk.name}</h3>
            </div>
            <span className={`risk-badge risk-badge-${risk.severity}`}>{badge[risk.severity].label}</span>
          </div>
          <p>{risk.reason}</p>
        </div>
      ))}
    </div>
  )
}

/** How each severity is labelled and glyphed on its card. `ok` never renders one. */
const badge: Record<Severity, { label: string; icon: 'error' | 'warn' | 'app' }> = {
  crit: { label: 'critical', icon: 'error' },
  warn: { label: 'warning', icon: 'warn' },
  unknown: { label: 'unknown', icon: 'app' },
  ok: { label: 'ok', icon: 'app' },
}
