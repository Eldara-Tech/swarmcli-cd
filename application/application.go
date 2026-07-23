// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package application defines the Application spec, its observed status, and
// the JSON both serialise to. It is the wire contract shared by the config
// loader, the reconciler, the HTTP API and the CLI.
//
// Per D3 the API is designed UI-first, and these types are that design: a
// shape that is awkward to render is a bug here rather than in a handler.
//
// The package is deliberately top-level rather than internal. Per D6 a private
// companion repository imports this one, and Go's internal rule is per-module,
// so anything the companion touches cannot live under internal/ — the same
// reason Eldara-Tech/swarmcli has no internal/ directory at all.
//
// It depends on nothing but the standard library and a YAML decoder: not on
// Docker, not on the reconciler, not on the CE charts package. The types that
// mirror charts concepts are re-expressed here so that the API's shape does
// not move when CE's does.
package application

import "time"

// Spec is what an operator declares in applications.yaml. It is read-only in
// Phase 1: the file is the only source of truth and the API serves it.
type Spec struct {
	Name   string `json:"name" yaml:"name"`
	Source Source `json:"source" yaml:"source"`

	// RegistryAuth names a Docker secret holding a docker config.json
	// ({"auths":{...}}). The controller uses only this application's secret to
	// authenticate the pulls its images need, so one application cannot pull
	// another's private images even though both credentials are mounted in the
	// same controller. Empty means the application's images are public.
	//
	// It is a secret name, not a path: the controller reads it from the default
	// mount /run/secrets/<name>, so the secret must be mounted there — the
	// short form `secrets: [<name>]` in stack.yml does exactly that.
	RegistryAuth string `json:"registryAuth,omitempty" yaml:"registryAuth,omitempty"`

	Destination    Destination    `json:"destination" yaml:"destination"`
	SyncPolicy     SyncPolicy     `json:"syncPolicy" yaml:"syncPolicy"`
	DriftDetection DriftDetection `json:"driftDetection" yaml:"driftDetection"`
}

// Source locates the desired state in git. Exactly one of ReleaseFile and
// Chart is set, and which one is present is the source type — a separate
// discriminator field would be a second thing to keep consistent with it.
type Source struct {
	RepoURL  string `json:"repoURL" yaml:"repoURL"`
	Revision string `json:"revision" yaml:"revision"` // branch, tag or SHA, as written

	ReleaseFile string       `json:"releaseFile,omitempty" yaml:"releaseFile,omitempty"` // path within the repo
	Chart       *ChartSource `json:"chart,omitempty" yaml:"chart,omitempty"`
}

// ChartSource is the "one application, one chart" case that does not deserve a
// release file in the repository. Its rules are the ones charts itself
// enforces: Version is required for a repository reference and forbidden for a
// path, because a floating pin would silently upgrade production on the next
// reconcile.
type ChartSource struct {
	Release      string           `json:"release" yaml:"release"`
	Path         string           `json:"path,omitempty" yaml:"path,omitempty"` // chart directory within the repo
	Ref          string           `json:"ref,omitempty" yaml:"ref,omitempty"`   // repo/chart
	Version      string           `json:"version,omitempty" yaml:"version,omitempty"`
	Values       []string         `json:"values,omitempty" yaml:"values,omitempty"` // paths within the repo
	Repositories []RepositorySpec `json:"repositories,omitempty" yaml:"repositories,omitempty"`
}

// RepositorySpec names a chart repository a ChartSource resolves Ref against.
type RepositorySpec struct {
	Name string `json:"name" yaml:"name"`
	URL  string `json:"url" yaml:"url"`
}

// Destination names the swarm, resolved through the SwarmRegistry seam. Empty
// means the local swarm, which is the only one Phase 1 can resolve.
type Destination struct {
	Swarm string `json:"swarm,omitempty" yaml:"swarm,omitempty"`
}

// SyncPolicy governs when and how a plan is applied. Wait, Timeout and
// HistoryMax map onto charts.InstallOptions; Interval overrides the
// controller-wide poll interval for one application.
type SyncPolicy struct {
	Automated  bool     `json:"automated" yaml:"automated"`
	Interval   Duration `json:"interval,omitempty" yaml:"interval,omitempty"`
	Wait       bool     `json:"wait,omitempty" yaml:"wait,omitempty"`
	Timeout    Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	HistoryMax int      `json:"historyMax,omitempty" yaml:"historyMax,omitempty"`
}

