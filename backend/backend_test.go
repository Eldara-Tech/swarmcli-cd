// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
)

const oneOfEach = `
services:
  web:
    image: nginx
    networks: [front]
    configs: [site]
    secrets: [apikey]
networks:
  front: {}
configs:
  site:
    external: true
    name: s_site
secrets:
  apikey:
    external: true
    name: s_apikey
`

// oneOfEachAllowed permits what oneOfEach references. An external: reference
// needs the app set's permission whatever it is called — the manifest has said it
// is not the release's — so names scoped under the release are no exception.
var oneOfEachAllowed = application.Allow{Configs: []string{"s_site"}, Secrets: []string{"s_apikey"}}

// A service can reference a network, config or secret, so each has to exist
// before the service does. Getting the order wrong produces a create that fails
// on a reference the next call would have satisfied.
func TestDeployStackCreatesReferencesBeforeServices(t *testing.T) {
	api := &fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	}

	if err := allowing(t, api, oneOfEachAllowed).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}

	if len(api.created) != 1 || api.created[0].Name != "s_web" {
		t.Fatalf("created services %+v, want one scoped s_web", api.created)
	}
	// The network is created before the service that joins it.
	want := []string{"network:s_front"}
	if !reflect.DeepEqual(api.order, want) {
		t.Errorf("mutation order = %v, want %v before the service create", api.order, want)
	}
}

// declaresAndMounts is the shape #84 was filed for: a chart that brings its own
// secret and mounts it. Driver-backed, because since #99 that is the one way a
// stack owns a secret — the other way was a path, and compose resolves a path
// against the controller's filesystem rather than the chart's. shipsAConfig is
// the config half.
const declaresAndMounts = `
services:
  app:
    image: busybox
    secrets: [apikey]
secrets:
  apikey:
    driver: vault
`

// A chart may mount what it declares. Converting a service resolves each config
// and secret it mounts to the id Swarm addresses it by, so the conversion that
// is applied cannot run until they exist — which is the ordering #84 got wrong.
//
// The id asserted at the end is the point of the whole arrangement: it is the
// one the daemon reported for the secret this deploy created, so the spec that
// reached the swarm came from the authoritative conversion and not from the
// reference-free one the guard reads.
func TestDeployStackCreatesASecretItThenMounts(t *testing.T) {
	api := &fakeAPI{}

	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "rel", Manifest: declaresAndMounts, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want a chart to be able to mount what it declares", err)
	}

	want := []string{"network:rel_default", "secret:rel_apikey"}
	if !reflect.DeepEqual(api.order, want) {
		t.Errorf("mutation order = %v, want %v", api.order, want)
	}
	if len(api.created) != 1 {
		t.Fatalf("created %d services, want 1", len(api.created))
	}
	cs := api.created[0].TaskTemplate.ContainerSpec
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretName != "rel_apikey" || cs.Secrets[0].SecretID != "sec-rel_apikey" {
		t.Errorf("secrets = %+v, want the id the daemon reported for the secret just created", cs.Secrets)
	}
}

// shipsAConfig is a chart that brings a config's content with it and mounts it:
// the file: names a path in the chart, and the bytes arrive beside the manifest
// as DeployRequest.Files.
const shipsAConfig = `
services:
  app:
    image: busybox
    configs: [site]
configs:
  site:
    file: files/nginx.conf
`

// DeployRequest.Files is where a config's content comes from, and the only
// place. Three things are checked: the config is created with those bytes, the
// service is created holding the id the daemon reported for it — so the applied
// conversion ran after the create, as #84 needs — and nothing was written to
// disk on the way. The temp directory is the probe for the last because it is
// where materialising them would put them: CE's own docker.DeployStackInContext
// writes its manifest there.
func TestDeployStackAppliesTheChartsFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	api := &fakeAPI{}

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{
		Name:     "rel",
		Manifest: shipsAConfig,
		Resolve:  ResolveNever,
		Files:    map[string][]byte{"files/nginx.conf": []byte("server {}\n")},
	})
	if err != nil {
		t.Fatalf("DeployStack = %v, want a chart's own config deployed from its files", err)
	}

	if want := []string{"network:rel_default", "config:rel_site"}; !reflect.DeepEqual(api.order, want) {
		t.Errorf("mutation order = %v, want %v", api.order, want)
	}
	if len(api.createdConfigs) != 1 || string(api.createdConfigs[0].Data) != "server {}\n" {
		t.Errorf("created configs %+v, want rel_site holding the chart's files/nginx.conf", api.createdConfigs)
	}
	if len(api.created) != 1 {
		t.Fatalf("created %d services, want 1", len(api.created))
	}
	cs := api.created[0].TaskTemplate.ContainerSpec
	if len(cs.Configs) != 1 || cs.Configs[0].ConfigName != "rel_site" || cs.Configs[0].ConfigID != "cfg-rel_site" {
		t.Errorf("configs = %+v, want the id the daemon reported for the config just created", cs.Configs)
	}
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d entries under %s, want the files used in memory rather than materialised", len(left), tmp)
	}
}

// The same chart handed no files names content nothing supplied. It is refused
// whole, before anything is created, rather than deployed with an empty config.
func TestDeployStackRefusesAConfigItWasNotGivenTheFilesFor(t *testing.T) {
	api := &fakeAPI{}

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "rel", Manifest: shipsAConfig, Resolve: ResolveNever})
	if err == nil || !strings.Contains(err.Error(), "not shipped by the chart") {
		t.Fatalf("DeployStack = %v, want the config refused for content nothing supplied", err)
	}
	if len(api.order) != 0 || len(api.created) != 0 {
		t.Errorf("created %v and %d services, want nothing", api.order, len(api.created))
	}
}

// A manifest that cannot be converted at all creates nothing. The stack is
// refused whole for every conversion failure and not only for a forbidden mount,
// which is what the reference-free first pass buys: it fails here, before the
// first create, rather than after the configs and secrets are on the swarm.
func TestAnUnconvertibleManifestCreatesNothing(t *testing.T) {
	api := &fakeAPI{}

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: `
services:
  web:
    image: nginx
    secrets: [absent]
`, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a manifest referencing an undeclared secret to be refused")
	}
	if len(api.order) != 0 || len(api.created) != 0 {
		t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
	}
}

// A stack referencing one of the controller's own secrets as external is refused
// whole, before any resource is created — the exfiltration path that would
// otherwise read the controller's admin token, git token or a registry
// credential straight out of a tenant container.
const mountsControllerSecret = `
services:
  evil:
    image: alpine
    secrets: [stolen]
secrets:
  stolen:
    external: true
    name: swarmcli-cd-token
`

func TestDeployStackRefusesMountingAControllerSecret(t *testing.T) {
	api := &fakeAPI{
		secrets: []swarm.Secret{{ID: "tok", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "swarmcli-cd-token"}}}},
	}
	b := testBackend(t, api, nil).WithForbiddenSecrets(map[string]struct{}{"swarmcli-cd-token": {}}).(*Backend)

	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: mountsControllerSecret, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want the stack refused for mounting a controller secret")
	}
	for _, want := range []string{"evil", "swarmcli-cd-token", "controller"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// Refused whole: nothing was created before the reject.
	if len(api.order) != 0 || len(api.created) != 0 {
		t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
	}
}

func TestRejectForbiddenSecretMounts(t *testing.T) {
	withSecret := func(name string) swarm.ServiceSpec {
		return swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:   "nginx",
			Secrets: []*swarm.SecretReference{{SecretName: name}},
		}}}
	}
	forbidden := map[string]struct{}{"swarmcli-cd-token": {}}
	// Permitted by the application, so that the only thing that can refuse these
	// is the forbidden set this test is about.
	permitted := application.Allow{Secrets: []string{"swarmcli-cd-token", "s_apikey"}}

	for name, tc := range map[string]struct {
		forbidden map[string]struct{}
		svc       swarm.ServiceSpec
		wantErr   bool
	}{
		"mounts a forbidden secret":  {forbidden, withSecret("swarmcli-cd-token"), true},
		"mounts an allowed secret":   {forbidden, withSecret("s_apikey"), false},
		"empty forbidden set skips":  {nil, withSecret("swarmcli-cd-token"), false},
		"nil container spec is safe": {forbidden, swarm.ServiceSpec{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			b := testBackend(t, &fakeAPI{}, nil)
			b.forbiddenSecrets = tc.forbidden
			b.allow = permitted
			st := stack("s", cdService{"svc", tc.svc})

			err := b.rejectForbiddenResources(t.Context(), st)
			if tc.wantErr && err == nil {
				t.Fatal("rejectForbiddenResources = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("rejectForbiddenResources = %v, want nil", err)
			}
		})
	}
}

// The forbidden set is controller-wide, but it must not leak onto the shared
// per-swarm backend — the same copy-not-mutate discipline as WithRegistryAuth.
func TestWithForbiddenSecretsDoesNotMutate(t *testing.T) {
	shared := testBackend(t, &fakeAPI{}, nil)
	if _, ok := shared.WithForbiddenSecrets(map[string]struct{}{"swarmcli-cd-token": {}}).(*Backend); !ok {
		t.Fatal("WithForbiddenSecrets did not return a *Backend")
	}
	if shared.forbiddenSecrets != nil {
		t.Error("WithForbiddenSecrets mutated the shared backend")
	}
}

func TestMountedSecretNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"swarmcli-cd-token", "swarmcli-cd-regauth-a"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := MountedSecretNames(dir)
	if err != nil {
		t.Fatalf("MountedSecretNames = %v, want nil", err)
	}
	if _, ok := got["swarmcli-cd-token"]; !ok {
		t.Error("swarmcli-cd-token not in the mounted set")
	}
	if _, ok := got["swarmcli-cd-regauth-a"]; !ok {
		t.Error("swarmcli-cd-regauth-a not in the mounted set")
	}

	// A controller outside a swarm has no /run/secrets and must still start.
	empty, err := MountedSecretNames(filepath.Join(dir, "does-not-exist"))
	if err != nil {
		t.Fatalf("MountedSecretNames(missing) = %v, want nil", err)
	}
	if len(empty) != 0 {
		t.Errorf("missing dir yielded %d names, want 0", len(empty))
	}
}

// The rule Swarm imposes and `docker stack deploy` fumbles: a config's content
// cannot be changed, only its labels. Sending new data gets "only updates to
// Labels are allowed" from the daemon, which names neither the config nor the
// remedy.
func TestConfigContentChangeIsRefusedWithAnExplanation(t *testing.T) {
	api := &fakeAPI{configs: []swarm.Config{{
		ID:   "c",
		Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}, Data: []byte("old")},
	}}}

	err := testBackend(t, api, nil).applyConfigs(context.Background(), []swarm.ConfigSpec{{
		Annotations: swarm.Annotations{Name: "s_site"},
		Data:        []byte("new"),
	}})
	if err == nil {
		t.Fatal("applyConfigs = nil, want a content change to be refused")
	}
	for _, want := range []string{"s_site", "immutable", "content hash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(api.updatedConfigs) != 0 {
		t.Error("a content change was sent to the daemon anyway")
	}
}

// Same content is the normal case on every reconcile after the first. Labels
// may still have moved, so the update is made — with the data stripped, because
// sending it back is what the daemon rejects.
func TestUnchangedConfigUpdatesLabelsWithoutData(t *testing.T) {
	api := &fakeAPI{configs: []swarm.Config{{
		ID:   "c",
		Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}, Data: []byte("same")},
	}}}

	if err := testBackend(t, api, nil).applyConfigs(context.Background(), []swarm.ConfigSpec{{
		Annotations: swarm.Annotations{Name: "s_site", Labels: map[string]string{"a": "b"}},
		Data:        []byte("same"),
	}}); err != nil {
		t.Fatalf("applyConfigs = %v, want nil", err)
	}
	if len(api.updatedConfigs) != 1 {
		t.Fatalf("made %d updates, want 1", len(api.updatedConfigs))
	}
	if api.updatedConfigs[0].Data != nil {
		t.Error("data was sent on an update; the daemon only accepts label changes")
	}
	if api.updatedConfigs[0].Labels["a"] != "b" {
		t.Error("the label change was not sent")
	}
}

// A secret's stored data is unreadable — GetSecret nils out Spec.Data — so
// there is nothing to compare and nothing to send.
func TestSecretUpdateNeverSendsData(t *testing.T) {
	api := &fakeAPI{secrets: []swarm.Secret{{
		ID:   "s",
		Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}},
	}}}

	if err := testBackend(t, api, nil).applySecrets(context.Background(), []swarm.SecretSpec{{
		Annotations: swarm.Annotations{Name: "s_apikey"},
		Data:        []byte("hunter2"),
	}}); err != nil {
		t.Fatalf("applySecrets = %v, want nil", err)
	}
	if len(api.updatedSecrets) != 1 || api.updatedSecrets[0].Data != nil {
		t.Errorf("update = %+v, want the material withheld", api.updatedSecrets)
	}
}

func TestMissingConfigsAndSecretsAreCreated(t *testing.T) {
	api := &fakeAPI{}
	b := testBackend(t, api, nil)

	if err := b.applyConfigs(context.Background(), []swarm.ConfigSpec{{Annotations: swarm.Annotations{Name: "s_site"}}}); err != nil {
		t.Fatalf("applyConfigs = %v, want nil", err)
	}
	if err := b.applySecrets(context.Background(), []swarm.SecretSpec{{Annotations: swarm.Annotations{Name: "s_key"}}}); err != nil {
		t.Fatalf("applySecrets = %v, want nil", err)
	}
	if len(api.createdConfigs) != 1 || len(api.createdSecrets) != 1 {
		t.Errorf("created %d configs and %d secrets, want one of each", len(api.createdConfigs), len(api.createdSecrets))
	}
}

// Swarm cannot update a network in place, so an existing one is left alone —
// removing it disconnects every attached service. Being silent about that is
// the defect #1 names, so the difference is reported.
func TestExistingNetworkIsNotRecreatedButIsReported(t *testing.T) {
	var log strings.Builder
	existing := stackNetwork("n", "s_front", "s")
	existing.Driver, existing.Attachable = "overlay", false
	api := &fakeAPI{networks: []network.Summary{existing}}
	b := New(api, Options{Log: slog.New(slog.NewTextHandler(&log, nil))})

	st := stack("s")
	st.Networks = []cdcompose.Network{{Name: "s_front", Spec: network.CreateOptions{Driver: "overlay", Attachable: true}}}

	if err := b.applyNetworks(context.Background(), st); err != nil {
		t.Fatalf("applyNetworks = %v, want nil", err)
	}
	if len(api.createdNets) != 0 {
		t.Error("an existing network was recreated; that disconnects every attached service")
	}
	if !strings.Contains(log.String(), "attachable") {
		t.Errorf("log %q does not report the difference", log.String())
	}
}

// Every option the diff compares, one at a time.
//
// The warning above is the only signal there is. Swarm cannot update a network
// in place, so a live network that no longer matches its manifest is never
// corrected and never will be — an operator reading this log line is the whole
// remedy. An option the diff quietly stopped comparing is therefore a manifest
// that says one thing while the swarm does another, permanently and silently,
// which is the failure `docker stack deploy` was faulted for in #1.
//
// Only attachable was ever exercised, so driver and internal could each be
// deleted with the suite green.
func TestNetworkDiffNamesEveryOptionItCompares(t *testing.T) {
	want := network.CreateOptions{Driver: "overlay", Attachable: true, Internal: true}
	match := network.Summary{Driver: "overlay", Attachable: true, Internal: true}

	for _, tc := range []struct {
		option string
		got    network.Summary
	}{
		{"driver", network.Summary{Driver: "macvlan", Attachable: true, Internal: true}},
		{"attachable", network.Summary{Driver: "overlay", Attachable: false, Internal: true}},
		{"internal", network.Summary{Driver: "overlay", Attachable: true, Internal: false}},
	} {
		t.Run(tc.option, func(t *testing.T) {
			diff := networkDiff(want, tc.got)
			if len(diff) != 1 || !strings.HasPrefix(diff[0], tc.option+"(") {
				t.Fatalf("networkDiff = %v, want only %s reported", diff, tc.option)
			}
		})
	}

	// And nothing at all when they agree: a warning on every reconcile of a
	// correct network is a warning nobody reads.
	if diff := networkDiff(want, match); len(diff) != 0 {
		t.Errorf("networkDiff = %v, want nothing for a network that matches", diff)
	}
}

// A network the manifest names but the swarm lacks is created, with the driver
// `docker stack deploy` would have assumed.
func TestMissingNetworkIsCreatedWithTheDefaultDriver(t *testing.T) {
	api := &fakeAPI{}
	st := stack("s")
	st.Networks = []cdcompose.Network{{Name: "s_front", Spec: network.CreateOptions{}}}

	if err := testBackend(t, api, nil).applyNetworks(context.Background(), st); err != nil {
		t.Fatalf("applyNetworks = %v, want nil", err)
	}
	if got := api.createdNets["s_front"].Driver; got != "overlay" {
		t.Errorf("driver = %q, want overlay", got)
	}
}

// What `docker stack rm` removes, and in an order the daemon will accept: a
// network still attached to a running task cannot be removed, and a config or
// secret in use is refused outright.
func TestRemoveStackRemovesServicesFirstThenWhatTheyUsed(t *testing.T) {
	api := &fakeAPI{
		existing: []swarm.Service{{ID: "svc", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "s_web"}}}},
		configs:  []swarm.Config{{ID: "cfg", Spec: swarm.ConfigSpec{Annotations: stackScoped("s_site", "s")}}},
		secrets:  []swarm.Secret{{ID: "sec", Spec: swarm.SecretSpec{Annotations: stackScoped("s_key", "s")}}},
		networks: []network.Summary{stackNetwork("net", "s_front", "s")},
	}

	if err := testBackend(t, api, nil).RemoveStack(t.Context(), "s"); err != nil {
		t.Fatalf("RemoveStack = %v, want nil", err)
	}

	want := []string{"service:svc", "config:cfg", "secret:sec", "network:net"}
	if !reflect.DeepEqual(api.removed, want) {
		t.Errorf("removed %v, want %v", api.removed, want)
	}
	// Volumes are not in that list. A stack's data outliving the stack is the
	// point of a named volume, and `docker stack rm` leaves them too.
	for _, r := range api.removed {
		if strings.HasPrefix(r, "volume:") {
			t.Error("a volume was removed; a stack's data must outlive the stack")
		}
	}
	// Every list was scoped to the namespace label. Without that the engine's
	// own release configs — which carry com.swarmcli.* labels, not a namespace —
	// would be in range, and uninstalling a release would delete its history.
	for _, f := range api.labelFilters {
		if f != convert.LabelNamespace+"=s" {
			t.Errorf("a list was scoped by %q, want the stack namespace label", f)
		}
	}
}

// The engine's release records are Docker configs too. Storing one carries the
// same swarmcli.created label the CE backend writes, so a release recorded here
// and one recorded from the command line look alike to the TUI.
func TestCreateConfigStampsTheCreationTime(t *testing.T) {
	api := &fakeAPI{}
	at := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	b := New(api, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return at }})

	if err := b.CreateConfig(context.Background(), "swarmcli.release.hello.v1", []byte("gz"), map[string]string{"com.swarmcli.type": "release"}); err != nil {
		t.Fatalf("CreateConfig = %v, want nil", err)
	}
	spec := api.createdConfigs[0]
	if spec.Labels["com.swarmcli.type"] != "release" {
		t.Error("the caller's labels were lost")
	}
	if got := spec.Labels["swarmcli.created"]; got != "2026-07-22T10:00:00Z" {
		t.Errorf("swarmcli.created = %q, want the creation time", got)
	}
}

// One list call, not a list plus an inspect per config. This runs several times
// per reconcile against a store that grows by one config per release revision.
//
// A release record's payload has to ride along, or the engine inspects each one
// to decode it and the saving is undone one revision at a time.
func TestListConfigsDoesNotInspectEachOne(t *testing.T) {
	api := &fakeAPI{configs: []swarm.Config{
		{Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "swarmcli.release.web.v1", Labels: map[string]string{charts.LabelType: charts.TypeRelease, "k": "v"}},
			Data:        []byte("payload-a"),
		}},
		{Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "b"}}},
	}}

	got, err := testBackend(t, api, nil).ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs = %v, want nil", err)
	}
	if len(got) != 2 || got[0].Name != "swarmcli.release.web.v1" || got[0].Labels["k"] != "v" {
		t.Errorf("configs = %+v, want name and labels carried through", got)
	}
	if string(got[0].Data) != "payload-a" {
		t.Errorf("Data = %q, want the payload the list response carried", got[0].Data)
	}
	if api.inspects != 0 {
		t.Errorf("inspected %d configs; the list response already holds all of it", api.inspects)
	}
}

// Every config's name and labels, and only a release record's payload.
//
// The listing cannot be filtered: the engine asks this one method both which
// configs hold release history and which names exist at all, and the second is
// asked about a chart's external configs, which carry no swarmcli label — so a
// filter would report an external config that exists as missing and refuse the
// deploy. What the daemon sends cannot be narrowed, but what is held on to can:
// nothing ever decodes a config that is not a release record, and a stack's own
// mounted configs are exactly the payloads that would otherwise be retained for
// the length of a plan, on the manager holding the raft log.
func TestListConfigsKeepsOnlyReleasePayloads(t *testing.T) {
	api := &fakeAPI{configs: []swarm.Config{
		{Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "swarmcli.release.web.v3", Labels: map[string]string{charts.LabelType: charts.TypeRelease}},
			Data:        []byte("a release record"),
		}},
		{Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "web_nginx.conf", Labels: map[string]string{"com.docker.stack.namespace": "web"}},
			Data:        []byte("somebody else's half a megabyte"),
		}},
	}}

	got, err := testBackend(t, api, nil).ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("configs = %+v, want every config listed so the external-config pre-flight can still see them", got)
	}
	if string(got[0].Data) != "a release record" {
		t.Errorf("release record Data = %q, want the payload the list response carried", got[0].Data)
	}
	if got[1].Name != "web_nginx.conf" || got[1].Data != nil {
		t.Errorf("config = %+v, want its name kept and its payload dropped", got[1])
	}
	// Unfiltered, and it has to stay that way: the pre-flight asks about names
	// carrying no swarmcli label at all.
	if len(api.labelFilters) != 1 || api.labelFilters[0] != "" {
		t.Errorf("label filters = %q, want one unfiltered list call", api.labelFilters)
	}
}

