// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

// Capability names one question this build's reconciler can answer.
//
// It is deliberately not a feature.Name, and the two must not be merged. A
// feature is what a *licence* grants, reported by a Reporter the licensed
// module supplies; a capability is what this build's reconciler is wired to do,
// which is a fact about the process rather than about an entitlement. D12 names
// four licensed capabilities across five flags and neither of these is among
// them, so putting them in feature.All() would have made the licence document
// answer a question about wiring.
//
// It would also have had nowhere honest to answer from. The reconciler that
// serves the node roster is the Apache-2.0 one, so the *community* reporter
// would have had to report a fact it has no view of — and the moment a build
// wired the seam differently, the flag and the endpoint would disagree with
// nothing to notice it.
//
// A string type rather than an enumeration, for the reason authz.Action and
// feature.Name give: the list grows, and a client written against it must keep
// working when it does.
type Capability string

const (
	// CapabilityLogs is tailing a service's container output —
	// GET /api/v1/applications/{app}/services/{svc}/logs, served by LogStreamer.
	CapabilityLogs Capability = "logs"
	// CapabilityNodes is describing the swarm's nodes — GET /api/v1/nodes,
	// served by NodeLister.
	CapabilityNodes Capability = "nodes"
)

// Capabilities names every capability the document reports, in the order it
// should list them.
//
// It exists for the reason feature.All does: the key set of the document is
// decided here rather than by whatever computed the values. A UI hiding a
// control on capabilities["logs"] has to tell false from absent, and a document
// whose keys moved would make a dropped key read exactly like a capability that
// is off — the control would vanish rather than stay hidden for a stated
// reason.
func Capabilities() []Capability { return []Capability{CapabilityLogs, CapabilityNodes} }

// capabilities reports what this build's reconciler can answer.
//
// Read off the same type assertions the handlers make, rather than from a
// configured list, so the document cannot drift from the endpoints: there is no
// second place to update, and a reconciler that gains or loses a seam changes
// both at once.
//
// # What it does not promise
//
// That the assertion succeeds is a compile-time fact about the reconciler, and
// which backend serves a destination is settled per request. So a reconciler
// that implements NodeLister may still meet a backend with no roster and answer
// application.ErrUnsupported — reported as 501 exactly as an unwired seam is.
// This document therefore says what the build is *wired* for, and the endpoint
// stays the authority on any given request. Probing instead would mean a daemon
// round trip on a document the shell reads on every load, to pre-empt a state
// the endpoint already reports correctly.
func (s *Server) capabilityReport() map[Capability]bool {
	_, logs := s.rec.(LogStreamer)
	_, nodes := s.rec.(NodeLister)

	out := make(map[Capability]bool, len(Capabilities()))
	for _, name := range Capabilities() {
		switch name {
		case CapabilityLogs:
			out[name] = logs
		case CapabilityNodes:
			out[name] = nodes
		}
	}
	return out
}
