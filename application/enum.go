// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import "encoding/json"

// The enums below all behave the same way on the wire: they marshal as their
// lowercase name, and they decode a name they do not recognise to their
// Unknown member rather than failing. A newer controller reporting a state an
// older client has never heard of leaves that client showing "unknown", not
// erroring out — the same choice charts.CompatStatus makes.
//
// Each Unknown member is the empty string, so a zero-valued struct is already
// Unknown and there is exactly one internal representation of the state. The
// name "unknown" appears only on the wire.
//
// Leniency here does not make applications.yaml lenient. Spec fields are
// validated on load and an unrecognised value is rejected there, so a typo
// still fails loudly at startup instead of quietly disabling something.

const unknownName = "unknown"

// marshalEnum renders v, substituting name for the empty (Unknown) member.
func marshalEnum[T ~string](v T, name string) ([]byte, error) {
	if v == "" {
		return json.Marshal(name)
	}
	return json.Marshal(string(v))
}

// unmarshalEnum decodes data into whichever member it names, or into the zero
// member when it names none. A value that is not a JSON string is an error:
// not recognising a name is expected, being sent a number is not.
func unmarshalEnum[T ~string](data []byte, members ...T) (T, error) {
	var zero T
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return zero, err
	}
	for _, m := range members {
		if string(m) == s {
			return m, nil
		}
	}
	return zero, nil
}

// SyncState is whether the swarm matches git.
type SyncState string

const (
	SyncUnknown   SyncState = ""
	SyncSynced    SyncState = "synced"
	SyncOutOfSync SyncState = "out-of-sync"
)

// MarshalJSON implements json.Marshaler.
func (s SyncState) MarshalJSON() ([]byte, error) { return marshalEnum(s, unknownName) }

// UnmarshalJSON implements json.Unmarshaler.
func (s *SyncState) UnmarshalJSON(data []byte) error {
	v, err := unmarshalEnum(data, SyncSynced, SyncOutOfSync)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// HealthState is whether what is running is working. Missing — declared but
// not present — is deliberately distinct from Degraded, which is present and
// unhealthy: a UI needs to tell those apart and so does an operator.
type HealthState string

const (
	HealthUnknown     HealthState = ""
	HealthHealthy     HealthState = "healthy"
	HealthProgressing HealthState = "progressing"
	HealthDegraded    HealthState = "degraded"
	HealthMissing     HealthState = "missing"
)

// MarshalJSON implements json.Marshaler.
func (h HealthState) MarshalJSON() ([]byte, error) { return marshalEnum(h, unknownName) }

// UnmarshalJSON implements json.Unmarshaler.
func (h *HealthState) UnmarshalJSON(data []byte) error {
	v, err := unmarshalEnum(data, HealthHealthy, HealthProgressing, HealthDegraded, HealthMissing)
	if err != nil {
		return err
	}
	*h = v
	return nil
}

// SyncAction is what a sync would do to one release. The names mirror the
// chart engine's own vocabulary value-for-value; diverging from it would mean
// translating in both directions for no gain.
type SyncAction string

const (
	ActionUnknown   SyncAction = ""
	ActionUnchanged SyncAction = "unchanged"
	ActionInstall   SyncAction = "install"
	ActionUpgrade   SyncAction = "upgrade"
)

// MarshalJSON implements json.Marshaler.
func (a SyncAction) MarshalJSON() ([]byte, error) { return marshalEnum(a, unknownName) }

// UnmarshalJSON implements json.Unmarshaler.
func (a *SyncAction) UnmarshalJSON(data []byte) error {
	v, err := unmarshalEnum(data, ActionUnchanged, ActionInstall, ActionUpgrade)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// CompatState is a chart's swarmcliVersion verdict against the engine this
// controller embeds.
type CompatState string

const (
	CompatUnknown      CompatState = ""
	CompatOK           CompatState = "ok"
	CompatIncompatible CompatState = "incompatible"
)

// MarshalJSON implements json.Marshaler.
func (c CompatState) MarshalJSON() ([]byte, error) { return marshalEnum(c, unknownName) }

// UnmarshalJSON implements json.Unmarshaler.
func (c *CompatState) UnmarshalJSON(data []byte) error {
	v, err := unmarshalEnum(data, CompatOK, CompatIncompatible)
	if err != nil {
		return err
	}
	*c = v
	return nil
}

// DriftDetection is how an application's drift is decided.
//
// manifest compares the rendered manifest against what was last applied, which
// catches a changed chart version, changed values and a changed template. live
// additionally compares the running ServiceSpec against the one the repository
// renders to, which is the only thing that can catch a change made to the swarm
// afterwards — Swarm has no server-side apply, so `docker service update
// --replicas 10` produces no conflict signal at all.
type DriftDetection string

const (
	DriftUnknown  DriftDetection = ""
	DriftManifest DriftDetection = "manifest"
	DriftLive     DriftDetection = "live"
)

// Valid reports whether d names a mode this build implements. It is what the
// config loader checks: unlike the wire, applications.yaml gets no leniency.
func (d DriftDetection) Valid() bool { return d == DriftManifest || d == DriftLive }

// MarshalJSON implements json.Marshaler.
func (d DriftDetection) MarshalJSON() ([]byte, error) { return marshalEnum(d, unknownName) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *DriftDetection) UnmarshalJSON(data []byte) error {
	v, err := unmarshalEnum(data, DriftManifest, DriftLive)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// DriftState is whether the running services match what the repository renders
// to, under driftDetection: live. It is a separate axis from SyncState rather
// than a member of it, because "git moved" and "the swarm moved" need different
// actions from an operator even though both make an application out of sync.
//
// Unknown is not merely "not evaluated". It is also what a release whose live
// state could not be read reports, and the reconciler will not converge one:
// the controller does not write on the strength of a read it could not make.
type DriftState string

const (
	DriftStateUnknown  DriftState = ""
	DriftStateNone     DriftState = "none"
	DriftStateDetected DriftState = "detected"
)

// MarshalJSON implements json.Marshaler.
func (d DriftState) MarshalJSON() ([]byte, error) { return marshalEnum(d, unknownName) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *DriftState) UnmarshalJSON(data []byte) error {
	v, err := unmarshalEnum(data, DriftStateNone, DriftStateDetected)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// DriftReason is how one service differs. The four are genuinely different
// problems: Modified is a service whose spec was changed, Missing is one that is
// declared and not running at all, Unexpected is one running under the stack's
// namespace that the manifest does not declare, and RolledBack is one Swarm
// itself reverted because the spec this controller wrote would not converge.
//
// Only two of them are worth redeploying for. An Unexpected service is not there
// to be rewritten, and a RolledBack one has already had the repository's answer
// rejected by the platform — see reconcile.convergeable.
type DriftReason string

const (
	DriftReasonUnknown DriftReason = ""
	DriftModified      DriftReason = "modified"
	DriftMissing       DriftReason = "missing"
	DriftUnexpected    DriftReason = "unexpected"
	DriftRolledBack    DriftReason = "rolled-back"
)

// MarshalJSON implements json.Marshaler.
func (d DriftReason) MarshalJSON() ([]byte, error) { return marshalEnum(d, unknownName) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *DriftReason) UnmarshalJSON(data []byte) error {
	v, err := unmarshalEnum(data, DriftModified, DriftMissing, DriftUnexpected, DriftRolledBack)
	if err != nil {
		return err
	}
	*d = v
	return nil
}
