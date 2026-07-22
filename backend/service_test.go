// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

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

func (f *fakeAPI) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return f.existing, nil
}

func (f *fakeAPI) ServiceCreate(_ context.Context, spec swarm.ServiceSpec, opts swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	f.created = append(f.created, spec)
	f.createOpt = append(f.createOpt, opts)
	return swarm.ServiceCreateResponse{ID: "new-" + spec.Name}, nil
}

func (f *fakeAPI) ServiceUpdate(_ context.Context, id string, v swarm.Version, spec swarm.ServiceSpec, opts swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error) {
	f.updated = append(f.updated, updateCall{id: id, version: v, spec: spec, opts: opts})
	if n := len(f.updated) - 1; n < len(f.updateErrs) {
		return swarm.ServiceUpdateResponse{}, f.updateErrs[n]
	}
	return swarm.ServiceUpdateResponse{}, nil
}

func (f *fakeAPI) ServiceInspectWithRaw(_ context.Context, id string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
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

func TestUpdatesServicesThatExist(t *testing.T) {
	api := &fakeAPI{existing: []swarm.Service{deployed("s_web", "nginx:1.1", "nginx:1.1", 7)}}
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

// A service on the swarm that the manifest no longer declares is left alone.
// Phase 1 is explicitly no prune, and the fake panics on any delete call.
func TestNothingIsDeleted(t *testing.T) {
	api := &fakeAPI{existing: []swarm.Service{
		deployed("s_web", "nginx", "nginx", 1),
		deployed("s_gone", "old", "old", 1),
	}}
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
	api := &fakeAPI{
		existing:   []swarm.Service{deployed("s_web", "nginx", "nginx", 3)},
		updateErrs: []error{outOfSequence()},
	}
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
	api := &fakeAPI{
		existing:   []swarm.Service{deployed("s_web", "nginx", "nginx", 1)},
		updateErrs: []error{outOfSequence(), outOfSequence(), outOfSequence(), outOfSequence()},
	}
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
	api := &fakeAPI{
		existing:   []swarm.Service{deployed("s_web", "nginx", "nginx", 1)},
		updateErrs: []error{errors.New("invalid mount config")},
	}
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
			api := &fakeAPI{existing: []swarm.Service{deployed("s_web", tag, digest, 1)}}
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
	api := &fakeAPI{existing: []swarm.Service{deployed("s_web", "nginx:1.2", "nginx:1.2@sha256:aaaa", 1)}}
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
	api := &fakeAPI{existing: []swarm.Service{deployed("s_web", "nginx", "nginx@sha256:aaaa", 1)}}
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
	api := &fakeAPI{existing: []swarm.Service{cur}}
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
	api := &fakeAPI{
		existing:   []swarm.Service{deployed("s_web", "nginx", "nginx", 1)},
		updateErrs: []error{outOfSequence()},
		inspectErr: errors.New("daemon gone"),
	}
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
	if err == nil || !strings.Contains(err.Error(), `service "s_a"`) {
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
	f.labelFilters = append(f.labelFilters, labelOf(o.Filters))
	if f.networkErr != nil {
		return nil, f.networkErr
	}
	return f.networks, nil
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
	f.removed = append(f.removed, "network:"+id)
	return nil
}

func (f *fakeAPI) ConfigList(_ context.Context, o swarm.ConfigListOptions) ([]swarm.Config, error) {
	f.labelFilters = append(f.labelFilters, labelOf(o.Filters))
	return f.configs, nil
}

func (f *fakeAPI) ConfigInspectWithRaw(_ context.Context, name string) (swarm.Config, []byte, error) {
	for _, c := range f.configs {
		if c.Spec.Name == name {
			return c, nil, nil
		}
	}
	return swarm.Config{}, nil, errdefs.ErrNotFound
}

func (f *fakeAPI) ConfigCreate(_ context.Context, spec swarm.ConfigSpec) (swarm.ConfigCreateResponse, error) {
	f.createdConfigs = append(f.createdConfigs, spec)
	f.note("config:" + spec.Name)
	return swarm.ConfigCreateResponse{ID: "cfg-" + spec.Name}, nil
}

func (f *fakeAPI) ConfigUpdate(_ context.Context, _ string, _ swarm.Version, spec swarm.ConfigSpec) error {
	f.updatedConfigs = append(f.updatedConfigs, spec)
	return nil
}

func (f *fakeAPI) ConfigRemove(_ context.Context, id string) error {
	f.removed = append(f.removed, "config:"+id)
	return nil
}

func (f *fakeAPI) SecretList(_ context.Context, o swarm.SecretListOptions) ([]swarm.Secret, error) {
	f.labelFilters = append(f.labelFilters, labelOf(o.Filters))
	return f.secrets, nil
}

func (f *fakeAPI) SecretInspectWithRaw(_ context.Context, name string) (swarm.Secret, []byte, error) {
	for _, s := range f.secrets {
		if s.Spec.Name == name {
			return s, nil, nil
		}
	}
	return swarm.Secret{}, nil, errdefs.ErrNotFound
}

func (f *fakeAPI) SecretCreate(_ context.Context, spec swarm.SecretSpec) (swarm.SecretCreateResponse, error) {
	f.createdSecrets = append(f.createdSecrets, spec)
	f.note("secret:" + spec.Name)
	return swarm.SecretCreateResponse{ID: "sec-" + spec.Name}, nil
}

func (f *fakeAPI) SecretUpdate(_ context.Context, _ string, _ swarm.Version, spec swarm.SecretSpec) error {
	f.updatedSecrets = append(f.updatedSecrets, spec)
	return nil
}

func (f *fakeAPI) SecretRemove(_ context.Context, id string) error {
	f.removed = append(f.removed, "secret:"+id)
	return nil
}

func (f *fakeAPI) ServiceRemove(_ context.Context, id string) error {
	f.removed = append(f.removed, "service:"+id)
	return nil
}

func (f *fakeAPI) VolumeList(_ context.Context, o volume.ListOptions) (volume.ListResponse, error) {
	f.labelFilters = append(f.labelFilters, labelOf(o.Filters))
	out := make([]*volume.Volume, 0, len(f.volumes))
	for i := range f.volumes {
		out = append(out, &f.volumes[i])
	}
	return volume.ListResponse{Volumes: out}, nil
}

func (f *fakeAPI) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.removed = append(f.removed, "volume:"+name)
	return nil
}

func (f *fakeAPI) NodeList(context.Context, swarm.NodeListOptions) ([]swarm.Node, error) {
	return f.nodes, nil
}

func (f *fakeAPI) TaskList(context.Context, swarm.TaskListOptions) ([]swarm.Task, error) {
	return f.tasks, nil
}

func (f *fakeAPI) Info(context.Context) (system.Info, error) { return system.Info{}, nil }
