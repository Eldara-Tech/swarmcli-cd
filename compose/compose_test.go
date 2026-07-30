// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package compose

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"
)

// fakeAPI is everything conversion needs from a daemon, which is only the three
// things it genuinely cannot answer offline: the negotiated API version, and
// the ids behind the secret and config names a service references.
//
// client.APIClient is embedded nil on purpose. Anything else conversion reaches
// for panics with the method name, which is a far better failure than a silent
// zero value — and it is how this package finds out if a docker/cli bump starts
// calling the daemon somewhere new.
type fakeAPI struct {
	client.APIClient
	secrets []swarm.Secret
	configs []swarm.Config
}

func (fakeAPI) ClientVersion() string { return "1.51" }

func (f fakeAPI) SecretList(context.Context, swarm.SecretListOptions) ([]swarm.Secret, error) {
	return f.secrets, nil
}

func (f fakeAPI) ConfigList(context.Context, swarm.ConfigListOptions) ([]swarm.Config, error) {
	return f.configs, nil
}

func convertOK(t *testing.T, manifest, stack string, api client.APIClient) *Stack {
	t.Helper()
	if api == nil {
		api = fakeAPI{}
	}
	got, err := Convert(context.Background(), manifest, stack, api)
	if err != nil {
		t.Fatalf("Convert = %v, want nil", err)
	}
	return got
}

// Names, not just counts: a stack is a name prefix plus a label, so scoping is
// the entire mechanism by which one stack's resources are distinguishable from
// another's. Swarm has no /stacks endpoint to ask instead.
func TestConvertScopesNamesAndLabelsThem(t *testing.T) {
	got := convertOK(t, `
services:
  web:
    image: nginx
    networks: [front]
networks:
  front: {}
`, "shop", nil)

	if len(got.Services) != 1 {
		t.Fatalf("got %d services, want 1", len(got.Services))
	}
	if got.Services[0].Name != "web" {
		t.Errorf("Name = %q, want the manifest's own name", got.Services[0].Name)
	}
	if got.Services[0].Spec.Name != "shop_web" {
		t.Errorf("Spec.Name = %q, want shop_web", got.Services[0].Spec.Name)
	}
	if ns := got.Services[0].Spec.Labels["com.docker.stack.namespace"]; ns != "shop" {
		t.Errorf("namespace label = %q, want shop", ns)
	}
	if len(got.Networks) != 1 || got.Networks[0].Name != "shop_front" {
		t.Fatalf("networks = %+v, want one scoped shop_front", got.Networks)
	}
	if ns := got.Networks[0].Spec.Labels["com.docker.stack.namespace"]; ns != "shop" {
		t.Errorf("network namespace label = %q, want shop", ns)
	}
}

// Map iteration order is one of the named defects of `docker stack deploy`.
// Reproducing it would make every reconcile of an unchanged manifest produce a
// differently ordered work list, so the plan diff would be noise.
func TestConvertOrdersEverythingByName(t *testing.T) {
	manifest := `
services:
  zeta:
    image: a
    networks: [zulu, alpha]
  alpha:
    image: b
    networks: [zulu]
  middle:
    image: c
    networks: [alpha]
networks:
  zulu: {}
  alpha: {}
configs:
  zconf:
    file: ./z.txt
  aconf:
    file: ./a.txt
secrets:
  zsec:
    file: ./z.key
  asec:
    file: ./a.key
`
	dir := t.TempDir()
	for _, f := range []string{"z.txt", "a.txt", "z.key", "a.key"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest = strings.ReplaceAll(manifest, "./", dir+"/")

	got := convertOK(t, manifest, "s", nil)

	if names := serviceNames(got); !reflect.DeepEqual(names, []string{"alpha", "middle", "zeta"}) {
		t.Errorf("services = %v, want them sorted", names)
	}
	if names := networkNames(got); !reflect.DeepEqual(names, []string{"s_alpha", "s_zulu"}) {
		t.Errorf("networks = %v, want them sorted", names)
	}
	if len(got.Configs) != 2 || got.Configs[0].Name != "s_aconf" || got.Configs[1].Name != "s_zconf" {
		t.Errorf("configs = %+v, want them sorted and scoped", got.Configs)
	}
	if len(got.Secrets) != 2 || got.Secrets[0].Name != "s_asec" || got.Secrets[1].Name != "s_zsec" {
		t.Errorf("secrets = %+v, want them sorted and scoped", got.Secrets)
	}
}

// The property the ordering exists for: the same manifest twice is the same
// work list twice, down to the bytes.
func TestConvertIsDeterministic(t *testing.T) {
	manifest := mustRead(t, "testdata/traefik.yaml")

	first := convertOK(t, manifest, "edge", nil)
	for i := range 5 {
		again := convertOK(t, manifest, "edge", nil)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differed from the first", i+2)
		}
	}
}

