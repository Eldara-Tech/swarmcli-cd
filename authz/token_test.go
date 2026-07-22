// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package authz

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func files(pairs map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		v, ok := pairs[p]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(v), nil
	}
}

func request(header string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/applications", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

// An unconfigured controller must fail loudly at startup rather than serving an
// open API or a wall of indistinguishable 401s.
func TestUnconfiguredIsNotReady(t *testing.T) {
	tok := newToken(env(nil), files(nil))

	if !errors.Is(tok.Ready(), ErrNoToken) {
		t.Errorf("Ready = %v, want ErrNoToken", tok.Ready())
	}
	if _, err := tok.Authenticate(request("Bearer anything")); err == nil {
		t.Error("an unconfigured authorizer authenticated a request")
	}
}

func TestTokenFromEnv(t *testing.T) {
	tok := newToken(env(map[string]string{EnvToken: "  s3cret  "}), files(nil))

	if err := tok.Ready(); err != nil {
		t.Fatalf("Ready = %v, want nil", err)
	}
	if _, err := tok.Authenticate(request("Bearer s3cret")); err != nil {
		t.Errorf("Authenticate = %v, want nil (surrounding space should be trimmed)", err)
	}
}

// A Docker secret arrives as a file, so the file form is the one production
// uses, and it wins over the string form.
func TestTokenFileWins(t *testing.T) {
	tok := newToken(
		env(map[string]string{EnvTokenFile: "/run/secrets/admin", EnvToken: "ignored"}),
		files(map[string]string{"/run/secrets/admin": "from-file\n"}),
	)

	if err := tok.Ready(); err != nil {
		t.Fatalf("Ready = %v, want nil", err)
	}
	if _, err := tok.Authenticate(request("Bearer from-file")); err != nil {
		t.Errorf("Authenticate with the file token = %v, want nil", err)
	}
	if _, err := tok.Authenticate(request("Bearer ignored")); err == nil {
		t.Error("the string token was accepted even though the file form was set")
	}
}

func TestTokenFileProblemsAreNotReady(t *testing.T) {
	for name, tok := range map[string]*token{
		"unreadable": newToken(env(map[string]string{EnvTokenFile: "/run/secrets/absent"}), files(nil)),
		"empty":      newToken(env(map[string]string{EnvTokenFile: "/run/secrets/blank"}), files(map[string]string{"/run/secrets/blank": "  \n"})),
	} {
		t.Run(name, func(t *testing.T) {
			err := tok.Ready()
			if err == nil {
				t.Fatal("Ready = nil, want an error")
			}
			if !strings.Contains(err.Error(), EnvTokenFile) {
				t.Errorf("error %q does not name %s", err, EnvTokenFile)
			}
		})
	}
}

func TestAuthenticateRejects(t *testing.T) {
	tok := newToken(env(map[string]string{EnvToken: "s3cret"}), files(nil))

	for name, header := range map[string]string{
		"no header":     "",
		"wrong token":   "Bearer wrong",
		"wrong scheme":  "Basic s3cret",
		"bare token":    "s3cret",
		"empty bearer":  "Bearer ",
		"prefix only":   "Bearer s3cre",
		"longer token":  "Bearer s3crets",
		"schemeless sp": " s3cret",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tok.Authenticate(request(header)); err == nil {
				t.Errorf("Authenticate(%q) = nil, want an error", header)
			}
		})
	}
}

// RFC 7235 makes the scheme case-insensitive.
func TestAuthenticateAcceptsAnySchemeCase(t *testing.T) {
	tok := newToken(env(map[string]string{EnvToken: "s3cret"}), files(nil))

	for _, header := range []string{"Bearer s3cret", "bearer s3cret", "BEARER s3cret"} {
		got, err := tok.Authenticate(request(header))
		if err != nil {
			t.Errorf("Authenticate(%q) = %v, want nil", header, err)
			continue
		}
		if got.Name != "admin" {
			t.Errorf("subject = %q, want admin", got.Name)
		}
	}
}

// The default authorises everything it authenticates; finer scoping is the
// Business Edition's job.
func TestAuthorizeAllowsEverything(t *testing.T) {
	tok := newToken(env(map[string]string{EnvToken: "s3cret"}), files(nil))

	for _, act := range []Action{ActionRead, ActionSync} {
		if err := tok.Authorize(context.Background(), Subject{Name: "admin"}, act, "edge"); err != nil {
			t.Errorf("Authorize(%s) = %v, want nil", act, err)
		}
	}
}

func TestDefaultIsRegistered(t *testing.T) {
	if got := Active(); got != "token" {
		t.Errorf("Active = %q, want token", got)
	}
	if Get() == nil {
		t.Error("Get returned nil")
	}
}

func TestRegisterReplaces(t *testing.T) {
	original, originalName := Get(), Active()
	t.Cleanup(func() { Register(originalName, original) })

	Register("companion", stubAuthorizer{})
	if Active() != "companion" {
		t.Errorf("Active = %q, want companion", Active())
	}
	if _, ok := Get().(stubAuthorizer); !ok {
		t.Errorf("Get returned %T, want the companion's", Get())
	}
}

type stubAuthorizer struct{}

func (stubAuthorizer) Ready() error                                { return nil }
func (stubAuthorizer) Authenticate(*http.Request) (Subject, error) { return Subject{}, nil }
func (stubAuthorizer) Authorize(context.Context, Subject, Action, string) error {
	return nil
}
