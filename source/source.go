// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package source turns a checked-out working tree into what the chart engine's
// PlanApply takes: a release file, a chart source, and the reader it uses for
// values files.
//
// There are two source types and they converge here. A releaseFile application
// points at a swarmcli-release.yaml the repository already contains. A chart
// application names one chart, and this package synthesises the release file it
// would have written — by rendering it and handing it to the engine's own
// parser, so a synthesised file obeys exactly the rules a committed one does
// rather than a second implementation of them.
package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/secrets"
)

// Built is what PlanApply needs.
type Built struct {
	ReleaseFile *charts.ReleaseFile
	Charts      charts.ChartSource
	// ReadFile is charts.PlanOptions.ReadFile: it reads one values file the
	// release file names, checks it really is repository content, and runs it
	// through the SecretProvider seam on the way past.
	ReadFile func(path string) ([]byte, error)
}

// Builder turns working trees into plans' inputs.
type Builder struct {
	root  string
	warnf func(string, ...any)
}

// NewBuilder returns a Builder keeping its per-application state under root.
//
// Root must not be the directory the git sourcer clones into. Everything under
// a clone is inside a working tree that gets force-checked-out and cleaned on
// every fetch, so a repository cache living there would be deleted underneath
// this package — or, worse, show up as repository content.
//
// Warnf receives the chart repository store's warnings. Without it they are
// dropped: a repository index that could not be refreshed is best-effort in the
// engine and silent by default.
func NewBuilder(root string, warnf func(string, ...any)) *Builder {
	return &Builder{root: root, warnf: warnf}
}

// Build produces the release file and chart source for one application.
func (b *Builder) Build(ctx context.Context, app string, spec application.Source, co git.Checkout) (*Built, error) {
	rf, err := b.releaseFile(app, spec, co)
	if err != nil {
		return nil, err
	}

	// A repository store per application, rather than the process-wide XDG
	// default: two applications naming the same repository with different URLs
	// would otherwise collide, and the engine refuses to repoint an existing
	// name rather than picking one.
	store := charts.NewRepoStoreAt(filepath.Join(b.root, app, "repositories"))
	if b.warnf != nil {
		store.Warnf = b.warnf
	}
	if err := store.EnsureRepos(rf.Repositories); err != nil {
		return nil, fmt.Errorf("application %q: %w", app, err)
	}

	return &Built{
		ReleaseFile: rf,
		Charts:      charts.NewChartSource(store),
		ReadFile:    valuesReader(ctx, app, co, rf),
	}, nil
}

// releaseFile returns the release file for either source type.
func (b *Builder) releaseFile(app string, spec application.Source, co git.Checkout) (*charts.ReleaseFile, error) {
	switch {
	case spec.ReleaseFile != "":
		path, err := contained(co.Dir, spec.ReleaseFile)
		if err != nil {
			return nil, fmt.Errorf("application %q: releaseFile: %w", app, err)
		}
		rf, err := charts.LoadReleaseFile(path)
		if err != nil {
			return nil, fmt.Errorf("application %q: %w", app, err)
		}
		return rf, nil

	case spec.Chart != nil:
		rf, err := synthesise(app, *spec.Chart, co)
		if err != nil {
			return nil, fmt.Errorf("application %q: %w", app, err)
		}
		return rf, nil

	default:
		return nil, fmt.Errorf("application %q: source names neither a releaseFile nor a chart", app)
	}
}

// synthesise writes the release file a chart application implies and parses it
// with the engine's own parser.
//
// Going through YAML rather than constructing the struct is deliberate: the
// parser is where "a repository reference must carry a version, a path must
// not" lives, along with the release-name charset and the duplicate checks. A
// hand-built struct would skip all of it and diverge the first time the engine
// gained a rule.
func synthesise(app string, c application.ChartSource, co git.Checkout) (*charts.ReleaseFile, error) {
	ref := c.Ref
	if ref == "" {
		// charts.IsPathRef is syntactic — it looks for a leading ./, ../, /
		// or ~ — so a perfectly good "charts/mine" would be read as the chart
		// "mine" in a repository called "charts".
		ref = "./" + filepath.ToSlash(filepath.Clean(c.Path))
	}

	repos := make([]charts.RepoSpec, 0, len(c.Repositories))
	for _, r := range c.Repositories {
		repos = append(repos, charts.RepoSpec{Name: r.Name, URL: r.URL})
	}

	doc, err := yaml.Marshal(charts.ReleaseFile{
		APIVersion:   "v1",
		Repositories: repos,
		Releases: []charts.ReleaseSpec{{
			Name:    c.Release,
			Chart:   ref,
			Version: c.Version,
			Values:  c.Values,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("building a release file for the chart: %w", err)
	}

	// The path is never opened; it exists so that the engine resolves relative
	// values and chart paths against the working tree root, and so its error
	// messages name the application rather than a file nobody wrote.
	return charts.ParseReleaseFile(doc, filepath.Join(co.Dir, "<application "+app+">"))
}

// valuesReader is the reader the engine uses for every values file the plan
// reads: it checks the path really is repository content, then runs the bytes
// through the SecretProvider seam.
//
// The engine used to read these files itself, so material a provider had
// transformed could only reach it by being written to a scratch directory the
// release file was then repointed at — decrypted secrets on the controller's
// filesystem purely because there was nowhere to hand over bytes. That is what
// Eldara-Tech/swarmcli#501 removed; the material now exists only in the map the
// engine merges it into.
//
// The engine names the file with the path it resolved from the release file, so
// containment is checked against that absolute path rather than the declared
// one. Failing here aborts the whole plan before anything is deployed, which is
// where a values file that resolves outside the repository belongs.
func valuesReader(ctx context.Context, app string, co git.Checkout, rf *charts.ReleaseFile) func(string) ([]byte, error) {
	provider := secrets.Get()

	return func(path string) ([]byte, error) {
		resolved, err := containedAbs(rf.Dir, path)
		if err != nil {
			return nil, fmt.Errorf("application %q: values: %w", app, err)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil, err
		}

		// The provider is given the path as the repository sees it, so it can
		// decide by name or extension.
		relative, err := filepath.Rel(co.Dir, resolved)
		if err != nil {
			relative = filepath.Base(resolved)
		}
		out, err := provider.Resolve(ctx, secrets.Request{Path: filepath.ToSlash(relative), Data: data})
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", relative, err)
		}
		return out, nil
	}
}

// containedAbs is contained for a path the caller already joined, which is what
// the engine hands its values reader: ReleaseFile.ValuesPaths has resolved the
// declared path against the manifest's directory before the reader ever sees it.
// A path outside root becomes a "../" relative one and is refused by the check
// below rather than by a second copy of it.
func containedAbs(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("%q is not under the repository", path)
	}
	return contained(root, rel)
}

// contained resolves rel against root and refuses anything that ends up
// outside it.
//
// The config loader already rejects an escaping path, but this is the check
// that matters: it resolves symlinks. Repository content is not trusted the way
// the operator's own configuration is — anyone who can land a commit can add a
// symlink — and a values file pointing at /run/secrets would otherwise be read,
// merged and rendered into a manifest that is then stored in a Docker config
// readable by anyone with Docker access.
func contained(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%q must be relative to the repository", rel)
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolving the working tree: %w", err)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(realRoot, rel))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%q is not in the repository at this revision", rel)
		}
		return "", fmt.Errorf("resolving %q: %w", rel, err)
	}

	inside, err := filepath.Rel(realRoot, path)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q resolves outside the repository", rel)
	}
	return path, nil
}
