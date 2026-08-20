// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/distribution/reference"

	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
)

// whyOlder is the fifth loss rejectSelfLoss refuses, and the only one that takes
// nothing away from the running spec.
//
// The other four are things the manifest drops. This one is a thing it adds: a
// controller older than the one applying it, which may not understand the app
// set it inherits. The version that added `self:` is the worked example —
// anything before it refuses a file naming the key, keeps its last-good set and
// stops following git — and the remedy for that is not a commit, because
// nothing is left reading them.
const whyOlder = "the manifest takes this controller's image back to '%s' from '%s'. An older controller " +
	"may not understand the app set it inherits — the build that added `self:` is refused by every build " +
	"before it — and a controller that cannot read its app set cannot apply the commit that would put this " +
	"back. Name the version you mean in the values this application deploys from"

// checkSelfVersion refuses a self manifest that would take this controller
// backwards.
//
// It is deliberately narrow. Only a strict downgrade of the *same repository*,
// with both references carrying a tag that parses as a semantic version, is
// refused; everything else — a different repository, a digest, `latest`, a date
// stamp, a build id — is logged and applied. Refusing what cannot be ordered
// would break every operator who tags their own builds, and this is not a
// compatibility check: it cannot know whether the next version can read the app
// set. It knows that backwards is the direction where the answer can only get
// worse, and that this is the one deploy whose mistake removes the thing that
// would correct it.
//
// The warning is only for an image that actually changes. A self release
// redeploys on every drift correction, and a controller pinned by digest would
// otherwise be told the same thing about the same unchanged reference for ever.
func (b *Backend) checkSelfVersion(service, from, to string) error {
	if from == "" || to == "" || cdcompose.SameImage(from, to) {
		return nil
	}
	fromRepo, fromV, fromOK := imageVersion(from)
	toRepo, toV, toOK := imageVersion(to)
	if !fromOK || !toOK || fromRepo != toRepo {
		b.log.Warn("this release changes this controller's own image to one that cannot be ordered against "+
			"the running one, so nothing here can tell an upgrade from a downgrade",
			"service", service, "from", from, "to", to)
		return nil
	}
	if toV.LessThan(fromV) {
		return selfLoss(service, fmt.Sprintf(whyOlder, to, from))
	}
	return nil
}

// imageVersion reads a reference's repository and the semantic version its tag
// names.
//
// The digest is dropped first: a live spec carries the one the daemon resolved
// the tag to, and it is the tag that says which build this is. A reference with
// no tag, or a tag that is not a version, is not comparable and says so — the
// caller warns rather than refusing, so this returning false must never be
// mistaken for "not a downgrade".
func imageVersion(image string) (repo string, v *semver.Version, ok bool) {
	ref, err := reference.ParseNormalizedNamed(withoutDigest(image))
	if err != nil {
		return "", nil, false
	}
	tagged, isTagged := ref.(reference.Tagged)
	if !isTagged {
		return "", nil, false
	}
	v, err = semver.NewVersion(tagged.Tag())
	if err != nil {
		return "", nil, false
	}
	return reference.FamiliarName(ref), v, true
}

// withoutDigest strips a digest suffix. LastIndex rather than Cut, so a
// reference containing more than one "@" loses only the digest — the same rule
// compose.imageTag applies, and unexported there.
func withoutDigest(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		return image[:i]
	}
	return image
}
