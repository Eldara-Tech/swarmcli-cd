// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package compose

import "testing"

// digest is a real sha256, because a short one is not a reference at all:
// ParseNormalizedNamed rejects it and the normalisation declines, so a test
// using "@sha256:aaaa" for a pinned manifest would prove the wrong thing.
const digest = "sha256:0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"

func TestImageTagStripsTheDigest(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":                        "nginx",
		"nginx:1.2":                    "nginx:1.2",
		"nginx:1.2@sha256:aaaa":        "nginx:1.2",
		"ghcr.io/team/app@sha256:aaaa": "ghcr.io/team/app",
		"":                             "",
	} {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// What the client does to an image on the way to the daemon, which is what the
// daemon then stores and reads back. Executed against the real
// distribution/reference rather than asserted from the docs: these four
// behaviours are the whole of why the desired side needs normalising at all.
func TestNormalisedImageIsWhatTheDaemonStores(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":                           "nginx:latest",
		"nginx:1.25":                      "nginx:1.25",
		"docker.io/library/nginx:1.25":    "nginx:1.25",
		"index.docker.io/library/redis:7": "redis:7",
		"myreg.io/foo/bar":                "myreg.io/foo/bar:latest",
		"myreg.io:5000/foo/bar":           "myreg.io:5000/foo/bar:latest",
		// TagNameOnly adds no tag to a canonical reference, so a manifest that
		// pinned a digest keeps it and loses only the default registry.
		"docker.io/library/nginx@" + digest: "nginx@" + digest,
		// Not a reference: returned as written, the way the client's own call
		// site leaves it, so the daemon is the one to say why.
		"NOT A REFERENCE": "NOT A REFERENCE",
		"":                "",
	} {
		if got := normalisedImage(in); got != want {
			t.Errorf("normalisedImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// The predicate the applier and the drift reporter share, in both directions.
// A false negative here is a service redeployed on every tick for ever; a false
// positive is an image somebody changed by hand being written back as ours.
func TestSameImage(t *testing.T) {
	for name, tc := range map[string]struct {
		live, wanted string
		same         bool
	}{
		"written as written":                {"nginx:1.2", "nginx:1.2", true},
		"resolved to a digest":              {"nginx:1.2@sha256:aaaa", "nginx:1.2", true},
		"tagged latest by the client":       {"nginx:latest", "nginx", true},
		"tagged and then resolved":          {"nginx:latest@sha256:aaaa", "nginx", true},
		"familiarised by the client":        {"nginx:1.25", "docker.io/library/nginx:1.25", true},
		"private registry tagged latest":    {"myreg.io/foo/bar:latest", "myreg.io/foo/bar", true},
		"pinned by digest in the manifest":  {"nginx@" + digest, "docker.io/library/nginx@" + digest, true},
		"a different tag":                   {"nginx:9.9", "nginx:1.2", false},
		"a different tag under a digest":    {"nginx:9.9@sha256:bbbb", "nginx:1.2", false},
		"a different repository":            {"evil.io/nginx:latest", "nginx", false},
		"changed behind an unstated tag":    {"nginx:9.9@sha256:bbbb", "nginx", false},
		"repinned by hand to a bare digest": {"nginx@" + digest, "nginx", false},
		"nothing running yet":               {"", "nginx", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := SameImage(tc.live, tc.wanted); got != tc.same {
				t.Errorf("SameImage(%q, %q) = %v, want %v", tc.live, tc.wanted, got, tc.same)
			}
		})
	}
}
