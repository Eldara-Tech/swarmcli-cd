// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router'

import { appSetShape } from '../api/appset'
import { apiGet } from '../api/client'
import { useFeature } from '../api/discovery'
import { decodeEnum, driftStates, healthStates, syncStates } from '../api/enums'
import type { ApplicationList, ControllerStatus, View } from '../api/types'
import { DriftCell, HealthChip, SyncChip } from '../components/StateChip'
import { Instant } from '../components/Instant'
import { destination, plural, serviceCounts, shortRevision } from '../format'

/**
 * The applications list: every application the token may see, in one request.
 *
 * # The filters live in the URL and nowhere else
 *
 * Not in state, not in sessionStorage. A filtered list is then a link — the
 * thing somebody pastes into an incident channel so that the next person opens
 * the same three rows rather than a healthy-looking twenty. Stored filters
 * would also mean a tab that silently hides applications on the next visit,
 * which is the one failure a status screen must not have.
 *
 * Written with `replace`, because a filter is not a place: typing four
 * characters into the search box would otherwise take four presses of Back to
 * undo.
 *
 * # Two presentations, one row's worth of data
 *
 * The table mirrors `swarmcli-cd app list` column for column, including which
 * columns appear at all — an operator moving between the CLI and this screen
 * should not have to translate. The cards are the same fields with room for the
 * sentences a cell cannot hold.
 */
export function Applications() {
  const [params, setParams] = useSearchParams()
  // False in a build with no capability endpoint — see api/discovery.ts — so
  // the free build's table has the columns Phase B shipped and no others.
  const showSwarm = useFeature('multi-swarm')

  const applications = useQuery({
    queryKey: ['applications'],
    queryFn: () => apiGet<ApplicationList>('/api/v1/applications'),
  })
  // The controller's own state, for the banner below. Same query key as the
  // controller screen, so opening that screen from here costs no request.
  const controller = useQuery({
    queryKey: ['status'],
    queryFn: () => apiGet<ControllerStatus>('/api/v1/status'),
  })

  const filters: Filters = {
    text: params.get('q') ?? '',
    sync: params.get('sync') ?? '',
    health: params.get('health') ?? '',
    drift: params.get('drift') ?? '',
  }
  const cards = params.get('view') === 'cards'

  function set(key: string, value: string): void {
    const next = new URLSearchParams(params)
    // Dropped rather than set empty, so that a list nobody has filtered has a
    // clean URL and a pasted one carries only what was actually chosen.
    if (value === '') next.delete(key)
    else next.set(key, value)
    setParams(next, { replace: true })
  }

  if (applications.isPending) return <p>Loading…</p>
  if (applications.isError) {
    return (
      <p className="error" role="alert">
        {applications.error.message}
      </p>
    )
  }

  const all = applications.data.applications
  const shown = all.filter((v) => matches(v, filters))
  // No status yet, or a status request that failed, says nothing about the app
  // set — so the banner stays away rather than guessing. The list is the thing
  // this screen owes the reader, and it must not be blocked on a second read.
  const shape = controller.data === undefined ? 'ok' : appSetShape(controller.data)

  return (
    <section className="screen">
      <header className="screen-header">
        <h1>Applications</h1>
        <p data-testid="application-count">
          {shown.length === all.length
            ? plural(all.length, 'application')
            : `${shown.length} of ${plural(all.length, 'application')}`}
        </p>
      </header>

      {/* The reason this screen and the controller screen shipped together: a
          refused app set makes every row below describe a set that is not the
          one in the repository, and nothing on a row can say so. */}
      {(shape === 'stale' || shape === 'partial' || shape === 'never-loaded') && (
        <p className={`notice notice-${shape}`} role="status">
          {appSetWarning[shape]} <Link to="/status">Controller status</Link>
        </p>
      )}

      <form
        className="filters"
        // Nothing to submit: every control applies as it changes, and the
        // Enter key must not reload the page and lose the query cache.
        onSubmit={(event) => {
          event.preventDefault()
        }}
      >
        <label htmlFor="filter-q">Search</label>
        <input
          id="filter-q"
          type="search"
          value={filters.text}
          placeholder="name, repository or revision"
          onChange={(event) => {
            set('q', event.target.value)
          }}
        />

        <label htmlFor="filter-sync">Sync</label>
        <select
          id="filter-sync"
          value={filters.sync}
          onChange={(event) => {
            set('sync', event.target.value)
          }}
        >
          <option value="">any</option>
          {syncStates.map((state) => (
            <option key={state} value={state}>
              {state}
            </option>
          ))}
        </select>

        <label htmlFor="filter-health">Health</label>
        <select
          id="filter-health"
          value={filters.health}
          onChange={(event) => {
            set('health', event.target.value)
          }}
        >
          <option value="">any</option>
          {healthStates.map((state) => (
            <option key={state} value={state}>
              {state}
            </option>
          ))}
        </select>

        <label htmlFor="filter-drift">Drift</label>
        <select
          id="filter-drift"
          value={filters.drift}
          onChange={(event) => {
            set('drift', event.target.value)
          }}
        >
          <option value="">any</option>
          {driftStates.map((state) => (
            <option key={state} value={state}>
              {state}
            </option>
          ))}
          {/* Its own option rather than a fifth drift state: these applications
              run driftDetection: manifest and were never asked, which is not a
              value the enum has or should have. */}
          <option value={notChecked}>not checked</option>
        </select>

        {/* Labelled with what it will do rather than with what is showing, and
            deliberately not aria-pressed as well: a toggle whose name changes
            and whose pressed state changes reads as two contradictory controls
            to a screen reader. */}
        <button
          type="button"
          className="view-toggle"
          onClick={() => {
            set('view', cards ? '' : 'cards')
          }}
        >
          {cards ? 'Table view' : 'Card view'}
        </button>
      </form>

      {all.length === 0 && <p className="empty">No applications.</p>}
      {all.length > 0 && shown.length === 0 && <p className="empty">No application matches these filters.</p>}
      {shown.length > 0 && (cards ? <Cards views={shown} /> : <Table views={shown} showSwarm={showSwarm} />)}

      <ReconcileErrors views={shown} />
    </section>
  )
}

