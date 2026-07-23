// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package regauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	registrytypes "github.com/docker/docker/api/types/registry"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// dockerConfig renders the config.json an operator's `docker login` writes, with
// one host per credential.
func dockerConfig(t *testing.T, creds map[string]string) []byte {
	t.Helper()
	auths := map[string]map[string]string{}
	for host, userPass := range creds {
		auths[host] = map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(userPass))}
	}
	data, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		t.Fatalf("marshalling config: %v", err)
	}
	return data
}

// decode resolves an encoded X-Registry-Auth back to its username and password.
func decode(t *testing.T, encoded string) registrytypes.AuthConfig {
	t.Helper()
	ac, err := registrytypes.DecodeAuthConfig(encoded)
	if err != nil {
		t.Fatalf("decoding auth %q: %v", encoded, err)
	}
	return *ac
}

func TestResolvesMatchingHost(t *testing.T) {
	resolve, err := FromConfig(dockerConfig(t, map[string]string{"ghcr.io": "team-a:pat-123"}))
	if err != nil {
		t.Fatalf("FromConfig = %v, want nil", err)
	}

	encoded, err := resolve("ghcr.io/team-a/app:1.2")
	if err != nil {
		t.Fatalf("resolve = %v, want nil", err)
	}
	got := decode(t, encoded)
	if got.Username != "team-a" || got.Password != "pat-123" {
		t.Errorf("resolved %q/%q, want team-a/pat-123", got.Username, got.Password)
	}
}

// A bare Docker Hub image resolves under the historical index key, which is the
// key `docker login` writes and the reason this is not a plain host match.
func TestResolvesDockerHubShortName(t *testing.T) {
	resolve, err := FromConfig(dockerConfig(t, map[string]string{"https://index.docker.io/v1/": "hubuser:hubpass"}))
	if err != nil {
		t.Fatalf("FromConfig = %v, want nil", err)
	}

	encoded, err := resolve("nginx")
	if err != nil {
		t.Fatalf("resolve = %v, want nil", err)
	}
	if got := decode(t, encoded); got.Username != "hubuser" {
		t.Errorf("username = %q, want hubuser", got.Username)
	}
}

// An image whose registry is not in the config resolves to an anonymous
// credential — the same thing an unauthenticated pull sends — rather than an
// error, so a stack mixing public and private images still deploys.
func TestUnmatchedHostIsAnonymous(t *testing.T) {
	resolve, err := FromConfig(dockerConfig(t, map[string]string{"ghcr.io": "team-a:pat-123"}))
	if err != nil {
		t.Fatalf("FromConfig = %v, want nil", err)
	}

	encoded, err := resolve("quay.io/other/image:latest")
	if err != nil {
		t.Fatalf("resolve = %v, want nil", err)
	}
	if got := decode(t, encoded); got.Username != "" || got.Password != "" {
		t.Errorf("resolved %q/%q, want anonymous", got.Username, got.Password)
	}
}

func TestMalformedConfigIsRejected(t *testing.T) {
	if _, err := FromConfig([]byte("{not json")); err == nil {
		t.Fatal("FromConfig = nil, want an error for malformed config")
	}
}

// A credential store or helper cannot be honoured: the controller image ships no
// helper binaries, so this must fail at load rather than exec a missing helper
// mid-deploy.
func TestCredentialHelperIsRejected(t *testing.T) {
	for _, cfg := range []string{
		`{"credsStore":"ecr-login"}`,
		`{"credHelpers":{"public.ecr.aws":"ecr-login"}}`,
	} {
		if _, err := FromConfig([]byte(cfg)); err == nil {
			t.Errorf("FromConfig(%s) = nil, want an error naming the credential-helper limitation", cfg)
		}
	}
}

func TestLoadBuildsResolversForDeclaringApps(t *testing.T) {
	apps := []application.Spec{
		{Name: "team-a", RegistryAuth: "regauth-a"},
		{Name: "public", RegistryAuth: ""},
	}
	files := map[string][]byte{
		"/run/secrets/regauth-a": dockerConfig(t, map[string]string{"ghcr.io": "team-a:pat"}),
	}
	read := func(p string) ([]byte, error) {
		data, ok := files[p]
		if !ok {
			return nil, os.ErrNotExist
		}
		return data, nil
	}

	resolvers, err := Load(apps, "/run/secrets", read)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if _, ok := resolvers["team-a"]; !ok {
		t.Error("team-a has no resolver, want one")
	}
	if _, ok := resolvers["public"]; ok {
		t.Error("an application with no registryAuth got a resolver, want none")
	}
}

func TestLoadFailsOnMissingSecret(t *testing.T) {
	apps := []application.Spec{{Name: "team-a", RegistryAuth: "regauth-a"}}
	read := func(string) ([]byte, error) { return nil, os.ErrNotExist }

	_, err := Load(apps, "/run/secrets", read)
	if err == nil {
		t.Fatal("Load = nil, want an error for a missing secret")
	}
	// The error must name the application and the secret so an operator knows
	// what to add to stack.yml.
	if !strings.Contains(err.Error(), "team-a") || !strings.Contains(err.Error(), "regauth-a") {
		t.Errorf("error %q does not name the application and secret", err)
	}
}

func TestLoadFailsOnUnparseableSecret(t *testing.T) {
	apps := []application.Spec{{Name: "team-a", RegistryAuth: "regauth-a"}}
	read := func(string) ([]byte, error) { return []byte("{not json"), nil }

	if _, err := Load(apps, "/run/secrets", read); err == nil {
		t.Fatal("Load = nil, want an error for an unparseable secret")
	}
}

// Load joins the secret name onto the secrets dir, so the config validator's
// charset is what keeps that join inside the directory. This is a belt-and-braces
// check that a well-formed name lands where expected.
func TestLoadReadsFromSecretsDir(t *testing.T) {
	apps := []application.Spec{{Name: "team-a", RegistryAuth: "regauth-a"}}
	var readPath string
	read := func(p string) ([]byte, error) {
		readPath = p
		return dockerConfig(t, map[string]string{"ghcr.io": "u:p"}), nil
	}

	if _, err := Load(apps, "/custom/secrets", read); err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if want := fmt.Sprintf("/custom/secrets/%s", "regauth-a"); readPath != want {
		t.Errorf("read %q, want %q", readPath, want)
	}
}
