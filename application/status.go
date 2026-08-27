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
	ID          string `json:"id"`
	Severity    string `json:"severity"` // "bad", "warn", "info"
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

// DiagnosticsResponse is the payload served by GET /api/v1/diagnostics.
type DiagnosticsResponse struct {
	Score      int         `json:"score"`
	Tone       string      `json:"tone"` // "ok", "warn", "bad"
	ClearCount int         `json:"clearCount"`
	TotalCount int         `json:"totalCount"`
	Risks      []RiskItem  `json:"risks"`
	Checks     []CheckItem `json:"checks"`
}