// A config carrying a stack's namespace label was created by a stack deploy, and
// a release record never is: the engine writes one through CreateConfig with
// com.swarmcli.* labels and no namespace. So a stack-owned config is not reported
// as a release record whatever its other labels say — isReleaseRecord's rule —
// and it loses the type label as well as its payload, because the engine falls
// back to inspecting a typed config whose payload is missing.
func TestListConfigsDoesNotReportAStackConfigAsARecord(t *testing.T) {
	labels := map[string]string{
		charts.LabelType:             charts.TypeRelease,
		charts.LabelRelease:          "other-app",
		"com.docker.stack.namespace": "web",
	}
	api := &fakeAPI{configs: []swarm.Config{
		{Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "swarmcli.release.web.v1", Labels: map[string]string{charts.LabelType: charts.TypeRelease}},
			Data:        []byte("a release record"),
		}},
		{Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "web_site", Labels: labels},
			Data:        []byte("a stack's config"),
		}},
	}}

	got, err := testBackend(t, api, nil).ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs = %v, want nil", err)
	}
	if len(got) != 2 || string(got[0].Data) != "a release record" || got[0].Labels[charts.LabelType] != charts.TypeRelease {
		t.Fatalf("configs = %+v, want the genuine record reported as one", got)
	}
	stacked := got[1]
	if stacked.Name != "web_site" || stacked.Data != nil {
		t.Errorf("config = %+v, want its name kept and its payload dropped", stacked)
	}
	if _, typed := stacked.Labels[charts.LabelType]; typed {
		t.Errorf("labels = %v, want the type label dropped so the engine does not read it as a record", stacked.Labels)
	}
	if stacked.Labels[charts.LabelRelease] != "other-app" {
		t.Errorf("labels = %v, want the rest carried through", stacked.Labels)
	}
	if labels[charts.LabelType] != charts.TypeRelease {
		t.Error("the swarm's own label map was modified; the listing must copy rather than edit it")
	}
}

func TestStackVolumesAreScopedAndSorted(t *testing.T) {
	scoped := map[string]string{convert.LabelNamespace: "s"}
	api := &fakeAPI{volumes: []volume.Volume{{Name: "zeta", Labels: scoped}, {Name: "alpha", Labels: scoped}, {Name: "other"}}}

	got, err := testBackend(t, api, nil).StackVolumes(context.Background(), "s")
	if err != nil {
		t.Fatalf("StackVolumes = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Errorf("volumes = %v, want them sorted", got)
	}
	if api.labelFilters[0] != convert.LabelNamespace+"=s" {
		t.Errorf("scoped by %q, want the stack namespace label", api.labelFilters[0])
	}
}

// On a manager the daemon appends every CSI cluster volume to a volume listing
// without applying its filter, so what comes back is not only the stack's. What
// StackVolumes returns is what a purge removes, so it keeps only the node-local
// volumes carrying this stack's namespace. A cluster volume is never one of
// them, even labelled with it: a stack deploy neither creates nor labels one, so
// it was provisioned outside the release, and removing it deletes its storage.
func TestStackVolumesKeepOnlyTheStacksOwn(t *testing.T) {
	ns := func(stack string) map[string]string { return map[string]string{convert.LabelNamespace: stack} }
	api := &fakeAPI{
		volumes: []volume.Volume{{Name: "s_data", Labels: map[string]string{convert.LabelNamespace: "s", "tier": "db"}}},
		clusterVolumes: []volume.Volume{
			{Name: "shared-csi"},
			{Name: "other_db", Labels: ns("other")},
			{Name: "s_csi", Labels: ns("s")},
			{Name: "S_csi", Labels: ns("S")},
			{Name: "s-staging_db", Labels: ns("s-staging")},
			{Name: "s_orphan"},
		},
	}

	var logged bytes.Buffer
	b := New(api, Options{Log: slog.New(slog.NewTextHandler(&logged, nil))})
	got, err := b.StackVolumes(context.Background(), "s")
	if err != nil {
		t.Fatalf("StackVolumes = %v, want nil", err)
	}
	if want := []string{"s_data"}; !reflect.DeepEqual(got, want) {
		t.Errorf("volumes = %v, want %v", got, want)
	}
	// A cluster volume is simply not a candidate; it is not a node-local volume
	// left out for sharing a name, and is not reported as one.
	if strings.Contains(logged.String(), "left a volume out") {
		t.Errorf("log %q reports a cluster volume as a node-local one left out", logged.String())
	}
}

// A volume is removed by name, and when the node-local volume of that name is
// gone by the time it is removed the daemon resolves the name in the swarm store
// instead: by a cluster volume's name, without regard to case, or as a prefix of
// its ID. So a node-local name a cluster volume in the same listing also answers
// to is left out of the purge.
func TestStackVolumesLeaveOutANameACLusterVolumeAlsoAnswers(t *testing.T) {
	ns := map[string]string{convert.LabelNamespace: "s"}
	api := &fakeAPI{
		volumes: []volume.Volume{
			{Name: "s_data", Labels: ns},
			{Name: "s_logs", Labels: ns},
			{Name: "abc", Labels: ns},
		},
		clusterVolumes: []volume.Volume{
			{Name: "S_DATA"},
			{Name: "other", ClusterVolume: &volume.ClusterVolume{ID: "abcdef0123456789"}},
		},
	}

	got, err := testBackend(t, api, nil).StackVolumes(context.Background(), "s")
	if err != nil {
		t.Fatalf("StackVolumes = %v, want nil", err)
	}
	if want := []string{"s_logs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("volumes = %v, want %v", got, want)
	}
}

// unfilteredVolumes hands back extra node-local volumes whatever the listing's
// filter, so that the label StackVolumes checks is what keeps them out.
type unfilteredVolumes struct {
	*fakeAPI
	extra []volume.Volume
}

func (u unfilteredVolumes) VolumeList(ctx context.Context, o volume.ListOptions) (volume.ListResponse, error) {
	resp, err := u.fakeAPI.VolumeList(ctx, o)
	for i := range u.extra {
		resp.Volumes = append(resp.Volumes, &u.extra[i])
	}
	return resp, err
}

// The label is checked on what comes back, for node-local volumes too: a listing
// that returned another stack's, or an unlabelled one, does not put it in a purge.
func TestStackVolumesCheckTheLabelOfEveryVolume(t *testing.T) {
	api := unfilteredVolumes{fakeAPI: &fakeAPI{}, extra: []volume.Volume{
		{Name: "s_data", Labels: map[string]string{convert.LabelNamespace: "s"}},
		{Name: "t_data", Labels: map[string]string{convert.LabelNamespace: "t"}},
		{Name: "loose"},
	}}

	got, err := testBackend(t, api, nil).StackVolumes(context.Background(), "s")
	if err != nil {
		t.Fatalf("StackVolumes = %v, want nil", err)
	}
	if want := []string{"s_data"}; !reflect.DeepEqual(got, want) {
		t.Errorf("volumes = %v, want %v", got, want)
	}
}

func TestNetworkScopesAndSecretNames(t *testing.T) {
	api := &fakeAPI{
		networks: []network.Summary{{Name: "traefik-public", Scope: "swarm"}, {Name: "bridge", Scope: "local"}},
		secrets:  []swarm.Secret{{Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "db-password"}}}},
	}
	b := testBackend(t, api, nil)

	scopes, err := b.NetworkScopes(context.Background())
	if err != nil {
		t.Fatalf("NetworkScopes = %v, want nil", err)
	}
	if scopes["traefik-public"] != "swarm" || scopes["bridge"] != "local" {
		t.Errorf("scopes = %v", scopes)
	}

	names, err := b.SecretNames(context.Background())
	if err != nil {
		t.Fatalf("SecretNames = %v, want nil", err)
	}
	if _, ok := names["db-password"]; !ok {
		t.Errorf("names = %v, want the existing secret", names)
	}
}

func TestCreateOverlayNetworkDefaultsTheDriver(t *testing.T) {
	api := &fakeAPI{}

	if err := testBackend(t, api, nil).CreateOverlayNetwork(context.Background(), "shared", "", true); err != nil {
		t.Fatalf("CreateOverlayNetwork = %v, want nil", err)
	}
	got := api.createdNets["shared"]
	if got.Driver != "overlay" || !got.Attachable {
		t.Errorf("options = %+v, want an attachable overlay", got)
	}
}

// Removing a network is how an install rolls back one it auto-created. The
// caller is undoing, and undoing something that did not happen has succeeded.
func TestRemoveOverlayNetworkThatIsAlreadyGoneSucceeds(t *testing.T) {
	api := &fakeAPI{}

	if err := testBackend(t, api, nil).RemoveOverlayNetwork(context.Background(), "absent"); err != nil {
		t.Fatalf("RemoveOverlayNetwork = %v, want nil for a network that is already gone", err)
	}
	if len(api.removed) != 0 {
		t.Errorf("removed %v, want nothing", api.removed)
	}
}

func TestRemoveOverlayNetworkRemovesByID(t *testing.T) {
	api := &fakeAPI{networks: []network.Summary{{ID: "n1", Name: "shared"}}}

	if err := testBackend(t, api, nil).RemoveOverlayNetwork(context.Background(), "shared"); err != nil {
		t.Fatalf("RemoveOverlayNetwork = %v, want nil", err)
	}
	if !reflect.DeepEqual(api.removed, []string{"network:n1"}) {
		t.Errorf("removed %v, want the network's id", api.removed)
	}
}

// The states come from the engine's own mapping, so every rule behind #443,
// #473, #480, #481 and #494 has exactly one copy — this asserts the wiring, not
// the rules, which are tested in swarmcli.
func TestStackServicesReadsThroughTheEngineMapping(t *testing.T) {
	replicas := uint64(1)
	api := &fakeAPI{
		nodes: []swarm.Node{{
			ID:     "n1",
			Status: swarm.NodeStatus{State: swarm.NodeStateReady},
			Spec:   swarm.NodeSpec{Availability: swarm.NodeAvailabilityActive},
		}},
		existing: []swarm.Service{{
			ID: "svc",
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{Name: "s_web", Labels: map[string]string{convert.LabelNamespace: "s"}},
				Mode:        swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
			},
		}},
		tasks: []swarm.Task{{
			ServiceID:    "svc",
			NodeID:       "n1",
			DesiredState: swarm.TaskStateRunning,
			Status:       swarm.TaskStatus{State: swarm.TaskStateRunning},
		}},
	}

	got := testBackend(t, api, nil).StackServices(t.Context(), "s")
	if len(got) != 1 {
		t.Fatalf("got %d states, want 1", len(got))
	}
	if got[0].Name != "s_web" || got[0].Running != 1 || got[0].Desired != 1 {
		t.Errorf("state = %+v, want the running service counted", got[0])
	}
}

// The backend fetches every read, so there is no cache to invalidate — which is
// what lets one process serve several swarms without them evicting each other.
func TestRefreshSnapshotIsANoOp(t *testing.T) {
	if err := testBackend(t, &fakeAPI{}, nil).RefreshSnapshot(t.Context()); err != nil {
		t.Errorf("RefreshSnapshot = %v, want nil", err)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	api := &fakeAPI{configs: []swarm.Config{{
		ID:   "c",
		Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "swarmcli.release.hello.v1"}, Data: []byte("payload")},
	}}}
	b := testBackend(t, api, nil)

	data, err := b.InspectConfig(context.Background(), "swarmcli.release.hello.v1")
	if err != nil {
		t.Fatalf("InspectConfig = %v, want nil", err)
	}
	if string(data) != "payload" {
		t.Errorf("data = %q, want the stored payload", data)
	}

	if err := b.DeleteConfig(context.Background(), "swarmcli.release.hello.v1"); err != nil {
		t.Fatalf("DeleteConfig = %v, want nil", err)
	}
	if !reflect.DeepEqual(api.removed, []string{"config:swarmcli.release.hello.v1"}) {
		t.Errorf("removed %v, want the config", api.removed)
	}
}

func TestRemoveVolume(t *testing.T) {
	api := &fakeAPI{volumes: []volume.Volume{{Name: "s_data"}}}
	if err := testBackend(t, api, nil).RemoveVolume(context.Background(), "s_data"); err != nil {
		t.Fatalf("RemoveVolume = %v, want nil", err)
	}
	if !reflect.DeepEqual(api.removed, []string{"volume:s_data"}) {
		t.Errorf("removed %v, want the volume", api.removed)
	}
}

// A daemon that will not answer must not be mistaken for a swarm with nothing
// on it: reporting an empty list would make a plan think every resource is
// missing, and a remove think there is nothing to remove.
func TestDaemonFailuresSurface(t *testing.T) {
	boom := errors.New("daemon unreachable")
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func(*Backend) error
	}{
		{"DeployStack", func(b *Backend) error {
			return b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: "services:\n  web:\n    image: x\n", Resolve: ResolveNever})
		}},
		{"RemoveStack", func(b *Backend) error { return b.RemoveStack(t.Context(), "s") }},
		{"ListConfigs", func(b *Backend) error { _, err := b.ListConfigs(ctx); return err }},
		{"InspectConfig", func(b *Backend) error { _, err := b.InspectConfig(ctx, "x"); return err }},
		{"DeleteConfig", func(b *Backend) error { return b.DeleteConfig(ctx, "x") }},
		{"StackVolumes", func(b *Backend) error { _, err := b.StackVolumes(ctx, "s"); return err }},
		{"RemoveVolume", func(b *Backend) error { return b.RemoveVolume(ctx, "v") }},
		{"NetworkScopes", func(b *Backend) error { _, err := b.NetworkScopes(ctx); return err }},
		{"CreateOverlayNetwork", func(b *Backend) error { return b.CreateOverlayNetwork(ctx, "n", "", true) }},
		{"RemoveOverlayNetwork", func(b *Backend) error { return b.RemoveOverlayNetwork(ctx, "n") }},
		{"SecretNames", func(b *Backend) error { _, err := b.SecretNames(ctx); return err }},
		{"CreateConfig", func(b *Backend) error { return b.CreateConfig(ctx, "n", nil, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(testBackend(t, &errAPI{err: boom}, nil))
			if err == nil || !strings.Contains(err.Error(), boom.Error()) {
				t.Errorf("err = %v, want the daemon's failure surfaced", err)
			}
		})
	}
}

// A snapshot that cannot be read reports no services rather than failing: the
// caller is polling for convergence, so an unavailable daemon is "not yet".
func TestStackServicesReportsNothingWhenTheSnapshotFails(t *testing.T) {
	if got := testBackend(t, &errAPI{err: errors.New("daemon unreachable")}, nil).StackServices(t.Context(), "s"); got != nil {
		t.Errorf("StackServices = %v, want nil", got)
	}
}

// The same read for the caller that does not poll. Without the error it cannot
// tell an empty stack from a daemon that did not answer, and the health rollup
// reads the first as "deployed, but no services are present on the swarm" — the
// loudest thing it can say about a stack that is fine (#107).
func TestReadStackServicesKeepsTheSnapshotFailure(t *testing.T) {
	states, err := testBackend(t, &errAPI{err: errors.New("daemon unreachable")}, nil).ReadStackServices(t.Context(), "s")
	if err == nil {
		t.Fatal("ReadStackServices err = nil, want the snapshot failure")
	}
	if !strings.Contains(err.Error(), "daemon unreachable") {
		t.Errorf("err = %v, want the daemon's own failure carried", err)
	}
	if states != nil {
		t.Errorf("states = %v, want none", states)
	}
}

// And a stack the daemon answered about, with nothing under it, is not an error.
// That is the case Missing exists for, and it has to survive the fix.
func TestReadStackServicesReportsAnEmptyStackWithoutAnError(t *testing.T) {
	states, err := testBackend(t, &fakeAPI{}, nil).ReadStackServices(t.Context(), "s")
	if err != nil {
		t.Fatalf("ReadStackServices err = %v, want nil", err)
	}
	if len(states) != 0 {
		t.Errorf("states = %v, want none", states)
	}
}

// A network create that fails aborts the deploy: the services about to be
// created would reference a network that is not there.
func TestNetworkCreateFailureAbortsTheDeploy(t *testing.T) {
	api := &createNetErrAPI{}
	st := stack("s")
	st.Networks = []cdcompose.Network{{Name: "s_front"}}

	err := testBackend(t, api, nil).applyNetworks(context.Background(), st)
	if err == nil || !strings.Contains(err.Error(), "s_front") {
		t.Fatalf("err = %v, want the network named", err)
	}
}

type createNetErrAPI struct{ client.APIClient }

func (createNetErrAPI) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	return nil, nil
}

func (createNetErrAPI) NetworkCreate(context.Context, string, network.CreateOptions) (network.CreateResponse, error) {
	return network.CreateResponse{}, errors.New("pool overlaps")
}

// errAPI fails every call it is asked for, so one table can assert that no
// method quietly swallows a daemon failure.
type errAPI struct {
	client.APIClient
	err error
}

func (e *errAPI) ClientVersion() string { return "1.51" }

// The read every deploy and every removal now makes about this controller
// itself. A daemon that will not answer it is not the news that this controller
// has no stack of its own, so it surfaces here like every other failure.
func (e *errAPI) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{}, e.err
}

func (e *errAPI) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return nil, e.err
}

func (e *errAPI) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	return nil, e.err
}

func (e *errAPI) NetworkCreate(context.Context, string, network.CreateOptions) (network.CreateResponse, error) {
	return network.CreateResponse{}, e.err
}

func (e *errAPI) ConfigList(context.Context, swarm.ConfigListOptions) ([]swarm.Config, error) {
	return nil, e.err
}

func (e *errAPI) ConfigInspectWithRaw(context.Context, string) (swarm.Config, []byte, error) {
	return swarm.Config{}, nil, e.err
}

func (e *errAPI) ConfigCreate(context.Context, swarm.ConfigSpec) (swarm.ConfigCreateResponse, error) {
	return swarm.ConfigCreateResponse{}, e.err
}

func (e *errAPI) ConfigRemove(context.Context, string) error { return e.err }

func (e *errAPI) SecretList(context.Context, swarm.SecretListOptions) ([]swarm.Secret, error) {
	return nil, e.err
}

func (e *errAPI) VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error) {
	return volume.ListResponse{}, e.err
}

func (e *errAPI) VolumeRemove(context.Context, string, bool) error { return e.err }

func (e *errAPI) VolumeInspect(context.Context, string) (volume.Volume, error) {
	return volume.Volume{}, e.err
}

func (e *errAPI) NodeList(context.Context, swarm.NodeListOptions) ([]swarm.Node, error) {
	return nil, e.err
}

// The engine stores each release revision as a Docker config, and those must
// survive an uninstall — a release's history stays readable after it is gone.
// They survive today because they carry com.swarmcli.* labels and no stack
// namespace, so the filter cannot see them. This asserts the second line of
// defence: even a release config that somehow did carry the namespace label is
// left alone, because deleting one turns uninstall into "and lose the history".
func TestRemoveStackNeverDeletesReleaseRecords(t *testing.T) {
	api := &fakeAPI{configs: []swarm.Config{
		{ID: "app", Spec: swarm.ConfigSpec{Annotations: stackScoped("s_site", "s")}},
		{ID: "rel", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
			Name: "swarmcli.release.s.v3",
			Labels: map[string]string{
				charts.LabelType: charts.TypeRelease,
				// Deliberately mislabelled, which is the case the namespace
				// filter alone would not survive.
				convert.LabelNamespace: "s",
			},
		}}},
	}}

	if err := testBackend(t, api, nil).RemoveStack(t.Context(), "s"); err != nil {
		t.Fatalf("RemoveStack = %v, want nil", err)
	}
	for _, r := range api.removed {
		if r == "config:rel" {
			t.Fatal("a release history record was deleted by an uninstall")
		}
	}
	if !slices.Contains(api.removed, "config:app") {
		t.Errorf("removed %v, want the stack's own config gone", api.removed)
	}
}

// A destructive call with nothing to scope to must not proceed and find out
// what the daemon makes of an empty filter value.
func TestRemoveStackRefusesAnEmptyName(t *testing.T) {
	api := &fakeAPI{}
	if err := testBackend(t, api, nil).RemoveStack(t.Context(), ""); err == nil {
		t.Fatal("RemoveStack = nil, want an unnamed stack refused")
	}
	if len(api.removed) != 0 {
		t.Errorf("removed %v before refusing", api.removed)
	}
}

// Swarm garbage-collects an overlay network once the last task attached to it
// goes, which happens while the services removed a moment earlier are still
// shutting down. So the network RemoveStack listed is routinely gone before it
// is asked to remove it, and that is the state it wanted — not a failure.
//
// It matters beyond tidiness: prune retries a pass that failed part-way by
// re-listing and re-deleting, so a "not found" that failed the call would make
// every retry fail forever, and the release would never be recorded as gone.
func TestRemoveStackTreatsAnAlreadyDeletedResourceAsDone(t *testing.T) {
	for _, missing := range []string{"service:svc", "config:cfg", "secret:sec", "network:net"} {
		t.Run(missing, func(t *testing.T) {
			api := &fakeAPI{
				existing:  []swarm.Service{{ID: "svc", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "s_web"}}}},
				configs:   []swarm.Config{{ID: "cfg", Spec: swarm.ConfigSpec{Annotations: stackScoped("s_site", "s")}}},
				secrets:   []swarm.Secret{{ID: "sec", Spec: swarm.SecretSpec{Annotations: stackScoped("s_key", "s")}}},
				networks:  []network.Summary{stackNetwork("net", "s_front", "s")},
				removeErr: map[string]error{missing: errdefs.ErrNotFound},
			}

			if err := testBackend(t, api, nil).RemoveStack(t.Context(), "s"); err != nil {
				t.Fatalf("RemoveStack = %v, want nil when %s had already gone", err, missing)
			}
			// Everything else is still attempted: one resource vanishing must
			// not abandon the rest of the stack.
			want := []string{"service:svc", "config:cfg", "secret:sec", "network:net"}
			if !reflect.DeepEqual(api.removed, want) {
				t.Errorf("removed %v, want %v", api.removed, want)
			}
		})
	}
}

// The case the error alone cannot answer. A swarm-scoped network removal is
// proxied through swarmkit, and its reply for something that had already gone
// does not reliably arrive as a not-found the client recognises — so the only
// dependable question is whether anything is still there.
//
// This is what CI hit: prune deleted the departed stack and then reported a
// failure, so the application was recorded as an orphan the swarm no longer had.
func TestRemoveStackAcceptsAFailureThatLeftNothingBehind(t *testing.T) {
	api := &fakeAPI{
		existing:   []swarm.Service{{ID: "svc", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "s_web"}}}},
		networks:   []network.Summary{stackNetwork("net", "s_front", "s")},
		removeErr:  map[string]error{"network:net": errors.New("network net not found")},
		removeGone: map[string]bool{"network:net": true},
	}

	if err := testBackend(t, api, nil).RemoveStack(t.Context(), "s"); err != nil {
		t.Fatalf("RemoveStack = %v, want nil — the stack is gone, whatever the daemon said", err)
	}
}

// Release-history configs stay behind on purpose, so the re-check must not read
// them as "the stack is still here" and turn every removal into a failure.
//
// The namespace label on the record is what makes this test the test it is
// named for. Without it the re-check's own ConfigList filters the record out
// before the skip is reached, so the loop never runs and the case is asserted
// only by coincidence — the same mislabelled record
// TestRemoveStackNeverDeletesReleaseRecords uses, and the same reason: the skip
// is the second line of defence, and only a record the filter lets through
// exercises it.
func TestRemoveStackIgnoresReleaseRecordsWhenRechecking(t *testing.T) {
	api := &fakeAPI{
		networks: []network.Summary{stackNetwork("net", "s_front", "s")},
		configs: []swarm.Config{{ID: "rel", Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{
				Name: "swarmcli.release.s.v1",
				Labels: map[string]string{
					charts.LabelType:       charts.TypeRelease,
					convert.LabelNamespace: "s",
				},
			},
		}}},
		removeErr:  map[string]error{"network:net": errors.New("network net not found")},
		removeGone: map[string]bool{"network:net": true},
	}

	if err := testBackend(t, api, nil).RemoveStack(t.Context(), "s"); err != nil {
		t.Fatalf("RemoveStack = %v, want nil; the release record is not the stack", err)
	}
	// And it is still there to be read, which is the whole point of leaving it.
	if slices.Contains(api.removed, "config:rel") {
		t.Error("the release record was deleted by the removal it was meant to survive")
	}
}

