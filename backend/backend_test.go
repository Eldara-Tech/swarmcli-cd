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

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
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

// A service can reference a network, config or secret, so each has to exist
// before the service does. Getting the order wrong produces a create that fails
// on a reference the next call would have satisfied.
func TestDeployStackCreatesReferencesBeforeServices(t *testing.T) {
	api := &fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	}

	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
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
// secret and mounts it. Driver-backed, because since #99 that is the whole of
// what a stack can own — the other way was a path, and compose resolves a path
// against the controller's filesystem rather than the chart's.
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

// DeployRequest.Files is ignored, and this asserts that it is ignored on purpose
// rather than dropped by accident. Since #99 no manifest reaching this backend
// can name a file — configs.*.file, secrets.*.file and services.*.env_file are
// all refused before the loader runs — so a request carrying files describes a
// chart whose files nothing here will ever read. Two things follow, and both are
// checked: the deploy proceeds exactly as it would without them, and the bytes
// are not written anywhere. The temp directory is the probe for the second
// because it is where materialising them would put them — CE's own
// docker.DeployStackInContext writes its manifest there — so an empty one after
// the deploy is the evidence that this method did nothing with the field.
func TestDeployStackIgnoresTheChartsFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	api := &fakeAPI{}

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{
		Name:     "rel",
		Manifest: declaresAndMounts,
		Resolve:  ResolveNever,
		Files:    map[string][]byte{"files/nginx.conf": []byte("server {}\n")},
	})
	if err != nil {
		t.Fatalf("DeployStack = %v, want a request carrying files deployed like any other", err)
	}

	if len(api.created) != 1 || api.created[0].Name != "rel_app" {
		t.Errorf("created services %+v, want the deploy to have run as it does without files", api.created)
	}
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d entries under %s, want the files ignored rather than materialised", len(left), tmp)
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
	// is the forbidden set this test is about. "s_apikey" needs no entry: it is
	// scoped to the release being deployed, which is nobody's permission to give.
	permitted := application.Allow{Secrets: []string{"swarmcli-cd-token"}}

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

func TestStackVolumesAreScopedAndSorted(t *testing.T) {
	api := &fakeAPI{volumes: []volume.Volume{{Name: "zeta"}, {Name: "alpha"}}}

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
	api := &fakeAPI{}
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

// stealsByDeclaring is a manifest that takes a secret over instead of
// referencing it: the entry is not `external:`, so it is one of the stack's own
// as far as conversion and the guard's reference check are concerned, and `name:`
// points it at something that already exists.
//
// Driver-backed, and secrets only. This fixture used to carry a `file:` and to
// take a kind, which is how it covered the config half of the same rule too —
// but #99 refuses a path before the guard ever runs, and a config has no
// `driver:` to fall back on, so a manifest can no longer express a stack-owned
// config at all. The two config cases below therefore address the guard
// directly.
func stealsByDeclaring(name string, mount bool) string {
	svc := "services:\n  thief:\n    image: alpine\n"
	if mount {
		svc += "    secrets: [x]\n"
	}
	return svc + "secrets:\n  x:\n    name: " + name + "\n    driver: vault\n"
}

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

	err := b.DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: stealsByDeclaring("swarmcli-cd-token", true), Resolve: ResolveNever})
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

	err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: stealsByDeclaring("swarmcli-cd-token", false), Resolve: ResolveNever})
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
//
// It reaches the guard directly rather than through DeployStack because #99
// closed the only route a manifest had to a stack-owned config: content came
// from `file:`, a config has no `driver:` to declare it without content, and an
// `external:` entry is a reference rather than a declaration. Put back through
// DeployStack it would be refused for the file source and prove nothing about
// this rule. The rule stays under test because the route can reopen — it is
// compose's inability to carry content that shuts it, not anything here.
func TestDeployStackRefusesAStackDeclaringTheControllersOwnConfig(t *testing.T) {
	api := asController(&fakeAPI{configs: []swarm.Config{{
		ID:   "app-set",
		Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "swarmcli-cd-applications"}, Data: []byte("real")},
	}}})
	st := stack("tenant")
	st.Configs = []swarm.ConfigSpec{{Annotations: swarm.Annotations{Name: "swarmcli-cd-applications"}}}

	err := testBackend(t, api, nil).rejectForbiddenResources(t.Context(), st)
	if err == nil {
		t.Fatal("rejectForbiddenResources = nil, want the stack refused for declaring the controller's own config")
	}
	if !strings.Contains(err.Error(), "declares") || !strings.Contains(err.Error(), "controller") {
		t.Errorf("error %q does not say what was refused", err)
	}
	if len(api.order) != 0 {
		t.Errorf("resources were created despite the refusal: %v", api.order)
	}
}

