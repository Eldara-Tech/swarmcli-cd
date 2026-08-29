// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The badge, across all five statuses of feature.Status.
//
// Rendered through <App /> rather than in isolation, because what the badge has
// to get right is not its own markup: it is that a build with no licence to
// report draws nothing at all, and that is a property of the header it sits in.

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App, queryClient } from "../App";
import { setToken } from "../auth/session";
import {
  clone,
  communityCapabilities,
  communityDiscovery,
  controller,
  json,
  openStream,
} from "../test/fakeApi";

const okStatus = {
  appSet: { mode: "static", loadedAt: "2026-07-22T09:41:10Z", stale: false },
  applications: 0,
};

/** An instant `days` before now, so an assertion about "4 days ago" needs no clock control. */
function daysBeforeNow(days: number): string {
  return new Date(Date.now() - days * 86_400_000).toISOString();
}

/**
 * An instant `days` after now, with an hour of slack so that whole-day
 * rounding lands on `days` rather than on `days - 1` when the test runs a
 * millisecond later than the value was built.
 */
function daysAfterNow(days: number): string {
  return new Date(Date.now() + days * 86_400_000 + 3_600_000).toISOString();
}

/**
 * The capability document of a licensed build whose licence is in `status`.
 *
 * A plain string rather than LicenceStatus, so the last test below can send a
 * status this build has never heard of — which is what a controller ahead of it
 * does.
 */
function licensed(
  status: string,
  expiresAt: string | null,
  extra: Record<string, unknown> = {},
): unknown {
  const capabilities = clone(communityCapabilities) as Record<string, unknown>;
  capabilities.edition = "business";
  capabilities.licence = { tier: "be", status, expiresAt, ...extra };
  return capabilities;
}

async function show(capabilities: unknown): Promise<void> {
  controller({
    ...communityDiscovery(),
    "/api/v1/capabilities": () => json(200, capabilities),
    "/api/v1/status": () => json(200, okStatus),
    "/api/v1/applications": () => json(200, { applications: [] }),
    "/api/v1/events": openStream,
  });
  render(<App />);
  await screen.findByTestId("application-count");
}

/** The badge's whole text, chip and detail together. */
async function badgeText(): Promise<string> {
  return (await screen.findByTestId("licence-badge")).textContent ?? "";
}

