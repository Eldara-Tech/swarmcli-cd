// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	api := &fakeAPI{networks: []network.Summary{{
		ID: "n", Name: "s_front", Driver: "overlay", Attachable: false,
	}}}
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
		configs:  []swarm.Config{{ID: "cfg", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}}},
		secrets:  []swarm.Secret{{ID: "sec", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "s_key"}}}},
		networks: []network.Summary{{ID: "net", Name: "s_front"}},
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
func TestListConfigsDoesNotInspectEachOne(t *testing.T) {
	api := &fakeAPI{configs: []swarm.Config{
		{Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "a", Labels: map[string]string{"k": "v"}}}},
		{Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "b"}}},
	}}

	got, err := testBackend(t, api, nil).ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs = %v, want nil", err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[0].Labels["k"] != "v" {
		t.Errorf("configs = %+v, want name and labels carried through", got)
	}
	if api.inspects != 0 {
		t.Errorf("inspected %d configs; the engine reads only name and labels", api.inspects)
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
		{ID: "app", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "s_site"}}},
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
