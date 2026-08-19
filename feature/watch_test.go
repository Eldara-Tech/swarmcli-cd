// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package feature

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func at(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }

func licenceReport(status Status, expiresAt *time.Time) Report {
	return Report{
		Edition: "business",
		Licence: &Licence{Tier: "be", Status: status, ExpiresAt: expiresAt},
	}
}

// The states worth an operator's attention, and — as deliberately — the ones
// that are not. A warning on a healthy licence every six hours is how an
// operator learns to filter the line that matters.
func TestLicenceWarning_WhatIsWorthSaying(t *testing.T) {
	now := at(2026, time.August, 19)
	soon := now.Add(10 * 24 * time.Hour)
	distant := now.Add(200 * 24 * time.Hour)
	past := now.Add(-3 * 24 * time.Hour)

	cases := []struct {
		name     string
		report   Report
		wantSaid bool
		contains []string
	}{
		{
			name:     "a build with no licensed module says nothing",
			report:   Report{Edition: "community"},
			wantSaid: false,
		},
		{
			name:     "a valid licence with time left says nothing",
			report:   licenceReport(StatusValid, &distant),
			wantSaid: false,
		},
		{
			name:     "a perpetual licence says nothing",
			report:   licenceReport(StatusValid, nil),
			wantSaid: false,
		},
		{
			name:     "a valid licence inside the window warns with the days left",
			report:   licenceReport(StatusValid, &soon),
			wantSaid: true,
			contains: []string{"expires soon"},
		},
		{
			name:     "grace says the features are still on, and that it will not last",
			report:   licenceReport(StatusGrace, &past),
			wantSaid: true,
			contains: []string{"grace period", "still granted"},
		},
		{
			name:     "expired says the features are off",
			report:   licenceReport(StatusExpired, &past),
			wantSaid: true,
			contains: []string{"expired", "features are off"},
		},
		{
			name:     "invalid names the remedy, which is not a configuration change",
			report:   licenceReport(StatusInvalid, nil),
			wantSaid: true,
			contains: []string{"did not verify"},
		},
		{
			name:     "absent is said too — a licensed build with no licence is a state",
			report:   licenceReport(StatusAbsent, nil),
			wantSaid: true,
			contains: []string{"no licence is installed"},
		},
		{
			name:     "a status from a newer reporter is surfaced rather than swallowed",
			report:   licenceReport(Status("suspended"), nil),
			wantSaid: true,
			contains: []string{"does not know"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, args, ok := licenceWarning(tc.report, now)
			if ok != tc.wantSaid {
				t.Fatalf("said = %v, want %v (msg %q)", ok, tc.wantSaid, msg)
			}
			if !ok {
				return
			}
			for _, want := range tc.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not contain %q", msg, want)
				}
			}
			if len(args)%2 != 0 {
				t.Errorf("odd number of log arguments: %v", args)
			}
		})
	}
}

// The warning must name the remedy, not just the state: an operator reading it
// at 03:00 in a log pipeline has no page to click through to.
func TestLicenceWarning_NamesTheRemedy(t *testing.T) {
	now := at(2026, time.August, 19)
	past := now.Add(-time.Hour)

	for _, status := range []Status{StatusGrace, StatusExpired, StatusInvalid, StatusAbsent} {
		_, args, ok := licenceWarning(licenceReport(status, &past), now)
		if !ok {
			t.Fatalf("%s: nothing said", status)
		}
		var found bool
		for i := 0; i+1 < len(args); i += 2 {
			if args[i] == "remedy" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s carries no remedy", status)
		}
	}
}

// The reporter revalidates per request, so between requests the clock can be
// past an expiry the report still calls valid. Saying so beats saying nothing.
func TestLicenceWarning_ValidPastItsExpiry(t *testing.T) {
	now := at(2026, time.August, 19)
	past := now.Add(-time.Hour)

	msg, _, ok := licenceWarning(licenceReport(StatusValid, &past), now)
	if !ok || !strings.Contains(msg, "has not re-read it yet") {
		t.Fatalf("said = %v, msg = %q", ok, msg)
	}
}

// panicReporter is the seam misbehaving: the registered reporter is documented
// not to panic, but the implementation belongs to another module.
type panicReporter struct{}

func (panicReporter) Report(context.Context) Report { panic("licence reporter exploded") }

// A warning path that can take the controller down is a worse bug than the one
// it fixes.
func TestWatchLicence_SurvivesAPanickingReporter(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	rep := reportSafely(context.Background(), log, panicReporter{})
	if rep.Licence != nil {
		t.Fatalf("a panicking reporter must yield no licence: %+v", rep.Licence)
	}
	if !strings.Contains(buf.String(), "panicked") {
		t.Errorf("the panic was swallowed silently: %q", buf.String())
	}
}

// syncBuffer is a log sink two goroutines share: the watch writes to it while
// the test reads it. slog.Handler makes no promise about which goroutine
// writes, and a bytes.Buffer makes none about concurrent access.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type staticReporter struct{ rep Report }

func (s staticReporter) Report(context.Context) Report { return s.rep }

// register installs a reporter for one test and puts the previous one back,
// the way TestRegisterReplacesTheReporter does. WatchLicence reads the
// registered reporter rather than taking one, because Get is this package's to
// call — a watch that accepted a Reporter would hand one to a caller outside
// it.
func register(t *testing.T, r Reporter) {
	t.Helper()
	before, name := Get(), Active()
	t.Cleanup(func() { Register(name, before) })
	Register("watch-test", r)
}

// It warns on the first pass rather than waiting out an interval — a
// controller restarted into a lapsed licence must say so at startup, which is
// the moment an operator is watching.
func TestWatchLicence_WarnsOnTheFirstPassAndStops(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	now := at(2026, time.August, 19)
	past := now.Add(-2 * 24 * time.Hour)

	register(t, staticReporter{licenceReport(StatusGrace, &past)})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		WatchLicence(ctx, log, func() time.Time { return now })
		close(done)
	}()

	// The first pass happens before the first tick, so the line is there
	// without waiting six hours for it.
	deadline := time.After(2 * time.Second)
	for !strings.Contains(buf.String(), "grace period") {
		select {
		case <-deadline:
			t.Fatalf("no warning on the first pass: %q", buf.String())
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchLicence did not stop when its context was cancelled")
	}
}
