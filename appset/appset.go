// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package appset sources the set of applications the controller reconciles,
// so that the applications file is itself GitOps-managed (issue #47).
//
// # Two tiers
//
// The bootstrap config says where the app set lives and is the single anchor
// nothing in git can repoint. The app set — today's applications.yaml, the same
// config.File schema — lives wherever that says, and changing it is an ordinary
// commit rather than a controller redeploy.
//
// # Two modes, one rule
//
// The set arrives either from a repository the controller pulls itself,
// reusing the sourcer and the credential the applications already use, or from
// a path on a volume that an external process — a git-sync sidecar, the Flux
// source-controller pattern — keeps current. The two modes differ in nothing
// but how they produce bytes: parsing, validation, change detection and the
// swap are written once here, so the guarantee below is the same whichever is
// configured.
//
// # Validate before swap, keep last-good
//
// A load parses and fully validates before anything is swapped in. On any
// failure — unreachable remote, missing file, malformed YAML, an unknown key, a
// duplicate application name — the last set that validated stays current and
// the error is returned. The set is never partially applied, so a bad commit
// cannot break a running controller. Until the first successful load there is
// no current set and a failure is all the caller gets.
//
// # The path mode's one extra rule
//
// The writer must publish atomically: write a temporary file and rename it over
// the app-set file. Rename is atomic, so a reader sees one whole version or the
// other and never a half-written one. A writer that rewrites the file in place
// instead can be read mid-write; that read fails to parse and is therefore a
// failed load with the last-good set kept, which is the safe outcome but not a
// substitute for publishing atomically — a truncation that happens to still be
// valid YAML would be indistinguishable from a set an operator meant to shrink.
//
// # Not here
//
// No timer and no goroutine: Load is called by whoever owns the interval. What
// to do with a changed set — diffing it and driving per-application loops — is
// the reconcile loop's (issue #52), and the bootstrap flags that choose a mode
// are issue #53's.
package appset

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/config"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/source"
)

// Fetcher brings a repository to a revision. *git.Sourcer implements it, which
// is the whole of this package's dependency on git: the app set is pulled by
// the same client, with the same credential, as the applications it declares.
type Fetcher interface {
	Fetch(ctx context.Context, key string, src application.Source) (git.Checkout, error)
}

// cacheKey names the app set's clone in the sourcer's cache root. The sourcer
// keys one clone per key, and the app set gets its own root rather than sharing
// the applications' — one anchor the git set cannot reach, and a name that
// cannot collide with an application's however the set is edited.
const cacheKey = "repo"

// GitConfig locates the app set in a repository the controller pulls itself.
type GitConfig struct {
	// RepoURL is https:// or an absolute path, as the sourcer accepts.
	RepoURL string
	// Revision is a branch, tag or commit. A branch is the useful case here:
	// the point of app-of-apps is that a commit to it is picked up.
	Revision string
	// Path is the app-set file within the repository.
	Path string
}

// PathConfig locates the app set on a volume an external process keeps current.
//
// It is a directory plus a path within it rather than one file path, matching
// the git mode: what a sidecar syncs is a repository, its content is no more
// trusted than a checkout's, and both modes then resolve the file through the
// same containment check.
type PathConfig struct {
	Dir  string
	Path string
}

// document is one app-set file as it was read.
//
// Path is what parse errors are prefixed with, so it names the file the way the
// operator wrote it — and, for the git mode, the commit it came from, because
// "which commit broke the app set" is the first question a failed load raises.
// Revision is that commit on its own, for the controller's status: an operator
// reading an error wants it in the message, and a UI wants it in a field.
type document struct {
	Data     []byte
	Path     string
	Revision string
}

// reader produces the app-set document. It is the only thing the two modes
// disagree about.
type reader interface {
	read(ctx context.Context) (document, error)
}

// Loader loads the app set and keeps the last one that validated.
//
// It is safe for concurrent use. The *config.File it hands out is shared with
// every other caller and with Current, and must be treated as immutable.
type Loader struct {
	reader reader

	mu      sync.RWMutex
	current *config.File
	// digest is the SHA-256 of the document the current set was parsed from.
	// Content, not mtime and not the commit: it is the app set changing that
	// matters, a writer that preserves timestamps cannot hide from it, and a
	// commit that touches nothing else in the file costs no re-parse. Only a
	// successful load advances it, so a bad commit re-reports its error every
	// tick rather than failing once and going quiet.
	digest [sha256.Size]byte
	loaded bool
	// revision and loadedAt describe the last successful load, for the
	// controller's status endpoint. They move with current, under the same lock,
	// so what is reported and what is running cannot disagree.
	revision string
	loadedAt time.Time
}

// NewGit returns a Loader that pulls the app set with the given fetcher.
//
// The fetcher should cache under a root of its own rather than the one the
// applications clone into: the app set is the bootstrap tier, and keeping its
// clone separate is what makes its cache key unambiguous.
func NewGit(fetch Fetcher, cfg GitConfig) *Loader {
	return &Loader{reader: &gitReader{fetch: fetch, cfg: cfg}}
}

