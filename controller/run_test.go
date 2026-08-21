// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/appset"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/extension"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/reconcile"
	"github.com/Eldara-Tech/swarmcli-cd/source"

	swarmlog "github.com/Eldara-Tech/swarmcli/v2/utils/log"
)

func TestControllerHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "swarmcli-cd controller [options]") {
		t.Errorf("stdout = %q, want the controller's usage", stdout.String())
	}
	// The credentials are the part an operator cannot guess, so the help has to
	// name them rather than leaving them to the README.
	if !strings.Contains(stdout.String(), authz.EnvTokenFile) {
		t.Errorf("stdout = %q, want it to name %s", stdout.String(), authz.EnvTokenFile)
	}
}

// An unknown flag must be an error rather than being ignored: the alternative
// is a controller that silently runs with a default the operator thought they
// had overridden.
func TestControllerRejectsAnUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--listem", ":9000"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "listem") {
		t.Errorf("stderr = %q, want it to name the flag", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q to stdout, want the error on stderr only", stdout.String())
	}
}

func TestControllerRejectsAPositionalArgument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "start"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "start") {
		t.Errorf("stderr = %q, want it to name the argument", stderr.String())
	}
}

// A file mounted at deploy time that cannot be read is fatal, as it has always
// been: nothing will change under the controller's feet, so a restart is the
// operator's notification. The authorizer is swapped in because serve refuses
// on an unready one before it reads anything, which is the whole point of that
// check going first.
func TestServeReportsAMissingConfig(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})

	err := serve(context.Background(), staticOptions(filepath.Join(t.TempDir(), "applications.yaml"), t.TempDir()), discardLog())
	if err == nil {
		t.Fatal("serve = nil, want an error for a missing applications file")
	}
	if !strings.Contains(err.Error(), "applications.yaml") {
		t.Errorf("serve = %v, want the error to name the file", err)
	}
}

// The controller refuses to start rather than serving an API that rejects
// everything: an unconfigured authorizer and a wrong token are indistinguishable
// from the outside, and only one of them is worth an operator's afternoon.
func TestServeRefusesAnUnreadyAuthorizer(t *testing.T) {
	swapAuthorizer(t, unreadyAuthorizer{})

	err := serve(context.Background(), staticOptions(writeConfig(t), t.TempDir()), discardLog())
	if err == nil {
		t.Fatal("serve = nil, want a refusal")
	}
	if !errors.Is(err, authz.ErrNoToken) {
		t.Errorf("serve = %v, want it to carry the authorizer's own reason", err)
	}
	if !strings.Contains(err.Error(), "test") {
		t.Errorf("serve = %v, want it to name the authorizer in force", err)
	}
}

// The route set is decided by a seam, so a companion can hand the controller one
// it cannot serve. Coming up anyway would mean serving whichever half of a
// colliding pair the mux happened to keep — and which half that is depends on
// declaration order, so the endpoint an operator thought was guarded may not be
// the one answering. serve must refuse, name the pattern, and bind nothing.
func TestServeRefusesARouteSetItCannotServe(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})
	collision.armed.Store(true)
	t.Cleanup(func() { collision.armed.Store(false) })

	o := staticOptions(writeConfig(t), t.TempDir())
	o.listen = freePort(t)

	// Already cancelled, so that a serve which failed to refuse returns rather
	// than running the whole controller until the test timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := serve(ctx, o, discardLog())
	if err == nil {
		t.Fatal("serve = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), collidingPattern) {
		t.Errorf("serve = %v, want the error to name the pattern", err)
	}
	// The refusal is only worth something if it happened before anything was
	// served. A controller half-up on a route set it could not build is the
	// outage the refusal exists to prevent, so binding the address ourselves is
	// the assertion: it succeeds only if nothing else holds it.
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		t.Fatalf("something is listening on %s: %v", o.listen, err)
	}
	_ = ln.Close()
}

// collidingPattern is one of the core's own routes, so an extension declaring it
// makes the whole route set unservable.
const collidingPattern = "GET /api/v1/status"

// collidingExtension declares that pattern, but only while a test has armed it.
//
// It is registered once, from the init below, because extension is a seam.List:
// it appends, it has no removal, and api.Handler re-reads it every time it
// builds a mux. A colliding route left in that list would therefore fail every
// later Handler call in this binary — which is every test here that starts an
// API server. Disarmed it declares nothing, and an extension declaring nothing
// builds exactly the mux the OSS build does, so no other test can observe it
// whatever order they run in. That is what makes this safe under -shuffle, and
// it is why the arming is scoped to one test rather than the registration.
type collidingExtension struct{ armed atomic.Bool }