// The other side of that re-check, one kind at a time.
//
// stackRemains asks about services, then configs, then secrets, then networks,
// and returns at the first that answers "still here" — so a check that was
// deleted is only caught by a case in which its kind is the *only* thing left.
// Each case below refuses one removal and leaves nothing else behind.
//
// What it decides is not cosmetic. A "false" here is RemoveStack reporting
// success, which lets prune.Release call engine.Uninstall and delete the
// owner-stamped release records — the evidence that these resources were ever
// ours. Doing that with the stack still up strands them permanently, invisible
// to every future prune.
//
// The network case is TestRemoveStackStillFailsOnARealError above.
func TestRemoveStackReportsWhicheverKindIsStillThere(t *testing.T) {
	for _, tc := range []struct {
		kind string
		api  *fakeAPI
	}{
		{"service", &fakeAPI{
			existing:  []swarm.Service{{ID: "svc", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "s_web"}}}},
			removeErr: map[string]error{"service:svc": errors.New("service is being updated")},
		}},
		{"config", &fakeAPI{
			configs:   []swarm.Config{{ID: "cfg", Spec: swarm.ConfigSpec{Annotations: stackScoped("s_site", "s")}}},
			removeErr: map[string]error{"config:cfg": errors.New("config is in use")},
		}},
		{"secret", &fakeAPI{
			secrets:   []swarm.Secret{{ID: "sec", Spec: swarm.SecretSpec{Annotations: stackScoped("s_key", "s")}}},
			removeErr: map[string]error{"secret:sec": errors.New("secret is in use")},
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			err := testBackend(t, tc.api, nil).RemoveStack(t.Context(), "s")
			if err == nil {
				t.Fatalf("RemoveStack = nil, but the %s is still on the swarm; reporting success here deletes the release history that proves it is ours", tc.kind)
			}
			if !strings.Contains(err.Error(), "in use") && !strings.Contains(err.Error(), "being updated") {
				t.Errorf("RemoveStack = %v, want the daemon's refusal surfaced", err)
			}
		})
	}
}

// A real failure still fails. The tolerance above is for "it is already gone",
// not for "the daemon refused".
func TestRemoveStackStillFailsOnARealError(t *testing.T) {
	api := &fakeAPI{
		networks:  []network.Summary{stackNetwork("net", "s_front", "s")},
		removeErr: map[string]error{"network:net": errors.New("has active endpoints")},
	}

	err := testBackend(t, api, nil).RemoveStack(t.Context(), "s")
	if err == nil || !strings.Contains(err.Error(), "has active endpoints") {
		t.Fatalf("RemoveStack = %v, want the daemon's refusal surfaced", err)
	}
}

// stackScoped is a config or secret as a real deploy by this controller leaves
// it: carrying the namespace label, which is the whole of "this belongs to that
// stack" and the only thing RemoveStack has to find it by, and the creation
// marker that says this controller made it rather than adopted it.
//
// adopted below is the same resource without that second half, which is what an
// operator's own secret looks like once a chart has declared its name.
func stackScoped(name, stack string) swarm.Annotations {
	a := adopted(name, stack)
	a.Labels[createdLabel] = "2026-01-01T00:00:00Z"
	return a
}

// adopted is a resource carrying the stack's namespace label and nothing that
// says who created it — either because somebody else did, or because a build of
// this controller from before the marker existed did.
func adopted(name, stack string) swarm.Annotations {
	return swarm.Annotations{
		Name:   name,
		Labels: map[string]string{convert.LabelNamespace: stack},
	}
}

// stackNetwork is the same thing for a network, which carries its labels on the
// summary rather than in annotations — and no creation marker, because
// applyNetworks never adopts and so LiveNetworks never asks for one.
func stackNetwork(id, name, stack string) network.Summary {
	return network.Summary{
		ID:     id,
		Name:   name,
		Labels: map[string]string{convert.LabelNamespace: stack},
	}
}

// ---------------------------------------- the controller's own state (#63)

// controllerService is this controller as Swarm holds it: deployed as the stack
// the README says to deploy it as, mounting its own bootstrap config and its
// admin token under the names a reference resolves by, and carrying the two
// mounts stack.yml gives it.
//
// The target rename on the secret is the point. MountedSecretNames would derive
// "token" from /run/secrets and never match the "swarmcli-cd-token" a tenant
// stack would actually name, so the filesystem-derived guard has a hole exactly
// here — and reading the service spec closes it.
//
// The namespace label comes off the same read and is the whole of #102: `docker
// stack deploy -c stack.yml swarmcli-cd` puts it there, so it is the name no
// release may claim.
//
// The volume is #103's half of the same read, and unlike the secret it is not
// derivable from anywhere else at all: nothing on the filesystem says which
// swarm volume /var/lib/swarmcli-cd is. The bind beside it is there so that the
// self-read is exercised on a spec that has both kinds — a guard that collected
// every mount would refuse a tenant naming the host path /var/run/docker.sock,
// which is a different question and a deliberately open one.
func controllerService() swarm.ServiceSpec {
	return swarm.ServiceSpec{Annotations: swarm.Annotations{
		Name:   "swarmcli-cd_controller",
		Labels: map[string]string{convert.LabelNamespace: "swarmcli-cd"},
	}, TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
		Configs: []*swarm.ConfigReference{{
			ConfigName: "swarmcli-cd-applications",
			File:       &swarm.ConfigReferenceFileTarget{Name: "/etc/swarmcli-cd/applications.yaml"},
		}},
		Secrets: []*swarm.SecretReference{{
			SecretName: "swarmcli-cd-token",
			File:       &swarm.SecretReferenceFileTarget{Name: "token"},
		}},
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true},
			{Type: mount.TypeVolume, Source: "swarmcli-cd_swarmcli-cd-data", Target: "/var/lib/swarmcli-cd"},
		},
	}}}
}

// asController makes the fake answer as though this process is that service's
// task, which is what SelfMounts reads.
// allowing is a backend scoped to one application's permissions, the way the
// reconciler scopes it — through the exported upgrade rather than by setting the
// field, so what these tests exercise is what reconcile calls.
func allowing(t *testing.T, api client.APIClient, allow application.Allow) charts.Backend {
	t.Helper()
	return testBackend(t, api, nil).WithAllowedReferences(allow)
}

func asController(api *fakeAPI) *fakeAPI {
	api.selfServiceID = "controller-svc"
	api.selfSpec = controllerService()
	return api
}

// ---------------------------------------- the controller's own release (#235)

// noDeferral is a sink that throws the write away. Every test here is refused
// before a service is written, so nothing reaches it; it exists because
// WithSelfRelease refuses a nil one, which is the next test.
func noDeferral(func(context.Context) error) {}

const trivialStack = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
`

// `self: true` says "this application deploys me", and the only thing that makes
// that true is the release name — a release name is the stack namespace. Named
// anything else, a self release deploys a *second* controller beside this one,
// each reconciling the same app set on the same swarm. That is swarmcli-cd#234
// as it was actually reported, and it got as far as it did because the name
// looked like a detail. The refusal names the stack this controller runs as,
// because that string is the fix.
func TestASelfReleaseMustBeNamedAfterTheControllersOwnStack(t *testing.T) {
	api := asController(&fakeAPI{})
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)

	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "cd", Manifest: trivialStack, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a self release named other than the controller's stack refused")
	}
	for _, want := range []string{"'cd'", "'swarmcli-cd'"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if len(api.created)+len(api.updated) > 0 {
		t.Errorf("wrote %d services; a refused self release must reach nothing", len(api.created)+len(api.updated))
	}
}

// A process that is not a swarm service has no stack of its own to upgrade, so
// the answer to "is this release me" is no — the opposite of what the guards
// built on the same read do with an empty answer, and deliberately. They ask
// whether there is anything of ours to protect and go quiet when there is not;
// this asks whether this release *is* us. An application whose destination
// resolves to another swarm arrives here identically, because that swarm's
// daemon has never heard of this container.
func TestASelfReleaseIsRefusedWhereTheControllerIsNotAService(t *testing.T) {
	api := &fakeAPI{}
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)

	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: trivialStack, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a self release refused where this controller is not a swarm service")
	}
	if !strings.Contains(err.Error(), "not deployed as a stack on this swarm") {
		t.Errorf("error %q does not say why", err)
	}
}

// The exemptions a self release will get are only defensible because the write
// replacing this controller is issued last, after the pass has been recorded. A
// copy with nowhere to hand that write back would deploy everything except the
// controller, report a successful sync, and have upgraded nothing — so a nil
// sink does not produce a self backend at all.
func TestWithSelfReleaseNeedsSomewhereToHandTheWrite(t *testing.T) {
	b := testBackend(t, asController(&fakeAPI{}), nil)
	if got := b.WithSelfRelease(nil); got != charts.Backend(b) {
		t.Error("WithSelfRelease(nil) returned a different backend, want the original unchanged")
	}
	if b.selfRelease {
		t.Error("selfRelease = true after a nil sink, want the backend left alone")
	}
}

// A backend is built once per swarm and reused by every application, so the
// answer to "is this release the controller's own" has to live on the copy. The
// shared one still deploys an ordinary release under any name it likes.
func TestWithSelfReleaseDoesNotMarkTheSharedBackend(t *testing.T) {
	api := asController(&fakeAPI{})
	shared := testBackend(t, api, nil)

	self, ok := shared.WithSelfRelease(noDeferral).(*Backend)
	if !ok {
		t.Fatal("WithSelfRelease did not return a *Backend")
	}
	if !self.selfRelease || self.holdSelf == nil {
		t.Errorf("the copy has selfRelease=%v holdSelf==nil:%v, want both set", self.selfRelease, self.holdSelf == nil)
	}
	if shared.selfRelease || shared.holdSelf != nil {
		t.Fatal("WithSelfRelease marked the shared backend, so the next application would deploy as the controller's own")
	}
	// And the shared one is unaffected in the only way that matters: a release
	// named anything at all still deploys, with no self comparison made.
	if err := shared.DeployStack(t.Context(), charts.DeployRequest{Name: "cd", Manifest: trivialStack, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack through the shared backend = %v, want nil", err)
	}
}

// ---------------------------------------- what the controller's own release may reach (#235)

// selfStack is the shape the swarmcli-cd chart renders: the controller's own
// admin token and app-set config referenced as external, its data volume
// declared, and the socket bound. Deployed under the namespace this controller
// runs as, which is what makes every one of those the release's own.
const selfStack = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    secrets: [swarmcli-cd-token]
    configs:
      - source: swarmcli-cd-applications
        target: /etc/swarmcli-cd/applications.yaml
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - swarmcli-cd-data:/var/lib/swarmcli-cd
secrets:
  swarmcli-cd-token:
    external: true
configs:
  swarmcli-cd-applications:
    external: true
volumes:
  swarmcli-cd-data: {}
`

// bootstrappedStack is what the swarmcli-cd chart renders for a controller
// deployed the way stack.yml deploys it — every part of which bootstrappedController
// is already running with, so it drops nothing and is the baseline the losses below
// are written against.
const bootstrappedStack = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    command: ["controller", "--config", "/etc/swarmcli-cd/applications.yaml"]
    environment:
      SWARMCLI_CD_ADMIN_TOKEN_FILE: /run/secrets/token
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - swarmcli-cd-data:/var/lib/swarmcli-cd
    secrets:
      - source: swarmcli-cd-token
        target: token
    configs:
      - source: swarmcli-cd-applications
        target: /etc/swarmcli-cd/applications.yaml
secrets:
  swarmcli-cd-token:
    external: true
configs:
  swarmcli-cd-applications:
    external: true
volumes:
  swarmcli-cd-data: {}
`

// selfAPI is a swarm holding the controller's own credentials, as an operator's
// `docker secret create` left them: cluster-global names with no stack of their
// own, which is exactly why a reference to one carries no namespace.
func selfAPI() *fakeAPI {
	return asController(&fakeAPI{
		secrets: []swarm.Secret{{ID: "tok", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "swarmcli-cd-token"}}}},
		configs: []swarm.Config{{ID: "apps", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "swarmcli-cd-applications"}}}},
	})
}

// The whole of #234, from the other side. This manifest is refused for every
// application on the swarm — it mounts the admin token, which is root-equivalent
// — and for the one release that *is* this controller it is not reaching outside
// itself at all.
func TestASelfReleaseMountsTheControllersOwnSecretsAndConfigs(t *testing.T) {
	api := selfAPI()
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)

	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: selfStack, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want the controller's own release deployed", err)
	}
	if len(api.created) != 1 || api.created[0].Name != "swarmcli-cd_controller" {
		t.Errorf("created %d services, want just the controller's own", len(api.created))
	}
}

// The same manifest, one application over, and permitted the socket outright so
// that the bind guard is not what refuses it. Nothing about the exemption is
// carried by the chart, the secret names or the swarm — only by the backend copy
// the reconciler marked. So an operator generous enough to hand an application
// the docker socket still has not handed it the admin token, which is the line
// #46 drew and the one this must not move.
func TestTheSameManifestIsRefusedForEveryOtherApplication(t *testing.T) {
	api := selfAPI()
	err := allowing(t, api, application.Allow{HostPaths: []string{"/var/run/docker.sock"}}).DeployStack(t.Context(),
		charts.DeployRequest{Name: "cd", Manifest: selfStack, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want an ordinary release still refused the controller's own secret")
	}
	if !strings.Contains(err.Error(), "swarmcli-cd-token") || !strings.Contains(err.Error(), whatControllerSecret) {
		t.Errorf("error %q does not refuse the controller's secret", err)
	}
	if len(api.created)+len(api.updated) > 0 {
		t.Error("wrote services for a refused release")
	}
}

// A release record is the one thing on that list no release owns, this
// controller's own included: each holds the rendered manifest of a release on
// this swarm. Checked before anything is recognised as ours, so the recognition
// cannot reach it.
func TestASelfReleaseStillCannotMountAReleaseRecord(t *testing.T) {
	api := installed(selfAPI(), "other")
	const manifest = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    configs: [history]
configs:
  history:
    external: true
    name: swarmcli.release.other.v1
`
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)
	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: manifest, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a release record refused to the controller's own release too")
	}
	if !strings.Contains(err.Error(), whatReleaseRecord) {
		t.Errorf("error %q does not name what was refused", err)
	}
}

// The asymmetry with the referenced half, and it is deliberate. Mounting a
// secret the controller already has changes nothing about it; declaring one runs
// it through applySecrets, which relabels the existing secret into the stack's
// namespace — an adoption of a credential nobody needed to adopt. `external:
// true` is how the swarmcli-cd chart references its own, so nothing legitimate
// is refused here.
func TestASelfReleaseStillCannotDeclareTheControllersSecretAsItsOwn(t *testing.T) {
	api := selfAPI()
	const manifest = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    secrets: [tok]
secrets:
  tok:
    name: swarmcli-cd-token
    driver: vault
`
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)
	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: manifest, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want #86's declaration refused for the self release too")
	}
	if !strings.Contains(err.Error(), "declares secret") {
		t.Errorf("error %q is not the declaration refusal", err)
	}
}

// The set the recognition reads is the controller's own *service spec*, not the
// listing of /run/secrets — which names mount targets. This controller's admin
// token arrives at /run/secrets/token, so "token" is in the filesystem-derived
// set and is not the name any reference resolves by; a secret that really is
// called "token" belongs to somebody else, and the self release may not have it.
func TestASelfReleaseDoesNotGetASecretItOnlyKnowsByMountTarget(t *testing.T) {
	api := selfAPI()
	api.secrets = append(api.secrets, swarm.Secret{ID: "t", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "token"}}})
	const manifest = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    secrets: [t]
secrets:
  t:
    external: true
    name: token
`
	b := testBackend(t, api, nil).
		WithForbiddenSecrets(map[string]struct{}{"token": {}}).(*Backend).
		WithSelfRelease(noDeferral)
	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: manifest, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a mount target's name refused even to the self release")
	}
	if !strings.Contains(err.Error(), whatControllerSecret) {
		t.Errorf("error %q does not refuse it as the controller's own", err)
	}
}

// A volume the controller mounts that is not scoped under its stack is the case
// the recognition is actually needed for: appset.mode dir mounts an external
// volume something else writes, so its name carries no namespace at all.
func TestASelfReleaseMountsTheControllersUnscopedVolume(t *testing.T) {
	api := selfAPI()
	spec := controllerService()
	spec.TaskTemplate.ContainerSpec.Mounts = append(spec.TaskTemplate.ContainerSpec.Mounts,
		mount.Mount{Type: mount.TypeVolume, Source: "swarmcli-cd-appset", Target: "/etc/swarmcli-cd/appset"})
	api.selfSpec = spec
	// The socket with it: a manifest that dropped it would be refused by
	// rejectSelfLoss before this got as far as the volume.
	const manifest = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - swarmcli-cd-appset:/etc/swarmcli-cd/appset:ro
volumes:
  swarmcli-cd-appset:
    external: true
`
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)
	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: manifest, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want the controller's own app-set volume mounted", err)
	}
}

// The socket is the fourth guard, and the only one that is not a name
// comparison: compose.checkBindSources refuses a bind the application's
// allow.hostPaths does not cover, during conversion, before any of the others is
// reached. The chart that deploys this controller binds the socket because that
// is what the controller talks to the swarm through, so the self release has to
// be able to re-declare the mount it is already running with. Covered above by
// TestASelfReleaseMountsTheControllersOwnSecretsAndConfigs, whose manifest binds
// it; this is the other half of the bound.
func TestASelfReleaseMayNotBindAPathTheControllerDoesNot(t *testing.T) {
	const reachesFurther = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    volumes:
      - /var/lib/docker:/host/docker
`
	api := selfAPI()
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)
	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: reachesFurther, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a bind the controller does not hold refused to the self release too")
	}
	if !strings.Contains(err.Error(), "/var/lib/docker") || !strings.Contains(err.Error(), "allow.hostPaths") {
		t.Errorf("error %q does not refuse it as an unpermitted bind", err)
	}
}

// The controller's stack was put there by `docker stack deploy` and has no
// release record, exactly like any other foreign stack — and the remedy the
// refusal offers, remove it and let the controller install it, leaves nothing
// running to install it again. So the first self sync adopts.
func TestASelfReleaseAdoptsTheControllersRunningServices(t *testing.T) {
	api := selfAPI()
	api.existing = []swarm.Service{{ID: "svc", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{
		Name:   "swarmcli-cd_controller",
		Labels: map[string]string{convert.LabelNamespace: "swarmcli-cd"},
	}}}}

	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)
	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: selfStack, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want the controller's own services adopted", err)
	}
	if len(api.updated) != 1 || api.updated[0].spec.Name != "swarmcli-cd_controller" {
		t.Errorf("updated %d services, want the running controller written over", len(api.updated))
	}
}

// Adoption is the self release's alone. Anything else finding services under a
// namespace it has no record for is still writing over somebody else's stack.
func TestAnOrdinaryReleaseStillCannotAdoptAStackItDidNotInstall(t *testing.T) {
	api := &fakeAPI{existing: []swarm.Service{{ID: "svc", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{
		Name:   "legacy_web",
		Labels: map[string]string{convert.LabelNamespace: "legacy"},
	}}}}}
	err := testBackend(t, api, nil).DeployStack(t.Context(),
		charts.DeployRequest{Name: "legacy", Manifest: trivialStack, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a foreign namespace still refused")
	}
	if len(api.updated) > 0 {
		t.Errorf("updated %d services, want nothing written", len(api.updated))
	}
}

// Deploying onto the controller's own services is what upgrading it is;
// deleting them is not, and no declaration in the app set makes it so. Both
// verbs stay refused — the removal, and the volume listing a purge deletes from,
// which is the one part no reconcile can put back.
func TestTheSelfReleaseStillCannotRemoveOrPurgeTheController(t *testing.T) {
	api := selfAPI()
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)

	if err := b.RemoveStack(t.Context(), "swarmcli-cd"); err == nil {
		t.Error("RemoveStack = nil, want the controller's own stack still refused")
	}
	self, ok := b.(*Backend)
	if !ok {
		t.Fatal("WithSelfRelease did not return a *Backend")
	}
	if _, err := self.StackVolumes(t.Context(), "swarmcli-cd"); err == nil {
		t.Error("StackVolumes = nil, want the controller's own volumes still refused")
	}
}

// ---------------------------------------- written last, and what it may not drop (#235)

// bootstrappedController is the controller as stack.yml actually deploys it:
// the socket, its admin token behind a renamed mount target, and the app set as
// a Docker config named on the command line. controllerService alone carries
// neither the environment nor the command, so the losses those two describe are
// invisible against it.
func bootstrappedController() swarm.ServiceSpec {
	spec := controllerService()
	cs := spec.TaskTemplate.ContainerSpec
	cs.Args = []string{"controller", "--config", "/etc/swarmcli-cd/applications.yaml"}
	cs.Env = []string{"SWARMCLI_CD_ADMIN_TOKEN_FILE=/run/secrets/token"}
	return spec
}

// runningController is that spec as a service already on the swarm, under the id
// this process's own container reports — which is what makes it *this*
// controller rather than one that happens to share a name.
func runningController(api *fakeAPI) *fakeAPI {
	api.selfSpec = bootstrappedController()
	api.existing = append(api.existing, swarm.Service{ID: api.selfServiceID, Spec: api.selfSpec})
	return api
}

// The whole reason a self release can be deployed at all. Swarm's update order is
// stop-first, so writing this service kills the task doing the writing — and the
// chart engine records the revision *after* the deploy returns. Made in place,
// the write would leave a stack with no record of the revision that produced it,
// which rejectForeignNamespace then refuses for ever.
func TestTheControllersOwnServiceIsWrittenLastAndNotByTheDeploy(t *testing.T) {
	api := runningController(selfAPI())
	var deferred func(context.Context) error
	b := testBackend(t, api, nil).WithSelfRelease(func(apply func(context.Context) error) { deferred = apply })

	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: bootstrappedStack, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}
	if len(api.updated) != 0 {
		t.Fatalf("the deploy wrote %d services; this controller's own must not be one of them", len(api.updated))
	}
	if deferred == nil {
		t.Fatal("no write was handed back, so nothing would ever replace this controller")
	}

	if err := deferred(t.Context()); err != nil {
		t.Fatalf("the deferred write = %v, want nil", err)
	}
	if len(api.updated) != 1 || api.updated[0].spec.Name != "swarmcli-cd_controller" {
		t.Errorf("the deferred write updated %d services, want this controller's own", len(api.updated))
	}
}

