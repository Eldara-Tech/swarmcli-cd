// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { decodeEnum, driftStates, healthStates, syncStates } from '../api/enums'
import type { HealthState, SyncState } from '../api/enums'
import type { DriftState } from '../api/enums'
import type { ReleaseStatus, ServiceStatus, View } from '../api/types'
import { Dot, type DotTone } from './Dot'
import { Icon } from './Icon'

/**
 * The application as a map: application → releases → services, drawn as node
 * cards on connector rails.
 *
 * It stops at services for the reason the overview's tree does — that is where
 * the API stops. ServiceStatus carries running and desired counts but no list
 * of tasks, so the replica pips below are a rendering of "running of desired"
 * and not a claim about individual containers the controller never named. A
 * fourth level of pods, as the ArgoCD mockup sketches, would be promising depth
 * no endpoint can answer.
 */
export function TopologyTree({ view }: { view: View }) {
  const releases = view.status.releases ?? []
  const appTone = worstTone(view) as DotTone

  return (
    <div className="topo">
      <div className="topo-item">
        <div className={`topo-node topo-${appTone}`}>
          <Dot tone={appTone} />
          <Icon name="app" size={16} />
          <span className="topo-name">{view.spec.name}</span>
          <span className="topo-kind label-caps">application</span>
        </div>
        {releases.length > 0 && (
          <div className="topo-children">
            {releases.map((release) => (
              <ReleaseNode key={release.name} release={release} />
            ))}
          </div>
        )}
      </div>

      <div className="topo-legend">
        <div className="topo-legend-item">
          <Dot tone="ok" />
          <span>Healthy / In sync</span>
        </div>
        <div className="topo-legend-item">
          <Dot tone="warn" />
          <span>Progressing / Out of sync</span>
        </div>
        <div className="topo-legend-item">
          <Dot tone="bad" />
          <span>Degraded / Missing / Error</span>
        </div>
      </div>
    </div>
  )
}

function ReleaseNode({ release }: { release: ReleaseStatus }) {
  const services = release.services ?? []
  const tone = healthTone(release.health.state) as DotTone
  return (
    <div className="topo-item">
      <div className={`topo-node topo-${tone}`}>
        <Dot tone={tone} />
        <Icon name="layers" size={16} />
        <span className="topo-name">{release.name}</span>
        <code className="topo-meta">
          {release.chart} {release.version}
        </code>
      </div>
      {services.length > 0 && (
        <div className="topo-children">
          {services.map((service) => (
            <ServiceNode key={service.name} service={service} />
          ))}
        </div>
      )}
    </div>
  )
}

function ServiceNode({ service }: { service: ServiceStatus }) {
  // Cap the pips so a wide service (a global mode across a large swarm) draws a
  // row rather than a paragraph; the exact count is beside them in any case.
  const pips = Math.min(Math.max(service.desired, 0), 12)
  const tone = healthTone(service.health) as DotTone
  return (
    <div className="topo-item">
      <div className={`topo-node topo-${tone}`}>
        <Dot tone={tone} />
        <Icon name="service" size={16} />
        <span className="topo-name">{service.name}</span>
        <span className="topo-meta">
          {service.running}/{service.desired}
        </span>
        {pips > 0 && (
          <span className="pips" aria-hidden="true">
            {Array.from({ length: pips }, (_, index) => (
              <span key={index} className={index < service.running ? 'pip pip-on' : 'pip pip-off'} />
            ))}
          </span>
        )}
      </div>
    </div>
  )
}

/** The tone one status axis reads as, in the console's three-way palette. */
function healthTone(state: HealthState): string {
  switch (decodeEnum(state, healthStates)) {
    case 'healthy':
      return 'ok'
    case 'degraded':
    case 'missing':
      return 'bad'
    case 'progressing':
      return 'warn'
    default:
      return 'info'
  }
}

function syncTone(state: SyncState): string {
  switch (decodeEnum(state, syncStates)) {
    case 'synced':
      return 'ok'
    case 'out-of-sync':
      return 'warn'
    default:
      return 'info'
  }
}

function driftTone(state: DriftState): string {
  return decodeEnum(state, driftStates) === 'detected' ? 'warn' : 'ok'
}

/**
 * worstTone folds an application's three axes and its reconcile error into the
 * single tone its node wears — the same "show me the worst thing first" the
 * list's stale mark makes: a failed reconcile is red, then a dead health or
 * sync, then a warning, then unknown, and only an all-clear application is green.
 */
function worstTone(view: View): string {
  const { status } = view
  if (status.error !== undefined && status.error !== '') return 'bad'
  const tones = [healthTone(status.health.state), syncTone(status.sync.state)]
  if (status.drift !== undefined) tones.push(driftTone(status.drift.state))
  if (tones.includes('bad')) return 'bad'
  if (tones.includes('warn')) return 'warn'
  if (tones.includes('info')) return 'info'
  return 'ok'
}
