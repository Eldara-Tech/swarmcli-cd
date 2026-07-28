// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package prune

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

const testController = "prod"

type fakeSwarms struct {
	err     error
	backend charts.Backend
}

func (f fakeSwarms) Backend(context.Context, string) (charts.Backend, error) {
	return f.backend, f.err
}

// fakeBackend is the deployed side of a prune: what RemoveStack and the volume
// calls did, and whether they were allowed to work. charts.Backend is embedded
// nil so any other method panics naming itself rather than silently answering.
type fakeBackend struct {
	charts.Backend
	mu sync.Mutex

	removedStacks []string
	removeErr     map[string]error

	volumes    map[string][]string
	removedVol []string
	// volInUse counts, per volume, how many removal attempts still fail before
	// it frees — the "container has not let go yet" case.
	volInUse map[string]int
}

func (b *fakeBackend) RemoveStack(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removedStacks = append(b.removedStacks, name)
	return b.removeErr[name]
}

func (b *fakeBackend) StackVolumes(_ context.Context, name string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.volumes[name], nil
}

func (b *fakeBackend) RemoveVolume(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := b.volInUse[name]; n > 0 {
		b.volInUse[name] = n - 1
		return errors.New("volume is in use")
	}
	b.removedVol = append(b.removedVol, name)
	return nil
}

func (b *fakeBackend) stacks() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.removedStacks)
}

type uninstalled struct {
	release string
	volumes bool
}

type fakeEngine struct {
	releases []charts.Release
	listErr  error
	fail     map[string]error
	// gone marks a release that its failing Uninstall nevertheless finished —
	// the records are deleted, so a later List no longer has it. That is what a
	// spurious stack-removal error looks like from the outside.
	gone     map[string]bool
	networks map[string][]string

	calls []uninstalled
}

func (e *fakeEngine) List(context.Context) ([]charts.Release, error) {
	return e.releases, e.listErr
}

func (e *fakeEngine) Uninstall(_ context.Context, release string, purgeVolumes bool) (*charts.UninstallResult, error) {
	e.calls = append(e.calls, uninstalled{release, purgeVolumes})
	err := e.fail[release]
	if err == nil || e.gone[release] {
		e.releases = slices.DeleteFunc(e.releases, func(r charts.Release) bool { return r.Name == release })
	}
	if err != nil {
		return nil, err
	}
	return &charts.UninstallResult{OrphanedNetworks: e.networks[release]}, nil
}

func (e *fakeEngine) pruned() []string {
	var out []string
	for _, c := range e.calls {
		out = append(out, c.release)
	}
	return out
}

func testPruner(t *testing.T, e *fakeEngine, volumes bool) *Pruner {
	t.Helper()
	return prunerWith(t, e, &fakeBackend{}, volumes)
}