/** The drift filter's fifth option: absent rather than any member of the enum. */
const notChecked = 'not-checked'

const appSetWarning: Record<'stale' | 'partial' | 'never-loaded', string> = {
  stale: 'The running application set is stale — a newer one is being refused, so these rows describe the last set that validated.',
  partial: 'The last application-set load reported an error, so these rows may not be the whole set.',
  'never-loaded': 'No application set has ever loaded. This list is empty for a reason that has nothing to do with your applications.',
}

interface Filters {
  text: string
  sync: string
  health: string
  drift: string
}

function matches(view: View, filters: Filters): boolean {
  const { spec, status } = view
  if (filters.text !== '') {
    const needle = filters.text.toLowerCase()
    const haystack = [spec.name, spec.source.repoURL, spec.source.revision, status.sync.revision ?? '']
    if (!haystack.some((field) => field.toLowerCase().includes(needle))) return false
  }
  // Decoded, not compared raw: a controller ahead of this build sends a state
  // the option list does not offer, and it has to be filterable as "unknown"
  // rather than matching nothing at all.
  if (filters.sync !== '' && decodeEnum(status.sync.state, syncStates) !== filters.sync) return false
  if (filters.health !== '' && decodeEnum(status.health.state, healthStates) !== filters.health) return false
  if (filters.drift === notChecked) return status.drift === undefined
  if (filters.drift !== '') {
    if (status.drift === undefined) return false
    if (decodeEnum(status.drift.state, driftStates) !== filters.drift) return false
  }
  return true
}

/**
 * stale reports whether every other field on a row is the last *successful*
 * observation rather than the current one.
 *
 * status.error is the last reconcile that did not get through, and nothing else
 * on the status moves when one fails — so an application whose repository has
 * been unreachable for a week still reports last week's sync state, health and
 * revision under an observedAt of three seconds ago, and the row reads green
 * (#107). Marking the row is the only thing that stops it being believed.
 */
function stale(view: View): boolean {
  return view.status.error !== undefined && view.status.error !== ''
}

