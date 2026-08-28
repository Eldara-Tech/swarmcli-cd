// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { assess, healthToneOf, toneOf } from '../api/severity'
import type { ReleaseStatus, ServiceStatus, View } from '../api/types'
import { Dot } from './Dot'
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
  const appTone = toneOf(assess(view).severity)

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
  const tone = healthToneOf(release.health.state)
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
  const tone = healthToneOf(service.health)
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
