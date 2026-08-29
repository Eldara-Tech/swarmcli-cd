// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package feature reports what a build grants, for display.
//
// It is the sixth seam, and it clears the bar docs/extensibility.md sets: a
// behaviour that genuinely differs between editions and that no existing seam
// can express. authz decides a request and notify carries an event; neither can
// answer "what does this build grant" — which is the question a licence badge,
// a hidden nav item and a disabled control are all asking.
//
// # A report, never an enforcement point
//
// Nothing in this repository reads a Set to decide whether to do something, and
// nothing may. A Reporter is supplied by the very module a gate would be
// constraining, so gating on one asks the licensed module for permission to
// limit it: a companion that reported everything true would have granted itself
// everything, and the check would read like a licence check while being the
// opposite of one. Entitlement is enforced where seam's "Entitlement gating is
// not a re-registration" says it is — from inside the implementation whose
// entitlement lapsed, which is the only place that holds both the licence and
// the operation.
//
// So api is the only consumer of Get, and feature_test.go asserts that from the
// source rather than trusting this paragraph: the comment is what somebody
// reads once, and the test is what fails the day somebody does not.
//
// # A Slot, not a List
//
// Against seam's own stated test for the two shapes:
//
//   - There is exactly one answer to "what does this build grant". A List needs
//     a composition rule, and the only rule available for a set of booleans is
//     OR — under which one module's report turns on another module's feature.
//     That is an entitlement hole, and it is why authz is a Slot too.
//   - A List has no removal, and a report must be able to say no: a licensed
//     reporter whose licence lapses has to be able to go back to reporting
//     community.
//   - A Slot is settled by the end of init(), so api.New reads Get once and
//     keeps the result, exactly as it already does for the authorizer. What
//     settles is *who answers*; the answer itself is recomputed per request,
//     because Report is called per request and never cached.
//
// The default below registers from this package's own init(), so the
// swarms/local exception — "a default that is not in the seam package does not
// overwrite" — does not apply here: a companion has to import this package to
// name Register, so its init() is guaranteed to run after this one and win.
package feature

import (
	"context"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/seam"
)

// Name identifies one capability a build may grant.
//
// A string type rather than an enumeration with a fixed range, for the reason
// authz.Action gives: the list grows, and a reporter implemented outside this
// repository must keep compiling when it does.
type Name string

const (
	// MultiSwarm is resolving destinations to more than the swarm the
	// controller runs in.
	MultiSwarm Name = "multi-swarm"
	// SSO is authenticating a browser against an identity provider rather than
	// against the shared admin token.
	SSO Name = "sso"
	// Projects is scoping applications to tenants, and the RBAC that goes with
	// it.
	Projects Name = "projects"
	// Audit is the recorded, queryable history of who asked for what.
	Audit Name = "audit"
	// Notifications is delivering events somewhere other than the log and the
	// event stream.
	Notifications Name = "notifications"
)

// All names every feature this repository knows about, in the order a document
// should list them.
//
// It exists so that the key set of the capability document is decided here
// rather than by whatever a reporter happened to put in its map. A UI that
// hides a control on features["sso"] has to be able to tell false from absent,
// and a document whose keys move with the reporter cannot: a reporter that
// dropped a key would read exactly like one that reported the feature off, and
// the control would disappear rather than grey out.
func All() []Name { return []Name{MultiSwarm, SSO, Projects, Audit, Notifications} }

// Set is what a build grants, by name.
//
// A nil Set reads false for every name, which is exactly what a build granting
// nothing is — so a reporter with nothing to grant need not build a map.
type Set map[Name]bool

// Status is the state of a licence, as a badge renders it.
//
// Five values, per D25. The set is frozen here even though the mapping onto it
// lives in the companion: the moment this package ships, Status is public API
// that a UI in this repository switches on.
type Status string

