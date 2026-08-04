// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { useQuery } from '@tanstack/react-query'

import { apiGet } from '../api/client'
import type { ApplicationList } from '../api/types'

/**
 * The applications screen, as far as B1 takes it: the one request that proves
 * the whole chain — the bearer header, the guarded endpoint, the query cache and
 * the 401 path back to the login screen.
 *
 * B3 (#173) replaces the body below with the real list. What it must not replace
 * is the query key or the endpoint, which B4's invalidation map is written
 * against.
 */
export function Applications() {
  const applications = useQuery({
    queryKey: ['applications'],
    queryFn: () => apiGet<ApplicationList>('/api/v1/applications'),
  })

  if (applications.isPending) return <p>Loading…</p>
  if (applications.isError) {
    return (
      <p className="error" role="alert">
        {applications.error.message}
      </p>
    )
  }

  return (
    <p data-testid="application-count">
      {applications.data.applications.length} application
      {applications.data.applications.length === 1 ? '' : 's'}
    </p>
  )
}
