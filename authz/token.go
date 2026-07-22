// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package authz

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Environment variables the default authorizer reads. The file form exists
// because a Docker secret arrives as a file: in Swarm it is encrypted at rest
// in the raft log and delivered in memory, which the string form gives up.
const (
	EnvToken     = "SWARMCLI_CD_ADMIN_TOKEN"
	EnvTokenFile = "SWARMCLI_CD_ADMIN_TOKEN_FILE"
)

// ErrNoToken is what Ready returns when neither variable is set. The
// controller turns it into a refusal to start rather than serving an open API:
// it holds root-equivalent access to the swarm, so an unauthenticated endpoint
// is a root shell, and defaulting to open would make the safe configuration
// the one an operator has to remember.
var ErrNoToken = fmt.Errorf("no admin token configured: set %s to a file (a Docker secret) or %s to the token itself", EnvTokenFile, EnvToken)

var errBadToken = errors.New("invalid or missing bearer token")

func init() { Register("token", newToken(os.Getenv, os.ReadFile)) }

// token authenticates a single shared bearer token and authorises everything
// it authenticates. Anything finer — per-user identity, per-project scoping —
// is the Business Edition's job.
type token struct {
	value string
	err   error // why there is no token, reported by Ready
}

// newToken takes its environment and file reader as arguments so the tests can
// drive it without touching the process environment.
func newToken(getenv func(string) string, readFile func(string) ([]byte, error)) *token {
	v, err := TokenFromEnv(getenv, readFile)
	if err != nil {
		return &token{err: err}
	}
	return &token{value: v}
}

// TokenFromEnv resolves the admin token, preferring the file form. It is
// exported because the command-line client has to present the same token this
// authorizer expects, and two copies of the precedence would eventually
// disagree about which variable wins.
//
// It takes its environment and file reader as arguments so the tests can drive
// it without touching the process environment.
func TokenFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	if path := getenv(EnvTokenFile); path != "" {
		data, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", EnvTokenFile, err)
		}
		if v := strings.TrimSpace(string(data)); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%s names an empty file", EnvTokenFile)
	}
	if v := strings.TrimSpace(getenv(EnvToken)); v != "" {
		return v, nil
	}
	return "", ErrNoToken
}

// Ready implements Authorizer.
func (t *token) Ready() error { return t.err }

// Authenticate implements Authorizer. The comparison is constant-time: the
// token is a shared secret and a timing oracle on it is a full compromise.
func (t *token) Authenticate(r *http.Request) (Subject, error) {
	if t.err != nil {
		return Subject{}, t.err
	}
	presented, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		return Subject{}, errBadToken
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(t.value)) != 1 {
		return Subject{}, errBadToken
	}
	return Subject{Name: "admin"}, nil
}

// Authorize implements Authorizer. A subject this build authenticated is the
// administrator, so there is nothing left to decide.
func (t *token) Authorize(_ context.Context, _ Subject, _ Action, _ string) error { return nil }

// bearer extracts the credential from an Authorization header, accepting the
// scheme case-insensitively as RFC 7235 requires.
func bearer(header string) (string, bool) {
	scheme, credential, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	credential = strings.TrimSpace(credential)
	return credential, credential != ""
}