func (e *collidingExtension) Routes() []extension.Route {
	if !e.armed.Load() {
		return nil
	}
	return []extension.Route{{
		Pattern: collidingPattern,
		Action:  authz.ActionRead,
		Handler: func(http.ResponseWriter, *http.Request, authz.Subject) {},
	}}
}

func (e *collidingExtension) PublicRoutes() []extension.Route { return nil }

var collision = &collidingExtension{}

func init() { extension.Register("collision", collision) }

// The clone root and the chart cache must be separate directories: everything
// under a clone is force-checked-out and cleaned on every fetch, so a chart
// cache living there would be deleted underneath the builder.
func TestServeCreatesSeparateCloneAndChartDirectories(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})
	data := t.TempDir()

	// Already cancelled: serve wires everything up, starts both components and
	// unwinds immediately, which is as far as this can go without a daemon.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := serve(ctx, staticOptions(writeConfig(t), data), discardLog()); err != nil {
		t.Fatalf("serve = %v, want nil", err)
	}

	for _, dir := range []string{"repos", "charts"} {
		if _, err := os.Stat(filepath.Join(data, dir)); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
	// The app set is not cloned in this mode, so its root is not created.
	if _, err := os.Stat(filepath.Join(data, "appset")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("appset directory = %v, want it absent in static mode", err)
	}
}

// Which selector was given decides the mode; nothing states it twice.
func TestAppSetModeFollowsTheSelectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    options
		want string
	}{
		{"no selector is the mounted file", options{configPath: "/etc/applications.yaml"}, appSetModeStatic},
		{"a repository", options{appSetRepo: "https://example.com/apps.git"}, appSetModeGit},
		{"a directory", options{appSetDir: "/var/lib/appset"}, appSetModePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.mode(); got != tc.want {
				t.Errorf("mode = %q, want %q", got, tc.want)
			}
		})
	}
}

