// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package config reads the applications the controller reconciles.
//
// Applications are declared in a file, not a database. A GitOps controller
// whose own desired state lives in mutable storage has a bootstrap problem, and
// Phase 1 has no need for one: the file is the only source of truth and the API
// serves it read-only.
//
// # Why there is no hot reload
//
// The file is delivered as a Docker config, and Docker configs are immutable.
// Changing one means creating a new config object and updating the service to
// reference it, which replaces the container. A watcher would therefore never
// fire: the process that would notice the change does not outlive it. Restart
// is not a limitation here, it is the only thing that can happen.
//
// # Everything else is environment
//
// This file holds applications and nothing else. The listen address, the poll
// interval, the admin token — those are environment variables, matching how
// swarmcli-rbac-proxy is configured and keeping the one file an operator edits
// about the one thing they think about.
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
// and half of an owner stamp — "cd/<name>:release/<release>" — and the chart
// engine rejects an owner id containing a colon, so the charset is narrow on
// purpose rather than by convention.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// secretNameRE is what registryAuth may name. It is a Docker secret name, and
// it is joined onto /run/secrets to find the mounted file — so the charset that
// keeps it a valid secret name also keeps it from escaping that directory: no
// slash, and a leading dot (hence "..") is rejected.
var secretNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

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
		return fmt.Errorf("%q: unsupported driftDetection %q, this build implements %q", app.Name, app.DriftDetection, application.DriftManifest)
	}
	if app.RegistryAuth != "" && !secretNameRE.MatchString(app.RegistryAuth) {
		return fmt.Errorf("%q: invalid registryAuth %q: it names a Docker secret, so letters, digits, dot, dash and underscore only", app.Name, app.RegistryAuth)
	}
	if err := validateSource(app.Source); err != nil {
		return fmt.Errorf("%q: %w", app.Name, err)
	}
	if app.SyncPolicy.Interval < 0 || app.SyncPolicy.Timeout < 0 {
		return fmt.Errorf("%q: syncPolicy interval and timeout cannot be negative", app.Name)
	}
	if app.SyncPolicy.HistoryMax < 0 {
		return fmt.Errorf("%q: syncPolicy historyMax cannot be negative", app.Name)
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
