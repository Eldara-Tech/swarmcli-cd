// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The controller's wire types, mirrored from application/{application,status,
// history}.go.
//
// # The JSON tags are the only authority
//
// Several fields carry a `yaml:"…,omitempty"` that their `json:` tag does not,
// because applications.yaml is written by a person and a status document is
// written by a program. `allow` is the clearest case: omitempty in YAML, always
// present in JSON. Reading optionality off the YAML tag would make half of this
// file wrong in the direction that produces a runtime crash rather than a
// compile error.
//
// # Optional means the key is absent, never null
//
// Go's omitempty drops the key; it does not send null. So `foo?: T` here means
// "may be missing" and nothing else, and the three fields that really can be
// null say so explicitly with `| null`. Those three are not a defensive habit —
// each is a documented state of the controller, and each is called out where it
// is declared below.
//
// web/ui/src/test/wire.test.ts checks this file against fixtures generated from
// the Go types by application/fixtures_test.go, so a field that gains or loses
// omitempty fails a build rather than a screen.

import type {
  CompatState,
  DriftDetection,
  DriftReason,
  DriftState,
  HealthState,
  ResourceKind,
  SyncAction,
  SyncState,
} from './enums'

/**
 * An RFC 3339 timestamp, as Go's time.Time marshals one.
 *
 * A string rather than a Date because that is what arrives, and parsing every
 * one at the boundary would mean a Date in every type here for the two screens
 * that format one.
 *
 * A field of this type with no `?` can still be *unset*: Go's zero time
 * marshals as "0001-01-01T00:00:00Z", which is what `loadedAt` reads on a
 * controller that has never loaded an app set. Date.parse of that is a large
 * negative number and emphatically not NaN, so the usual "is it a real date"
 * guard passes and the screen renders the year 1. Compare against
 * `zeroTimestamp` instead.
 */
export type Timestamp = string

/** The value a never-set Timestamp carries; see Timestamp. */
export const zeroTimestamp = '0001-01-01T00:00:00Z'

// ---------------------------------------------------------------------------
// Spec — what an operator declared
// ---------------------------------------------------------------------------

export interface Spec {
  name: string
  source: Source
  registryAuth?: string
  /**
   * The one application that deploys the stack the controller itself runs as,
   * which is how the controller upgrades itself. Absent on every other
   * application, and on any controller older than the release that added it.
   */
  self?: boolean
  /** Always present: `json:"allow"` carries no omitempty, though the yaml tag does. */
  allow: Allow
  destination: Destination
  syncPolicy: SyncPolicy
  driftDetection: DriftDetection
}

/** Exactly one of releaseFile and chart is set, and which one is the source type. */
export interface Source {
  repoURL: string
  /** A branch, tag or SHA as written, not the resolved commit. That is `status.sync.revision`. */
  revision: string
  releaseFile?: string
  chart?: ChartSource
}

export interface ChartSource {
  /**
   * Always present: the config loader resolves the default (the application's
   * own name) before a spec is ever served, so this holds the release name that
   * will reach the swarm — which is also the Swarm stack namespace.
   */
  release: string
  path?: string
  ref?: string
  version?: string
  values?: string[]
  repositories?: RepositorySpec[]
}

export interface RepositorySpec {
  name: string
  url: string
}

/** What this application's charts may reach outside the releases they install. */
export interface Allow {
  hostPaths?: string[]
  secrets?: string[]
  configs?: string[]
  volumes?: string[]
  networks?: string[]
}

/** Empty — the key absent — is the swarm the controller runs in. */
export interface Destination {
  swarm?: string
}

export interface SyncPolicy {
  automated: boolean
  /** A Go duration as text ("30s", "10m"), not a number of anything. */
  interval?: string
  wait?: boolean
  timeout?: string
  /** Absent is the default of 10; an explicit 0 means keep every revision. */
  historyMax?: number
  prune?: boolean
  pruneVolumes?: boolean
  pruneResources?: boolean
  pruneFirst?: boolean
}

// ---------------------------------------------------------------------------
// Status — what the controller last observed
// ---------------------------------------------------------------------------

/** One application: what was declared beside what was observed. */
export interface View {
  spec: Spec
  status: Status
}

/** The list endpoint's wrapper. Always an array — the handler builds a non-nil slice. */
export interface ApplicationList {
  applications: View[]
}

export interface Status {
  sync: Sync
  health: Health
  /** Absent for an application whose driftDetection is `manifest` — the mode that does not ask. */
  drift?: Drift
  /**
   * Absent means "not requested", not "none": the list view strips it and the
   * engine rejects a release file declaring no releases, so a populated
   * application always has at least one.
   */
  releases?: ReleaseStatus[]
  /** The last reconcile error. Not a failed sync — that is `sync.lastSync.error`. */
  error?: string
  observedAt: Timestamp
}

export interface Sync {
  state: SyncState
  /** The resolved SHA the assessment was made against, never a branch name. */
  revision?: string
  summary: SyncSummary
  lastSync?: SyncResult
}

export interface SyncSummary {
  install: number
  upgrade: number
  unchanged: number
  /**
   * Omitted rather than zero when nothing drifted, unlike its three neighbours:
   * the question is only asked under `driftDetection: live`, so absent means
   * "not asked" and 0 means "asked, none".
   */
  drifted?: number
}

export interface SyncResult {
  revision: string
  startedAt: Timestamp
  finishedAt: Timestamp
  succeeded: boolean
  error?: string
}

export interface Health {
  state: HealthState
  message?: string
  services: ServiceCounts
}

/** The "3/4" a list row shows. */
export interface ServiceCounts {
  healthy: number
  total: number
}