// Only this controller's own service is held back. Everything else the stack
// declares — the git-sync sidecar in the SSH app-set mode, anything an operator
// added — is written by the deploy, so the pass that hands the last write back
// has already done all of its other work.
func TestOnlyTheControllersOwnServiceIsDeferred(t *testing.T) {
	api := runningController(selfAPI())
	api.existing = append(api.existing, swarm.Service{ID: "sidecar", Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "swarmcli-cd_git-sync", Labels: map[string]string{convert.LabelNamespace: "swarmcli-cd"}},
	}})
	const withSidecar = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    command: ["controller", "--config", "/etc/swarmcli-cd/applications.yaml"]
    environment:
      SWARMCLI_CD_ADMIN_TOKEN_FILE: /run/secrets/token
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    secrets:
      - source: swarmcli-cd-token
        target: token
    configs:
      - source: swarmcli-cd-applications
        target: /etc/swarmcli-cd/applications.yaml
  git-sync:
    image: alpine/git:2.45.2
secrets:
  swarmcli-cd-token:
    external: true
configs:
  swarmcli-cd-applications:
    external: true
`
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)
	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: withSidecar, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}
	if len(api.updated) != 1 || api.updated[0].spec.Name != "swarmcli-cd_git-sync" {
		t.Errorf("the deploy wrote %d services, want only the sidecar", len(api.updated))
	}
}

// The four losses a controller cannot recover from, because each one takes away
// the thing that would perform the next reconcile. Refused before any resource
// is created, so a swarm is never left with the answer half applied.
func TestASelfManifestMayNotDropWhatWouldFixIt(t *testing.T) {
	for name, tc := range map[string]struct{ manifest, want string }{
		"the socket": {`
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    command: ["controller", "--config", "/etc/swarmcli-cd/applications.yaml"]
    environment:
      SWARMCLI_CD_ADMIN_TOKEN_FILE: /run/secrets/token
    secrets:
      - source: swarmcli-cd-token
        target: token
    configs:
      - source: swarmcli-cd-applications
        target: /etc/swarmcli-cd/applications.yaml
secrets:
  swarmcli-cd-token:
    external: true
configs:
  swarmcli-cd-applications:
    external: true
`, "/var/run/docker.sock"},

		"the admin token": {`
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    command: ["controller", "--config", "/etc/swarmcli-cd/applications.yaml"]
    environment:
      SWARMCLI_CD_ADMIN_TOKEN_FILE: /run/secrets/token
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    configs:
      - source: swarmcli-cd-applications
        target: /etc/swarmcli-cd/applications.yaml
configs:
  swarmcli-cd-applications:
    external: true
`, "swarmcli-cd-token"},

		"the app-set flag": {`
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    command: ["controller"]
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    secrets:
      - source: swarmcli-cd-token
        target: token
    environment:
      SWARMCLI_CD_ADMIN_TOKEN_FILE: /run/secrets/token
secrets:
  swarmcli-cd-token:
    external: true
`, "--config"},

		"the app set the flag names": {`
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    command: ["controller", "--config", "/etc/swarmcli-cd/applications.yaml"]
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    secrets:
      - source: swarmcli-cd-token
        target: token
    environment:
      SWARMCLI_CD_ADMIN_TOKEN_FILE: /run/secrets/token
secrets:
  swarmcli-cd-token:
    external: true
`, "/etc/swarmcli-cd/applications.yaml"},

		"the controller itself": {`
services:
  something-else:
    image: eldaratech/swarmcli-cd:1.2.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
`, "declares no service under that name"},
	} {
		t.Run(name, func(t *testing.T) {
			api := runningController(selfAPI())
			b := testBackend(t, api, nil).WithSelfRelease(noDeferral)

			err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: tc.manifest, Resolve: ResolveNever})
			if err == nil {
				t.Fatalf("DeployStack = nil, want the loss of %s refused", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if len(api.created)+len(api.updated) > 0 {
				t.Error("wrote services for a refused release; the refusal must come before anything is created")
			}
		})
	}
}

// And what it may drop. A deployment that loses TLS or single sign-on is still
// running, still reconciling, and one commit away from having them back — so the
// loss is said out loud and applied. Refusing it would also refuse an operator
// legitimately turning one off, which the app set is entitled to decide.
func TestASelfManifestMayDropWhatTheControllerCanLiveWithout(t *testing.T) {
	api := runningController(selfAPI())
	api.selfSpec.TaskTemplate.ContainerSpec.Secrets = append(api.selfSpec.TaskTemplate.ContainerSpec.Secrets,
		&swarm.SecretReference{SecretName: "swarmcli-cd-oidc-secret", File: &swarm.SecretReferenceFileTarget{Name: "swarmcli-cd-oidc-secret"}})
	api.existing[0].Spec = api.selfSpec

	var logged bytes.Buffer
	b := New(api, Options{Log: slog.New(slog.NewTextHandler(&logged, nil))}).WithSelfRelease(noDeferral)

	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: bootstrappedStack, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want a survivable loss applied", err)
	}
	if !strings.Contains(logged.String(), "swarmcli-cd-oidc-secret") {
		t.Errorf("log %q does not say what stopped being mounted", logged.String())
	}
}

const mountsControllerConfig = `
services:
  evil:
    image: nginx
    configs: [stolen]
configs:
  stolen:
    external: true
    name: swarmcli-cd-applications
`

// The gap #63 was filed for. The app set names every repository, revision,
// destination and policy this controller applies; a tenant stack mounting it by
// name is reconnaissance handed over for free.
func TestDeployStackRefusesMountingTheControllersOwnConfig(t *testing.T) {
	api := asController(&fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "swarmcli-cd-applications"},
		}}},
	})

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: mountsControllerConfig, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want the stack refused for mounting the controller's config")
	}
	for _, want := range []string{"evil", "swarmcli-cd-applications", "controller"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// Refused whole: nothing was created before the reject.
	if len(api.order) != 0 || len(api.created) != 0 {
		t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
	}
}

// Which container the guard asks about, which is the premise everything above
// rests on and the one thing none of it asserts.
//
// In a container the hostname is the container id unless somebody overrode it,
// and that is what makes this process resolvable from the inside. Ask about
// anything else and a real daemon answers not-found — which readSelfMounts reads
// as "not a swarm task", correctly, and so returns empty sets. The guard is then
// off: every stack may mount the controller's own configs and secrets, on a
// healthy daemon, silently, with the whole suite green.
func TestTheSelfGuardIdentifiesThisContainerByItsHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	api := asController(&fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "swarmcli-cd-applications"},
		}}},
	})

	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsControllerConfig, Resolve: ResolveNever}); err == nil {
		t.Fatal("DeployStack = nil; the guard did not recognise this process as the controller")
	}
	if !reflect.DeepEqual(api.inspectedIDs, []string{host}) {
		t.Errorf("inspected %v, want this process's hostname %q — no other id resolves to this container", api.inspectedIDs, host)
	}
}

const mountsAReleaseRecord = `
services:
  evil:
    image: nginx
    configs: [stolen]
configs:
  stolen:
    external: true
    name: swarmcli.release.other-app.v3
`

// The other half, and the one no set captured at startup could cover: release
// records are created on every deploy and hold the rendered manifest of every
// release on the swarm.
func TestDeployStackRefusesMountingAReleaseRecord(t *testing.T) {
	api := asController(&fakeAPI{
		configs: []swarm.Config{{ID: "r", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
			Name:   "swarmcli.release.other-app.v3",
			Labels: map[string]string{charts.LabelType: charts.TypeRelease},
		}}}},
	})

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: mountsAReleaseRecord, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want the stack refused for mounting a release record")
	}
	if !strings.Contains(err.Error(), "release record") {
		t.Errorf("error %q does not say what was refused", err)
	}
}

// The secret half, via the service spec rather than the /run/secrets listing.
// Nothing is wired into forbiddenSecrets here: a controller whose stack.yml
// renames the mount target derives the wrong name from the filesystem, and this
// is what still refuses the stack.
func TestDeployStackRefusesAControllerSecretRenamedOnTheWayIn(t *testing.T) {
	api := asController(&fakeAPI{
		secrets: []swarm.Secret{{ID: "tok", Spec: swarm.SecretSpec{
			Annotations: swarm.Annotations{Name: "swarmcli-cd-token"},
		}}},
	})

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: mountsControllerSecret, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want the stack refused for mounting the controller's token")
	}
	if !strings.Contains(err.Error(), "swarmcli-cd-token") {
		t.Errorf("error %q does not name the secret", err)
	}
}

// ------------------------------------ a stack that claims one of our names (#86)

// stealsByDeclaring is a manifest that takes a resource over instead of
// referencing it: the entry is not `external:`, so it is one of the stack's own
// as far as conversion and the guard's reference check are concerned, and `name:`
// points it at something that already exists.
//
// kind is "secrets" or "configs". A secret is driver-backed, since a chart cannot
// ship a secret's content; a config takes its content from the chart's own
// files/decoy.conf, which is what decoyFiles carries.
func stealsByDeclaring(kind, name string, mount bool) string {
	svc := "services:\n  thief:\n    image: alpine\n"
	if mount {
		svc += "    " + kind + ": [x]\n"
	}
	source := "    driver: vault\n"
	if kind == "configs" {
		source = "    file: files/decoy.conf\n"
	}
	return svc + kind + ":\n  x:\n    name: " + name + "\n" + source
}

// decoyFiles is what a config-claiming stealsByDeclaring ships. It never reaches
// the swarm — a config that already exists is not created — so what is in it does
// not matter, which is exactly why this is not hard to write.
var decoyFiles = map[string][]byte{"files/decoy.conf": []byte("not the real thing\n")}

// The controller's own token, taken by declaring it rather than by referencing
// it. Before #86 this deployed: the reference check saw a name the stack declares
// and passed it, applySecrets found the secret already there and — unable to read
// a stored secret's data, so with nothing to compare — updated its labels and
// moved on, and the service was created holding the real secret's id.
//
// So the assertions are about both halves of that. Nothing created, and nothing
// *updated* either: a refusal that had already relabelled the controller's own
// secret into this stack's namespace would have handed a later RemoveStack the
// right to delete it.
func TestDeployStackRefusesAStackDeclaringAControllerSecret(t *testing.T) {
	api := asController(&fakeAPI{secrets: []swarm.Secret{{
		ID:   "real-token",
		Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "swarmcli-cd-token"}, Data: []byte("REAL")},
	}}})
	b := testBackend(t, api, nil).WithForbiddenSecrets(map[string]struct{}{"swarmcli-cd-token": {}}).(*Backend)

	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: stealsByDeclaring("secrets", "swarmcli-cd-token", true), Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want the stack refused for declaring the controller's own secret")
	}
	for _, want := range []string{"declares", "swarmcli-cd-token", "controller"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(api.created) != 0 {
		t.Errorf("%d services created; one of them holds the controller's token", len(api.created))
	}
	if len(api.order) != 0 || len(api.updatedSecrets) != 0 {
		t.Errorf("the controller's own secret was touched: order=%v updated=%+v", api.order, api.updatedSecrets)
	}
}

// Mounting it is not what makes it wrong. applySecrets and applyConfigs run over
// everything the manifest declares, so a declaration no service references still
// relabels the controller's resource into this stack's namespace — and that alone
// is enough for a later RemoveStack to delete it.
func TestDeployStackRefusesADeclarationNoServiceMounts(t *testing.T) {
	api := asController(&fakeAPI{secrets: []swarm.Secret{{
		ID:   "real-token",
		Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "swarmcli-cd-token"}},
	}}})

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: stealsByDeclaring("secrets", "swarmcli-cd-token", false), Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a declaration of the controller's secret refused even unmounted")
	}
	if len(api.updatedSecrets) != 0 {
		t.Errorf("the controller's own secret was relabelled: %+v", api.updatedSecrets)
	}
}

// The config half of the same shape. This one used to be refused by applyConfigs'
// immutability check rather than by the guard — an error about configs being
// immutable, for what is actually an attempt to take the application set over,
// and only after applySecrets had already run.
func TestDeployStackRefusesAStackDeclaringTheControllersOwnConfig(t *testing.T) {
	api := asController(&fakeAPI{configs: []swarm.Config{{
		ID:   "app-set",
		Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "swarmcli-cd-applications"}, Data: []byte("real")},
	}}})

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{
		Name: "tenant", Manifest: stealsByDeclaring("configs", "swarmcli-cd-applications", true),
		Resolve: ResolveNever, Files: decoyFiles,
	})
	if err == nil {
		t.Fatal("DeployStack = nil, want the stack refused for declaring the controller's own config")
	}
	if !strings.Contains(err.Error(), "declares") || !strings.Contains(err.Error(), "controller") {
		t.Errorf("error %q does not say what was refused", err)
	}
	if len(api.order) != 0 || len(api.updatedConfigs) != 0 {
		t.Errorf("the controller's own config was touched: order=%v updated=%+v", api.order, api.updatedConfigs)
	}
}

// A release record, taken the same way. Not immutability's business at all: the
// record it names does not exist yet, so nothing would have refused this.
//
// Configs and secrets are separate namespaces on the swarm, and the guard's
// config half compares configs only, so a config is the one kind that can claim a
// record's name — which is why this case, and the integration test
// TestAStackMayNotClaimAReleaseRecordAsItsOwn, are written as a config the chart
// ships.
//
// Two names, because two rules refuse it. The record's name format is the
// engine's and unexported, so an existing record is matched by its label too,
// and the second name is one a renamed format could produce: only that match
// catches it.
func TestDeployStackRefusesAStackDeclaringAReleaseRecordName(t *testing.T) {
	for _, record := range []string{"swarmcli.release.other-app.v3", "records.other-app.3"} {
		t.Run(record, func(t *testing.T) {
			api := asController(&fakeAPI{configs: []swarm.Config{{
				ID: "rec",
				Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
					Name:   record,
					Labels: map[string]string{charts.LabelType: charts.TypeRelease},
				}},
			}}})

			err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{
				Name: "tenant", Manifest: stealsByDeclaring("configs", record, true),
				Resolve: ResolveNever, Files: decoyFiles,
			})
			if err == nil {
				t.Fatal("DeployStack = nil, want the stack refused for declaring a release record's name")
			}
			if !strings.Contains(err.Error(), "declares") || !strings.Contains(err.Error(), "release record") {
				t.Errorf("error %q does not say what was refused", err)
			}
			if len(api.order) != 0 || len(api.updatedConfigs) != 0 {
				t.Errorf("the release record was touched: order=%v updated=%+v", api.order, api.updatedConfigs)
			}
		})
	}
}

// reservedLabelled is a chart declaring one config of its own and one driver-backed
// secret, each carrying the given labels — which, unlike a name, the chart
// chooses freely.
func reservedLabelled(labels string) string {
	return "services:\n  app:\n    image: busybox\n    configs: [site]\n    secrets: [token]\n" +
		"configs:\n  site:\n    file: files/decoy.conf\n    labels:\n" + labels +
		"secrets:\n  token:\n    driver: vault\n    labels:\n" + labels
}

// Labels under com.swarmcli. are the chart engine's and this controller's own
// bookkeeping — what marks a config as a release record, and the marker the sweep
// reads as proof that this controller created a resource. A declaration carrying
// one is refused whatever its name, before anything is created, and for either
// kind.
func TestDeployStackRefusesADeclarationCarryingAReservedLabel(t *testing.T) {
	for _, tc := range []struct{ name, labels, key string }{
		{"a release record's type", "      com.swarmcli.type: release\n      com.swarmcli.release: other-app\n", "com.swarmcli.release"},
		{"the creation marker", "      com.swarmcli.cd.created: \"2026-01-01T00:00:00Z\"\n", "com.swarmcli.cd.created"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{}
			err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{
				Name: "rel", Manifest: reservedLabelled(tc.labels), Resolve: ResolveNever, Files: decoyFiles,
			})
			if err == nil {
				t.Fatal("DeployStack = nil, want a declaration carrying a com.swarmcli. label refused")
			}
			for _, want := range []string{"declares", "'" + tc.key + "'", "com.swarmcli."} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			if len(api.order) != 0 || len(api.created) != 0 {
				t.Errorf("created %v and %d services, want nothing", api.order, len(api.created))
			}
		})
	}
}

// The secret half on its own: the config above is refused first, so without this
// a secret-only regression would pass unseen.
func TestDeployStackRefusesASecretCarryingAReservedLabel(t *testing.T) {
	api := &fakeAPI{}
	manifest := "services:\n  app:\n    image: busybox\n    secrets: [token]\n" +
		"secrets:\n  token:\n    driver: vault\n    labels:\n      com.swarmcli.cd.created: x\n"
	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "rel", Manifest: manifest, Resolve: ResolveNever})
	if err == nil || !strings.Contains(err.Error(), "declares secret 'rel_token'") {
		t.Fatalf("DeployStack = %v, want the secret refused for its com.swarmcli. label", err)
	}
	if len(api.order) != 0 {
		t.Errorf("created %v, want nothing", api.order)
	}
}

// An operator's config pre-seeded under the name a chart then declares, with the
// same bytes, is adopted — relabelled into the stack and never given the creation
// marker, which is what keeps the sweep off it. A chart that brought the marker
// among its own labels would have it carried onto the operator's config, so the
// declaration is refused and the config left as it was.
func TestAnAdoptedConfigCannotBeGivenTheCreationMarker(t *testing.T) {
	seeded := swarm.Config{ID: "seeded", Spec: swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: "rel_site", Labels: map[string]string{"owner": "operator"}},
		Data:        decoyFiles["files/decoy.conf"],
	}}
	api := &fakeAPI{configs: []swarm.Config{seeded}}
	manifest := "services:\n  app:\n    image: busybox\n    configs: [site]\n" +
		"configs:\n  site:\n    file: files/decoy.conf\n    labels:\n      com.swarmcli.cd.created: x\n"

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{
		Name: "rel", Manifest: manifest, Resolve: ResolveNever, Files: decoyFiles,
	})
	if err == nil || !strings.Contains(err.Error(), "com.swarmcli.cd.created") {
		t.Fatalf("DeployStack = %v, want the declaration refused for the creation marker", err)
	}
	if len(api.updatedConfigs) != 0 {
		t.Errorf("the operator's config was relabelled: %+v", api.updatedConfigs)
	}
}

// A release record's name is the engine's to allocate — a fixed prefix, the
// release name and a revision — including the ones it has not written yet. A
// release whose own name starts with that prefix can make a future record's name
// look scoped to itself, so a declared config or secret whose name starts with
// the prefix is refused whatever exists on the swarm, for either kind.
//
// Swarm keeps config and secret names unique regardless of case, so a name that
// differs from a future record's only in case would still hold it; the prefix is
// compared without regard to case.
func TestDeployStackRefusesADeclarationNamedLikeAReleaseRecord(t *testing.T) {
	for _, tc := range []struct{ name, kind, release, target string }{
		{"a config", "configs", "swarmcli.release.team", "swarmcli.release.team_web.v2"},
		{"a secret", "secrets", "swarmcli.release.team", "swarmcli.release.team_web.v2"},
		{"an upper-case config", "configs", "SWARMCLI.RELEASE.team", "SWARMCLI.RELEASE.team_web.v2"},
		{"an upper-case secret", "secrets", "SWARMCLI.RELEASE.team", "SWARMCLI.RELEASE.team_web.v2"},
		{"a mixed-case config", "configs", "Swarmcli.Release.team", "Swarmcli.Release.team_web.v2"},
		{"a mixed-case secret", "secrets", "Swarmcli.Release.team", "Swarmcli.Release.team_web.v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := asController(&fakeAPI{})
			err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{
				Name: tc.release, Manifest: stealsByDeclaring(tc.kind, tc.target, true),
				Resolve: ResolveNever, Files: decoyFiles,
			})
			if err == nil {
				t.Fatalf("DeployStack = nil, want %s named %s refused", tc.kind, tc.target)
			}
			for _, want := range []string{"declares", "'" + tc.target + "'", "release record"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			if len(api.order) != 0 {
				t.Errorf("created %v, want nothing", api.order)
			}
		})
	}
}

// An existing record is matched by name without regard to case as well, whether
// the stack declares the name or mounts it, so the stack is refused whole before
// anything is created, as for the exact name. The record here is named in a
// format other than the engine's, so only that match refuses it, and the
// application is permitted the name so that nothing else does. Neither
// spelling is in lower case, so both sides of the comparison are lowered.
func TestAReleaseRecordIsMatchedWhateverTheCase(t *testing.T) {
	const record, variant = "Records.other-app.3", "RECORDS.Other-App.3"
	for _, tc := range []struct{ name, manifest string }{
		{"declared", stealsByDeclaring("configs", variant, true)},
		{"mounted", mountsAConfig(variant)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := asController(&fakeAPI{configs: []swarm.Config{{
				ID: "rec",
				Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
					Name:   record,
					Labels: map[string]string{charts.LabelType: charts.TypeRelease},
				}},
			}}})
			err := allowing(t, api, application.Allow{Configs: []string{variant}}).DeployStack(t.Context(), charts.DeployRequest{
				Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever, Files: decoyFiles,
			})
			if err == nil || !strings.Contains(err.Error(), "'"+variant+"'") || !strings.Contains(err.Error(), "release record") {
				t.Fatalf("DeployStack = %v, want %s refused as a release record", err, variant)
			}
			if len(api.order) != 0 || len(api.updatedConfigs) != 0 {
				t.Errorf("the release record was touched: order=%v updated=%+v", api.order, api.updatedConfigs)
			}
		})
	}
}

// A config a stack deploy created is not a release record, whatever its labels
// say (isReleaseRecord), so the guard does not refuse a stack for mounting one
// that the app set permits, as it would a record.
func TestAStackConfigLabelledAsARecordIsNotOneToTheGuard(t *testing.T) {
	api := asController(&fakeAPI{configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: "other_site", Labels: map[string]string{
			charts.LabelType:       charts.TypeRelease,
			convert.LabelNamespace: "other",
		}},
	}}}})
	err := allowing(t, api, application.Allow{Configs: []string{"other_site"}}).DeployStack(t.Context(), charts.DeployRequest{
		Name: "tenant", Manifest: mountsAConfig("other_site"), Resolve: ResolveNever,
	})
	if err != nil {
		t.Fatalf("DeployStack = %v, want a stack's config mounted as the app set permits", err)
	}
	if !slices.Contains(api.labelFilters, charts.LabelType+"="+charts.TypeRelease) {
		t.Errorf("label filters = %q, want the records listed by their type label", api.labelFilters)
	}
}

// declaresExternal is a chart declaring one config or secret external:, named
// name, with the given labels (none when empty). No service mounts it unless
// mounted says so.
func declaresExternal(kind, name, labels string, mounted bool) string {
	svc := "services:\n  app:\n    image: busybox\n"
	if mounted {
		svc += "    " + kind + ": [x]\n"
	}
	decl := kind + ":\n  x:\n    external: true\n    name: " + name + "\n"
	if labels != "" {
		decl += "    labels:\n" + labels
	}
	return svc + decl
}

// An external: declaration creates and relabels nothing, but a name in the space
// release records are named in, or a label under com.swarmcli., is refused on one
// as on a stack's own declaration — whether or not a service mounts it, whatever
// the app set permits, and before the app set is consulted, so that the refusal
// never asks for a permission that would not help.
func TestDeployStackRefusesAReservedExternalDeclaration(t *testing.T) {
	const record = "swarmcli.release.other.v2"
	behindAnOrdinaryOne := "services:\n  app:\n    image: busybox\nconfigs:\n" +
		"  a:\n    external: true\n    name: a-shared\n  x:\n    external: true\n    name: " + record + "\n"
	named := func(kind, name string) []string {
		return []string{"external " + kind + " '" + name + "'", "'swarmcli.release.'", "release record"}
	}
	for _, tc := range []struct {
		name, manifest, target string
		permit                 bool
		want                   []string
	}{
		{"a config named like a record", declaresExternal("configs", record, "", false), record, true, named("config", record)},
		{"an upper-case secret name", declaresExternal("secrets", "SWARMCLI.RELEASE.other.v2", "", false),
			"SWARMCLI.RELEASE.other.v2", true, named("secret", "SWARMCLI.RELEASE.other.v2")},
		{"a mixed-case config name, mounted and permitted", declaresExternal("configs", "Swarmcli.Release.other.v2", "", true),
			"Swarmcli.Release.other.v2", true, named("config", "Swarmcli.Release.other.v2")},
		{"a config named like a record, mounted and not permitted", declaresExternal("configs", record, "", true),
			record, false, named("config", record)},
		{"a record's name behind an ordinary external", behindAnOrdinaryOne, record, true, named("config", record)},
		{"a config labelled as a record", declaresExternal("configs", "shared-site", "      com.swarmcli.type: release\n", false),
			"shared-site", true, []string{"external config 'shared-site'", "'com.swarmcli.type'"}},
		{"a secret carrying the creation marker", declaresExternal("secrets", "shared-key", "      com.swarmcli.cd.created: x\n", false),
			"shared-key", true, []string{"external secret 'shared-key'", "'com.swarmcli.cd.created'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := asController(&fakeAPI{
				configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: tc.target}}}},
				secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: tc.target}}}},
			})
			var allow application.Allow
			if tc.permit {
				allow = application.Allow{Configs: []string{tc.target}, Secrets: []string{tc.target}}
			}
			err := allowing(t, api, allow).DeployStack(t.Context(), charts.DeployRequest{
				Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever,
			})
			if err == nil {
				t.Fatalf("DeployStack = nil, want the external '%s' refused", tc.target)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			if len(api.order) != 0 || len(api.created) != 0 {
				t.Errorf("created %v and %d services, want nothing", api.order, len(api.created))
			}
		})
	}
}

// And the other side: an external whose labels are ordinary, and whose name
// holds the prefix anywhere but at the start, is referenced like any other.
func TestDeployStackAllowsAnOrdinaryLabelledExternal(t *testing.T) {
	const name = "app.swarmcli.release.v2"
	api := asController(&fakeAPI{configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: name}}}}})
	err := allowing(t, api, application.Allow{Configs: []string{name}}).DeployStack(t.Context(), charts.DeployRequest{
		Name: "tenant", Manifest: declaresExternal("configs", name, "      team: web\n", true), Resolve: ResolveNever,
	})
	if err != nil {
		t.Fatalf("DeployStack = %v, want an ordinary labelled external mounted", err)
	}
}

// The false-positive check, and the reason the new rule compares names rather
// than refusing declarations outright: a chart declaring and mounting its own
// secret is ordinary, and #84 exists so that it works. Its name is
// namespace-scoped, so it is nobody else's.
func TestAStackDeclaringItsOwnSecretIsAllowed(t *testing.T) {
	api := asController(&fakeAPI{})

	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "rel", Manifest: declaresAndMounts, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want a chart's own secret to be allowed", err)
	}
}

// An external reference is the ordinary way to share a config an operator
// created, and refusing those would make the guard unusable. One the app set
// permits deploys; only the controller's own and the engine's own are off limits
// whatever it permits.
func TestDeployStackAllowsAnOrdinaryExternalConfig(t *testing.T) {
	api := asController(&fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	})

	if err := allowing(t, api, oneOfEachAllowed).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}
}

// A stack that declares everything it uses reaches outside itself for nothing,
// so the guard must cost it no reading of the swarm's release records — the one
// listing here that grows with every release ever deployed.
//
// The read about this controller itself is no longer conditional and cannot be:
// a release name is compared against the controller's own namespace whatever the
// manifest declares (#102). It costs one pair of round trips for the life of the
// process, which is what TestTheControllersOwnMountsAreReadOnce pins. So is the
// read of the records by name prefix (rejectRecordedCollision), which is bounded
// by this release's own history and those of the releases its name collides with,
// rather than by every release's.
func TestAStackThatReachesForNothingCostsNoReleaseLookup(t *testing.T) {
	const selfContained = `
