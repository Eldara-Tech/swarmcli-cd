// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { apiGet } from '../api/client'
import { listKey } from '../api/queries'
import type { ApplicationList } from '../api/types'
import { ServiceLogViewer } from '../components/ServiceLogViewer'
import { TerminalStream } from '../components/TerminalStream'
import { useLive } from '../live'

/**
 * Monitor 2.0: Interactive controller stream and live per-service container log console.
 */
export function Monitor() {
  const live = useLive()
  const [streamMode, setStreamMode] = useState<'controller' | 'service'>('controller')
  const [selectedApp, setSelectedApp] = useState<string>('')
  const [selectedService, setSelectedService] = useState<string>('')

  const appsQuery = useQuery({
    queryKey: listKey,
    queryFn: () => apiGet<ApplicationList>('/api/v1/applications'),
  })

  const views = appsQuery.data?.applications ?? []
  const activeApp = views.find((v) => v.spec.name === selectedApp) ?? views[0]
  const currentApp = selectedApp || activeApp?.spec.name || ''

  const appsList = views.map((v) => {
    const svcs = v.status.releases?.flatMap((r) => r.services?.map((s) => s.name) ?? []) ?? []
    return {
      name: v.spec.name,
      services: svcs.length > 0 ? svcs : [`${v.spec.name}_web`],
    }
  })

  const currentAppObj = appsList.find((a) => a.name === currentApp)
  const currentService = selectedService || currentAppObj?.services[0] || (currentApp ? `${currentApp}_web` : '')

  return (
    <section className="screen monitor-screen">
      <header className="screen-header">
        <div>
          <h1>Monitor</h1>
          <p className="muted">Live operational telemetry, reconcile events &amp; container output.</p>
        </div>

        <div className="btn-segment header-segment">
          <button
            type="button"
            className={`btn-seg ${streamMode === 'service' ? 'active' : ''}`}
            onClick={() => setStreamMode('service')}
          >
            Service Container Logs
          </button>
          <button
            type="button"
            className={`btn-seg ${streamMode === 'controller' ? 'active' : ''}`}
            onClick={() => setStreamMode('controller')}
          >
            Controller Events
          </button>
        </div>
      </header>

      {streamMode === 'service' ? (
        <ServiceLogViewer
          app={currentApp}
          service={currentService}
          apps={appsList}
          onSelectApp={(app) => {
            setSelectedApp(app)
            setSelectedService('')
          }}
          onSelectService={(svc) => setSelectedService(svc)}
        />
      ) : (
        <TerminalStream title="Controller stream" events={live.log} className="monitor-terminal" />
      )}
    </section>
  )
}
