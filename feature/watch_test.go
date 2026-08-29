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
		// remedy is checked against the `remedy` log argument rather than the
		// message. It was unchecked entirely: TestLicenceWarning_NamesTheRemedy
		// asserts the key is present and never what it says, so a remedy that
		// named the wrong action passed both tests.
		remedy []string
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
			// The managed half of the remedy, which grace has carried since
			// #242 and nothing checked. It is the shape absent's now follows.
			remedy: []string{"renew the licence", "license sync"},
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
			name:     "absent is said too — a licensed build with nothing granting is a state",
			report:   licenceReport(StatusAbsent, nil),
			wantSaid: true,
			// Both halves of the remedy, because absent is two states: no
			// licence at all, and a managed one this swarm never activated.
			// Saying only "install" sends the second one's operator after a
			// key already in their hand.
			//
			// And both routes to the second half. 'license sync' reaches the
			// licence service, which the swarm most likely to be sitting in
			// this state cannot do; the lease file it already holds is the
			// remedy swarmcli-be's managed-licensing doc gives that operator.
			contains: []string{"no licence is active"},
			remedy:   []string{"install a licence", "activate a managed one", "lease install"},
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
			for _, want := range tc.remedy {
				if got := remedyOf(args); !strings.Contains(got, want) {
					t.Errorf("remedy %q does not contain %q", got, want)
				}
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
		// The value, not just the key: an empty remedy is not a remedy, and
		// asserting only that the key is there is what let one go unread.
		if got := remedyOf(args); got == "" {
			t.Errorf("%s carries no remedy", status)
		}
	}
}

// remedyOf is the `remedy` log argument's value, or "" when there is none.
func remedyOf(args []any) string {
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "remedy" {
			s, _ := args[i+1].(string)
			return s
		}
	}
	return ""
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

// The deadline, which is the thing an operator in a grace period actually
// needs: "still granting features" is the same sentence one day and
// twenty-six days from the end.
//
// And the word this line may not use. StatusGrace folds a token that really
// did expire and a managed licence that verifies, has not expired, and whose
// activation renewal is late; "the licence has expired" is false for the
// second, and its reader goes looking for a replacement licence instead of
// running the sync the remedy already names.
func TestLicenceWarning_GraceNamesTheDeadlineWithoutAssertingAnExpiry(t *testing.T) {
	now := at(2026, time.August, 19)
	lapsed := now.Add(-4 * 24 * time.Hour)
	off := now.Add(26 * 24 * time.Hour)

	rep := licenceReport(StatusGrace, &lapsed)
	rep.Licence.FeaturesOffAt = &off

	msg, args, ok := licenceWarning(rep, now)
	if !ok {
		t.Fatal("nothing said for a licence in its grace period")
	}
	if strings.Contains(msg, "expired") {
		t.Errorf("message %q asserts an expiry; half the states that reach here have not expired", msg)
	}
	if got := argOf(args, "featuresOffAt"); got != off.UTC().Format(time.RFC3339) {
		t.Errorf("featuresOffAt = %v, want %v", got, off.UTC().Format(time.RFC3339))
	}
	if got := argOf(args, "daysLeft"); got != 26 {
		t.Errorf("daysLeft = %v, want 26", got)
	}
	// The date it did lapse on is still worth carrying — it is what a support
	// ticket quotes — under a key that does not claim which clock it was.
	if got := argOf(args, "lapsedAt"); got != lapsed.UTC().Format(time.RFC3339) {
		t.Errorf("lapsedAt = %v, want %v", got, lapsed.UTC().Format(time.RFC3339))
	}
}

// A companion built before the field existed sets neither of the two new ones,
// and the line it produces has to stay a line. The message points at
// 'featuresOffAt', so the key has to be there saying it does not know rather
// than absent.
func TestLicenceWarning_GraceFromACompanionWithNoDeadline(t *testing.T) {
	now := at(2026, time.August, 19)
	lapsed := now.Add(-4 * 24 * time.Hour)

	msg, args, ok := licenceWarning(licenceReport(StatusGrace, &lapsed), now)
	if !ok || !strings.Contains(msg, "grace period") {
		t.Fatalf("said = %v, msg = %q", ok, msg)
	}
	if got := argOf(args, "featuresOffAt"); got != "unknown" {
		t.Errorf("featuresOffAt = %v, want %q", got, "unknown")
	}
	if got := remedyOf(args); got == "" {
		t.Error("the remedy went missing along with the date")
	}
}