// A chart resolved its own templating before the manifest existed, so a
// surviving ${...} is a literal the chart meant. Interpolating it would make
// the controller's own environment an invisible input to every application's
// deployment.
func TestConvertDoesNotInterpolateTheControllersEnvironment(t *testing.T) {
	t.Setenv("SECRET_FROM_THE_CONTROLLER", "leaked")

	got := convertOK(t, `
services:
  web:
    image: nginx
    environment:
      TOKEN: "${SECRET_FROM_THE_CONTROLLER}"
`, "s", nil)

	env := got.Services[0].Spec.TaskTemplate.ContainerSpec.Env
	if len(env) != 1 {
		t.Fatalf("env = %v, want one entry", env)
	}
	if env[0] != "TOKEN=${SECRET_FROM_THE_CONTROLLER}" {
		t.Errorf("env = %q, want the literal preserved and the controller's environment untouched", env[0])
	}
}

// An external network is a promise the manifest makes about the swarm, not
// something to conjure. Creating one would silently produce an empty overlay
// where the operator meant an existing shared network.
func TestExternalNetworksAreReportedNotCreated(t *testing.T) {
	got := convertOK(t, mustRead(t, "testdata/traefik.yaml"), "edge", nil)

	if !reflect.DeepEqual(got.ExternalNetworks, []string{"traefik-public"}) {
		t.Errorf("external = %v, want [traefik-public]", got.ExternalNetworks)
	}
	if len(got.Networks) != 0 {
		t.Errorf("networks = %+v, want nothing to create", got.Networks)
	}
}

// A service naming no network joins "default", which the stack then has to
// create — matching `docker stack deploy`, whose behaviour operators already
// depend on.
func TestServiceWithoutNetworksGetsTheDefaultOne(t *testing.T) {
	got := convertOK(t, "services:\n  web:\n    image: nginx\n", "s", nil)

	if len(got.Networks) != 1 || got.Networks[0].Name != "s_default" {
		t.Errorf("networks = %+v, want a scoped default", got.Networks)
	}
}

// A network the manifest declares but no service joins is not created. It is
// the services' references that decide, not the networks block.
func TestUnreferencedNetworkIsNotCreated(t *testing.T) {
	got := convertOK(t, `
services:
  web:
    image: nginx
    networks: [used]
networks:
  used: {}
  never: {}
`, "s", nil)

	if names := networkNames(got); !reflect.DeepEqual(names, []string{"s_used"}) {
		t.Errorf("networks = %v, want only the referenced one", names)
	}
}

