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
	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/appset"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
	"github.com/Eldara-Tech/swarmcli-cd/backend"
	"github.com/Eldara-Tech/swarmcli-cd/config"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
	"github.com/Eldara-Tech/swarmcli-cd/prune"
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

// defaultAppSetFile is what the set is called in a directory an external
// process keeps current, when the deployment does not say. There is no
// equivalent default for a repository: a file inside somebody else's layout has
// no name this end can guess.
const defaultAppSetFile = "applications.yaml"

// shutdownTimeout bounds how long in-flight requests have to finish once a
// signal has arrived. Swarm sends SIGKILL ten seconds after SIGTERM by default,
// so anything longer would be cut off mid-drain.
const shutdownTimeout = 5 * time.Second

var controllerUsage = `Usage: swarmcli-cd controller [options]

Runs the reconcile loop and serves the HTTP API until it is signalled.

Options:
  --config <path>   Applications file (default ` + defaultConfigPath + `)
  --listen <addr>   API listen address (default ` + defaultListen + `)
  --data <dir>      Repository clones and chart cache (default ` + defaultDataDir + `)

The application set is the mounted --config file unless one of these points it
somewhere that can change without a redeploy. Which one is set selects the mode;
setting both, or either alongside an explicit --config, is an error.

  --appset-repo <url>       Pull the set from this repository
  --appset-revision <ref>   Branch, tag or SHA to track (required with --appset-repo)
  --appset-dir <dir>        Read the set from a directory something else keeps current
  --appset-path <file>      The set's path within the repository or the directory
                            (required with --appset-repo; default ` + defaultAppSetFile + ` with --appset-dir)
  --appset-interval <dur>   How often the set is re-read (default ` + appset.DefaultInterval.String() + `)

An application that leaves the set stops being reconciled and its stack is left
running, reported as orphaned. Prune deletes it instead. It is off by default,
and with it on, removing an application from the set is an outage rather than a
pause:

  --prune                   Delete the resources of an application that has
                            left the app set
  --prune-volumes           Also delete its named volumes, which is the one part
                            nothing can restore (requires --prune)
  --controller-id <id>      This controller's identity, stamped on every release
                            it installs (default ` + application.DefaultControllerID + `). Two controllers
                            sharing a swarm MUST be given distinct ids, or each
                            will see the other's applications as departed and,
                            with --prune, delete them

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
	var o options
	fs.StringVar(&o.configPath, "config", defaultConfigPath, "")
	fs.StringVar(&o.listen, "listen", defaultListen, "")
	fs.StringVar(&o.dataDir, "data", defaultDataDir, "")
	fs.StringVar(&o.appSetRepo, "appset-repo", "", "")
	fs.StringVar(&o.appSetRevision, "appset-revision", "", "")
	fs.StringVar(&o.appSetDir, "appset-dir", "", "")
	fs.StringVar(&o.appSetPath, "appset-path", "", "")
	fs.DurationVar(&o.appSetInterval, "appset-interval", appset.DefaultInterval, "")
	fs.BoolVar(&o.prune, "prune", false, "")
	fs.BoolVar(&o.pruneVolumes, "prune-volumes", false, "")
	fs.StringVar(&o.controllerID, "controller-id", application.DefaultControllerID, "")

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
	// --config always has a value, so "the operator set it" is a question only
	// the flag set can answer. It matters because pointing the controller at a
	// mounted file and at a repository at the same time says two things.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			o.configSet = true
		}
	})
	if err := o.validate(); err != nil {
		return usageErr(stderr, err.Error(), controllerUsage)
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))

	// Both signals stop the process the same way. Swarm sends SIGTERM on
	// `service update` and on rollout; SIGINT is what a terminal sends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, o, log); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// options is what runController parsed.
type options struct {
	configPath string
	listen     string
	dataDir    string

	// The app-set selectors. Which of appSetRepo and appSetDir is set chooses
	// the mode; neither leaves the mounted configPath, which is what every
	// deployment before this one does.
	appSetRepo     string
	appSetRevision string
	appSetDir      string
	appSetPath     string
	appSetInterval time.Duration

	// prune deletes the resources of an application that has left the app set,
	// and pruneVolumes extends that to its named volumes. Controller-wide
	// rather than per-application because a departed application's spec is
	// gone: there is nothing left to carry the setting.
	prune        bool
	pruneVolumes bool

	// controllerID is this controller's half of the owner stamp. It bounds what
	// prune will even consider, so it is what keeps two controllers on one
	// swarm from deleting each other's applications.
	controllerID string

	// configSet records that --config was given explicitly rather than
	// defaulted. See runController.
	configSet bool
}

// serve wires the packages together and runs until ctx ends or a component
// fails. It is the only place in the repository that knows how the whole thing
// fits together.
func serve(ctx context.Context, o options, log *slog.Logger) error {
	// First, and before any I/O: an unconfigured authorizer that merely rejects
	// every request looks, to an operator, exactly like a wrong token, and
	// refusing to start names the problem instead. It goes ahead of the app-set
	// load because that load may now be a clone of a remote repository, and
	// spending its timeout before refusing for an unrelated reason helps nobody.
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

	// Three directories, not one. Everything under a clone is force-checked-out
	// and cleaned on every fetch, so a chart cache living there would be deleted
	// underneath the builder — or show up as repository content. The app set's
	// clone is separate again: it is the bootstrap tier, and keeping it out of
	// the applications' root is what makes its cache key unambiguous.
	repos := filepath.Join(o.dataDir, "repos")
	chartCache := filepath.Join(o.dataDir, "charts")
	appSetRoot := filepath.Join(o.dataDir, "appset")
	dirs := []string{repos, chartCache}
	mode := o.mode()
	if mode == appSetModeGit {
		dirs = append(dirs, appSetRoot)
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	src, sourceDesc := appSetSource(o, auth, appSetRoot)

	// Fatal for a file mounted at deploy time: nothing will change under the
	// controller's feet, so a restart is the operator's notification and this is
	// what it has always done. Not fatal for a set something else produces — a
	// repository briefly unreachable at boot, or a git-sync sidecar that has not
	// finished its first sync, would otherwise become a restart loop, and under
	// it the API that could say so never comes up. So the controller starts with
	// no applications, says why on the status endpoint and in the log, and the
	// loop below retries every interval until the set arrives.
	cfg, _, err := src.Load(ctx)
	if err != nil {
		if mode == appSetModeStatic {
			return err
		}
		log.Error("the app set could not be loaded; starting with none and retrying",
			"mode", mode, "appSet", sourceDesc, "error", err)
		cfg = &config.File{}
	}

	// Per-application image-pull credentials, read from the Docker secrets each
	// application's registryAuth names. A missing or unparseable one is fatal
	// here rather than a convergence timeout three minutes into a deploy. An
	// application that joins later goes through the loop, which resolves its
	// credential the same way and fails that application alone.
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

	rec := reconcile.New(cfg.Applications, reconcile.Options{
		Fetcher: git.New(repos, auth),
		Builder: source.NewBuilder(chartCache, func(format string, a ...any) {
			log.Warn(fmt.Sprintf(format, a...))
		}),
		RegistryAuth:          resolvers,
		ForbiddenSecretMounts: forbidden,
		ControllerID:          o.controllerID,
		Log:                   log,
	})

	// The loop that keeps the running set equal to the app set. Watching an
	// immutable Docker config it changes nothing and still serves the status
	// endpoint; pointed at a repository it is the whole of app-of-apps. Same
	// program either way, which is the point of the two modes sharing a loader.
	// Constructed only when prune is enabled, so the nil the loop checks is the
	// report-only default rather than a flag read in two places.
	var pruner appset.Pruner
	if o.prune {
		pruner = prune.New(prune.Options{
			Volumes: o.pruneVolumes, ControllerID: o.controllerID, Log: log,
		})
	}

	loop := appset.NewLoop(src, rec, appset.LoopOptions{
		Mode:     mode,
		Source:   sourceDesc,
		Interval: o.appSetInterval,
		Log:      log,
		Pruner:   pruner,
	})

	srv := api.New(rec, api.Options{Log: log, Controller: loop})
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

	log.Info("starting",
		"applications", len(cfg.Applications), "listen", o.listen,
		"mode", mode, "appSet", sourceDesc,
		"prune", o.prune, "pruneVolumes", o.pruneVolumes, "controllerID", o.controllerID)
	return runUntilStopped(ctx, rec, loop, httpSrv, log)
}

// The modes the bootstrap can select, as the status endpoint reports them.
// They are the deployment's own word for where the set lives rather than the
// mechanism that happens to read it: static and path share a reader and are
// still two entirely different things to the operator who deployed one of them.
const (
	appSetModeStatic = "static"
	appSetModeGit    = "git"
	appSetModePath   = "path"
)

// mode reports which source the selectors chose. Every combination that would
// make this ambiguous has already been refused by validate.
func (o options) mode() string {
	switch {
	case o.appSetRepo != "":
		return appSetModeGit
	case o.appSetDir != "":
		return appSetModePath
	default:
		return appSetModeStatic
	}
}

// validate refuses a bootstrap that says two things, or half of one.
//
// Errors rather than a precedence rule: the bootstrap is the single anchor
// nothing in git can repoint (D-f of #47), and a controller that silently
// ignored half of it would be reconciling a set nobody pointed it at.
func (o options) validate() error {
	switch {
	case o.appSetRepo != "" && o.appSetDir != "":
		return errors.New("--appset-repo and --appset-dir are alternatives: one app set, one source")
	case o.configSet && o.appSetRepo != "":
		return errors.New("--config names a mounted file and --appset-repo a repository, so which one holds the app set is ambiguous")
	case o.configSet && o.appSetDir != "":
		return errors.New("--config names a mounted file and --appset-dir a directory, so which one holds the app set is ambiguous")
	case o.appSetRepo != "" && o.appSetRevision == "":
		return errors.New("--appset-repo needs --appset-revision: an unpinned app set would follow whatever the default branch happens to point at")
	case o.appSetRepo != "" && o.appSetPath == "":
		return errors.New("--appset-repo needs --appset-path: the app set's path within the repository")
	case o.appSetRevision != "" && o.appSetRepo == "":
		return errors.New("--appset-revision means nothing without --appset-repo")
	case o.appSetPath != "" && o.appSetRepo == "" && o.appSetDir == "":
		return errors.New("--appset-path means nothing without --appset-repo or --appset-dir")
	case o.appSetInterval <= 0:
		// Not silently defaulted: an operator writing 0 means something by it,
		// and "never re-read" is not a thing this controller can do.
		return errors.New("--appset-interval must be positive")
	case application.ValidateControllerID(o.controllerID) != nil:
		return fmt.Errorf("--controller-id: %w", application.ValidateControllerID(o.controllerID))
	case o.pruneVolumes && !o.prune:
		// Refused rather than resolved either way. Read as "prune with volumes"
		// it destroys data nobody asked to lose; read as "prune nothing" it
		// leaves an operator believing cleanup is on when it is not.
		return errors.New("--prune-volumes means nothing without --prune")
	}
	return nil
}

// appSetSource builds the loader the bootstrap selected and describes it the
// way the status endpoint reports it. The description is built once so the
// startup log line and the endpoint cannot disagree about what is being
// followed.
func appSetSource(o options, auth git.Auth, root string) (*appset.Loader, string) {
	switch o.mode() {
	case appSetModeGit:
		// Its own sourcer under its own root, sharing only the type and the
		// credential with the applications' — the app set is the bootstrap
		// tier, and its clone is not somewhere an application can reach.
		src := appset.NewGit(git.New(root, auth), appset.GitConfig{
			RepoURL:  o.appSetRepo,
			Revision: o.appSetRevision,
			Path:     o.appSetPath,
		})
		return src, fmt.Sprintf("%s @ %s (%s)", o.appSetRepo, o.appSetRevision, o.appSetPath)

	case appSetModePath:
		file := o.appSetPath
		if file == "" {
			file = defaultAppSetFile
		}
		return appset.NewPath(appset.PathConfig{Dir: o.appSetDir, Path: file}),
			fmt.Sprintf("%s (%s)", o.appSetDir, file)

	default:
		// The mounted file, read through the same source as everything else so
		// that a Docker config and a set kept current by something else are one
		// code path. A bare filename resolves against the working directory,
		// which is what a bare filename means everywhere else.
		dir, file := filepath.Split(o.configPath)
		if dir == "" {
			dir = "."
		}
		return appset.NewPath(appset.PathConfig{Dir: dir, Path: file}), o.configPath
	}
}

// runUntilStopped runs the reconciler, the app-set loop and the listener as
// peers and returns once all three have stopped. Whichever stops first stops the
// others: a listener that cannot bind must not leave a controller reconciling
// with no way to observe it, and a reconciler that returns must not leave an API
// serving views that will never update.
func runUntilStopped(ctx context.Context, rec *reconcile.Reconciler, loop *appset.Loop, httpSrv *http.Server, log *slog.Logger) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	errs := make(chan error, 3)

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
		err := loop.Run(ctx)
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

	return errors.Join(<-errs, <-errs, <-errs)
}