/**
 * The table, with the CLI's two conditional columns and its reasoning.
 *
 * Both are computed over every application rather than over the filtered rows:
 * a column that appeared and disappeared as somebody typed in the search box
 * would be a table that changes shape under the reader.
 *
 * Swarm is the third conditional column and the one with a different condition:
 * it is on the build rather than on the data. Every spec carries
 * destination.swarm and a single-swarm build resolves every one of them to the
 * swarm the controller runs in, so the column would say the same thing on every
 * row — which is why it is gated on features["multi-swarm"] and not on the rows
 * having anything to distinguish. The CLI has no counterpart yet; its
 * multi-swarm surface is its own issue, and until then this is the one place
 * the two tables differ.
 */
function Table({ views, showSwarm }: { views: View[]; showSwarm: boolean }) {
  const showDrift = views.some((v) => v.status.drift !== undefined)
  const showError = views.some(stale)

  return (
    <table className="app-table">
      <thead>
        <tr>
          <th scope="col">Name</th>
          {showSwarm && <th scope="col">Swarm</th>}
          <th scope="col">Sync</th>
          {showDrift && <th scope="col">Drift</th>}
          <th scope="col">Health</th>
          <th scope="col">Services</th>
          <th scope="col">Revision</th>
          {showError && <th scope="col">Reconcile</th>}
        </tr>
      </thead>
      <tbody>
        {views.map((view) => (
          <tr key={view.spec.name} className={stale(view) ? 'row-stale' : undefined}>
            <th scope="row">
              <Link to={detailPath(view.spec.name)}>{view.spec.name}</Link>
            </th>
            {showSwarm && <td>{destination(view.spec.destination)}</td>}
            <td>
              <SyncChip state={view.status.sync.state} />
            </td>
            {showDrift && (
              <td>
                <DriftCell drift={view.status.drift} />
              </td>
            )}
            <td>
              <HealthChip state={view.status.health.state} />
            </td>
            <td>{serviceCounts(view.status.health.services)}</td>
            <td>
              <Revision revision={view.status.sync.revision} />
            </td>
            {showError && <td>{stale(view) ? <span className="chip chip-bad">failed</span> : 'ok'}</td>}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function Cards({ views }: { views: View[] }) {
  return (
    <ul className="app-cards">
      {views.map((view) => (
        <li key={view.spec.name}>
          <article className={stale(view) ? 'card card-stale' : 'card'}>
            <h2>
              <Link to={detailPath(view.spec.name)}>{view.spec.name}</Link>
            </h2>
            <p className="card-chips">
              <SyncChip state={view.status.sync.state} />
              <HealthChip state={view.status.health.state} />
              <DriftCell drift={view.status.drift} />
            </p>
            <dl className="card-fields">
              <dt>Services</dt>
              <dd>{serviceCounts(view.status.health.services)}</dd>
              <dt>Revision</dt>
              <dd>
                <Revision revision={view.status.sync.revision} />
              </dd>
              <dt>Repository</dt>
              <dd className="wrap">{view.spec.source.repoURL}</dd>
              <dt>Observed</dt>
              <dd>
                <Instant at={view.status.observedAt} />
              </dd>
            </dl>
          </article>
        </li>
      ))}
    </ul>
  )
}

/**
 * ReconcileErrors names why, under the list rather than in it.
 *
 * The choice controller/app.go makes for the same reason: a reconcile error is
 * a sentence — a repository URL and a dial error, a chart that would not render
 * — and a cell wide enough for one would wreck every other row. The mark on the
 * row says which application to stop trusting; this says what happened to it.
 */
function ReconcileErrors({ views }: { views: View[] }) {
  const failed = views.filter(stale)
  if (failed.length === 0) return null
  return (
    <div className="reconcile-errors" role="status">
      <h2>Last reconcile failed</h2>
      <p>Everything shown for these applications is the last successful observation, not the current one.</p>
      <ul>
        {failed.map((view) => (
          <li key={view.spec.name}>
            <strong>{view.spec.name}</strong>: {view.status.error}
          </li>
        ))}
      </ul>
    </div>
  )
}

/** An application that has never been reconciled has no revision to abbreviate. */
function Revision({ revision }: { revision?: string }) {
  if (revision === undefined || revision === '') return <span className="muted">–</span>
  return <code title={revision}>{shortRevision(revision)}</code>
}

function detailPath(name: string): string {
  return `/applications/${encodeURIComponent(name)}`
}