// A swarm bind mount names a path on whichever node runs the task, so a
// relative source has no referent. The loader would quietly resolve it against
// the working directory and bind a directory nobody named.
func TestRelativeBindSourceIsRejected(t *testing.T) {
	for _, tc := range []struct{ name, volumes string }{
		{"short syntax", `["./data:/data"]`},
		{"parent", `["../data:/data"]`},
		{"home", `["~/data:/data"]`},
		{"long syntax", `[{type: bind, source: ./data, target: /data}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Convert(context.Background(),
				"services:\n  app:\n    image: x\n    volumes: "+tc.volumes+"\n", "s", fakeAPI{})
			if err == nil {
				t.Fatal("Convert = nil, want a relative bind source to be refused")
			}
			if !strings.Contains(err.Error(), "must be absolute") {
				t.Errorf("error %q does not say why", err)
			}
			if !strings.Contains(err.Error(), `service "app"`) {
				t.Errorf("error %q does not name the service", err)
			}
		})
	}
}

// The shapes that do have a meaning on a worker node must still pass. An
// over-eager check here would reject the traefik chart, which binds the docker
// socket.
func TestAbsoluteBindsAndNamedVolumesAreAccepted(t *testing.T) {
	got := convertOK(t, `
services:
  app:
    image: x
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - data:/data
      - /anonymous
      - {type: volume, source: named, target: /named}
volumes:
  data: {}
  named: {}
`, "s", nil)

	if n := len(got.Services[0].Spec.TaskTemplate.ContainerSpec.Mounts); n != 4 {
		t.Errorf("got %d mounts, want 4", n)
	}
}

// Resolving a referenced secret to its id is the one thing conversion cannot do
// offline, and getting it wrong means a service that references a secret Swarm
// will not attach.
func TestServiceSecretsAndConfigsResolveThroughTheClient(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"api.key", "app.conf"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	api := fakeAPI{
		secrets: []swarm.Secret{{ID: "sec-id", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
		configs: []swarm.Config{{ID: "cfg-id", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_appconf"}}}},
	}
	got := convertOK(t, `
services:
  app:
    image: x
    secrets: [apikey]
    configs: [appconf]
secrets:
  apikey:
    file: `+filepath.Join(dir, "api.key")+`
configs:
  appconf:
    file: `+filepath.Join(dir, "app.conf")+`
`, "s", api)

	spec := got.Services[0].Spec.TaskTemplate.ContainerSpec
	if len(spec.Secrets) != 1 || spec.Secrets[0].SecretID != "sec-id" || spec.Secrets[0].SecretName != "s_apikey" {
		t.Errorf("secrets = %+v, want the id the daemon reported", spec.Secrets)
	}
	if len(spec.Configs) != 1 || spec.Configs[0].ConfigID != "cfg-id" || spec.Configs[0].ConfigName != "s_appconf" {
		t.Errorf("configs = %+v, want the id the daemon reported", spec.Configs)
	}
}

// declaresAndMounts is the chart shape swarmcli-cd#84 was filed for: it brings
// its own config and secret and mounts them. Files, because that is the only way
// compose carries content, resolved against this process's filesystem.
func declaresAndMounts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return `
services:
  app:
    image: busybox
    configs: [site]
    secrets: [apikey]
configs:
  site:
    file: ` + write("site.conf", "listen 80;\n") + `
secrets:
  apikey:
    file: ` + write("api.key", "s3cr3t\n") + `
`
}

// The conversion that is applied cannot also be the one that decides whether to
// apply it: it resolves each reference against the daemon, and a chart's own
// config does not exist until this controller creates it. So Convert refuses
// this manifest, and ConvertUnresolved is what the mount guard reads instead.
func TestConvertUnresolvedNeedsNothingToExistYet(t *testing.T) {
	manifest := declaresAndMounts(t)

	_, err := Convert(context.Background(), manifest, "rel", fakeAPI{})
	if err == nil {
		t.Fatal("Convert = nil, want a reference to a resource that does not exist yet to fail")
	}
	if !strings.Contains(err.Error(), "secret not found: rel_apikey") {
		t.Errorf("error %q is not the unresolvable reference", err)
	}

	got, err := ConvertUnresolved(context.Background(), manifest, "rel", fakeAPI{})
	if err != nil {
		t.Fatalf("ConvertUnresolved = %v, want nil", err)
	}

	// The names are the ones a deploy will create and mount; only the ids are
	// made up, and nothing reads those.
	if len(got.Configs) != 1 || got.Configs[0].Name != "rel_site" {
		t.Errorf("configs = %+v, want one scoped rel_site to create", got.Configs)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Name != "rel_apikey" {
		t.Errorf("secrets = %+v, want one scoped rel_apikey to create", got.Secrets)
	}
	cs := got.Services[0].Spec.TaskTemplate.ContainerSpec
	if len(cs.Configs) != 1 || cs.Configs[0].ConfigName != "rel_site" || cs.Configs[0].ConfigID != unresolvedID {
		t.Errorf("mounted configs = %+v, want rel_site with an unresolved id", cs.Configs)
	}
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretName != "rel_apikey" || cs.Secrets[0].SecretID != unresolvedID {
		t.Errorf("mounted secrets = %+v, want rel_apikey with an unresolved id", cs.Secrets)
	}
}

// What the guard's soundness rests on: the two conversions of one manifest agree
// on everything except the ids. If they could disagree on a name, a stack refused
// by the first could still be deployed by the second.
func TestConvertUnresolvedDiffersOnlyByTheIds(t *testing.T) {
	manifest := declaresAndMounts(t)
	api := fakeAPI{
		configs: []swarm.Config{{ID: "cfg-id", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "rel_site"}}}},
		secrets: []swarm.Secret{{ID: "sec-id", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "rel_apikey"}}}},
	}

	resolved := convertOK(t, manifest, "rel", api)
	unresolved, err := ConvertUnresolved(context.Background(), manifest, "rel", api)
	if err != nil {
		t.Fatalf("ConvertUnresolved = %v, want nil", err)
	}
	if resolved.Services[0].Spec.TaskTemplate.ContainerSpec.Configs[0].ConfigID != "cfg-id" {
		t.Fatal("the real conversion did not resolve the reference; the comparison below would prove nothing")
	}

	forgetIDs(resolved)
	forgetIDs(unresolved)
	if !reflect.DeepEqual(resolved, unresolved) {
		t.Errorf("the conversions differ in more than the ids.\n--- resolved ---\n%s\n--- unresolved ---\n%s",
			describe(resolved), describe(unresolved))
	}
}

// Assuming a resource exists must not extend to assuming the manifest declared
// it. A reference to nothing is a chart-authoring error, and the answer to it is
// the loader's, not the daemon's — so it survives the substitution.
func TestConvertUnresolvedStillNeedsTheReferenceDeclared(t *testing.T) {
	_, err := ConvertUnresolved(context.Background(),
		"services:\n  web:\n    image: nginx\n    secrets: [absent]\n", "s", fakeAPI{})
	if err == nil {
		t.Fatal("ConvertUnresolved = nil, want a reference the manifest never declared to be refused")
	}
	if !strings.Contains(err.Error(), "undefined secret") {
		t.Errorf("error %q does not say what is wrong with the manifest", err)
	}
}

// A manifest that is not valid compose must fail here, while nothing has been
// deployed, rather than partway through an apply.
func TestInvalidManifestFails(t *testing.T) {
	for _, tc := range []struct{ name, manifest, want string }{
		{"not yaml", "services: [oh: no: wait", "parsing the manifest"},
		{"schema violation", "services:\n  web:\n    image: nginx\n    ports: not-a-list\n", "loading the manifest"},
		{"undefined secret", "services:\n  web:\n    image: nginx\n    secrets: [absent]\n", "converting services"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Convert(context.Background(), tc.manifest, "s", fakeAPI{})
			if err == nil {
				t.Fatal("Convert = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say which stage failed (want %q)", err, tc.want)
			}
		})
	}
}

// A real chart from swarmcli-charts, rendered by `swarmcli charts template`.
// The golden file is what the applier will actually be handed in production, so
// a docker/cli bump that changes conversion shows up here as a diff rather than
// as a surprise on somebody's swarm.
func TestTraefikChartGolden(t *testing.T) {
	got := convertOK(t, mustRead(t, "testdata/traefik.yaml"), "edge", nil)

	golden := "testdata/traefik.golden"
	dump := describe(got)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(dump), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if want := mustRead(t, golden); dump != want {
		t.Errorf("conversion changed.\n--- got ---\n%s\n--- want ---\n%s\nRe-run with UPDATE_GOLDEN=1 if this is intended.", dump, want)
	}
}

// --- helpers ---

func serviceNames(s *Stack) []string {
	out := make([]string, 0, len(s.Services))
	for _, svc := range s.Services {
		out = append(out, svc.Name)
	}
	return out
}

func networkNames(s *Stack) []string {
	out := make([]string, 0, len(s.Networks))
	for _, nw := range s.Networks {
		out = append(out, nw.Name)
	}
	return out
}

// forgetIDs blanks the one thing a conversion can only learn from a daemon, so
// that two conversions of the same manifest can be compared for everything else.
func forgetIDs(s *Stack) {
	for _, svc := range s.Services {
		cs := svc.Spec.TaskTemplate.ContainerSpec
		for _, ref := range cs.Configs {
			ref.ConfigID = ""
		}
		for _, ref := range cs.Secrets {
			ref.SecretID = ""
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// describe renders a stack for the golden file. yaml over the whole spec rather
// than a hand-picked summary: the point of the golden is to notice a conversion
// change nobody asked for, and a summary only notices the fields somebody
// thought to list.
func describe(s *Stack) string {
	type dump struct {
		Namespace        string             `yaml:"namespace"`
		Services         []Service          `yaml:"services"`
		Networks         []Network          `yaml:"networks"`
		Configs          []swarm.ConfigSpec `yaml:"configs,omitempty"`
		Secrets          []swarm.SecretSpec `yaml:"secrets,omitempty"`
		ExternalNetworks []string           `yaml:"externalNetworks,omitempty"`
	}
	out, err := yaml.Marshal(dump{
		Namespace:        s.Namespace.Name(),
		Services:         s.Services,
		Networks:         s.Networks,
		Configs:          s.Configs,
		Secrets:          s.Secrets,
		ExternalNetworks: s.ExternalNetworks,
	})
	if err != nil {
		panic(err)
	}
	return string(out)
}
