// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package client talks to the controller's HTTP API.
//
// Per D3 the API is the only surface: the command-line client, the TUI view and
// the web UI all read the same endpoints, so anything one of them can show, the
// others can. That is why this package exists rather than the CLI reaching into
// the reconciler — a second path to the same state would be a second definition
// of what an application's state is.
//
// Every read hands back the decoded value and the bytes it was decoded from.
// Machine-readable output prints those bytes rather than re-encoding the value,
// so `--output json` is exactly what the controller said and cannot drift from
// it as the types grow.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// EnvServer names the controller to talk to, so that a shell exports it once
// rather than repeating --server on every command.
const EnvServer = "SWARMCLI_CD_SERVER"

// DefaultServer is the controller on this host, which is where a `docker exec`
// into the controller's own container finds it.
const DefaultServer = "http://127.0.0.1:8080"

// requestTimeout bounds one request. History reads the swarm's raft store and
// is the slowest of them; a sync returns 202 immediately and does not wait for
// the rollout, so nothing here is legitimately slow for minutes.
const requestTimeout = 30 * time.Second

// maxBody caps a response. The API is the only thing being read and its largest
// response is a diff of rendered manifests, but a client that will happily read
// an unbounded body from a host it was pointed at is a client that can be made
// to exhaust memory by being pointed at the wrong one.
const maxBody = 32 << 20

// Client reads one controller.
type Client struct {
	server string
	token  string
	http   *http.Client
}

// New returns a client for the controller at server, presenting token as a
// bearer credential. An empty token still produces a usable client — the
// resulting 401 is the API's to explain, not this constructor's.
func New(server, token string) *Client {
	return &Client{
		server: strings.TrimSuffix(server, "/"),
		token:  token,
		http:   &http.Client{Timeout: requestTimeout},
	}
}

// Error is a non-2xx response, carrying the API's own message.
type Error struct {
	StatusCode int
	Message    string
}

func (e *Error) Error() string { return e.Message }

// Applications is the list response. The controller wraps the array in an
// object so that the response can grow fields without becoming a different
// kind of document.
type Applications struct {
	Applications []application.View `json:"applications"`
}

// Diff is the diff response. Planned distinguishes "nothing would change" from
// "this application has not been reconciled yet", which look identical in the
// releases array and mean very different things.
type Diff struct {
	Releases []application.ReleaseDiff `json:"releases"`
	Planned  bool                      `json:"planned"`
}

// List returns every application with its sync state and health, without
// per-release detail.
func (c *Client) List(ctx context.Context) (Applications, []byte, error) {
	return decode[Applications](ctx, c, "/api/v1/applications")
}

// Get returns one application with its releases and their services.
func (c *Client) Get(ctx context.Context, app string) (application.View, []byte, error) {
	return decode[application.View](ctx, c, "/api/v1/applications/"+url.PathEscape(app))
}

// Diff returns the manifest change each of the application's releases would
// undergo.
func (c *Client) Diff(ctx context.Context, app string) (Diff, []byte, error) {
	return decode[Diff](ctx, c, "/api/v1/applications/"+url.PathEscape(app)+"/diff")
}

// History returns every declared release's revisions, newest first.
func (c *Client) History(ctx context.Context, app string) (application.History, []byte, error) {
	return decode[application.History](ctx, c, "/api/v1/applications/"+url.PathEscape(app)+"/history")
}

// Sync asks the controller to reconcile the application now.
//
// It returns as soon as the controller has accepted the request: a sync fetches,
// renders, plans and deploys, and under a wait policy blocks until the rollout
// converges, which is legitimately minutes. Follow it by polling Get — the
// application's LastSync is what records the outcome.
func (c *Client) Sync(ctx context.Context, app string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/applications/"+url.PathEscape(app)+"/sync")
	return err
}

// decode is a GET returning both the decoded body and the bytes it came from.
// It is a function rather than a method because Go does not allow methods to
// introduce type parameters.
func decode[T any](ctx context.Context, c *Client, path string) (T, []byte, error) {
	var v T
	raw, err := c.do(ctx, http.MethodGet, path)
	if err != nil {
		return v, raw, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, raw, fmt.Errorf("%s: the controller's response could not be read: %w", path, err)
	}
	return v, raw, nil
}

// do performs one request and returns its body. A non-2xx becomes an *Error
// carrying the API's own message, and the body is returned alongside it so a
// caller printing raw output has something to print either way.
func (c *Client) do(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.server+path, err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, c.server+path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading the response: %w", method, c.server+path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return body, &Error{StatusCode: resp.StatusCode, Message: message(body, resp.Status)}
	}
	return body, nil
}

// message extracts the API's error text, falling back to the status line for a
// response that did not come from the API at all — a proxy's HTML error page
// being the one an operator is most likely to meet.
func message(body []byte, status string) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != "" {
		return envelope.Error
	}
	return strings.ToLower(status)
}
