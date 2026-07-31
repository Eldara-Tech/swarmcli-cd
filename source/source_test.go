// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/git"
	"github.com/Eldara-Tech/swarmcli-cd/secrets"
)

// tree is a checked-out working tree. The git sourcer produced it; nothing
// here needs a repository, only the directory it left behind.
func tree(t *testing.T, files map[string]string) git.Checkout {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return git.Checkout{Dir: dir, Revision: strings.Repeat("a", 40)}
}

func builder(t *testing.T) *Builder {
	t.Helper()
	return NewBuilder(t.TempDir(), nil)
}

// trustTestServer makes the chart repository store accept srv's certificate.
//
// A chart repository has to be served over TLS since Eldara-Tech/swarmcli#531,
// and the store builds its own *http.Client inside charts.NewRepoStoreAt — there
// is no seam to hand it a tls.Config through. A nil Transport resolves
// http.DefaultTransport at request time, though, so that package variable is the
// seam, and httptest has already assembled a transport trusting the server it
// started rather than this having to build a second one.
//
// The alternative is charts.RepoStore.AllowPlaintext, and it is deliberately not
// taken: package git refuses a plaintext remote outright, with no escape hatch,
// because anyone on the path to it chooses what this controller deploys. A chart
// repository serves the tarball that *becomes* the workload, so opting out one
// layer down would contradict the rule one layer up.
func trustTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = prev })
}

func TestBuildReleaseFile(t *testing.T) {
	co := tree(t, map[string]string{
		"swarm/prod/swarmcli-release.yaml": `
apiVersion: v1
releases:
  - name: hello
    chart: ./charts/hello
`,
		"swarm/prod/charts/hello/Chart.yaml": "apiVersion: v1\nname: hello\nversion: 0.1.0\n",
	})

	got, err := builder(t).Build(context.Background(), "edge", application.Source{
		ReleaseFile: "swarm/prod/swarmcli-release.yaml",
	}, co)
	if err != nil {
		t.Fatalf("Build = %v, want nil", err)
	}

	if len(got.ReleaseFile.Releases) != 1 || got.ReleaseFile.Releases[0].Name != "hello" {
		t.Errorf("release file decoded wrong: %+v", got.ReleaseFile.Releases)
	}
	// Values and local chart paths resolve against the release file's own
	// directory, not the repository root.
	if want := filepath.Join(co.Dir, "swarm", "prod"); got.ReleaseFile.Dir != want {
		t.Errorf("Dir = %q, want %q", got.ReleaseFile.Dir, want)
	}
	if got.Charts == nil {
		t.Error("no chart source")
	}
}

// A chart application declares one chart; the release file it implies is
// synthesised.
func TestBuildChartSourceWithPath(t *testing.T) {
	co := tree(t, map[string]string{
		"charts/hello/Chart.yaml": "apiVersion: v1\nname: hello\nversion: 0.1.0\n",
		"values/hello.yaml":       "replicas: 3\n",
	})

	got, err := builder(t).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{
			Release: "hello",
			Path:    "charts/hello",
			Values:  []string{"values/hello.yaml"},
		},
	}, co)
	if err != nil {
		t.Fatalf("Build = %v, want nil", err)
	}

	rf := got.ReleaseFile
	if len(rf.Releases) != 1 {
		t.Fatalf("got %d releases, want 1", len(rf.Releases))
	}
	spec := rf.Releases[0]

	// Without a leading "./" the engine reads "charts/hello" as the chart
	// "hello" in a repository named "charts".
	if !charts.IsPathRef(spec.Chart) {
		t.Errorf("chart ref %q is not a path reference", spec.Chart)
	}
	if want := filepath.Join(co.Dir, "charts", "hello"); rf.ChartRef(spec) != want {
		t.Errorf("ChartRef = %q, want %q", rf.ChartRef(spec), want)
	}
	if got := rf.ValuesPaths(spec); len(got) != 1 || got[0] != filepath.Join(co.Dir, "values", "hello.yaml") {
		t.Errorf("ValuesPaths = %v, want the values file under the working tree", got)
	}
	if spec.Version != "" {
		t.Errorf("version = %q, want empty for a path chart", spec.Version)
	}
}

