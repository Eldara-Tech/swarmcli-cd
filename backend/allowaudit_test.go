// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
)

// What a recorded manifest needs the allowlist to name: every external:
// reference, and every volume or cluster mount that is not the release's own,
// that the allowlist does not name already — each once, sorted. What the
// release declares needs nothing; nor does what the allowlist names, nor what
// belongs to the controller, which no entry could grant. A bind does not stop
// the manifest being read, since binds are not what this reports.
func TestUnpermittedNamesAreWhatADeployWouldBeRefusedFor(t *testing.T) {
	const manifest = `
services:
  app:
    image: busybox
    secrets: [db, own, token]
    configs: [site, other]
    networks: [front, public, outside]
    volumes:
      - data:/data
      - shared:/shared
      - /var/run/docker.sock:/var/run/docker.sock
      - {type: cluster, source: "group:db", target: /csi}
  worker:
    image: busybox
    secrets: [db]
secrets:
  db: {external: true, name: web_db}
  own: {driver: vault}
  token: {external: true, name: swarmcli-cd-token}
configs:
  site: {external: true, name: web_site}
  other: {external: true, name: shared-site}
networks:
  front: {}
  public: {external: true, name: web_public}
  outside: {name: shared-net}
volumes:
  data: {}
  shared: {external: true, name: web_shared}
`
	got, err := testBackend(t, asController(&fakeAPI{}), nil).UnpermittedNames(t.Context(), capability.AllowRequest{
		ManifestRequest: capability.ManifestRequest{Name: "web", Manifest: manifest},
		Allow:           application.Allow{Configs: []string{"shared-site"}},
	})
	if err != nil {
		t.Fatalf("UnpermittedNames = %v, want the manifest read", err)
	}
	want := application.Allow{
		Secrets:  []string{"web_db"},
		Configs:  []string{"web_site"},
		Volumes:  []string{"group:db", "web_shared"},
		Networks: []string{"shared-net", "web_public"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UnpermittedNames = %+v, want %+v", got, want)
	}
}

// A manifest that does not convert, or a daemon that cannot say what the
// controller holds, is the caller's to report — not an empty answer.
func TestUnpermittedNamesThatCannotBeWorkedOutAreAnError(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		api            *fakeAPI
	}{
		{"unconvertible", "services: [", &fakeAPI{}},
		{"unreadable controller", "services:\n  app:\n    image: busybox\n", asController(&fakeAPI{selfErr: errors.New("daemon busy")})},
	} {
		if _, err := testBackend(t, tc.api, nil).UnpermittedNames(t.Context(), capability.AllowRequest{
			ManifestRequest: capability.ManifestRequest{Name: "web", Manifest: tc.manifest},
		}); err == nil {
			t.Errorf("%s: UnpermittedNames = nil error, want the failure", tc.name)
		}
	}
}