// NewPath returns a Loader that reads the app set from a directory an external
// process keeps current. That writer must publish atomically; see the package
// documentation.
func NewPath(cfg PathConfig) *Loader {
	return &Loader{reader: &pathReader{cfg: cfg}}
}

// Load reads, parses and validates the app set.
//
// It returns the current set and whether it changed since the last successful
// load. A failure returns no set at all — not the last-good one — so that a
// caller cannot mistake a stale set for a fresh one; the last-good set is
// reached deliberately, through Current.
func (l *Loader) Load(ctx context.Context) (*config.File, bool, error) {
	doc, err := l.reader.read(ctx)
	if err != nil {
		return nil, false, err
	}
	digest := sha256.Sum256(doc.Data)

	l.mu.RLock()
	current, unchanged := l.current, l.loaded && digest == l.digest
	l.mu.RUnlock()
	if unchanged {
		return current, false, nil
	}

	// The whole file, every rule: unknown keys, duplicate names, per-application
	// source validation. Nothing is swapped in until this has passed.
	file, err := config.Parse(doc.Data, doc.Path)
	if err != nil {
		return nil, false, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.current, l.digest, l.loaded = file, digest, true
	l.revision, l.loadedAt = doc.Revision, time.Now()
	return file, true, nil
}

// Current returns the last set that loaded successfully, or nil if none ever
// has. It is what keeps running while a load is failing.
func (l *Loader) Current() *config.File {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.current
}

// LastLoad reports the commit the current set came from and when it was loaded.
// The revision is empty in path mode, where there is no commit to report, and
// the time is zero until the first successful load.
func (l *Loader) LastLoad() (revision string, at time.Time) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.revision, l.loadedAt
}

// gitReader pulls the app set from a repository.
type gitReader struct {
	fetch Fetcher
	cfg   GitConfig
}

func (r *gitReader) read(ctx context.Context) (document, error) {
	// The app set is not an application, and only two of an application's
	// source fields locate a repository. The rest are what an application
	// declares about its own content, which the fetcher neither reads nor
	// validates.
	checkout, err := r.fetch.Fetch(ctx, cacheKey, application.Source{
		RepoURL:  r.cfg.RepoURL,
		Revision: r.cfg.Revision,
	})
	if err != nil {
		return document{}, fmt.Errorf("fetching the app set: %w", err)
	}

	data, err := readContained(checkout.Dir, r.cfg.Path)
	if err != nil {
		return document{}, err
	}
	return document{
		Data:     data,
		Path:     r.cfg.Path + "@" + checkout.Revision,
		Revision: checkout.Revision,
	}, nil
}

// pathReader reads the app set from a directory an external process keeps
// current.
type pathReader struct {
	cfg PathConfig
}

func (r *pathReader) read(context.Context) (document, error) {
	data, err := readContained(r.cfg.Dir, r.cfg.Path)
	if err != nil {
		return document{}, err
	}
	return document{Data: data, Path: filepath.Join(r.cfg.Dir, r.cfg.Path)}, nil
}

// maxAppSetSize bounds the app-set file.
//
// This is content the controller did not write — a checkout, or a directory
// something else keeps current — and os.ReadFile sizes its buffer from the file,
// so a four-gigabyte applications.yaml exhausted the controller before the
// decoder saw a byte, and did it again every interval. yaml.v3 caps alias
// expansion, which is why the billion-laughs half of this needs nothing; the
// size half had nothing at all.
//
// The chart engine bounds its own reads of untrusted content the same way — 16
// MiB for an index, 20 MiB for an archive, 10 MiB per file in a chart — and the
// number is smaller here because the content is: an application spec is a few
// hundred bytes, so this is room for tens of thousands of them, orders of
// magnitude beyond any set one controller reconciles.
const maxAppSetSize = 4 << 20 // 4 MiB

// readContained returns the whole app-set file under root, refusing one larger
// than maxAppSetSize.
//
// One shot, not a stat followed by a read: with an atomic publisher there is
// nothing to gain from looking first, and with a careless one the gap between
// the two calls is where the file changes. The bound is applied to the bytes as
// they arrive for the same reason — a size taken from a stat is a size the
// content can exceed by the time it is read. The containment check is the same
// one values files go through — this tree is written by something other than
// the controller, and a symlink in it must not turn a config read into a read
// of whatever the controller can reach.
func readContained(root, path string) ([]byte, error) {
	file, err := source.Contained(root, path)
	if err != nil {
		// An absent file is the one an operator meets — the config was not
		// mounted, or the path is not what the repository calls it — and the
		// containment check describes it in terms of a repository at a revision,
		// which is the wrong sentence for a file on a volume.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no app-set file at %s", filepath.Join(root, path))
		}
		return nil, fmt.Errorf("the app-set file %s: %w", filepath.Join(root, path), err)
	}

	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("reading the app-set file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// One byte past the limit, so a file exactly at it still loads and anything
	// over it is known to be over rather than silently truncated to it.
	data, err := io.ReadAll(io.LimitReader(f, maxAppSetSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading the app-set file: %w", err)
	}
	if len(data) > maxAppSetSize {
		return nil, fmt.Errorf("the app-set file %s is larger than the %d-byte limit", filepath.Join(root, path), maxAppSetSize)
	}
	return data, nil
}