func TestBuildChartSourceWithRepositoryRef(t *testing.T) {
	index := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index.yaml" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, "apiVersion: v1\nentries:\n  whoami:\n    - name: whoami\n      version: 0.1.8\n      urls: [whoami-0.1.8.tgz]\n")
	}))
	defer index.Close()
	trustTestServer(t, index)

	co := tree(t, nil)
	got, err := builder(t).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{
			Release:      "hello",
			Ref:          "swarmcli-charts/whoami",
			Version:      "0.1.8",
			Repositories: []application.RepositorySpec{{Name: "swarmcli-charts", URL: index.URL}},
		},
	}, co)
	if err != nil {
		t.Fatalf("Build = %v, want nil", err)
	}

	spec := got.ReleaseFile.Releases[0]
	if spec.Chart != "swarmcli-charts/whoami" || spec.Version != "0.1.8" {
		t.Errorf("release decoded wrong: %+v", spec)
	}
	if charts.IsPathRef(spec.Chart) {
		t.Error("a repository reference was turned into a path")
	}
	if len(got.ReleaseFile.Repositories) != 1 {
		t.Errorf("repositories = %v, want the one declared", got.ReleaseFile.Repositories)
	}
}

// The synthesised file goes through the engine's own parser, so the engine's
// rules apply to it without this package restating any of them.
func TestSynthesisedFileIsValidatedByTheEngine(t *testing.T) {
	co := tree(t, nil)

	_, err := builder(t).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{Release: "Not A Stack Name", Path: "charts/hello"},
	}, co)
	if err == nil {
		t.Fatal("Build = nil, want the engine to reject the release name")
	}
	if !strings.Contains(err.Error(), "edge") {
		t.Errorf("error %q does not name the application", err)
	}
}

func TestBuildErrors(t *testing.T) {
	co := tree(t, map[string]string{"a.yaml": "x: 1\n"})
	b := builder(t)

	for name, tc := range map[string]struct {
		spec application.Source
		want string
	}{
		"missing release file": {
			application.Source{ReleaseFile: "absent.yaml"},
			"not in the repository at this revision",
		},
		"no source type": {
			application.Source{},
			"neither a releaseFile nor a chart",
		},
		"absolute release file": {
			application.Source{ReleaseFile: "/etc/passwd"},
			"must be relative",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := b.Build(context.Background(), "edge", tc.spec, co)
			if err == nil {
				t.Fatalf("Build = nil, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// A repository that cannot be reached fails the build rather than planning
// against a chart nobody could resolve.
func TestUnreachableChartRepository(t *testing.T) {
	dead := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	dead.Close() // nothing is listening

	_, err := builder(t).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{
			Release:      "hello",
			Ref:          "swarmcli-charts/whoami",
			Version:      "0.1.8",
			Repositories: []application.RepositorySpec{{Name: "swarmcli-charts", URL: dead.URL}},
		},
	}, tree(t, nil))
	if err == nil {
		t.Fatal("Build = nil, want an error for an unreachable repository")
	}
	if !strings.Contains(err.Error(), "edge") {
		t.Errorf("error %q does not name the application", err)
	}
	// Which error matters. On a plain-http server this test kept passing after
	// Eldara-Tech/swarmcli#531 while never reaching the network at all — the
	// store refused the URL's scheme, and "an error was returned" cannot tell the
	// two apart. Assert the download is what failed.
	if !strings.Contains(err.Error(), "index download failed") {
		t.Errorf("error %q is not a failed download, so the repository was never reached", err)
	}
}

// The engine refuses a plaintext chart repository since
// Eldara-Tech/swarmcli#531, and this controller does not set the
// charts.RepoStore.AllowPlaintext escape hatch it ships with. Package git
// already refuses a plaintext *remote* with no hatch at all, because anyone on
// the path to it chooses what gets deployed; a chart repository serves the
// tarball that becomes the workload, so the same answer belongs one layer down.
func TestPlaintextChartRepositoryIsRefused(t *testing.T) {
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "apiVersion: v1\nentries: {}\n")
	}))
	defer index.Close()

	_, err := builder(t).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{
			Release:      "hello",
			Ref:          "swarmcli-charts/whoami",
			Version:      "0.1.8",
			Repositories: []application.RepositorySpec{{Name: "swarmcli-charts", URL: index.URL}},
		},
	}, tree(t, nil))
	if err == nil {
		t.Fatal("Build = nil, want the plaintext repository refused")
	}
	if !strings.Contains(err.Error(), "edge") || !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("error %q names neither the application nor the reason", err)
	}
}

