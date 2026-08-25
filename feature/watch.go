// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package feature

import (
	"context"
	"log/slog"
	"time"
)

// Telling an operator that the licence is about to stop granting, before it
// does.
//
// The licence reader is built so that nothing about licensing can stop the
// controller converging — every failure resolves to a report, a log line and a
// build that grants nothing. That is the right call for a reconciler holding
// write access to a swarm, and it has one consequence: an expiring licence
// produced no warning anywhere an operator looks, and then SSO stopped. The
// people who would have noticed are the ones locked out of the UI by the
// feature that lapsed (#233).
//
// The badge in the web UI is the other half of this. It is not enough on its
// own for the deployment that needs it most: swarmcli-cd is a daemon, which is
// the point of it, so nobody is looking at its UI on the day this matters.
// A log line reaches a log pipeline, an alert rule and a support ticket.
//
// Two properties this must keep, both of them the reason the reader was
// written the way it was:
//
//   - It cannot fail the controller. It runs on its own goroutine, holds no
//     lock anything else takes, and recovers from a reporter that panics —
//     a warning path that can take the process down is a worse bug than the
//     one it fixes.
//   - It says nothing in a build with no licensed module. `Licence` is nil
//     there, and a free build must not grow a licensing log line any more
//     than it grows a badge (#178).
const (
	// licenceCheckInterval is how often the licence is re-examined. The
	// quantities are days and weeks — a lease window is 30 days, a token's
	// grace is measured in days — so six hours is far finer than the thing
	// being watched, and cheap: the report is served from the reporter's own
	// cache and touches no daemon.
	licenceCheckInterval = 6 * time.Hour

	// licenceExpiryWarning is how long before expiry the warnings start. Two
	// weeks is enough for a renewal that involves a human on the other side —
	// a support ticket, an invoice, a signing ceremony — and short enough that
	// it is not background noise for a year.
	licenceExpiryWarning = 14 * 24 * time.Hour

	// licenceRenotify bounds repetition. Without it a controller warns every
	// six hours for the whole window and the line becomes wallpaper; without
	// any repetition a warning emitted once at 03:00 is a warning nobody read.
	licenceRenotify = 24 * time.Hour
)

// WatchLicence logs the licence's state whenever it is worth an operator's
// attention, at startup and then every licenceCheckInterval, until ctx ends.
//
// It lives here rather than in the controller because the report is this
// package's to interpret — and because Get is this package's to call. The seam
// is a report and never an enforcement point, which feature_test.go keeps true
// by refusing any feature.Get outside api; a log line is not a gate, and the
// way to keep saying so is to not need the exception.
//
// Blocks; callers run it on its own goroutine.
func WatchLicence(ctx context.Context, log *slog.Logger, now func() time.Time) {
	ticker := time.NewTicker(licenceCheckInterval)
	defer ticker.Stop()

	var lastMsg string
	var lastAt time.Time

	for {
		if msg, args, ok := licenceWarning(reportSafely(ctx, log, Get()), now()); ok {
			// Repeat only when the state changed, or when it has been long
			// enough that a still-unresolved warning is worth saying again.
			if msg != lastMsg || now().Sub(lastAt) >= licenceRenotify {
				log.Warn(msg, args...)
				lastMsg, lastAt = msg, now()
			}
		} else {
			lastMsg, lastAt = "", time.Time{}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reportSafely asks the reporter for a report and survives it misbehaving.
//
// The registered reporter is documented not to panic. This one is a seam,
// which means the implementation is somebody else's — including a companion
// module's — and a panic on this goroutine would take the controller down
// with it. Recovering here is what keeps "the licence could not be read" from
// outranking "the swarm is converging".
func reportSafely(ctx context.Context, log *slog.Logger, r Reporter) (rep Report) {
	defer func() {
		if p := recover(); p != nil {
			log.Warn("the licence reporter panicked; licence state is unknown and nothing else is affected", "panic", p)
			rep = Report{}
		}
	}()
	return r.Report(ctx)
}

// licenceWarning renders the one line worth logging for a report, or reports
// that there is nothing to say.
//
// Pure, so every state is testable without a clock, a goroutine or a licence.
// The states it stays silent about are as deliberate as the ones it warns on:
// a valid licence with time left, a perpetual one, and a build with no
// licensed module at all.
func licenceWarning(rep Report, now time.Time) (msg string, args []any, ok bool) {
	lic := rep.Licence
	if lic == nil {
		// The Apache-2.0 build. There is no licence to lapse.
		return "", nil, false
	}

	switch lic.Status {
	case StatusGrace:
		return "the licence has expired and is in its grace period — features are still granted, but not for long",
			[]any{"status", lic.Status, "tier", lic.Tier, "expired", expiredArg(lic.ExpiresAt), "remedy", "renew the licence, or run 'swarmcli license sync' on a manager if it is a managed licence"}, true

	case StatusExpired:
		return "the licence has expired — licensed features are off",
			[]any{"status", lic.Status, "tier", lic.Tier, "expired", expiredArg(lic.ExpiresAt), "remedy", "install a current licence and restart the controller"}, true

	case StatusInvalid:
		return "the licence did not verify — licensed features are off",
			[]any{"status", lic.Status, "remedy", "ask for a replacement licence; this is not a controller misconfiguration"}, true

	case StatusAbsent:
		// Not a lapse: nothing is broken and the operator's own action fixes
		// it. Said once at startup (the first pass) and then only every
		// licenceRenotify, by the caller's own repetition rule.
		//
		// "installed" is the word this cannot use, and the remedy is two:
		// StatusAbsent also carries a managed licence that is installed and
		// not activated for this swarm, whose operator is holding the key
		// already and would spend the afternoon installing it again.
		return "no licence is active — licensed features are off",
			[]any{"status", lic.Status, "remedy", "install a licence, or run 'swarmcli license sync' on a manager to activate a managed one"}, true

	case StatusValid:
		if lic.ExpiresAt == nil {
			return "", nil, false // perpetual
		}
		left := lic.ExpiresAt.Sub(now)
		if left > licenceExpiryWarning {
			return "", nil, false
		}
		if left <= 0 {
			// The reporter has not caught up with the clock — its own
			// revalidation is per-request and this is between requests.
			// Saying so beats saying nothing until it does.
			return "the licence expiry has passed and the controller has not re-read it yet",
				[]any{"status", lic.Status, "tier", lic.Tier, "expiresAt", lic.ExpiresAt.UTC().Format(time.RFC3339)}, true
		}
		return "the licence expires soon",
			[]any{
				"status", lic.Status, "tier", lic.Tier,
				"expiresAt", lic.ExpiresAt.UTC().Format(time.RFC3339),
				"daysLeft", int(left.Hours() / 24),
				"remedy", "renew it before that date; losing the licence takes SSO with it",
			}, true
	}

	// A status this build does not know, from a reporter ahead of it. Worth a
	// line precisely because it cannot be interpreted here.
	return "the licence reports a status this build does not know",
		[]any{"status", lic.Status}, true
}

// expiredArg is the expiry as a log value, or "unknown" when the reporter sent
// an expired status with no date — a contradiction worth showing rather than
// inventing a day for.
func expiredArg(at *time.Time) string {
	if at == nil {
		return "unknown"
	}
	return at.UTC().Format(time.RFC3339)
}
