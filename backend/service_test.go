// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli/charts"

	cdcompose "github.com/Eldara-Tech/swarmcli-cd/compose"
)

// fakeAPI records what was asked of the daemon and replays scripted answers.
//
// client.APIClient is embedded nil deliberately: anything this package reaches
// for beyond the four methods below panics naming the method. That is how a test
// notices a new daemon call — in particular a delete, which Phase 1 must never
// make.
type fakeAPI struct {
	client.APIClient

	existing []swarm.Service
	// updateErrs is consumed one per ServiceUpdate call; a nil entry succeeds.
	updateErrs []error
	inspectErr error
	listErr    error

	created   []swarm.ServiceSpec
	createOpt []swarm.ServiceCreateOptions
	updated   []updateCall
	inspects  int

	// --- resources ---
	networks   []network.Summary
	configs    []swarm.Config
	secrets    []swarm.Secret
	volumes    []volume.Volume
	nodes      []swarm.Node
	tasks      []swarm.Task
	networkErr error
	// nodeErr fails the node listing, which is what a worker node answers: only
	// a manager can enumerate the swarm.
	nodeErr error
	// removeErr fails one removal, keyed as the removed slice records it
	// ("network:net"). By default the resource survives the failure, which is a
	// genuine refusal.
	removeErr map[string]error
	// removeGone marks a failed removal whose resource is nevertheless no
	// longer there — the daemon's reply for something that had already gone,
	// which does not reliably arrive classified as not-found.
	removeGone map[string]bool

	// labelFilters records the label filter of every list call, so a test can
	// assert that a stack-scoped operation was actually scoped.
	labelFilters []string
	// order records every mutation in the order it was made.
	order          []string
	createdNets    map[string]network.CreateOptions
	createdConfigs []swarm.ConfigSpec
	createdSecrets []swarm.SecretSpec
	updatedConfigs []swarm.ConfigSpec
	updatedSecrets []swarm.SecretSpec
	removed        []string

	// selfServiceID is what ContainerInspect reports this process's container
	// belongs to, and selfSpec the service spec ServiceInspectWithRaw then
	// returns for it — the two reads the mount guard makes about the controller
	// itself. Unset means "not a swarm task", which is what every test that is
	// not about that guard wants and what a development run really is.
	selfServiceID string
	selfSpec      swarm.ServiceSpec
	// selfSpecErr fails the second of those two reads instead. It is a separate
	// round trip, so the daemon that answered the first one can be gone by it.
	selfSpecErr error
	// selfInspects counts the reads about this controller's own container, so a
	// test can assert both that the answer is cached and that a stack reaching
	// for nothing outside itself never asks at all.
	selfInspects int
	selfErr      error
	// inspectedIDs records the id of every container inspect, because the id is
	// the guard's whole premise rather than a detail of the call.
	inspectedIDs []string
}

// ContainerInspect answers for this process's own container. Not-found unless a
// test says otherwise, so nothing else has to know the guard exists.
//
// It answers for that container and no other. The id the guard sends is
// os.Hostname(), which in a container is the container id — that is the entire
// reason a controller can find itself from the inside, and a daemon asked about
// anything else replies not-found, which readSelfMounts reads as "not a swarm
// task" and therefore as no guard at all. A fake that discarded the id would
// answer for any string, so the guard would pass its tests while asking about
// the wrong container, which is the one way it can fail silently and completely.
func (f *fakeAPI) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	f.selfInspects++
	f.inspectedIDs = append(f.inspectedIDs, id)
	if f.selfErr != nil {
		return container.InspectResponse{}, f.selfErr
	}
	host, err := os.Hostname()
	if err != nil {
		return container.InspectResponse{}, err
	}
	if f.selfServiceID == "" || id != host {
		return container.InspectResponse{}, errdefs.ErrNotFound
	}
	return container.InspectResponse{Config: &container.Config{
		Labels: map[string]string{serviceIDLabel: f.selfServiceID},
	}}, nil
}

type updateCall struct {
	id      string
	version swarm.Version
	spec    swarm.ServiceSpec
	opts    swarm.ServiceUpdateOptions
}

// ClientVersion is what compose conversion reads to gate version-dependent
// spec fields.
func (f *fakeAPI) ClientVersion() string { return "1.51" }

// The context is honoured, as the real client's is. A fake that ignored it would
// let a caller reading the swarm on an uncancellable context ship green — which
// is exactly what happened before charts.Backend took one.
func (f *fakeAPI) ServiceList(ctx context.Context, o swarm.ServiceListOptions) ([]swarm.Service, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.labelFilters = append(f.labelFilters, labelOf(o.Filters))
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.existing, nil
}

// ServiceCreate stores what it created, so a later read finds it — as a daemon
// would, and for the reason ConfigCreate does. Without it there is no
// unit-level "deploy, then read back what the daemon holds": every apply starts
// from an empty swarm, so the create-or-update decision is only ever tested on
// the branch a fixture hands it, and nothing notices an applier that created the
// same service twice.
func (f *fakeAPI) ServiceCreate(_ context.Context, spec swarm.ServiceSpec, opts swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	f.created = append(f.created, spec)
	f.createOpt = append(f.createOpt, opts)
	id := "new-" + spec.Name
	f.existing = append(f.existing, swarm.Service{ID: id, Spec: spec})
	return swarm.ServiceCreateResponse{ID: id}, nil
}

func (f *fakeAPI) ServiceUpdate(_ context.Context, id string, v swarm.Version, spec swarm.ServiceSpec, opts swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error) {
	f.updated = append(f.updated, updateCall{id: id, version: v, spec: spec, opts: opts})
	if n := len(f.updated) - 1; n < len(f.updateErrs) {
		return swarm.ServiceUpdateResponse{}, f.updateErrs[n]
	}
	return swarm.ServiceUpdateResponse{}, nil
}