// A release file is repository content, so the repositories it declares are
// controlled by anyone who can land a commit — and the engine turns a repository
// name into a path by concatenating it into the cache's "index-<name>.yaml".
// The name below cleans back out to <root>/<app>/escaped.yaml, so without the
// check the index this server serves is written there, and a name carrying more
// "../" reaches the app set the controller deploys from (#100).
//
// The applications file never sees any of this: these names are not the
// operator's, which is why the config loader's check cannot be the only one.
func TestReleaseFileRepositoryNameEscapingTheCacheIsRefused(t *testing.T) {
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "apiVersion: v1\nentries: {}\n")
	}))
	defer index.Close()

	co := tree(t, map[string]string{
		"swarm/prod/swarmcli-release.yaml": "apiVersion: v1\nrepositories:\n" +
			"  - name: ../../../../escaped\n    url: " + index.URL + "\n" +
			"releases:\n  - name: hello\n    chart: ./charts/hello\n",
		"swarm/prod/charts/hello/Chart.yaml": "apiVersion: v1\nname: hello\nversion: 0.1.0\n",
	})

	root := t.TempDir()
	_, err := NewBuilder(root, nil).Build(context.Background(), "edge", application.Source{
		ReleaseFile: "swarm/prod/swarmcli-release.yaml",
	}, co)
	if err == nil {
		t.Error("Build = nil, want the traversing repository name to be refused")
	} else if !strings.Contains(err.Error(), "edge") || !strings.Contains(err.Error(), "escaped") {
		t.Errorf("error %q names neither the application nor the repository", err)
	}
	if _, err := os.Stat(filepath.Join(root, "edge", "escaped.yaml")); !os.IsNotExist(err) {
		t.Errorf("an index was written outside the repository store: %v", err)
	}
}

func TestMalformedReleaseFile(t *testing.T) {
	co := tree(t, map[string]string{"r.yaml": "releases: [{name: hello, chart: ./c, nonsense: 1}]\n"})

	_, err := builder(t).Build(context.Background(), "edge", application.Source{ReleaseFile: "r.yaml"}, co)
	if err == nil {
		t.Fatal("Build = nil, want the engine's parse error")
	}
	if !strings.Contains(err.Error(), "edge") {
		t.Errorf("error %q does not name the application", err)
	}
}

func TestMissingWorkingTree(t *testing.T) {
	co := git.Checkout{Dir: filepath.Join(t.TempDir(), "absent"), Revision: strings.Repeat("a", 40)}

	_, err := builder(t).Build(context.Background(), "edge", application.Source{ReleaseFile: "r.yaml"}, co)
	if err == nil {
		t.Fatal("Build = nil, want an error for a working tree that is not there")
	}
}

