// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package compose

import (
	"strings"

	"github.com/distribution/reference"
)

// SameImage reports whether a live spec's image is still the one a manifest
// asked for.
//
// One definition and two callers, because the two are the same question asked
// for different reasons. backend asks it of the stack image label, to decide
// whether the digest sitting in the live spec is the one our own tag resolved
// to; drift asks it of the desired spec, to decide whether somebody ran
// `docker service update --image`. A copy in each is two places to fix a rule,
// and the two disagreeing is a controller that reports drift, redeploys, and
// reports the same drift again.
//
// Neither side is the string that was written, for two separate reasons:
//
//   - The daemon appends the digest it resolved the tag to, so the live side is
//     matched both whole and with a digest stripped. Whole is what covers a
//     manifest that pinned a digest itself.
//   - The client rewrites what it sends. imageWithTagString runs
//     reference.FamiliarString(reference.TagNameOnly(ref)) over the image on the
//     way into every ServiceCreate and ServiceUpdate — unconditionally, before
//     the request is built and whatever QueryRegistry says (docker
//     client/service_create.go, client/service_update.go) — so `nginx` is stored
//     as `nginx:latest` and `docker.io/library/nginx:1.25` as `nginx:1.25`.
//     Nothing rewrites the manifest's side, so it is done here, through the same
//     two functions rather than by hand: a second implementation is a second
//     thing that can disagree with the daemon about what a reference means.
//
// Only the wanted side is normalised, which is the half that matters. Doing it
// to the live side as well would strip the digest off `nginx@sha256:…`, default
// what was left to `nginx:latest`, and call an image somebody pinned by hand a
// match for `nginx` — the one drift a converge must not be blind to.
func SameImage(live, wanted string) bool {
	wanted = normalisedImage(wanted)
	return live == wanted || imageTag(live) == wanted
}

// normalisedImage is the reference the daemon will store for the one a manifest
// named.
//
// An unparseable reference is returned unchanged, which is the client's own
// handling of it: imageWithTagString returns the empty string and the call site
// then leaves the image alone, so what reaches the daemon is what was written
// and the daemon is left to say why it is not a reference.
func normalisedImage(image string) string {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return image
	}
	return reference.FamiliarString(reference.TagNameOnly(ref))
}

// imageTag strips a digest suffix. LastIndex rather than Cut so a reference
// containing more than one "@" loses only the digest.
func imageTag(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		return image[:i]
	}
	return image
}
