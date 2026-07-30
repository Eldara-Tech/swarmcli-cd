// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
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
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli/charts"

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

	if err := testBackend(t, api, nil).DeployStack("s", oneOfEach, ResolveNever); err != nil {
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
// config and secret and mounts them. Both sources are files, because that is the
// only way compose carries content, and the loader resolves them against this
// process's filesystem.
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

// A chart may mount what it declares. Converting a service resolves each config
// and secret it mounts to the id Swarm addresses it by, so the conversion that
// is applied cannot run until they exist — which is the ordering #84 got wrong.
//
// The id asserted at the end is the point of the whole arrangement: it is the
// one the daemon reported for the config this deploy created, so the spec that
// reached the swarm came from the authoritative conversion and not from the
// reference-free one the guard reads.
func TestDeployStackCreatesAConfigAndSecretItThenMounts(t *testing.T) {
	api := &fakeAPI{}

	if err := testBackend(t, api, nil).DeployStack("rel", declaresAndMounts(t), ResolveNever); err != nil {
		t.Fatalf("DeployStack = %v, want a chart to be able to mount what it declares", err)
	}

	want := []string{"network:rel_default", "secret:rel_apikey", "config:rel_site"}
	if !reflect.DeepEqual(api.order, want) {
		t.Errorf("mutation order = %v, want %v", api.order, want)
	}
	if len(api.created) != 1 {
		t.Fatalf("created %d services, want 1", len(api.created))
	}
	cs := api.created[0].TaskTemplate.ContainerSpec
	if len(cs.Configs) != 1 || cs.Configs[0].ConfigName != "rel_site" || cs.Configs[0].ConfigID != "cfg-rel_site" {
		t.Errorf("configs = %+v, want the id the daemon reported for the config just created", cs.Configs)
	}
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretName != "rel_apikey" || cs.Secrets[0].SecretID != "sec-rel_apikey" {
		t.Errorf("secrets = %+v, want the id the daemon reported for the secret just created", cs.Secrets)
	}
}

// A manifest that cannot be converted at all creates nothing. The stack is
// refused whole for every conversion failure and not only for a forbidden mount,
// which is what the reference-free first pass buys: it fails here, before the
// first create, rather than after the configs and secrets are on the swarm.
func TestAnUnconvertibleManifestCreatesNothing(t *testing.T) {
	api := &fakeAPI{}

	err := testBackend(t, api, nil).DeployStack("s", `
services:
  web:
    image: nginx
    secrets: [absent]
`, ResolveNever)
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

	err := b.DeployStack("s", mountsControllerSecret, ResolveNever)
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
			st := stack("s", cdService{"svc", tc.svc})

			err := b.rejectForbiddenMounts(t.Context(), st)
			if tc.wantErr && err == nil {
				t.Fatal("rejectForbiddenMounts = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("rejectForbiddenMounts = %v, want nil", err)
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

	if err := testBackend(t, api, nil).RemoveStack("s"); err != nil {
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

// One list call, not a list plus an inspect per config. This runs on every
// reconcile against a store that grows by one config per release revision.
//
// The payload has to ride along, or the engine inspects each release config to
// decode it and the saving is undone one revision at a time.
func TestListConfigsDoesNotInspectEachOne(t *testing.T) {
	api := &fakeAPI{configs: []swarm.Config{
		{Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "a", Labels: map[string]string{"k": "v"}}, Data: []byte("payload-a")}},
		{Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "b"}}},
	}}

	got, err := testBackend(t, api, nil).ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs = %v, want nil", err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[0].Labels["k"] != "v" {
		t.Errorf("configs = %+v, want name and labels carried through", got)
	}
	if string(got[0].Data) != "payload-a" {
		t.Errorf("Data = %q, want the payload the list response carried", got[0].Data)
	}
	if api.inspects != 0 {
		t.Errorf("inspected %d configs; the list response already holds all of it", api.inspects)
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

	got := testBackend(t, api, nil).StackServices("s")
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
	if err := testBackend(t, &fakeAPI{}, nil).RefreshSnapshot(); err != nil {
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
		{"DeployStack", func(b *Backend) error { return b.DeployStack("s", "services:\n  web:\n    image: x\n", ResolveNever) }},
		{"RemoveStack", func(b *Backend) error { return b.RemoveStack("s") }},
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
	if got := testBackend(t, &errAPI{err: errors.New("daemon unreachable")}, nil).StackServices("s"); got != nil {
		t.Errorf("StackServices = %v, want nil", got)
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

	if err := testBackend(t, api, nil).RemoveStack("s"); err != nil {
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
	if err := testBackend(t, api, nil).RemoveStack(""); err == nil {
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

			if err := testBackend(t, api, nil).RemoveStack("s"); err != nil {
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

	if err := testBackend(t, api, nil).RemoveStack("s"); err != nil {
		t.Fatalf("RemoveStack = %v, want nil — the stack is gone, whatever the daemon said", err)
	}
}

// Release-history configs stay behind on purpose, so the re-check must not read
// them as "the stack is still here" and turn every removal into a failure.
func TestRemoveStackIgnoresReleaseRecordsWhenRechecking(t *testing.T) {
	api := &fakeAPI{
		networks: []network.Summary{stackNetwork("net", "s_front", "s")},
		configs: []swarm.Config{{ID: "rel", Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{
				Name:   "swarmcli.release.s.v1",
				Labels: map[string]string{charts.LabelType: charts.TypeRelease},
			},
		}}},
		removeErr:  map[string]error{"network:net": errors.New("network net not found")},
		removeGone: map[string]bool{"network:net": true},
	}

	if err := testBackend(t, api, nil).RemoveStack("s"); err != nil {
		t.Fatalf("RemoveStack = %v, want nil; the release record is not the stack", err)
	}
}

// A real failure still fails. The tolerance above is for "it is already gone",
// not for "the daemon refused".
func TestRemoveStackStillFailsOnARealError(t *testing.T) {
	api := &fakeAPI{
		networks:  []network.Summary{stackNetwork("net", "s_front", "s")},
		removeErr: map[string]error{"network:net": errors.New("has active endpoints")},
	}

	err := testBackend(t, api, nil).RemoveStack("s")
	if err == nil || !strings.Contains(err.Error(), "has active endpoints") {
		t.Fatalf("RemoveStack = %v, want the daemon's refusal surfaced", err)
	}
}

// stackScoped is a resource as a real deploy leaves it: carrying the namespace
// label, which is the whole of "this belongs to that stack" and the only thing
// RemoveStack has to find it by.
func stackScoped(name, stack string) swarm.Annotations {
	return swarm.Annotations{
		Name:   name,
		Labels: map[string]string{convert.LabelNamespace: stack},
	}
}

// stackNetwork is the same thing for a network, which carries its labels on the
// summary rather than in annotations.
func stackNetwork(id, name, stack string) network.Summary {
	return network.Summary{
		ID:     id,
		Name:   name,
		Labels: map[string]string{convert.LabelNamespace: stack},
	}
}

// ---------------------------------------- the controller's own state (#63)

// controllerService is this controller as Swarm holds it: mounting its own
// bootstrap config and its admin token, under the names a reference resolves by.
//
// The target rename on the secret is the point. MountedSecretNames would derive
// "token" from /run/secrets and never match the "swarmcli-cd-token" a tenant
// stack would actually name, so the filesystem-derived guard has a hole exactly
// here — and reading the service spec closes it.
func controllerService() swarm.ServiceSpec {
	return swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
		Configs: []*swarm.ConfigReference{{
			ConfigName: "swarmcli-cd-applications",
			File:       &swarm.ConfigReferenceFileTarget{Name: "/etc/swarmcli-cd/applications.yaml"},
		}},
		Secrets: []*swarm.SecretReference{{
			SecretName: "swarmcli-cd-token",
			File:       &swarm.SecretReferenceFileTarget{Name: "token"},
		}},
	}}}
}

// asController makes the fake answer as though this process is that service's
// task, which is what SelfMounts reads.
func asController(api *fakeAPI) *fakeAPI {
	api.selfServiceID = "controller-svc"
	api.selfSpec = controllerService()
	return api
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

	err := testBackend(t, api, nil).DeployStack("s", mountsControllerConfig, ResolveNever)
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

	err := testBackend(t, api, nil).DeployStack("s", mountsAReleaseRecord, ResolveNever)
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

	err := testBackend(t, api, nil).DeployStack("s", mountsControllerSecret, ResolveNever)
	if err == nil {
		t.Fatal("DeployStack = nil, want the stack refused for mounting the controller's token")
	}
	if !strings.Contains(err.Error(), "swarmcli-cd-token") {
		t.Errorf("error %q does not name the secret", err)
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

	if err := testBackend(t, api, nil).DeployStack("s", oneOfEach, ResolveNever); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}
}

// A stack that declares everything it uses reaches outside itself for nothing,
// so the guard must cost it no daemon calls at all.
func TestAStackThatReachesForNothingCostsNoLookup(t *testing.T) {
	const selfContained = `
services:
  web:
    image: nginx
`
	api := asController(&fakeAPI{})
	if err := testBackend(t, api, nil).DeployStack("s", selfContained, ResolveNever); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}
	if api.selfInspects != 0 {
		t.Errorf("asked about this controller %d times, want 0", api.selfInspects)
	}
}

// Read once and reused: every With* method copies the backend, and a per-copy
// cache would re-read this for every application on every deploy.
func TestTheControllersOwnMountsAreReadOnce(t *testing.T) {
	api := asController(&fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	})
	b := testBackend(t, api, nil)

	for range 3 {
		if err := b.WithRegistryAuth(nil).DeployStack("s", oneOfEach, ResolveNever); err != nil {
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

	if err := b.DeployStack("s", oneOfEach, ResolveNever); err == nil {
		t.Fatal("DeployStack = nil, want the deploy refused rather than run unguarded")
	}
	if len(api.created) != 0 {
		t.Errorf("%d services created despite the failure", len(api.created))
	}

	api.selfErr = nil
	if err := b.DeployStack("s", oneOfEach, ResolveNever); err != nil {
		t.Fatalf("second DeployStack = %v, want the failure not to have been cached", err)
	}
}

// A controller that is not a swarm task — a development run — has nothing
// mounted by Swarm, which is an answer rather than a failure.
func TestOutsideASwarmThereIsNothingOfOursToProtect(t *testing.T) {
	api := &fakeAPI{
		configs: []swarm.Config{{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_apikey"}}}},
	}
	if err := testBackend(t, api, nil).DeployStack("s", oneOfEach, ResolveNever); err != nil {
		t.Fatalf("DeployStack = %v, want nil", err)
	}
}
