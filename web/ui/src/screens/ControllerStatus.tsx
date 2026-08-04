// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'

import { appSetShape, type AppSetShape } from '../api/appset'
import { apiGet } from '../api/client'
import { controllerKey } from '../api/queries'
import type { AppSetStatus, ControllerStatus as Status } from '../api/types'
import { Instant } from '../components/Instant'
import { shortRevision } from '../format'

/**
 * The controller itself, as distinct from the applications it reconciles.
 *
 * It shipped with the list rather than with the settings screens because it is
 * the screen that explains why the list is wrong. A stale app set means every
 * row over there describes a set the controller is refusing; a never-loaded one
 * means the list is empty for a reason that has nothing to do with anybody's
 * applications.
 *
 * The three failure shapes are drawn differently on purpose, and the loudest is
 * the one that would otherwise be quietest: "no applications and no error on
 * any of them" is what a controller pointed at an unreachable repository looks
 * like, and it has none of the per-row signals the other two have.
 */
export function ControllerStatusScreen() {
  const status = useQuery({
    queryKey: controllerKey,
    queryFn: () => apiGet<Status>('/api/v1/status'),
  })

  if (status.isPending) return <p>Loading…</p>
  if (status.isError) {
    return (
      <p className="error" role="alert">
        {status.error.message}
      </p>
    )
  }

  const set = status.data.appSet
  const shape = appSetShape(status.data)

  return (
    <section className="screen">
      <header className="screen-header">
        <h1>Controller</h1>
      </header>

      <AppSetNotice shape={shape} appSet={set} />

      <dl className="detail-fields">
        <dt>Mode</dt>
        {/* An empty mode is not an unknown one: it is the status handler's
            no-controller arm, and "unknown" would suggest something to diagnose
            where there is nothing wired to diagnose. */}
        <dd>{set.mode === '' ? <span className="muted">none</span> : set.mode}</dd>
        {set.source !== undefined && set.source !== '' && (
          <>
            <dt>Source</dt>
            <dd className="wrap">{set.source}</dd>
          </>
        )}
        {set.revision !== undefined && set.revision !== '' && (
          <>
            <dt>Revision</dt>
            <dd>
              <code title={set.revision}>{shortRevision(set.revision)}</code>
            </dd>
          </>
        )}
        <dt>Loaded</dt>
        <dd data-testid="app-set-loaded">
          <Instant at={set.loadedAt} />
        </dd>
        <dt>Applications</dt>
        <dd data-testid="app-set-applications">{status.data.applications}</dd>
      </dl>

      {/* The three lists that only exist when something happened. A "Pruned:
          none" row on every healthy controller would train an operator to stop
          reading the block — the same reason controller/status.go omits them. */}
      <NameList label="Orphaned" hint="They have left the application set. Their stacks are still deployed and nobody reconciles them." names={set.orphaned} />
      <NameList label="Pruned" hint="Resources this controller has deleted, most recent last." names={set.pruned} />
      <NameList label="Prune held" hint="The sweep is waiting for these applications to reconcile and say what they declare." names={set.pruneHeldBy} />
    </section>
  )
}

/**
 * The notice, one per shape, and no two alike.
 *
 * `ok` renders nothing at all: a controller that is fine should say so by
 * having nothing to say, so that the presence of a block here is itself the
 * signal.
 */
function AppSetNotice({ shape, appSet }: { shape: AppSetShape; appSet: AppSetStatus }) {
  if (shape === 'ok') return null

  if (shape === 'unwired') {
    // Not a failure, and it must not be drawn as one. This is api.go answering
    // with a zero status because it has no controller — every field below is a
    // zero value rather than an observation, which is why the failure logic
    // above never ran.
    return (
      <div className="notice notice-unwired" role="status">
        <h2>No application set source is wired</h2>
        <p>This controller is serving the API without an application-set loop, so there is nothing to load and nothing to be stale.</p>
      </div>
    )
  }

  if (shape === 'never-loaded') {
    return (
      <div className="notice notice-never-loaded" role="alert" data-testid="app-set-notice">
        <h2>No application set has ever loaded</h2>
        <p>
          The controller has never managed to load an application set. Nothing is being reconciled, and the
          applications list is empty for a reason that has nothing to do with any application in it. This is
          what a controller pointed at an unreachable repository looks like.
        </p>
        <Reason error={appSet.error} />
      </div>
    )
  }

  if (shape === 'stale') {
    return (
      <div className="notice notice-stale" role="alert" data-testid="app-set-notice">
        <h2>The running application set is stale</h2>
        <p>
          A newer application set is being refused, and what is running is the last one that validated. The
          applications below are real; the file they came from is behind the repository.
        </p>
        <Reason error={appSet.error} />
      </div>
    )
  }

  return (
    <div className="notice notice-partial" role="alert" data-testid="app-set-notice">
      <h2>The last load reported an error</h2>
      <p>
        The application set loaded and is being reconciled, but part of the change did not land — one
        application that would not start, say, while the rest of it did.
      </p>
      <Reason error={appSet.error} />
    </div>
  )
}

function Reason({ error }: { error?: string }) {
  if (error === undefined || error === '') return null
  return <p className="wrap notice-reason">{error}</p>
}

function NameList({ label, hint, names }: { label: string; hint: string; names?: string[] }) {
  if (names === undefined || names.length === 0) return null
  return (
    <section className="name-list">
      <h2>{label}</h2>
      <p>{hint}</p>
      <ul>
        {names.map((name) => (
          <li key={name}>{name}</li>
        ))}
      </ul>
    </section>
  )
}
