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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// EnvServer names the controller to talk to, so that a shell exports it once
// rather than repeating --server on every command.
const EnvServer = "SWARMCLI_CD_SERVER"

// EnvCACert names a PEM file whose certificates are trusted for an https
// server. It exists beside EnvServer because the two are set together: the
// moment a deployment points EnvServer at its own TLS listener, every command
// run inside the controller — the case DefaultServer's doc comment names —
// meets a certificate no public authority signed.
const EnvCACert = "SWARMCLI_CD_CA_CERT"

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

// Options configure the TLS New leaves at the standard library's defaults.
//
// A second constructor rather than two more parameters on New: this is an
// exported package of a released v1 module, and widening New would break every
// caller for the sake of the two commands that pass these.
type Options struct {
	// CACert is a PEM file whose certificates are the ones trusted for an https
	// server, for a controller presenting a certificate no public authority
	// signed. Empty verifies against the system pool, which is what a
	// certificate from a real CA needs.
	CACert string

	// SkipVerifyOnLoopback skips certificate verification when — and only when —
	// the server is an https URL whose host is syntactically a loopback address.
	//
	// It exists for the container healthcheck, which runs beside the controller
	// as a separate process invocation, cannot be told the controller's
	// --tls-cert, and is asserting liveness rather than identity: /healthz
	// discloses nothing and the connection never leaves the host.
	//
	// It is deliberately not --insecure. The exception is decided here, against
	// the parsed URL, so no caller passing it can reach another host unverified
	// — which takes two things, because the decision is taken once and the
	// connection can be moved afterwards: the predicate below, and the refusal
	// to follow a redirect while the exception is in force.
	SkipVerifyOnLoopback bool
}

// NewWithOptions is New with the TLS configuration opts describes.
func NewWithOptions(server, token string, opts Options) (*Client, error) {
	c := New(server, token)

	skip := opts.SkipVerifyOnLoopback && loopbackTLS(c.server)
	if opts.CACert == "" && !skip {
		// Nothing to configure, so this is exactly New's client — including for
		// a caller that asked for the loopback exception and named a host that
		// is not one. That identity is the whole statement that this is not
		// --insecure: there is no route from these options to an unverified
		// connection to anywhere else.
		return c, nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: skip}
	if opts.CACert != "" {
		pemBytes, err := os.ReadFile(opts.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading the CA certificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			// Named rather than ignored: AppendCertsFromPEM reports failure by
			// returning false, so a file of the wrong kind would otherwise
			// produce an empty pool and a verification error naming the
			// controller's certificate instead of the operator's file.
			return nil, fmt.Errorf("%s: no PEM certificate found in it", opts.CACert)
		}
		cfg.RootCAs = pool
	}

	// Cloned from the default rather than built here, so proxy handling, the
	// dial timeouts and connection reuse stay whatever the standard library
	// currently does rather than a snapshot of it taken on this line.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg
	c.http.Transport = tr

	if skip {
		// A client that will not check a certificate must not be repointed by
		// the thing it is not checking.
		//
		// The exception is decided once, against the URL the caller named. A
		// transport is installed on the whole Client, and a Client follows
		// redirects — so without this, a loopback https server answering 302 to
		// any other TLS host is fetched with verification off, and the
		// predicate that was so careful about the first host never sees the
		// second. Nothing reachable today does that: /healthz is the only URL
		// fetched with the flag and it does not redirect. But stack.yml
		// recommends a reverse proxy in front, and a proxy canonicalising a URL
		// is ordinary.
		c.http.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("refusing a redirect to '%s': this connection does not verify certificates, so it may not be repointed", req.URL.Redacted())
		}
	}
	return c, nil
}

// loopbackTLS reports whether server is an https URL naming this host, written
// as an address rather than as a name that merely resolves to one.
//
// The predicate never resolves, deliberately. Deciding whether to verify a
// certificate by first asking a resolver would put the trust decision in DNS,
// which is the one thing TLS exists not to depend on — and the answer can
// differ between the lookup that decided and the dial that connected. An
// operator with such a name puts it in the certificate's SANs, or probes
// https://127.0.0.1:8080, which is what stack.yml does.
func loopbackTLS(server string) bool {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" {
		return false
	}
	// Not an address, but RFC 6761 reserves it: a resolver may not answer
	// "localhost" with anything but the loopback interface.
	if strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}
	// Covers 127.0.0.0/8, ::1 and the v4-in-v6 form, which is what IsLoopback
	// is for — a hand-written 127.0.0.1 comparison would miss the other three.
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
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

// Status returns the controller's own state: where the app set is sourced from,
// the revision and time of the last successful load, and whether what is running
// is a last-good set because a newer one is being refused.
func (c *Client) Status(ctx context.Context) (application.ControllerStatus, []byte, error) {
	return decode[application.ControllerStatus](ctx, c, "/api/v1/status")
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

// Health probes the controller's liveness endpoint.
//
// It is the one endpoint that takes no credential, because a container
// healthcheck runs beside the process and cannot carry one without putting it
// in the stack file and in `docker inspect` output.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/healthz")
	return err
}