// Repository content is not trusted the way the operator's own configuration
// is: anyone who can land a commit can add a symlink, and a values file
// pointing at /run/secrets would be merged and rendered into a manifest that is
// then stored in a Docker config readable by anyone with Docker access.
func TestSymlinkOutOfTheTreeIsRejected(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "stolen.yaml")
	if err := os.WriteFile(outside, []byte("password: hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	co := tree(t, map[string]string{"charts/hello/Chart.yaml": "apiVersion: v1\nname: hello\nversion: 0.1.0\n"})
	if err := os.Symlink(outside, filepath.Join(co.Dir, "values.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := builder(t).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{Release: "hello", Path: "charts/hello", Values: []string{"values.yaml"}},
	}, co)
	if err != nil {
		t.Fatalf("Build = %v, want nil", err)
	}

	// The reader is where the check lives now, and the engine calls it while
	// planning — so an escaping path still aborts before anything is deployed.
	_, err = got.ReadFile(got.ReleaseFile.ValuesPaths(got.ReleaseFile.Releases[0])[0])
	if err == nil {
		t.Fatal("ReadFile = nil, want the escaping symlink to be refused")
	}
	if !strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("error %q does not say the path escaped", err)
	}
}

// decrypter stands in for the Business Edition's SOPS provider.
type decrypter struct{}

func (decrypter) Resolve(_ context.Context, req secrets.Request) ([]byte, error) {
	if !bytes.HasPrefix(req.Data, []byte("ENC[")) {
		return req.Data, nil
	}
	body, ok := bytes.CutSuffix(bytes.TrimPrefix(req.Data, []byte("ENC[")), []byte("]\n"))
	if !ok {
		return nil, errors.New("corrupt ciphertext in " + req.Path)
	}
	return body, nil
}

func useProvider(t *testing.T, p secrets.Provider) {
	t.Helper()
	original, name := secrets.Get(), secrets.Active()
	t.Cleanup(func() { secrets.Register(name, original) })
	secrets.Register("test", p)
}

func TestProviderTransformsValuesWithoutTouchingTheFilesystem(t *testing.T) {
	useProvider(t, decrypter{})
	root := t.TempDir()

	co := tree(t, map[string]string{
		"charts/hello/Chart.yaml": "apiVersion: v1\nname: hello\nversion: 0.1.0\n",
		"values/secret.yaml":      "ENC[replicas: 3]\n",
		"values/plain.yaml":       "image: whoami\n",
	})

	got, err := NewBuilder(root, nil).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{
			Release: "hello",
			Path:    "charts/hello",
			Values:  []string{"values/secret.yaml", "values/plain.yaml"},
		},
	}, co)
	if err != nil {
		t.Fatalf("Build = %v, want nil", err)
	}

	paths := got.ReleaseFile.ValuesPaths(got.ReleaseFile.Releases[0])
	if len(paths) != 2 {
		t.Fatalf("got %d values paths, want 2", len(paths))
	}

	// Both files stay where the repository put them. Before swarmcli#501 the
	// transformed one had to be copied out and the release file repointed at
	// the copy, because the engine read the paths itself.
	for i, want := range []string{
		filepath.Join(co.Dir, "values", "secret.yaml"),
		filepath.Join(co.Dir, "values", "plain.yaml"),
	} {
		if paths[i] != want {
			t.Errorf("values path %d = %s, want %s", i, paths[i], want)
		}
	}

	data, err := got.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("ReadFile = %v, want nil", err)
	}
	if string(data) != "replicas: 3" {
		t.Errorf("resolved values = %q, want the decrypted body", data)
	}

	// The decrypted body existed only in that return value. Nothing was
	// written, so there is nothing left readable and nothing to go stale.
	if _, err := os.Stat(filepath.Join(root, "edge", "values")); !os.IsNotExist(err) {
		t.Errorf("decrypted material reached the filesystem: %v", err)
	}
	onDisk, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != "ENC[replicas: 3]\n" {
		t.Errorf("the working tree was rewritten: %q", onDisk)
	}
}