beforeEach(() => {
  sessionStorage.clear();
  queryClient.clear();
  window.history.pushState({}, "", "/");
  setToken("good");
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("the licence badge", () => {
  // The acceptance criterion, from the badge's own side: an Apache-2.0 build
  // has no licensed module and therefore no licence, and a chip reading
  // "community" would be a control that can never say anything else.
  it("renders nothing in a build with no licence to report", async () => {
    await show(communityCapabilities);

    expect(screen.queryByTestId("licence-badge")).toBeNull();
  });

  it("reads valid, with what expires and when", async () => {
    await show(licensed("valid", "2027-03-01T00:00:00Z"));

    const text = await badgeText();
    expect(text).toContain("licence valid");
    expect(text).toContain("be");
    expect(text).toContain("expires");
  });

  // A licence four days from expiry used to read green, and the first thing its
  // expiry takes is SSO — which locks out the operators who would have noticed
  // (#233). The badge has to stop reading healthy while there is still time to
  // act on it.
  it("warns before the licence expires, not after", async () => {
    await show(licensed("valid", daysAfterNow(4)));

    const text = await badgeText();
    expect(text).toContain("licence expiring");
    expect(text).toContain("4 days left");
    expect(text).toContain("renew now");
    expect(screen.getByText("licence expiring").className).toBe(
      "chip chip-warn",
    );
  });

  it("still reads healthy while the expiry is far off", async () => {
    await show(licensed("valid", daysAfterNow(200)));

    const text = await badgeText();
    expect(text).toContain("licence valid");
    expect(text).not.toContain("renew now");
    expect(screen.getByText("licence valid").className).toBe("chip chip-good");
  });

  // The last day is a day, not a rounding artefact: "0 days left" would read as
  // an expiry that has already happened.
  it("says expires today on the final day", async () => {
    await show(
      licensed("valid", new Date(Date.now() + 3 * 3_600_000).toISOString()),
    );

    expect(await badgeText()).toContain("expires today");
  });

  it("says perpetual rather than nothing for a licence with no expiry", async () => {
    await show(licensed("valid", null));

    expect(await badgeText()).toContain("perpetual");
  });

  // The one the fifth status exists for (D25). A licence in its grace period is
  // granting features and is not valid, so a badge that read healthy would stay
  // green until the morning everything stopped.
  //
  // "lapsed" rather than "expired": two states reach this status, and the
  // managed one verifies and has not expired at all.
  it("does not read healthy in the grace period, and says how long ago it lapsed", async () => {
    await show(licensed("grace", daysBeforeNow(4)));

    const text = await badgeText();
    expect(text).toContain("grace period");
    expect(text).toContain("lapsed 4 days ago");
    expect(text).not.toContain("expired");
    expect(text).toContain("renew now");
    expect(text).not.toContain("valid");
    // Warn, not good: the features are still on, and there has to be somewhere
    // louder to go when they are not.
    expect(screen.getByText("grace period").className).toBe("chip chip-warn");
  });

  // The whole of defect one. "still granting features" is the same sentence one
  // day and twenty-six days from the end, and the length of a grace period is
  // the licensed module's — a badge cannot work it out from `expiresAt`, which
  // is why the controller now sends the day itself.
  it("names the day the features stop, not just the day something lapsed", async () => {
    await show(
      licensed("grace", daysBeforeNow(4), { featuresOffAt: daysAfterNow(26) }),
    );

    const text = await badgeText();
    expect(text).toContain("lapsed 4 days ago");
    expect(text).toContain("they stop in 26 days");
  });

  // A controller older than this bundle sends no such key, and the line it
  // produces has to be the line it always produced rather than a phrase about
  // a date nobody has.
  it("says nothing about a deadline a controller did not send", async () => {
    await show(licensed("grace", daysBeforeNow(4)));

    expect(await badgeText()).not.toContain("they stop");
  });

  it("reads expired, and says the features are off", async () => {
    await show(licensed("expired", daysBeforeNow(1)));

    const text = await badgeText();
    expect(text).toContain("licence expired");
    expect(text).toContain("expired 1 day ago");
    expect(text).toContain("features are off");
    expect(screen.getByText("licence expired").className).toBe("chip chip-bad");
  });

  it("reads invalid, and points at the vendor rather than at the deployment", async () => {
    await show(licensed("invalid", null));

    expect(await badgeText()).toContain("licence invalid");
    expect(await badgeText()).toContain("ask for a replacement");
  });

  // Two states arrive here — no licence at all, and a managed one this swarm
  // has never been activated for — because the seam is frozen at five values.
  // The copy has to fit both, so it names both halves of the remedy.
  it("reads absent as something to turn on, not as something broken", async () => {
    await show(licensed("absent", null));

    const text = await badgeText();
    expect(text).toContain("no active licence");
    expect(text).toContain("install one");
    expect(text).toContain("activate a managed one");
  });

  // A companion that sends the wrong one of two clocks — a managed licence's
  // token expiry instead of its lease's — reports a lapse beside a date that
  // has not happened. Flooring the age made that read "today", which is the one
  // wrong answer that looks right.
  it("does not invent a day for a lapse that has not happened", async () => {
    await show(licensed("grace", daysAfterNow(30)));

    const text = await badgeText();
    expect(text).toContain("grace period");
    expect(text).toContain("lapsed");
    expect(text).not.toContain("lapsed today");
  });

  // Defect three. The free tier's node cap is judged at the licence service, so
  // a controller-only deployment over it is `valid` here, grants everything,
  // and had no surface saying so at all — a year of silence ending in the
  // features switching off.
  it("renders the issuer's node allowance when it says the swarm is over it", async () => {
    await show(
      licensed("valid", daysAfterNow(200), {
        allowance: {
          overLimit: true,
          nodes: 4,
          maxNodes: 3,
          termEndsAt: "2026-10-01T00:00:00Z",
        },
      }),
    );

    const advisory = (await screen.findByTestId("licence-allowance"))
      .textContent;
    expect(advisory).toContain("4 of 3 nodes");
    expect(advisory).toContain("stops rolling");
    expect(advisory).toContain("unless the count comes down");
  });

  // It is unsigned — what a server said, not what the controller verified
  // against a compiled-in key — so it may say something and may not make the
  // build look like a different licence state. The chip stays exactly what the
  // signed document earned.
  it("does not let the unsigned allowance change the licence's own state", async () => {
    await show(
      licensed("valid", daysAfterNow(200), {
        allowance: { overLimit: true, nodes: 4, maxNodes: 3, termEndsAt: null },
      }),
    );

    expect(await badgeText()).toContain("licence valid");
    expect(screen.getByText("licence valid").className).toBe("chip chip-good");
  });

  // Inside the allowance there is nothing to say, and a badge reciting "2 of 3
  // nodes" on every healthy licence is a badge an operator stops reading.
  it("says nothing about an allowance the swarm is inside", async () => {
    await show(
      licensed("valid", daysAfterNow(200), {
        allowance: { overLimit: false, nodes: 2, maxNodes: 3, termEndsAt: null },
      }),
    );

    await screen.findByTestId("licence-badge");
    expect(screen.queryByTestId("licence-allowance")).toBeNull();
  });

  // A controller ahead of this build reports a sixth status. It has to render
  // as something: falling through every case would leave a header with a
  // licence the operator cannot see at all.
  it("renders a status this build has never heard of", async () => {
    await show(licensed("revoked", null));

    expect(await badgeText()).toContain("licence revoked");
  });
});