func (f *fakeAPI) ServiceInspectWithRaw(_ context.Context, id string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	if f.selfServiceID != "" && id == f.selfServiceID {
		if f.selfSpecErr != nil {
			return swarm.Service{}, nil, f.selfSpecErr
		}
		return swarm.Service{ID: id, Spec: f.selfSpec}, nil, nil
	}
	f.inspects++
	if f.inspectErr != nil {
		return swarm.Service{}, nil, f.inspectErr
	}
	for _, s := range f.existing {
		if s.ID == id {
			// A fresh read carries a newer version, as a real one would.
			s.Version.Index += uint64(f.inspects)
			return s, nil, nil
		}
	}
	return swarm.Service{}, nil, errors.New("no such service")
}

// outOfSequence is what swarmkit actually returns when ?version= is stale. It
// arrives as gRPC InvalidArgument, which the daemon renders as 400 rather than
// 409 — which is why matching it needs the message.
func outOfSequence() error {
	return errors.New("Error response from daemon: rpc error: code = Unknown desc = update out of sequence")
}

func testBackend(t *testing.T, api client.APIClient, onConflict func(string)) *Backend {
	t.Helper()
	return New(api, Options{
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnOutOfBandChange: onConflict,
	})
}

// stack builds a converted stack directly, so these tests exercise applying
// rather than converting.
func stack(namespace string, services ...cdService) *cdcompose.Stack {
	s := &cdcompose.Stack{Namespace: convert.NewNamespace(namespace)}
	for _, svc := range services {
		spec := svc.spec
		spec.Name = s.Namespace.Scope(svc.name)
		s.Services = append(s.Services, cdcompose.Service{Name: svc.name, Spec: spec})
	}
	return s
}

type cdService struct {
	name string
	spec swarm.ServiceSpec
}

// installed makes the fake answer as though this controller installed the
// release: one release-history config, written the way the engine writes them —
// com.swarmcli.* labels and no stack namespace.
//
// Every test whose subject is *updating* a service needs it, because that is the
// proof the write path asks for before it overwrites something already running
// under a stack's namespace. A namespace with services in it and no record of
// ours is somebody else's stack (#102).
func installed(api *fakeAPI, release string) *fakeAPI {
	api.configs = append(api.configs, swarm.Config{
		ID: "rec-" + release,
		Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
			Name: "swarmcli.release." + release + ".v1",
			Labels: map[string]string{
				charts.LabelType:    charts.TypeRelease,
				charts.LabelRelease: release,
			},
		}},
	})
	return api
}

// deployed builds a service as it would come back off the swarm: the resolved
// image in the spec, and the tag the manifest asked for in the stack label.
func deployed(name, tag, resolved string, version uint64) swarm.Service {
	return swarm.Service{
		ID:   "id-" + name,
		Meta: swarm.Meta{Version: swarm.Version{Index: version}},
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{
				Name:   name,
				Labels: map[string]string{convert.LabelImage: tag, convert.LabelNamespace: "s"},
			},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: resolved}},
		},
	}
}

func spec(image string) swarm.ServiceSpec {
	return swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Labels: map[string]string{convert.LabelImage: image}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: image}},
	}
}

