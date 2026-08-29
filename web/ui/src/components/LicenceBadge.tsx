// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import type { ReactNode } from "react";

import {
  licenceStatuses,
  useCapabilities,
  type Allowance,
  type Licence,
  type LicenceStatus,
} from "../api/discovery";
import { decodeEnum } from "../api/enums";
import { plural } from "../format";
import { Instant } from "./Instant";

/**
 * The licence badge: what state this build's licence is in, and what to do
 * about it.
 *
 * # It renders nothing in the free build
 *
 * `licence` is null in a build with no licensed module linked — the Apache-2.0
 * one — and nothing is drawn for it. That is the acceptance criterion of #178
 * rather than a shortcut: a chip reading "community" in a build that has no
 * licence to report is a control that can never say anything else, and the free
 * product must not grow one. The state that *is* worth a badge in a build with
 * everything switched off is a licensed one whose licence lapsed, and that one
 * carries a licence object saying so.
 *
 * # grace is why the five statuses are five
 *
 * A licence in its grace period is granting features and is not valid, so a
 * badge that folded it into either would be wrong in the direction that matters:
 * it reads healthy right up to the morning the features stop. What is rendered
 * for it is the day that morning falls on, in the loudest treatment short of a
 * failure.
 *
 * That day used to be missing, and this comment used to ask the next reader for
 * the field rather than the arithmetic. It is `featuresOffAt` now: the length of
 * a grace period is the licensed module's and is not the same length in the two
 * states that reach "grace", so a deadline derived here from a guessed period
 * would have been right for one of them and confidently wrong for the other.
 *
 * The same two states are why nothing here says "expired". One of them is a
 * managed licence that verifies and has not expired at all — its activation
 * renewal is simply late — and its reader, told the licence expired, goes
 * looking for a replacement instead of syncing the activation.
 *
 * # the allowance is rendered and never acted on
 *
 * `allowance` is the licence issuer's own report and is unsigned: it is what a
 * server said, not something the controller verified against a compiled-in key.
 * So it is drawn beside the status and never becomes one — it does not colour
 * the chip, hide a control or change what the build claims to grant. See
 * feature.Allowance for the argument.
 */
export function LicenceBadge() {
  const licence = useCapabilities().data?.licence;
  // undefined while the request is in flight or after it failed, null in a
  // build with no licensed module. Both render nothing, and that is what makes
  // a controller with no capability endpoint indistinguishable from a free one.
  if (licence === undefined || licence === null) return null;

  // Decoded rather than switched on raw, for the reason api/enums.ts gives: a
  // controller ahead of this build reports a status the union does not have,
  // and it has to render as something rather than falling through every case.
  const status = decodeEnum(licence.status, licenceStatuses);
  const { tone, label, detail } = describe(licence, status);
  // Beside the status rather than inside it, because it answers a different
  // question and is true or false independently of every one of the five.
  const allowance = describeAllowance(licence.allowance);

  return (
    <p className="licence" role="status" data-testid="licence-badge">
      <span className={`chip chip-${tone}`}>{label}</span>
      {detail !== null && <span className="licence-detail">{detail}</span>}
      {allowance !== null && (
        <span className="licence-detail" data-testid="licence-allowance">
          {allowance}
        </span>
      )}
    </p>
  );
}

interface Rendering {
  /** The chip's colour, from the four index.css already defines. */
  tone: "good" | "warn" | "bad" | "muted";
  label: string;
  detail: ReactNode;
}

function describe(
  licence: Licence,
  status: LicenceStatus | "unknown",
): Rendering {
  switch (status) {
    case "valid": {
      if (licence.expiresAt === null)
        return {
          tone: "good",
          label: "licence valid",
          detail: `${licence.tier}, perpetual`,
        };

      // Warn *before* it lapses. A licence four days from expiry used to read
      // green, and the first thing its expiry takes is SSO — which locks out
      // the operators who would have noticed (#233). The controller logs the
      // same window, because the deployment that needs this most is a daemon
      // nobody is looking at.
      const days = daysUntil(licence.expiresAt);
      if (days !== null && days <= expiryWarningDays) {
        return {
          tone: "warn",
          label: "licence expiring",
          detail: (
            <>
              {licence.tier},{" "}
              {days <= 0 ? "expires today" : `${plural(days, "day")} left`} —
              renew now (
              <Instant at={licence.expiresAt} />)
            </>
          ),
        };
      }
      return {
        tone: "good",
        label: "licence valid",
        detail: (
          <>
            {licence.tier}, expires <Instant at={licence.expiresAt} />
          </>
        ),
      };
    }
    case "grace":
      // Warn rather than bad: the features are still on, and colouring this the
      // same as a build that has already lost them would leave nothing to
      // escalate to when it does.
      //
      // "lapsed" rather than "expired", and the day they stop rather than only
      // the day something ran out — see the two states named at the top of this
      // file. Without `featuresOffAt` this reads exactly as it did before the
      // field existed, which is what an older controller gets.
      return {
        tone: "warn",
        label: "grace period",
        detail: (
          <>
            {agoWhen(licence.expiresAt, "lapsed")}, still granting features
            {featuresStop(licence.featuresOffAt)} — renew now
          </>
        ),
      };
    case "expired":
      return {
        tone: "bad",
        label: "licence expired",
        detail: `${agoWhen(licence.expiresAt, "expired")}, features are off`,
      };
    case "invalid":
      // The remedy is a licence from the vendor rather than a change to this
      // deployment, and the badge says which so that nobody spends an afternoon
      // on the controller's configuration.
      return {
        tone: "bad",
        label: "licence invalid",
        detail: "it did not verify — ask for a replacement",
      };
    case "absent":
      // Nothing is broken, and the reader's own action turns it on. Two states
      // share this status and the copy has to fit both: no licence installed at
      // all, and a managed one that is installed, verifies, and has not been
      // activated for this swarm. "install one" alone was written for the first
      // and sends the second's reader after a key already in their hand.
      //
      // Naming both actions still left one route, and it is the online one:
      // activating a managed licence normally means asking the licence service.
      // The operator most likely to be reading this cannot — an air-gapped
      // swarm is exactly where an activation is not automatic — and the lease
      // file they were sent is the path they have. Both halves fit both states,
      // which is the constraint this status puts on every word here.
      return {
        tone: "warn",
        label: "no active licence",
        detail: (
          <>
            install one, or activate a managed one, to turn the licensed
            features on; an air-gapped swarm activates with{" "}
            <code>:license lease install &lt;file&gt;</code>
          </>
        ),
      };
    default:
      return {
        tone: "muted",
        label: `licence ${licence.status}`,
        detail: "a status this build does not know",
      };
  }
}