const (
	// StatusValid is a licence that verified and has not expired. Nothing to
	// do.
	StatusValid Status = "valid"
	// StatusGrace is a licence that is past a deadline and still granting
	// features, for a bounded period. Renew before FeaturesOffAt, or the build
	// silently becomes the community one.
	//
	// Two states share it, and copy written for this value has to fit both: a
	// licence whose own term ran out and is inside its grace, and a managed
	// licence that verifies and has not expired at all, whose activation
	// renewal is late. Only the first has expired, so a message asserting an
	// expiry is false for the second and sends its reader after a replacement
	// licence when what they need is to sync the activation.
	//
	// Its own value rather than folded into valid, because collapsing it means
	// a badge cannot say "still granting features, and they stop on Tuesday" —
	// which is the single most useful thing a licence badge can say. Working
	// that out from ExpiresAt is not open to a consumer here: the period is the
	// licensed module's and is not the same one in the two states above, which
	// is why FeaturesOffAt is on the wire rather than the arithmetic.
	StatusGrace Status = "grace"
	// StatusExpired is a licence that verified and is past every grace it had.
	// The features are already off; renew to get them back.
	StatusExpired Status = "expired"
	// StatusInvalid is a licence that did not verify — a bad signature, a
	// corrupt file, or one issued for a different swarm. The remedy is a
	// licence from the vendor rather than a change to this deployment, and a
	// companion can say which of those it was through the reserved
	// /api/v1/licence path.
	//
	// A licence bound to another cluster is folded in here on purpose: its
	// remedy genuinely differs, but a sixth value that the Apache-2.0 build can
	// never produce is a worse cost than the ambiguity.
	StatusInvalid Status = "invalid"
	// StatusAbsent is a build with a licensed module linked and nothing
	// granting, where the operator's own action is what turns it on and
	// nothing is broken.
	//
	// Two states share it, and a message written for this value has to fit
	// both: no licence installed at all, and — since managed licensing — a
	// licence that is installed, verifies, and has not been activated for this
	// swarm. They share a value for the reason given against StatusInvalid: a
	// sixth that an Apache-2.0 build could never produce is a worse cost than
	// the ambiguity. What it costs here is the word "install", which is only
	// half the remedy.
	//
	// No other field separates them, and FeaturesOffAt and Allowance did not
	// change that: the first is nil under both, because nothing is running out
	// where nothing is granting, and the second is the issuer's answer to a
	// different question entirely. So a surface serves both by saying what is
	// true of both, never by branching on a field to pick a remedy.
	StatusAbsent Status = "absent"
)

// Licence is what a badge renders, and deliberately not the licence record.
//
// The full record — who it was issued to, which swarm it is bound to, what it
// entitles — is the companion's to serve through the reserved /api/v1/licence
// path. What is here is what a badge needs to draw itself and nothing an
// operator has to be authorised twice for.
type Licence struct {
	// Tier is the licence's product tier, as the companion names it: "be",
	// "trial". Free text to this repository, which never branches on it.
	Tier string `json:"tier"`
	// Status is what the badge says.
	Status Status `json:"status"`
	// ExpiresAt is when the licence stops being valid, or nil when it is
	// perpetual or when there is no licence to expire.
	//
	// No omitempty: an explicit null is what tells a badge "this licence does
	// not expire" apart from a document it failed to parse.
	ExpiresAt *time.Time `json:"expiresAt"`
	// FeaturesOffAt is the other end of that window: when this build actually
	// stops granting what the licence grants. Nil when nothing is running out —
	// a valid licence, a perpetual one, one that has already stopped — and nil
	// from a companion built before this field existed.
	//
	// A second date rather than a period, because ExpiresAt plus a period is
	// arithmetic no consumer here can do. StatusGrace folds two windows whose
	// lengths have nothing in common: a token's own grace, counted from its
	// expiry, and a managed licence's lease, which ends on a date the lease
	// carries and nothing derives. A badge given one date and a guessed period
	// would be right for one of them and confidently wrong for the other. What
	// that costs an operator is the whole of this field: "still granting
	// features" does not say whether that is one more day or twenty-six.
	//
	// No omitempty, for ExpiresAt's reason: an explicit null says "nothing is
	// running out", and a missing key is a document that did not parse.
	FeaturesOffAt *time.Time `json:"featuresOffAt"`
	// Allowance is what the licence *issuer* last said about this deployment's
	// size, or nil when it has said nothing — which is the ordinary state.
	//
	// Advisory, and the one thing here that may only ever be rendered. See
	// Allowance.
	//
	// No omitempty, for the reason above.
	Allowance *Allowance `json:"allowance"`
}