// View is what every API read returns: the declared spec beside what the
// controller last observed. Keeping them separate is what lets applications
// become writable later without moving anything.
type View struct {
	Spec   Spec   `json:"spec"`
	Status Status `json:"status"`
}

// Status is one application as last observed. The list view renders it without
// Releases and the detail view renders it with. Releases is never legitimately
// empty once populated — charts rejects a release file declaring no releases —
// so its absence unambiguously means "not requested" rather than "none".
type Status struct {
	Sync       Sync            `json:"sync"`
	Health     Health          `json:"health"`
	Releases   []ReleaseStatus `json:"releases,omitempty"`
	Error      string          `json:"error,omitempty"` // last reconcile error; not a failed sync
	ObservedAt time.Time       `json:"observedAt"`
}

// Sync answers "does the swarm match git".
//
// Revision is the commit the assessment was made against; LastSync.Revision is
// what was actually deployed. When the two differ there is a newer commit that
// has not been applied, which is a different condition from being OutOfSync,
// and a UI shows both.
type Sync struct {
	State    SyncState   `json:"state"`
	Revision string      `json:"revision,omitempty"` // resolved SHA, never a branch name
	Summary  SyncSummary `json:"summary"`
	LastSync *SyncResult `json:"lastSync,omitempty"`
}

// SyncSummary is the plan that made the state OutOfSync, counted by action.
type SyncSummary struct {
	Install   int `json:"install"`
	Upgrade   int `json:"upgrade"`
	Unchanged int `json:"unchanged"`
}

// SyncResult records the outcome of the last sync that was actually attempted.
type SyncResult struct {
	Revision   string    `json:"revision"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Succeeded  bool      `json:"succeeded"`
	Error      string    `json:"error,omitempty"`
}

// Health answers "is what is running actually working". It is a separate axis
// from Sync — a stack can be synced and degraded at once, and collapsing the
// two loses the distinction that makes the view useful. Services carries the
// counts a list row renders without descending into Releases.
type Health struct {
	State    HealthState   `json:"state"`
	Message  string        `json:"message,omitempty"`
	Services ServiceCounts `json:"services"`
}

// ServiceCounts is the "3/4" a list row shows.
type ServiceCounts struct {
	Healthy int `json:"healthy"`
	Total   int `json:"total"`
}

// ReleaseStatus is one release of one application.
type ReleaseStatus struct {
	Name     string          `json:"name"`
	Chart    string          `json:"chart"`
	Version  string          `json:"version"`
	Revision int             `json:"revision"` // charts revision number; 0 when never installed
	Action   SyncAction      `json:"action"`
	Sync     SyncState       `json:"sync"`
	Health   Health          `json:"health"`
	Services []ServiceStatus `json:"services,omitempty"`
	Compat   *Compat         `json:"compat,omitempty"`
}

// Compat is a chart's declared swarmcliVersion verdict. Planning records it
// and never enforces it, so an unattended controller has to surface it or the
// operator never learns the chart wanted a newer engine.
type Compat struct {
	Status   CompatState `json:"status"`
	Required string      `json:"required,omitempty"`
	Engine   string      `json:"engine,omitempty"`
	Reason   string      `json:"reason,omitempty"`
}

// ReleaseDiff is the manifest change one release would undergo.
//
// It is deliberately not part of ReleaseStatus: a diff carries whole
// manifests, and a list view rendering twenty applications must not drag them
// along. It is served by its own endpoint, for one application at a time.
type ReleaseDiff struct {
	Release string     `json:"release"`
	Action  SyncAction `json:"action"`
	Diff    string     `json:"diff"`
}

// ServiceStatus is one Swarm service under a release.
type ServiceStatus struct {
	Name        string      `json:"name"`
	Mode        string      `json:"mode"`
	Running     int         `json:"running"`
	Desired     int         `json:"desired"`
	Completed   int         `json:"completed,omitempty"`
	Health      HealthState `json:"health"`
	UpdateState string      `json:"updateState,omitempty"` // "" means never updated, not finished
	Message     string      `json:"message,omitempty"`
}
