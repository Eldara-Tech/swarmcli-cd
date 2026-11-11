// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package webhook

import (
	"sync"
	"time"
)

// target is one thing a delivery can wake: the app set, or one application.
type target struct {
	appSet bool
	app    string
}

// floor holds each target to one wake per min, which is application.MinInterval
// for the reason that floor exists: a wake is a git fetch and a full render, and
// whoever can push to a repository chooses how often deliveries arrive.
//
// A wake that comes too soon is deferred rather than dropped, and every one
// after it until then folds into that one. Dropping it would lose the push that
// mattered: the second of two commits a few seconds apart is the one whose
// content is still waiting to be read.
type floor struct {
	min   time.Duration
	now   func() time.Time
	after func(time.Duration, func())

	mu      sync.Mutex
	last    map[target]time.Time
	pending map[target]bool
}

func newFloor(min time.Duration) *floor {
	return &floor{
		min:     min,
		now:     time.Now,
		after:   func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		last:    map[target]time.Time{},
		pending: map[target]bool{},
	}
}

// wake runs fire now if t has not been woken within min, and otherwise once min
// has passed since it was. It reports whether it deferred.
//
// fire is called outside the lock. What it does — a non-blocking send on a wake
// channel — cannot wait on anything, but the floor has no reason to rely on it.
func (f *floor) wake(t target, fire func()) (deferred bool) {
	f.mu.Lock()
	if f.pending[t] {
		f.mu.Unlock()
		return true
	}
	now := f.now()
	// A target never woken has a zero last, and Sub saturates rather than
	// overflowing, so its wait is negative.
	if wait := f.min - now.Sub(f.last[t]); wait > 0 {
		f.pending[t] = true
		f.mu.Unlock()
		f.after(wait, func() {
			f.mu.Lock()
			delete(f.pending, t)
			f.last[t] = f.now()
			f.mu.Unlock()
			fire()
		})
		return true
	}
	f.last[t] = now
	f.mu.Unlock()
	fire()
	return false
}
