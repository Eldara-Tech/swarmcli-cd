// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'

import { apiGet } from '../api/client'
import type { DiagnosticsResponse, NodesResponse } from '../api/types'
import { Icon } from '../components/Icon'
import { Loading } from '../components/StateBlock'

/**
 * Diagnostics 2.0: Unified cluster health assessment and Swarm node topology matrix.
 */
export function Diagnostics() {
  const diag = useQuery({
    queryKey: ['diagnostics'],
    queryFn: () => apiGet<DiagnosticsResponse>('/api/v1/diagnostics'),
  })

  const nodes = useQuery({
    queryKey: ['nodes'],
    queryFn: () => apiGet<NodesResponse>('/api/v1/nodes'),
  })

  if (diag.isPending || nodes.isPending) return <Loading />
  if (diag.isError) {
    return (
      <p className="error" role="alert">
        {diag.error.message}
      </p>
    )
  }

  const score = diag.data?.score ?? 100
  const tone = diag.data?.tone ?? 'ok'
  const clearCount = diag.data?.clearCount ?? 0
  const totalCount = diag.data?.totalCount ?? 0
  const risks = diag.data?.risks ?? []
  const checks = diag.data?.checks ?? []
  const swarmNodes = nodes.data?.nodes ?? []

  const radius = 42
  const circumference = 2 * Math.PI * radius
  const offset = circumference * (1 - score / 100)

  return (
    <section className="screen">
      <header className="screen-header">
        <div>
          <h1>Diagnostics</h1>
          <p className="muted">Live cluster health scoring, risk analysis &amp; Swarm node matrix.</p>
        </div>
      </header>

      <div className="overview-grid">
        {/* System Integrity Gauge */}
        <div className="card-frame">
          <div className="card-frame-head">
            <h2>
              <Icon name="gauge" size={16} />
              Cluster Integrity
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
                  {clearCount} of {totalCount} operational checks passed
                </p>
              </div>
            </div>
          </div>
        </div>

        {/* Operational Checks Checklist */}
        <div className="card-frame">
          <div className="card-frame-head">
            <h2>
              <Icon name="check" size={16} />
              Health &amp; Invariance Audit
            </h2>
            <span className={`chip ${risks.length === 0 ? 'chip-good' : 'chip-warn'}`}>
              {risks.length === 0 ? 'All Checks Passed' : `${risks.length} Risks Detected`}
            </span>
          </div>
          <div className="card-frame-body">
            <div className="checklist">
              {checks.map((check) => (
                <div
                  key={check.id}
                  className={`checklist-item ${check.passed ? 'checklist-item-ok' : 'checklist-item-warn'}`}
                >
                  <Icon name={check.passed ? 'check' : 'warn'} size={16} />
                  <div className="checklist-meta">
                    <span className="checklist-text">{check.name}</span>
                    <span className="checklist-detail">{check.detail}</span>
                  </div>
                  <span className={`checklist-tag ${check.passed ? 'tag-pass' : 'tag-fail'}`}>
                    {check.passed ? 'PASS' : 'WARN'}
                  </span>
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>

      {/* Risks & Auto-Remediation */}
      {risks.length > 0 && (
        <div className="card-frame full-width-card">
          <div className="card-frame-head">
            <h2>
              <Icon name="warn" size={16} />
              Identified Risks &amp; Recommended Actions
            </h2>
          </div>
          <div className="card-frame-body">
            <div className="risk-list">
              {risks.map((risk) => (
                <div key={risk.id} className={`risk-card risk-${risk.severity === 'bad' ? 'crit' : 'warn'}`}>
                  <div className="risk-card-top">
                    <div className="risk-card-header">
                      <Icon name={risk.severity === 'bad' ? 'error' : 'warn'} size={16} />
                      <h3>{risk.title}</h3>
                    </div>
                    <span className={`risk-badge risk-badge-${risk.severity === 'bad' ? 'crit' : 'warn'}`}>
                      {risk.severity === 'bad' ? 'critical' : 'warning'}
                    </span>
                  </div>
                  <p>{risk.summary}</p>
                  {risk.remedy && (
                    <div className="remedy-box">
                      <strong>Remedy:</strong> {risk.remedy}
                    </div>
                  )}
                </div>
              ))}
            </div>
          </div>
        </div>
      )}

      {/* Swarm Nodes Topology Matrix */}
      <div className="card-frame full-width-card">
        <div className="card-frame-head">
          <h2>
            <Icon name="layers" size={16} />
            Swarm Node Topology &amp; Health Matrix
          </h2>
          <span className="chip chip-muted">{swarmNodes.length} {swarmNodes.length === 1 ? 'Node Active' : 'Nodes Active'}</span>
        </div>
        <div className="card-frame-body">
          <div className="node-matrix-grid">
            {swarmNodes.map((node) => {
              const taskPct = node.tasksDesired > 0 ? Math.round((node.tasksRunning / node.tasksDesired) * 100) : 100
              return (
                <div key={node.id} className="node-card-pro">
                  <div className="node-card-pro-head">
                    <div className="node-pro-title">
                      <div className="node-icon-wrap">
                        <Icon name="service" size={18} />
                        <span className={`node-status-dot ${node.status === 'ready' ? 'status-ready' : 'status-down'}`} />
                      </div>
                      <div className="node-pro-names">
                        <span className="node-pro-hostname">{node.hostname}</span>
                        <span className="node-pro-id font-mono">{node.id}</span>
                      </div>
                    </div>

                    <div className="node-pro-badges">
                      <span className={`chip ${node.status === 'ready' ? 'chip-good' : 'chip-bad'}`}>
                        {node.status.toUpperCase()}
                      </span>
                      <span className="chip chip-muted font-mono">{node.role.toUpperCase()}</span>
                      {node.leader && <span className="chip chip-warn font-mono">LEADER</span>}
                    </div>
                  </div>

                  <div className="node-pro-metrics">
                    <div className="node-metric-cell">
                      <span className="metric-cell-label">ENGINE</span>
                      <span className="metric-cell-val font-mono">v{node.engineVersion}</span>
                    </div>
                    <div className="node-metric-cell">
                      <span className="metric-cell-label">ADDRESS</span>
                      <span className="metric-cell-val font-mono">{node.addr}</span>
                    </div>
                    <div className="node-metric-cell">
                      <span className="metric-cell-label">AVAILABILITY</span>
                      <span className="metric-cell-val font-mono">{node.availability.toUpperCase()}</span>
                    </div>
                    <div className="node-metric-cell">
                      <span className="metric-cell-label">TASKS ALLOCATED</span>
                      <div className="node-task-gauge">
                        <span className="metric-cell-val font-mono">
                          {node.tasksRunning} / {node.tasksDesired}
                        </span>
                        <div className="task-progress-track">
                          <div className="task-progress-fill" style={{ width: `${taskPct}%` }} />
                        </div>
                      </div>
                    </div>
                  </div>
                </div>
              )
            })}
          </div>
        </div>
      </div>
    </section>
  )
}
