// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// tolerance is how far a signed GitLab delivery's timestamp may be from this
// controller's clock. The timestamp is inside the signature, so this is what
// stops a captured delivery being replayed for ever; five minutes is the
// Standard Webhooks recommendation, and wide enough for ordinary clock skew.
const tolerance = 5 * time.Minute

// The reasons a delivery is refused. They are logged at debug and never sent
// back: the forge's delivery log is readable by whoever administers the
// repository, and which check failed is a hint to somebody guessing.
var (
	errUnsigned     = errors.New("no signature or token")
	errMalformed    = errors.New("the signature is malformed")
	errNoMatch      = errors.New("not signed with a configured secret")
	errStale        = errors.New("the signed timestamp is more than five minutes from this controller's clock")
	errTokenNoMatch = errors.New("the token is not a configured secret")
)

// secrets holds the configured secrets twice: as written, which is the HMAC key
// GitHub signs with and what GitLab's plain token is compared against, and as
// the key a Standard Webhooks signature is made with.
type secrets struct {
	raw [][]byte
	// signing is each secret with GitLab's whsec_ prefix stripped and the rest
	// base64-decoded, which is how GitLab derives the key from the token it
	// shows. A GitHub secret or a plain GitLab token usually does not decode and
	// then has no signing key; one that happens to decode is a key nothing signs
	// with, which costs one HMAC per delivery and matches nothing.
	signing [][]byte
}

func newSecrets(list []string) secrets {
	var s secrets
	for _, secret := range list {
		s.raw = append(s.raw, []byte(secret))
		if key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_")); err == nil && len(key) > 0 {
			s.signing = append(s.signing, key)
		}
	}
	return s
}

// verifyGitHub checks the HMAC-SHA256 of the body that GitHub, GitHub
// Enterprise, Gitea and Forgejo all send. GitHub's header is read first and the
// other two, which carry the same HMAC in bare hex, only without it. The first
// one present is the one checked: falling through to the next after a failure
// would only let a delivery choose which check it faces.
//
// There is no timestamp in this scheme, so a captured delivery can be replayed.
// What a replay can do is ask for a refresh the floor already rate-limits.
func verifyGitHub(s secrets, h http.Header, body []byte, _ time.Time) error {
	var sig string
	switch {
	case h.Get("X-Hub-Signature-256") != "":
		v, ok := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
		if !ok {
			return errMalformed
		}
		sig = v
	case h.Get("X-Gitea-Signature") != "":
		sig = h.Get("X-Gitea-Signature")
	case h.Get("X-Forgejo-Signature") != "":
		sig = h.Get("X-Forgejo-Signature")
	default:
		return errUnsigned
	}
	mac, err := hex.DecodeString(sig)
	if err != nil {
		return errMalformed
	}
	if !signedBy(s.raw, body, mac) {
		return errNoMatch
	}
	return nil
}

// verifyGitLab checks a signed delivery when there is a signature, and the
// plain X-Gitlab-Token otherwise.
//
// GitLab from 19.0 signs with a per-hook signing token, following Standard
// Webhooks, and older versions — or a hook nobody has moved over — send a
// shared token in the clear. A delivery carrying a signature is held to it and
// never falls back to the token: a hook that has both is mid-migration, and the
// signature is the stronger of the two.
func verifyGitLab(s secrets, h http.Header, body []byte, now time.Time) error {
	if sigs := h.Get("Webhook-Signature"); sigs != "" {
		return verifyStandard(s.signing, h, body, sigs, now)
	}
	token := h.Get("X-Gitlab-Token")
	if token == "" {
		return errUnsigned
	}
	match := false
	// Every secret is compared whatever the earlier ones answered, so the time
	// taken says nothing about which one matched.
	for _, secret := range s.raw {
		if subtle.ConstantTimeCompare([]byte(token), secret) == 1 {
			match = true
		}
	}
	if !match {
		return errTokenNoMatch
	}
	return nil
}

// verifyStandard checks a Standard Webhooks signature: an HMAC-SHA256 over the
// delivery id, the timestamp and the body, base64-encoded and versioned "v1,",
// with several allowed in one header separated by spaces.
func verifyStandard(keys [][]byte, h http.Header, body []byte, sigs string, now time.Time) error {
	id, ts := h.Get("Webhook-Id"), h.Get("Webhook-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if id == "" || err != nil {
		return errMalformed
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return errStale
	}

	msg := append([]byte(id+"."+ts+"."), body...)
	match := false
	for _, sig := range strings.Fields(sigs) {
		version, b64, ok := strings.Cut(sig, ",")
		if !ok || version != "v1" {
			continue
		}
		mac, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		if signedBy(keys, msg, mac) {
			match = true
		}
	}
	if !match {
		return errNoMatch
	}
	return nil
}

// signedBy reports whether mac is the HMAC-SHA256 of msg under one of keys,
// comparing in constant time and trying every key whatever the earlier ones
// answered.
func signedBy(keys [][]byte, msg, mac []byte) bool {
	match := false
	for _, key := range keys {
		m := hmac.New(sha256.New, key)
		m.Write(msg)
		if hmac.Equal(m.Sum(nil), mac) {
			match = true
		}
	}
	return match
}
