// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package config reads the applications the controller reconciles.
//
// Applications are declared in a file, not a database. A GitOps controller
// whose own desired state lives in mutable storage has a bootstrap problem: the
// file is the only source of truth and the API serves it read-only.
//
// # What reloads and what does not
//
// This package parses the file; where it comes from, and whether it can change
// under a running controller, is the bootstrap's answer and not this one's
// (see package appset). The two tiers behave differently on purpose:
//
// A file mounted as a Docker config cannot change under the process. Docker
// configs are immutable, so changing one means creating a new config object and
// updating the service to reference it, which replaces the container — a
// watcher would never fire, because the process that would notice the change
// does not outlive it. Restart is not a limitation there; it is the only thing
// that can happen.
//
// A file the controller sources from git, or from a directory something else
// keeps current, is re-read on an interval and swapped in once it validates.
// That is the whole of issue #47: the set of applications became an ordinary
// git commit rather than a redeploy. The API stays read-only either way, since
// the way to change the set is to change the file.
//
// # Everything else is environment
//
// This file holds applications and nothing else. The listen address, the admin
// token, where the set itself lives — those are flags and environment
// variables, matching how swarmcli-rbac-proxy is configured and keeping the one
// file an operator edits about the one thing they think about.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// File is a parsed applications file.
type File struct {
	// APIVersion is "v1", or absent meaning the same. It mirrors the chart
	// engine's release file so the two read alike.
	APIVersion string `yaml:"apiVersion,omitempty"`

	Applications []application.Spec `yaml:"applications"`

	// Path is the file this was read from. It prefixes error messages and is
	// not part of the document.
	Path string `yaml:"-"`
}

// nameRE is what an application may be called. It becomes a URL path segment
// and part of an owner stamp — "cd/<controller>/<name>:release/<release>" —
// and the chart engine rejects an owner id containing a colon, so the charset
// is narrow on purpose rather than by convention.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// secretNameRE is what registryAuth may name. It is a Docker secret name, and
// it is joined onto /run/secrets to find the mounted file — so the charset that
// keeps it a valid secret name also keeps it from escaping that directory: no
// slash, and a leading dot (hence "..") is rejected.
var secretNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// allowNameRE is what an `allow` entry may name. It is the charset Docker holds
// a secret, config, volume or network name to, so a name outside it can never
// match anything on the swarm.
//
// Checked rather than shrugged at because an allowlist fails silently in the one
// direction that is hard to notice: a mistyped entry does not permit something
// wrong, it permits nothing, and the deploy is then refused with a message about
// the name in the chart rather than about the typo in the app set.
var allowNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Load reads and validates the applications file at path.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading applications file: %w", err)
	}
	return Parse(data, path)
}

