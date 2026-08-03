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
	"github.com/Eldara-Tech/swarmcli-cd/extension"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/notify"
	"github.com/Eldara-Tech/swarmcli-cd/prune"
	"github.com/Eldara-Tech/swarmcli-cd/reclaim"
	"github.com/Eldara-Tech/swarmcli-cd/reconcile"
	"github.com/Eldara-Tech/swarmcli-cd/regauth"
	"github.com/Eldara-Tech/swarmcli-cd/secrets"
	"github.com/Eldara-Tech/swarmcli-cd/source"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"

	swarmlog "github.com/Eldara-Tech/swarmcli/utils/log"
)

// Defaults for the in-swarm deployment: applications.yaml arrives as a Docker
// config, the data directory is a volume, and the API is reached over the
// stack's overlay network.
const (
	defaultConfigPath = "/etc/swarmcli-cd/applications.yaml"
	defaultListen     = ":8080"
	defaultDataDir    = "/var/lib/swarmcli-cd"
)

// The log defaults. Text is what an operator reading `docker service logs`
// wants; json is for whatever ships them somewhere that parses them.
const (
	defaultLogLevel  = "info"
	defaultLogFormat = "text"
)

// defaultAppSetFile is what the set is called when nothing says otherwise: in a
// directory an external process keeps current, and in a checkout `validate` was
// pointed at. There is no equivalent default for a repository: a file inside
// somebody else's layout has no name this end can guess.
const defaultAppSetFile = "applications.yaml"

// shutdownTimeout bounds how long in-flight requests have to finish once a
// signal has arrived.
//
// Swarm sends SIGKILL ten seconds after SIGTERM by default, and that budget is
// now shared: the listener drains first and the reconciler drains after it, so
// this is the smaller half rather than the whole. Three seconds is enough
// because the one thing that used to sit here for the full timeout — an event
// stream, which never goes idle — is ended by RegisterOnShutdown instead of
// waited out.
const shutdownTimeout = 3 * time.Second