// A release record, taken the same way. Not immutability's business at all: the
// record it names does not exist yet, so nothing would have refused this.
//
// Through the guard rather than through DeployStack for the reason above, and
// this is the one the integration suite used to prove end to end — see the note
// where TestAStackMayNotClaimAReleaseRecordAsItsOwn was. Do not "restore" the
// DeployStack form: no manifest can declare a config since #99, so it would fail
// on the file-source refusal and prove nothing about this rule.
func TestDeployStackRefusesAStackDeclaringAReleaseRecordName(t *testing.T) {
	api := asController(&fakeAPI{configs: []swarm.Config{{
		ID: "rec",
		Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
			Name:   "swarmcli.release.other-app.v3",
			Labels: map[string]string{charts.LabelType: charts.TypeRelease},
		}},
	}}})
	st := stack("tenant")
	st.Configs = []swarm.ConfigSpec{{Annotations: swarm.Annotations{Name: "swarmcli.release.other-app.v3"}}}

	err := testBackend(t, api, nil).rejectForbiddenResources(t.Context(), st)
	if err == nil {
		t.Fatal("rejectForbiddenResources = nil, want the stack refused for declaring a release record's name")
	}
	if !strings.Contains(err.Error(), "release record") {
		t.Errorf("error %q does not say what was refused", err)
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
// created, and refusing those would make the guard unusable. Only the
// controller's own and the engine's own are off limits.
func TestDeployStackAllowsAnOrdinaryExternalConfig(t *testing.T) {
	api := asController(&fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	})

	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
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
// process, which is what TestTheControllersOwnMountsAreReadOnce pins.
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
	b := testBackend(t, api, nil)

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
	b := testBackend(t, api, nil)

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
	b := testBackend(t, api, nil)

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
	if err := testBackend(t, api, nil).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
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
	b := testBackend(t, api, nil)

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
// operator ran `docker secret create s_apikey`, and a chart then declared it.
// The apply adopts it — relabels it into the stack's namespace, which is what
// makes it a sweep candidate — and must not claim to have created it.
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
	api := &fakeAPI{removeErr: map[string]error{"volume:s_data": errdefs.ErrNotFound}}

	if err := testBackend(t, api, nil).RemoveVolume(context.Background(), "s_data"); err != nil {
		t.Errorf("RemoveVolume = %v, want nil for one already gone", err)
	}
}

func TestRemoveVolumeSurfacesARefusal(t *testing.T) {
	api := &fakeAPI{removeErr: map[string]error{"volume:s_data": errors.New("volume is in use")}}

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
			api := asController(&fakeAPI{})

			err := allowing(t, api, tc.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: mountsAVolume("shared-cache", true), Resolve: ResolveNever})
			if err == nil {
				t.Fatal("DeployStack = nil, want the volume refused")
			}
			for _, want := range []string{"thief", "shared-cache", "allow.volumes"} {
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
			"a secret it declares under another stack's name", stealsByDeclaring("shared-apikey", true), "allow.secrets",
			application.Allow{Secrets: []string{"shared-apikey"}},
			application.Allow{Secrets: []string{"another-apikey"}},
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

			if err := allowing(t, existing(), tc.permitted).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever}); err != nil {
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

					err := allowing(t, api, refused.allow).DeployStack(t.Context(), charts.DeployRequest{Name: "tenant", Manifest: tc.manifest, Resolve: ResolveNever})
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

// What the release owns needs no entry at all, which is what keeps the gate from
// being a per-chart inventory: conversion scopes everything a stack declares to
// "<release>_<name>", so a chart's own secrets, configs, volumes and networks are
// nobody else's and nobody's to permit.
//
// The external references here are scoped names too — the shape oneOfEach uses —
// because a reference to something inside your own release is still your own.
func TestAStackNeedsNoPermissionForWhatItOwns(t *testing.T) {
	api := asController(&fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	})

	if err := allowing(t, api, application.Allow{}).DeployStack(t.Context(), charts.DeployRequest{Name: "s", Manifest: oneOfEach, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack = %v, want a release's own resources to need no permission", err)
	}
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