services:
  web:
    image: nginx
`
	api := asController(&fakeAPI{})
	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: selfContained, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}
	for _, f := range api.labelFilters {
		if strings.HasPrefix(f, "com.swarmcli.") {
			t.Errorf("listed by %q; a stack that reaches for nothing must not cost a release-record read", f)
		}
	}
	if api.selfInspects != 1 {
		t.Errorf("asked about this controller %d times, want 1", api.selfInspects)
	}
}

// Read once and reused: every With* method copies the backend, and a per-copy
// cache would re-read this for every application on every deploy.
func TestTheControllersOwnMountsAreReadOnce(t *testing.T) {
	api := asController(&fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	})
	// The fake now records service creates, so the second deploy sees the first's
	// services. A real DeployStack writes a release record on the way through;
	// the fake does not, so stand one in or the ownership guard refuses (#102).
	installed(api, "s")
	b := testBackend(t, api, nil).WithAllowedReferences(oneOfEachAllowed).(*Backend)

	for range 3 {
		if err := b.WithRegistryAuth(nil).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
			t.Fatalf("DeployStack = %v, want nil", err)
		}
	}
	if api.selfInspects != 1 {
		t.Errorf("asked about this controller %d times, want 1", api.selfInspects)
	}
}

// A daemon that is reachable and answers with an error fails the deploy rather
// than letting it through unguarded, and the failure is not cached: a hiccup
// must not disable deploys for the life of the controller.
func TestAFailedSelfReadRefusesTheDeployAndIsRetried(t *testing.T) {
	api := &fakeAPI{
		selfErr: errors.New("daemon busy"),
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	}
	b := allowing(t, api, oneOfEachAllowed)

	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err == nil {
		t.Fatal("DeployStack = nil, want the deploy refused rather than run unguarded")
	}
	if len(api.created) != 0 {
		t.Errorf("%d services created despite the failure", len(api.created))
	}

	api.selfErr = nil
	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
		t.Fatalf("second DeployStack = %v, want the failure not to have been cached", err)
	}
}

// connectionFailure is the error the moby client returns when it could not reach
// the daemon at all — a dockerd restart, or a socket that was not there for a few
// hundred milliseconds.
//
// It has to come from the client itself: IsErrConnectionFailed matches an
// unexported type, so no error built here would be recognised as one, and the
// exported constructor for it is deprecated. Dialling a socket path that does not
// exist produces the real thing without a daemon or a network.
func connectionFailure(t *testing.T) error {
	t.Helper()
	c, err := client.NewClientWithOpts(client.WithHost("unix:///nonexistent/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ContainerInspect(context.Background(), "any")
	if !client.IsErrConnectionFailed(err) {
		t.Fatalf("inspecting through a dead socket = %v, want a connection failure", err)
	}
	return err
}

// The daemon being unreachable is not the news that nothing is mounted (#101).
// This container is still running, and the spec it was started from still mounts
// the controller's token and its application set; the only thing that changed is
// that nobody could be asked. So it fails the deploy, exactly as a daemon that
// answers with an error does, rather than proceeding unguarded.
func TestAnUnreachableDaemonRefusesTheDeployAndIsRetried(t *testing.T) {
	api := asController(&fakeAPI{
		selfErr: connectionFailure(t),
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	})
	b := allowing(t, api, oneOfEachAllowed)

	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want the deploy refused rather than run unguarded")
	}
	if !client.IsErrConnectionFailed(err) {
		t.Errorf("DeployStack = %v, want the daemon's connection failure surfaced", err)
	}
	if len(api.created) != 0 {
		t.Errorf("%d services created despite the failure", len(api.created))
	}

	api.selfErr = nil
	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
		t.Fatalf("second DeployStack = %v, want the failure not to have been cached", err)
	}
}

// And the half that made #101 critical rather than merely noisy: what the
// unreachable daemon must not do is leave an empty set behind, because an empty
// set is a valid answer and therefore caches. One hiccup used to turn the guard
// off for the life of the controller — silently, with a healthy daemon, and for
// every application on the swarm.
func TestAnUnreachableDaemonDoesNotDisableTheGuard(t *testing.T) {
	api := asController(&fakeAPI{
		selfErr: connectionFailure(t),
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "swarmcli-cd-applications"},
		}}},
	})
	b := testBackend(t, api, nil)

	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsControllerConfig, Resolve: ResolveNever}); err == nil {
		t.Fatal("DeployStack = nil, want the deploy refused while the guard could not be read")
	}

	api.selfErr = nil
	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsControllerConfig, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want the guard still on once the daemon answers")
	}
	if !strings.Contains(err.Error(), "swarmcli-cd-applications") || !strings.Contains(err.Error(), "controller") {
		t.Errorf("error %q is not the guard refusing the stack", err)
	}
	if len(api.created) != 0 || len(api.order) != 0 {
		t.Errorf("the stack was deployed: created=%d order=%v", len(api.created), api.order)
	}
}

// The service spec is a second round trip, so the daemon can be gone by the time
// it is made — and reading it is the whole point of reading the daemon at all,
// since the mount targets on the filesystem carry the wrong names.
func TestAnUnreachableDaemonOnTheServiceReadAlsoRefuses(t *testing.T) {
	api := asController(&fakeAPI{
		selfSpecErr: connectionFailure(t),
		secrets: []swarm.Secret{{ID: "tok", Spec: swarm.SecretSpec{
			Annotations: swarm.Annotations{Name: "swarmcli-cd-token"},
		}}},
	})
	b := testBackend(t, api, nil)

	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsControllerSecret, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want the deploy refused rather than run unguarded")
	}
	if !client.IsErrConnectionFailed(err) {
		t.Errorf("DeployStack = %v, want the daemon's connection failure surfaced", err)
	}

	api.selfSpecErr = nil
	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsControllerSecret, Resolve: ResolveNever}); err == nil {
		t.Fatal("DeployStack = nil, want the guard still on once the daemon answers")
	}
	if len(api.created) != 0 {
		t.Errorf("%d services created, one of them holding the controller's token", len(api.created))
	}
}

// A controller that is not a swarm task — a development run — has nothing
// mounted by Swarm, which is an answer rather than a failure.
func TestOutsideASwarmThereIsNothingOfOursToProtect(t *testing.T) {
	api := &fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	}
	if err := allowing(t, api, oneOfEachAllowed).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}
}

// The other side of the split #101 drew, as the daemon really phrases it. A
// not-found is the daemon answering, and what it answers is that this process is
// not one of its containers — so nothing was mounted by Swarm, and failing the
// deploy would break every development run and every plain `docker run` for no
// gain. Only "could not ask" fails closed.
func TestAContainerThisDaemonDoesNotKnowIsAnAnswer(t *testing.T) {
	api := &fakeAPI{
		selfErr: fmt.Errorf("Error: No such container: deadbeef: %w", errdefs.ErrNotFound),
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	}
	// The fake now records service creates, so the second deploy sees the first's
	// services. A real DeployStack writes a release record on the way through;
	// the fake does not, so stand one in or the ownership guard refuses (#102).
	installed(api, "s")
	b := allowing(t, api, oneOfEachAllowed)

	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want a controller that is not a swarm task to deploy", err)
	}
	// Cached, unlike the unreachable case: this one is an answer, and it cannot
	// change for the life of the process.
	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
		t.Fatalf("second DeployStack = %v, want nil", err)
	}
	if api.selfInspects != 1 {
		t.Errorf("asked about this controller %d times, want 1", api.selfInspects)
	}
}

// -------------------------------------------- the creation marker (#108)

// What the marker is for: a config or secret this controller made carries it,
// so the sweep can tell it from one of the same name it merely adopted.
func TestCreatedConfigsAndSecretsCarryTheCreationMarker(t *testing.T) {
	api := &fakeAPI{}
	b := testBackend(t, api, nil)
	ctx := context.Background()

	if err := b.applyConfigs(ctx, []swarm.ConfigSpec{{Annotations: swarm.Annotations{Name: "s_site"}}}); err != nil {
		t.Fatalf("applyConfigs = %v, want nil", err)
	}
	if err := b.applySecrets(ctx, []swarm.SecretSpec{{Annotations: swarm.Annotations{Name: "s_key"}}}); err != nil {
		t.Fatalf("applySecrets = %v, want nil", err)
	}
	if _, ok := api.createdConfigs[0].Labels[createdLabel]; !ok {
		t.Errorf("config created with %v, want the creation marker", api.createdConfigs[0].Labels)
	}
	if _, ok := api.createdSecrets[0].Labels[createdLabel]; !ok {
		t.Errorf("secret created with %v, want the creation marker", api.createdSecrets[0].Labels)
	}
}

// The pre-seed pattern, which is the whole of issue #108's fourth item: an
// operator ran `docker secret create s_apikey`, permitted it in the app set, and
// a chart then declared it. The apply adopts it — relabels it into the stack's
// namespace, which is what makes it a sweep candidate — and must not claim to
// have created it.
func TestAnAdoptedConfigOrSecretNeverGainsTheCreationMarker(t *testing.T) {
	api := &fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "s_site"}, Data: []byte("same"),
		}}},
		secrets: []swarm.Secret{{ID: "k", Spec: swarm.SecretSpec{
			Annotations: swarm.Annotations{Name: "s_apikey"},
		}}},
	}
	b := testBackend(t, api, nil)
	b.allow = application.Allow{Configs: []string{"s_site"}, Secrets: []string{"s_apikey"}}
	ctx := context.Background()

	if err := b.applyConfigs(ctx, []swarm.ConfigSpec{{
		Annotations: swarm.Annotations{Name: "s_site", Labels: map[string]string{convert.LabelNamespace: "s"}},
		Data:        []byte("same"),
	}}); err != nil {
		t.Fatalf("applyConfigs = %v, want nil", err)
	}
	if err := b.applySecrets(ctx, []swarm.SecretSpec{{
		Annotations: swarm.Annotations{Name: "s_apikey", Labels: map[string]string{convert.LabelNamespace: "s"}},
	}}); err != nil {
		t.Fatalf("applySecrets = %v, want nil", err)
	}

	if _, ok := api.updatedConfigs[0].Labels[createdLabel]; ok {
		t.Errorf("an adopted config was marked as created: %v", api.updatedConfigs[0].Labels)
	}
	if _, ok := api.updatedSecrets[0].Labels[createdLabel]; ok {
		t.Errorf("an adopted secret was marked as created: %v", api.updatedSecrets[0].Labels)
	}
	// The adoption itself is unchanged: the namespace label still goes on, which
	// is what lets `docker stack rm` and RemoveStack find it.
	if api.updatedSecrets[0].Labels[convert.LabelNamespace] != "s" {
		t.Errorf("adopted secret labels = %v, want it still relabelled into the stack", api.updatedSecrets[0].Labels)
	}
}

// A config or secret update replaces the whole annotation set, so a resource
// this controller did create would lose its marker on the next reconcile — and
// with it the sweep's only proof, silently, one deploy after it was written.
func TestAnUpdateCarriesTheCreationMarkerForward(t *testing.T) {
	api := &fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{
			Annotations: stackScoped("s_site", "s"), Data: []byte("same"),
		}}},
		secrets: []swarm.Secret{{ID: "k", Spec: swarm.SecretSpec{
			Annotations: stackScoped("s_apikey", "s"),
		}}},
	}
	b := testBackend(t, api, nil)
	ctx := context.Background()

	if err := b.applyConfigs(ctx, []swarm.ConfigSpec{{
		Annotations: swarm.Annotations{Name: "s_site", Labels: map[string]string{convert.LabelNamespace: "s"}},
		Data:        []byte("same"),
	}}); err != nil {
		t.Fatalf("applyConfigs = %v, want nil", err)
	}
	if err := b.applySecrets(ctx, []swarm.SecretSpec{{
		Annotations: swarm.Annotations{Name: "s_apikey", Labels: map[string]string{convert.LabelNamespace: "s"}},
	}}); err != nil {
		t.Fatalf("applySecrets = %v, want nil", err)
	}

	if _, ok := api.updatedConfigs[0].Labels[createdLabel]; !ok {
		t.Errorf("config update dropped the creation marker: %v", api.updatedConfigs[0].Labels)
	}
	if _, ok := api.updatedSecrets[0].Labels[createdLabel]; !ok {
		t.Errorf("secret update dropped the creation marker: %v", api.updatedSecrets[0].Labels)
	}
}

// The map belongs to the converted stack, which DeployStack reads again after
// applying the unresolved half — a label written into it here would travel into
// specs this never saw.
func TestStampingTheCreationMarkerDoesNotMutateTheCallersLabels(t *testing.T) {
	labels := map[string]string{convert.LabelNamespace: "s"}
	specs := []swarm.ConfigSpec{{Annotations: swarm.Annotations{Name: "s_site", Labels: labels}}}

	if err := testBackend(t, &fakeAPI{}, nil).applyConfigs(context.Background(), specs); err != nil {
		t.Fatalf("applyConfigs = %v, want nil", err)
	}
	if len(labels) != 1 {
		t.Errorf("the caller's labels became %v, want them untouched", labels)
	}
}

// -------------------------------------------------- volumes (#108)

// A volume that went between the list and the delete has reached the state this
// was asking for. Alone among the five removals this reported it as a failure,
// and the caller retries — so the loss was the whole settle budget spent on a
// volume that had already gone, and then a prune failed naming "still in use".
func TestRemoveVolumeToleratesOneAlreadyGone(t *testing.T) {
	api := &fakeAPI{volumes: []volume.Volume{{Name: "s_data"}}, removeErr: map[string]error{"volume:s_data": errdefs.ErrNotFound}}

	if err := testBackend(t, api, nil).RemoveVolume(context.Background(), "s_data"); err != nil {
		t.Errorf("RemoveVolume = %v, want nil for one already gone", err)
	}
}

// A volume is removed by name, and the daemon falls back to a cluster volume of
// that name when no node-local one answers it. So a removal inspects first and
// acts only on a node-local volume — and, asked for one of a stack's volumes,
// only on one still carrying that stack's namespace. Anything else the name now
// answers with is left in place.
func TestAVolumeIsRemovedOnlyWhileItIsTheOneListed(t *testing.T) {
	ns := func(stack string) map[string]string { return map[string]string{convert.LabelNamespace: stack} }
	for _, tc := range []struct {
		name    string
		api     *fakeAPI
		stack   string
		removed bool
	}{
		{"the stack's node-local volume", &fakeAPI{volumes: []volume.Volume{{Name: "s_data", Labels: ns("s")}}}, "s", true},
		{"a node-local volume of another stack", &fakeAPI{volumes: []volume.Volume{{Name: "s_data", Labels: ns("t")}}}, "s", false},
		{"a node-local volume of a stack named in another case", &fakeAPI{volumes: []volume.Volume{{Name: "s_data", Labels: ns("S")}}}, "s", false},
		{"a node-local volume of a stack sharing the prefix", &fakeAPI{volumes: []volume.Volume{{Name: "s_data", Labels: ns("s-staging")}}}, "s", false},
		{"only a cluster volume of that name", &fakeAPI{clusterVolumes: []volume.Volume{{Name: "s_data", Labels: ns("s")}}}, "s", false},
		{"nothing of that name", &fakeAPI{}, "s", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := testBackend(t, tc.api, nil).RemoveStackVolume(t.Context(), capability.StackVolume{Stack: tc.stack, Name: "s_data"})
			// Left in place is said, so a purge does not report it as deleted;
			// gone already is not.
			wantLeft := !tc.removed && len(tc.api.volumes)+len(tc.api.clusterVolumes) > 0
			if got := errors.Is(err, capability.ErrVolumeLeft); got != wantLeft || (err != nil && !wantLeft) {
				t.Fatalf("RemoveStackVolume = %v, want ErrVolumeLeft: %v", err, wantLeft)
			}
			if got := slices.Contains(tc.api.removed, "volume:s_data"); got != tc.removed {
				t.Errorf("removed %v, want the volume removed: %v", tc.api.removed, tc.removed)
			}
		})
	}

	// RemoveVolume, which carries no stack, still acts only on a node-local one.
	api := &fakeAPI{clusterVolumes: []volume.Volume{{Name: "s_data"}}}
	if err := testBackend(t, api, nil).RemoveVolume(t.Context(), "s_data"); err != nil || len(api.removed) != 0 {
		t.Errorf("RemoveVolume = %v, removed %v; want a cluster volume of that name left in place", err, api.removed)
	}
}

// An inspect that fails answers nothing about the name, so nothing is removed on
// the strength of it: the failure is returned, and the delete is not sent.
func TestAVolumeIsNotRemovedWhenItCouldNotBeInspected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(*Backend, *fakeAPI) error
	}{
		{"RemoveVolume", func(b *Backend, _ *fakeAPI) error { return b.RemoveVolume(t.Context(), "s_data") }},
		{"RemoveStackVolume", func(b *Backend, _ *fakeAPI) error {
			return b.RemoveStackVolume(t.Context(), capability.StackVolume{Stack: "s", Name: "s_data"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{
				volumes:          []volume.Volume{{Name: "s_data", Labels: map[string]string{convert.LabelNamespace: "s"}}},
				volumeInspectErr: errors.New("daemon busy"),
			}
			if err := tc.remove(testBackend(t, api, nil), api); err == nil || !strings.Contains(err.Error(), "daemon busy") {
				t.Errorf("err = %v, want the inspect failure", err)
			}
			if len(api.removed) != 0 {
				t.Errorf("removed %v after an inspect that failed", api.removed)
			}
		})
	}
}

func TestRemoveVolumeSurfacesARefusal(t *testing.T) {
	api := &fakeAPI{volumes: []volume.Volume{{Name: "s_data"}}, removeErr: map[string]error{"volume:s_data": errors.New("volume is in use")}}

	err := testBackend(t, api, nil).RemoveVolume(context.Background(), "s_data")
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("RemoveVolume = %v, want the daemon's refusal surfaced", err)
	}
}

// The count that says whether a node-local volume listing was the whole swarm's.
func TestSwarmNodesCountsTheSwarm(t *testing.T) {
	api := &fakeAPI{nodes: []swarm.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}}}

	n, err := testBackend(t, api, nil).SwarmNodes(context.Background())
	if err != nil {
		t.Fatalf("SwarmNodes = %v, want nil", err)
	}
	if n != 3 {
		t.Errorf("SwarmNodes = %d, want 3", n)
	}
}

// A worker cannot enumerate the swarm, and that unanswerable question must
// arrive as one rather than as "one node".
func TestSwarmNodesSurfacesAFailureRatherThanCountingZero(t *testing.T) {
	api := &fakeAPI{nodeErr: errors.New("this node is not a swarm manager")}

	if _, err := testBackend(t, api, nil).SwarmNodes(context.Background()); err == nil {
		t.Fatal("SwarmNodes = nil, want the listing failure surfaced")
	}
}

// ------------------------- a release named for the controller's own stack (#102)

// takesOverTheController is the shape the report was filed with: a release named
// after the controller's stack, with a service named after the controller's, and
// the socket the controller itself is mounted with.
//
// It does not have to be this exact manifest to be dangerous — any spec written
// over the controller is a controller running somebody else's image — but this is
// the one that makes the escalation obvious.
const takesOverTheController = `
services:
  controller:
    image: attacker/evil
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
`

// controllerStack is the swarm as it stands with a controller deployed on it:
// its service, its overlay network and the volume holding every application's
// clone and chart cache, all carrying its namespace.
func controllerStack() *fakeAPI {
	return asController(&fakeAPI{
		existing: []swarm.Service{{
			ID:   "ctl",
			Spec: swarm.ServiceSpec{Annotations: stackScoped("swarmcli-cd_controller", "swarmcli-cd")},
		}},
		networks: []network.Summary{stackNetwork("net", "swarmcli-cd_default", "swarmcli-cd")},
		volumes: []volume.Volume{{
			Name:   "swarmcli-cd_swarmcli-cd-data",
			Labels: map[string]string{convert.LabelNamespace: "swarmcli-cd"},
		}},
	})
}

// The takeover direction. A release name is the stack namespace, and the
// controller's own service carries that label like anything else Swarm deployed —
// so a release called "swarmcli-cd" with a service called "controller" scopes to
// swarmcli-cd_controller, the controller's exact service name, and the write path
// hands the daemon this chart's spec for it.
func TestDeployStackRefusesTheControllersOwnStackName(t *testing.T) {
	api := controllerStack()

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: takesOverTheController, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a release claiming the controller's own stack refused")
	}
	// The name, and this refusal rather than the ownership one below it: a
	// release claiming the controller's own stack is refused for being that name
	// and not because the services under it happen to be unaccounted for.
	if !strings.Contains(err.Error(), "swarmcli-cd") || !strings.Contains(err.Error(), "this controller itself") {
		t.Errorf("error %q does not say which name was refused and why", err)
	}
	if len(api.created) != 0 || len(api.updated) != 0 || len(api.order) != 0 {
		t.Errorf("the controller was written to: created=%d updated=%d order=%v",
			len(api.created), len(api.updated), api.order)
	}
	// And the other way out. This guard fires only for a release the app set did
	// not mark, so an operator following the self documentation who lands here
	// has one thing to change and it is not the release name — which is what
	// #234 cost twice over.
	if !strings.Contains(err.Error(), "self: true") {
		t.Errorf("error %q does not name `self: true` as the other way out", err)
	}
}

// The destruction direction, which needs no chart at all. RemoveStack deletes
// everything carrying the namespace label and checks no ownership — deliberately,
// because that is what `docker stack rm` does — so its only protection is that the
// name is not the controller's.
func TestRemoveStackRefusesTheControllersOwnStackName(t *testing.T) {
	api := controllerStack()

	err := testBackend(t, api, nil).RemoveStack(t.Context(), "swarmcli-cd")
	if err == nil {
		t.Fatal("RemoveStack = nil, want the controller's own stack refused")
	}
	if len(api.removed) != 0 {
		t.Errorf("removed %v; the controller deleted itself", api.removed)
	}
}

// And the volumes, which is what makes the destruction permanent: the controller
// comes back from git, the git clone and chart cache in its volume do not, and
// nothing reconverges because the thing that would have has been deleted.
//
// Guarded on the listing rather than only on the removal because the chart
// engine's Uninstall purges volumes even when the stack removal before it failed:
// it collects that error and carries on.
func TestStackVolumesRefusesTheControllersOwnStackName(t *testing.T) {
	api := controllerStack()

	vols, err := testBackend(t, api, nil).StackVolumes(context.Background(), "swarmcli-cd")
	if err == nil {
		t.Fatal("StackVolumes = nil, want the controller's own volumes refused")
	}
	if vols != nil {
		t.Errorf("StackVolumes = %v, want nothing for a purge to delete", vols)
	}
}

// Swarm compares the names a deploy creates without regard to case, so a release
// named like the controller's stack in another case scopes names the swarm
// already holds for the controller. The deploy is refused before anything is
// created, rather than left to fail partway on the first name that collides.
func TestDeployStackRefusesTheControllersStackNameInAnotherCase(t *testing.T) {
	for _, tc := range []struct{ namespace, release string }{
		{"swarmcli-cd", "SWARMCLI-CD"},
		{"SwarmCD", "swarmcd"},
	} {
		api := controllerStack()
		api.selfSpec.Labels[convert.LabelNamespace] = tc.namespace

		err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: tc.release, Manifest: trivialStack, Resolve: ResolveNever})
		if err == nil {
			t.Fatalf("DeployStack(%s) = nil, want a release named like the controller's stack '%s' in another case refused", tc.release, tc.namespace)
		}
		for _, want := range []string{"'" + tc.release + "'", "'" + tc.namespace + "'", "case"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not say %s", err, want)
			}
		}
		if len(api.created) != 0 || len(api.updated) != 0 || len(api.order) != 0 {
			t.Errorf("the swarm was written to: created=%d updated=%d order=%v", len(api.created), len(api.updated), api.order)
		}
	}
	// The exact name is rejectOwnNamespace's, whose refusal names the other way
	// out; this one answers only for another case, and for no other name.
	for _, name := range []string{"swarmcli-cd", "swarmcli-cd-edge"} {
		if err := testBackend(t, controllerStack(), nil).rejectOwnNamespaceInAnotherCase(t.Context(), name); err != nil {
			t.Errorf("rejectOwnNamespaceInAnotherCase(%s) = %v, want nil", name, err)
		}
	}
}

// A release's records are Swarm configs, named after the release, so a release
// whose name differs from one that already has records only in case would need
// record names the swarm already holds: its deploy would land and then fail to
// be recorded, and every later deploy be refused for want of a record. Refused
// before anything is created, whichever source declared it, naming the release
// that holds the names; the release that holds them deploys as before.
func TestDeployStackRefusesAReleaseDifferingOnlyInCaseFromARecordedOne(t *testing.T) {
	record := func(release string) swarm.Config {
		return swarm.Config{ID: "rec-" + release, Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
			Name:   "swarmcli.release." + release + ".v1",
			Labels: map[string]string{charts.LabelType: charts.TypeRelease, charts.LabelRelease: release},
		}}}
	}
	api := asController(&fakeAPI{configs: []swarm.Config{record("WEB"), record("web.x")}})

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: trivialStack, Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a release differing only in case from a recorded one refused")
	}
	for _, want := range []string{"'web'", "'WEB'", "case"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %s", err, want)
		}
	}
	if len(api.created) != 0 || len(api.order) != 0 {
		t.Errorf("created %v and %d services, want nothing", api.order, len(api.created))
	}
	if !slices.Contains(api.configNameFilters, "swarmcli.release.web.") {
		t.Errorf("config name filters = %q, want the records listed by this release's name prefix", api.configNameFilters)
	}

	// A stack's config typed as a record is not one (isReleaseRecord), so it
	// holds no names for this check either.
	stacked := record("API")
	stacked.Spec.Labels[convert.LabelNamespace] = "API"
	for _, release := range []string{"WEB", "web.x", "api"} {
		api := asController(&fakeAPI{configs: []swarm.Config{record("WEB"), record("web.x"), stacked}})
		err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: release, Manifest: trivialStack, Resolve: ResolveNever})
		if err != nil {
			t.Errorf("DeployStack(%s) = %v, want a release holding its own names deployed", release, err)
		}
	}
}

// A release whose name is another's followed by '_', or which that other's name
// extends, scopes names that collide with it, so it is not installed while the
// other has records — whichever of the two it is, and however the case differs.
// A release that has records of its own keeps deploying beside such a one, which
// is a pair installed before this was refused; and a name merely starting with
// another's collides with nothing. The records are asked for by name prefix, the
// shorter names' included.
func TestDeployStackRefusesInstallingAReleaseCollidingWithARecordedOne(t *testing.T) {
	record := func(release string) swarm.Config {
		return swarm.Config{ID: "rec-" + release, Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
			Name:   "swarmcli.release." + release + ".v1",
			Labels: map[string]string{charts.LabelType: charts.TypeRelease, charts.LabelRelease: release},
		}}}
	}
	for _, tc := range []struct {
		release  string
		recorded []string
		refused  string
	}{
		{"web_a", []string{"web"}, "'web'"},
		{"web", []string{"WEB_a"}, "'WEB_a'"},
		{"web", []string{"web", "web_a"}, ""},
		{"web_a", []string{"web_a", "web"}, ""},
		{"web", []string{"web", "WEB_a"}, ""},
		{"web", []string{"web"}, ""},
		{"web", []string{"webapp", "web-a"}, ""},
	} {
		api := asController(&fakeAPI{configs: nil})
		for _, r := range tc.recorded {
			api.configs = append(api.configs, record(r))
		}
		var logged bytes.Buffer
		b := New(api, Options{Log: slog.New(slog.NewTextHandler(&logged, nil))})
		err := b.DeployStack(t.Context(), charts.DeployRequest{Name: tc.release, Manifest: trivialStack, Resolve: ResolveNever})
		switch {
		case tc.refused == "" && err != nil:
			t.Errorf("DeployStack(%s) beside %v = %v, want it deployed", tc.release, tc.recorded, err)
		case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused) || !strings.Contains(err.Error(), "'_'")):
			t.Errorf("DeployStack(%s) beside %v = %v, want it refused naming %s", tc.release, tc.recorded, err, tc.refused)
		case tc.refused != "" && (len(api.created) != 0 || len(api.order) != 0):
			t.Errorf("DeployStack(%s) created %v and %d services, want nothing", tc.release, api.order, len(api.created))
		}
		// A pair installed together before this was refused keeps deploying, and
		// warns, either half of it, since its volumes are still told apart by name
		// alone. One whose names match only without regard to case shares no
		// volume name, since those keep their case, and does not.
		other := map[string]string{"web": "web_a", "web_a": "web"}[tc.release]
		pair := slices.Contains(tc.recorded, tc.release) && slices.Contains(tc.recorded, other)
		if warned := strings.Contains(logged.String(), "level=WARN"); warned != pair || pair && !strings.Contains(logged.String(), "collidesWith="+other) {
			t.Errorf("DeployStack(%s) beside %v logged %q, want a warning %t", tc.release, tc.recorded, logged.String(), pair)
		}
	}

	// Beside two, the refusal names the first in order, whatever order the
	// daemon lists them in.
	for _, listed := range [][]string{{"web_b", "web_a"}, {"web_a", "web_b"}} {
		api := asController(&fakeAPI{configs: []swarm.Config{record(listed[0]), record(listed[1])}})
		err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: trivialStack, Resolve: ResolveNever})
		if err == nil || !strings.Contains(err.Error(), "'web_a'") {
			t.Errorf("DeployStack(web) beside %v = %v, want it refused naming 'web_a'", listed, err)
		}
	}

	// The self release adopts the stack this controller already runs as, so a
	// recorded release whose name extends it does not refuse it.
	self := selfAPI()
	self.configs = append(self.configs, record("swarmcli-cd_x"))
	if err := testBackend(t, self, nil).WithSelfRelease(noDeferral).DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: selfStack, Resolve: ResolveNever}); err != nil {
		t.Errorf("DeployStack(self) beside a recorded 'swarmcli-cd_x' = %v, want the self release adopted", err)
	}

	api := asController(&fakeAPI{})
	_ = testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "a_b_c", Manifest: trivialStack, Resolve: ResolveNever})
	for _, want := range []string{"swarmcli.release.a_b_c.", "swarmcli.release.a_b_c_", "swarmcli.release.a.", "swarmcli.release.a_b."} {
		if !slices.Contains(api.configNameFilters, want) {
			t.Errorf("config name filters = %q, want %q among them", api.configNameFilters, want)
		}
	}
}

// Where the adoption happens, the same rule as the guard: a config or secret that
// has come to hold a declared name since the guard looked, labelled as another
// stack's or as nobody's, is not relabelled into this one unless the app set
// permits the name. One already labelled as this stack's is.
func TestAHolderThatAppearedAfterTheCheckIsNotAdopted(t *testing.T) {
	for _, holder := range []map[string]string{{convert.LabelNamespace: "web_a"}, nil} {
		api := &fakeAPI{
			configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "web_a_site", Labels: holder}, Data: []byte("same")}}},
			secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "web_a_site", Labels: holder}}}},
		}
		labels := map[string]string{convert.LabelNamespace: "web"}
		configs := []swarm.ConfigSpec{{Annotations: swarm.Annotations{Name: "web_a_site", Labels: labels}, Data: []byte("same")}}
		secrets := []swarm.SecretSpec{{Annotations: swarm.Annotations{Name: "web_a_site", Labels: labels}}}

		b := testBackend(t, api, nil)
		if err := b.applyConfigs(t.Context(), configs); err == nil || !strings.Contains(err.Error(), "allow.configs") {
			t.Errorf("applyConfigs over %v = %v, want the adoption refused", holder, err)
		}
		if err := b.applySecrets(t.Context(), secrets); err == nil || !strings.Contains(err.Error(), "allow.secrets") {
			t.Errorf("applySecrets over %v = %v, want the adoption refused", holder, err)
		}
		if len(api.updatedConfigs)+len(api.updatedSecrets) != 0 {
			t.Errorf("relabelled %v and %v, want nothing", api.updatedConfigs, api.updatedSecrets)
		}

		b.allow = application.Allow{Configs: []string{"web_a_site"}, Secrets: []string{"web_a_site"}}
		if err := b.applyConfigs(t.Context(), configs); err != nil {
			t.Errorf("applyConfigs over %v = %v, want a permitted name adopted", holder, err)
		}
		if err := b.applySecrets(t.Context(), secrets); err != nil {
			t.Errorf("applySecrets over %v = %v, want a permitted name adopted", holder, err)
		}
	}
}

// Every network a stack declares is looked up in one listing of the swarm's, not
// one each.
func TestDeclaredNetworksAreLookedUpInOneListing(t *testing.T) {
	api := &countingNetworksAPI{fakeAPI: asController(&fakeAPI{})}
	manifest := "services:\n  app:\n    image: busybox\n    networks: [a, b, c]\nnetworks:\n  a: {}\n  b: {}\n  c: {}\n"
	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: manifest, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}
	if api.unfiltered != 1 {
		t.Errorf("listed every network %d times for three declared ones, want once", api.unfiltered)
	}
}

// countingNetworksAPI counts the network listings made with no filter at all.
type countingNetworksAPI struct {
	*fakeAPI
	unfiltered int
}

func (a *countingNetworksAPI) NetworkList(ctx context.Context, o network.ListOptions) ([]network.Summary, error) {
	if o.Filters.Len() == 0 {
		a.unfiltered++
	}
	return a.fakeAPI.NetworkList(ctx, o)
}

// A service joins only a swarm-scoped network, so a node-local one of the same
// name — a compose project's default network on the manager, say — is nobody's
// claim on the release's own and does not refuse it.
func TestALocalNetworkOfTheSameNameIsNotTheHolder(t *testing.T) {
	api := asController(&fakeAPI{networks: []network.Summary{{ID: "l", Name: "web_default", Scope: "local"}}})
	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: trivialStack, Resolve: ResolveNever}); err != nil {
		t.Errorf("DeployStack = %v, want the release's own network created beside a local one", err)
	}
}

// Removing it is not refused. RemoveStack and StackVolumes select by the
// namespace label, whose value Swarm compares exactly, so they reach only what
// carries that spelling — never the controller's — and refusing would leave such
// a stack for every later sweep to fail on.
//
// The controller's service is left out of the fixture: the fake's ServiceList
// returns every service whatever the filter, where its config, network and
// volume listings filter as the daemon does.
func TestRemovingTheControllersStackNameInAnotherCaseLeavesTheControllerAlone(t *testing.T) {
	api := controllerStack()
	api.existing = nil
	api.configs = append(api.configs, swarm.Config{ID: "variant", Spec: swarm.ConfigSpec{Annotations: stackScoped("SWARMCLI-CD_site", "SWARMCLI-CD")}})
	api.volumes = append(api.volumes, volume.Volume{Name: "SWARMCLI-CD_data", Labels: map[string]string{convert.LabelNamespace: "SWARMCLI-CD"}})

	if err := testBackend(t, api, nil).RemoveStack(t.Context(), "SWARMCLI-CD"); err != nil {
		t.Fatalf("RemoveStack = %v, want the variant's own stack removed", err)
	}
	if !reflect.DeepEqual(api.removed, []string{"config:variant"}) {
		t.Errorf("removed %v, want only what carries the variant's namespace", api.removed)
	}
	vols, err := testBackend(t, api, nil).StackVolumes(t.Context(), "SWARMCLI-CD")
	if err != nil || !reflect.DeepEqual(vols, []string{"SWARMCLI-CD_data"}) {
		t.Errorf("StackVolumes = %v, %v; want the variant's own volume alone", vols, err)
	}
}

// A release's own names are those scoped under its namespace as written, and
// scopedUnder does not fold case although Swarm does. Folding would be the loose
// direction: a release named 'Web' would read another application's 'web_site'
// as its own and mount it without the app set's permission, where it now needs
// that permission like any other name outside the release.
func TestANamespaceInAnotherCaseIsNotTheReleasesOwn(t *testing.T) {
	api := asController(&fakeAPI{configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: stackScoped("web_site", "web")}}}})

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "Web", Manifest: mountsAConfig("web_site"), Resolve: ResolveNever})
	if err == nil || !strings.Contains(err.Error(), "allow.configs") {
		t.Fatalf("DeployStack = %v, want another stack's config refused as not permitted", err)
	}
	if len(api.created) != 0 {
		t.Errorf("created %d services, want none", len(api.created))
	}
}

// A cluster mount names an existing CSI volume, or a whole volume group, as a
// volume mount names a volume, and passes the same guard: another stack's is
// refused unless the app set permits the name, and the controller's own is
// refused outright. None is the release's own, whatever it is called: a stack
// never creates a cluster volume, so a name scoped under the release — its own
// key, or one scoped into the names of a release called tenant_a — is somebody's
// already, and needs the app set's permission like any other.
func TestAClusterMountPassesTheVolumeGuard(t *testing.T) {
	mounts := func(source, decl string) string {
		return "services:\n  app:\n    image: busybox\n    volumes:\n" +
			"      - {type: cluster, source: " + source + ", target: /data}\n" + decl
	}
	for _, tc := range []struct {
		name, manifest, want string
		allow                application.Allow
	}{
		{"another stack's volume", mounts("shared-csi", "volumes:\n  shared-csi: {external: true}\n"), "allow.volumes", application.Allow{}},
		{"a volume group", mounts("group:db", ""), "allow.volumes", application.Allow{}},
		{"the controller's own volume", mounts("data", "volumes:\n  data: {external: true, name: swarmcli-cd_swarmcli-cd-data}\n"),
			"this controller's own volume", application.Allow{Volumes: []string{"swarmcli-cd_swarmcli-cd-data"}}},
		{"permitted", mounts("shared-csi", "volumes:\n  shared-csi: {external: true}\n"), "", application.Allow{Volumes: []string{"shared-csi"}}},
		{"a permitted volume group", mounts("group:db", ""), "", application.Allow{Volumes: []string{"group:db"}}},
		{"a name scoped under the release", mounts("data", "volumes:\n  data: {}\n"), "allow.volumes", application.Allow{}},
		{"a name scoped into another release's", mounts("a_data", "volumes:\n  a_data: {}\n"), "allow.volumes", application.Allow{}},
		{"a permitted name scoped under the release", mounts("data", "volumes:\n  data: {}\n"), "", application.Allow{Volumes: []string{"tenant_data"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := asController(&fakeAPI{})
			err := allowing(t, api, tc.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("DeployStack = %v, want the cluster mount deployed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DeployStack = %v, want the cluster mount refused (%s)", err, tc.want)
			}
			if len(api.created) != 0 || len(api.order) != 0 {
				t.Errorf("created %v and %d services, want nothing", api.order, len(api.created))
			}
		})
	}
}

// The cluster rule and the driver-options rule hold together in one service: a
// cluster mount scoped under the release needs its entry, a volume of the
// release's own with driver options needs its own, and each entry answers for
// its own mount and not the other's. A volume named outside the release with
// driver options is refused whatever the app set permits.
func TestClusterMountsAndDriverOptionsAreHeldApart(t *testing.T) {
	const manifest = "services:\n  app:\n    image: busybox\n    volumes:\n" +
		"      - {type: cluster, source: csi, target: /csi}\n      - opts:/opts\n" +
		"volumes:\n  csi: {}\n  opts: {driver_opts: {type: none, o: bind, device: /srv}}\n"
	const foreign = "services:\n  app:\n    image: busybox\n    volumes: [\"opts:/opts\"]\n" +
		"volumes:\n  opts: {name: shared-data, driver_opts: {type: none, o: bind, device: /srv}}\n"
	for _, tc := range []struct {
		name, manifest, want string
		allow                application.Allow
	}{
		{"neither permitted", manifest, "'web_csi'", application.Allow{}},
		{"only the cluster mount", manifest, "driver_opts", application.Allow{Volumes: []string{"web_csi"}}},
		{"only the volume with options", manifest, "'web_csi'", application.Allow{Volumes: []string{"web_opts"}}},
		{"both", manifest, "", application.Allow{Volumes: []string{"web_csi", "web_opts"}}},
		{"options outside the release", foreign, "external:", application.Allow{Volumes: []string{"shared-data"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := asController(&fakeAPI{})
			err := allowing(t, api, tc.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: tc.manifest, Resolve: ResolveNever})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("DeployStack = %v, want it deployed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DeployStack = %v, want it refused mentioning %s", err, tc.want)
			}
			if len(api.created) != 0 || len(api.order) != 0 {
				t.Errorf("created %v and %d services, want nothing", api.order, len(api.created))
			}
		})
	}
}

// A controller that keeps its own state on a CSI cluster volume holds that name
// as it holds a volume's: no app set permits another release to mount it, and
// the self release may re-declare it.
//
// Swarm resolves a group source to any volume in that group, so the group of a
// cluster volume the controller mounts by name is the controller's too.
func TestTheControllersOwnClusterVolumeIsItsOwn(t *testing.T) {
	const tenantMounts = "services:\n  app:\n    image: busybox\n    volumes:\n" +
		"      - {type: cluster, source: %s, target: /state}\n%s"
	withCSI := func(api *fakeAPI) *fakeAPI {
		api = asController(api)
		cs := api.selfSpec.TaskTemplate.ContainerSpec
		cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeCluster, Source: "cd-csi", Target: "/state"})
		api.clusterVolumes = append(api.clusterVolumes, volume.Volume{Name: "cd-csi", ClusterVolume: &volume.ClusterVolume{
			ID: "csi-cd", Spec: volume.ClusterVolumeSpec{Group: "cd-state"},
		}})
		return api
	}

	for _, tc := range []struct{ name, source, decl string }{
		{"by name", "state", "volumes:\n  state: {external: true, name: cd-csi}\n"},
		{"by its group", "group:cd-state", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := withCSI(&fakeAPI{})
			err := allowing(t, api, application.Allow{Volumes: []string{"cd-csi", "group:cd-state"}}).DeployStack(t.Context(), charts.DeployRequest{
				Name: "tenant", Manifest: fmt.Sprintf(tenantMounts, tc.source, tc.decl), Resolve: ResolveNever,
			})
			if err == nil || !strings.Contains(err.Error(), "this controller's own volume") {
				t.Fatalf("DeployStack = %v, want the controller's cluster volume refused whatever the app set says", err)
			}
		})
	}

	// A cluster volume whose group cannot be read is not taken for one without a
	// group: the controller's own mounts are not known, so nothing is deployed.
	api := withCSI(&fakeAPI{volumeInspectErr: errors.New("daemon busy")})
	err := allowing(t, api, application.Allow{Volumes: []string{"group:cd-state"}}).DeployStack(t.Context(), charts.DeployRequest{
		Name: "tenant", Manifest: fmt.Sprintf(tenantMounts, "group:cd-state", ""), Resolve: ResolveNever,
	})
	if err == nil || !strings.Contains(err.Error(), "daemon busy") {
		t.Fatalf("DeployStack = %v, want the failed read of the controller's own volume surfaced", err)
	}

	// One that is gone has no group left to protect, and does not stop anybody
	// else's deploy.
	api = asController(&fakeAPI{})
	cs := api.selfSpec.TaskTemplate.ContainerSpec
	cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeCluster, Source: "cd-csi", Target: "/state"})
	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: trivialStack, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want a deploy unaffected by a controller volume that is gone", err)
	}

	// And the self release re-declares it: the controller's own stack, with the
	// cluster mount beside everything else the controller runs with.
	api = withCSI(selfAPI())
	manifest := strings.Replace(selfStack, "      - swarmcli-cd-data:/var/lib/swarmcli-cd\n",
		"      - swarmcli-cd-data:/var/lib/swarmcli-cd\n      - {type: cluster, source: state, target: /state}\n", 1) +
		"  state: {external: true, name: cd-csi}\n"
	if err := testBackend(t, api, nil).WithSelfRelease(noDeferral).DeployStack(t.Context(), charts.DeployRequest{
		Name: "swarmcli-cd", Manifest: manifest, Resolve: ResolveNever,
	}); err != nil {
		t.Fatalf("DeployStack(self) = %v, want the controller's own cluster volume recognised as its own", err)
	}
}

// The guard is one name, not a mode. Everything else on the swarm deploys and is
// removed exactly as before, including on a controller that is itself a stack.
func TestAnyOtherReleaseIsDeployedAndRemovedAsBefore(t *testing.T) {
	api := installed(asController(&fakeAPI{
		existing: []swarm.Service{{
			ID:   "svc",
			Spec: swarm.ServiceSpec{Annotations: stackScoped("s_web", "s")},
		}},
	}), "s")
	b := testBackend(t, api, nil)

	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: "services:\n  web:\n    image: nginx\n", Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want an ordinary release deployed", err)
	}
	if err := b.RemoveStack(t.Context(), "s"); err != nil {
		t.Fatalf("RemoveStack = %v, want an ordinary release removed", err)
	}
	if !slices.Contains(api.removed, "service:svc") {
		t.Errorf("removed %v, want the release's own service", api.removed)
	}
}

// A controller that is not a swarm service has no stack of its own, so there is
// no name to protect and the guard says nothing. Refusing "swarmcli-cd" here
// would be inventing a rule from a string rather than reading one off the swarm —
// and it would break every development run against a swarm that happens to have a
// release by that name.
func TestOutsideASwarmTheNamespaceGuardIsInert(t *testing.T) {
	api := &fakeAPI{existing: []swarm.Service{{
		ID:   "ctl",
		Spec: swarm.ServiceSpec{Annotations: stackScoped("swarmcli-cd_controller", "swarmcli-cd")},
	}}}
	b := testBackend(t, api, nil)

	if err := b.RemoveStack(t.Context(), "swarmcli-cd"); err != nil {
		t.Fatalf("RemoveStack = %v, want no guard for a controller that has no stack", err)
	}
	if !slices.Contains(api.removed, "service:ctl") {
		t.Errorf("removed %v, want the stack removed as it always was", api.removed)
	}
	if _, err := b.StackVolumes(context.Background(), "swarmcli-cd"); err != nil {
		t.Fatalf("StackVolumes = %v, want no guard for a controller that has no stack", err)
	}
}

// -------------------------------- the volume and the network (#103)

// mountsAVolume is a stack whose service mounts a volume it did not create.
//
// The two forms are the two ways a manifest names one, and they are the same two
// #86 found for a secret: `external: true` with a sibling `name:`, and a
// stack-owned entry whose `name:` points at something that already exists. They
// are one code path by the time conversion is done — convert.Volumes puts
// whatever `name:` said on the mount either way — which is exactly why the guard
// reads the mount rather than the declaration.
func mountsAVolume(name string, external bool) string {
	entry := "volumes:\n  loot:\n    name: " + name + "\n"
	if external {
		entry = "volumes:\n  loot:\n    external: true\n    name: " + name + "\n"
	}
	return "services:\n  thief:\n    image: busybox\n    volumes: [\"loot:/loot\"]\n" + entry
}

// A named volume is addressed on the node by name with no namespace on the
// reference, exactly as a secret is on the cluster — so a stack naming the
// controller's own gets it.
//
// What that is worth is not the controller's own state so much as everyone
// else's: the volume holds every application's git clone and chart cache. Read,
// it is the content of every watched private repository. Written, it is the next
// reconcile deploying the attacker's tree under the victim application's name,
// which is tenant to tenant rather than merely tenant to controller.
func TestDeployStackRefusesMountingTheControllersOwnVolume(t *testing.T) {
	for _, external := range []bool{true, false} {
		name := "declared"
		if external {
			name = "external"
		}
		t.Run(name, func(t *testing.T) {
			api := asController(&fakeAPI{})

			err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsAVolume("swarmcli-cd_swarmcli-cd-data", external), Resolve: ResolveNever})
			if err == nil {
				t.Fatal("DeployStack = nil, want the stack refused for mounting the controller's own volume")
			}
			for _, want := range []string{"thief", "swarmcli-cd_swarmcli-cd-data", "chart cache"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			// Refused whole, before the network the stack also declares is created.
			if len(api.created) != 0 || len(api.order) != 0 {
				t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
			}
		})
	}
}

// The false-positive half, which is now the application's to decide. Sharing a
// volume between stacks is what `external:` is for and remains available; what
// changed with #64 is that the app set has to say so, because a volume another
// tenant owns is that tenant's data and the reference is a bare name.
//
// A chart's own volume needs no entry: it is scoped to the release, so it is
// nobody else's and nobody's to permit.
func TestAStackMayMountAVolumeItsApplicationPermits(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		allow          application.Allow
	}{
		{"an operator's shared volume", mountsAVolume("shared-cache", true), application.Allow{Volumes: []string{"shared-cache"}}},
		{"the chart's own", "services:\n  app:\n    image: busybox\n    volumes: [\"data:/data\"]\nvolumes:\n  data: {}\n", application.Allow{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fake of its own per case: this one records what it creates, so a
			// second deploy through the same one sees the first's services under a
			// namespace with no release record and is refused for that instead (#105).
			if err := allowing(t, asController(&fakeAPI{}), tc.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever}); err != nil {
				t.Fatalf("DeployStack = %v, want the volume allowed", err)
			}
		})
	}
}

// And the two ways the same chart is refused: an application that permitted
// nothing, and one that permitted something else. The second is what makes this
// an allowlist rather than a switch.
func TestAStackMayNotMountAVolumeItsApplicationDoesNotPermit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow application.Allow
	}{
		{"permitted nothing", application.Allow{}},
		{"permitted another volume", application.Allow{Volumes: []string{"some-other-cache"}}},
		// The kinds are separate lists on purpose: a name is a volume or a
		// secret, and one entry must not answer for both.
		{"permitted it as a secret", application.Allow{Secrets: []string{"shared-cache"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both forms: an external: one, and one the stack declares under a
			// name: outside the release, which is its own volume in name only.
			for _, external := range []bool{true, false} {
				api := asController(&fakeAPI{})

				err := allowing(t, api, tc.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsAVolume("shared-cache", external), Resolve: ResolveNever})
				if err == nil {
					t.Fatalf("DeployStack (external %t) = nil, want the volume refused", external)
				}
				for _, want := range []string{"thief", "shared-cache", "allow.volumes"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				if len(api.created) != 0 || len(api.order) != 0 {
					t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
				}
			}
		})
	}
}

// declaresAVolume is a chart declaring a volume of its own, data, with the given
// block, and mounting it. Scoped to the release, so nothing but the block can make
// it need an allow.volumes entry.
func declaresAVolume(block string) string {
	return "services:\n  app:\n    image: busybox\n    volumes: [\"data:/data\"]\nvolumes:\n  data:\n" + block
}

// driverBackedVolumes are declarations whose driver or driver_opts decide what the
// node mounts when it creates the volume. Every one is held to allow.volumes, with
// no attempt to tell one option set from another.
var driverBackedVolumes = []struct{ name, block string }{
	{"a local bind", "    driver: local\n    driver_opts:\n      type: none\n      o: bind\n      device: /srv/data\n"},
	{"driver_opts with the driver left to default", "    driver_opts:\n      type: none\n      o: bind\n      device: /srv/data\n"},
	{"overlay", "    driver: local\n    driver_opts:\n      type: overlay\n      device: overlay\n      o: \"lowerdir=/srv/a,upperdir=/srv/u,workdir=/srv/w\"\n"},
	{"tmpfs", "    driver: local\n    driver_opts:\n      type: tmpfs\n      device: tmpfs\n      o: size=100m\n"},
	{"nfs", "    driver: local\n    driver_opts:\n      type: nfs\n      o: \"addr=10.0.0.1,rw\"\n      device: \":/export\"\n"},
	{"a plugin driver", "    driver: example/volume-plugin\n"},
}

// A stack's own volume given a driver or driver_opts needs an allow.volumes entry
// like a volume it does not own, and a refusal creates nothing.
func TestAVolumeWithDriverOptionsNeedsAllowVolumes(t *testing.T) {
	for _, tc := range driverBackedVolumes {
		t.Run(tc.name, func(t *testing.T) {
			api := asController(&fakeAPI{})
			err := allowing(t, api, application.Allow{}).DeployStack(t.Context(), charts.DeployRequest{
				Name: "tenant", Manifest: declaresAVolume(tc.block), Resolve: ResolveNever})
			if err == nil {
				t.Fatal("DeployStack = nil, want the volume refused")
			}
			for _, want := range []string{"service 'app'", "tenant_data", "driver_opts", "allow.volumes"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if len(api.created) != 0 || len(api.order) != 0 {
				t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
			}
		})
	}
}

// And each deploys once the application names it.
func TestAVolumeWithDriverOptionsDeploysWhenPermitted(t *testing.T) {
	for _, tc := range driverBackedVolumes {
		t.Run(tc.name, func(t *testing.T) {
			err := allowing(t, asController(&fakeAPI{}), application.Allow{Volumes: []string{"tenant_data"}}).DeployStack(t.Context(),
				charts.DeployRequest{Name: "tenant", Manifest: declaresAVolume(tc.block), Resolve: ResolveNever})
			if err != nil {
				t.Fatalf("DeployStack = %v, want the permitted volume deployed", err)
			}
		})
	}
}

// Driver options belong only on a volume named within the release. Another
// stack's volume is shared by declaring it external:, which carries none, so a
// declaration naming one with options is refused even when allow.volumes lists
// it — while the same name as an external: reference, and a release-scoped name:
// with options, deploy under the same entry.
func TestDriverOptionsNeedAVolumeNamedWithinTheRelease(t *testing.T) {
	const opts = "    driver: local\n    driver_opts:\n      type: nfs\n      o: \"addr=10.0.0.1,rw\"\n      device: \":/export\"\n"
	for _, tc := range []struct {
		name, manifest, allowed, want string
	}{
		{"another stack's name with options", declaresAVolume("    name: shared-cache\n" + opts), "shared-cache", "external:"},
		{"another stack's name as external", mountsAVolume("shared-cache", true), "shared-cache", ""},
		{"a release-scoped name with options", declaresAVolume("    name: tenant_custom\n" + opts), "tenant_custom", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := asController(&fakeAPI{})
			err := allowing(t, api, application.Allow{Volumes: []string{tc.allowed}}).DeployStack(t.Context(),
				charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("DeployStack = %v, want the volume deployed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.allowed) {
				t.Fatalf("DeployStack = %v, want it refused naming %q and %q", err, tc.allowed, tc.want)
			}
			if len(api.created) != 0 || len(api.order) != 0 {
				t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
			}
		})
	}
}

// What the rule leaves alone: a declaration naming the default driver and no
// options is a plain named volume, and an anonymous one has no declaration at all
// — its mount carries no VolumeOptions. None needs an entry.
func TestAVolumeWithoutDriverOptionsNeedsNoEntry(t *testing.T) {
	for _, tc := range []struct{ name, manifest string }{
		{"a plain declaration", declaresAVolume("    {}\n")},
		{"the local driver named", declaresAVolume("    driver: local\n")},
		{"empty driver_opts", declaresAVolume("    driver_opts: {}\n")},
		{"an anonymous volume", "services:\n  app:\n    image: busybox\n    volumes: [\"/data\"]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := allowing(t, asController(&fakeAPI{}), application.Allow{}).DeployStack(t.Context(),
				charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever}); err != nil {
				t.Fatalf("DeployStack = %v, want the volume deployed", err)
			}
		})
	}
}

// The self release redeclares the volume the controller already runs with, and a
// deployment that gave it driver options keeps deploying without an entry: it is
// recognised as the controller's own before the options are looked at.
func TestASelfReleaseKeepsItsOwnVolumeWithDriverOptions(t *testing.T) {
	const manifest = `
