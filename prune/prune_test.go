// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package prune

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/Eldara-Tech/swarmcli-cd/capability"
	"github.com/Eldara-Tech/swarmcli-cd/swarms"
)

const testController = "prod"

type fakeSwarms struct {
	err     error
	backend charts.Backend
}

func (f fakeSwarms) Backend(context.Context, swarms.Target) (charts.Backend, error) {
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
	// volDelay is how long one volume's removal takes, so that a test can spend
	// a settle budget rather than assert about one.
	volDelay map[string]time.Duration
}

// sizedBackend is a backend that can say how big the swarm is. Separate from
// fakeBackend rather than a field on it, because "cannot answer" is one of the
// three cases under test and a method cannot be un-declared.
type sizedBackend struct {
	*fakeBackend
	nodes int
	err   error
}

func (b sizedBackend) SwarmNodes(context.Context) (int, error) { return b.nodes, b.err }

func (b *fakeBackend) RemoveStack(_ context.Context, name string) error {
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
	time.Sleep(b.volDelay[name])
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
	// listErrAt is the 1-based call from which List starts failing; 0 means the
	// first. The re-check after a failed uninstall is the second call, and
	// failing only that one is the only way to reach the path where the sweep
	// holds diagnoses it cannot check.
	listErrAt int
	listCalls int
	fail      map[string]error
	// gone marks a release that its failing Uninstall nevertheless finished —
	// the records are deleted, so a later List no longer has it. That is what a
	// spurious stack-removal error looks like from the outside.
	gone     map[string]bool
	networks map[string][]string

	calls []uninstalled
}

func (e *fakeEngine) List(context.Context) ([]charts.Release, error) {
	e.listCalls++
	if e.listErr != nil && e.listCalls >= max(e.listErrAt, 1) {
		return nil, e.listErr
	}
	return e.releases, nil
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
	return prunerLogging(t, e, b, volumes, io.Discard)
}

func prunerLogging(t *testing.T, e *fakeEngine, b charts.Backend, volumes bool, w io.Writer) *Pruner {
	t.Helper()
	return New(Options{
		Swarms:       fakeSwarms{backend: b},
		Engine:       func(charts.Backend) Engine { return e },
		Volumes:      volumes,
		ControllerID: testController,
		Log:          slog.New(slog.NewTextHandler(w, nil)),
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

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, nil)
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

			got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, nil)
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

// Issue #62. Renaming an application leaves its release stamped for the name
// that departed, because a plan that came out identical deploys nothing and so
// re-stamps nothing. The stamp alone would therefore condemn a stack that an
// application still in the set is reconciling.
func TestAReleaseStillDeclaredIsNeverPruned(t *testing.T) {
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, []string{"api"})
	if err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}
	if len(got) != 0 || len(e.calls) != 0 {
		t.Errorf("pruned %v / uninstalled %v, want nothing: api is still declared", got, e.pruned())
	}
}

// Sparing is per release rather than per application, so a departed application
// that handed one release over still loses the rest.
func TestOnlyTheStillDeclaredReleaseOfADepartedApplicationSurvives(t *testing.T) {
	e := &fakeEngine{releases: []charts.Release{
		owned("api", "gone"),
		owned("web", "gone"),
	}}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, []string{"api"})
	if err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}
	if want := []string{"gone"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pruned applications = %v, want %v", got, want)
	}
	if want := []string{"web"}; !reflect.DeepEqual(e.pruned(), want) {
		t.Errorf("uninstalled releases = %v, want %v; api is still declared", e.pruned(), want)
	}
}

