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
	// EnvUsername is the name the login screen asks for beside the token, and
	// the name everything the token authenticates is attributed to. It is not
	// a secret, so it has no file form.
	EnvUsername = "SWARMCLI_CD_ADMIN_USERNAME"
)

// DefaultUsername is the username when EnvUsername is unset, and the subject
// name every request carried before the variable existed.
const DefaultUsername = "admin"

// ErrNoToken is what Ready returns when neither variable is set. The
// controller turns it into a refusal to start rather than serving an open API:
// it holds root-equivalent access to the swarm, so an unauthenticated endpoint
// is a root shell, and defaulting to open would make the safe configuration
// the one an operator has to remember.
var ErrNoToken = fmt.Errorf("no admin token configured: set %s to a file (a Docker secret) or %s to the token itself", EnvTokenFile, EnvToken)

var errBadToken = errors.New("invalid or missing credential")

func init() { Register("token", newToken(os.Getenv, os.ReadFile)) }

// token authenticates a single shared bearer token and authorises everything
// it authenticates. Anything finer — per-user identity, per-project scoping —
// is the Business Edition's job.
type token struct {
	value    string
	username string
	err      error // why there is no token, reported by Ready
}

// newToken takes its environment and file reader as arguments so the tests can
// drive it without touching the process environment.
func newToken(getenv func(string) string, readFile func(string) ([]byte, error)) *token {
	username := strings.TrimSpace(getenv(EnvUsername))
	if username == "" {
		username = DefaultUsername
	}
	// A colon ends the username in a Basic credential (RFC 7617), so a name
	// holding one could never sign in, and nothing else would say why.
	if strings.Contains(username, ":") {
		return &token{err: fmt.Errorf("%s must not contain ':', which separates the username from the token at login", EnvUsername)}
	}
	v, err := TokenFromEnv(getenv, readFile)
	if err != nil {
		return &token{err: err}
	}
	return &token{value: v, username: username}
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
//
// Two forms are accepted. The login screen sends Basic credentials, the
// username beside the token. The CLI, scripts and the container healthcheck
// send the token alone as a bearer, as they always have, so the username names
// the subject rather than adding a second secret.
func (t *token) Authenticate(r *http.Request) (Subject, error) {
	if t.err != nil {
		return Subject{}, t.err
	}
	if !t.accepts(r) {
		return Subject{}, errBadToken
	}
	return Subject{Name: t.username}, nil
}

func (t *token) accepts(r *http.Request) bool {
	if username, presented, ok := r.BasicAuth(); ok {
		// Both compared whatever the first says: an && would answer a wrong
		// username sooner than a wrong token.
		user := subtle.ConstantTimeCompare([]byte(username), []byte(t.username))
		tok := subtle.ConstantTimeCompare([]byte(presented), []byte(t.value))
		return user&tok == 1
	}
	presented, ok := bearer(r.Header.Get("Authorization"))
	return ok && subtle.ConstantTimeCompare([]byte(presented), []byte(t.value)) == 1
}

// Authorize implements Authorizer. A subject this build authenticated is the
// administrator, so there is nothing left to decide.
func (t *token) Authorize(_ context.Context, _ Subject, _ Action, _ string) error { return nil }

// tokenLogin is the box the login screen draws for this authorizer, and the
// fallback MethodsFor hands out for an authorizer that names none of its own.
// One value, so that the two cannot say different things about the same box.
var tokenLogin = LoginMethod{ID: "token", Label: "Admin token"}

// LoginMethods implements LoginMethods. No Start: the credential is typed in,
// so there is nowhere for the browser to go first.
func (t *token) LoginMethods() []LoginMethod { return []LoginMethod{tokenLogin} }

// Visible implements Authorizer. There is one subject and it may see
// everything, so there is nothing to narrow — the applications come back as
// they went in, including a nil, which is the empty list a caller ranges over
// without noticing.
func (t *token) Visible(_ context.Context, _ Subject, _ Action, applications []string) ([]string, error) {
	return applications, nil
}

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
