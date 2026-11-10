// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package webhook

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is the floor's clock and timer, so a test moves time rather than
// waiting it out.
type fakeClock struct {
	mu      sync.Mutex
	t       time.Time
	pending []scheduled
}

type scheduled struct {
	at time.Time
	f  func()
}

func newFakeClock() *fakeClock { return &fakeClock{t: testNow} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) after(d time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, scheduled{c.t.Add(d), f})
}

// advance moves the clock and runs, outside the lock, whatever fell due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	var due []func()
	kept := c.pending[:0]
	for _, s := range c.pending {
		if s.at.After(c.t) {
			kept = append(kept, s)
		} else {
			due = append(due, s.f)
		}
	}
	c.pending = kept
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

func clockedFloor(c *fakeClock) *floor {
	f := newFloor(10 * time.Second)
	f.now, f.after = c.now, c.after
	return f
}

// A wake inside the floor is deferred rather than dropped, everything after it
// folds into the one deferred wake, and it fires when the floor has passed since
// the last one did.
func TestTheFloorDefersAndFoldsRatherThanDrops(t *testing.T) {
	c := newFakeClock()
	f := clockedFloor(c)
	fired := 0
	fire := func() { fired++ }
	edge := target{app: "edge"}

	if f.wake(edge, fire) || fired != 1 {
		t.Fatalf("the first wake: fired %d, want at once", fired)
	}
	c.advance(3 * time.Second)
	if !f.wake(edge, fire) || fired != 1 {
		t.Fatalf("a wake 3s later: fired %d, want it deferred", fired)
	}
	c.advance(2 * time.Second)
	if !f.wake(edge, fire) {
		t.Fatal("a wake while one is deferred was not reported as deferred")
	}
	c.advance(4 * time.Second)
	if fired != 1 {
		t.Fatalf("fired %d at 9s, want the deferred wake still waiting for the floor", fired)
	}
	c.advance(time.Second)
	if fired != 2 {
		t.Fatalf("fired %d at 10s, want the two deferred wakes folded into one", fired)
	}

	// The floor is measured from that deferred wake, not from the first.
	c.advance(5 * time.Second)
	if !f.wake(edge, fire) {
		t.Error("a wake 5s after the deferred one fired was not deferred")
	}
	c.advance(5 * time.Second)
	if fired != 3 {
		t.Errorf("fired %d, want the next deferred wake 10s after the last", fired)
	}
	c.advance(10 * time.Second)
	if f.wake(edge, fire) || fired != 4 {
		t.Errorf("fired %d, want a wake once the floor has passed to run at once", fired)
	}
}

// Each target has its own floor: a push to one application's repository says
// nothing about how recently another was woken, nor the app set.
func TestEachTargetHasItsOwnFloor(t *testing.T) {
	f := clockedFloor(newFakeClock())
	fired := map[target]int{}
	for _, tg := range []target{{app: "edge"}, {app: "core"}, {appSet: true}} {
		if f.wake(tg, func() { fired[tg]++ }) || fired[tg] != 1 {
			t.Errorf("%+v was deferred by another target's wake", tg)
		}
	}
}