// The guard that stops a controller from emptying a swarm because the app set
// momentarily parsed to nothing.
func TestEmptyDesiredSetPrunesNothing(t *testing.T) {
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}

	got, err := testPruner(t, e, false).Departed(t.Context(), nil, nil)
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

		if _, err := prunerWith(t, e, b, volumes).Departed(t.Context(), []string{"kept"}, nil); err != nil {
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

	got, err := prunerWith(t, e, b, false).Departed(t.Context(), []string{"kept"}, nil)
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

// The one departure that is not a cleanup. Dropping the self application from
// the app set leaves the stack this controller runs as stamped for an
// application nobody declares — and no sweep can remove it, because the
// controller would be deleting itself. Reported as a failure it would be the
// same failure every interval, for ever, about a controller that is working.
func TestTheControllersOwnStackIsLeftAloneWhenItLeavesTheSet(t *testing.T) {
	e := &fakeEngine{releases: []charts.Release{owned("swarmcli-cd", "gone")}}
	b := &fakeBackend{removeErr: map[string]error{"swarmcli-cd": fmt.Errorf(
		"refusing to act on release 'swarmcli-cd': %w, so removing it would delete the controller", capability.ErrOwnStack)}}

	got, err := prunerWith(t, e, b, false).Departed(t.Context(), []string{"kept"}, nil)
	if err != nil {
		t.Fatalf("Departed = %v, want the controller's own stack skipped rather than reported", err)
	}
	if len(got) != 0 {
		t.Errorf("pruned = %v, want none — nothing was removed", got)
	}
	if len(e.calls) != 0 {
		t.Error("the release records were deleted for a stack that is still deployed")
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

	if _, err := prunerWith(t, e, b, true).Departed(t.Context(), []string{"kept"}, nil); err == nil {
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

	got, err := prunerWith(t, e, b, true).Departed(t.Context(), []string{"kept"}, nil)
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

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, nil)
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

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, nil)
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

// The re-check exists to decide whether the uninstall failures were real, so a
// re-check that itself fails leaves the sweep with the failures and no verdict.
// Reporting only its own error threw away one wrapped diagnosis per release that
// would not come down and replaced them all with "re-checking which releases are
// left", which names nothing an operator can act on (#107).
func TestARecheckFailureKeepsTheUninstallDiagnoses(t *testing.T) {
	e := &fakeEngine{
		releases: []charts.Release{owned("api", "gone"), owned("web", "gone")},
		fail: map[string]error{
			"api": errors.New("network still attached"),
			"web": errors.New("volume is in use"),
		},
		listErr:   errors.New("daemon unreachable"),
		listErrAt: 2,
	}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, nil)
	if err == nil {
		t.Fatal("Departed = nil, want the failures reported")
	}
	if len(got) != 0 {
		t.Errorf("pruned applications = %v, want none — nothing was proved gone", got)
	}
	for _, want := range []string{"network still attached", "volume is in use", "daemon unreachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
}

func TestListFailureIsReportedAndDeletesNothing(t *testing.T) {
	boom := errors.New("daemon unreachable")
	e := &fakeEngine{listErr: boom}

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, nil)
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

	if _, err := p.Departed(t.Context(), []string{"kept"}, nil); !errors.Is(err, boom) {
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

	if _, err := p.Departed(t.Context(), []string{"kept"}, nil); err != nil {
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

	got := departed(releases, []string{"kept"}, nil, testController)
	want := []departedApp{
		{name: "alpha", releases: []string{"a-api", "c-web"}},
		{name: "beta", releases: []string{"b-api"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("departed = %+v, want %+v", got, want)
	}
}

func TestOwnerRejectsWhatItCannotProve(t *testing.T) {
	if app, ok := Owner(owned("api", "edge"), testController); !ok || app != "edge" {
		t.Errorf("Owner(well-formed) = %q, %v; want edge, true", app, ok)
	}
	for _, rel := range []charts.Release{
		stamped("api", ""),
		stamped("api", "garbage"),
		stamped("api", "cd/prod/edge:release/other"),
		stamped("api", "apply/edge:release/api"),
		ownedBy("api", "staging", "edge"),
	} {
		if app, ok := Owner(rel, testController); ok {
			t.Errorf("Owner(%q) = %q, true; want it rejected", rel.Owner, app)
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

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, nil)
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

	got, err := testPruner(t, e, false).Departed(t.Context(), []string{"kept"}, nil)
	if err == nil {
		t.Fatal("Departed = nil, want the failure to survive the re-check")
	}
	if len(got) != 0 {
		t.Errorf("pruned = %v, want none", got)
	}
}

// ------------------------------------------------------- the service rule

func TestUndeclaredNamesWhatTheManifestDroppedAndNothingElse(t *testing.T) {
	running := []string{"web_app", "web_sidecar", "web_cache"}
	declared := []string{"web_app", "web_worker"}

	// web_worker is declared and not running, which is a missing service and
	// somebody else's problem: a redeploy creates it, and this only ever looks
	// at what is already there.
	if got, want := Undeclared(running, declared), []string{"web_cache", "web_sidecar"}; !slices.Equal(got, want) {
		t.Errorf("Undeclared = %v, want %v", got, want)
	}
}

func TestUndeclaredIsSortedWhateverTheSwarmReturned(t *testing.T) {
	got := Undeclared([]string{"web_z", "web_a", "web_m"}, nil)
	if want := []string{"web_a", "web_m", "web_z"}; !slices.Equal(got, want) {
		t.Errorf("Undeclared = %v, want %v — an unstable order makes a status poll look like a change", got, want)
	}
}

func TestUndeclaredFindsNothingWhenEverythingRunningIsDeclared(t *testing.T) {
	if got := Undeclared([]string{"web_app"}, []string{"web_app", "web_worker"}); got != nil {
		t.Errorf("Undeclared = %v, want nil", got)
	}
}

// The empty swarm, which is also the release that has just been installed.
func TestUndeclaredFindsNothingWhenNothingIsRunning(t *testing.T) {
	if got := Undeclared(nil, []string{"web_app"}); got != nil {
		t.Errorf("Undeclared = %v, want nil", got)
	}
}

func TestClaimSplitsOnWhatTheRevisionDeclared(t *testing.T) {
	claimed, rest := Claim([]string{"web_sidecar", "web_stranger"}, []string{"web_app", "web_sidecar"})

	if want := []string{"web_sidecar"}; !slices.Equal(claimed, want) {
		t.Errorf("claimed = %v, want %v", claimed, want)
	}
	// The one no revision of ours declared. It may be another tool's, or ours
	// from before the retained history; from here those are the same thing.
	if want := []string{"web_stranger"}; !slices.Equal(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
	}
}

// What lets a sweep stop reading history: everything accounted for leaves
// nothing to carry into an older revision.
func TestClaimLeavesNothingWhenARevisionDeclaredEveryCandidate(t *testing.T) {
	claimed, rest := Claim([]string{"web_a", "web_b"}, []string{"web_a", "web_b", "web_c"})

	if want := []string{"web_a", "web_b"}; !slices.Equal(claimed, want) {
		t.Errorf("claimed = %v, want %v", claimed, want)
	}
	if len(rest) != 0 {
		t.Errorf("rest = %v, want none — the walk should stop here", rest)
	}
}

// A revision that declared none of them proves nothing and consumes nothing.
func TestClaimCarriesEveryCandidatePastAnUnrelatedRevision(t *testing.T) {
	claimed, rest := Claim([]string{"web_a"}, []string{"web_other"})

	if len(claimed) != 0 {
		t.Errorf("claimed = %v, want none", claimed)
	}
	if want := []string{"web_a"}; !slices.Equal(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
	}
}

func TestClaimWithNoCandidatesDoesNothing(t *testing.T) {
	claimed, rest := Claim(nil, []string{"web_app"})
	if claimed != nil || rest != nil {
		t.Errorf("Claim(nil, …) = %v, %v; want nil, nil", claimed, rest)
	}
}

// ------------------------------------- volumes this node cannot see (#108)

// GET /volumes answers from the node-local volume store, so on a swarm of more
// than one node an empty listing is not evidence that the release had no
// volumes — and reporting it as a completed purge is the claim this must not
// make. The stack's data survives on whichever nodes ran its tasks, and the
// only honest thing left is to say so and hand over the filter that finds it.
func TestAMultiNodeSwarmDoesNotClaimToHavePrunedVolumesItCannotSee(t *testing.T) {
	var buf bytes.Buffer
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	// Nothing under this stack's label on the node the controller talks to,
	// which is what a stack scheduled elsewhere looks like from here.
	b := sizedBackend{fakeBackend: &fakeBackend{}, nodes: 3}

	got, err := prunerLogging(t, e, b, true, &buf).Departed(t.Context(), []string{"kept"}, nil)
	if err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}

	log := buf.String()
	if !strings.Contains(log, "node-local") {
		t.Errorf("log %q does not say the listing was node-local", log)
	}
	if !strings.Contains(log, "label=com.docker.stack.namespace=api") {
		t.Errorf("log %q does not hand over the filter that finds what is left", log)
	}
	if strings.Contains(log, "deleted the release's volumes") {
		t.Errorf("log %q claims a completed purge on a multi-node swarm", log)
	}
	// What is deliberately *not* done: the sweep still finishes. The release
	// records are what a later sweep finds a release by, and a later sweep on
	// this node would read the same node-local listing and be no better off, so
	// holding them back would only leave an application that can never finish
	// being pruned. See purgeVolumes.
	if want := []string{"api"}; !reflect.DeepEqual(e.pruned(), want) {
		t.Errorf("uninstalled %v, want %v — the records go, and the warning is what carries the rest", e.pruned(), want)
	}
	if want := []string{"gone"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pruned applications = %v, want %v", got, want)
	}
}

// The one case where the node-local listing really is the swarm's.
func TestASingleNodeSwarmReportsThePurgeAsComplete(t *testing.T) {
	var buf bytes.Buffer
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	b := sizedBackend{
		fakeBackend: &fakeBackend{volumes: map[string][]string{"api": {"api_data"}}},
		nodes:       1,
	}

	if _, err := prunerLogging(t, e, b, true, &buf).Departed(t.Context(), []string{"kept"}, nil); err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}

	log := buf.String()
	if !strings.Contains(log, "deleted the release's volumes") || !strings.Contains(log, "api_data") {
		t.Errorf("log %q does not report the purge that did cover the swarm", log)
	}
	if strings.Contains(log, "node-local") {
		t.Errorf("log %q warns about other nodes on a one-node swarm", log)
	}
}

// A count that could not be read is not one node. Both shapes of "cannot
// answer" have to land on the cautious side, because the alternative is
// claiming a purge covered a swarm nothing checked.
func TestAnUnanswerableNodeCountIsNotOneNode(t *testing.T) {
	for name, b := range map[string]charts.Backend{
		"a backend without the seam": &fakeBackend{volumes: map[string][]string{"api": {"api_data"}}},
		"a listing that failed": sizedBackend{
			fakeBackend: &fakeBackend{volumes: map[string][]string{"api": {"api_data"}}},
			err:         errors.New("this node is not a swarm manager"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}

			if _, err := prunerLogging(t, e, b, true, &buf).Departed(t.Context(), []string{"kept"}, nil); err != nil {
				t.Fatalf("Departed = %v, want nil", err)
			}
			if !strings.Contains(buf.String(), "node-local") {
				t.Errorf("log %q claims a swarm-wide purge without having established one", buf.String())
			}
		})
	}
}

// The settle budget is per volume, because the thing being waited for is per
// volume: each is held by its own containers and drains on its own schedule.
// Shared, the last volume of a stack answers for however long the first ones
// took, and the same stack passes or fails on how many volumes it declares.
func TestEachVolumeGetsItsOwnSettleBudget(t *testing.T) {
	restoreTimeout, restoreInterval := volumeSettleTimeout, volumeSettleInterval
	volumeSettleTimeout, volumeSettleInterval = 100*time.Millisecond, 0
	t.Cleanup(func() { volumeSettleTimeout, volumeSettleInterval = restoreTimeout, restoreInterval })

	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	b := &fakeBackend{
		volumes: map[string][]string{"api": {"api_slow", "api_data"}},
		// The first spends more than a whole budget, as a stack whose
		// stop_grace_period outlasts the timeout does.
		volDelay: map[string]time.Duration{"api_slow": 150 * time.Millisecond},
		// The second needs one retry, which a shared deadline can no longer
		// afford by the time it is reached.
		volInUse: map[string]int{"api_data": 1},
	}

	got, err := prunerWith(t, e, b, true).Departed(t.Context(), []string{"kept"}, nil)
	if err != nil {
		t.Fatalf("Departed = %v, want nil — the second volume has its own budget", err)
	}
	if want := []string{"api_slow", "api_data"}; !slices.Equal(b.removedVol, want) {
		t.Errorf("removed volumes %v, want %v", b.removedVol, want)
	}
	if want := []string{"gone"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pruned applications = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------- node reach

// reachingSwarms is a registry that has solved what the OSS one cannot: a
// handle to each node's own daemon. Separate from fakeSwarms rather than a
// field on it, because "does not implement swarms.NodeReach" is the case the
// whole node-local fallback hangs on and a method cannot be un-declared.
type reachingSwarms struct {
	fakeSwarms
	nodes    []swarms.Node
	nodesErr error
	// backends is the node-local daemon of each node, by hostname. A hostname
	// absent from it is a node the registry named and then cannot reach.
	backends map[string]*fakeBackend
}

func (r reachingSwarms) Nodes(context.Context, swarms.Target) ([]swarms.Node, error) {
	return r.nodes, r.nodesErr
}

func (r reachingSwarms) NodeBackend(_ context.Context, _ swarms.Target, n swarms.Node) (swarms.NodeBackend, error) {
	b, ok := r.backends[n.Hostname]
	if !ok {
		return nil, errors.New("no route to " + n.Hostname)
	}
	return b, nil
}

// node is one reachable node holding the named volumes of release "api".
func node(hostname string, volumes ...string) (swarms.Node, *fakeBackend) {
	return swarms.Node{ID: "id-" + hostname, Hostname: hostname},
		&fakeBackend{volumes: map[string][]string{"api": volumes}}
}

// prunerReaching builds a Pruner whose registry can reach nodes, over the same
// swarm-level backend the sweep itself uses.
func prunerReaching(t *testing.T, e *fakeEngine, b charts.Backend, reg reachingSwarms, w io.Writer) *Pruner {
	t.Helper()
	reg.fakeSwarms = fakeSwarms{backend: b}
	return New(Options{
		Swarms:       reg,
		Engine:       func(charts.Backend) Engine { return e },
		Volumes:      true,
		ControllerID: testController,
		Log:          slog.New(slog.NewTextHandler(w, nil)),
	})
}

// The whole of what #108 left open. A stack's ordinary named volumes live on
// whichever nodes ran its tasks, and the daemon's volume list answers from the
// node's own store — so a registry that can hand back a daemon per node is the
// only thing that makes the purge complete.
func TestVolumesArePurgedOnEveryNodeTheRegistryReaches(t *testing.T) {
	var buf bytes.Buffer
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}

	// Three nodes: the controller's own, one holding the volume the stack
	// actually scheduled, and one holding nothing.
	n1, b1 := node("manager-1")
	n2, b2 := node("worker-1", "api_data")
	n3, b3 := node("worker-2", "api_cache", "api_logs")
	reg := reachingSwarms{
		nodes:    []swarms.Node{n1, n2, n3},
		backends: map[string]*fakeBackend{"manager-1": b1, "worker-1": b2, "worker-2": b3},
	}

	// The swarm-level backend sees none of them, which is exactly what made
	// this undeletable before — and says the swarm has the three nodes the
	// registry reached, which is what lets the purge claim it covered them.
	swarmWide := sizedBackend{fakeBackend: &fakeBackend{}, nodes: 3}

	got, err := prunerReaching(t, e, swarmWide, reg, &buf).Departed(t.Context(), []string{"kept"}, nil)
	if err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}

	if len(b1.removedVol) != 0 {
		t.Errorf("node with no volumes had %v removed", b1.removedVol)
	}
	if want := []string{"api_data"}; !slices.Equal(b2.removedVol, want) {
		t.Errorf("worker-1 removed %v, want %v", b2.removedVol, want)
	}
	if want := []string{"api_cache", "api_logs"}; !slices.Equal(b3.removedVol, want) {
		t.Errorf("worker-2 removed %v, want %v", b3.removedVol, want)
	}
	if len(swarmWide.removedVol) != 0 {
		t.Errorf("the swarm-level backend was asked to remove %v; the purge is per node now", swarmWide.removedVol)
	}

	log := buf.String()
	if !strings.Contains(log, "every node of the swarm") {
		t.Errorf("log %q does not report the purge as swarm-wide", log)
	}
	// Node attribution, because "api_data was deleted" is not something an
	// operator can check without knowing where it was.
	if !strings.Contains(log, "worker-1/api_data") {
		t.Errorf("log %q does not say which node each volume came off", log)
	}
	// And none of the node-local hedging, which would now be false.
	if strings.Contains(log, "node-local") {
		t.Errorf("log %q still warns about nodes it did reach", log)
	}
	if want := []string{"gone"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pruned applications = %v, want %v", got, want)
	}
}

// A registry that cannot reach nodes leaves everything exactly as it was: the
// node-local purge, and the warning that says so. This is the OSS build, and it
// is the floor the seam must not lower.
func TestARegistryWithoutNodeReachKeepsTheNodeLocalPurge(t *testing.T) {
	var buf bytes.Buffer
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	b := sizedBackend{
		fakeBackend: &fakeBackend{volumes: map[string][]string{"api": {"api_data"}}},
		nodes:       3,
	}

	if _, err := prunerLogging(t, e, b, true, &buf).Departed(t.Context(), []string{"kept"}, nil); err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}

	if want := []string{"api_data"}; !slices.Equal(b.removedVol, want) {
		t.Errorf("removed %v, want %v from the controller's own node", b.removedVol, want)
	}
	log := buf.String()
	if !strings.Contains(log, "node-local") {
		t.Errorf("log %q does not say the listing was node-local", log)
	}
	if strings.Contains(log, "every node of the swarm") {
		t.Errorf("log %q claims a swarm-wide purge from a registry that reaches one node", log)
	}
}

// A registry implementing swarms.NodeReach has claimed it can reach these
// nodes, so a node it then cannot list or delete on is a real failure — not the
// expected partial answer the node-local purge lives with. The release records
// stay, and the next sweep tries again.
func TestAPurgeAcrossNodesFailsRatherThanCoveringPart(t *testing.T) {
	reachable, backend := node("worker-1", "api_data")
	unreachable := swarms.Node{ID: "id-worker-2", Hostname: "worker-2"}

	cases := map[string]struct {
		reg  reachingSwarms
		want string
	}{
		"the node list could not be read": {
			reg:  reachingSwarms{nodesErr: errors.New("the proxy is down")},
			want: "the proxy is down",
		},
		// Not "there were no volumes". A registry that reaches nothing has not
		// looked, and treating that as a finished purge would delete the records
		// that are the only way to find this release again.
		"the registry reaches no node": {
			reg:  reachingSwarms{nodes: nil},
			want: "reaches no node",
		},
		"a node cannot be reached": {
			reg: reachingSwarms{
				nodes:    []swarms.Node{reachable, unreachable},
				backends: map[string]*fakeBackend{"worker-1": backend},
			},
			want: "worker-2",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}

			got, err := prunerReaching(t, e, &fakeBackend{}, tc.reg, io.Discard).
				Departed(t.Context(), []string{"kept"}, nil)
			if err == nil {
				t.Fatal("Departed = nil, want the purge failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if len(got) != 0 {
				t.Errorf("pruned = %v, want nothing reported as pruned", got)
			}
			// The records carry the owner stamp, which is the only thing a
			// later sweep finds this release by.
			if len(e.pruned()) != 0 {
				t.Errorf("uninstalled %v; a failed purge must keep the release records", e.pruned())
			}
		})
	}
}

// The settle budget is per volume on every node too. The containers holding a
// volume on worker-2 drain on their own schedule, not on whatever worker-1's
// took.
func TestEachNodesVolumesGetTheirOwnSettleBudget(t *testing.T) {
	restoreTimeout, restoreInterval := volumeSettleTimeout, volumeSettleInterval
	volumeSettleTimeout, volumeSettleInterval = 100*time.Millisecond, 0
	t.Cleanup(func() { volumeSettleTimeout, volumeSettleInterval = restoreTimeout, restoreInterval })

	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	n1, b1 := node("worker-1", "api_data")
	n2, b2 := node("worker-2", "api_data")
	// The first node's volume spends more than a whole budget, as a stack
	// whose stop_grace_period outlasts the timeout does.
	b1.volDelay = map[string]time.Duration{"api_data": 150 * time.Millisecond}
	// The second needs one retry, which a budget shared across the swarm can no
	// longer afford by the time this node is reached.
	b2.volInUse = map[string]int{"api_data": 1}
	reg := reachingSwarms{
		nodes:    []swarms.Node{n1, n2},
		backends: map[string]*fakeBackend{"worker-1": b1, "worker-2": b2},
	}

	if _, err := prunerReaching(t, e, &fakeBackend{}, reg, io.Discard).
		Departed(t.Context(), []string{"kept"}, nil); err != nil {
		t.Fatalf("Departed = %v, want nil — the second node's volume has its own budget", err)
	}
	for host, b := range map[string]*fakeBackend{"worker-1": b1, "worker-2": b2} {
		if want := []string{"api_data"}; !slices.Equal(b.removedVol, want) {
			t.Errorf("%s removed %v, want %v", host, b.removedVol, want)
		}
	}
}

// A release that declared no volumes says nothing, on either path. Every node
// was asked, so an empty answer really is an empty answer — but it is still not
// a deletion, and the line an operator goes looking for is about data going.
func TestAPurgeAcrossNodesThatFoundNothingSaysNothing(t *testing.T) {
	var buf bytes.Buffer
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	n1, b1 := node("worker-1")
	reg := reachingSwarms{nodes: []swarms.Node{n1}, backends: map[string]*fakeBackend{"worker-1": b1}}
	swarmWide := sizedBackend{fakeBackend: &fakeBackend{}, nodes: 1}

	if _, err := prunerReaching(t, e, swarmWide, reg, &buf).
		Departed(t.Context(), []string{"kept"}, nil); err != nil {
		t.Fatalf("Departed = %v, want nil", err)
	}
	if strings.Contains(buf.String(), "deleted the release's volumes") {
		t.Errorf("log %q reports a deletion for a release that had no volumes", buf.String())
	}
}

// The registry hands over the nodes it can reach, and Nodes documents that one
// it cannot connect to should be left out — which a swarm with a node down
// routinely produces. Treating that list as the whole swarm would log a
// completed purge and then delete the release records, which are the only thing
// a later sweep finds the release by: #108 re-created one layer up, inside the
// build that was supposed to have fixed it.
func TestAPurgeThatCouldNotCoverEveryNodeDoesNotClaimItDid(t *testing.T) {
	cases := map[string]charts.Backend{
		// Four nodes in the swarm, two reached.
		"the swarm has more nodes than the registry reached": sizedBackend{
			fakeBackend: &fakeBackend{}, nodes: 4,
		},
		// A count that could not be read is not a match, for the reason a count
		// that could not be read is not one node.
		"the node count could not be read": sizedBackend{
			fakeBackend: &fakeBackend{}, err: errors.New("this node is not a swarm manager"),
		},
		"the backend cannot answer at all": &fakeBackend{},
	}

	for name, swarmWide := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
			n1, b1 := node("worker-1", "api_data")
			n2, b2 := node("worker-2")
			reg := reachingSwarms{
				nodes:    []swarms.Node{n1, n2},
				backends: map[string]*fakeBackend{"worker-1": b1, "worker-2": b2},
			}

			if _, err := prunerReaching(t, e, swarmWide, reg, &buf).
				Departed(t.Context(), []string{"kept"}, nil); err != nil {
				t.Fatalf("Departed = %v, want nil", err)
			}

			// What it reached, it still deletes. Refusing would make the purge
			// fail permanently on every swarm with a node down.
			if want := []string{"api_data"}; !slices.Equal(b1.removedVol, want) {
				t.Errorf("worker-1 removed %v, want %v", b1.removedVol, want)
			}

			log := buf.String()
			if strings.Contains(log, "every node of the swarm") {
				t.Errorf("log %q claims a swarm-wide purge it could not establish", log)
			}
			if !strings.Contains(log, "could not be shown to be all of them") {
				t.Errorf("log %q does not say the coverage is unproven", log)
			}
			// Named, so an operator can tell which nodes were covered from the
			// ones that were not.
			if !strings.Contains(log, "worker-1") || !strings.Contains(log, "worker-2") {
				t.Errorf("log %q does not name the nodes that were covered", log)
			}
			if !strings.Contains(log, "label=com.docker.stack.namespace=api") {
				t.Errorf("log %q does not hand over the filter that finds what is left", log)
			}
		})
	}
}

