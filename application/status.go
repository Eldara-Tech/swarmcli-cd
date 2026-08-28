// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import "time"

// ControllerStatus is the controller itself as last observed, as distinct from
// the applications it reconciles.
//
// It exists because the app set became something that can fail on its own: once
// the set is pulled from git rather than mounted at deploy time, "every
// application looks fine" and "the controller has been refusing every commit
// for an hour" are both true at the same time, and nothing in the per-application
// views can say the second. This is where that is said.
//
// Applications counts what is actually being reconciled, which is not
// necessarily what the last-loaded file declares: an application the set added
// but that could not be started is in the file and not in this count.
type ControllerStatus struct {
	AppSet       AppSetStatus `json:"appSet"`
	Applications int          `json:"applications"`
}

// AppSetStatus is where the set of applications came from and how that is
// going.
type AppSetStatus struct {
	// Mode is how the set is sourced, as the deployment configured it —
	// "static", "git" or "path". It is a label passed in rather than something
	// the source infers about itself, so the bootstrap's own vocabulary is what
	// an operator sees reflected back.
	Mode string `json:"mode"`

	// Source is what the bootstrap was pointed at, for a human: the repository,
	// the revision it tracks and the file within it, or the directory and file
	// an external process keeps current. Mode alone cannot be checked against
	// anything, and "which branch is this controller following" is the first
	// question a set that does not look right raises — and the one thing nothing
	// in git can change (D-f of #47).
	Source string `json:"source,omitempty"`

	// Revision is the commit the running set was loaded from. Empty when the
	// set does not come from a repository.
	Revision string `json:"revision,omitempty"`

	// LoadedAt is when the running set last loaded successfully — not when it
	// was last checked. A load that changed nothing does not move it, so the
	// pair with Error answers "how old is what I am running".
	LoadedAt time.Time `json:"loadedAt"`

	// Error is why the last attempt failed: a load that was refused, or the
	// applications a diff could not apply. Cleared by the next attempt that
	// succeeds.
	Error string `json:"error,omitempty"`

	// Stale reports that the running set is a last-good one and a newer version
	// is being refused. It is the field a UI colours; Error is what it shows
	// beside it. An error with Stale false is one of two different problems: a
	// set that loaded but could not be fully applied, or — with Applications at
	// zero and LoadedAt unset — a controller that has never managed to load one,
	// which is the louder of the three.
	Stale bool `json:"stale"`

	// Orphaned names applications that left the set. Their loops are stopped and
	// their stacks are still deployed and no longer reconciled by anyone —
	// reported rather than removed, unless prune is enabled.
	//
	// This list is what the running loop watched leave, so a restart empties it.
	// That is a gap in the reporting and not in the cleanup: the owner stamps
	// the releases carry are the durable record, so a controller with prune
	// enabled still finds and removes an application that departed before it
	// started. With prune disabled a restart does forget, and the swarm is the
	// only place left that knows.
	Orphaned []string `json:"orphaned,omitempty"`

	// Pruned names applications whose resources this controller has deleted,
	// most recent last. Empty whenever prune is disabled, which is the default.
	//
	// It exists because a departed application otherwise leaves no trace at all
	// once prune has run: it is gone from the app set, gone from Orphaned, and
	// gone from the swarm. "Did it actually go, or did the controller never
	// notice" is then only answerable from the logs.
	//
	// Like Orphaned, in memory: a restart empties it. What it reports is this
	// process's own deletions, not an audit log.
	Pruned []string `json:"pruned,omitempty"`

	// PruneHeldBy names the applications that have not reconciled yet and are
	// therefore holding the sweep back. Empty whenever prune is disabled, and
	// on any controller whose applications have all planned at least once —
	// which after a settled startup is every controller.
	//
	// The sweep deletes what no application declares, so it cannot run while an
	// application has not said what it declares; it would read that silence as
	// a departure. Waiting is the safe half of that trade and this is the other
	// half: an operator who enabled prune and sees nothing being pruned has to
	// be able to find out which application is holding it, or the safety
	// measure is indistinguishable from a broken feature.
	PruneHeldBy []string `json:"pruneHeldBy,omitempty"`
}