// Allowance is the licence issuer's own report about this deployment's size, as
// the companion last received it.
//
// # It is unsigned, and nothing may branch on it
//
// Every other field on Licence is derived from a document the companion
// verified against a key compiled into the binary. This one is not: it is what
// a server said over a network, and a server is exactly what one firewall rule
// or one /etc/hosts line can stand in for. So it may be rendered and it may be
// logged, and nothing anywhere may grant, deny, hide or disable on it — a build
// that switched a feature off on an unsigned status would be switchable off by
// anyone able to answer a request, and one that switched a feature *on* would
// be worse. Enforcement is unchanged and stays where it already is: the signed
// token's expiry and the lease window, judged offline inside the licensed
// module. swarmcli-be's license/entitlement.go is the fuller argument, and the
// reason this is a type of its own with no path into any status.
//
// The issuer's verdict travels rather than the comparison, for the same reason:
// a renderer weighing Nodes against MaxNodes would be a second implementation of
// a judgement that belongs to whoever made it, and it would disagree the first
// time the issuer's rule changed.
type Allowance struct {
	// OverLimit is the issuer's verdict, and the only question to ask of this
	// block. False is also what a verdict this build has no word for reads as:
	// a status added upstream is not grounds to warn an operator about
	// something this build cannot name.
	OverLimit bool `json:"overLimit"`
	// Nodes is the count the issuer last recorded for this licence, and
	// MaxNodes the allowance it compared that count against. Zero means the
	// issuer said nothing — never a swarm with no nodes, and never an allowance
	// of none — so a surface holding a zero says less rather than rendering
	// "0 of 0".
	Nodes    int `json:"nodes"`
	MaxNodes int `json:"maxNodes"`
	// TermEndsAt is when the issuer stops rolling this licence's term forward,
	// or nil when there is no such date to name. It is most of the value of
	// this block: a cap judged at the issuer is otherwise invisible until a
	// term quietly fails to renew, which under a year-long term is a year of
	// silence ending in the features switching off.
	TermEndsAt *time.Time `json:"termEndsAt"`
}

// Report is one answer to "what does this build grant".
type Report struct {
	// Edition is what the build calls itself: "community" for the Apache-2.0
	// default, "business" once a licence verifies.
	Edition string
	// Features is what is granted. A consumer must not take its keys as the
	// list of features — see All.
	Features Set
	// Licence is nil in a build with no licensed module linked, which is a
	// different thing from a licensed build with no licence installed: that one
	// reports StatusAbsent.
	Licence *Licence
}

// EditionCommunity is what the default below reports, and what a licensed build
// reports until its licence verifies.
const EditionCommunity = "community"

// Reporter answers what the build grants.
type Reporter interface {
	// Report is called per request and its result is never cached, so an
	// entitlement that lapses stops being reported without a restart. An
	// implementation that has to reach for the licence should hold its own
	// cache and say so; a consumer cannot hold one for it, because it has no
	// way to know when it went stale.
	Report(ctx context.Context) Report
}

var slot seam.Slot[Reporter]

// Register installs r as the reporter, replacing whatever was there. Call it
// from an init().
func Register(name string, r Reporter) { slot.Register(name, r) }

// Get returns the reporter in force.
//
// api is its only caller. See the package comment, and feature_test.go.
func Get() Reporter { return slot.Get() }

// Active names the reporter in force, for startup logging.
//
// It is the diagnostic the capability document's seams.feature carries: under
// D20 the licensed reporter is always linked, so this reads "licence" while the
// edition still reads "community" until a licence verifies. That pair is what
// tells "the module is not loaded" apart from "the module is loaded and the
// licence is missing", which no single field can.
func Active() string { return slot.Name() }

func init() { Register(EditionCommunity, community{}) }

// community is what an Apache-2.0 build grants: nothing licensed, and no
// licence to report.
//
// It lives here rather than in a package of its own because there is no
// implementation to keep out of anyone's import graph — the reason swarms/local
// exists — and putting it here is what makes the ordering argument in seam's
// doc comment hold without exception.
type community struct{}

// Report implements Reporter. The nil Set is not an omission: it reads false
// for every name, and the document's keys come from All rather than from here.
func (community) Report(context.Context) Report {
	return Report{Edition: EditionCommunity}
}