/**
 * expiryWarningDays is how long before expiry the badge stops reading healthy.
 *
 * Two weeks, matching the controller's own warning window
 * (`controller/licencewatch.go`): the two surfaces answer the same question and
 * an operator who saw one and not the other would be told two different things
 * about the same date. They are separate constants because they are separate
 * programs — a shared one would have to travel on the wire, and this is not a
 * fact about the deployment.
 */
const expiryWarningDays = 14;

/**
 * daysUntil is whole days from now until `at`, rounded down, or null when the
 * controller sent something that is not a date. Rounding down is what makes
 * "0 days left" mean the last day rather than a day that has already gone.
 */
function daysUntil(at: string): number | null {
  const parsed = Date.parse(at);
  if (Number.isNaN(parsed)) return null;
  return Math.floor((parsed - Date.now()) / 86_400_000);
}

/**
 * agoWhen says how long ago, in whole days, under the reader's own word for it.
 *
 * The word is a parameter because the two statuses that call this do not mean
 * the same thing by the date. "expired" is right for a licence that is off; it
 * is wrong for half of "grace", where the licence verifies and has not expired
 * and only its activation renewal is late. "lapsed" is true of both of those.
 *
 * Whole days rather than an exact instant because the reader's question is how
 * much time is left, not when the certificate was minted; the exact value is a
 * click away in the capability document. A licence with no date under one of
 * these statuses is a contradiction the controller sent, so it says only the
 * bare word rather than inventing a day for it.
 *
 * A date in the *future* is the same contradiction and used to be the one that
 * read plausibly: flooring a negative age gave "expired today", so a licence
 * that lapsed a month ago and one still inside its window rendered
 * identically, and both read as though it had just happened. That is what a
 * companion sending the wrong one of two clocks looks like from here, and
 * hedging is the honest answer to it — this build cannot know which of them the
 * controller meant.
 */
function agoWhen(at: string | null, word: string): string {
  if (at === null) return word;
  const parsed = Date.parse(at);
  if (Number.isNaN(parsed) || parsed > Date.now()) return word;
  const days = Math.floor((Date.now() - parsed) / 86_400_000);
  return days <= 0 ? `${word} today` : `${word} ${plural(days, "day")} ago`;
}

/**
 * featuresStop is the day the grant actually ends, or nothing when the
 * controller did not send one.
 *
 * Nothing, rather than a hedge, because the sentence around it is already true
 * without this clause: a controller too old to carry the field renders the line
 * it always rendered instead of growing a phrase about a date nobody has.
 */
function featuresStop(at: string | null | undefined): ReactNode {
  if (at === null || at === undefined) return null;
  const days = daysUntil(at);
  if (days === null) return null;
  return (
    <>
      {" — they stop "}
      {days <= 0 ? "today" : `in ${plural(days, "day")}`} (<Instant at={at} />)
    </>
  );
}

/**
 * describeAllowance renders the issuer's advisory report, or nothing.
 *
 * Only when the issuer says the deployment is over: a swarm inside its
 * allowance has nothing to be told, and a badge that recited "2 of 3 nodes" on
 * every healthy licence is a badge an operator stops reading. The verdict is
 * the issuer's own rather than a comparison made here — see feature.Allowance —
 * so this reads `overLimit` and never the two counts against each other.
 *
 * It says what will happen rather than that something has: nothing is switched
 * off, and the thing that eventually goes wrong is a term that stops being
 * renewed on a date months away. That date is the whole point of showing it.
 */
function describeAllowance(
  allowance: Allowance | null | undefined,
): ReactNode {
  if (allowance === null || allowance === undefined || !allowance.overLimit)
    return null;

  // Both counts or neither: a zero is the issuer saying nothing, and "4 nodes"
  // without the allowance beside it tells an operator something they know.
  const counts =
    allowance.nodes > 0 && allowance.maxNodes > 0
      ? `${allowance.nodes} of ${allowance.maxNodes} nodes`
      : "over the node allowance";

  return (
    <>
      {counts}, as the licence service sees it — the term{" "}
      {allowance.termEndsAt === null ? (
        "will stop rolling"
      ) : (
        <>
          stops rolling <Instant at={allowance.termEndsAt} />
        </>
      )}{" "}
      unless the count comes down
    </>
  );
}
