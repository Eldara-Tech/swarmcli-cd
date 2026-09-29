// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package compose

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/cli/cli/compose/loader"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"

	"github.com/Eldara-Tech/swarmcli-cd/application"
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

// convertOK converts under an application permitted nothing, which is what an
// application that says nothing is permitted (#64). Most manifests here need no
// permission at all: everything a stack owns is scoped to its own release.
func convertOK(t *testing.T, manifest, stack string, api client.APIClient) *Stack {
	t.Helper()
	return convertAllowing(t, manifest, stack, api, application.Allow{})
}

// traefikAllow is what an operator writes to reconcile the traefik fixture: the
// chart's swarm provider talks to the daemon, and that one line is the whole of
// what #64 restores.
var traefikAllow = application.Allow{HostPaths: []string{"/var/run/docker.sock"}}

func convertAllowing(t *testing.T, manifest, stack string, api client.APIClient, allow application.Allow) *Stack {
	t.Helper()
	if api == nil {
		api = fakeAPI{}
	}
	got, err := Convert(context.Background(), manifest, stack, nil, api, allow)
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
//
// The secrets are driver-backed, because since #99 that is the one way a stack
// owns a secret: a secret's `file:` is refused, and an `external:` declaration is
// a reference to an operator's resource rather than something this stack
// creates, so it never reaches Stack.Secrets at all.
func TestConvertOrdersEverythingByName(t *testing.T) {
	got := convertOK(t, `
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
secrets:
  zsec:
    driver: vault
  asec:
    driver: vault
`, "s", nil)

	if names := serviceNames(got); !reflect.DeepEqual(names, []string{"alpha", "middle", "zeta"}) {
		t.Errorf("services = %v, want them sorted", names)
	}
	if names := networkNames(got); !reflect.DeepEqual(names, []string{"s_alpha", "s_zulu"}) {
		t.Errorf("networks = %v, want them sorted", names)
	}
	if len(got.Secrets) != 2 || got.Secrets[0].Name != "s_asec" || got.Secrets[1].Name != "s_zsec" {
		t.Errorf("secrets = %+v, want them sorted and scoped", got.Secrets)
	}
}

// The property the ordering exists for: the same manifest twice is the same
// work list twice, down to the bytes.
func TestConvertIsDeterministic(t *testing.T) {
	manifest := mustRead(t, "testdata/traefik.yaml")

	first := convertAllowing(t, manifest, "edge", nil, traefikAllow)
	for i := range 5 {
		again := convertAllowing(t, manifest, "edge", nil, traefikAllow)
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

// The other half of interpolation is the file format's own: `$$` is one `$`,
// and a chart writes it so the CONTAINER's shell does the expanding. Handing
// `$$` to that shell instead gets the pid of the process it runs in, so a
// password reads "1(cat /run/secrets/db)" and the app fails far from here
// (Eldara-Tech/swarmcli-charts#108).
func TestConvertUnescapesTheComposeDollarEscape(t *testing.T) {
	t.Setenv("SECRET_FROM_THE_CONTROLLER", "leaked")

	got := convertOK(t, `
services:
  web:
    image: nginx
    entrypoint: ["/bin/bash", "-c"]
    command:
      - |-
        export PASS="$$(cat /run/secrets/db)"
        exec nginx -g "daemon off;"
    environment:
      HTPASSWD: "user:$$apr1$$salt$$hash"
      LITERAL: "${SECRET_FROM_THE_CONTROLLER}"
      DOUBLED: "$$$$"
    healthcheck:
      test: ["CMD-SHELL", "curl -sf http://$$HOSTNAME:80/ || exit 1"]
`, "s", nil)

	cs := got.Services[0].Spec.TaskTemplate.ContainerSpec

	// command -> Args: the case charts/zammad hit. A whole shell wrapper, so
	// the fix has to reach a string nested in a list under a service.
	wantArgs := "export PASS=\"$(cat /run/secrets/db)\"\nexec nginx -g \"daemon off;\""
	if len(cs.Args) != 1 || cs.Args[0] != wantArgs {
		t.Errorf("args = %q, want %q", cs.Args, wantArgs)
	}

	// environment -> Env: a map's values. `$apr1$` is charts/traefik's htpasswd
	// hash, which would otherwise be a silently wrong one rather than a failure;
	// `$$$$` must reduce to `$$`, not to `$`; and substitution stays off, so the
	// controller's environment is still not an input.
	wantEnv := map[string]string{
		"HTPASSWD": "user:$apr1$salt$hash",
		"DOUBLED":  "$$",
		"LITERAL":  "${SECRET_FROM_THE_CONTROLLER}",
	}
	for _, e := range cs.Env {
		k, v, _ := strings.Cut(e, "=")
		want, ok := wantEnv[k]
		if !ok {
			t.Errorf("env %q is not one of the three set", e)
			continue
		}
		if v != want {
			t.Errorf("env %s = %q, want %q", k, v, want)
		}
		delete(wantEnv, k)
	}
	for k := range wantEnv {
		t.Errorf("env %s missing", k)
	}

	// healthcheck.test -> Healthcheck.Test: a list the shell expands at runtime.
	wantTest := []string{"CMD-SHELL", "curl -sf http://$HOSTNAME:80/ || exit 1"}
	if cs.Healthcheck == nil || !reflect.DeepEqual(cs.Healthcheck.Test, wantTest) {
		t.Errorf("healthcheck = %+v, want test %q", cs.Healthcheck, wantTest)
	}
}

// An external network is a promise the manifest makes about the swarm, not
// something to conjure. Creating one would silently produce an empty overlay
// where the operator meant an existing shared network.
func TestExternalNetworksAreReportedNotCreated(t *testing.T) {
	got := convertAllowing(t, mustRead(t, "testdata/traefik.yaml"), "edge", nil, traefikAllow)

	if !reflect.DeepEqual(got.ExternalNetworks, []string{"traefik-public"}) {
		t.Errorf("external = %v, want [traefik-public]", got.ExternalNetworks)
	}
	if len(got.Networks) != 0 {
		t.Errorf("networks = %+v, want nothing to create", got.Networks)
	}
}

// An external config or secret is reported like an external network: by the
// name it has on the swarm — its name:, the deprecated external.name, or else
// its key — with the labels it was declared with, and never among what the stack
// creates. Sorted by that name rather than by key, which here differ in order;
// an entry that is not external is not among them.
func TestExternalConfigsAndSecretsAreReportedNotCreated(t *testing.T) {
	const manifest = `
services:
  web:
    image: nginx
configs:
  site:
    external: true
    labels:
      team: web
  legacy:
    external:
      name: zz-legacy
secrets:
  key:
    external: true
    name: shared-key
  own:
    driver: vault
`
	got := convertOK(t, manifest, "s", nil)

	wantConfigs := []swarm.Annotations{
		{Name: "site", Labels: map[string]string{"team": "web"}},
		{Name: "zz-legacy"},
	}
	if !reflect.DeepEqual(got.ExternalConfigs, wantConfigs) {
		t.Errorf("external configs = %+v, want %+v", got.ExternalConfigs, wantConfigs)
	}
	if want := []swarm.Annotations{{Name: "shared-key"}}; !reflect.DeepEqual(got.ExternalSecrets, want) {
		t.Errorf("external secrets = %+v, want %+v", got.ExternalSecrets, want)
	}
	if len(got.Configs) != 0 || len(got.Secrets) != 1 || got.Secrets[0].Name != "s_own" {
		t.Errorf("configs %+v and secrets %+v, want only s_own to create", got.Configs, got.Secrets)
	}
}

// An external volume is reported by the name its mount carries — its name:, the
// deprecated external.name, or else its key — sorted, and a volume the stack
// declares as its own is not among them, although its mount carries a name too.
func TestExternalVolumesAreReportedByTheirMountName(t *testing.T) {
	const manifest = `
services:
  web:
    image: nginx
    volumes: ["data:/data", "shared:/shared", "legacy:/legacy", "plain:/plain"]
volumes:
  data: {}
  shared:
    external: true
    name: s_shared-cache
  legacy:
    external:
      name: aa-legacy
  plain:
    external: true
`
	got := convertOK(t, manifest, "s", nil)

	if want := []string{"aa-legacy", "plain", "s_shared-cache"}; !reflect.DeepEqual(got.ExternalVolumes, want) {
		t.Errorf("external volumes = %q, want %q", got.ExternalVolumes, want)
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
		// Windows-style sources, which compose reads as binds and which the
		// heuristic this replaced never saw at all — the drive letter's colon is
		// not a separator, so cutting at the first one yielded a source of "C"
		// that looked like a volume name (#153). They are refused rather than
		// permitted, because an allow.hostPaths entry is a Linux path and this
		// controller reconciles a Linux swarm.
		{"a drive letter", `['C:\data:/data']`},
		{"a UNC path", `['\\server\share:/data']`},
		{"drive-relative", `["C:data:/data"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Convert(context.Background(),
				"services:\n  app:\n    image: x\n    volumes: "+tc.volumes+"\n", "s", nil, fakeAPI{}, application.Allow{})
			if err == nil {
				t.Fatal("Convert = nil, want a relative bind source to be refused")
			}
			if !strings.Contains(err.Error(), "must be absolute") {
				t.Errorf("error %q does not say why", err)
			}
			if !strings.Contains(err.Error(), `service 'app'`) {
				t.Errorf("error %q does not name the service", err)
			}
		})
	}
}

// The shapes a bind can take, all of them permitted by one entry, and every
// other kind of volume passing untouched. The host paths are under /srv and
// nothing else here is a host path at all: a named volume, an anonymous one and
// a long-form volume have no host side, so a check that treated them as one
// would refuse most charts that persist anything.
func TestPermittedBindsAndNamedVolumesAreAccepted(t *testing.T) {
	got := convertAllowing(t, `
services:
  app:
    image: x
    volumes:
      - /srv/app/data:/data:ro
      - /srv/app/state:/state
      - data:/data2
      - /anonymous
      - {type: volume, source: named, target: /named}
      - {type: bind, source: /srv/app/app.conf, target: /etc/app.conf}
      - my.data:/data3
volumes:
  data: {}
  named: {}
  my.data: {}
`, "s", nil, application.Allow{HostPaths: []string{"/srv/app"}})

	if n := len(got.Services[0].Spec.TaskTemplate.ContainerSpec.Mounts); n != 7 {
		t.Errorf("got %d mounts, want 7", n)
	}
}

// The refusal, in the ways a manifest can write a host path.
//
// Every one of these is refused for the same reason and it is not the reason
// #103 gave: not "this is the docker socket" but "this application named no host
// paths", so an ordinary /srv/app is here beside the socket. That is the
// allowlist — an entry is what makes a path bindable, and nothing else is.
//
// The socket cases are kept because they are the ones with teeth: a chart
// chooses where its services run, so `node.role == manager` plus that socket is
// the swarm's control plane. Both spellings are here, and so is every directory
// containing one, because under a denylist each was a rule that had to be
// remembered — under this one they are refused by saying nothing, which is what
// makes the direction worth having.
func TestAnUnpermittedHostPathIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, volumes string }{
		{"the socket", `["/var/run/docker.sock:/var/run/docker.sock"]`},
		{"read only", `["/var/run/docker.sock:/var/run/docker.sock:ro"]`},
		{"the other spelling", `["/run/docker.sock:/var/run/docker.sock"]`},
		{"the directory holding it", `["/var/run:/var/run"]`},
		{"the other directory", `["/run:/run"]`},
		{"one level further out", `["/var:/host/var"]`},
		{"the whole root filesystem", `["/:/host"]`},
		{"a trailing slash", `["/var/run/:/var/run"]`},
		{"an unclean path", `["/var/lib/../run/docker.sock:/var/run/docker.sock"]`},
		{"long syntax", `[{type: bind, source: /var/run/docker.sock, target: /var/run/docker.sock}]`},
		// The type that is not a bind and names a host path anyway. Nothing else
		// in this package would notice, and the manifest converts without
		// complaint — see bindSource.
		{"long syntax as a named pipe", `[{type: npipe, source: /var/run/docker.sock, target: /var/run/docker.sock}]`},
		// The daemon's data root, which #103 named as what its socket rule
		// deliberately left open: it holds the contents of every named volume on
		// the node, this controller's own included.
		{"the daemon's data root", `["/var/lib/docker:/host/docker"]`},
		// And an ordinary one, which was allowed before this and is not now.
		{"an ordinary host path", `["/srv/app/data:/data"]`},
		{"traefik", `["/var/run/docker.sock:/var/run/docker.sock:ro", "certs:/certificates"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Convert(context.Background(),
				"services:\n  app:\n    image: traefik:v3.7.1\n    volumes: "+tc.volumes+"\n"+
					"volumes:\n  certs: {}\n", "s", nil, fakeAPI{}, application.Allow{})
			if err == nil {
				t.Fatal("Convert = nil, want a host path this application may not bind refused")
			}
			if !strings.Contains(err.Error(), "allow.hostPaths") {
				t.Errorf("error %q does not say where the permission would go", err)
			}
			if !strings.Contains(err.Error(), `service 'app'`) {
				t.Errorf("error %q does not name the service", err)
			}
		})
	}
}

// The capability #64 exists to give back. Traefik's swarm provider needs the
// daemon and #103 could only refuse it, so this chart could not be reconciled by
// this controller at all — it is the manifest of the fixture beside this file,
// with the bind the published chart actually carries.
//
// The third case is the one that makes the gate a gate rather than a switch: an
// application permitted a different path is refused exactly as one permitted
// nothing is.
func TestAPermittedHostPathIsBound(t *testing.T) {
	const traefik = "services:\n  app:\n    image: traefik:v3.7.1\n" +
		"    volumes: [\"/var/run/docker.sock:/var/run/docker.sock:ro\", \"certs:/certificates\"]\n" +
		"volumes:\n  certs: {}\n"

	for _, tc := range []struct {
		name    string
		allow   application.Allow
		refused bool
	}{
		{"the socket itself", application.Allow{HostPaths: []string{"/var/run/docker.sock"}}, false},
		// A bind of a directory is everything under it, so permitting the
		// directory permits the socket in it. Written down because it is the
		// footgun of the containment rule as much as its convenience.
		{"the directory holding it", application.Allow{HostPaths: []string{"/var/run"}}, false},
		{"somewhere else entirely", application.Allow{HostPaths: []string{"/srv/app"}}, true},
		// The one-directional half. Permitting a path below the bind does not
		// permit the bind, which would otherwise be a way to write "/var/run"
		// without meaning to.
		{"a path under it", application.Allow{HostPaths: []string{"/var/run/docker.sock/deeper"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Convert(context.Background(), traefik, "edge", nil, fakeAPI{}, tc.allow)
			if tc.refused {
				if err == nil {
					t.Fatal("Convert = nil, want a bind no entry covers refused")
				}
				return
			}
			if err != nil {
				t.Fatalf("Convert = %v, want the permitted bind converted", err)
			}
			mounts := got.Services[0].Spec.TaskTemplate.ContainerSpec.Mounts
			if len(mounts) != 2 || mounts[0].Source != "/var/run/docker.sock" {
				t.Errorf("mounts = %+v, want the socket bound", mounts)
			}
		})
	}
}

// chartConfigs is a chart that ships its configs' content: the shapes a config's
// file: may take, and the bytes the chart engine resolved them to.
const chartConfigs = `
services:
  web:
    image: nginx
configs:
  site:
    file: files/site.conf
    labels:
      tier: front
    template_driver: golang
  operator:
    file: values/operator.conf
  pinned:
    name: s_site-v2
    file: ./files/nested/../site.conf
  shared:
    external: true
`

var chartConfigFiles = map[string][]byte{
	"files/site.conf":      []byte("listen 80;\n"),
	"values/operator.conf": []byte("from --set-file\n"),
}

// A config takes its content from the files the chart engine handed over with
// the manifest, and from nowhere else. Each shape a chart writes is here: a file
// the chart ships, a value the operator supplied, an explicit name, and a path
// that only becomes a key once cleaned — which is the key the engine stored it
// under.
func TestAConfigTakesItsContentFromTheChartsFiles(t *testing.T) {
	got, err := Convert(context.Background(), chartConfigs, "s", chartConfigFiles, fakeAPI{}, application.Allow{})
	if err != nil {
		t.Fatalf("Convert = %v, want a chart's own configs converted", err)
	}

	byName := map[string]swarm.ConfigSpec{}
	for _, c := range got.Configs {
		byName[c.Name] = c
	}
	if len(got.Configs) != 3 {
		t.Fatalf("configs = %v, want three — the external one is a reference, not the stack's", configNames(got))
	}

	site := byName["s_site"]
	if string(site.Data) != "listen 80;\n" {
		t.Errorf("s_site data = %q, want the chart's files/site.conf", site.Data)
	}
	if site.Labels["tier"] != "front" || site.Labels["com.docker.stack.namespace"] != "s" {
		t.Errorf("s_site labels = %v, want its own and the stack's namespace", site.Labels)
	}
	if site.Templating == nil || site.Templating.Name != "golang" {
		t.Errorf("s_site templating = %+v, want the golang template driver", site.Templating)
	}

	if op := byName["s_operator"]; string(op.Data) != "from --set-file\n" || op.Templating != nil {
		t.Errorf("s_operator = %+v, want the value's bytes and no template driver", op)
	}

	pinned, ok := byName["s_site-v2"]
	if !ok {
		t.Fatalf("configs = %v, want the explicit name: kept as written", configNames(got))
	}
	if string(pinned.Data) != "listen 80;\n" {
		t.Errorf("s_site-v2 data = %q, want files/site.conf, which is what its path cleans to", pinned.Data)
	}

	// No files is not an empty chart: the same manifest rendered without them
	// names content nothing handed over.
	if _, err := Convert(context.Background(), chartConfigs, "s", nil, fakeAPI{}, application.Allow{}); err == nil {
		t.Error("Convert with no files = nil, want a config naming a file refused")
	}
}

// Every way a config's file: can fail to name something the chart shipped, each
// refused with its own reason. files holds a key for the shapes whose only fault
// is the path itself, so what refuses them is the rule rather than a missing key.
func TestAConfigFileNotShippedByTheChartIsRefused(t *testing.T) {
	files := map[string][]byte{
		"files/app.conf":      []byte("x"),
		"files/${X}/app.conf": []byte("x"),
		"files/a$$b.conf":     []byte("x"),
		"app.conf":            []byte("x"),
		"files.conf":          []byte("x"),
		"filesystem/a.conf":   []byte("x"),
		"values.conf":         []byte("x"),
	}
	for _, tc := range []struct{ name, file, why string }{
		{"absolute", "/run/secrets/swarmcli-cd-token", "is an absolute path"},
		{"parent", "../app.conf", "escapes the chart"},
		{"parent after cleaning", "files/../../app.conf", "escapes the chart"},
		{"outside files and values", "app.conf", "is outside files/ and values/"},
		// Beside files/ and values/ rather than in them: the directory is the
		// prefix, slash included.
		{"a sibling of files/", "files.conf", "is outside files/ and values/"},
		{"a directory named like files/", "filesystem/a.conf", "is outside files/ and values/"},
		{"a sibling of values/", "values.conf", "is outside files/ and values/"},
		{"not shipped", "files/missing.conf", "is not shipped by the chart"},
		// Interpolation-shaped paths are refused, whether or not a key of that
		// spelling exists.
		{"interpolation-shaped", "files/${X}/app.conf", "interpolation-shaped"},
		// The path is quoted as it is read, which is after the $$ escape.
		{"an escaped dollar", "files/a$$b.conf", "'files/a$b.conf' (with $$ read as $) is interpolation-shaped"},
		{"not a string", "42", "must be a path, got 42 (int)"},
		{"no path at all", "", "names no path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := "services:\n  x:\n    image: alpine\n" +
				"configs:\n  site:\n    file: " + tc.file + "\n"
			_, err := Convert(context.Background(), manifest, "s", files, fakeAPI{}, application.Allow{})
			if err == nil {
				t.Fatalf("Convert = nil, want file: %s refused", tc.file)
			}
			for _, want := range []string{"config 'site'", tc.why, chartFilesRule} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
		})
	}
}

// A config with no file: and no external: has nowhere for its content to come
// from, and is refused rather than created empty.
func TestAConfigWithoutContentIsRefused(t *testing.T) {
	_, err := Convert(context.Background(),
		"services:\n  x:\n    image: alpine\nconfigs:\n  site:\n    labels:\n      a: b\n",
		"s", chartConfigFiles, fakeAPI{}, application.Allow{})
	if err == nil {
		t.Fatal("Convert = nil, want a config with no source refused")
	}
	if !strings.Contains(err.Error(), "config 'site' has no content") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

// A secret's file: and a service's env_file: stay refused whatever the chart
// ships — the files handed over here would satisfy either path. The secret beside
// external: is refused too: the refusal is of the key.
func TestASecretFileAndAnEnvFileAreRefused(t *testing.T) {
	files := map[string][]byte{"files/token": []byte("x"), "files/app.env": []byte("A=1\n")}
	for _, tc := range []struct{ name, manifest, want string }{
		{
			"a secret's file:",
			"services:\n  x:\n    image: alpine\n    secrets: [token]\n" +
				"secrets:\n  token:\n    file: files/token\n",
			"secret 'token': file: is refused",
		},
		{
			"a secret's file: beside external:",
			"services:\n  x:\n    image: alpine\n" +
				"secrets:\n  token:\n    external: true\n    file: files/token\n",
			"secret 'token': file: is refused",
		},
		{
			"env_file: as a string",
			"services:\n  x:\n    image: alpine\n    env_file: files/app.env\n",
			"service 'x': env_file: is refused; set variables with environment:",
		},
		{
			"env_file: as a list",
			"services:\n  x:\n    image: alpine\n    env_file: [files/app.env]\n",
			"service 'x': env_file: is refused; set variables with environment:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Convert(context.Background(), tc.manifest, "s", files, fakeAPI{}, application.Allow{})
			if err == nil {
				t.Fatal("Convert = nil, want it refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// Every secret that reaches conversion is external or driver-backed. That is a
// property of the value, not of which keys are present, so each of these is
// refused although none of them names a file.
func TestASecretThatIsNeitherExternalNorDriverBackedIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, secret string }{
		{"no source at all", "labels:\n      a: b"},
		{"external: false", "external: false"},
		{"an empty driver", `driver: ""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := "services:\n  x:\n    image: alpine\n" +
				"secrets:\n  token:\n    " + tc.secret + "\n"
			_, err := Convert(context.Background(), manifest, "s", nil, fakeAPI{}, application.Allow{})
			if err == nil {
				t.Fatal("Convert = nil, want the secret refused")
			}
			for _, want := range []string{"secret 'token'", "neither external: nor driver-backed", "declare it external:"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
		})
	}
}

// A mapping with a key that is not a string arrives as a map[any]any rather than
// the map[string]any every later check reads, so it is refused wherever it sits
// — in a section, in an entry, or inside a list — and the message says where.
func TestAMappingWithANonStringKeyIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, manifest, at string }{
		{
			"in a section",
			"services:\n  1:\n    image: alpine\n  web:\n    image: alpine\n    env_file: files/app.env\n",
			"'services'",
		},
		{
			"in an entry",
			"services:\n  web:\n    image: alpine\n    env_file: files/app.env\n    1: x\n",
			"'services.web'",
		},
		{
			"inside a list",
			"services:\n  web:\n    image: alpine\n    configs:\n      - source: site\n        1: x\n",
			"'services.web.configs[0]'",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Convert(context.Background(), tc.manifest, "s", nil, fakeAPI{}, application.Allow{})
			if err == nil {
				t.Fatal("Convert = nil, want the mapping refused")
			}
			if !strings.Contains(err.Error(), tc.at) || !strings.Contains(err.Error(), "not a string") {
				t.Errorf("error %q does not say which mapping, or why", err)
			}
		})
	}
}

// convertConfigs reimplements upstream's conversion minus the read, and this is
// what keeps the two the same: one manifest converted both ways, upstream reading
// the same bytes from a directory it was pointed at. A docker/cli bump that
// changes the upstream conversion fails here.
func TestConvertConfigsMatchesUpstream(t *testing.T) {
	got, err := Convert(context.Background(), chartConfigs, "s", chartConfigFiles, fakeAPI{}, application.Allow{})
	if err != nil {
		t.Fatalf("Convert = %v, want nil", err)
	}

	dir := t.TempDir()
	for key, data := range chartConfigFiles {
		p := filepath.Join(dir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dict, err := loader.ParseYAML([]byte(chartConfigs))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loader.Load(composetypes.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []composetypes.ConfigFile{{Config: dict}},
	}, func(o *loader.Options) { o.SkipInterpolation = true })
	if err != nil {
		t.Fatal(err)
	}
	want, err := convert.Configs(convert.NewNamespace("s"), cfg.Configs)
	if err != nil {
		t.Fatal(err)
	}

	byName := func(a, b swarm.ConfigSpec) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(want, byName)
	if len(want) != 3 {
		t.Fatalf("upstream converted %d configs, want 3; the comparison below would prove less than it claims", len(want))
	}
	if !reflect.DeepEqual(got.Configs, want) {
		t.Errorf("configs differ from upstream's.\n--- in memory ---\n%+v\n--- upstream ---\n%+v", got.Configs, want)
	}
}

// No production file in this package reads a path. Config content comes from the
// files a caller hands over, and this is what holds that down: it fails on any
// reference to the calls that would read one instead — convert.Configs, which
// reads each config's file:, and the os, io/ioutil and path/filepath functions
// that open, read, list, walk or stat a path.
//
// An *os.Root's methods read too, and a scan without type information cannot
// see a method call's receiver. So the type itself is banned — os.Root — along
// with os.OpenRoot, which returns one, and os.OpenInRoot, which opens through
// one; that leaves no way to name or make a root in this package. One returned by
// another package's function and held only in an inferred variable would still
// pass, and nothing this package imports returns one.
//
// Two upstream calls this package keeps have read branches of their own, which a
// scan of this package cannot see; what keeps each shut is a guard, not this
// test. loader.Load reads a service's env_file: while resolving its environment
// — shut because checkFileSources refuses env_file:, and checkStringKeys refuses
// any block that could hide one from it. convert.Secrets reads a secret's file: —
// shut because checkSecretSources lets only external and driver-backed secrets
// through, and it reads for neither. Allowing either key later means revisiting
// this.
//
// Test files are exempt: the parity test above reads through upstream on purpose.
func TestNoProductionFileReadsAPath(t *testing.T) {
	banned := map[string][]string{
		"os": {"ReadFile", "Open", "OpenFile", "OpenRoot", "OpenInRoot", "ReadDir", "DirFS", "Stat", "Lstat",
			"Readlink", "Root"},
		"io/ioutil":     {"ReadFile", "ReadDir"},
		"path/filepath": {"Glob", "Walk", "WalkDir"},
		"github.com/docker/cli/cli/compose/convert": {"Configs"},
	}

	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var scanned []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		scanned = append(scanned, name)

		// Local name to import path, so an aliased import is caught too.
		imported := map[string]string{}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			local := path.Base(p)
			if imp.Name != nil {
				local = imp.Name.Name
			}
			if _, ok := banned[p]; ok && local == "." {
				t.Errorf("%s: dot-imports %s, which would hide a banned call from this check", name, p)
			}
			imported[local] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if slices.Contains(banned[imported[pkg.Name]], sel.Sel.Name) {
				t.Errorf("%s: %s.%s reads a path; config content comes from the files the caller hands over",
					fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			}
			return true
		})
	}
	if !slices.Contains(scanned, "compose.go") {
		t.Fatalf("scanned %v, which does not include compose.go; the check would pass without looking", scanned)
	}
}

// None of the refusals above may be reachable by a manifest that names no path
// at all. An over-eager version of these checks would refuse most charts there
// are.
func TestAManifestWithoutFileSourcesIsUntouched(t *testing.T) {
	got := convertAllowing(t, `
services:
  web:
    image: nginx
    environment:
      PORT: "80"
    volumes:
      - /srv/www:/usr/share/nginx/html:ro
    networks: [front]
networks:
  front: {}
`, "s", nil, application.Allow{HostPaths: []string{"/srv/www"}})

	if len(got.Services) != 1 || len(got.Networks) != 1 {
		t.Errorf("got %+v, want the manifest converted untouched", got)
	}
}

// Resolving a referenced secret to its id is the one thing conversion cannot do
// offline, and getting it wrong means a service that references a secret Swarm
// will not attach.
//
// Both are `external:`, which is also this file's regression test for #99: what
// that check refuses is a path, and an external declaration carries none — it
// names something an operator created on the swarm. Its name is the operator's
// rather than namespace-scoped, for the same reason.
func TestServiceSecretsAndConfigsResolveThroughTheClient(t *testing.T) {
	api := fakeAPI{
		secrets: []swarm.Secret{{ID: "sec-id", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "apikey"}}}},
		configs: []swarm.Config{{ID: "cfg-id", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "appconf"}}}},
	}
	got := convertOK(t, `
services:
  app:
    image: x
    secrets: [apikey]
    configs: [appconf]
secrets:
  apikey:
    external: true
configs:
  appconf:
    external: true
`, "s", api)

	spec := got.Services[0].Spec.TaskTemplate.ContainerSpec
	if len(spec.Secrets) != 1 || spec.Secrets[0].SecretID != "sec-id" || spec.Secrets[0].SecretName != "apikey" {
		t.Errorf("secrets = %+v, want the id the daemon reported", spec.Secrets)
	}
	if len(spec.Configs) != 1 || spec.Configs[0].ConfigID != "cfg-id" || spec.Configs[0].ConfigName != "appconf" {
		t.Errorf("configs = %+v, want the id the daemon reported", spec.Configs)
	}
	// An external declaration is a reference; there is nothing for this stack
	// to create.
	if len(got.Secrets) != 0 || len(got.Configs) != 0 {
		t.Errorf("secrets = %+v, configs = %+v, want nothing to create", got.Secrets, got.Configs)
	}
}

// declaresAndMounts is the chart shape swarmcli-cd#84 was filed for: it mounts a
// secret of its own that does not exist until this controller creates it, next to
// a reference to a config that already does.
//
// The secret is driver-backed because since #99 that is the only way a stack owns
// a secret — `file:` was the other way, and it read the controller's filesystem
// rather than the chart's.
const declaresAndMounts = `
services:
  app:
    image: busybox
    configs: [site]
    secrets: [apikey]
configs:
  site:
    external: true
secrets:
  apikey:
    driver: vault
`

// The conversion that is applied cannot also be the one that decides whether to
// apply it: it resolves each reference against the daemon, and a chart's own
// config does not exist until this controller creates it. So Convert refuses
// this manifest, and ConvertUnresolved is what the mount guard reads instead.
func TestConvertUnresolvedNeedsNothingToExistYet(t *testing.T) {
	_, err := Convert(context.Background(), declaresAndMounts, "rel", nil, fakeAPI{}, application.Allow{})
	if err == nil {
		t.Fatal("Convert = nil, want a reference to a resource that does not exist yet to fail")
	}
	if !strings.Contains(err.Error(), "secret not found: rel_apikey") {
		t.Errorf("error %q is not the unresolvable reference", err)
	}

	got, err := ConvertUnresolved(context.Background(), declaresAndMounts, "rel", nil, fakeAPI{}, application.Allow{})
	if err != nil {
		t.Fatalf("ConvertUnresolved = %v, want nil", err)
	}

	// The names are the ones a deploy will create and mount; only the ids are
	// made up, and nothing reads those.
	if len(got.Configs) != 0 {
		t.Errorf("configs = %+v, want nothing to create for an external reference", got.Configs)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Name != "rel_apikey" {
		t.Errorf("secrets = %+v, want one scoped rel_apikey to create", got.Secrets)
	}
	cs := got.Services[0].Spec.TaskTemplate.ContainerSpec
	if len(cs.Configs) != 1 || cs.Configs[0].ConfigName != "site" || cs.Configs[0].ConfigID != unresolvedID {
		t.Errorf("mounted configs = %+v, want site with an unresolved id", cs.Configs)
	}
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretName != "rel_apikey" || cs.Secrets[0].SecretID != unresolvedID {
		t.Errorf("mounted secrets = %+v, want rel_apikey with an unresolved id", cs.Secrets)
	}
}

// What the guard's soundness rests on: the two conversions of one manifest agree
// on everything except the ids. If they could disagree on a name, a stack refused
// by the first could still be deployed by the second.
func TestConvertUnresolvedDiffersOnlyByTheIds(t *testing.T) {
	api := fakeAPI{
		configs: []swarm.Config{{ID: "cfg-id", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "site"}}}},
		secrets: []swarm.Secret{{ID: "sec-id", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "rel_apikey"}}}},
	}

	resolved := convertOK(t, declaresAndMounts, "rel", api)
	unresolved, err := ConvertUnresolved(context.Background(), declaresAndMounts, "rel", nil, api, application.Allow{})
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
		"services:\n  web:\n    image: nginx\n    secrets: [absent]\n", "s", nil, fakeAPI{}, application.Allow{})
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
			_, err := Convert(context.Background(), tc.manifest, "s", nil, fakeAPI{}, application.Allow{})
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
	got := convertAllowing(t, mustRead(t, "testdata/traefik.yaml"), "edge", nil, traefikAllow)

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

func configNames(s *Stack) []string {
	out := make([]string, 0, len(s.Configs))
	for _, c := range s.Configs {
		out = append(out, c.Name)
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
		Namespace        string              `yaml:"namespace"`
		Services         []Service           `yaml:"services"`
		Networks         []Network           `yaml:"networks"`
		Configs          []swarm.ConfigSpec  `yaml:"configs,omitempty"`
		Secrets          []swarm.SecretSpec  `yaml:"secrets,omitempty"`
		ExternalNetworks []string            `yaml:"externalNetworks,omitempty"`
		ExternalConfigs  []swarm.Annotations `yaml:"externalConfigs,omitempty"`
		ExternalSecrets  []swarm.Annotations `yaml:"externalSecrets,omitempty"`
		ExternalVolumes  []string            `yaml:"externalVolumes,omitempty"`
	}
	out, err := yaml.Marshal(dump{
		Namespace:        s.Namespace.Name(),
		Services:         s.Services,
		Networks:         s.Networks,
		Configs:          s.Configs,
		Secrets:          s.Secrets,
		ExternalNetworks: s.ExternalNetworks,
		ExternalConfigs:  s.ExternalConfigs,
		ExternalSecrets:  s.ExternalSecrets,
		ExternalVolumes:  s.ExternalVolumes,
	})
	if err != nil {
		panic(err)
	}
	return string(out)
}