// A purge that gives up part-way through has already deleted data irreversibly,
// and the error names only the node it stopped on. Something has to say what
// went, or the one destructive thing this package does happens silently.
func TestAPurgeThatFailsPartWayReportsWhatItAlreadyDeleted(t *testing.T) {
	var buf bytes.Buffer
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	done, b := node("worker-1", "api_data")
	unreachable := swarms.Node{ID: "id-worker-2", Hostname: "worker-2"}
	reg := reachingSwarms{
		nodes:    []swarms.Node{done, unreachable},
		backends: map[string]*fakeBackend{"worker-1": b},
	}

	if _, err := prunerReaching(t, e, &fakeBackend{}, reg, &buf).
		Departed(t.Context(), []string{"kept"}, nil); err == nil {
		t.Fatal("Departed = nil, want the purge failure")
	}

	if want := []string{"api_data"}; !slices.Equal(b.removedVol, want) {
		t.Fatalf("worker-1 removed %v, want %v — nothing was deleted, so there is nothing to report", b.removedVol, want)
	}
	log := buf.String()
	if !strings.Contains(log, "before the purge failed") {
		t.Errorf("log %q does not report the volumes that went before it gave up", log)
	}
	if !strings.Contains(log, "worker-1/api_data") {
		t.Errorf("log %q does not name what was deleted", log)
	}
}

// A registry that gave no hostname is still named in the error, by its id.
// Otherwise the one thing an operator has to go on is an empty string.
func TestANodeWithNoHostnameIsNamedByItsID(t *testing.T) {
	e := &fakeEngine{releases: []charts.Release{owned("api", "gone")}}
	reg := reachingSwarms{nodes: []swarms.Node{{ID: "z3k9q1"}}}

	_, err := prunerReaching(t, e, &fakeBackend{}, reg, io.Discard).
		Departed(t.Context(), []string{"kept"}, nil)
	if err == nil {
		t.Fatal("Departed = nil, want the unreachable node's failure")
	}
	if !strings.Contains(err.Error(), "z3k9q1") {
		t.Errorf("error %q does not name the node by its id", err)
	}
}