// SwarmNode is one Docker Swarm engine node observed in the cluster.
type SwarmNode struct {
	ID            string `json:"id"`
	Hostname      string `json:"hostname"`
	Role          string `json:"role"`         // "manager" or "worker"
	Availability  string `json:"availability"` // "active", "pause", "drain"
	Status        string `json:"status"`       // "ready", "down", "unknown"
	EngineVersion string `json:"engineVersion"`
	Addr          string `json:"addr"`
	Leader        bool   `json:"leader,omitempty"`
	TasksRunning  int    `json:"tasksRunning"`
	TasksDesired  int    `json:"tasksDesired"`
}

// NodesResponse is the payload served by GET /api/v1/nodes.
type NodesResponse struct {
	Swarm string      `json:"swarm"`
	Nodes []SwarmNode `json:"nodes"`
}

// ServiceLogEvent is one framed log line emitted over SSE.
type ServiceLogEvent struct {
	Service   string    `json:"service"`
	TaskID    string    `json:"taskID,omitempty"`
	NodeID    string    `json:"nodeID,omitempty"`
	Stream    string    `json:"stream"` // "stdout" or "stderr"
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// RiskItem is one identified risk in cluster health diagnostics.
type RiskItem struct {
	ID string `json:"id"`
	// Severity is "bad", "warn" or "unknown".
	//
	// "unknown" is its own tier rather than a flavour of the other two: every
	// enum in enum.go has the empty string as its Unknown member, so an
	// application the controller has accepted and not yet reconciled reports
	// exactly that. Called "bad" it says a healthy young fleet is degraded;
	// called clear it says a fleet nobody has looked at is fine.
	Severity    string `json:"severity"`
	Application string `json:"application,omitempty"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Remedy      string `json:"remedy,omitempty"`
}

// CheckItem is one operational check item in cluster diagnostics.
type CheckItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// AppSetShape is which of the app set's five states a controller is in.
//
// Mirrored by appSetShape in web/ui/src/api/appset.ts, and the reason both
// exist is that three of these are *different* failures — docs/api.md is
// explicit about it — and a boolean cannot tell them apart. `Stale` alone was
// what the controller-integrity check read, so a controller that had never
// loaded a set, and one with no reconcile loop wired at all, both reported as
// operational.
type AppSetShape string

const (
	// AppSetUnwired is api.go's no-controller arm: a zero ControllerStatus, so
	// Mode is empty and every field below it is a zero value rather than an
	// observation. Checked first, which is the whole reason this is a function
	// — LoadedAt is zero here too, and would otherwise read as never-loaded.
	AppSetUnwired AppSetShape = "unwired"
	// AppSetNeverLoaded is the loudest: no set has ever loaded, so the
	// applications list is empty for a reason that has nothing to do with any
	// application in it.
	AppSetNeverLoaded AppSetShape = "never-loaded"
	// AppSetStale is a newer set being refused while the last good one runs.
	AppSetStale AppSetShape = "stale"
	// AppSetPartial is a set that loaded and could not be applied in full.
	AppSetPartial AppSetShape = "partial"
	AppSetOK      AppSetShape = "ok"
)

// Shape classifies this controller status. See AppSetShape.
func (s ControllerStatus) Shape() AppSetShape {
	switch {
	case s.AppSet.Mode == "":
		return AppSetUnwired
	// Both halves, as docs/api.md states it: a set that loaded and legitimately
	// declares nothing has a LoadedAt, and a controller mid-first-load has
	// neither — the difference between "nothing to do" and "nothing works".
	case s.AppSet.LoadedAt.IsZero() && s.Applications == 0:
		return AppSetNeverLoaded
	case s.AppSet.Stale:
		return AppSetStale
	case s.AppSet.Error != "":
		return AppSetPartial
	default:
		return AppSetOK
	}
}

// DiagnosticsResponse is the payload served by GET /api/v1/diagnostics.
type DiagnosticsResponse struct {
	Score int `json:"score"`
	// Tone is "ok", "warn" or "bad", taken from the worst risk present rather
	// than from a threshold on Score. A threshold said "Nominal" beside a red
	// risk list as soon as the arithmetic landed on 90.
	Tone       string      `json:"tone"`
	ClearCount int         `json:"clearCount"`
	TotalCount int         `json:"totalCount"`
	Risks      []RiskItem  `json:"risks"`
	Checks     []CheckItem `json:"checks"`
}
