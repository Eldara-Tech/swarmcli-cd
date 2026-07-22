// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import (
	"encoding/json"
	"testing"
)

// Each enum is exercised through a pointer to its zero value, so one table
// covers marshalling, the unknown-name rule and the not-a-string rule for all
// of them.
type enumCase struct {
	name    string
	target  json.Unmarshaler
	known   string // a name this build recognises
	members []json.Marshaler
}

func enumCases() []enumCase {
	var (
		sync   SyncState
		health HealthState
		action SyncAction
		compat CompatState
		drift  DriftDetection
	)
	return []enumCase{
		{"SyncState", &sync, "out-of-sync",
			[]json.Marshaler{SyncSynced, SyncOutOfSync}},
		{"HealthState", &health, "degraded",
			[]json.Marshaler{HealthHealthy, HealthProgressing, HealthDegraded, HealthMissing}},
		{"SyncAction", &action, "upgrade",
			[]json.Marshaler{ActionUnchanged, ActionInstall, ActionUpgrade}},
		{"CompatState", &compat, "incompatible",
			[]json.Marshaler{CompatOK, CompatIncompatible}},
		{"DriftDetection", &drift, "manifest",
			[]json.Marshaler{DriftManifest}},
	}
}

func TestEnumKnownNameRoundTrips(t *testing.T) {
	for _, c := range enumCases() {
		t.Run(c.name, func(t *testing.T) {
			if err := c.target.UnmarshalJSON([]byte(`"` + c.known + `"`)); err != nil {
				t.Fatalf("unmarshal %q: %v", c.known, err)
			}
			got, err := json.Marshal(c.target)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != `"`+c.known+`"` {
				t.Errorf("round trip = %s, want %q", got, c.known)
			}
		})
	}
}

// An older client meeting a state a newer controller reports must show
// "unknown", not fail to decode the whole response.
func TestEnumUnrecognisedNameDecodesToUnknown(t *testing.T) {
	for _, c := range enumCases() {
		t.Run(c.name, func(t *testing.T) {
			if err := c.target.UnmarshalJSON([]byte(`"live-from-2027"`)); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, err := json.Marshal(c.target)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != `"unknown"` {
				t.Errorf("got %s, want \"unknown\"", got)
			}
		})
	}
}

// Not recognising a name is expected; being sent a number is not.
func TestEnumNonStringIsAnError(t *testing.T) {
	for _, c := range enumCases() {
		t.Run(c.name, func(t *testing.T) {
			if err := c.target.UnmarshalJSON([]byte(`7`)); err == nil {
				t.Error("want an error for a non-string value, got nil")
			}
		})
	}
}

func TestEnumZeroValueMarshalsAsUnknown(t *testing.T) {
	for _, c := range enumCases() {
		t.Run(c.name, func(t *testing.T) {
			for _, m := range c.members {
				got, err := json.Marshal(m)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if string(got) == `"unknown"` {
					t.Errorf("member marshalled as unknown: %s", got)
				}
			}
		})
	}
	// The zero value of every enum is its Unknown member.
	for name, got := range map[string]json.Marshaler{
		"SyncState":      SyncUnknown,
		"HealthState":    HealthUnknown,
		"SyncAction":     ActionUnknown,
		"CompatState":    CompatUnknown,
		"DriftDetection": DriftUnknown,
	} {
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if string(b) != `"unknown"` {
			t.Errorf("%s zero value marshalled as %s, want \"unknown\"", name, b)
		}
	}
}

func TestDriftDetectionValid(t *testing.T) {
	for _, tc := range []struct {
		in   DriftDetection
		want bool
	}{
		{DriftManifest, true},
		{DriftUnknown, false},
		{DriftDetection("live"), false}, // Phase 2, and not implemented by this build
	} {
		if got := tc.in.Valid(); got != tc.want {
			t.Errorf("%q.Valid() = %v, want %v", tc.in, got, tc.want)
		}
	}
}
