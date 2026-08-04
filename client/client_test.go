// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package client

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// serve returns a client pointed at a server that records what it was asked.
func serve(t *testing.T, status int, body string) (*Client, *request) {
	t.Helper()
	got := &request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.authorization = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "s3cret"), got
}

type request struct{ method, path, authorization string }

func TestListDecodesAndReturnsTheBytesItDecoded(t *testing.T) {
	body := `{"applications":[{"spec":{"name":"edge"},"status":{"sync":{"state":"Synced"}}}]}`
	c, got := serve(t, http.StatusOK, body)

	apps, raw, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List = %v, want nil", err)
	}
	if len(apps.Applications) != 1 || apps.Applications[0].Spec.Name != "edge" {
		t.Errorf("List = %+v, want one application named edge", apps.Applications)
	}
	// The raw bytes are what --output json prints, so they have to be the
	// controller's own response rather than a re-encoding of the decoded value.
	if string(raw) != body {
		t.Errorf("raw = %q, want %q", raw, body)
	}
	if got.path != "/api/v1/applications" || got.method != http.MethodGet {
		t.Errorf("requested %s %s", got.method, got.path)
	}
	if got.authorization != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the bearer token", got.authorization)
	}
}

func TestPathsAndMethods(t *testing.T) {
	for name, tc := range map[string]struct {
		call       func(*Client) error
		wantMethod string
		wantPath   string
	}{
		"status": {func(c *Client) error {
			_, _, err := c.Status(context.Background())
			return err
		}, http.MethodGet, "/api/v1/status"},
		"get": {func(c *Client) error {
			_, _, err := c.Get(context.Background(), "edge")
			return err
		}, http.MethodGet, "/api/v1/applications/edge"},
		"diff": {func(c *Client) error {
			_, _, err := c.Diff(context.Background(), "edge")
			return err
		}, http.MethodGet, "/api/v1/applications/edge/diff"},
		"history": {func(c *Client) error {
			_, _, err := c.History(context.Background(), "edge")
			return err
		}, http.MethodGet, "/api/v1/applications/edge/history"},
		"sync": {func(c *Client) error {
			return c.Sync(context.Background(), "edge")
		}, http.MethodPost, "/api/v1/applications/edge/sync"},
	} {
		t.Run(name, func(t *testing.T) {
			c, got := serve(t, http.StatusOK, `{}`)
			if err := tc.call(c); err != nil {
				t.Fatalf("call = %v, want nil", err)
			}
			if got.method != tc.wantMethod || got.path != tc.wantPath {
				t.Errorf("requested %s %s, want %s %s", got.method, got.path, tc.wantMethod, tc.wantPath)
			}
		})
	}
}

// A sync is accepted, not performed: 202 is a success.
func TestSyncAcceptsAccepted(t *testing.T) {
	c, _ := serve(t, http.StatusAccepted, `{"application":"edge","accepted":true}`)
	if err := c.Sync(context.Background(), "edge"); err != nil {
		t.Errorf("Sync = %v, want nil", err)
	}
}

func TestErrorCarriesTheAPIsOwnMessage(t *testing.T) {
	c, _ := serve(t, http.StatusNotFound, `{"error":"no such application"}`)

	_, _, err := c.Get(context.Background(), "nope")
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("Get = %v, want a *client.Error", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", apiErr.StatusCode)
	}
	if apiErr.Message != "no such application" {
		t.Errorf("Message = %q, want the API's own", apiErr.Message)
	}
}

// A proxy in front of the controller answers in HTML, and "unexpected end of
// JSON input" would tell the operator nothing about what actually happened.
func TestErrorFallsBackToTheStatusLine(t *testing.T) {
	c, _ := serve(t, http.StatusBadGateway, "<html>502 Bad Gateway</html>")

	_, _, err := c.List(context.Background())
	if err == nil {
		t.Fatal("List = nil, want an error")
	}
	if !strings.Contains(err.Error(), "bad gateway") {
		t.Errorf("err = %v, want it to name the status", err)
	}
}

func TestUndecodableSuccessIsReported(t *testing.T) {
	c, _ := serve(t, http.StatusOK, "not json")

	_, _, err := c.List(context.Background())
	if err == nil {
		t.Fatal("List = nil, want an error")
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("err = %v, want it to say the response could not be read", err)
	}
}

// A trailing slash on the server address must not become a double slash in the
// path: net/http would answer 301 and the client would follow it silently, but
// only for GETs.
func TestServerAddressToleratesATrailingSlash(t *testing.T) {
	got := &request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	if _, _, err := New(srv.URL+"/", "").List(context.Background()); err != nil {
		t.Fatalf("List = %v, want nil", err)
	}
	if got.path != "/api/v1/applications" {
		t.Errorf("path = %q, want it unchanged", got.path)
	}
}

