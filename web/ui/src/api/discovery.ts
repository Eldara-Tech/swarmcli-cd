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

import { useCookieSessionRefused, useToken } from '../auth/useToken'
import { apiGet, publicGet } from './client'

/** The path the login screen reads before anyone has a credential. */
export const bootstrapPath = '/ui/bootstrap.json'

/**
 * Where signing out of a cookie session goes.
 *
 * Written here rather than discovered, unlike a login method's `start`. Which
 * methods exist is genuinely dynamic — it depends on which authorizer is
 * registered — but this path is not: docs/extensibility.md § Reserved paths
 * fixes the spelling and obliges any companion whose Authenticate honours a
 * cookie to serve it, exactly as it fixes /auth/login and /auth/callback for the
 * other end of the same flow. Discovering a constant would have cost a seam
 * signature frozen for ever.
 *
 * It is only ever navigated to when the controller has reported a session, so a
 * build that serves no such route is never sent here.
 */
export const signOutPath = '/auth/logout'

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

/** Who the controller says this tab already is; see BootstrapDocument.session. */
export interface Session {
  name: string
}

export interface BootstrapDocument {
  /** Always an array: the handler builds a sized slice so that no methods is [] and never null. */
  login: LoginOption[]
  /**
   * Present only when the request that fetched this document already
   * authenticated, which for a browser means it carried a companion's session
   * cookie — the credential `HttpOnly` keeps script from ever seeing.
   *
   * That is the whole reason it is on the wire. A token is the tab's own and
   * auth/session.ts knows about it; a cookie is not, so the controller is the
   * only side that can see both and it says which one arrived. Absent in every
   * build whose only credential is the admin token, because publicGet attaches
   * nothing — see api/discovery.go.
   */
  session?: Session
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

/**
 * Every capability name the controller reports on, from api.Capabilities().
 *
 * Deliberately not a FeatureName. A feature is what a *licence* grants; a
 * capability is what this build's reconciler is wired to answer, read off the
 * same type assertions the handlers make. Merging them would let a licensed
 * reporter turn an endpoint on, and would make the licence document answer a
 * question about wiring.
 *
 * Carried on the same terms as the feature map: the document always has all of
 * them, so a control can tell false from absent rather than vanishing when a
 * key goes missing.
 */
export const capabilityNames = ['logs', 'nodes'] as const
export type CapabilityName = (typeof capabilityNames)[number]

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
  /**
   * When the licence actually stops granting features, or null when nothing is
   * running out. The other end of the window `expiresAt` opens: a status of
   * "grace" covers two windows of different lengths, so this is the only way a
   * badge can say whether that means one more day or twenty-six without doing
   * arithmetic on a period it would have to guess.
   *
   * Optional as well as nullable, which `expiresAt` is not: a controller older
   * than this bundle sends no key at all, and in dev the bundle is served from
   * Vite against whatever controller is running.
   */
  featuresOffAt?: string | null
  /**
   * What the licence *issuer* last said about this deployment's size, or null
   * when it has said nothing. Unsigned and advisory — see feature.Allowance —
   * so it is rendered and nothing else. Optional for `featuresOffAt`'s reason.
   */
  allowance?: Allowance | null
}

/**
 * The issuer's advisory report about the node allowance.
 *
 * Nothing may branch on this beyond rendering it. It is what a server said
 * rather than something the controller verified against a compiled-in key, and
 * a UI that hid a control on it would be a UI anyone able to answer a request
 * could reconfigure. What it is for is the warning nobody gets today: the free
 * tier's cap is judged at the issuer, so the first enforced sign of being over
 * it is a term that quietly stops being renewed.
 */
export interface Allowance {
  /** The issuer's verdict, and the only question to ask of this block. */
  overLimit: boolean
  /**
   * The count the issuer last recorded, and the allowance it compared against.
   * Zero means the issuer said nothing — never a swarm with no nodes, and never
   * an allowance of none — so a surface holding a zero says less.
   */
  nodes: number
  maxNodes: number
  /** When the issuer stops rolling the term forward, or null when there is no date to name. */
  termEndsAt: string | null
}

export interface CapabilityDocument {
  version: string
  /** "community" until a licence verifies, "business" after. */
  edition: string
  features: Record<FeatureName, boolean>
  /**
   * What this build's reconciler is wired to answer, which is a different
   * question from what its licence grants. Read-scoped like `features`: it is
   * what the UI decides what to draw from, and a control present for one
   * subject and absent for another is the dead control #178 forbids.
   */
  capabilities: Record<CapabilityName, boolean>
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
 * useSession reports who this tab is signed in as when it is not the tab that
 * knows — undefined for a token, and for the free build always.
 *
 * A named reader rather than callers reaching into useBootstrap, because that
 * hook answers "how does one sign in here" and this answers "am I signed in":
 * the two travel together because a login screen needs both in one round trip,
 * and they are not the same question.
 */
export function useSession(): Session | undefined {
  return useBootstrap().data?.session
}

/**
 * useSignedIn reports whether this tab holds a credential the controller will
 * accept, of either kind.
 *
 * Both kinds, which is the point: everything guarded — the capability document
 * below, and the route table in App — was gated on the token alone, so a browser
 * that signed in through an identity provider had a working session and a UI
 * that behaved as though it had none.
 *
 * A refused cookie counts as no credential. auth/session.ts cannot drop the
 * cookie itself, so a 401 records the refusal instead, and this is where that
 * record is honoured.
 */
export function useSignedIn(): boolean {
  // All three read unconditionally: the || below short-circuits, and a hook
  // behind a short circuit is a hook this component calls a different number of
  // times on different renders.
  const token = useToken()
  const refused = useCookieSessionRefused()
  const session = useSession()
  return token !== null || (!refused && session !== undefined)
}

/**
 * useCapabilities reads what this build is and grants.
 *
 * Disabled without a credential, because the endpoint is guarded by `read`: a
 * request issued from the login screen would be a 401 that the fetch wrapper
 * answers by clearing a token nobody has, and it would ask the operator's
 * browser to make a request that could only ever fail.
 *
 * "Without a credential" is useSignedIn's question and not useToken's, and the
 * difference is the whole licensed UI: gated on the token, a browser signed in
 * by cookie never read this document, so the badge, the Projects item and the
 * swarm column stayed hidden in exactly the build that grants them.
 */
export function useCapabilities(): UseQueryResult<CapabilityDocument> {
  const signedIn = useSignedIn()
  return useQuery({
    queryKey: capabilitiesKey,
    queryFn: () => apiGet<CapabilityDocument>(capabilitiesPath),
    enabled: signedIn,
    ...discovery,
  })
}

/** useFeature reports whether this build's licence grants one feature. Off unless the document says otherwise. */
export function useFeature(name: FeatureName): boolean {
  // Compared against true rather than coerced, because the value is undefined
  // for a document that omitted the key — a controller older than the name, or
  // ahead of it — and a control must not appear on an undefined.
  return useCapabilities().data?.features[name] === true
}

/**
 * useCapability reports whether this build is wired to answer one thing.
 *
 * Off while the document is in flight and off when it failed, the same bargain
 * useFeature makes: a control that flickered in and then out on every load is
 * worse than one that appears a moment late, and a control drawn on a document
 * that never arrived is the advertisement of a capability this exists to stop.
 */
export function useCapability(name: CapabilityName): boolean {
  return useCapabilities().data?.capabilities[name] === true
}