// A bootstrap that says two things, or half of one, is refused rather than
// resolved by a precedence rule nobody can remember. Each error has to name the
// flag that has to change.
func TestAppSetSelectorsAreValidated(t *testing.T) {
	valid := func(o options) options {
		o.appSetInterval = appset.DefaultInterval
		o.interval = reconcile.DefaultInterval
		if o.controllerID == "" {
			o.controllerID = application.DefaultControllerID
		}
		return o
	}
	for _, tc := range []struct {
		name string
		o    options
		want string
	}{
		{"two sources", valid(options{appSetRepo: "https://example.com/apps.git", appSetRevision: "main", appSetPath: "applications.yaml", appSetDir: "/var/lib/appset"}), "--appset-dir"},
		{"an explicit config beside a repository", valid(options{configSet: true, appSetRepo: "https://example.com/apps.git", appSetRevision: "main", appSetPath: "applications.yaml"}), "--config"},
		{"an explicit config beside a directory", valid(options{configSet: true, appSetDir: "/var/lib/appset"}), "--config"},
		{"a repository with no revision", valid(options{appSetRepo: "https://example.com/apps.git", appSetPath: "applications.yaml"}), "--appset-revision"},
		{"a repository with no path", valid(options{appSetRepo: "https://example.com/apps.git", appSetRevision: "main"}), "--appset-path"},
		{"a revision with no repository", valid(options{appSetRevision: "main"}), "--appset-revision"},
		{"a path with no source", valid(options{appSetPath: "applications.yaml"}), "--appset-path"},
		{"an interval of zero", options{appSetDir: "/var/lib/appset", controllerID: application.DefaultControllerID}, "--appset-interval"},
		{"a reconcile interval of zero", options{appSetDir: "/var/lib/appset", appSetInterval: appset.DefaultInterval, controllerID: application.DefaultControllerID}, "--reconcile-interval"},
		{"a controller id with a slash", valid(options{controllerID: "a/b"}), "--controller-id"},
		{"a controller id with a colon", valid(options{controllerID: "a:b"}), "--controller-id"},
		{"volumes without prune", valid(options{pruneVolumes: true}), "--prune-volumes"},
		// Each names the half that is missing, because that is the flag the
		// operator has to add — and because half a pair accepted would serve
		// plaintext on a listener nobody would think to check.
		{"a certificate with no key", valid(options{tlsCert: "/tls/tls.crt"}), "--tls-key"},
		{"a key with no certificate", valid(options{tlsKey: "/tls/tls.key"}), "--tls-cert"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.o.validate()
			if err == nil {
				t.Fatal("validate = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// The default --config must not trip the ambiguity check: every deployment has
// one, and only an operator who typed it meant it.
func TestTheDefaultConfigIsNotAnExplicitOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// A repository that cannot be resolved, so this gets past the flags and
	// fails later — the assertion is only that it is not a usage error.
	code := run([]string{"controller", "--appset-repo", "/nonexistent.git", "--appset-revision", "main",
		"--appset-path", "applications.yaml", "--listen", testListen, "--data", t.TempDir()}, &stdout, &stderr)
	if code == 2 {
		t.Errorf("run = 2 (%s), want the default --config not to count as explicit", stderr.String())
	}
}

func TestControllerRejectsAnExplicitConfigBesideARepository(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"controller", "--config", "/etc/applications.yaml",
		"--appset-repo", "https://example.com/apps.git", "--appset-revision", "main",
		"--appset-path", "applications.yaml"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--config") {
		t.Errorf("stderr = %q, want it to name the flag", stderr.String())
	}
}

// The app set is a file inside a root, in both modes, and the directory mode is
// the only one with a name this end can guess.
func TestAppSetSourceDescribesWhereTheSetLives(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    options
		want string
	}{
		{
			"a repository names its revision and file",
			options{appSetRepo: "https://example.com/apps.git", appSetRevision: "main", appSetPath: "apps/applications.yaml"},
			"https://example.com/apps.git @ main (apps/applications.yaml)",
		},
		{
			"a directory defaults the file",
			options{appSetDir: "/var/lib/appset"},
			"/var/lib/appset (applications.yaml)",
		},
		{
			"a directory keeps a file it was given",
			options{appSetDir: "/var/lib/appset", appSetPath: "prod.yaml"},
			"/var/lib/appset (prod.yaml)",
		},
		{
			"the mounted file is itself the description",
			options{configPath: "/etc/swarmcli-cd/applications.yaml"},
			"/etc/swarmcli-cd/applications.yaml",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, got := appSetSource(tc.o, git.Auth{}, t.TempDir())
			if src == nil {
				t.Fatal("appSetSource returned no loader")
			}
			if got != tc.want {
				t.Errorf("description = %q, want %q", got, tc.want)
			}
		})
	}
}

// A set something else produces may simply not be ready yet: a repository
// briefly unreachable at boot, or a sidecar that has not finished its first
// sync. Exiting would restart-loop the controller under Swarm and take the API
// that could explain it down with it, so it starts with no applications and
// retries.
func TestServeStartsWhenASourcedAppSetCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    func(data string) options
	}{
		{"an unreachable repository", func(data string) options {
			return options{
				listen: testListen, dataDir: data, appSetInterval: appset.DefaultInterval,
				appSetRepo:     filepath.Join(data, "no-such-repo.git"),
				appSetRevision: "main",
				appSetPath:     "applications.yaml",
			}
		}},
		{"a directory nothing has written yet", func(data string) options {
			return options{
				listen: testListen, dataDir: data, appSetInterval: appset.DefaultInterval,
				appSetDir: filepath.Join(data, "not-synced-yet"),
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapAuthorizer(t, readyAuthorizer{})
			data := t.TempDir()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			if err := serve(ctx, tc.o(data), discardLog()); err != nil {
				t.Fatalf("serve = %v, want it to start anyway and retry", err)
			}
		})
	}
}

// The app set's clone must not share a root with the applications': everything
// under a clone is force-checked-out and cleaned on every fetch.
func TestServeCreatesTheAppSetCloneRoot(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})
	data := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := serve(ctx, options{
		listen: testListen, dataDir: data, appSetInterval: appset.DefaultInterval,
		appSetRepo:     filepath.Join(data, "no-such-repo.git"),
		appSetRevision: "main",
		appSetPath:     "applications.yaml",
	}, discardLog())
	if err != nil {
		t.Fatalf("serve = %v, want nil", err)
	}

	for _, dir := range []string{"repos", "charts", "appset"} {
		if _, err := os.Stat(filepath.Join(data, dir)); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
}