services:
  controller:
    image: eldaratech/swarmcli-cd:1.2.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - swarmcli-cd-data:/var/lib/swarmcli-cd
volumes:
  swarmcli-cd-data:
    driver: local
    driver_opts:
      type: nfs
      o: "addr=10.0.0.1,rw"
      device: ":/export"
`
	b := testBackend(t, selfAPI(), nil).WithSelfRelease(noDeferral)
	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "swarmcli-cd", Manifest: manifest, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want the controller's own volume kept", err)
	}
}

// joinsANetwork is the network shape of the same two forms.
func joinsANetwork(name string, external bool) string {
	entry := "networks:\n  inside:\n    name: " + name + "\n"
	if external {
		entry = "networks:\n  inside:\n    external: true\n    name: " + name + "\n"
	}
	return "services:\n  thief:\n    image: busybox\n    networks: [inside]\n" + entry
}

// The controller's API holds root-equivalent access to the swarm behind one
// shared bearer token over plaintext HTTP, and stack.yml publishes no port for
// it — being reachable only from inside the swarm is what makes that acceptable.
// A stack that joins the controller's own network is inside.
func TestDeployStackRefusesJoiningTheControllersOwnNetwork(t *testing.T) {
	for _, external := range []bool{true, false} {
		name := "declared"
		if external {
			name = "external"
		}
		t.Run(name, func(t *testing.T) {
			api := asController(&fakeAPI{})

			err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: joinsANetwork("swarmcli-cd_default", external), Resolve: ResolveNever})
			if err == nil {
				t.Fatal("DeployStack = nil, want the stack refused for joining the controller's own network")
			}
			for _, want := range []string{"swarmcli-cd_default", "this controller itself"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if len(api.created) != 0 || len(api.order) != 0 {
				t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
			}
		})
	}
}

// And the namespace itself, which is a network name a manifest can write even
// though `docker stack deploy` would never produce one.
func TestDeployStackRefusesANetworkNamedForTheControllersStack(t *testing.T) {
	api := asController(&fakeAPI{})

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: joinsANetwork("swarmcli-cd", true), Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want the controller's own namespace refused as a network name")
	}
	if !strings.Contains(err.Error(), "this controller itself") {
		t.Errorf("error %q is not the network guard refusing the stack", err)
	}
	if len(api.created) != 0 || len(api.order) != 0 {
		t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
	}
}

// The false-positive half again, and the reason the rule is the controller's own
// namespace rather than every network it is attached to: a shared external
// network is the ordinary way to put a stack behind an ingress proxy, and a
// chart's own network is scoped to its release.
//
// The shared one now needs the app set's permission, which is #64 — a network is
// every service already on it, so which ones an application may join is the
// operator's answer rather than the chart's. The other two need none: both are
// the release's own `<release>_default`.
func TestAStackMayJoinANetworkItsApplicationPermits(t *testing.T) {
	for _, tc := range []struct {
		name, release, manifest string
		allow                   application.Allow
	}{
		{"an operator's shared network", "tenant", joinsANetwork("traefik-public", true), application.Allow{Networks: []string{"traefik-public"}}},
		{"the chart's own", "tenant", "services:\n  app:\n    image: busybox\n", application.Allow{}},
		// "swarmcli-cd-apps_default" starts with the controller's namespace and is
		// not scoped under it. A prefix test without the separator would refuse it.
		{"a release named like ours", "swarmcli-cd-apps", "services:\n  app:\n    image: busybox\n", application.Allow{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := allowing(t, asController(&fakeAPI{}), tc.allow).DeployStack(t.Context(), charts.DeployRequest{Name: tc.release, Manifest: tc.manifest, Resolve: ResolveNever}); err != nil {
				t.Fatalf("DeployStack = %v, want the network allowed", err)
			}
		})
	}
}

// Both forms of naming somebody else's network, refused by an application that
// did not ask for it — and by one that asked for a different one.
func TestAStackMayNotJoinANetworkItsApplicationDoesNotPermit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		external bool
		allow    application.Allow
	}{
		{"external, permitted nothing", true, application.Allow{}},
		{"declared, permitted nothing", false, application.Allow{}},
		{"permitted another network", true, application.Allow{Networks: []string{"some-other-public"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := asController(&fakeAPI{})

			err := allowing(t, api, tc.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: joinsANetwork("traefik-public", tc.external), Resolve: ResolveNever})
			if err == nil {
				t.Fatal("DeployStack = nil, want the network refused")
			}
			for _, want := range []string{"traefik-public", "allow.networks"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if len(api.created) != 0 || len(api.order) != 0 {
				t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
			}
		})
	}
}

// A controller that is not a swarm service has nothing mounted by Swarm and no
// stack of its own, so both guards are inert rather than refusing everything —
// the same answer the secret and config halves give, for the same reason. A
// development run against a swarm that happens to hold a volume or a network by
// these names must deploy exactly as before.
//
// The application permits the names, so the only thing that could refuse here is
// the controller guard — which is the one being asserted inert. That the entry
// works at all is the other half of the statement: with no controller of its own
// on the swarm, "swarmcli-cd_swarmcli-cd-data" is an ordinary name an operator
// may grant like any other.
func TestOutsideASwarmTheVolumeAndNetworkGuardsAreInert(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		allow          application.Allow
	}{
		{"volume", mountsAVolume("swarmcli-cd_swarmcli-cd-data", true), application.Allow{Volumes: []string{"swarmcli-cd_swarmcli-cd-data"}}},
		{"network", joinsANetwork("swarmcli-cd_default", true), application.Allow{Networks: []string{"swarmcli-cd_default"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := allowing(t, &fakeAPI{}, tc.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever}); err != nil {
				t.Fatalf("DeployStack = %v, want no guard for a controller with no stack of its own", err)
			}
		})
	}
}

// The fail-closed property #101 established, on the two channels added since. A
// daemon that could not be asked is not the news that this controller has no
// volume and no stack of its own: the deploy fails, and the failure is not
// cached, so the next one tries again.
func TestAnUnreachableDaemonRefusesAVolumeOrNetworkDeploy(t *testing.T) {
	for _, tc := range []struct{ name, manifest string }{
		{"volume", mountsAVolume("swarmcli-cd_swarmcli-cd-data", true)},
		{"network", joinsANetwork("swarmcli-cd_default", true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := asController(&fakeAPI{selfErr: connectionFailure(t)})
			b := testBackend(t, api, nil)

			err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever})
			if err == nil {
				t.Fatal("DeployStack = nil, want the deploy refused rather than run unguarded")
			}
			if !client.IsErrConnectionFailed(err) {
				t.Errorf("DeployStack = %v, want the daemon's connection failure surfaced", err)
			}

			api.selfErr = nil
			if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever}); err == nil {
				t.Fatal("DeployStack = nil, want the guard on once the daemon answers")
			}
			if len(api.created) != 0 {
				t.Errorf("%d services created despite the refusal", len(api.created))
			}
		})
	}
}

// The swarm read runs under the caller's context and stops when it does.
//
// It could not before: charts.Backend declared StackServices with no context, so
// this backend substituted context.Background() and a reconcile being cancelled
// had no way to stop a read against an unresponsive daemon. CE widened the
// interface (Eldara-Tech/swarmcli#532); this is the end of it that matters here.
//
// It is a real test only because the API fake honours the context too. One that
// ignored it would pass whether or not this backend threaded anything.
func TestTheSwarmReadStopsWithItsContext(t *testing.T) {
	b := New(&fakeAPI{}, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := b.ReadStacks(ctx, []string{"whoami"}); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadStacks = %v, want context.Canceled in the chain", err)
	}
	if _, err := b.ReadStackServices(ctx, "whoami"); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadStackServices = %v, want context.Canceled in the chain", err)
	}
}

// One request for many releases, not one each. The read behind it is a
// whole-swarm snapshot filtered by stack name afterwards, so asking per release
// fetched the whole swarm per release and discarded all but one stack's worth
// each time (#134).
func TestReadStacksAsksTheSwarmOnce(t *testing.T) {
	api := &fakeAPI{}
	b := New(api, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	states, err := b.ReadStacks(t.Context(), []string{"whoami", "api", "web"})
	if err != nil {
		t.Fatalf("ReadStacks = %v, want nil", err)
	}
	if len(states) != 3 {
		t.Fatalf("answered for %d releases, want 3", len(states))
	}
	// One ServiceList is one snapshot: the fake records a filter per call.
	if n := len(api.labelFilters); n != 1 {
		t.Errorf("listed services %d times for three releases, want 1", n)
	}
}

// -------------------------------- the per-application gate (#64)

// mountsASecret and mountsAConfig are the ordinary way a chart names something
// an operator created on the swarm: `external: true`, and a bare name that
// resolves against the cluster-wide store with no namespace on the reference.
//
// That reference is what #64 was filed about. Nothing scoped which of them an
// application could make, so every application on a swarm could name every other
// application's secrets — and a Swarm secret is the shape a database password, a
// registry credential and a signing key all arrive in.
func mountsASecret(name string) string {
	return "services:\n  app:\n    image: busybox\n    secrets: [" + name + "]\n" +
		"secrets:\n  " + name + ":\n    external: true\n"
}

func mountsAConfig(name string) string {
	return "services:\n  app:\n    image: busybox\n    configs: [" + name + "]\n" +
		"configs:\n  " + name + ":\n    external: true\n"
}

// The three cases per kind: permitted deploys, unpermitted is refused, and an
// entry naming something else does not help.
//
// The declared form is here as its own case because it is a different code path
// and #86 is the proof that it has to be: a manifest can put any name it likes
// on a resource it declares, conversion resolves the reference to that name, and
// the reference check then correctly sees a stack minding its own business while
// applySecrets adopts the real thing.
func TestAStackMayReferenceOnlyWhatItsApplicationPermits(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, field string
		permitted, wrong      application.Allow
	}{
		{
			"a secret it references", mountsASecret("shared-apikey"), "allow.secrets",
			application.Allow{Secrets: []string{"shared-apikey"}},
			application.Allow{Secrets: []string{"another-apikey"}},
		},
		{
			"a config it references", mountsAConfig("shared-site"), "allow.configs",
			application.Allow{Configs: []string{"shared-site"}},
			application.Allow{Configs: []string{"another-site"}},
		},
		{
			"a secret it declares under another stack's name", stealsByDeclaring("secrets", "shared-apikey", true), "allow.secrets",
			application.Allow{Secrets: []string{"shared-apikey"}},
			application.Allow{Secrets: []string{"another-apikey"}},
		},
		{
			"a config it declares under another stack's name", stealsByDeclaring("configs", "shared-conf", true), "allow.configs",
			application.Allow{Configs: []string{"shared-conf"}},
			application.Allow{Configs: []string{"another-conf"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The referenced resource exists, because the conversion that is
			// applied resolves it to an id. The declared case creates its own.
			existing := func() *fakeAPI {
				return asController(&fakeAPI{
					secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "shared-apikey"}}}},
					configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "shared-site"}}}},
				})
			}

			if err := allowing(t, existing(), tc.permitted).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever, Files: decoyFiles}); err != nil {
				t.Fatalf("DeployStack = %v, want the permitted reference deployed", err)
			}

			for _, refused := range []struct {
				why   string
				allow application.Allow
			}{
				{"permitted nothing", application.Allow{}},
				{"permitted another name", tc.wrong},
			} {
				t.Run(refused.why, func(t *testing.T) {
					api := existing()

					err := allowing(t, api, refused.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever, Files: decoyFiles})
					if err == nil {
						t.Fatal("DeployStack = nil, want the reference refused")
					}
					if !strings.Contains(err.Error(), tc.field) {
						t.Errorf("error %q does not name the field the permission would go in", err)
					}
					if len(api.created) != 0 || len(api.order) != 0 {
						t.Errorf("resources were created despite the refusal: order=%v created=%d", api.order, len(api.created))
					}
				})
			}
		})
	}
}

// ownsOneOfEach declares a network, a config, a secret and a volume and uses
// each, so every name it reaches for is one conversion scoped under the release.
const ownsOneOfEach = `
services:
  web:
    image: nginx
    networks: [front]
    configs: [site]
    secrets: [apikey]
    volumes: ["data:/data"]
