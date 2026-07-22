// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package seam

import (
	"reflect"
	"sync"
	"testing"
)

func TestSlotZeroValueIsEmpty(t *testing.T) {
	var s Slot[string]
	if got := s.Get(); got != "" {
		t.Errorf("Get on a zero Slot = %q, want empty", got)
	}
	if got := s.Name(); got != "" {
		t.Errorf("Name on a zero Slot = %q, want empty", got)
	}
}

// The whole point of the mechanism: a companion registering after the default
// wins, because Go runs an imported package's init() first.
func TestSlotRegisterReplaces(t *testing.T) {
	var s Slot[string]
	s.Register("default", "oss")
	s.Register("companion", "be")

	if got := s.Get(); got != "be" {
		t.Errorf("Get = %q, want the last registration", got)
	}
	if got := s.Name(); got != "companion" {
		t.Errorf("Name = %q, want companion", got)
	}
}

func TestListRegisterAppends(t *testing.T) {
	var l List[string]
	if got := l.All(); len(got) != 0 {
		t.Errorf("All on a zero List = %v, want empty", got)
	}

	l.Register("log", "a")
	l.Register("stream", "b")
	l.Register("slack", "c")

	if got, want := l.All(), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("All = %v, want %v", got, want)
	}
	if got, want := l.Names(), []string{"log", "stream", "slack"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
}

// All returns a copy, so a caller ranging over the result cannot race a late
// registration and cannot mutate the list through it either.
func TestListAllReturnsACopy(t *testing.T) {
	var l List[string]
	l.Register("log", "a")

	got := l.All()
	got[0] = "tampered"

	if l.All()[0] != "a" {
		t.Error("mutating the result of All changed the list")
	}
}

func TestConcurrentRegisterAndRead(t *testing.T) {
	var (
		s  Slot[int]
		l  List[int]
		wg sync.WaitGroup
	)
	for i := range 50 {
		wg.Add(2)
		go func() { defer wg.Done(); s.Register("x", i); l.Register("x", i) }()
		go func() { defer wg.Done(); _, _ = s.Get(), l.All() }()
	}
	wg.Wait()

	if got := len(l.All()); got != 50 {
		t.Errorf("registered 50 entries, All has %d", got)
	}
}