func prunerWith(t *testing.T, e *fakeEngine, b charts.Backend, volumes bool) *Pruner {
	t.Helper()
	return New(Options{
		Swarms:       fakeSwarms{backend: b},
		Engine:       func(charts.Backend) Engine { return e },
		Volumes:      volumes,
		ControllerID: testController,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// owned builds a release carrying a well-formed stamp for one of this
// controller's applications, the way the reconciler writes it.
func owned(release, app string) charts.Release {
	return ownedBy(release, testController, app)
}

func ownedBy(release, controller, app string) charts.Release {
	return stamped(release, charts.OwnerRef{
		ID:   application.OwnerID(controller, app),
		Kind: charts.OwnerKindRelease,
		Name: release,
	}.String())
}

func stamped(release, owner string) charts.Release {
	return charts.Release{Name: release, Owner: owner}
}

func TestDepartedApplicationIsPruned(t *testing.T) {
	e := &fakeEngine{releases: []charts.Release{
		owned("api", "gone"),
		owned("web", "kept"),
	}}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"})
	if err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}
	if want := []string{"gone"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pruned applications = %v, want %v", got, want)
	}
	if want := []string{"api"}; !reflect.DeepEqual(e.pruned(), want) {
		t.Errorf("uninstalled releases = %v, want %v", e.pruned(), want)
	}
}

// Everything the sweep must leave alone, each for its own reason. A release
// that is not provably this controller's is not a prune candidate, and neither
// is one whose application is still declared.
func TestOnlyProvablyDepartedReleasesArePruned(t *testing.T) {
	for name, rel := range map[string]charts.Release{
		"still in the set":          owned("api", "kept"),
		"installed from the CLI":    stamped("api", "apply/prod:release/api"),
		"another tool's stamp":      stamped("api", "flux/prod:release/api"),
		"no stamp at all":           stamped("api", ""),
		"unparseable stamp":         stamped("api", "not-an-owner-ref"),
		"stamp names another rel":   stamped("api", "cd/prod/gone:release/web"),
		"stamp is a bare cd/":       stamped("api", "cd/:release/api"),
		"stamp of the wrong kind":   stamped("api", "cd/prod/gone:stack/api"),
		"another controller's":      ownedBy("api", "staging", "gone"),
		"the pre-controller-id fmt": stamped("api", "cd/gone:release/api"),
	} {
		t.Run(name, func(t *testing.T) {
			e := &fakeEngine{releases: []charts.Release{rel}}

			got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"})
			if err != nil {
				t.Fatalf("Departed = %v, want nil", err)
			}
			if len(got) != 0 {
				t.Errorf("pruned applications = %v, want none", got)
			}
			if len(e.calls) != 0 {
				t.Errorf("uninstalled %v, want nothing", e.pruned())
			}
		})
	}
}

// The guard that stops a controller from emptying a swarm because the app set
// momentarily parsed to nothing.
func TestEmptyDesiredSetPrunesNothing(t *testing.T) {
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}

	got, err := testPruner(t, e, false).Departed(t.Context(), nil)
	if err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}
	if len(got) != 0 || len(e.calls) != 0 {
		t.Errorf("pruned %v / uninstalled %v, want nothing on an empty desired set", got, e.pruned())
	}
}

func TestVolumesAreOnlyPurgedWhenAsked(t *testing.T) {
	for _, volumes := range []bool{false, true} {
		e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
		b := &fakeBackend{volumes: map[string][]string{"api": {"api_data"}}}

		if _, err := prunerWith(t, e, b, volumes).Departed(t.Context(), []string{"kept"}); err != nil {
			t.Fatalf("Departed = %v, want nil", err)
		}

		var want []string
		if volumes {
			want = []string{"api_data"}
		}
		if !slices.Equal(b.removedVol, want) {
			t.Errorf("removed volumes %v with Volumes=%v, want %v", b.removedVol, volumes, want)
		}
		// Never through Uninstall: the volumes have to go before the release
		// records do, so prune does them itself and asks for none here.
		if len(e.calls) != 1 || e.calls[0].volumes {
			t.Errorf("uninstall calls = %+v, want one that purges no volumes", e.calls)
		}
	}
}

// The records carry the owner stamp, which is the only thing a later sweep can
// find this release by. Deleting them when the stack is still up would strand
// the resources permanently, so a failed removal must leave them alone.
func TestAFailedStackRemovalKeepsTheReleaseRecords(t *testing.T) {
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	b := &fakeBackend{removeErr: map[string]error{"api": errors.New("network still attached")}}

	got, err := prunerWith(t, e, b, false).Departed(t.Context(), []string{"kept"})
	if err == nil {
		t.Fatal("Departed = nil, want the removal failure")
	}
	if len(got) != 0 {
		t.Errorf("pruned = %v, want none — nothing was removed", got)
	}
	if len(e.calls) != 0 {
		t.Error("the release records were deleted even though the stack is still deployed")
	}
}

