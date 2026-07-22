// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import (
	"encoding/json"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDurationJSON(t *testing.T) {
	d := Duration(90 * time.Second)

	got, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != `"1m30s"` {
		t.Errorf("marshal = %s, want \"1m30s\"", got)
	}

	var back Duration
	if err := json.Unmarshal([]byte(`"10m"`), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != Duration(10*time.Minute) {
		t.Errorf("unmarshal = %v, want 10m", back)
	}
}

func TestDurationYAML(t *testing.T) {
	var got struct {
		Interval Duration `yaml:"interval"`
	}
	if err := yaml.Unmarshal([]byte("interval: 45s\n"), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Interval != Duration(45*time.Second) {
		t.Errorf("interval = %v, want 45s", got.Interval)
	}

	out, err := yaml.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != "interval: 45s\n" {
		t.Errorf("marshal = %q, want %q", out, "interval: 45s\n")
	}
}

// An omitted interval or timeout means "use the default", so the empty string
// is zero rather than an error.
func TestDurationEmptyIsZero(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`""`), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d != 0 {
		t.Errorf("got %v, want 0", d)
	}
}

func TestDurationRejectsNonsense(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"soon"`), &d); err == nil {
		t.Error("want an error for an unparseable duration, got nil")
	}
	if err := json.Unmarshal([]byte(`60`), &d); err == nil {
		t.Error("want an error for a bare number, got nil")
	}
	if err := yaml.Unmarshal([]byte("interval: [1, 2]\n"), &struct {
		Interval Duration `yaml:"interval"`
	}{}); err == nil {
		t.Error("want an error for a non-scalar YAML value, got nil")
	}
}