networks:
  front: {}
configs:
  site:
    file: files/decoy.conf
secrets:
  apikey:
    driver: vault
volumes:
  data: {}
`

// What the release owns needs no entry at all, which is what keeps the gate from
// being a per-chart inventory: conversion scopes what a stack declares to
// "<release>_<name>", so a chart's own secrets, configs, volumes and networks
// need nobody's permission — on the first deploy, when nothing holds those names
// yet, and on every later one, when what holds them carries the release's label.
// The network is there from the start, labelled as the release's.
func TestAStackNeedsNoPermissionForWhatItOwns(t *testing.T) {
	front := stackNetwork("n", "s_front", "s")
	front.Scope = "swarm"
	api := installed(asController(&fakeAPI{networks: []network.Summary{front}}), "s")
	b := allowing(t, api, application.Allow{})

	for i := range 2 {
		if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: ownsOneOfEach, Resolve: ResolveNever, Files: decoyFiles}); err != nil {
			t.Fatalf("deploy %d = %v, want a release's own resources to need no permission", i+1, err)
		}
	}
	if len(api.createdConfigs) != 1 || len(api.createdSecrets) != 1 || len(api.createdNets) != 0 {
		t.Errorf("created configs %d, secrets %d, networks %d; want the first deploy to create its own config and secret once",
			len(api.createdConfigs), len(api.createdSecrets), len(api.createdNets))
	}
}

// An external: reference is not the release's own whatever it is called: the
// manifest has said the thing is somebody else's, and a name scoped under the
// release is no exception. "web_a_site" is the shape that matters — a release
// name may contain '_', so it is stack "web_a"'s site as much as anything of
// release "web"'s — and "web_site" labelled as the release's own needs the entry
// too, because an external reference is not where a release reaches its own.
func TestAnExternalReferenceNeedsPermissionWhateverItsName(t *testing.T) {
	for _, name := range []struct{ name, owner string }{{"web_a_site", "web_a"}, {"web_site", "web"}} {
		for _, tc := range []struct {
			kind, manifest, field string
			permit                application.Allow
		}{
			{"config", mountsAConfig(name.name), "allow.configs", application.Allow{Configs: []string{name.name}}},
			{"secret", mountsASecret(name.name), "allow.secrets", application.Allow{Secrets: []string{name.name}}},
			{"volume", mountsAVolume(name.name, true), "allow.volumes", application.Allow{Volumes: []string{name.name}}},
			{"network", joinsANetwork(name.name, true), "allow.networks", application.Allow{Networks: []string{name.name}}},
		} {
			t.Run(tc.kind+" "+name.name, func(t *testing.T) {
				api := func() *fakeAPI {
					return asController(&fakeAPI{
						configs:  []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: stackScoped(name.name, name.owner)}}},
						secrets:  []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: stackScoped(name.name, name.owner)}}},
						networks: []network.Summary{stackNetwork("n", name.name, name.owner)},
					})
				}

				refused := api()
				err := allowing(t, refused, application.Allow{}).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: tc.manifest, Resolve: ResolveNever})
				if err == nil || !strings.Contains(err.Error(), tc.field) {
					t.Fatalf("DeployStack = %v, want the reference refused as not permitted", err)
				}
				if len(refused.created) != 0 || len(refused.order) != 0 {
					t.Errorf("resources were created despite the refusal: order=%v created=%d", refused.order, len(refused.created))
				}

				if err := allowing(t, api(), tc.permit).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: tc.manifest, Resolve: ResolveNever}); err != nil {
					t.Errorf("DeployStack = %v, want the reference deployed once the app set permits it", err)
				}
			})
		}
	}
}

// A declared name is the release's own only if whatever already holds it carries
// exactly the release's namespace label. Declaring a config or secret that exists
// hands it to the stack's services and relabels it as the stack's, and joining a
// declared network that exists is joining it, so each of these reaches something
// the release does not own although its name is scoped under the release: a key
// with '_' in it, or a name:, lands on stack "web_a"'s; one made by hand carries
// no label at all; and one labelled "Web" is another stack's, since the label is
// compared as written. The network is held under another case, which Swarm's
// name index folds, so the lookup has to as well.
//
// A config or secret the app set permits is adopted. A network is not adopted
// but created, and Swarm keeps network names unique, so one already held is
// refused whatever the app set permits — joining it is what external: is for.
func TestADeclaredNameHeldByAnotherStackIsNotTheReleasesOwn(t *testing.T) {
	declaresKey := func(kind, key string) string {
		source := map[string]string{"configs": "    file: files/decoy.conf\n", "secrets": "    driver: vault\n", "networks": "    driver: overlay\n"}[kind]
		return "services:\n  app:\n    image: busybox\n    " + kind + ": [" + key + "]\n" + kind + ":\n  " + key + ":\n" + source
	}
	for _, holder := range []struct{ why, owner, name string }{
		{"another stack's", "web_a", "web_a_site"},
		{"made by hand", "", "web_a_site"},
		{"another case", "Web", "web_a_site"},
		// Held under another case of the name, which the daemon's lookup folds:
		// the permitted half is left out, since Swarm refuses the relabel a
		// different spelling would ask for.
		{"another stack's, held in another case", "web_a", "WEB_A_site"},
	} {
		labels := func() map[string]string {
			if holder.owner == "" {
				return nil
			}
			return map[string]string{convert.LabelNamespace: holder.owner}
		}
		for _, tc := range []struct {
			kind, manifest, field string
			permit                application.Allow
			held                  bool
		}{
			{"config by key", declaresKey("configs", "a_site"), "allow.configs", application.Allow{Configs: []string{"web_a_site"}}, false},
			{"config by name", stealsByDeclaring("configs", "web_a_site", true), "allow.configs", application.Allow{Configs: []string{"web_a_site"}}, false},
			{"secret by key", declaresKey("secrets", "a_site"), "allow.secrets", application.Allow{Secrets: []string{"web_a_site"}}, false},
			{"network by key", declaresKey("networks", "a_site"), "allow.networks", application.Allow{Networks: []string{"web_a_site"}}, true},
			{"network by name", joinsANetwork("web_a_site", false), "allow.networks", application.Allow{Networks: []string{"web_a_site"}}, true},
		} {
			t.Run(holder.why+"/"+tc.kind, func(t *testing.T) {
				api := func() *fakeAPI {
					return asController(&fakeAPI{
						configs:  []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: holder.name, Labels: labels()}, Data: decoyFiles["files/decoy.conf"]}}},
						secrets:  []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: holder.name, Labels: labels()}}}},
						networks: []network.Summary{{ID: "n", Name: "WEB_A_site", Scope: "swarm", Labels: labels()}},
					})
				}

				refused := api()
				err := allowing(t, refused, application.Allow{}).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: tc.manifest, Resolve: ResolveNever, Files: decoyFiles})
				if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "web_a_site") {
					t.Fatalf("DeployStack = %v, want the declaration refused as not permitted", err)
				}
				if len(refused.created) != 0 || len(refused.order) != 0 || len(refused.updatedConfigs)+len(refused.updatedSecrets) != 0 {
					t.Errorf("resources were created or relabelled despite the refusal: order=%v created=%d", refused.order, len(refused.created))
				}

				if holder.name != "web_a_site" {
					return
				}
				err = allowing(t, api(), tc.permit).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: tc.manifest, Resolve: ResolveNever, Files: decoyFiles})
				switch {
				case tc.held && (err == nil || !strings.Contains(err.Error(), "already exists")):
					t.Errorf("DeployStack = %v, want a held network refused whatever the app set permits", err)
				case !tc.held && err != nil:
					t.Errorf("DeployStack = %v, want the declaration deployed once the app set permits it", err)
				}
			})
		}
	}
}

// Looking up what holds a declared name is a daemon read, and a daemon that
// could not answer is not the news that nothing holds it: the deploy fails.
func TestADeclaredNameThatCannotBeLookedUpRefusesTheDeploy(t *testing.T) {
	for _, tc := range []struct{ kind, manifest string }{
		{"config", shipsAConfig},
		{"secret", declaresAndMounts},
		{"network", "services:\n  app:\n    image: busybox\n"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			api := &lookupErrAPI{fakeAPI: &fakeAPI{}, kind: tc.kind, err: errors.New("daemon busy")}
			err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: tc.manifest, Resolve: ResolveNever, Files: map[string][]byte{"files/nginx.conf": []byte("x")}})
			if err == nil || !strings.Contains(err.Error(), "daemon busy") {
				t.Fatalf("DeployStack = %v, want the lookup's failure", err)
			}
			if len(api.created) != 0 || len(api.order) != 0 {
				t.Errorf("resources were created despite the failure: order=%v created=%d", api.order, len(api.created))
			}
		})
	}
}

// lookupErrAPI fails the read ownDeclared makes for one kind: a config's or a
// secret's inspect, or the listing of every network. applyNetworks lists by
// label, so its read still answers.
type lookupErrAPI struct {
	*fakeAPI
	kind string
	err  error
}

func (a *lookupErrAPI) ConfigInspectWithRaw(ctx context.Context, name string) (swarm.Config, []byte, error) {
	if a.kind == "config" {
		return swarm.Config{}, nil, a.err
	}
	return a.fakeAPI.ConfigInspectWithRaw(ctx, name)
}

func (a *lookupErrAPI) SecretInspectWithRaw(ctx context.Context, name string) (swarm.Secret, []byte, error) {
	if a.kind == "secret" {
		return swarm.Secret{}, nil, a.err
	}
	return a.fakeAPI.SecretInspectWithRaw(ctx, name)
}

func (a *lookupErrAPI) NetworkList(ctx context.Context, o network.ListOptions) ([]network.Summary, error) {
	if a.kind == "network" && labelOf(o.Filters) == "" {
		return nil, a.err
	}
	return a.fakeAPI.NetworkList(ctx, o)
}

// The invariant that makes the whole design hold: an allowlist cannot reach the
// controller's own. The app-set author is the highest privilege there is here,
// and this is not a thing they may lend — permitting one is not granting an
// application something of the operator's, it is handing whoever writes the
// chart the controller's credentials and with them the app set.
//
// So each entry below names exactly what the chart asks for, and each deploy is
// still refused, with the flat guard's reason rather than the gate's.
func TestNoAllowlistReachesTheControllersOwn(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, why string
		allow               application.Allow
		api                 *fakeAPI
	}{
		{
			"its token", mountsControllerSecret, "controller",
			application.Allow{Secrets: []string{"swarmcli-cd-token"}},
			asController(&fakeAPI{secrets: []swarm.Secret{{ID: "tok", Spec: swarm.SecretSpec{
				Annotations: swarm.Annotations{Name: "swarmcli-cd-token"},
			}}}}),
		},
		{
			"its application set", mountsControllerConfig, "application set",
			application.Allow{Configs: []string{"swarmcli-cd-applications"}},
			asController(&fakeAPI{configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{
				Annotations: swarm.Annotations{Name: "swarmcli-cd-applications"},
			}}}}),
		},
		{
			"its volume", mountsAVolume("swarmcli-cd_swarmcli-cd-data", true), "chart cache",
			application.Allow{Volumes: []string{"swarmcli-cd_swarmcli-cd-data"}},
			asController(&fakeAPI{}),
		},
		{
			"its network", joinsANetwork("swarmcli-cd_default", true), "this controller itself",
			application.Allow{Networks: []string{"swarmcli-cd_default"}},
			asController(&fakeAPI{}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := allowing(t, tc.api, tc.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever})
			if err == nil {
				t.Fatal("DeployStack = nil, want the controller's own refused whatever the app set says")
			}
			if !strings.Contains(err.Error(), tc.why) {
				t.Errorf("error %q is not the flat guard refusing it", err)
			}
			if len(tc.api.created) != 0 || len(tc.api.order) != 0 {
				t.Errorf("resources were created despite the refusal: order=%v created=%d", tc.api.order, len(tc.api.created))
			}
		})
	}
}

// A release record is the same invariant one layer along: it is the engine's,
// not the controller's, and it holds the rendered manifest of every release on
// the swarm.
func TestNoAllowlistReachesAReleaseRecord(t *testing.T) {
	api := asController(&fakeAPI{configs: []swarm.Config{{
		ID:   "rec",
		Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "swarmcli.release.victim.v1"}},
	}}})
	api.configs[0].Spec.Labels = map[string]string{charts.LabelType: charts.TypeRelease}

	err := allowing(t, api, application.Allow{Configs: []string{"swarmcli.release.victim.v1"}}).
		DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsAConfig("swarmcli.release.victim.v1"), Resolve: ResolveNever})
	if err == nil {
		t.Fatal("DeployStack = nil, want a release record refused whatever the app set says")
	}
	if !strings.Contains(err.Error(), "release record") {
		t.Errorf("error %q is not the flat guard refusing it", err)
	}
}

// The permissions are per application, so they must not leak onto the per-swarm
// backend every application shares — the same copy-not-mutate discipline as
// WithRegistryAuth, and here it is the difference between two tenants.
func TestWithAllowedReferencesDoesNotMutate(t *testing.T) {
	shared := testBackend(t, &fakeAPI{}, nil)
	scoped, ok := shared.WithAllowedReferences(application.Allow{Secrets: []string{"shared-apikey"}}).(*Backend)
	if !ok {
		t.Fatal("WithAllowedReferences did not return a *Backend")
	}
	if len(shared.allow.Secrets) != 0 {
		t.Error("WithAllowedReferences mutated the shared backend; a per-swarm backend must permit nothing")
	}
	if len(scoped.allow.Secrets) != 1 {
		t.Errorf("scoped.allow = %+v, want the application's own", scoped.allow)
	}
}

// The backend nobody scoped to an application, which is what the swarms seam
// hands back and what the prune sweep resolves for itself: it permits nothing.
//
// Fail-closed is the property that makes the sweep safe to leave undecorated. A
// sweep removes rather than deploys, so it reaches neither place the allowlist is
// read — but "it never gets there" is a claim about today's call graph, and this
// is the claim about the value itself. If a sweep ever did convert a manifest it
// would refuse rather than wave one through.
func TestAnUndecoratedBackendPermitsNothing(t *testing.T) {
	b := testBackend(t, asController(&fakeAPI{}), nil)

	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsAVolume("shared-cache", true), Resolve: ResolveNever}); err == nil {
		t.Fatal("DeployStack = nil, want a backend nobody scoped to permit nothing")
	}
	// And what a release owns still needs nothing, so "permits nothing" does not
	// mean "refuses everything": an undecorated backend still deploys an ordinary
	// chart.
	if err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: "services:\n  app:\n    image: busybox\n    volumes: [\"data:/data\"]\nvolumes:\n  data: {}\n", Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want a chart that owns everything it names deployed", err)
	}
}

// ---------------------------------------- a self release that goes backwards (#244)

// controllerOnImage is the swarm with a controller on it that names the image it
// is running. controllerService leaves Image empty, which is the honest shape
// for every guard that does not look at it — this one does.
func controllerOnImage(image string) *fakeAPI {
	api := asController(&fakeAPI{})
	api.selfSpec.TaskTemplate.ContainerSpec.Image = image
	return api
}

// selfManifest is bootstrappedStack's shape reduced to what controllerService is
// running with — the socket, which is the one earlier loss this fixture would
// otherwise trip — parameterised on the image, which is what these tests move.
func selfManifest(image string) string {
	return `