// Same guarantee for the volume half: a volume that will not free must not cost
// us the stamp that lets the next sweep try again.
func TestAFailedVolumePurgeKeepsTheReleaseRecords(t *testing.T) {
	restore := volumeSettleTimeout
	volumeSettleTimeout, volumeSettleInterval = 0, 0
	t.Cleanup(func() { volumeSettleTimeout, volumeSettleInterval = restore, 2*time.Second })

	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	b := &fakeBackend{
		volumes:  map[string][]string{"api": {"api_data"}},
		volInUse: map[string]int{"api_data": 100},
	}

	if _, err := prunerWith(t, e, b, true).Departed(t.Context(), []string{"kept"}); err == nil {
		t.Fatal("Departed = nil, want the volume failure")
	}
	if len(e.calls) != 0 {
		t.Error("the release records were deleted even though a volume survived")
	}
	// The stack came down, which is what makes the retry cheap next time.
	if !slices.Equal(b.stacks(), []string{"api"}) {
		t.Errorf("removed stacks %v, want [api]", b.stacks())
	}
}

// The services were removed a moment ago, so the first attempt on a volume
// routinely fails while the container is still letting go. That is a wait, not
// a failure.
func TestAVolumeStillInUseIsRetried(t *testing.T) {
	restore := volumeSettleInterval
	volumeSettleInterval = 0
	t.Cleanup(func() { volumeSettleInterval = restore })

	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	b := &fakeBackend{
		volumes:  map[string][]string{"api": {"api_data"}},
		volInUse: map[string]int{"api_data": 3},
	}

	got, err := prunerWith(t, e, b, true).Departed(t.Context(), []string{"kept"})
	if err != nil {
		t.Fatalf("Departed = %v, want nil once the volume frees", err)
	}
	if !slices.Equal(b.removedVol, []string{"api_data"}) {
		t.Errorf("removed volumes %v, want [api_data]", b.removedVol)
	}
	if !slices.Equal(got, []string{"gone"}) {
		t.Errorf("pruned = %v, want [gone]", got)
	}
}

// Every departed application is attempted even when an earlier one fails, and
// every failure surfaces. A stack whose network is still attached must not
// strand the rest of the sweep.
func TestOneFailureDoesNotStopTheSweep(t *testing.T) {
	boom := errors.New("network still attached")
	e := &fakeEngine{
		releases: []charts.Release{owned("api", "first"), owned("web", "second")},
		fail:     map[string]error{"api": boom},
	}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"})
	if !errors.Is(err, boom) {
		t.Fatalf("Departed = %v, want it to carry %v", err, boom)
	}
	if want := []string{"api", "web"}; !reflect.DeepEqual(e.pruned(), want) {
		t.Errorf("uninstalled %v, want both attempted (%v)", e.pruned(), want)
	}
	// The one that failed is not reported as pruned; the one that worked is.
	if want := []string{"second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pruned applications = %v, want %v", got, want)
	}
}

// An application with several releases is pruned whole, and is only reported as
// pruned if all of them went — a half-deleted application still has resources
// on the swarm.
func TestPartiallyFailedApplicationIsNotReportedAsPruned(t *testing.T) {
	e := &fakeEngine{
		releases: []charts.Release{owned("api", "gone"), owned("web", "gone")},
		fail:     map[string]error{"web": errors.New("nope")},
	}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"})
	if err == nil {
		t.Fatal("Departed = nil, want the failure to surface")
	}
	if len(got) != 0 {
		t.Errorf("pruned applications = %v, want none — the application is only half gone", got)
	}
	if want := []string{"api", "web"}; !reflect.DeepEqual(e.pruned(), want) {
		t.Errorf("uninstalled %v, want both attempted (%v)", e.pruned(), want)
	}
}

func TestListFailureIsReportedAndDeletesNothing(t *testing.T) {
	boom := errors.New("daemon unreachable")
	e := &fakeEngine{listErr: boom}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"})
	if !errors.Is(err, boom) {
		t.Fatalf("Departed = %v, want it to carry %v", err, boom)
	}
	if len(got) != 0 || len(e.calls) != 0 {
		t.Error("a failed list must not delete anything")
	}
}