// The flags an operator cannot guess are the app-set ones, so the help has to
// name them rather than leaving them to the docs.
func TestControllerHelpNamesTheAppSetFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	for _, flagName := range []string{"--appset-repo", "--appset-revision", "--appset-dir", "--appset-path", "--appset-interval"} {
		if !strings.Contains(stdout.String(), flagName) {
			t.Errorf("stdout = %q, want it to name %s", stdout.String(), flagName)
		}
	}
}

// Every application that names no syncPolicy.interval of its own runs on this
// one, and until #160 nothing could set it: the app set is the untrusted tier,
// so an operator wanting the whole controller to reconcile faster had to write
// the number into every application in it.
func TestControllerTakesAReconcileInterval(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "--reconcile-interval") {
		t.Errorf("stdout = %q, want it to name --reconcile-interval", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	// Zero is refused rather than defaulted: reconcile.New reads a non-positive
	// interval as "use the default", so accepting it would hand an operator who
	// asked for none the three-minute one. Asserted on the refusal's own words,
	// because a flag that was never registered is also a usage error naming the
	// flag — which is what this test used to accept.
	if code := run([]string{"controller", "--reconcile-interval", "0", "--data", t.TempDir()}, &stdout, &stderr); code != 2 {
		t.Errorf("run = %d, want 2 for an interval of zero", code)
	}
	if !strings.Contains(stderr.String(), "--reconcile-interval must be positive") {
		t.Errorf("stderr = %q, want the refusal to say --reconcile-interval must be positive", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	// And a duration it accepts gets past the usage layer. This fails later, on
	// the unconfigured authorizer, which is exit 1 rather than 2.
	if code := run([]string{"controller", "--reconcile-interval", "90s", "--data", t.TempDir()}, &stdout, &stderr); code == 2 {
		t.Errorf("run = 2 (%s), want --reconcile-interval 90s to be accepted", stderr.String())
	}
}

// D15: the UI is served unless an operator says otherwise, and saying
// otherwise is a supported configuration rather than a usage error.
func TestControllerTakesTheUiFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	for _, want := range []string{"--ui", "default true"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to mention %q", stdout.String(), want)
		}
	}

	stdout.Reset()
	stderr.Reset()
	// Accepted, and failing later on the unconfigured authorizer, which is exit
	// 1 rather than the 2 a usage error gives.
	if code := run([]string{"controller", "--ui=false", "--data", t.TempDir()}, &stdout, &stderr); code == 2 {
		t.Errorf("run = 2 (%s), want --ui=false to be accepted", stderr.String())
	}
}

// Prune is the one destructive setting, so the help has to say both that it
// exists and what turning it on costs.
func TestControllerHelpNamesThePruneFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	for _, want := range []string{"--prune", "--prune-volumes", "--controller-id", "outage"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to mention %q", stdout.String(), want)
		}
	}
}

func TestControllerHelpNamesTheLogFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	for _, want := range []string{"--log-level", "--log-format"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to name %s", stdout.String(), want)
		}
	}
}

// A misspelled level must not leave the controller running at the default one:
// an operator who asked for debug and silently got info would conclude the
// thing they are chasing does not log at all.
func TestControllerRejectsAnUnusableLogLevelOrFormat(t *testing.T) {
	for _, tc := range []struct{ flagName, value string }{
		{"--log-level", "verbose"},
		{"--log-format", "logfmt"},
	} {
		t.Run(tc.flagName, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{"controller", tc.flagName, tc.value}, &stdout, &stderr); code != 2 {
				t.Fatalf("run = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tc.flagName) {
				t.Errorf("stderr = %q, want it to name %s", stderr.String(), tc.flagName)
			}
		})
	}
}

func TestNewLoggerWritesTheSelectedFormat(t *testing.T) {
	for _, tc := range []struct{ format, want string }{
		{"text", `level=INFO msg=reconcile application=whoami`},
		{"json", `"msg":"reconcile","application":"whoami"`},
	} {
		t.Run(tc.format, func(t *testing.T) {
			var out bytes.Buffer
			log, err := newLogger(&out, "info", tc.format)
			if err != nil {
				t.Fatal(err)
			}
			log.Info("reconcile", "application", "whoami")
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("wrote %q, want it to contain %q", out.String(), tc.want)
			}
		})
	}
}

