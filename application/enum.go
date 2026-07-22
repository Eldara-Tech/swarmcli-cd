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

// DriftDetection is how an application's drift is decided. Phase 1 has one
// mode: manifest, which compares the rendered manifest against what was last
// applied. Comparing the desired ServiceSpec against the live one is Phase 2.
type DriftDetection string

const (
	DriftUnknown  DriftDetection = ""
	DriftManifest DriftDetection = "manifest"
)

// Valid reports whether d names a mode this build implements. It is what the
// config loader checks: unlike the wire, applications.yaml gets no leniency.
func (d DriftDetection) Valid() bool { return d == DriftManifest }

// MarshalJSON implements json.Marshaler.
func (d DriftDetection) MarshalJSON() ([]byte, error) { return marshalEnum(d, unknownName) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *DriftDetection) UnmarshalJSON(data []byte) error {
	v, err := unmarshalEnum(data, DriftManifest)
	if err != nil {
		return err
	}
	*d = v
	return nil
}
