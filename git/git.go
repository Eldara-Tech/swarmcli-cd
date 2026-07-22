// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package git is the pull half of GitOps: it fetches an application's
// repository and pins it to a commit.
//
// It uses go-git rather than shelling out. The controller image should carry no
// binaries it does not need — the same reasoning that made swarmcli-cd
// implement charts.Backend on the moby client rather than ship the docker CLI —
// and an in-process client is testable against a fixture repository with no
// exec dependency and no network.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// Checkout is a repository pinned to a commit.
type Checkout struct {
	// Dir is the working tree root. Paths from the application's spec are
	// resolved against it.
	Dir string
	// Revision is the resolved commit, always a full hash and never the branch
	// or tag it was reached through. The status records what was actually
	// deployed, so a moving ref would make it a guess.
	Revision string
}

// Sourcer fetches repositories into a cache directory, one clone per
// application.
//
// Keying the cache by application rather than by URL means two applications
// tracking different revisions of the same repository do not fight over one
// working tree. It costs a clone per application; sharing one and using linked
// worktrees would be the optimisation, and go-git does not support them.
type Sourcer struct {
	root string
	auth Auth
}

// New returns a Sourcer caching under root.
//
// Credentials are passed in rather than read from the environment here: the
// sourcer should be drivable from a test without touching the process
// environment, and the controller should have exactly one place that reads
// configuration. See AuthFromEnv.
func New(root string, auth Auth) *Sourcer { return &Sourcer{root: root, auth: auth} }

// Fetch brings the application's repository up to date and checks out the
// revision its source names, returning the working tree and the commit it
// resolved to.
//
// The caller decides whether anything changed by comparing the returned
// revision with the one it last saw; an unchanged repository still costs a
// fetch, but no render and no plan.
func (s *Sourcer) Fetch(ctx context.Context, app string, src application.Source) (Checkout, error) {
	if err := validAppDir(app); err != nil {
		return Checkout{}, err
	}
	if err := supportedURL(src.RepoURL); err != nil {
		return Checkout{}, err
	}

	dir := filepath.Join(s.root, app)
	repo, err := s.open(ctx, dir, src.RepoURL)
	if err != nil {
		return Checkout{}, err
	}

	hash, err := resolve(repo, src.Revision)
	if err != nil {
		return Checkout{}, fmt.Errorf("%s: %w", src.RepoURL, err)
	}

	tree, err := repo.Worktree()
	if err != nil {
		return Checkout{}, fmt.Errorf("opening working tree: %w", err)
	}
	// Force discards anything left behind by an interrupted checkout, and
	// Clean removes untracked files, so what is rendered is what the commit
	// says and not what a previous revision left lying about.
	if err := tree.Checkout(&gogit.CheckoutOptions{Hash: hash, Force: true}); err != nil {
		return Checkout{}, fmt.Errorf("checking out %s: %w", hash, err)
	}
	if err := tree.Clean(&gogit.CleanOptions{Dir: true}); err != nil {
		return Checkout{}, fmt.Errorf("cleaning working tree: %w", err)
	}

	return Checkout{Dir: dir, Revision: hash.String()}, nil
}

// open returns the application's clone, creating it if it is absent and
// re-creating it if it points somewhere else.
func (s *Sourcer) open(ctx context.Context, dir, url string) (*gogit.Repository, error) {
	repo, err := gogit.PlainOpen(dir)
	switch {
	case errors.Is(err, gogit.ErrRepositoryNotExists):
		return s.clone(ctx, dir, url)
	case err != nil:
		return nil, fmt.Errorf("opening cached clone %s: %w", dir, err)
	}

	// An application repointed at a different repository must not keep
	// deploying the old one. Nothing else would catch it: every later step
	// succeeds against the stale clone.
	same, err := remoteIs(repo, url)
	if err != nil {
		return nil, err
	}
	if !same {
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("discarding the clone of a different repository: %w", err)
		}
		return s.clone(ctx, dir, url)
	}

	err = repo.FetchContext(ctx, &gogit.FetchOptions{
		Auth:  s.auth.method(),
		Force: true,
		Prune: true,
		Tags:  gogit.AllTags,
	})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	return repo, nil
}

func (s *Sourcer) clone(ctx context.Context, dir, url string) (*gogit.Repository, error) {
	repo, err := gogit.PlainCloneContext(ctx, dir, false, &gogit.CloneOptions{
		URL:  url,
		Auth: s.auth.method(),
		Tags: gogit.AllTags,
	})
	if err != nil {
		// A failed clone leaves a partial directory that would be opened as a
		// valid repository next time round.
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("cloning %s: %w", url, err)
	}
	return repo, nil
}