services:
  controller:
    image: ` + image + `
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
`
}

// The trap #243 describes, refused. A values file that does not pin `image.tag`
// renders the chart's appVersion, which is a controller that predates the key
// the app set it would inherit is written with — and once it is running, nothing
// left is reading that file to apply the correction.
func TestASelfReleaseIsRefusedAnOlderController(t *testing.T) {
	api := controllerOnImage("eldaratech/swarmcli-cd:1.3.0-rc1-oss")
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)

	err := b.DeployStack(t.Context(), charts.DeployRequest{
		Name: "swarmcli-cd", Manifest: selfManifest("eldaratech/swarmcli-cd:1.2.0"), Resolve: ResolveNever,
	})
	if err == nil {
		t.Fatal("DeployStack = nil, want a self release that downgrades this controller refused")
	}
	for _, want := range []string{"1.2.0", "1.3.0-rc1-oss"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if len(api.created)+len(api.updated) > 0 {
		t.Error("wrote services for a refused self release")
	}
}

// The daemon appends the digest it resolved the running tag to, so the live side
// is never the string the manifest would be compared against as written.
func TestTheRunningControllersDigestDoesNotDefeatTheComparison(t *testing.T) {
	api := controllerOnImage("eldaratech/swarmcli-cd:1.3.0-rc1-oss@sha256:" + strings.Repeat("a", 64))
	b := testBackend(t, api, nil).WithSelfRelease(noDeferral)

	err := b.DeployStack(t.Context(), charts.DeployRequest{
		Name: "swarmcli-cd", Manifest: selfManifest("eldaratech/swarmcli-cd:1.2.0"), Resolve: ResolveNever,
	})
	if err == nil {
		t.Fatal("DeployStack = nil, want the downgrade refused through the digest the daemon resolved")
	}
}

// Upgrading is the whole feature, and it must not be what this guard catches.
func TestASelfReleaseMayMoveThisControllerForward(t *testing.T) {
	for name, to := range map[string]string{
		"a newer version":             "eldaratech/swarmcli-cd:1.3.0",
		"the same version":            "eldaratech/swarmcli-cd:1.2.0",
		"a prerelease of it":          "eldaratech/swarmcli-cd:1.3.0-rc1-oss",
		"another repository":          "my-registry.example.com/swarmcli-cd:0.1.0",
		"a tag that is not a version": "eldaratech/swarmcli-cd:nightly",
	} {
		t.Run(name, func(t *testing.T) {
			api := controllerOnImage("eldaratech/swarmcli-cd:1.2.0")
			b := testBackend(t, api, nil).WithSelfRelease(noDeferral)

			err := b.DeployStack(t.Context(), charts.DeployRequest{
				Name: "swarmcli-cd", Manifest: selfManifest(to), Resolve: ResolveNever,
			})
			if err != nil {
				t.Fatalf("DeployStack = %v, want %s applied", err, name)
			}
		})
	}
}

// And the guard is the self release's alone. Every other application deploys
// whatever image it likes, including this controller's own repository at a
// version older than the one running — it is not being deployed over this
// controller, so there is nothing to be unable to fix.
func TestAnOrdinaryReleaseMayDeployAnOlderImage(t *testing.T) {
	api := controllerOnImage("eldaratech/swarmcli-cd:1.3.0-rc1-oss")

	err := allowing(t, api, application.Allow{HostPaths: []string{"/var/run/docker.sock"}}).DeployStack(t.Context(),
		charts.DeployRequest{Name: "elsewhere", Manifest: selfManifest("eldaratech/swarmcli-cd:1.2.0"), Resolve: ResolveNever})
	if err != nil {
		t.Fatalf("DeployStack = %v, want an ordinary release left alone", err)
	}
}

// What cannot be ordered is said out loud instead, next to the other drops. An
// operator tagging their own builds gets no refusal from this guard, and no
// silence either — the one deploy whose mistake removes the thing that would
// correct it is not a place to be quiet about not having checked.
func TestAnUnorderableSelfImageIsWarnedAboutRatherThanRefused(t *testing.T) {
	api := controllerOnImage("eldaratech/swarmcli-cd:1.2.0")
	var log bytes.Buffer
	b := New(api, Options{Log: slog.New(slog.NewTextHandler(&log, nil))}).WithSelfRelease(noDeferral)

	err := b.DeployStack(t.Context(), charts.DeployRequest{
		Name: "swarmcli-cd", Manifest: selfManifest("eldaratech/swarmcli-cd:nightly"), Resolve: ResolveNever,
	})
	if err != nil {
		t.Fatalf("DeployStack = %v, want an image that cannot be ordered applied", err)
	}
	if !strings.Contains(log.String(), "cannot be ordered") {
		t.Errorf("nothing was logged about an image the guard could not read:\n%s", log.String())
	}
}

// And an unchanged reference says nothing. A self release redeploys on every
// drift correction, so a controller pinned by digest would otherwise be told the
// same thing about the same image for ever.
func TestAnUnchangedSelfImageIsNotWarnedAbout(t *testing.T) {
	const pinned = "eldaratech/swarmcli-cd@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	api := controllerOnImage(pinned)
	var log bytes.Buffer
	b := New(api, Options{Log: slog.New(slog.NewTextHandler(&log, nil))}).WithSelfRelease(noDeferral)

	err := b.DeployStack(t.Context(), charts.DeployRequest{
		Name: "swarmcli-cd", Manifest: selfManifest(pinned), Resolve: ResolveNever,
	})
	if err != nil {
		t.Fatalf("DeployStack = %v, want the unchanged image applied", err)
	}
	if strings.Contains(log.String(), "cannot be ordered") {
		t.Errorf("an unchanged image was warned about:\n%s", log.String())
	}
}