func TestCreatesServicesThatDoNotExist(t *testing.T) {
	api := &fakeAPI{}
	st := stack("s", cdService{"web", spec("nginx")}, cdService{"db", spec("postgres")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}

	if len(api.created) != 2 || len(api.updated) != 0 {
		t.Fatalf("created %d, updated %d; want 2 and 0", len(api.created), len(api.updated))
	}
	// In the order the stack listed them, which is the order conversion sorted.
	if got := []string{api.created[0].Name, api.created[1].Name}; !reflect.DeepEqual(got, []string{"s_web", "s_db"}) {
		t.Errorf("created %v, want the stack's own order and scoped names", got)
	}
}

// The create-or-update decision made against what the daemon holds, rather than
// against a fixture that decided the answer in advance.
//
// Every other test here starts from an empty swarm or from a hand-written live
// service, so each exercises one branch with the other's outcome impossible. A
// second apply against the first one's result is the only shape in which the
// branch is chosen: an applier that could not find what it had just created
// would create it again on every reconcile, and the daemon would answer "name
// conflicts with an existing object" forever.
func TestASecondApplyUpdatesWhatTheFirstCreated(t *testing.T) {
	api := installed(&fakeAPI{}, "s")
	b := testBackend(t, api, nil)
	ctx := context.Background()

	if err := b.ApplyServices(ctx, stack("s", cdService{"web", spec("nginx:1.1")}), ResolveNever); err != nil {
		t.Fatalf("first ApplyServices = %v, want nil", err)
	}
	if err := b.ApplyServices(ctx, stack("s", cdService{"web", spec("nginx:1.2")}), ResolveNever); err != nil {
		t.Fatalf("second ApplyServices = %v, want nil", err)
	}

	if len(api.created) != 1 {
		t.Errorf("created %d services, want the second apply to have found the first's", len(api.created))
	}
	if len(api.updated) != 1 {
		t.Fatalf("updated %d services, want 1", len(api.updated))
	}
	if api.updated[0].id != "new-s_web" {
		t.Errorf("updated %q, want the id the create returned", api.updated[0].id)
	}
	if got := api.updated[0].spec.TaskTemplate.ContainerSpec.Image; got != "nginx:1.2" {
		t.Errorf("image = %q, want the second manifest's", got)
	}
}

func TestUpdatesServicesThatExist(t *testing.T) {
	api := installed(&fakeAPI{existing: []swarm.Service{deployed("s_web", "nginx:1.1", "nginx:1.1", 7)}}, "s")
	st := stack("s", cdService{"web", spec("nginx:1.2")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}

	if len(api.updated) != 1 || len(api.created) != 0 {
		t.Fatalf("updated %d, created %d; want 1 and 0", len(api.updated), len(api.created))
	}
	if api.updated[0].id != "id-s_web" {
		t.Errorf("id = %q, want the existing service's", api.updated[0].id)
	}
	// The compare-and-swap token has to be the one that came off the read, or
	// the update is not a compare-and-swap at all.
	if api.updated[0].version.Index != 7 {
		t.Errorf("version = %d, want 7", api.updated[0].version.Index)
	}
}

func TestCreateSendsRegistryAuth(t *testing.T) {
	api := &fakeAPI{}
	b := testBackend(t, api, nil)
	b.registryAuth = func(image string) (string, error) { return "auth:" + image, nil }
	st := stack("s", cdService{"web", spec("ghcr.io/team/app:1")})

	if err := b.ApplyServices(context.Background(), st, ResolveAlways); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if got, want := api.createOpt[0].EncodedRegistryAuth, "auth:ghcr.io/team/app:1"; got != want {
		t.Errorf("EncodedRegistryAuth = %q, want %q", got, want)
	}
}

func TestUpdateSendsRegistryAuth(t *testing.T) {
	api := installed(&fakeAPI{existing: []swarm.Service{deployed("s_web", "ghcr.io/team/app:1", "ghcr.io/team/app:1", 4)}}, "s")
	b := testBackend(t, api, nil)
	b.registryAuth = func(image string) (string, error) { return "auth:" + image, nil }
	st := stack("s", cdService{"web", spec("ghcr.io/team/app:2")})

	if err := b.ApplyServices(context.Background(), st, ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if got, want := api.updated[0].opts.EncodedRegistryAuth, "auth:ghcr.io/team/app:2"; got != want {
		t.Errorf("EncodedRegistryAuth = %q, want %q", got, want)
	}
}

// An application that declared no registryAuth pulls anonymously, exactly as
// before this backend authenticated at all.
func TestNoResolverSendsNoAuth(t *testing.T) {
	api := &fakeAPI{}
	st := stack("s", cdService{"web", spec("nginx")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveAlways); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if got := api.createOpt[0].EncodedRegistryAuth; got != "" {
		t.Errorf("EncodedRegistryAuth = %q, want empty", got)
	}
}

// The isolation property in one test: WithRegistryAuth scopes the credential to
// a copy and leaves the shared per-swarm backend anonymous, so one application's
// resolver cannot leak onto another's deploy through the backend they share.
func TestWithRegistryAuthLeavesTheSharedBackendAnonymous(t *testing.T) {
	api := &fakeAPI{}
	shared := testBackend(t, api, nil)

	scoped, ok := shared.WithRegistryAuth(func(image string) (string, error) { return "auth:" + image, nil }).(*Backend)
	if !ok {
		t.Fatal("WithRegistryAuth did not return a *Backend")
	}
	if shared.registryAuth != nil {
		t.Error("WithRegistryAuth mutated the shared backend; a per-swarm backend must stay anonymous")
	}

	// The copy shares the client, so applying through it still reaches the fake.
	st := stack("s", cdService{"web", spec("ghcr.io/team/app:1")})
	if err := scoped.ApplyServices(context.Background(), st, ResolveAlways); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if got := api.createOpt[0].EncodedRegistryAuth; got != "auth:ghcr.io/team/app:1" {
		t.Errorf("scoped copy sent %q, want the resolved auth", got)
	}
}

// A service on the swarm that the manifest no longer declares is left alone.
// Phase 1 is explicitly no prune, and the fake panics on any delete call.
func TestNothingIsDeleted(t *testing.T) {
	api := installed(&fakeAPI{existing: []swarm.Service{
		deployed("s_web", "nginx", "nginx", 1),
		deployed("s_gone", "old", "old", 1),
	}}, "s")
	st := stack("s", cdService{"web", spec("nginx")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if len(api.updated) != 1 {
		t.Errorf("updated %d services, want only the declared one", len(api.updated))
	}
}

// The conflict is a real signal, so it is reported — but a controller's job is
// to converge, and its desired spec is complete, so re-applying it is correcting
// drift rather than trampling it.
func TestVersionConflictIsRetriedAndReported(t *testing.T) {
	api := installed(&fakeAPI{
		existing:   []swarm.Service{deployed("s_web", "nginx", "nginx", 3)},
		updateErrs: []error{outOfSequence()},
	}, "s")
	var reported []string
	st := stack("s", cdService{"web", spec("nginx")})

	err := testBackend(t, api, func(s string) { reported = append(reported, s) }).
		ApplyServices(context.Background(), st, ResolveNever)
	if err != nil {
		t.Fatalf("ApplyServices = %v, want nil after the retry succeeded", err)
	}

	if len(api.updated) != 2 {
		t.Fatalf("updated %d times, want 2", len(api.updated))
	}
	if api.inspects != 1 {
		t.Errorf("re-read %d times, want 1", api.inspects)
	}
	// The retry must carry the version from the fresh read, not the stale one.
	if api.updated[1].version.Index == api.updated[0].version.Index {
		t.Errorf("retried with the same version %d; the re-read was not used", api.updated[1].version.Index)
	}
	if !reflect.DeepEqual(reported, []string{"s_web"}) {
		t.Errorf("reported = %v, want the overwrite to be recorded, not silent", reported)
	}
}

// Spinning forever on a service somebody else is rewriting helps nobody. Give
// up and let the next reconcile plan against whatever it settles on.
func TestPersistentConflictGivesUp(t *testing.T) {
	api := installed(&fakeAPI{
		existing:   []swarm.Service{deployed("s_web", "nginx", "nginx", 1)},
		updateErrs: []error{outOfSequence(), outOfSequence(), outOfSequence(), outOfSequence()},
	}, "s")
	var reported []string
	st := stack("s", cdService{"web", spec("nginx")})

	err := testBackend(t, api, func(s string) { reported = append(reported, s) }).
		ApplyServices(context.Background(), st, ResolveNever)
	if err == nil {
		t.Fatal("ApplyServices = nil, want an error after repeated conflicts")
	}
	if !strings.Contains(err.Error(), "s_web") {
		t.Errorf("error %q does not name the service", err)
	}
	if len(api.updated) != maxConflictRetries {
		t.Errorf("updated %d times, want %d", len(api.updated), maxConflictRetries)
	}
	if len(reported) != maxConflictRetries {
		t.Errorf("reported %d times, want one per losing write", len(reported))
	}
}

// Only the compare-and-swap failure is retried. Retrying a real failure would
// hammer the daemon and bury the reason.
func TestNonConflictErrorIsNotRetried(t *testing.T) {
	api := installed(&fakeAPI{
		existing:   []swarm.Service{deployed("s_web", "nginx", "nginx", 1)},
		updateErrs: []error{errors.New("invalid mount config")},
	}, "s")
	st := stack("s", cdService{"web", spec("nginx")})

	err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever)
	if err == nil || !strings.Contains(err.Error(), "invalid mount config") {
		t.Fatalf("err = %v, want the daemon's own reason", err)
	}
	if len(api.updated) != 1 {
		t.Errorf("updated %d times, want no retry", len(api.updated))
	}
	if api.inspects != 0 {
		t.Errorf("re-read %d times, want none", api.inspects)
	}
}

// The digest case, which is the one that silently redeploys a whole stack if it
// is wrong. `--resolve-image always` leaves the node holding only a digest ref,
// so the live spec's image is a digest while the manifest still says the tag.
// Writing the tag back would differ from the live spec and restart every task.
func TestUnchangedImageKeepsTheResolvedDigest(t *testing.T) {
	const tag = "nginx:1.2"
	const digest = "nginx:1.2@sha256:aaaa"

	for _, resolve := range []string{ResolveNever, ResolveChanged} {
		t.Run(resolve, func(t *testing.T) {
			api := installed(&fakeAPI{existing: []swarm.Service{deployed("s_web", tag, digest, 1)}}, "s")
			st := stack("s", cdService{"web", spec(tag)})

			if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, resolve); err != nil {
				t.Fatalf("ApplyServices = %v, want nil", err)
			}
			if got := api.updated[0].spec.TaskTemplate.ContainerSpec.Image; got != digest {
				t.Errorf("image = %q, want the resolved digest %q kept", got, digest)
			}
			if api.updated[0].opts.QueryRegistry {
				t.Error("QueryRegistry set for an image that did not change")
			}
		})
	}
}

// A genuinely changed tag is written as the tag, and the registry is queried so
// the daemon resolves it.
func TestChangedImageIsWrittenAndResolved(t *testing.T) {
	api := installed(&fakeAPI{existing: []swarm.Service{deployed("s_web", "nginx:1.2", "nginx:1.2@sha256:aaaa", 1)}}, "s")
	st := stack("s", cdService{"web", spec("nginx:1.3")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveChanged); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if got := api.updated[0].spec.TaskTemplate.ContainerSpec.Image; got != "nginx:1.3" {
		t.Errorf("image = %q, want the new tag", got)
	}
	if !api.updated[0].opts.QueryRegistry {
		t.Error("QueryRegistry not set for a changed image")
	}
}

func TestResolveAlwaysQueriesTheRegistry(t *testing.T) {
	api := installed(&fakeAPI{existing: []swarm.Service{deployed("s_web", "nginx", "nginx@sha256:aaaa", 1)}}, "s")
	st := stack("s", cdService{"web", spec("nginx")}, cdService{"new", spec("redis")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveAlways); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if !api.updated[0].opts.QueryRegistry {
		t.Error("update did not query the registry")
	}
	if !api.createOpt[0].QueryRegistry {
		t.Error("create did not query the registry")
	}
	// always means always: the tag is written back for the daemon to resolve
	// afresh rather than pinned to the digest it resolved to last time.
	if got := api.updated[0].spec.TaskTemplate.ContainerSpec.Image; got != "nginx" {
		t.Errorf("image = %q, want the tag re-resolved", got)
	}
}

func TestResolveNeverDoesNotQueryTheRegistry(t *testing.T) {
	api := &fakeAPI{}
	st := stack("s", cdService{"web", spec("nginx")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if api.createOpt[0].QueryRegistry {
		t.Error("QueryRegistry set despite resolve=never")
	}
}

// There is no --force here and there should not be. Dropping the existing
// counter would restart every task on an update that changes nothing else.
func TestForceUpdateCounterIsCarriedForward(t *testing.T) {
	cur := deployed("s_web", "nginx", "nginx", 1)
	cur.Spec.TaskTemplate.ForceUpdate = 4
	api := installed(&fakeAPI{existing: []swarm.Service{cur}}, "s")
	st := stack("s", cdService{"web", spec("nginx")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if got := api.updated[0].spec.TaskTemplate.ForceUpdate; got != 4 {
		t.Errorf("ForceUpdate = %d, want the existing 4 carried forward", got)
	}
}

// A re-read that fails leaves nothing to compare against, so the apply stops
// rather than guessing.
func TestReReadFailureAbortsTheUpdate(t *testing.T) {
	api := installed(&fakeAPI{
		existing:   []swarm.Service{deployed("s_web", "nginx", "nginx", 1)},
		updateErrs: []error{outOfSequence()},
		inspectErr: errors.New("daemon gone"),
	}, "s")
	st := stack("s", cdService{"web", spec("nginx")})

	err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever)
	if err == nil || !strings.Contains(err.Error(), "daemon gone") {
		t.Fatalf("err = %v, want the re-read failure surfaced", err)
	}
}

// A listing failure is not something to work around: without it there is no
// telling which services exist, so every one of them would be created afresh.
func TestListFailureAbortsBeforeAnyWrite(t *testing.T) {
	api := &listErrAPI{}
	st := stack("s", cdService{"web", spec("nginx")})

	err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever)
	if err == nil || !strings.Contains(err.Error(), "swarm unreachable") {
		t.Fatalf("err = %v, want the listing failure surfaced", err)
	}
}

type listErrAPI struct{ client.APIClient }

func (listErrAPI) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return nil, errors.New("swarm unreachable")
}

// A create that fails stops the apply where it is. Carrying on would leave the
// stack half-converged with no record of which half.
func TestCreateFailureStopsTheApply(t *testing.T) {
	api := &createErrAPI{}
	st := stack("s", cdService{"a", spec("x")}, cdService{"b", spec("y")})

	err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever)
	if err == nil || !strings.Contains(err.Error(), `service 's_a'`) {
		t.Fatalf("err = %v, want the failing service named", err)
	}
	if api.calls != 1 {
		t.Errorf("made %d create calls, want to stop at the first failure", api.calls)
	}
}

type createErrAPI struct {
	client.APIClient
	calls int
}

func (createErrAPI) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return nil, nil
}

func (c *createErrAPI) ServiceCreate(context.Context, swarm.ServiceSpec, swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	c.calls++
	return swarm.ServiceCreateResponse{}, errors.New("no such image")
}

// The daemon's warnings are the only place some misconfigurations surface at
// all — "no suitable node" among them — so they must not be dropped.
func TestSwarmWarningsAreLogged(t *testing.T) {
	var buf strings.Builder
	api := &warnAPI{}
	b := New(api, Options{Log: slog.New(slog.NewTextHandler(&buf, nil))})

	if err := b.ApplyServices(context.Background(), stack("s", cdService{"web", spec("nginx")}), ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "no suitable node") {
		t.Errorf("log %q does not carry the daemon's warning", buf.String())
	}
}

type warnAPI struct{ client.APIClient }

func (warnAPI) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return nil, nil
}

func (warnAPI) ServiceCreate(context.Context, swarm.ServiceSpec, swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	return swarm.ServiceCreateResponse{ID: "x", Warnings: []string{"no suitable node"}}, nil
}

// The zero Options must produce a usable Backend: a nil callback is the normal
// case until the reconcile loop wires one in.
func TestZeroOptionsIsUsable(t *testing.T) {
	api := &fakeAPI{}
	if err := New(api, Options{}).ApplyServices(context.Background(),
		stack("s", cdService{"web", spec("nginx")}), ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if len(api.created) != 1 {
		t.Errorf("created %d services, want 1", len(api.created))
	}
}

// errdefs classifies a 409 as a conflict; swarmkit's own stale-version error
// arrives as a 400 and only the message identifies it. Both must count, and
// an unrelated failure must not.
func TestIsVersionConflict(t *testing.T) {
	if !isVersionConflict(outOfSequence()) {
		t.Error("swarmkit's stale-version message was not recognised")
	}
	if !isVersionConflict(errdefs.ErrConflict) {
		t.Error("an errdefs conflict was not recognised")
	}
	if isVersionConflict(errors.New("invalid mount config")) {
		t.Error("an unrelated error was treated as a version conflict")
	}
	if isVersionConflict(nil) {
		t.Error("nil was treated as a version conflict")
	}
}

// --- resource methods ---

func labelOf(f filters.Args) string {
	for _, v := range f.Get("label") {
		return v
	}
	return ""
}

func (f *fakeAPI) note(what string) { f.order = append(f.order, what) }

func (f *fakeAPI) NetworkList(_ context.Context, o network.ListOptions) ([]network.Summary, error) {
	label := labelOf(o.Filters)
	f.labelFilters = append(f.labelFilters, label)
	if f.networkErr != nil {
		return nil, f.networkErr
	}
	if label == "" {
		return f.networks, nil
	}
	key, value, _ := strings.Cut(label, "=")
	var out []network.Summary
	for _, n := range f.networks {
		if v, ok := n.Labels[key]; ok && v == value {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeAPI) NetworkCreate(_ context.Context, name string, o network.CreateOptions) (network.CreateResponse, error) {
	if f.createdNets == nil {
		f.createdNets = map[string]network.CreateOptions{}
	}
	f.createdNets[name] = o
	f.note("network:" + name)
	return network.CreateResponse{ID: "net-" + name}, nil
}

func (f *fakeAPI) NetworkRemove(_ context.Context, id string) error {
	key := "network:" + id
	f.removed = append(f.removed, key)
	err := f.removeErr[key]
	if err == nil || f.removeGone[key] {
		f.networks = slices.DeleteFunc(f.networks, func(n network.Summary) bool { return n.ID == id })
	}
	return err
}

// ConfigList honours the label filter it is given, as NetworkList and SecretList
// do. A fake that ignored it would hand every caller the whole store, which is
// how a filtered read gets shipped untested — and the stack-scoped listers in
// live.go are nothing but a filter, so a fake that returned everything would
// make them pass whether or not they sent one. labelFilters remains the second
// check, because it also catches a list scoped by the wrong label.
func (f *fakeAPI) ConfigList(_ context.Context, o swarm.ConfigListOptions) ([]swarm.Config, error) {
	label := labelOf(o.Filters)
	f.labelFilters = append(f.labelFilters, label)
	if label == "" {
		return f.configs, nil
	}
	key, value, _ := strings.Cut(label, "=")
	var out []swarm.Config
	for _, c := range f.configs {
		if v, ok := c.Spec.Labels[key]; ok && v == value {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeAPI) ConfigInspectWithRaw(_ context.Context, name string) (swarm.Config, []byte, error) {
	for _, c := range f.configs {
		if c.Spec.Name == name {
			return c, nil, nil
		}
	}
	return swarm.Config{}, nil, errdefs.ErrNotFound
}

// ConfigCreate stores what it created, so a later read finds it — as a daemon
// would. A fake that only recorded the call could not model "create the config,
// then resolve against it the service that mounts it", which is the whole of
// #84: the conversion that resolves a reference runs after the create that
// satisfies it, and a fake forgetting its own writes makes that untestable.
func (f *fakeAPI) ConfigCreate(_ context.Context, spec swarm.ConfigSpec) (swarm.ConfigCreateResponse, error) {
	f.createdConfigs = append(f.createdConfigs, spec)
	f.note("config:" + spec.Name)
	id := "cfg-" + spec.Name
	f.configs = append(f.configs, swarm.Config{ID: id, Spec: spec})
	return swarm.ConfigCreateResponse{ID: id}, nil
}

func (f *fakeAPI) ConfigUpdate(_ context.Context, _ string, _ swarm.Version, spec swarm.ConfigSpec) error {
	f.updatedConfigs = append(f.updatedConfigs, spec)
	return nil
}

func (f *fakeAPI) ConfigRemove(_ context.Context, id string) error {
	key := "config:" + id
	f.removed = append(f.removed, key)
	err := f.removeErr[key]
	if err == nil || f.removeGone[key] {
		f.configs = slices.DeleteFunc(f.configs, func(c swarm.Config) bool { return c.ID == id })
	}
	return err
}

func (f *fakeAPI) SecretList(_ context.Context, o swarm.SecretListOptions) ([]swarm.Secret, error) {
	label := labelOf(o.Filters)
	f.labelFilters = append(f.labelFilters, label)
	if label == "" {
		return f.secrets, nil
	}
	key, value, _ := strings.Cut(label, "=")
	var out []swarm.Secret
	for _, s := range f.secrets {
		if v, ok := s.Spec.Labels[key]; ok && v == value {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeAPI) SecretInspectWithRaw(_ context.Context, name string) (swarm.Secret, []byte, error) {
	for _, s := range f.secrets {
		if s.Spec.Name == name {
			return s, nil, nil
		}
	}
	return swarm.Secret{}, nil, errdefs.ErrNotFound
}

// SecretCreate remembers its write for the reason ConfigCreate does.
func (f *fakeAPI) SecretCreate(_ context.Context, spec swarm.SecretSpec) (swarm.SecretCreateResponse, error) {
	f.createdSecrets = append(f.createdSecrets, spec)
	f.note("secret:" + spec.Name)
	id := "sec-" + spec.Name
	f.secrets = append(f.secrets, swarm.Secret{ID: id, Spec: spec})
	return swarm.SecretCreateResponse{ID: id}, nil
}

func (f *fakeAPI) SecretUpdate(_ context.Context, _ string, _ swarm.Version, spec swarm.SecretSpec) error {
	f.updatedSecrets = append(f.updatedSecrets, spec)
	return nil
}

func (f *fakeAPI) SecretRemove(_ context.Context, id string) error {
	key := "secret:" + id
	f.removed = append(f.removed, key)
	err := f.removeErr[key]
	if err == nil || f.removeGone[key] {
		f.secrets = slices.DeleteFunc(f.secrets, func(x swarm.Secret) bool { return x.ID == id })
	}
	return err
}

func (f *fakeAPI) ServiceRemove(_ context.Context, id string) error {
	key := "service:" + id
	f.removed = append(f.removed, key)
	err := f.removeErr[key]
	if err == nil || f.removeGone[key] {
		f.existing = slices.DeleteFunc(f.existing, func(x swarm.Service) bool { return x.ID == id })
	}
	return err
}

func (f *fakeAPI) VolumeList(_ context.Context, o volume.ListOptions) (volume.ListResponse, error) {
	f.labelFilters = append(f.labelFilters, labelOf(o.Filters))
	out := make([]*volume.Volume, 0, len(f.volumes))
	for i := range f.volumes {
		out = append(out, &f.volumes[i])
	}
	return volume.ListResponse{Volumes: out}, nil
}

// VolumeRemove honours removeErr as the other removals do, so that a volume
// that went between the list and the delete can be modelled at all.
func (f *fakeAPI) VolumeRemove(_ context.Context, name string, _ bool) error {
	key := "volume:" + name
	f.removed = append(f.removed, key)
	return f.removeErr[key]
}

func (f *fakeAPI) NodeList(ctx context.Context, _ swarm.NodeListOptions) ([]swarm.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.nodeErr != nil {
		return nil, f.nodeErr
	}
	return f.nodes, nil
}

func (f *fakeAPI) TaskList(ctx context.Context, _ swarm.TaskListOptions) ([]swarm.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.tasks, nil
}

func (f *fakeAPI) Info(ctx context.Context) (system.Info, error) {
	return system.Info{}, ctx.Err()
}

// The other half of the digest case, and the one that only matters once a
// controller corrects drift: the live image is not ours.
//
// `docker service update --image` rewrites ContainerSpec.Image and leaves the
// stack label alone, so the label still names the tag we deployed and the
// "unchanged image" branch still matches. Keeping the live image there would
// write the operator's image straight back, making the image the one thing a
// converge could not undo.
func TestOutOfBandImageIsNotPreserved(t *testing.T) {
	// The second pair is the shape that normalising the *live* side would hide:
	// `--image nginx@sha256:…` leaves the spec untagged, so stripping its digest
	// gives back exactly the string the manifest wrote.
	for name, tc := range map[string]struct{ tag, running string }{
		"a tagged manifest": {"nginx:1.2", "nginx:9.9@sha256:bbbb"},
		"an untagged manifest repinned by hand": {
			"nginx",
			"nginx@sha256:0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff",
		},
	} {
		for _, resolve := range []string{ResolveNever, ResolveChanged} {
			t.Run(name+"/"+resolve, func(t *testing.T) {
				// Label says what we deployed; the running image is somebody else's.
				api := installed(&fakeAPI{existing: []swarm.Service{deployed("s_web", tc.tag, tc.running, 1)}}, "s")
				st := stack("s", cdService{"web", spec(tc.tag)})

				if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, resolve); err != nil {
					t.Fatalf("ApplyServices = %v, want nil", err)
				}
				if got := api.updated[0].spec.TaskTemplate.ContainerSpec.Image; got != tc.tag {
					t.Errorf("image = %q, want the manifest's tag %q written back over the out-of-band one", got, tc.tag)
				}
			})
		}
	}
}

// A live service that is not a container service at all. swarm.TaskSpec holds a
// ContainerSpec, a PluginSpec and a NetworkAttachmentSpec, of which one is set,
// so a service under this stack's namespace label can arrive with no container
// spec — and anything that can reach the socket this controller holds can create
// one. Reading its image panicked, and nothing in this repository recovers, so
// that took the controller down for as long as the service existed.
func TestLiveServiceWithoutAContainerSpecDoesNotPanic(t *testing.T) {
	cur := deployed("s_web", "nginx:1.2", "nginx:1.2@sha256:aaaa", 1)
	cur.Spec.TaskTemplate.ContainerSpec = nil
	cur.Spec.TaskTemplate.NetworkAttachmentSpec = &swarm.NetworkAttachmentSpec{ContainerID: "abc"}
	api := installed(&fakeAPI{existing: []swarm.Service{cur}}, "s")
	st := stack("s", cdService{"web", spec("nginx:1.2")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want nil", err)
	}
	if len(api.updated) != 1 {
		t.Fatalf("updated %d services, want the update attempted", len(api.updated))
	}
	// There is no live image to preserve, so the manifest's own is written and
	// the daemon is left to answer for the runtime change.
	if got := api.updated[0].spec.TaskTemplate.ContainerSpec.Image; got != "nginx:1.2" {
		t.Errorf("image = %q, want the manifest's own tag", got)
	}
}

// The same guard from below the client, covering the desired side too and
// pinning the case both guards exist to protect: with both specs present and the
// tag unchanged, the digest the daemon resolved to is still kept.
func TestPrepareUpdateGuardsBothContainerSpecs(t *testing.T) {
	const digest = "nginx:1.2@sha256:aaaa"
	cur := deployed("s_web", "nginx:1.2", digest, 1)

	t.Run("both present", func(t *testing.T) {
		got, opts := prepareUpdate(cur, spec("nginx:1.2"), ResolveChanged)
		if img := got.TaskTemplate.ContainerSpec.Image; img != digest {
			t.Errorf("image = %q, want the resolved digest %q kept", img, digest)
		}
		if opts.QueryRegistry {
			t.Error("QueryRegistry set for an image that did not change")
		}
	})

	t.Run("live has none", func(t *testing.T) {
		other := cur
		other.Spec.TaskTemplate.ContainerSpec = nil
		got, _ := prepareUpdate(other, spec("nginx:1.2"), ResolveChanged)
		if img := got.TaskTemplate.ContainerSpec.Image; img != "nginx:1.2" {
			t.Errorf("image = %q, want the manifest's own tag", img)
		}
	})

	// Conversion always produces a container spec, so this half of the guard is
	// the cheap one — but prepareUpdate reads the field before anything has
	// established that.
	t.Run("desired has none", func(t *testing.T) {
		got, opts := prepareUpdate(cur, swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "s_web"}}, ResolveNever)
		if got.TaskTemplate.ContainerSpec != nil {
			t.Error("a container spec was invented for a desired spec that had none")
		}
		if opts.QueryRegistry {
			t.Error("QueryRegistry set despite resolve=never")
		}
	})
}

// The same case as TestUnchangedImageKeepsTheResolvedDigest for a manifest that
// named no tag, which is the shape the guard used to miss entirely.
//
// convert.Service writes the manifest's own string into the stack label, and the
// client tags what it sends, so the label says `nginx` where the live spec says
// `nginx:latest@…`. Comparing them literally never matched: the digest was
// dropped on the first correction and never resolved again under
// `--resolve-image changed`, so a stack pinned by digest quietly stopped being.
func TestUnchangedUntaggedImageKeepsTheResolvedDigest(t *testing.T) {
	const digest = "nginx:latest@sha256:aaaa"

	for _, resolve := range []string{ResolveNever, ResolveChanged} {
		t.Run(resolve, func(t *testing.T) {
			api := installed(&fakeAPI{existing: []swarm.Service{deployed("s_web", "nginx", digest, 1)}}, "s")
			st := stack("s", cdService{"web", spec("nginx")})

			if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, resolve); err != nil {
				t.Fatalf("ApplyServices = %v, want nil", err)
			}
			if got := api.updated[0].spec.TaskTemplate.ContainerSpec.Image; got != digest {
				t.Errorf("image = %q, want the resolved digest %q kept", got, digest)
			}
			if api.updated[0].opts.QueryRegistry {
				t.Error("QueryRegistry set for an image that did not change")
			}
		})
	}
}

// ---------------------------------------- whose services these are (#102)

// The write path's half of the rule the sweep has applied all along: the label
// says where a service lives, the record says whose it is. A service running
// under a namespace this controller has no release record for was put there by
// something else — `docker stack deploy`, another tool, the controller's own
// stack — and a release that happens to be named after it must not have its spec
// written over that service.
func TestRefusesToOverwriteAServiceItDidNotInstall(t *testing.T) {
	api := &fakeAPI{existing: []swarm.Service{deployed("s_web", "nginx", "nginx", 1)}}
	st := stack("s", cdService{"web", spec("attacker/evil")})

	err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever)
	if err == nil {
		t.Fatal("ApplyServices = nil, want a service this controller did not install left alone")
	}
	if !strings.Contains(err.Error(), "s_web") {
		t.Errorf("error %q does not name the service it refused to overwrite", err)
	}
	if len(api.updated) != 0 || len(api.created) != 0 {
		t.Errorf("wrote to a foreign namespace: updated=%d created=%d", len(api.updated), len(api.created))
	}
}

// Refused whether or not a name collides, because the namespace is the unit
// RemoveStack and every sweep act on: a release sharing one with a stack this
// engine did not install is a release whose uninstall takes both. It is also the
// two-commit version of the takeover — claim the namespace under harmless names
// first, add the service named like theirs afterwards — and this is where that is
// stopped, one commit earlier.
func TestRefusesToInstallAlongsideAStackItDidNotInstall(t *testing.T) {
	api := &fakeAPI{existing: []swarm.Service{deployed("s_theirs", "nginx", "nginx", 1)}}
	st := stack("s", cdService{"mine", spec("alpine")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err == nil {
		t.Fatal("ApplyServices = nil, want an install into somebody else's namespace refused")
	}
	if len(api.created) != 0 {
		t.Errorf("created %d services in a namespace that is not ours", len(api.created))
	}
}

// A chart can put any labels it likes on a config it declares, so the record's
// labels are forgeable — but a config created by a stack deploy is stamped with
// that stack's namespace on the way in and cannot be talked out of it, while a
// genuine record is written with com.swarmcli.* labels and no namespace at all.
// Declaring a name is not owning it (#86), and neither is declaring a label.
func TestAForgedReleaseRecordIsNotProofOfOwnership(t *testing.T) {
	api := &fakeAPI{
		existing: []swarm.Service{deployed("s_web", "nginx", "nginx", 1)},
		configs: []swarm.Config{{ID: "forged", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
			Name: "swarmcli.release.s.v1",
			Labels: map[string]string{
				charts.LabelType:       charts.TypeRelease,
				charts.LabelRelease:    "s",
				convert.LabelNamespace: "s",
			},
		}}}},
	}
	st := stack("s", cdService{"web", spec("attacker/evil")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err == nil {
		t.Fatal("ApplyServices = nil, want a config a chart declared not to count as a release record")
	}
	if len(api.updated) != 0 {
		t.Error("a chart-declared config was accepted as proof that the stack is ours")
	}
}

// Any owner counts, including none, and that is a decision rather than an
// oversight. The stamp answers which application installed a release, so that a
// sweep does not delete another controller's work; this asks whether the engine
// installed it at all. Requiring the stamp to be ours here would refuse both
// handovers the docs describe as supported — a release installed from the command
// line and then adopted by an application, and every release on the swarm after
// --controller-id changes, which is corrected by redeploying each one once.
func TestARecordWrittenByAnotherOwnerStillCounts(t *testing.T) {
	api := &fakeAPI{
		existing: []swarm.Service{deployed("s_web", "nginx:1.1", "nginx:1.1", 1)},
		configs: []swarm.Config{{ID: "rec", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
			Name: "swarmcli.release.s.v1",
			Labels: map[string]string{
				charts.LabelType:    charts.TypeRelease,
				charts.LabelRelease: "s",
				charts.LabelOwner:   "apply/prod-swarm:release/s",
			},
		}}}},
	}
	st := stack("s", cdService{"web", spec("nginx:1.2")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err != nil {
		t.Fatalf("ApplyServices = %v, want a release the engine installed to be updatable", err)
	}
	if len(api.updated) != 1 {
		t.Errorf("updated %d services, want the release taken over as the engine allows", len(api.updated))
	}
}

// Another release's records are not this one's proof. The read is scoped to the
// release being deployed, which the fake honours the way the daemon does.
func TestAnotherReleasesRecordIsNotProofOfOwnership(t *testing.T) {
	api := installed(&fakeAPI{existing: []swarm.Service{deployed("s_web", "nginx", "nginx", 1)}}, "other")
	st := stack("s", cdService{"web", spec("attacker/evil")})

	if err := testBackend(t, api, nil).ApplyServices(context.Background(), st, ResolveNever); err == nil {
		t.Fatal("ApplyServices = nil, want another release's record not to prove this one is ours")
	}
	if len(api.updated) != 0 {
		t.Error("a record belonging to another release was accepted as proof")
	}
}
