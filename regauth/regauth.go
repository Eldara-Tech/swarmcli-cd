// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package regauth turns an application's registry credential into the encoded
// auth the moby client sends when it creates or updates a service.
//
// The credential is a docker config.json ({"auths": {...}}) — the same file an
// operator gets from `docker login` — delivered to the controller as a Docker
// secret, per the seam-vs-configuration line drawn in package secrets. It is
// scoped per application: the reconciler hands each application only the
// resolver built from its own secret, so one application's manifest cannot
// borrow another's credential to pull a private image.
//
// The parsing, the Docker Hub index-key special-casing and the auths → header
// transform are docker/cli's own (config.LoadFromReader plus
// command.RetrieveAuthTokenFromImage), not reimplemented here.
package regauth

import (
	"bytes"
	"fmt"
	"path/filepath"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/config"

	"github.com/Eldara-Tech/swarmcli-cd/application"
)

// DefaultSecretsDir is where Swarm mounts a service's secrets. An application's
// registryAuth names a secret, and the controller reads it from here.
const DefaultSecretsDir = "/run/secrets"

// Resolver returns the base64 X-Registry-Auth value for an image, as
// swarm.ServiceCreateOptions.EncodedRegistryAuth expects it. An image whose
// registry has no entry in the config resolves to an anonymous credential,
// which is the same thing an unauthenticated pull sends.
type Resolver func(image string) (string, error)

// FromConfig builds a Resolver from the bytes of a docker config.json.
//
// A credsStore or credHelpers entry is rejected rather than honoured: resolving
// it would exec a docker credential helper, and the controller image ships
// none. Failing here names that limitation at startup instead of surfacing it
// as an exec error mid-deploy — which matters most for exactly the short-lived
// cloud-registry tokens (ECR, GCR) a helper would refresh.
func FromConfig(data []byte) (Resolver, error) {
	cfg, err := config.LoadFromReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parsing registry config: %w", err)
	}
	if cfg.CredentialsStore != "" || len(cfg.CredentialHelpers) > 0 {
		return nil, fmt.Errorf("registry config uses a credential store or helper, which this controller cannot run: " +
			"it ships no docker credential helpers. Use static \"auths\" entries instead (short-lived ECR/GCR tokens " +
			"must be refreshed out of band for now)")
	}
	return func(image string) (string, error) {
		return command.RetrieveAuthTokenFromImage(cfg, image)
	}, nil
}

// Load builds a resolver for every application that declares registryAuth,
// reading each named secret from secretsDir through readFile (os.ReadFile in
// production, a fake in tests). An application without registryAuth gets no
// entry and its pulls stay anonymous.
//
// A named secret that is missing or unparseable is fatal. A controller that
// started anyway would fail every deploy of that application with an
// unauthenticated pull — the convergence timeout naming nothing that #30 exists
// to eliminate — so the failure is named here, before the loop starts.
func Load(apps []application.Spec, secretsDir string, readFile func(string) ([]byte, error)) (map[string]Resolver, error) {
	out := make(map[string]Resolver)
	for _, app := range apps {
		if app.RegistryAuth == "" {
			continue
		}
		path := filepath.Join(secretsDir, app.RegistryAuth)
		data, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("application %q: registryAuth secret %q is not mounted at %s — "+
				"add it to the controller's secrets in stack.yml: %w", app.Name, app.RegistryAuth, path, err)
		}
		resolver, err := FromConfig(data)
		if err != nil {
			return nil, fmt.Errorf("application %q: registryAuth secret %q: %w", app.Name, app.RegistryAuth, err)
		}
		out[app.Name] = resolver
	}
	return out, nil
}