// remoteIs reports whether the clone's origin is url.
func remoteIs(repo *gogit.Repository, url string) (bool, error) {
	remote, err := repo.Remote(gogit.DefaultRemoteName)
	if err != nil {
		if errors.Is(err, gogit.ErrRemoteNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("reading the cached clone's remote: %w", err)
	}
	for _, got := range remote.Config().URLs {
		if got == url {
			return true, nil
		}
	}
	return false, nil
}

// resolve turns whatever the spec wrote — a commit, a tag, a branch — into a
// commit hash.
//
// The order matters and is not obvious. A clone leaves a local refs/heads for
// the default branch, and nothing ever advances it: a fetch updates
// refs/remotes/origin/*, and there is no merge because there is no checkout of
// that branch. Resolving the bare name would therefore keep returning the
// commit that was current when the clone was made, forever. The
// remote-tracking ref is the authoritative one, so it is tried before the bare
// name. Tags come first because git resolves them first, and the bare name
// last because that is what matches a raw commit hash.
//
// An annotated tag resolves to a tag object, which is dereferenced to the
// commit it points at.
func resolve(repo *gogit.Repository, revision string) (plumbing.Hash, error) {
	if revision == "" {
		return plumbing.ZeroHash, errors.New("no revision to resolve")
	}

	for _, candidate := range []string{
		"refs/tags/" + revision,
		gogit.DefaultRemoteName + "/" + revision,
		revision,
	} {
		hash, err := repo.ResolveRevision(plumbing.Revision(candidate))
		if err != nil {
			continue
		}
		// ResolveRevision peels an annotated tag to the commit it names, so
		// what comes back is already a commit hash.
		return *hash, nil
	}
	return plumbing.ZeroHash, fmt.Errorf("cannot resolve revision %q as a commit, tag or branch", revision)
}

// validAppDir keeps a caller from escaping the cache root. Names that reach
// here have already been validated by the config loader, but this is an
// exported entry point and the consequence of getting it wrong is writing
// wherever the controller can.
func validAppDir(app string) error {
	if app == "" {
		return errors.New("no application name")
	}
	if app == "." || app == ".." || strings.ContainsAny(app, `/\`) {
		return fmt.Errorf("invalid application name %q for a cache directory", app)
	}
	return nil
}

// supportedURL rejects transports this build cannot use safely.
//
// Plain http is refused rather than merely discouraged. This controller turns
// repository content into workloads on a swarm it holds the socket for, so
// anyone on the path between it and an http remote chooses what runs. A local
// path is allowed: a bind-mounted repository is a legitimate air-gapped
// pattern, and it is what the tests use.
func supportedURL(url string) error {
	switch {
	case url == "":
		return errors.New("no repository URL")
	case strings.HasPrefix(url, "https://"):
		return nil
	case strings.HasPrefix(url, "http://"):
		return fmt.Errorf("refusing the plaintext remote %q: anything on the path to it decides what this controller deploys", url)
	case strings.HasPrefix(url, "ssh://") || strings.HasPrefix(url, "git@"):
		return fmt.Errorf("ssh remotes are not supported yet, use https for %q", url)
	case filepath.IsAbs(url):
		return nil
	default:
		return fmt.Errorf("unsupported repository URL %q, want https:// or an absolute path", url)
	}
}

// Auth is how the controller authenticates to a git remote. It is HTTP basic
// authentication, which is how every forge accepts a token.
type Auth struct {
	Username string
	Password string
}

// method returns the go-git authentication method, or nil for an anonymous
// remote — a public repository needs no credential, and sending an empty one
// makes some forges reject the request outright.
func (a Auth) method() transport.AuthMethod {
	if a.Password == "" {
		return nil
	}
	username := a.Username
	if username == "" {
		// Forges ignore the username when the password is a token, but it
		// cannot be empty. This is the value GitHub documents.
		username = "x-access-token"
	}
	return &githttp.BasicAuth{Username: username, Password: a.Password}
}

// Environment variables AuthFromEnv reads. The file form exists because a
// Docker secret arrives as a file: in Swarm it is encrypted at rest in the raft
// log and delivered in memory, which the string form gives up.
const (
	EnvUsername  = "SWARMCLI_CD_GIT_USERNAME"
	EnvToken     = "SWARMCLI_CD_GIT_TOKEN"
	EnvTokenFile = "SWARMCLI_CD_GIT_TOKEN_FILE"
)

// AuthFromEnv reads the git credential from the environment. Its arguments are
// injected so the controller has one place that touches the process
// environment and the tests touch none.
//
// No credential is not an error: public repositories are a legitimate source.
//
// The credential is global to the controller. Per-repository credentials need
// a field on the application spec and a way to name a secret, which is worth
// doing when someone has two forges, not before.
func AuthFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (Auth, error) {
	auth := Auth{Username: strings.TrimSpace(getenv(EnvUsername))}

	if path := getenv(EnvTokenFile); path != "" {
		data, err := readFile(path)
		if err != nil {
			return Auth{}, fmt.Errorf("reading %s: %w", EnvTokenFile, err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return Auth{}, fmt.Errorf("%s names an empty file", EnvTokenFile)
		}
		auth.Password = token
		return auth, nil
	}

	auth.Password = strings.TrimSpace(getenv(EnvToken))
	return auth, nil
}
