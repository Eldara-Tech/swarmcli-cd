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

import (
	"regexp"
	"time"
)

// Spec is what an operator declares in applications.yaml. It is read-only over
// the API: the file is the only source of truth, whether it is mounted at
// deploy time or committed to git, and the API serves it rather than owning it.
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

// repoNameRE is the charset a chart repository name is held to: no separator,
// and a leading dot — hence ".." — rejected.
var repoNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidRepositoryName reports whether a chart repository may be called name.
//
// Declaring a name is not all it does. The chart engine builds its cache file
// by concatenating the name into "index-<name>.yaml" and joining that onto its
// store directory, so the name is a path component before it is an identifier.
// The concatenation is what makes it dangerous: "index-.." is an ordinary
// segment, so Join's Clean does not neutralise a traversal, it only costs one
// extra "../" — and a name that escapes writes attacker-chosen YAML wherever a
// process holding the docker socket can reach, up to and including the app set
// naming every repository the controller then deploys from.
//
// It lives here, in the wire contract, rather than in the config loader,
// because a name reaches the engine by two routes and only one of them is the
// operator's file: a release file committed to a tenant's repository carries
// repositories of its own, and package source checks those on the way past.
// Two copies of this charset that drifted apart would leave one route open,
// which is the whole of #100.
func ValidRepositoryName(name string) bool { return repoNameRE.MatchString(name) }

// Destination names the swarm, resolved through the SwarmRegistry seam. Empty
// means the local swarm, which is the only one Phase 1 can resolve.
type Destination struct {
	Swarm string `json:"swarm,omitempty" yaml:"swarm,omitempty"`
}

// SyncPolicy governs when and how a plan is applied. Wait, Timeout and
// HistoryMax map onto charts.InstallOptions; Interval overrides the
// controller-wide poll interval for one application.
type SyncPolicy struct {
	Automated bool     `json:"automated" yaml:"automated"`
	Interval  Duration `json:"interval,omitempty" yaml:"interval,omitempty"`
	Wait      bool     `json:"wait,omitempty" yaml:"wait,omitempty"`
	Timeout   Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// HistoryMax bounds a release's stored revision history: the newest N are
	// kept and the rest deleted after a deploy that succeeded. Absent means
	// DefaultHistoryMax; an explicit 0 means keep every revision, which is the
	// chart engine's own reading of the number.
	//
	// A pointer for exactly that reason. "Not set" and "set to keep all" have to
	// stay different answers, and they were the same one — zero — while the
	// default was to keep all, which is how a controller ends up writing one
	// Docker config per deploy into a manager's raft log for ever. See Retention.
	HistoryMax *int `json:"historyMax,omitempty" yaml:"historyMax,omitempty"`

	// Prune deletes the resources of a release this application used to declare
	// and no longer does. Off by default: reporting an orphan is safe and
	// deleting one is not, so it is a deliberate choice per application.
	//
	// It governs only this application's own releases — those carrying its
	// owner stamp. A release another application or the command line installed
	// is unmanaged here and is never touched, whatever this says.
	//
	// Not to be confused with HistoryMax, which prunes an individual release's
	// revision history rather than the release itself. Two senses of the word,
	// one of which is the chart engine's; see the prune package.
	Prune bool `json:"prune,omitempty" yaml:"prune,omitempty"`

	// PruneVolumes extends Prune to the named volumes of what it deletes, and
	// means nothing without it — a config declaring one and not the other is
	// refused rather than half-obeyed.
	//
	// Separate from Prune because it is the one irreversible part. Everything
	// else prune removes can be recreated from git on the next reconcile; the
	// data in a volume cannot be recreated from anything.
	PruneVolumes bool `json:"pruneVolumes,omitempty" yaml:"pruneVolumes,omitempty"`

	// PruneResources deletes a service, network, config or secret that this
	// application's own chart used to declare and no longer does. Off by
	// default, for the same reason as Prune.
	//
	// It is a sibling of Prune rather than an extension of it, and deliberately
	// does not require it: Prune decides what happens to a whole release the
	// application stopped declaring, this decides what happens inside a release
	// it still declares, and an operator may reasonably want the second without
	// the first. Applying deletes nothing, so without this a resource dropped
	// from a template stays until somebody removes it by hand.
	//
	// One consent covers all four kinds because it is one statement: this
	// chart is authoritative about what exists in its own release. Configs and
	// secrets are the ones that accumulate fastest — they are immutable, so a
	// chart that hashes content into the name as the applier tells it to
	// strands the previous copy on every value change, not only when a
	// declaration is removed.
	//
	// A resource is deleted only when the swarm, git and this controller's own
	// records all agree: it carries the release's stack namespace label, the
	// rendered manifest does not declare it, and a revision this controller
	// stamped for this application did. The namespace label alone is not
	// evidence — anything can carry it — which is why the third clause exists.
	// See the prune package.
	//
	// Volumes are never included, and neither are the external networks
	// swarmcli auto-created for a release: both are reported and left in place.
	PruneResources bool `json:"pruneResources,omitempty" yaml:"pruneResources,omitempty"`

	// PruneFirst deletes before installing, instead of after.
	//
	// The default order applies and then prunes, so a failed apply leaves the
	// old release running rather than nothing at all. The cost is that a
	// renamed release briefly coexists with the name it replaced: the new name
	// is an install and the old one an orphan, and between the two steps both
	// are deployed.
	//
	// For a workload where two instances running at once is worse than none —
	// a blockchain validator that would double-sign and be slashed, a job
	// runner that must not process a queue twice — that trade is the wrong way
	// round. This inverts it: the departing release is deleted before its
	// replacement is installed, so they never overlap, and a failed apply
	// leaves a gap instead.
	//
	// It bounds the overlap this controller creates deliberately; it is not a
	// distributed lock. Nothing here can prevent two instances during a network
	// partition or a node recovering with stale state, so a workload that
	// cannot tolerate that at all needs an external guard — a remote signer
	// with an anti-slashing record, or a lease.
	//
	// Means nothing without Prune or PruneResources — it orders whichever of
	// them is on — and is refused rather than ignored.
	PruneFirst bool `json:"pruneFirst,omitempty" yaml:"pruneFirst,omitempty"`
}