// With the OSS provider nothing is transformed, so nothing is written and
// nothing is repointed.
func TestPlaintextProviderLeavesValuesInPlace(t *testing.T) {
	root := t.TempDir()
	co := tree(t, map[string]string{
		"charts/hello/Chart.yaml": "apiVersion: v1\nname: hello\nversion: 0.1.0\n",
		"values/plain.yaml":       "image: whoami\n",
	})

	got, err := NewBuilder(root, nil).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{Release: "hello", Path: "charts/hello", Values: []string{"values/plain.yaml"}},
	}, co)
	if err != nil {
		t.Fatalf("Build = %v, want nil", err)
	}

	paths := got.ReleaseFile.ValuesPaths(got.ReleaseFile.Releases[0])
	if want := filepath.Join(co.Dir, "values", "plain.yaml"); paths[0] != want {
		t.Errorf("values path = %s, want the file in the working tree", paths[0])
	}
	if _, err := os.Stat(filepath.Join(root, "edge", "values")); !os.IsNotExist(err) {
		t.Errorf("a scratch directory was created for material nobody transformed: %v", err)
	}
}

// The provider is told which application the material belongs to, so one
// holding per-application key material can pick a key. A path and some bytes
// say nothing about who they belong to, and the builder is the only thing that
// knows.
func TestProviderIsToldWhichApplicationTheMaterialBelongsTo(t *testing.T) {
	seen := &recordingProvider{}
	useProvider(t, seen)

	co := tree(t, map[string]string{
		"charts/hello/Chart.yaml": "apiVersion: v1\nname: hello\nversion: 0.1.0\n",
		"values/plain.yaml":       "image: whoami\n",
	})

	got, err := builder(t).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{Release: "hello", Path: "charts/hello", Values: []string{"values/plain.yaml"}},
	}, co)
	if err != nil {
		t.Fatalf("Build = %v, want nil", err)
	}
	if _, err := got.ReadFile(got.ReleaseFile.ValuesPaths(got.ReleaseFile.Releases[0])[0]); err != nil {
		t.Fatalf("ReadFile = %v, want nil", err)
	}

	if len(seen.requests) != 1 {
		t.Fatalf("the provider saw %d requests, want 1", len(seen.requests))
	}
	if got := seen.requests[0].Application; got != "edge" {
		t.Errorf("Request.Application = %q, want edge", got)
	}
	// The path is still the repository's view of it, which is the other half of
	// what a provider decides on.
	if got := seen.requests[0].Path; got != "values/plain.yaml" {
		t.Errorf("Request.Path = %q, want values/plain.yaml", got)
	}
}

// recordingProvider passes everything through and keeps what it was asked.
type recordingProvider struct{ requests []secrets.Request }

func (p *recordingProvider) Resolve(_ context.Context, req secrets.Request) ([]byte, error) {
	p.requests = append(p.requests, req)
	return req.Data, nil
}

// The provider's error aborts the plan: the engine calls the reader while
// planning, and a values file that cannot be resolved must not be rendered.
func TestProviderErrorFailsTheRead(t *testing.T) {
	useProvider(t, decrypter{})

	co := tree(t, map[string]string{
		"charts/hello/Chart.yaml": "apiVersion: v1\nname: hello\nversion: 0.1.0\n",
		"values/broken.yaml":      "ENC[unterminated\n",
	})

	got, err := builder(t).Build(context.Background(), "edge", application.Source{
		Chart: &application.ChartSource{Release: "hello", Path: "charts/hello", Values: []string{"values/broken.yaml"}},
	}, co)
	if err != nil {
		t.Fatalf("Build = %v, want nil", err)
	}

	_, err = got.ReadFile(got.ReleaseFile.ValuesPaths(got.ReleaseFile.Releases[0])[0])
	if err == nil {
		t.Fatal("ReadFile = nil, want the provider's error")
	}
	if !strings.Contains(err.Error(), "corrupt ciphertext") {
		t.Errorf("error %q does not carry the provider's reason", err)
	}
}