func TestTheLogLevelFiltersWhatIsWritten(t *testing.T) {
	var out bytes.Buffer
	log, err := newLogger(&out, "warn", "text")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("reconcile")
	log.Warn("drifted")
	if strings.Contains(out.String(), "reconcile") {
		t.Errorf("wrote %q, want the info line dropped at warn", out.String())
	}
	if !strings.Contains(out.String(), "drifted") {
		t.Errorf("wrote %q, want the warn line kept", out.String())
	}
}

// Issue #70: the process must log in one format. Most components are handed
// the logger, but notify's log notifier is registered from an init() where
// none exists yet and reaches for slog.Default instead — which, unset, is the
// standard log package's own format on the same stream. So runController has
// to set it, and this is the regression test for that one line.
func TestTheControllerSetsTheDefaultLogger(t *testing.T) {
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	// serve stops on the unready authorizer, which is well after the logger has
	// been built and installed. Nothing here needs it to get further.
	swapAuthorizer(t, unreadyAuthorizer{})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--log-format", "json"}, &stdout, &stderr); code == 0 {
		t.Fatal("run = 0, want the unready authorizer to have failed it")
	}

	stderr.Reset()
	slog.Default().Info("reconcile", "application", "whoami")
	if !strings.Contains(stderr.String(), `"msg":"reconcile"`) {
		t.Errorf("slog.Default wrote %q, want it to go through the controller's handler", stderr.String())
	}
}

// The chart engine writes its warnings for a CLI's stderr, so they end in a
// newline. A slog message must not: the handler escapes the trailing one rather
// than ending the line, and the operator reads a `\n` at the end of the warning
// — the same complaint as #157, from the other direction.
func TestAChartWarningLosesItsTrailingNewline(t *testing.T) {
	var buf bytes.Buffer
	warnf := chartWarnf(slog.New(slog.NewTextHandler(&buf, nil)))
	warnf("could not refresh repository '%s' (%v); using the cached index\n", "eldara", io.EOF)

	got := buf.String()
	if strings.Contains(got, `\n`) {
		t.Errorf("warning logged as %q, want no escaped newline in it", got)
	}
	if !strings.Contains(got, "using the cached index") {
		t.Errorf("warning logged as %q, want the engine's text carried through", got)
	}
}

// Issue #72: the CE packages the applier is built on log through swarmcli's
// own logger, which stays a no-op until something initialises it. Nothing here
// did, so every diagnostic they wrote — docker.SnapshotWith's included — went
// nowhere. They belong in the controller's stream, in its format.
func TestTheControllerRoutesCELogsThroughItsHandler(t *testing.T) {
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	// swarmlog has no way to restore a previous logger, and leaving it pointed
	// at a buffer that has gone out of scope would make a later test's CE call
	// write into it. Discard is the honest reset.
	t.Cleanup(func() { swarmlog.InitSlog(slog.NewTextHandler(io.Discard, nil)) })
	swapAuthorizer(t, unreadyAuthorizer{})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--log-format", "json"}, &stdout, &stderr); code == 0 {
		t.Fatal("run = 0, want the unready authorizer to have failed it")
	}

	stderr.Reset()
	swarmlog.L().Warnf("snapshot: cli.Info failed (%s)", "cluster id unavailable")
	got := stderr.String()
	if !strings.Contains(got, `"msg":"snapshot: cli.Info failed (cluster id unavailable)"`) {
		t.Errorf("a CE log line wrote %q, want it in the controller's format", got)
	}
	if !strings.Contains(got, `"level":"WARN"`) {
		t.Errorf("a CE log line wrote %q, want its level carried over", got)
	}
}

// Criterion 13. One flag without the other refuses to start, exit 2, naming the
// missing half first — the failure it otherwise produces is a listener that came
// up in plaintext while the operator believed it had not.
func TestControllerRefusesHalfATLSPair(t *testing.T) {
	for _, tc := range []struct{ given, missing string }{
		{"--tls-cert", "--tls-key"},
		{"--tls-key", "--tls-cert"},
	} {
		t.Run(tc.given, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"controller", tc.given, "/tls/half", "--data", t.TempDir()}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("run = %d, want 2", code)
			}
			if !strings.HasPrefix(stderr.String(), "Error: "+tc.missing) {
				t.Errorf("stderr = %q, want it to name %s first", stderr.String(), tc.missing)
			}
		})
	}
}

func TestControllerHelpNamesTheTLSFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"controller", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	// The restart is the part an operator cannot guess: the pair is read once,
	// so a renewed certificate is not picked up until the controller is
	// restarted.
	for _, want := range []string{"--tls-cert", "--tls-key", "restart"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to mention %q", stdout.String(), want)
		}
	}
}

// The pair is loaded before the listener is built, so a path that is not there
// is a named startup error beside the authorizer check — not one arriving
// through runUntilStopped's channel with the reconciler already deploying.
func TestServeRefusesAnUnreadableTLSPair(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})

	o := staticOptions(writeConfig(t), t.TempDir())
	o.listen = freePort(t)
	o.tlsCert = filepath.Join(t.TempDir(), "absent.crt")
	o.tlsKey = filepath.Join(t.TempDir(), "absent.key")

	err := serve(context.Background(), o, discardLog())
	if err == nil {
		t.Fatal("serve = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "--tls-cert") {
		t.Errorf("serve = %v, want it to name the flags", err)
	}
	// And it refused before binding, which is what makes the refusal worth
	// anything: binding the address ourselves succeeds only if nothing holds it.
	ln, bindErr := net.Listen("tcp", o.listen)
	if bindErr != nil {
		t.Fatalf("something is listening on %s: %v", o.listen, bindErr)
	}
	_ = ln.Close()
}

// TestTheHealthcheckStaysHealthyWithTLSOn is the criterion issue #171 exists
// for, and it is asserted end to end: the real serve(), a real TLS listener and
// the real healthcheck subcommand.
//
// stack.yml runs `swarmcli-cd healthcheck`, which probes client.DefaultServer —
// http://127.0.0.1:8080, hard-coded. Against a TLS listener that probe fails, so
// Swarm marks the task unhealthy and restarts it, forever, with the controller
// working perfectly throughout and nothing in the logs saying TLS.
//
// Three probes rather than one because the stack file's healthcheck declares
// retries: 3 — a probe that passed once and then failed would restart the task
// just the same, and would pass a single-shot test.
func TestTheHealthcheckStaysHealthyWithTLSOn(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})
	certPath, keyPath := selfSignedPair(t)

	o := staticOptions(writeOfflineConfig(t), t.TempDir())
	o.listen = freePort(t)
	o.tlsCert, o.tlsKey = certPath, keyPath

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- serve(ctx, o, discardLog()) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-stopped:
			if err != nil {
				t.Errorf("serve = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not return")
		}
	})

	// The URL stack.yml's TLS example sets SWARMCLI_CD_SERVER to. Nothing tells
	// the probe about the certificate: the loopback rule is what carries it.
	server := "https://" + o.listen
	waitListening(t, o.listen)

	for probe := range 3 {
		code, stdout, stderr := cli(t, server, "healthcheck")
		if code != 0 {
			t.Fatalf("probe %d: exit = %d, want 0 (stderr: %s)", probe, code, stderr)
		}
		if !strings.Contains(stdout, "ok") {
			t.Errorf("probe %d: stdout = %q, want ok", probe, stdout)
		}
	}
}

// waitListening blocks until the server under test has finished binding, so the
// first probe is not racing the listener it is aimed at. Accepting a connection
// is the right signal for both protocols: ListenAndServeTLS wraps the listener
// before it starts accepting, so a TCP connect that succeeds means TLS is up.
func waitListening(t *testing.T, addr string) {
	t.Helper()
	for range 500 {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing is listening on %s", addr)
}

// selfSignedPair writes the certificate and key an operator following
// stack.yml's TLS example creates. The loopback SANs are what let a client that
// was handed the certificate verify it at all.
func selfSignedPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "swarmcli-cd test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	write := func(path, blockType string, body []byte) {
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: body}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(certPath, "CERTIFICATE", der)
	write(keyPath, "PRIVATE KEY", keyDER)
	return certPath, keyPath
}

// testListen binds an ephemeral port on the loopback: the tests need a real
// listener but must not collide with anything, least of all each other.
const testListen = "127.0.0.1:0"