// The state defect three is about: a licence the issuer has judged over its
// node allowance is `valid` here, grants everything, and — before this — said
// nothing anywhere. The cap is judged at the issuer, so the first enforced
// sign of it is a term that stops being rolled forward, which under a
// year-long term is a year of silence ending in the features switching off.
func TestLicenceWarning_TheIssuersAllowanceIsSaidWhenNothingElseIs(t *testing.T) {
	now := at(2026, time.August, 19)
	distant := now.Add(200 * 24 * time.Hour)
	ends := at(2026, time.October, 1)

	rep := licenceReport(StatusValid, &distant)
	rep.Licence.Allowance = &Allowance{OverLimit: true, Nodes: 4, MaxNodes: 3, TermEndsAt: &ends}

	msg, args, ok := licenceWarning(rep, now)
	if !ok {
		t.Fatal("nothing said about a swarm the licence service reports over its allowance")
	}
	// Nothing is switched off, and the line has to say so: an operator told
	// only "allowance exceeded" assumes something has already broken.
	if !strings.Contains(msg, "nothing is switched off") {
		t.Errorf("message %q does not say the features are still on", msg)
	}
	if got := argOf(args, "nodes"); got != 4 {
		t.Errorf("nodes = %v, want 4", got)
	}
	if got := argOf(args, "maxNodes"); got != 3 {
		t.Errorf("maxNodes = %v, want 3", got)
	}
	if got := argOf(args, "termEndsAt"); got != ends.UTC().Format(time.RFC3339) {
		t.Errorf("termEndsAt = %v, want %v", got, ends.UTC().Format(time.RFC3339))
	}
	if got := remedyOf(args); got == "" {
		t.Error("the advisory carries no remedy")
	}
}

// Zero is the issuer saying nothing rather than a swarm with no nodes, so the
// line says less rather than reporting "nodes=0 maxNodes=0" — which reads as a
// bug in this controller rather than as a report it passed on.
func TestLicenceWarning_AnAllowanceWithNoCountsOmitsThem(t *testing.T) {
	now := at(2026, time.August, 19)
	rep := licenceReport(StatusValid, nil)
	rep.Licence.Allowance = &Allowance{OverLimit: true}

	_, args, ok := licenceWarning(rep, now)
	if !ok {
		t.Fatal("nothing said")
	}
	if got := argOf(args, "nodes"); got != nil {
		t.Errorf("nodes = %v, want the key absent", got)
	}
	if got := argOf(args, "maxNodes"); got != nil {
		t.Errorf("maxNodes = %v, want the key absent", got)
	}
	if got := argOf(args, "termEndsAt"); got != "unknown" {
		t.Errorf("termEndsAt = %v, want %q", got, "unknown")
	}
}

// A report inside its allowance is the ordinary state, and the ordinary state
// is silent — a warning every six hours on a healthy licence is how an
// operator learns to filter the line that matters.
func TestLicenceWarning_AnAllowanceInsideItsLimitSaysNothing(t *testing.T) {
	now := at(2026, time.August, 19)
	distant := now.Add(200 * 24 * time.Hour)

	rep := licenceReport(StatusValid, &distant)
	rep.Licence.Allowance = &Allowance{Nodes: 2, MaxNodes: 3}

	if _, _, ok := licenceWarning(rep, now); ok {
		t.Error("a swarm inside its allowance was warned about")
	}
}

// The advisory block is unsigned — it is what a server said, and a server is
// what one firewall rule can stand in for — so it may fill a silence and may
// never speak over the licence itself. Anything else would let whoever can
// answer a request decide what this controller tells an operator about a
// licence it verified offline.
//
// This is as close as the report side gets to pinning "nothing branches on
// it": every state that has something of its own to say says exactly the same
// thing with the block present as without it.
func TestLicenceWarning_TheAdvisoryNeverOverridesTheLicence(t *testing.T) {
	now := at(2026, time.August, 19)
	past := now.Add(-3 * 24 * time.Hour)
	soon := now.Add(10 * 24 * time.Hour)
	ends := at(2026, time.October, 1)

	for _, tc := range []struct {
		status  Status
		expires *time.Time
	}{
		{StatusGrace, &past},
		{StatusExpired, &past},
		{StatusInvalid, nil},
		{StatusAbsent, nil},
		{StatusValid, &soon},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			plain, _, ok := licenceWarning(licenceReport(tc.status, tc.expires), now)
			if !ok {
				t.Fatalf("%s says nothing on its own; this test would pass vacuously", tc.status)
			}

			rep := licenceReport(tc.status, tc.expires)
			rep.Licence.Allowance = &Allowance{OverLimit: true, Nodes: 9, MaxNodes: 3, TermEndsAt: &ends}
			withBlock, _, ok := licenceWarning(rep, now)
			if !ok || withBlock != plain {
				t.Errorf("message = %q, want the licence's own %q: an unsigned report displaced a verified one",
					withBlock, plain)
			}
		})
	}
}

// argOf is one log argument's value, or nil when the key is not there.
func argOf(args []any, key string) any {
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == key {
			return args[i+1]
		}
	}
	return nil
}
