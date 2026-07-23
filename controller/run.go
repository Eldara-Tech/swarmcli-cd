// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package controller

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/api"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/backend"
	"github.com/Eldara-Tech/swarmcli-cd/config"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
	"github.com/Eldara-Tech/swarmcli-cd/reconcile"
	"github.com/Eldara-Tech/swarmcli-cd/regauth"
	"github.com/Eldara-Tech/swarmcli-cd/secrets"
	"github.com/Eldara-Tech/swarmcli-cd/source"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

// Defaults for the in-swarm deployment: applications.yaml arrives as a Docker
// config, the data directory is a volume, and the API is reached over the
// stack's overlay network.
const (
	defaultConfigPath = "/etc/swarmcli-cd/applications.yaml"
	defaultListen     = ":8080"
	defaultDataDir    = "/var/lib/swarmcli-cd"
)

// shutdownTimeout bounds how long in-flight requests have to finish once a
// signal has arrived. Swarm sends SIGKILL ten seconds after SIGTERM by default,
// so anything longer would be cut off mid-drain.
const shutdownTimeout = 5 * time.Second

const controllerUsage = `Usage: swarmcli-cd controller [options]

Runs the reconcile loop and serves the HTTP API until it is signalled.

Options:
  --config <path>   Applications file (default ` + defaultConfigPath + `)
  --listen <addr>   API listen address (default ` + defaultListen + `)
  --data <dir>      Repository clones and chart cache (default ` + defaultDataDir + `)

Credentials come from the environment, not from flags, because they arrive as
Docker secrets and a flag would put them in "docker inspect" output and in argv:

  ` + authz.EnvTokenFile + `   API admin token, read from a file
  ` + authz.EnvToken + `        API admin token, given directly
  ` + git.EnvTokenFile + `     Git password/token, read from a file
  ` + git.EnvToken + `          Git password/token, given directly
  ` + git.EnvUsername + `       Git username
`

// runController parses the daemon's flags and runs it.
func runController(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	// The flag package's own reporting is discarded: help belongs on stdout and
	// a misuse belongs on stderr with the same usage text every other command
	// prints, which is not what its defaults do.
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", defaultConfigPath, "")
	listen := fs.String("listen", defaultListen, "")
	dataDir := fs.String("data", defaultDataDir, "")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, controllerUsage)
			return 0
		}
		return usageErr(stderr, err.Error(), controllerUsage)
	}
	if fs.NArg() > 0 {
		return usageErr(stderr, fmt.Sprintf("unexpected argument %q", fs.Arg(0)), controllerUsage)
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))

	// Both signals stop the process the same way. Swarm sends SIGTERM on
	// `service update` and on rollout; SIGINT is what a terminal sends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, options{configPath: *configPath, listen: *listen, dataDir: *dataDir}, log); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// options is what runController parsed.
type options struct {
	configPath string
	listen     string
	dataDir    string
}

// serve wires the packages together and runs until ctx ends or a component
// fails. It is the only place in the repository that knows how the whole thing
// fits together.
func serve(ctx context.Context, o options, log *slog.Logger) error {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}

	// Before anything else, and before the listener exists. An unconfigured
	// authorizer that merely rejects every request looks, to an operator,
	// exactly like a wrong token; refusing to start names the problem instead.
	if err := authz.Get().Ready(); err != nil {
		return fmt.Errorf("authorizer %q: %w", authz.Active(), err)
	}

	// So that an operator can tell from the logs whether the companion loaded,
	// rather than inferring it from behaviour. See docs/extensibility.md.
	// Companion seams register from init(), so they are all present by now; the
	// API's own event stream is not a seam and joins the notifier list below.
	log.Info("seams",
		"swarms", swarms.Active(),
		"authz", authz.Active(),
		"notify", notify.Active(),
		"secrets", secrets.Active(),
	)

	auth, err := git.AuthFromEnv(os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}

	// Per-application image-pull credentials, read from the Docker secrets each
	// application's registryAuth names. A missing or unparseable one is fatal
	// here rather than a convergence timeout three minutes into a deploy.
	resolvers, err := regauth.Load(cfg.Applications, regauth.DefaultSecretsDir, os.ReadFile)
	if err != nil {
		return err
	}

	// The controller's own secrets, which no reconciled stack may mount: a chart
	// declaring one as an external secret would otherwise read the admin token,
	// the git token or another application's registry credential.
	forbidden, err := backend.MountedSecretNames(regauth.DefaultSecretsDir)
	if err != nil {
		return err
	}

	// Two directories, not one. Everything under a clone is force-checked-out
	// and cleaned on every fetch, so a chart cache living there would be
	// deleted underneath the builder — or show up as repository content.
	repos := filepath.Join(o.dataDir, "repos")
	chartCache := filepath.Join(o.dataDir, "charts")
	for _, dir := range []string{repos, chartCache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	rec := reconcile.New(cfg.Applications, reconcile.Options{
		Fetcher: git.New(repos, auth),
		Builder: source.NewBuilder(chartCache, func(format string, a ...any) {
			log.Warn(fmt.Sprintf(format, a...))
		}),
		RegistryAuth:          resolvers,
		ForbiddenSecretMounts: forbidden,
		Log:                   log,
	})

	srv := api.New(rec, api.Options{Log: log})
	// api.New deliberately does not do this itself: notify is a seam.List that
	// appends, so a self-registering server would leave one live event stream
	// behind per server ever constructed.
	notify.Register("api", srv)

	httpSrv := &http.Server{
		Addr:    o.listen,
		Handler: srv.Handler(),
		// The controller holds write access to the swarm. An unbounded header
		// read is the cheapest way to tie up a connection slot on something
		// that valuable.
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Info("starting", "applications", len(cfg.Applications), "listen", o.listen, "config", cfg.Path)
	return runUntilStopped(ctx, rec, httpSrv, log)
}

// runUntilStopped runs the reconciler and the listener as peers and returns
// once both have stopped. Whichever stops first stops the other: a listener
// that cannot bind must not leave a controller reconciling with no way to
// observe it, and a reconciler that cannot resolve its destinations must not
// leave an API serving views that will never update.
func runUntilStopped(ctx context.Context, rec *reconcile.Reconciler, httpSrv *http.Server, log *slog.Logger) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	errs := make(chan error, 2)

	go func() {
		err := rec.Run(ctx)
		// The loop returns ctx.Err() when it was asked to stop. That is how it
		// reports a clean shutdown, not a failure — treating it as one would
		// make every SIGTERM a non-zero exit and, under Swarm, a restart loop.
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		stop()
		errs <- err
	}()

	go func() {
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		stop()
		errs <- err
	}()

	<-ctx.Done()
	log.Info("stopping")

	// Not ctx: it is already cancelled, and Shutdown given a cancelled context
	// returns immediately without draining anything.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("the API did not shut down cleanly", "error", err)
	}

	return errors.Join(<-errs, <-errs)
}