var controllerUsage = `Usage: swarmcli-cd controller [options]

Runs the reconcile loop and serves the HTTP API until it is signalled.

Options:
  --config <path>   Applications file (default ` + defaultConfigPath + `)
  --listen <addr>   API listen address (default ` + defaultListen + `)
  --data <dir>      Repository clones and chart cache (default ` + defaultDataDir + `)

  --reconcile-interval <dur>
                    How often an application is reconciled, for those that do
                    not set syncPolicy.interval (default ` + reconcile.DefaultInterval.String() + `). Every tick
                    costs a git fetch, a full render and a read of the swarm's
                    release records, so lowering it costs all three per
                    application

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

Everything the controller writes goes through one handler on stderr:

  --log-level <level>       debug, info, warn or error (default ` + defaultLogLevel + `)
  --log-format <format>     text or json (default ` + defaultLogFormat + `)

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
	fs.DurationVar(&o.interval, "reconcile-interval", reconcile.DefaultInterval, "")
	fs.StringVar(&o.appSetRepo, "appset-repo", "", "")
	fs.StringVar(&o.appSetRevision, "appset-revision", "", "")
	fs.StringVar(&o.appSetDir, "appset-dir", "", "")
	fs.StringVar(&o.appSetPath, "appset-path", "", "")
	fs.DurationVar(&o.appSetInterval, "appset-interval", appset.DefaultInterval, "")
	fs.BoolVar(&o.prune, "prune", false, "")
	fs.BoolVar(&o.pruneVolumes, "prune-volumes", false, "")
	fs.StringVar(&o.controllerID, "controller-id", application.DefaultControllerID, "")
	fs.StringVar(&o.logLevel, "log-level", defaultLogLevel, "")
	fs.StringVar(&o.logFormat, "log-format", defaultLogFormat, "")

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

	log, err := newLogger(stderr, o.logLevel, o.logFormat)
	if err != nil {
		return usageErr(stderr, err.Error(), controllerUsage)
	}
	// One handler for the whole process. Components are given the logger
	// explicitly, and SetDefault covers what cannot be: notify's log notifier
	// registers from an init() where no logger exists yet, and anything reaching
	// for slog.Default or the standard log package would otherwise write a
	// second format onto the same stream.
	slog.SetDefault(log)
	// The CE packages this controller imports log through swarmcli's own
	// zap-based logger, which is a no-op until something initialises it — so
	// everything they report has been discarded here since the applier was
	// written. Its own Init would open a rotating file under ~/.local/state,
	// which in a container nobody would ever read; this hands it the same
	// handler instead, so one process still means one format on one stream.
	// See issue #72.
	swarmlog.InitSlog(log.Handler())

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

	// interval is how often an application that names no syncPolicy.interval is
	// reconciled. Controller-wide and deliberately not floored by
	// application.MinInterval, unlike the per-application one: the app set is the
	// untrusted tier and this is set by whoever runs the controller, who is
	// entitled to say how hard their own machine works. See reconcile.intervalFor.
	interval time.Duration

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

	// logLevel and logFormat configure the process's one handler. Flags rather
	// than environment variables: only the credentials come from the
	// environment, and for the reason controllerUsage gives.
	logLevel  string
	logFormat string
}

// newLogger builds the handler everything logs through. An unusable level or
// format is a usage error rather than a silent fallback, for the reason
// --appset-interval is: an operator who wrote it meant it.
func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("--log-level %q: want debug, info, warn or error", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("--log-format %q: want text or json", format)
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
		// Which modules registered routes, not which routes: what is actually
		// served cannot be known until the mux is built, which is a different
		// moment and one that can fail. See the "routes" line below.
		"extension", extension.Active(),
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
		Interval:              o.interval,
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
		// Unconditional, unlike the pruner. It deletes this controller's own
		// caches of an application that has left the set — nothing deployed, and
		// nothing that is not re-fetched the moment that name comes back — and
		// the volume they fill is a manager's, shared with the raft log. The
		// app-set root is not swept: it is the bootstrap tier and is not a name
		// the set can name.
		Reclaimer: reclaim.New(reclaim.Options{Roots: []string{repos, chartCache}, Log: log}),
	})

	srv := api.New(rec, api.Options{Log: log, Controller: loop})
	// api.New deliberately does not do this itself: notify is a seam.List that
	// appends, so a self-registering server would leave one live event stream
	// behind per server ever constructed.
	notify.Register("api", srv)

	// Building the mux is what reads the extension seam, so this is where a
	// companion's route set is either accepted or found unservable — a pattern
	// colliding with a core route or with another module's, a route naming no
	// action, a nil handler. Refusing here is the same judgement the authorizer
	// check at the top of this function makes: both alternatives reach an
	// operator as an outage rather than as a controller that said what was wrong
	// and stopped, since net/http panics on a duplicate pattern deep inside
	// wiring and a nil handler panics on the first request that finds it.
	h, err := srv.Handler()
	if err != nil {
		return fmt.Errorf("api routes: %w", err)
	}

	// What is served, now that it is settled. The guarded routes are one line
	// because a guarded route is unremarkable.
	routes := srv.Routes()
	guardedRoutes := make([]string, 0, len(routes))
	for _, rt := range routes {
		if !rt.Public {
			guardedRoutes = append(guardedRoutes, rt.Pattern)
		}
	}
	log.Info("routes", "guarded", guardedRoutes)
	for _, rt := range routes {
		// An extension's public route only. GET /healthz is public too, in every
		// build, and no deployment can change that — so warning about it would
		// put a WARN nobody can act on in every startup of every controller,
		// which is precisely how an operator learns to ignore the one that
		// matters: an unauthenticated endpoint a companion added to a process
		// holding the docker socket.
		if !rt.Public || rt.Extension == "" {
			continue
		}
		log.Warn("public route", "route", rt.Pattern, "extension", rt.Extension)
	}

	httpSrv := &http.Server{
		Addr:    o.listen,
		Handler: h,
		// The controller holds write access to the swarm. An unbounded header
		// read is the cheapest way to tie up a connection slot on something
		// that valuable.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shutdown waits for connections to go idle and an event stream never does,
	// so without this every shutdown with a UI attached burned the whole timeout
	// and then reported a failure that had not happened.
	httpSrv.RegisterOnShutdown(srv.Drain)

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
	case o.interval <= 0:
		// The same judgement, and the same reason: reconcile.New treats a
		// non-positive interval as "use the default", so accepting one here would
		// silently give an operator three minutes where they asked for none.
		return errors.New("--reconcile-interval must be positive")
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
// The two are stopped in that order and not together, which is the whole of the
// shutdown fix. One context used to cancel the reconciler the moment the signal
// arrived and only then drain the listener, so the gap between them was a window
// in which a POST /sync was still accepted, spawned a detached deploy, and ran
// on after the reconciler had provably stopped — with Shutdown seeing nothing in
// flight and reporting a clean exit over a process leaving mid-apply.
//
// So the listener goes down first: it is the only thing that can create new
// work, and once it is drained the reconciler cannot be given anything more to
// do. Then the reconciler is cancelled and drains what it already had.
func runUntilStopped(ctx context.Context, rec *reconcile.Reconciler, loop *appset.Loop, httpSrv *http.Server, log *slog.Logger) error {
	// stopping is the "something ended" signal, whichever peer ends first.
	stopping, stopAll := context.WithCancel(ctx)
	defer stopAll()

	// The reconcile side outlives it deliberately, so it is not cancelled by the
	// same signal that starts the listener's drain.
	recCtx, stopReconciling := context.WithCancel(context.WithoutCancel(ctx))
	defer stopReconciling()

	errs := make(chan error, 3)

	go func() {
		err := rec.Run(recCtx)
		// The loop returns ctx.Err() when it was asked to stop. That is how it
		// reports a clean shutdown, not a failure — treating it as one would
		// make every SIGTERM a non-zero exit and, under Swarm, a restart loop.
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		stopAll()
		errs <- err
	}()

	go func() {
		err := loop.Run(recCtx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		stopAll()
		errs <- err
	}()

	go func() {
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		stopAll()
		errs <- err
	}()

	<-stopping.Done()
	log.Info("stopping")

	// Not the signal context: it is already cancelled, and Shutdown given a
	// cancelled context returns immediately without draining anything.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("the API did not shut down cleanly", "error", err)
	}

	// Only now, with nothing able to ask for more.
	stopReconciling()

	return errors.Join(<-errs, <-errs, <-errs)
}