func TestHealth(t *testing.T) {
	c, got := serve(t, http.StatusOK, "ok\n")
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health = %v, want nil", err)
	}
	if got.path != "/healthz" {
		t.Errorf("path = %q, want /healthz", got.path)
	}
}

func TestHealthFailsWhenTheControllerIsNotServing(t *testing.T) {
	c, _ := serve(t, http.StatusServiceUnavailable, "")
	if err := c.Health(context.Background()); err == nil {
		t.Error("Health = nil, want an error")
	}
}

// The loopback exception is decided from the URL's own text and nothing else. A
// name that merely resolves to 127.0.0.1 must not qualify: deciding whether to
// verify a certificate by asking a resolver puts the trust decision in DNS,
// which is the one thing TLS exists not to depend on.
func TestTheLoopbackExceptionIsSyntacticAndHTTPSOnly(t *testing.T) {
	for _, tc := range []struct {
		server string
		want   bool
	}{
		{"https://127.0.0.1:8080", true},
		{"https://127.0.0.53:8080", true},    // the whole of 127.0.0.0/8
		{"https://[::1]:8080", true},         // and the v6 loopback
		{"https://[::ffff:127.0.0.1]", true}, // including the v4-in-v6 form
		{"https://localhost:8080", true},     // reserved to loopback by RFC 6761
		{"https://LocalHost:8080", true},     // hostnames are case-insensitive
		{"http://127.0.0.1:8080", false},     // plaintext: nothing to skip
		{"https://10.0.0.1:8080", false},
		{"https://example.com", false},
		// Deliberate. It may well resolve to 127.0.0.1 here and to something
		// else at the moment of the dial, and the certificate would be trusted
		// on the strength of a lookup nobody can audit afterwards.
		{"https://loopback.example.com", false},
		{"://not a url", false},
	} {
		t.Run(tc.server, func(t *testing.T) {
			if got := loopbackTLS(tc.server); got != tc.want {
				t.Errorf("loopbackTLS(%q) = %v, want %v", tc.server, got, tc.want)
			}
		})
	}
}

// The strongest available statement that SkipVerifyOnLoopback is not --insecure
// under another name: asked for against any other host, it produces the client
// New produces, with no TLS configuration of its own at all.
func TestSkipVerifyOnLoopbackChangesNothingForAnyOtherHost(t *testing.T) {
	for _, server := range []string{"https://controller.example.com", "http://127.0.0.1:8080"} {
		t.Run(server, func(t *testing.T) {
			got, err := NewWithOptions(server, "s3cret", Options{SkipVerifyOnLoopback: true})
			if err != nil {
				t.Fatalf("NewWithOptions = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, New(server, "s3cret")) {
				t.Errorf("client = %+v, want it identical to New's", got)
			}
		})
	}
}

// The container healthcheck's whole mechanism: an https loopback URL answers
// with no CA certificate to be had, because the probe asserts liveness rather
// than identity.
func TestHealthOverLoopbackTLSNeedsNoCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	}))
	t.Cleanup(srv.Close)

	c, err := NewWithOptions(srv.URL, "", Options{SkipVerifyOnLoopback: true})
	if err != nil {
		t.Fatalf("NewWithOptions = %v, want nil", err)
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health = %v, want nil", err)
	}

	// And the same server without the exception does not, which is what makes
	// the assertion above about the exception rather than about the certificate
	// httptest happens to generate.
	if err := New(srv.URL, "").Health(context.Background()); err == nil {
		t.Error("Health = nil without the exception, want a certificate error")
	}
}

func TestACACertIsWhatMakesASelfSignedControllerReachable(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c, err := NewWithOptions(srv.URL, "", Options{CACert: writeCert(t, srv)})
	if err != nil {
		t.Fatalf("NewWithOptions = %v, want nil", err)
	}
	if _, _, err := c.List(context.Background()); err != nil {
		t.Errorf("List = %v, want nil", err)
	}
}

// A path that is not a certificate is named here rather than surfacing later as
// a verification error against the controller's certificate, which points at
// the wrong file entirely.
func TestAnUnusableCACertIsRefusedAtConstruction(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "not-a-cert.pem")
	if err := os.WriteFile(notPEM, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"a file that is not there": filepath.Join(dir, "absent.pem"),
		"a file that is not PEM":   notPEM,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWithOptions("https://example.com", "", Options{CACert: path}); err == nil {
				t.Error("NewWithOptions = nil, want a refusal naming the file")
			}
		})
	}
}

// writeCert saves the test server's certificate as the PEM file an operator
// would pass to --ca-cert. A self-signed leaf is its own authority, which is
// what makes this the same file the controller was given as --tls-cert.
func writeCert(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