// staticOptions is the pre-#53 bootstrap: a file mounted at deploy time and no
// app-set selector.
func staticOptions(configPath, dataDir string) options {
	return options{
		configPath:     configPath,
		listen:         testListen,
		dataDir:        dataDir,
		appSetInterval: appset.DefaultInterval,
		controllerID:   application.DefaultControllerID,
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeOfflineConfig is writeConfig for a test that lets the controller run
// rather than cancelling it at once: the source is a path on this machine, so
// the reconcile that fires at startup fails locally and instantly instead of
// reaching for a repository on the internet. A set with no applications is not
// the alternative — the static loader refuses one.
func writeOfflineConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "applications.yaml")
	body := "applications:\n" +
		"  - name: edge\n" +
		"    source:\n" +
		"      repoURL: " + filepath.Join(dir, "no-such-repo.git") + "\n" +
		"      revision: main\n" +
		"      releaseFile: swarmcli-release.yaml\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeConfig writes the smallest applications file the loader accepts.
func writeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "applications.yaml")
	body := "applications:\n" +
		"  - name: edge\n" +
		"    source:\n" +
		"      repoURL: https://example.com/infra.git\n" +
		"      revision: main\n" +
		"      releaseFile: swarmcli-release.yaml\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// swapAuthorizer installs an authorizer for the duration of one test. The
// seam replaces rather than appends, so the original has to be put back.
func swapAuthorizer(t *testing.T, a authz.Authorizer) {
	t.Helper()
	original, originalName := authz.Get(), authz.Active()
	t.Cleanup(func() { authz.Register(originalName, original) })
	authz.Register("test", a)
}

type readyAuthorizer struct{ authz.Authorizer }

func (readyAuthorizer) Ready() error { return nil }

type unreadyAuthorizer struct{ authz.Authorizer }

func (unreadyAuthorizer) Ready() error { return authz.ErrNoToken }

// countingFetcher stands in for the git side so a test can watch the reconcile
// loop turn over. It is the cheapest observable that the reconciler is still
// running: nothing else it does is visible from this package.
type countingFetcher struct{ calls atomic.Int64 }

func (f *countingFetcher) Fetch(_ context.Context, app string, _ application.Source) (git.Checkout, error) {
	f.calls.Add(1)
	return git.Checkout{Dir: "/tmp/" + app, Revision: strings.Repeat("a", 40)}, nil
}

// TestTheListenerIsDrainedBeforeTheReconcilerStops is the shutdown ordering.
//
// One context used to cancel the reconciler the moment the signal arrived and
// only then drain the listener, so the gap between the two was a window in which
// a POST /sync was still accepted and spawned a detached, uncancellable deploy
// after the reconciler had provably stopped — with Shutdown seeing nothing in
// flight and reporting a clean exit over a process leaving mid-apply.
//
// What is asserted is the inverse: while a request is still in flight and the
// listener is therefore still draining, the reconciler has not been stopped.
//
// The TLS row is also the evidence D23 stands on. Server.ServeTLS calls
// setupHTTP2_ServeTLS, so Go's client negotiates h2 against the API and the
// event stream runs over it — and h2's graceful shutdown goes through the
// bundled http2 server's GOAWAY hook rather than closeIdleConns, with an h2
// connection counting as idle only once it has no open streams, which is
// exactly what srv.Drain closes. The protocol is asserted rather than assumed,
// because a row that silently fell back to HTTP/1.1 would prove nothing about
// the thing enabling TLS turns on.
func TestTheListenerIsDrainedBeforeTheReconcilerStops(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tls       bool
		wantProto string
	}{
		{"plaintext", false, "HTTP/1.1"},
		{"TLS, and therefore h2", true, "HTTP/2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &countingFetcher{}
			rec := reconcile.New([]application.Spec{{
				Name:           "edge",
				Source:         application.Source{RepoURL: "https://example.com/x.git", Revision: "main"},
				DriftDetection: application.DriftManifest,
			}}, reconcile.Options{
				Fetcher:  fetcher,
				Builder:  failingBuilder{},
				Interval: 5 * time.Millisecond,
				Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			})

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, defaultAppSetFile), []byte("applications: []\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			loop := appset.NewLoop(appset.NewPath(appset.PathConfig{Dir: dir, Path: defaultAppSetFile}), rec,
				appset.LoopOptions{Interval: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

			entered, release := make(chan struct{}), make(chan struct{})
			mux := http.NewServeMux()
			mux.HandleFunc("/hold", func(w http.ResponseWriter, _ *http.Request) {
				close(entered)
				<-release
				_, _ = w.Write([]byte("done"))
			})

			// runUntilStopped is what binds — it calls listenAndServe — so the
			// address goes on the server rather than the test serving a listener
			// of its own. An http.Server runs once, and doing both left Addr
			// empty, which means ":80" and is not bindable on a CI runner.
			addr := freePort(t)
			httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: time.Second}
			scheme, probeClient := "http", http.DefaultClient
			if tc.tls {
				certPath, keyPath := selfSignedPair(t)
				cfg, err := tlsConfigFor(options{tlsCert: certPath, tlsKey: keyPath})
				if err != nil {
					t.Fatal(err)
				}
				httpSrv.TLSConfig = cfg
				scheme, probeClient = "https", loopbackTLSClient()
			}

			ctx, cancel := context.WithCancel(context.Background())
			stopped := make(chan error, 1)
			go func() {
				stopped <- runUntilStopped(ctx, rec, loop, httpSrv, slog.New(slog.NewTextHandler(io.Discard, nil)))
			}()

			probed := make(chan string, 1)
			go func() {
				resp, err := waitForGet(probeClient, scheme+"://"+addr+"/hold")
				if err != nil {
					probed <- "no response: " + err.Error()
					return
				}
				_ = resp.Body.Close()
				probed <- resp.Proto
			}()
			<-entered

			// The signal, with a request still in flight.
			cancel()

			// The reconciler must keep going while the listener drains. Measured
			// as progress rather than as a state, because progress is what a
			// stopped loop cannot fake.
			before := fetcher.calls.Load()
			time.Sleep(200 * time.Millisecond)
			if after := fetcher.calls.Load(); after == before {
				t.Fatalf("the reconciler stopped at %d fetches while the listener was still draining", after)
			}

			close(release)
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatalf("runUntilStopped = %v, want nil", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("runUntilStopped did not return")
			}

			select {
			case got := <-probed:
				if got != tc.wantProto {
					t.Errorf("the in-flight request finished as %s, want %s", got, tc.wantProto)
				}
			case <-time.After(10 * time.Second):
				t.Error("the in-flight request never finished, so it was not drained")
			}
		})
	}
}

