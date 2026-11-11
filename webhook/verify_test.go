// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// The two secrets every test configures. The signing one is in GitLab's own
// shape, whsec_ and base64, and it is the second in the list so that every
// "second secret" row proves more than one is tried.
const (
	githubSecret  = "github-hook-secret"
	signingSecret = "whsec_c2lnbmluZy1rZXktYnl0ZXMtZm9yLXRoZS10ZXN0"
)

var testSecrets = newSecrets([]string{githubSecret, signingSecret})

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func hubSignature(secret string, body []byte) string {
	return "sha256=" + hex.EncodeToString(hmacSHA256([]byte(secret), body))
}

// standardSignature signs as GitLab does from 19.0.
func standardSignature(token, id, ts string, body []byte) string {
	key, err := base64.StdEncoding.DecodeString(token[len("whsec_"):])
	if err != nil {
		panic(err)
	}
	return "v1," + base64.StdEncoding.EncodeToString(hmacSHA256(key, append([]byte(id+"."+ts+"."), body...)))
}

var testNow = time.Unix(1_800_000_000, 0)

func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestGitHubVerification(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	bare := hex.EncodeToString(hmacSHA256([]byte(githubSecret), body))

	for _, tc := range []struct {
		name string
		h    http.Header
		want error
	}{
		{"GitHub's header", headers("X-Hub-Signature-256", hubSignature(githubSecret, body)), nil},
		{"the second secret", headers("X-Hub-Signature-256", hubSignature(signingSecret, body)), nil},
		{"Gitea's bare hex", headers("X-Gitea-Signature", bare), nil},
		{"Forgejo's bare hex", headers("X-Forgejo-Signature", bare), nil},
		{"a secret nobody configured", headers("X-Hub-Signature-256", hubSignature("guessed", body)), errNoMatch},
		{"a signature over another body", headers("X-Hub-Signature-256", hubSignature(githubSecret, []byte(`{}`))), errNoMatch},
		{"no signature at all", headers(), errUnsigned},
		{"the SHA-1 header's prefix", headers("X-Hub-Signature-256", "sha1="+bare), errMalformed},
		{"not hex", headers("X-Hub-Signature-256", "sha256=zz"), errMalformed},
		// GitHub's header is the one checked when it is there, so a delivery
		// cannot pick the check it faces by sending two.
		{"a bad GitHub header beside a good Gitea one", headers(
			"X-Hub-Signature-256", hubSignature("guessed", body), "X-Gitea-Signature", bare), errNoMatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyGitHub(testSecrets, tc.h, body, testNow); !errors.Is(got, tc.want) {
				t.Errorf("verifyGitHub = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGitLabVerification(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	const id = "msg_2b3c"
	at := func(d time.Duration) string { return strconv.FormatInt(testNow.Add(d).Unix(), 10) }
	ts := at(0)
	good := standardSignature(signingSecret, id, ts, body)
	signed := func(sig, ts string) http.Header {
		return headers("Webhook-Id", id, "Webhook-Timestamp", ts, "Webhook-Signature", sig)
	}

	for _, tc := range []struct {
		name string
		h    http.Header
		want error
	}{
		{"a signing token", signed(good, ts), nil},
		{"one of several signatures", signed("v1,bm90LWl0 "+good, ts), nil},
		{"a signature of another version", signed("v2,"+good[len("v1,"):], ts), errNoMatch},
		{"a key nobody configured", signed(standardSignature("whsec_Z3Vlc3NlZA==", id, ts, body), ts), errNoMatch},
		// The id and the timestamp are signed along with the body, so neither
		// can be swapped for a fresh one on a captured delivery.
		{"another delivery's id", headers("Webhook-Id", "msg_other", "Webhook-Timestamp", ts, "Webhook-Signature", good), errNoMatch},
		{"a timestamp moved forward", signed(good, at(time.Second)), errNoMatch},
		{"within the tolerance", signed(standardSignature(signingSecret, id, at(-4*time.Minute), body), at(-4*time.Minute)), nil},
		{"older than the tolerance", signed(standardSignature(signingSecret, id, at(-6*time.Minute), body), at(-6*time.Minute)), errStale},
		{"later than the tolerance", signed(standardSignature(signingSecret, id, at(6*time.Minute), body), at(6*time.Minute)), errStale},
		{"no id", headers("Webhook-Timestamp", ts, "Webhook-Signature", good), errMalformed},
		{"a timestamp that is not a number", signed(good, "yesterday"), errMalformed},
		// Signed means held to the signature: a hook mid-migration sends both,
		// and the weaker of the two must not rescue a signature that failed.
		{"a bad signature beside a good token", headers(
			"Webhook-Id", id, "Webhook-Timestamp", ts, "Webhook-Signature", "v1,bm90LWl0", "X-Gitlab-Token", githubSecret), errNoMatch},
		{"a plain token", headers("X-Gitlab-Token", githubSecret), nil},
		{"a plain token, the second secret", headers("X-Gitlab-Token", signingSecret), nil},
		{"a token nobody configured", headers("X-Gitlab-Token", "guessed"), errTokenNoMatch},
		{"neither", headers(), errUnsigned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyGitLab(testSecrets, tc.h, body, testNow); !errors.Is(got, tc.want) {
				t.Errorf("verifyGitLab = %v, want %v", got, tc.want)
			}
		})
	}
}
