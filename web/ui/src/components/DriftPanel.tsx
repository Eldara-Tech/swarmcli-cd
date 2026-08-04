// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { Fragment } from 'react'

import { decodeEnum, driftReasons, driftStates } from '../api/enums'
import type { ReleaseStatus, ServiceDrift } from '../api/types'

/**
 * What no longer matches the repository, per release.
 *
 * Separate from the service tree because it answers a different question: the
 * tree says whether what is running is working, this says whether it is what
 * git asked for. A release with nothing to report renders nothing at all.
 *
 * It differs from controller/app.go's printDrift in one place, deliberately: a
 * release whose drift state is "unknown" is rendered here even though it has no
 * services and no resources to list. That state is the record that the question
 * was asked and went unanswered, and a release whose live state could not be
 * read is not converged — so silence would read as "nothing to report", which
 * is the opposite of what it means.
 */
export function DriftPanel({ releases }: { releases: ReleaseStatus[] }) {
  const drifted = releases.filter(hasFindings)
  if (drifted.length === 0) return null

  return (
    <section className="drift-panel">
      <h2>Drift</h2>
      {drifted.map((release) => {
        // Narrowed by hasFindings; repeated here because a filter cannot say so
        // to the compiler.
        const drift = release.drift
        if (drift === undefined) return null
        const services = drift.services ?? []
        const resources = drift.resources ?? []
        const rolledBack = services.filter((s) => decodeEnum(s.reason, driftReasons) === 'rolled-back')
        const orphans = services.some((s) => s.orphaned === true) || resources.length > 0
        const environment = services.some((s) => (s.fields ?? []).some((f) => f.field.startsWith('env[')))

        return (
          <article key={release.name} className="drift-release">
            <h3>{release.name}</h3>
            {drift.message !== undefined && drift.message !== '' && (
              <p className="drift-message">{drift.message}</p>
            )}

            {(services.length > 0 || resources.length > 0) && (
              <table className="drift-table">
                <thead>
                  <tr>
                    <th scope="col">Kind</th>
                    <th scope="col">Name</th>
                    <th scope="col">Reason</th>
                    <th scope="col">Field</th>
                    <th scope="col">Desired</th>
                    <th scope="col">Live</th>
                  </tr>
                </thead>
                <tbody>
                  {services.map((service) => (
                    <Fragment key={service.name}>
                      {(service.fields ?? []).length === 0 ? (
                        // No fields means the service itself is the finding:
                        // missing, unexpected or rolled back, none of which has
                        // a field to name.
                        <tr>
                          <td>service</td>
                          <td>{service.name}</td>
                          <td>{reason(service)}</td>
                          <td className="muted">–</td>
                          <td className="muted">–</td>
                          <td className="muted">–</td>
                        </tr>
                      ) : (
                        (service.fields ?? []).map((field) => (
                          <tr key={`${service.name}/${field.field}`}>
                            <td>service</td>
                            <td>{service.name}</td>
                            <td>{reason(service)}</td>
                            <td>
                              <code>{field.field}</code>
                            </td>
                            {/* Verbatim, both of them. The controller decides
                                what a value may say — an environment difference
                                arrives already reduced to set/absent/changed —
                                and anything this side added back would be
                                disclosing what it withheld. */}
                            <td>{field.desired}</td>
                            <td>{field.live}</td>
                          </tr>
                        ))
                      )}
                      {service.truncated !== undefined && service.truncated > 0 && (
                        <tr className="drift-more">
                          <td>service</td>
                          <td>{service.name}</td>
                          <td>{reason(service)}</td>
                          <td colSpan={3}>(and {service.truncated} more)</td>
                        </tr>
                      )}
                    </Fragment>
                  ))}
                  {/* Always orphaned, and never field-compared: Swarm cannot
                      update a network in place and configs and secrets are
                      immutable, so the only finding either can produce is that
                      the repository has stopped declaring it. */}
                  {resources.map((resource) => (
                    <tr key={`${resource.kind}/${resource.name}`}>
                      {/* Rendered verbatim: ResourceKind is the one enum with no
                          unknown fallback, and coercing a kind a newer
                          controller added would hide exactly what a drift panel
                          exists to show. */}
                      <td>{resource.kind}</td>
                      <td>{resource.name}</td>
                      <td>unexpected (orphaned)</td>
                      <td className="muted">–</td>
                      <td className="muted">–</td>
                      <td className="muted">–</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}

            {rolledBack.map((service) => (
              <p key={service.name} className="drift-note drift-rollback">
                <strong>{service.name}</strong> was rolled back by Swarm; a sync will not re-apply it. The
                platform rejected the spec the repository asks for, so the remedy is a commit.
                {service.message !== undefined && service.message !== '' && (
                  <> {service.message}</>
                )}
              </p>
            ))}

            {orphans && (
              <p className="drift-note">
                Orphaned entries go on the next sync. An unexpected service with no marker is one this
                controller will never touch.
              </p>
            )}

            {environment && (
              <p className="drift-note">
                An environment difference reports only whether the variable is set, absent or changed. The
                value is never sent: this view is readable by anyone holding read scope, and an environment
                variable is exactly where a credential would be.
              </p>
            )}
          </article>
        )
      })}
    </section>
  )
}

function hasFindings(release: ReleaseStatus): boolean {
  const drift = release.drift
  if (drift === undefined) return false
  if (decodeEnum(drift.state, driftStates) === 'unknown') return true
  return (drift.services ?? []).length > 0 || (drift.resources ?? []).length > 0
}

/**
 * reason renders one finding, marking an unexpected service this application
 * provably installed and no longer declares.
 *
 * The distinction is the whole of what makes deleting one safe: an orphaned
 * service is what a sync removes once syncPolicy.pruneResources is on, and an
 * unexpected one without the marker is something this controller will never
 * touch. Reading them as the same thing is how somebody deletes a service
 * nobody asked them to.
 */
function reason(service: ServiceDrift): string {
  const decoded = decodeEnum(service.reason, driftReasons)
  return service.orphaned === true ? `${decoded} (orphaned)` : decoded
}
