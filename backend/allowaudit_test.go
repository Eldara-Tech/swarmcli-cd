// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
)

// What a recorded manifest needs the allowlist to name: every external:
// reference, every volume or cluster mount that is not the release's own —
// including a cluster mount scoped under the release — every network joined or
// declared outside the release, and every declared config or secret that is not
// the release's own, from every service, each once, sorted — and a volume of its
// own with driver options. What the release declares as its own needs nothing;
// nor does what the allowlist names, in each of its lists; nor what no entry
// could grant: what belongs to the controller, or a volume declared under a
// name outside the release, with driver options or without.
// A bind does not stop the manifest being read, since binds are not what this
// reports.
func TestUnpermittedNamesAreWhatADeployWouldBeRefusedFor(t *testing.T) {
	const manifest = `
services:
  app:
    image: busybox
    secrets: [db, own, token, declared, record]
    configs: [site, other, apps, shipped]
    networks: [front, public, outside, allowed, theirs]
    volumes:
      - data:/data
      - shared:/shared
      - ok:/ok
      - ctl:/ctl
      - /var/run/docker.sock:/var/run/docker.sock
      - {type: cluster, source: "group:db", target: /csi}
      - {type: cluster, source: scoped, target: /scoped}
      - optioned:/optioned
      - foreign:/foreign
      - borrowed:/borrowed
  worker:
    image: busybox
    secrets: [key, db]
secrets:
  db: {external: true, name: web_db}
  key: {external: true, name: web_key}
  own: {driver: vault}
  token: {external: true, name: swarmcli-cd-token}
  declared: {name: shared-token, driver: vault}
  record: {name: swarmcli.release.web.v9, driver: vault}
configs:
  site: {external: true, name: web_site}
  other: {external: true, name: shared-site}
  apps: {external: true, name: swarmcli-cd-applications}
  shipped: {name: shared-conf, file: files/decoy.conf}
networks:
  front: {}
  public: {external: true, name: web_public}
  outside: {name: shared-net}
  allowed: {external: true, name: traefik-public}
  theirs: {external: true, name: swarmcli-cd_default}
volumes:
  data: {}
  scoped: {}
  shared: {external: true, name: web_shared}
  ok: {external: true, name: web_ok}
  ctl: {external: true, name: swarmcli-cd_swarmcli-cd-data}
  optioned: {driver_opts: {type: tmpfs, device: tmpfs}}
  foreign: {name: shared-data, driver: vieux/sshfs}
  borrowed: {name: shared-plain}
`
	got, err := testBackend(t, asController(&fakeAPI{}), nil).UnpermittedNames(t.Context(), capability.AllowRequest{
		ManifestRequest: capability.ManifestRequest{Name: "web", Manifest: manifest, Files: decoyFiles},
		Allow: application.Allow{
			Secrets: []string{"web_key"}, Configs: []string{"shared-site"},
			Volumes: []string{"web_ok"}, Networks: []string{"traefik-public"},
		},
	})
	if err != nil {
		t.Fatalf("UnpermittedNames = %v, want the manifest read", err)
	}
	want := application.Allow{
		Secrets:  []string{"shared-token", "web_db"},
		Configs:  []string{"shared-conf", "web_site"},
		Volumes:  []string{"group:db", "web_optioned", "web_scoped", "web_shared"},
		Networks: []string{"shared-net", "web_public"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UnpermittedNames = %+v, want %+v", got, want)
	}
}

// The answer is the guard's: a deploy under the allowlist plus what
// UnpermittedNames returned goes through, and one missing any single one of
// those entries is refused. A declared config held by another stack is among
// them, as the guard reads it.
func TestUnpermittedNamesAgreeWithTheGuard(t *testing.T) {
	const manifest = `
services:
  app:
    image: busybox
    secrets: [db]
    configs: [site, held]
    networks: [public]
    volumes: ["shared:/shared", "optioned:/optioned", {type: cluster, source: scoped, target: /scoped}]
secrets:
  db: {external: true, name: web_db}
configs:
  site: {external: true, name: web_site}
  held: {name: web_held, file: files/decoy.conf}
networks:
  public: {external: true, name: web_public}
volumes:
  scoped: {}
  optioned: {driver_opts: {type: tmpfs, device: tmpfs}}
  shared: {external: true, name: web_shared}
`
	api := func() *fakeAPI {
		return asController(&fakeAPI{
			secrets: []swarm.Secret{{ID: "s", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "web_db"}}}},
			configs: []swarm.Config{
				{ID: "c", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "web_site"}}},
				{ID: "h", Spec: swarm.ConfigSpec{Annotations: stackScoped("web_held", "web_a"), Data: decoyFiles["files/decoy.conf"]}},
			},
		})
	}
	need, err := testBackend(t, api(), nil).UnpermittedNames(t.Context(), capability.AllowRequest{
		ManifestRequest: capability.ManifestRequest{Name: "web", Manifest: manifest, Files: decoyFiles},
	})
	if err != nil {
		t.Fatalf("UnpermittedNames = %v", err)
	}
	if err := allowing(t, api(), need).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: manifest, Files: decoyFiles, Resolve: ResolveNever}); err != nil {
		t.Fatalf("DeployStack under %+v = %v, want it deployed", need, err)
	}
	for _, field := range []func(*application.Allow) *[]string{
		func(a *application.Allow) *[]string { return &a.Secrets },
		func(a *application.Allow) *[]string { return &a.Configs },
		func(a *application.Allow) *[]string { return &a.Volumes },
		func(a *application.Allow) *[]string { return &a.Networks },
	} {
		names := *field(&need)
		for i, name := range names {
			less := need
			*field(&less) = slices.Delete(slices.Clone(names), i, i+1)
			if err := allowing(t, api(), less).DeployStack(t.Context(), charts.DeployRequest{Name: "web", Manifest: manifest, Files: decoyFiles, Resolve: ResolveNever}); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("DeployStack without %s = %v, want it refused naming it", name, err)
			}
		}
	}
	if n := len(need.Secrets) + len(need.Configs) + len(need.Volumes) + len(need.Networks); n != 7 {
		t.Errorf("UnpermittedNames = %+v, want seven names", need)
	}
}

// A manifest that does not convert, or a daemon that cannot say what the
// controller holds or whose a declared name is, is the caller's to report — not
// an empty answer.
func TestUnpermittedNamesThatCannotBeWorkedOutAreAnError(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		api            *Backend
	}{
		{"unconvertible", "services: [", testBackend(t, &fakeAPI{}, nil)},
		{"unreadable controller", "services:\n  app:\n    image: busybox\n", testBackend(t, asController(&fakeAPI{selfErr: errors.New("daemon busy")}), nil)},
		{"unreadable secret", declaresAndMounts, testBackend(t, &lookupErrAPI{fakeAPI: &fakeAPI{}, kind: "secret", err: errors.New("daemon busy")}, nil)},
		{"unreadable config", shipsAConfig, testBackend(t, &lookupErrAPI{fakeAPI: &fakeAPI{}, kind: "config", err: errors.New("daemon busy")}, nil)},
	} {
		if _, err := tc.api.UnpermittedNames(t.Context(), capability.AllowRequest{
			ManifestRequest: capability.ManifestRequest{Name: "web", Manifest: tc.manifest, Files: map[string][]byte{"files/nginx.conf": []byte("x")}},
		}); err == nil {
			t.Errorf("%s: UnpermittedNames = nil error, want the failure", tc.name)
		}
	}
}