/** The live-drift rollup for a whole application. */
export interface Drift {
  state: DriftState
  services: number
  resources?: number
  message?: string
}

export interface ReleaseStatus {
  name: string
  chart: string
  version: string
  /** The chart engine's revision number; 0 when never installed. */
  revision: number
  action: SyncAction
  sync: SyncState
  health: Health
  services?: ServiceStatus[]
  compat?: Compat
  /** The release file's wave. Omitted when 0, which is both the default and "explicitly first". */
  wave?: number
  drift?: ReleaseDrift
}

export interface ReleaseDrift {
  state: DriftState
  services?: ServiceDrift[]
  resources?: ResourceDrift[]
  /** Why state is "unknown"; empty for every other state. */
  message?: string
}

export interface ServiceDrift {
  /** Namespace-scoped, as the swarm shows it: "<release>_<service>". */
  name: string
  reason: DriftReason
  fields?: FieldDrift[]
  /** How many differences beyond those listed; the list is capped. */
  truncated?: number
  orphaned?: boolean
  message?: string
}

export interface FieldDrift {
  field: string
  /** Rendered, never typed. An environment difference reads "set"/"absent"/"differs", never the value. */
  desired: string
  live: string
}

export interface ResourceDrift {
  kind: ResourceKind
  name: string
}

export interface Compat {
  status: CompatState
  required?: string
  engine?: string
  reason?: string
}

export interface ServiceStatus {
  name: string
  mode: string
  running: number
  desired: number
  completed?: number
  health: HealthState
  /** Empty means never updated, not finished. */
  updateState?: string
  message?: string
}

// ---------------------------------------------------------------------------
// The documents with their own endpoints
// ---------------------------------------------------------------------------

/** The controller itself, as distinct from the applications it reconciles. */
export interface ControllerStatus {
  appSet: AppSetStatus
  /** What is actually being reconciled, which is not always what the file declares. */
  applications: number
}

export interface AppSetStatus {
  /** "static", "git" or "path". Empty on a controller with no app-set source wired. */
  mode: string
  source?: string
  revision?: string
  /**
   * When the running set last loaded *successfully*, not when it was last
   * checked — and carrying no omitempty, so a controller that has never loaded
   * one reports `zeroTimestamp` rather than nothing. See Timestamp.
   */
  loadedAt: Timestamp
  error?: string
  /** The field to colour: the running set is a last-good one and a newer version is being refused. */
  stale: boolean
  orphaned?: string[]
  pruned?: string[]
  pruneHeldBy?: string[]
}

/**
 * One application's release history.
 *
 * `releases` is nullable because the Go field carries no omitempty and the type
 * is a slice: `History{}` marshals to `{"releases":null}`. The OSS reconciler
 * always builds a non-nil slice, but api.Reconciler is an interface precisely so
 * that another one can serve these endpoints, and the contract is the type.
 */
export interface History {
  releases: ReleaseHistory[] | null
}

export interface ReleaseHistory {
  name: string
  /**
   * Null — not `[]` — for a release the repository declares and the plan would
   * install, because the engine has no record of it yet. That is a real and
   * expected state, it is what B4's "declared but never deployed" row is, and
   * `.map()` on it throws.
   */
  revisions: Revision[] | null
}

export interface Revision {
  revision: number
  chart: string
  version: string
  /** The engine's derived status: the highest revision keeps its stored one, lower deployed ones read "superseded". */
  status: string
  /** RFC 3339, as the engine recorded it — a plain string, not this module's Timestamp, because the engine writes it. */
  created?: string
  /** The stamp naming what produced the revision; absent when unclaimed by anything. */
  owner?: string
}

/** The manifest change one release would undergo. */
export interface ReleaseDiff {
  release: string
  action: SyncAction
  /**
   * A line-oriented diff with `-`, `+` and space prefixes. Not a unified diff:
   * it has no hunk headers, so every diff carries the whole rendered manifest as
   * context and a renderer must fold it.
   */
  diff: string
}

export interface DiffResponse {
  /**
   * Null is the success case, and this is the field the architecture review
   * called out first. `drift.Diffs` starts from a nil slice and appends only the
   * releases a sync would touch, so a converged application sends
   * `{"planned":true,"releases":null}` — while `{"planned":false,"releases":[]}`
   * means the opposite thing, that nothing has been reconciled yet and there is
   * nothing to compare.
   */
  releases: ReleaseDiff[] | null
  planned: boolean
}

// ---------------------------------------------------------------------------
// Phase 2.0 — Swarm Telemetry & Cluster Diagnostics
// ---------------------------------------------------------------------------

export interface SwarmNode {
  id: string
  hostname: string
  role: 'manager' | 'worker'
  availability: 'active' | 'pause' | 'drain'
  status: 'ready' | 'down' | 'unknown'
  engineVersion: string
  addr: string
  leader?: boolean
  tasksRunning: number
  tasksDesired: number
}

export interface NodesResponse {
  swarm: string
  nodes: SwarmNode[]
}

export interface ServiceLogEvent {
  service: string
  taskID?: string
  nodeID?: string
  stream: 'stdout' | 'stderr'
  message: string
  timestamp: Timestamp
}

export interface RiskItem {
  id: string
  severity: 'bad' | 'warn' | 'info'
  application?: string
  title: string
  summary: string
  remedy?: string
}

export interface CheckItem {
  id: string
  name: string
  passed: boolean
  detail: string
}

export interface DiagnosticsResponse {
  score: number
  tone: 'ok' | 'warn' | 'bad'
  clearCount: number
  totalCount: number
  risks: RiskItem[]
  checks: CheckItem[]
}