// DefaultHistoryMax is how many revisions of each release are kept when an
// application does not say.
//
// Every revision is one Docker Config in the swarm's raft log carrying the whole
// rendered manifest, written by every deploy that changes anything. Left
// unbounded that is one of the two ways this controller can fill the disk of the
// manager it runs on — a chart whose render is not reproducible being the other,
// and the two compound: at the three-minute default such a chart writes some 480
// of them per release per day. Ten is Helm's own --history-max default and is far
// more than is ever read back; rollback context is the last few revisions, and
// the record of what was deployed when is git's.
const DefaultHistoryMax = 10

// Retention is how many revisions of each release to keep: what HistoryMax says,
// or DefaultHistoryMax when it says nothing. Zero — which only an explicit
// `historyMax: 0` produces — keeps every revision, as the chart engine reads it.
//
// It lives on the type rather than in the config loader's defaults so that every
// way a spec is built gets the same answer, including the ones that never pass
// through a file.
func (p SyncPolicy) Retention() int {
	if p.HistoryMax == nil {
		return DefaultHistoryMax
	}
	return *p.HistoryMax
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
	Sync   Sync   `json:"sync"`
	Health Health `json:"health"`

	// Drift is the live-drift rollup, and is nil for an application whose
	// driftDetection is manifest — the mode that does not ask the question. Nil
	// rather than an Unknown state so that a manifest-mode payload is exactly
	// what it was before this axis existed, and so that "not asked" and "asked,
	// could not tell" stay distinguishable.
	Drift *Drift `json:"drift,omitempty"`

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
//
// Install, Upgrade and Unchanged describe the plan; Drifted describes the
// verdict on top of it, and the two are deliberately not exclusive. A release
// the manifest leaves alone but whose running services were changed by hand is
// both Unchanged and Drifted — the plan really would do nothing to it, and it
// really does not match git.
type SyncSummary struct {
	Install   int `json:"install"`
	Upgrade   int `json:"upgrade"`
	Unchanged int `json:"unchanged"`
	// Drifted counts releases whose live services differ from the manifest,
	// under driftDetection: live. Omitted rather than zero when there is none,
	// unlike its three neighbours: a plan always has counts, whereas this
	// question is only asked in one mode, and omitting it keeps a manifest-mode
	// payload exactly what it was before this axis existed.
	Drifted int `json:"drifted,omitempty"`
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

	// Drift is what the live comparison found for this release, nil when it was
	// not made: manifest mode, a release the plan would install or upgrade
	// (there is nothing settled to compare against), or a backend that cannot
	// read service specs.
	Drift *ReleaseDrift `json:"drift,omitempty"`
}

// Drift is the live-drift rollup for a whole application.
//
// It is a separate axis from Sync in the same way Health is: Sync.State says
// the application does not match git, and this says the reason is that the
// swarm moved rather than that git did. Those need different actions from an
// operator — one is a commit to review, the other is a change nobody recorded.
type Drift struct {
	State DriftState `json:"state"`
	// Services counts the services that differ, across every release. It is the
	// number a list row shows without descending into Releases.
	Services int `json:"services"`
	// Resources counts the networks, configs and secrets a sync will delete,
	// across every release. Separate from Services because they are different
	// findings: a service here differs from what the repository declares,
	// whereas a resource here is one the repository has stopped declaring at
	// all.
	Resources int `json:"resources,omitempty"`
	// Message names the worst release, or says why the comparison could not be
	// made when State is Unknown.
	Message string `json:"message,omitempty"`
}

// ReleaseDrift is what the live comparison found for one release.
type ReleaseDrift struct {
	State    DriftState     `json:"state"`
	Services []ServiceDrift `json:"services,omitempty"`
	// Resources are the networks, configs and secrets a sync will delete:
	// carrying the release's namespace label, absent from the render, and
	// declared by a revision this controller stamped for this application.
	//
	// Every entry is an orphan by construction, which is why there is no
	// Orphaned field to set. Unlike a service, none of these kinds is compared
	// field by field — Swarm cannot update a network in place, and configs and
	// secrets are immutable — so a difference in one is already a manifest-level
	// difference and there is nothing else a resource entry could mean.
	Resources []ResourceDrift `json:"resources,omitempty"`
	// Message is why State is Unknown. A release whose live state could not be
	// read is not converged, so this is the only record that the question was
	// asked and went unanswered.
	Message string `json:"message,omitempty"`
}

// ResourceDrift is one network, config or secret a sync will delete.
//
// Name is namespace-scoped, as it is on the swarm — the name docker network ls
// or docker config ls shows, and what an operator would type to remove it by
// hand. Kind is needed because the three share one namespace of names and a
// name alone does not say what to look at.
type ResourceDrift struct {
	Kind ResourceKind `json:"kind"`
	Name string       `json:"name"`
}

// ResourceKind names the kind of a ResourceDrift entry.
type ResourceKind string

const (
	ResourceNetwork ResourceKind = "network"
	ResourceConfig  ResourceKind = "config"
	ResourceSecret  ResourceKind = "secret"
)

// ServiceDrift is one service that does not match.
//
// Name is namespace-scoped — "<release>_<service>", what `docker service ls`
// shows and what an operator would type. Descoping would be a guess for a
// service whose own name contains the separator, and there is no unscoped name
// at all for one the manifest does not declare.
type ServiceDrift struct {
	Name   string       `json:"name"`
	Reason DriftReason  `json:"reason"`
	Fields []FieldDrift `json:"fields,omitempty"`
	// Truncated counts the differences beyond those listed. A service whose
	// every field was rewritten would otherwise put an unbounded list into a
	// status payload served on every poll.
	Truncated int `json:"truncated,omitempty"`
	// Orphaned marks an Unexpected service this application provably installed
	// and no longer declares: a revision carrying its owner stamp declared it,
	// which the stack's namespace label alone could never establish. Read it as
	// "this goes on the next sync".
	//
	// Only ever set when SyncPolicy.PruneResources is on, because that is the
	// only case where the proof is worth computing. An unexpected service on an
	// application that has not enabled it carries no marker and is not a
	// candidate for anything — the same distinction charts.Plan draws between
	// an orphaned release and an unmanaged one.
	Orphaned bool `json:"orphaned,omitempty"`
	// Message is the daemon's own account of why a RolledBack service was
	// reverted, verbatim from the service's UpdateStatus ("update paused due to
	// failure or early termination of task …").
	//
	// Passed through rather than reworded: it names the task that failed, which
	// is the thread an operator pulls next, and this controller did not observe
	// the failure and has nothing to add to it. Empty for every other reason.
	Message string `json:"message,omitempty"`
}

// FieldDrift is one field that differs, rendered rather than typed: this
// package deliberately depends on nothing but the standard library, so a
// swarm.ServiceSpec value cannot appear here.
//
// Environment values are never rendered. What is running is whatever an
// operator set out of band, this is served to anyone with read scope, and an
// environment variable is exactly where a credential would be — so an env
// difference reports "set", "absent" or "differs" and never the value itself.
type FieldDrift struct {
	Field   string `json:"field"`
	Desired string `json:"desired"`
	Live    string `json:"live"`
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
