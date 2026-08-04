// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The two discovery documents, mirrored from api/discovery.go.
//
// They are asymmetric for the reason that file gives — a login screen has to
// know how to log in before anyone is authenticated, and everything else about
// the build is behind the token — and the asymmetry is visible here as the two
// readers: bootstrap.json goes through publicGet and the capability document
// through apiGet. Reading the public one with a bearer would be reading it with
// a credential the browser does not have yet.
//
// # Everything defaults to off
//
// useFeature answers false while the request is in flight, when it failed, and
// when the document does not carry the key. That is not defensiveness: it is
// the acceptance criterion of #178. A build with no capability endpoint at all
// — which is every controller before Phase C — must render exactly the UI Phase
// B shipped, and it does so by taking the same path an all-false document
// takes. src/capabilities.test.tsx compares the two DOMs to prove it.

import { useQuery, type UseQueryResult } from '@tanstack/react-query'

import { useToken } from '../auth/useToken'
import { apiGet, publicGet } from './client'

/** The path the login screen reads before anyone has a credential. */
export const bootstrapPath = '/ui/bootstrap.json'

const capabilitiesPath = '/api/v1/capabilities'

/** Exported so a test can wait for the query to settle rather than for a paint. */
export const capabilitiesKey = ['capabilities'] as const

const bootstrapKey = ['bootstrap'] as const

/** One way to obtain a credential, as the public document reports it. */
export interface LoginOption {
  /** What this screen branches on: "token", "sso". */
  id: string
  label: string
  /**
   * Where the browser goes to begin. Absent — never empty — for a credential
   * that is typed in, because api/discovery.go omits the key rather than
   * sending "" precisely so that a UI need not know that "" means "do not
   * navigate".
   */
  start?: string
}

export interface BootstrapDocument {
  /** Always an array: the handler builds a sized slice so that no methods is [] and never null. */
  login: LoginOption[]
}

/**
 * Every feature name the controller reports on, from feature.All().
 *
 * The document always carries all of them, whatever the reporter put in its own
 * map — which is what lets a control distinguish false from absent rather than
 * vanishing when a reporter drops a key.
 */
export const featureNames = ['multi-swarm', 'sso', 'projects', 'audit', 'notifications'] as const
export type FeatureName = (typeof featureNames)[number]

/** The five statuses of feature.Status (D25). See LicenceBadge for what each one asks of the reader. */
export const licenceStatuses = ['valid', 'grace', 'expired', 'invalid', 'absent'] as const
export type LicenceStatus = (typeof licenceStatuses)[number]

export interface Licence {
  /** The vendor's own name for the tier: "be", "trial". Nothing here branches on it. */
  tier: string
  status: LicenceStatus
  /**
   * Null when the licence is perpetual or when there is none to expire, and
   * explicitly null rather than absent — that is what tells a badge "this does
   * not expire" apart from a document it failed to parse.
   */
  expiresAt: string | null
}

export interface CapabilityDocument {
  version: string
  /** "community" until a licence verifies, "business" after. */
  edition: string
  features: Record<FeatureName, boolean>
  /**
   * Null in a build with no licensed module linked, which is a different thing
   * from a licensed build with no licence installed: that one reports a status
   * of "absent". The badge renders nothing for the first and something for the
   * second, and that difference is the whole of the free build's identity.
   */
  licence: Licence | null
  seams: Seams
}

/** The startup `seams` log line, readable by an operator who has the token but not the logs. */
export interface Seams {
  swarms: string
  authz: string
  notify: string[]
  secrets: string
  feature: string
  extension: string[]
}

// Neither document changes without a controller restart: the login methods come
// from the authorizer registered in init(), and the capability report's *shape*
// is settled by the same seam. So both are read once and never polled — the
// 30-second default in App.tsx exists to compensate for an event stream that
// died silently, and neither of these is a thing an event can move.
const discovery = { staleTime: Infinity, refetchInterval: false as const }

/** useBootstrap reads the public login document. No credential is attached; see publicGet. */
export function useBootstrap(): UseQueryResult<BootstrapDocument> {
  return useQuery({
    queryKey: bootstrapKey,
    queryFn: () => publicGet<BootstrapDocument>(bootstrapPath),
    ...discovery,
  })
}

/**
 * useCapabilities reads what this build is and grants.
 *
 * Disabled without a credential, because the endpoint is guarded by `read`: a
 * request issued from the login screen would be a 401 that the fetch wrapper
 * answers by clearing a token nobody has, and it would ask the operator's
 * browser to make a request that could only ever fail.
 */
export function useCapabilities(): UseQueryResult<CapabilityDocument> {
  const token = useToken()
  return useQuery({
    queryKey: capabilitiesKey,
    queryFn: () => apiGet<CapabilityDocument>(capabilitiesPath),
    enabled: token !== null,
    ...discovery,
  })
}

/** useFeature reports whether this build grants one capability. Off unless the document says otherwise. */
export function useFeature(name: FeatureName): boolean {
  // Compared against true rather than coerced, because the value is undefined
  // for a document that omitted the key — a controller older than the name, or
  // ahead of it — and a control must not appear on an undefined.
  return useCapabilities().data?.features[name] === true
}
