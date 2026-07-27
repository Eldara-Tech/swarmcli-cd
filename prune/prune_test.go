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
	"strings"
	"testing"

	"github.com/Eldara-Tech/swarmcli/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

type fakeSwarms struct{ err error }

func (f fakeSwarms) Backend(context.Context, string) (charts.Backend, error) { return nil, f.err }

type uninstalled struct {
	release string
	volumes bool
}

type fakeEngine struct {
	releases []charts.Release
	listErr  error
	fail     map[string]error
	networks map[string][]string

	calls []uninstalled
}

func (e *fakeEngine) List(context.Context) ([]charts.Release, error) {
	return e.releases, e.listErr
}

func (e *fakeEngine) Uninstall(_ context.Context, release string, purgeVolumes bool) (*charts.UninstallResult, error) {
	e.calls = append(e.calls, uninstalled{release, purgeVolumes})
	if err := e.fail[release]; err != nil {
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
	return New(Options{
		Swarms:  fakeSwarms{},
		Engine:  func(charts.Backend) Engine { return e },
		Volumes: volumes,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// owned builds a release carrying a well-formed stamp for one of this
// controller's applications, the way the reconciler writes it.
func owned(release, app string) charts.Release {
	return stamped(release, charts.OwnerRef{
		ID:   application.OwnerID(app),
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
		"still in the set":        owned("api", "kept"),
		"installed from the CLI":  stamped("api", "apply/prod:release/api"),
		"another tool's stamp":    stamped("api", "flux/prod:release/api"),
		"no stamp at all":         stamped("api", ""),
		"unparseable stamp":       stamped("api", "not-an-owner-ref"),
		"stamp names another rel": stamped("api", "cd/gone:release/web"),
		"stamp is a bare cd/":     stamped("api", "cd/:release/api"),
		"stamp of the wrong kind": stamped("api", "cd/gone:stack/api"),
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

		if _, err := testPruner(t, e, volumes).Departed(t.Context(), []string{"kept"}); err != nil {
			t.Fatalf("Departed = %v, want nil", err)
		}
		if len(e.calls) != 1 || e.calls[0].volumes != volumes {
			t.Errorf("uninstall calls = %+v, want one with volumes=%v", e.calls, volumes)
		}
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
		Swarms: fakeSwarms{},
		Engine: func(charts.Backend) Engine {
			return &fakeEngine{
				releases: []charts.Release{owned("api", "gone")},
				networks: map[string][]string{"api": {"shared-net"}},
			}
		},
		Log: slog.New(slog.NewTextHandler(&buf, nil)),
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

	got := departed(releases, []string{"kept"})
	want := []departedApp{
		{name: "alpha", releases: []string{"a-api", "c-web"}},
		{name: "beta", releases: []string{"b-api"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("departed = %+v, want %+v", got, want)
	}
}

func TestOwnerRejectsWhatItCannotProve(t *testing.T) {
	if app, ok := owner(owned("api", "edge")); !ok || app != "edge" {
		t.Errorf("owner(well-formed) = %q, %v; want edge, true", app, ok)
	}
	for _, rel := range []charts.Release{
		stamped("api", ""),
		stamped("api", "garbage"),
		stamped("api", "cd/edge:release/other"),
		stamped("api", "apply/edge:release/api"),
	} {
		if app, ok := owner(rel); ok {
			t.Errorf("owner(%q) = %q, true; want it rejected", rel.Owner, app)
		}
	}
}