func TestUnresolvableSwarmIsReported(t *testing.T) {
	boom := errors.New("no such swarm")
	p := New(Options{
		Swarms: fakeSwarms{err: boom},
		Engine: func(charts.Backend) Engine { return &fakeEngine{} },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if _, err := p.Departed(t.Context(), []string{"kept"}); !errors.Is(err, boom) {
		t.Fatalf("Departed = %v, want it to carry %v", err, boom)
	}
}

// The networks the chart engine deliberately leaves behind are reported, not
// swallowed: an operator who is not told believes the cleanup was complete.
func TestLeftoverNetworksAreLogged(t *testing.T) {
	var buf bytes.Buffer
	p := New(Options{
		Swarms: fakeSwarms{backend: &fakeBackend{}},
		Engine: func(charts.Backend) Engine {
			return &fakeEngine{
				releases: []charts.Release{owned("api", "gone")},
				networks: map[string][]string{"api": {"shared-net"}},
			}
		},
		ControllerID: testController,
		Log:          slog.New(slog.NewTextHandler(&buf, nil)),
	})

	if _, err := p.Departed(t.Context(), []string{"kept"}); err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "shared-net") {
		t.Errorf("the leftover network was not logged, got %q", buf.String())
	}
}

// Grouping is by application and ordered as List returned the releases, so a
// sweep does not reshuffle what it reports between runs.
func TestReleasesAreGroupedByApplicationInListOrder(t *testing.T) {
	releases := []charts.Release{
		owned("a-api", "alpha"),
		owned("b-api", "beta"),
		owned("c-web", "alpha"),
	}

	got := departed(releases, []string{"kept"}, testController)
	want := []departedApp{
		{name: "alpha", releases: []string{"a-api", "c-web"}},
		{name: "beta", releases: []string{"b-api"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("departed = %+v, want %+v", got, want)
	}
}

func TestOwnerRejectsWhatItCannotProve(t *testing.T) {
	if app, ok := owner(owned("api", "edge"), testController); !ok || app != "edge" {
		t.Errorf("owner(well-formed) = %q, %v; want edge, true", app, ok)
	}
	for _, rel := range []charts.Release{
		stamped("api", ""),
		stamped("api", "garbage"),
		stamped("api", "cd/prod/edge:release/other"),
		stamped("api", "apply/edge:release/api"),
		ownedBy("api", "staging", "edge"),
	} {
		if app, ok := owner(rel, testController); ok {
			t.Errorf("owner(%q) = %q, true; want it rejected", rel.Owner, app)
		}
	}
}

// The failure CI kept hitting. Uninstall reported that it could not remove the
// stack — a swarm-scoped network delete proxied through swarmkit, replying
// about a network that had already gone — while having deleted everything it
// was asked to. Neither the error nor a re-list of the networks can tell that
// apart from a real failure; the release no longer being listed can.
func TestAFailureThatFinishedTheJobCountsAsPruned(t *testing.T) {
	e := &fakeEngine{
		releases: []charts.Release{owned("api", "gone")},
		fail:     map[string]error{"api": errors.New("removing stack: network xyz not found")},
		gone:     map[string]bool{"api": true},
	}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"})
	if err != nil {
		t.Fatalf("Departed = %v, want nil — the release is gone, whatever the call said", err)
	}
	if want := []string{"gone"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %v, want %v", got, want)
	}
}

// The other half: a failure that left the release in place is a real failure,
// and must not be explained away by the same check.
func TestAFailureThatLeftTheReleaseIsStillAFailure(t *testing.T) {
	e := &fakeEngine{
		releases: []charts.Release{owned("api", "gone")},
		fail:     map[string]error{"api": errors.New("network still attached")},
	}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"})
	if err == nil {
		t.Fatal("Departed = nil, want the failure to survive the re-check")
	}
	if len(got) != 0 {
		t.Errorf("pruned = %v, want none", got)
	}
}
