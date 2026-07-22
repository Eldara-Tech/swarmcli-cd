// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