// Parse validates an applications file that has already been read. Unknown
// keys are an error: a misspelled key that was quietly ignored would leave a
// setting an operator believes they configured silently doing nothing.
func Parse(data []byte, path string) (*File, error) {
	var f File

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// io.EOF is what an empty document decodes to. It is swallowed so the
	// failure is the specific "no applications declared" below rather than a
	// bare "EOF".
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	f.Path = path
	f.applyDefaults()
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

func (f *File) applyDefaults() {
	for i := range f.Applications {
		if f.Applications[i].DriftDetection == "" {
			f.Applications[i].DriftDetection = application.DriftManifest
		}
	}
}

func (f *File) validate() error {
	if f.APIVersion != "" && f.APIVersion != "v1" {
		return fmt.Errorf("unsupported apiVersion %q, want v1", f.APIVersion)
	}
	if len(f.Applications) == 0 {
		return errors.New("no applications declared")
	}

	seen := make(map[string]bool, len(f.Applications))
	for i, app := range f.Applications {
		if err := validateApplication(app); err != nil {
			return fmt.Errorf("applications[%d]: %w", i, err)
		}
		if seen[app.Name] {
			return fmt.Errorf("applications[%d]: duplicate application name %q", i, app.Name)
		}
		seen[app.Name] = true
	}
	return nil
}

func validateApplication(app application.Spec) error {
	if app.Name == "" {
		return errors.New("name is required")
	}
	if !nameRE.MatchString(app.Name) {
		return fmt.Errorf("invalid name %q: lowercase letters, digits, dot, dash and underscore only, starting with a letter or digit", app.Name)
	}
	if !app.DriftDetection.Valid() {
		return fmt.Errorf("%q: unsupported driftDetection %q, this build implements %q and %q",
			app.Name, app.DriftDetection, application.DriftManifest, application.DriftLive)
	}
	if app.RegistryAuth != "" && !secretNameRE.MatchString(app.RegistryAuth) {
		return fmt.Errorf("%q: invalid registryAuth %q: it names a Docker secret, so letters, digits, dot, dash and underscore only", app.Name, app.RegistryAuth)
	}
	if err := validateAllow(app.Allow); err != nil {
		return fmt.Errorf("%q: %w", app.Name, err)
	}
	if err := validateSource(app.Source); err != nil {
		return fmt.Errorf("%q: %w", app.Name, err)
	}
	if app.SyncPolicy.Interval < 0 || app.SyncPolicy.Timeout < 0 {
		return fmt.Errorf("%q: syncPolicy interval and timeout cannot be negative", app.Name)
	}
	// Nil is "not set", which means the default; only a number written down can
	// be wrong. An explicit 0 is legal and means keep every revision.
	if app.SyncPolicy.HistoryMax != nil && *app.SyncPolicy.HistoryMax < 0 {
		return fmt.Errorf("%q: syncPolicy historyMax cannot be negative", app.Name)
	}
	// Refused rather than ignored. Obeying half of a destructive instruction is
	// the wrong failure mode in both directions: taken as "prune with volumes"
	// it deletes data nobody asked to lose, and taken as "prune nothing" it
	// leaves an operator believing cleanup is on when it is not.
	if app.SyncPolicy.PruneVolumes && !app.SyncPolicy.Prune {
		return fmt.Errorf("%q: syncPolicy pruneVolumes means nothing without prune", app.Name)
	}
	// pruneFirst orders both prunes — the release sweep and the resource sweep —
	// so either gate gives it something to order. pruneResources needs no gate of
	// its own: it is a sibling of prune rather than an extension of it.
	if app.SyncPolicy.PruneFirst && !app.SyncPolicy.Prune && !app.SyncPolicy.PruneResources {
		return fmt.Errorf("%q: syncPolicy pruneFirst means nothing without prune or pruneResources", app.Name)
	}
	return nil
}

// validateAllow holds an application's allowlist to the shapes that can match
// anything, so that an entry which will never permit what its author meant is a
// startup failure rather than a refused deploy weeks later.
//
// A host path is held to being absolute and already clean. Absolute because it
// names a path on whichever node runs the task and there is nothing else for a
// relative one to be resolved against; clean because the comparison cleans the
// bind source but reports the entry as written, and "/srv/app/" silently
// permitting "/srv/app" is the kind of near-miss that teaches an operator the
// wrong rule. Both are one edit to fix and the message says which.
//
// Nothing here can tell whether an entry names something that exists, or
// something the operator should not have granted. It is a charset and a shape,
// not a policy: the policy is the operator's, which is the whole point of the
// field.
func validateAllow(a application.Allow) error {
	for i, p := range a.HostPaths {
		switch {
		case p == "":
			return fmt.Errorf("allow.hostPaths[%d] is empty", i)
		case !path.IsAbs(p):
			return fmt.Errorf("allow.hostPaths[%d] %q must be absolute: it names a path on whichever node runs the task, so there is nothing for a relative one to resolve against", i, p)
		case path.Clean(p) != p:
			return fmt.Errorf("allow.hostPaths[%d] %q is not written in its simplest form; use %q", i, p, path.Clean(p))
		}
	}
	for _, kind := range []struct {
		field string
		names []string
	}{
		{"allow.secrets", a.Secrets},
		{"allow.configs", a.Configs},
		{"allow.volumes", a.Volumes},
		{"allow.networks", a.Networks},
	} {
		for i, name := range kind.names {
			if !allowNameRE.MatchString(name) {
				return fmt.Errorf("%s[%d]: invalid name %q: it is compared against a name on the swarm, so letters, digits, dot, dash and underscore only, starting with a letter or digit", kind.field, i, name)
			}
		}
	}
	return nil
}

func validateSource(src application.Source) error {
	if src.RepoURL == "" {
		return errors.New("source.repoURL is required")
	}
	if src.Revision == "" {
		return errors.New("source.revision is required: an unpinned source would deploy whatever the default branch happens to be")
	}

	switch {
	case src.ReleaseFile != "" && src.Chart != nil:
		return errors.New("source has both releaseFile and chart, so which source type is meant is ambiguous")
	case src.ReleaseFile != "":
		return repoPath("source.releaseFile", src.ReleaseFile)
	case src.Chart != nil:
		return validateChart(*src.Chart)
	default:
		return errors.New("source needs one of releaseFile or chart")
	}
}

func validateChart(c application.ChartSource) error {
	if c.Release == "" {
		return errors.New("source.chart.release is required")
	}

	switch {
	case c.Path != "" && c.Ref != "":
		return errors.New("source.chart has both path and ref, so which chart is meant is ambiguous")

	case c.Path != "":
		// The chart's own Chart.yaml carries its version, so selecting one
		// here would be a second answer to a settled question. The chart
		// engine rejects the same combination.
		if c.Version != "" {
			return errors.New("source.chart.version cannot be set with a path: the chart's Chart.yaml is its version")
		}
		if err := repoPath("source.chart.path", c.Path); err != nil {
			return err
		}

	case c.Ref != "":
		// A floating pin would silently upgrade production on the next
		// reconcile, which is the failure this whole controller exists to
		// prevent. The chart engine requires it for the same reason.
		if c.Version == "" {
			return errors.New("source.chart.version is required with a ref: an unpinned chart would silently upgrade on the next reconcile")
		}
		if !strings.Contains(c.Ref, "/") {
			return fmt.Errorf("invalid source.chart.ref %q, want repository/chart", c.Ref)
		}
		if len(c.Repositories) == 0 {
			return fmt.Errorf("source.chart.ref %q needs source.chart.repositories to resolve it", c.Ref)
		}

	default:
		return errors.New("source.chart needs one of path or ref")
	}

	for i, r := range c.Repositories {
		if r.Name == "" || r.URL == "" {
			return fmt.Errorf("source.chart.repositories[%d] needs both name and url", i)
		}
		// The name becomes a filesystem path in the chart engine, so it is held
		// to a charset rather than only to being non-empty. Package source runs
		// the same check over a release file's own repositories, which never
		// pass through here.
		if !application.ValidRepositoryName(r.Name) {
			return fmt.Errorf("source.chart.repositories[%d]: invalid name %q: it becomes a file in the chart cache, so letters, digits, dot, dash and underscore only, starting with a letter or digit", i, r.Name)
		}
	}
	for i, v := range c.Values {
		if err := repoPath(fmt.Sprintf("source.chart.values[%d]", i), v); err != nil {
			return err
		}
	}
	return nil
}

// repoPath rejects anything that does not stay inside the checkout. The paths
// in this file are operator-supplied, but they are resolved against a working
// tree, and a path escaping it would read whatever the controller can — which,
// for a process holding the docker socket, is everything.
func repoPath(field, p string) error {
	if p == "" {
		return fmt.Errorf("%s is required", field)
	}
	if path.IsAbs(p) {
		return fmt.Errorf("%s %q must be relative to the repository root", field, p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%s %q escapes the repository", field, p)
	}
	return nil
}