// loopbackTLSClient talks to the test's own self-signed listener. It is not
// client.NewWithOptions: this test is about the drain and the protocol, and
// borrowing the production client would make a failure here ambiguous between
// the two.
func loopbackTLSClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
		// What makes the row an h2 row: without it the transport offers only
		// http/1.1 in ALPN and the server, which does support h2, agrees.
		ForceAttemptHTTP2: true,
	}}
}

// failingBuilder ends every reconcile immediately after the fetch, so the loop
// turns over quickly and touches no swarm.
type failingBuilder struct{}

func (failingBuilder) Build(context.Context, string, application.Source, git.Checkout) (*source.Built, error) {
	return nil, errors.New("not building anything in this test")
}

// freePort returns an address nothing is listening on, for a server that has to
// do its own binding. Closing before handing the address on leaves a window; it
// is a loopback ephemeral port inside one test process, which is as small as
// that window gets without changing what is under test.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// waitForGet retries until the server under test has finished binding, so the
// request is not racing the listener it is aimed at.
func waitForGet(c *http.Client, url string) (*http.Response, error) {
	var err error
	for range 200 {
		var resp *http.Response
		if resp, err = c.Get(url); err == nil {
			return resp, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, err
}

// The line an operator greps when a controller does not behave has to name the
// build. Whether this one knows a key the app set uses was the whole of #234,
// and answering it meant a shell inside the container.
func TestTheStartupLineNamesTheBuild(t *testing.T) {
	swapAuthorizer(t, readyAuthorizer{})
	data := t.TempDir()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := serve(ctx, options{
		listen: testListen, dataDir: data, appSetInterval: appset.DefaultInterval,
		appSetDir: filepath.Join(data, "not-synced-yet"),
	}, log)
	if err != nil {
		t.Fatalf("serve = %v, want nil", err)
	}

	out := buf.String()
	if !strings.Contains(out, "version="+version) {
		t.Errorf("the startup line does not name this build's version:\n%s", out)
	}
	// The engine is the other half: a chart's swarmcliVersion resolves against
	// it and not against the line above, so one without the other names the
	// wrong number for half the questions asked of it.
	if !strings.Contains(out, "engine=") {
		t.Errorf("the startup line does not name the chart engine:\n%s", out)
	}
}
