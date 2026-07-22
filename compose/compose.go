// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package compose turns a rendered compose manifest into the Swarm specs a
// stack is made of.
//
// It is the half of charts.Backend that needs no daemon: a manifest string goes
// in, an ordered set of service, network, config and secret specs comes out.
// Applying them is backend's job.
//
// The transformation is docker/cli's own — cli/compose/{loader,schema,convert},
// which are exported — rather than a second implementation. `docker stack
// deploy` is unusable as an applier for the reasons in swarmcli-cd#1 (--prune
// touches services only and swallows its own list error, networks are silently
// never updated, no dry-run, --detach returns before convergence, update order
// is Go map iteration), but every one of those is a defect of the *command*,
// not of the conversion underneath it.
package compose

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/cli/cli/compose/loader"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
)

// Stack is everything one rendered manifest says should exist.
//
// Every slice is sorted by name. Go map iteration order is one of the named
// defects of `docker stack deploy`, and reproducing it here would be
// self-inflicted: two reconciles of an unchanged manifest must produce the same
// work list in the same order, or a diff of the plan is noise and an operator
// reading the log sees a different deploy every time.
type Stack struct {
	// Namespace scopes every name. A stack is a name prefix plus a
	// com.docker.stack.namespace label — Swarm has no /stacks endpoint, no
	// server-side desired state and no owner references.
	Namespace convert.Namespace
	Services  []Service
	Networks  []Network
	Configs   []swarm.ConfigSpec
	Secrets   []swarm.SecretSpec
	// ExternalNetworks names networks the manifest expects to already exist.
	// They are not ours to create, and a missing one is a pre-flight failure
	// rather than something to conjure.
	ExternalNetworks []string
}

// Service pairs the name the manifest used with the spec it produced.
//
// Both are needed and neither is derivable from the other in general: Spec.Name
// is namespace-scoped, and Namespace.Descope would be a guess for a service
// whose own name contains the separator.
type Service struct {
	// Name is the service's name in the manifest, unscoped.
	Name string
	Spec swarm.ServiceSpec
}

// Network pairs a network's name on the swarm with what to create.
type Network struct {
	// Name is already namespace-scoped, unless the manifest set an explicit
	// `name:`, in which case it is that.
	Name string
	Spec network.CreateOptions
}

// Convert loads a rendered compose manifest and converts it to Swarm specs.
//
// The api client is used only for what conversion genuinely cannot do offline:
// resolving the secret and config names a service references to their ids, and
// reading the negotiated API version that gates a few spec fields. Nothing is
// written.
func Convert(ctx context.Context, manifest, stack string, api client.APIClient) (*Stack, error) {
	dict, err := loader.ParseYAML([]byte(manifest))
	if err != nil {
		return nil, fmt.Errorf("parsing the manifest: %w", err)
	}
	if err := checkBindSources(dict); err != nil {
		return nil, err
	}

	cfg, err := loader.Load(composetypes.ConfigDetails{
		// A rendered manifest is not a file, so there is no directory for a
		// relative path to mean anything against; checkBindSources has already
		// refused the one case where the loader would have used this.
		WorkingDir:  "/",
		ConfigFiles: []composetypes.ConfigFile{{Config: dict}},
	}, func(o *loader.Options) {
		// A chart resolved its own templating before this manifest existed, so
		// a surviving ${FOO} is either a literal the chart meant or an
		// accident. Interpolating it would make the controller's environment —
		// whatever its own stack.yml happens to give it — an invisible input to
		// every application's deployment, and would silently substitute the
		// empty string for anything unset. Schema validation stays on.
		o.SkipInterpolation = true
	})
	if err != nil {
		return nil, fmt.Errorf("loading the manifest: %w", err)
	}

	ns := convert.NewNamespace(stack)

	specs, err := convert.Services(ctx, ns, cfg, api)
	if err != nil {
		return nil, fmt.Errorf("converting services: %w", err)
	}
	services := make([]Service, 0, len(specs))
	for name, spec := range specs {
		services = append(services, Service{Name: name, Spec: spec})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

	created, external := convert.Networks(ns, cfg.Networks, declaredNetworks(cfg.Services))
	networks := make([]Network, 0, len(created))
	for name, spec := range created {
		networks = append(networks, Network{Name: name, Spec: spec})
	}
	sort.Slice(networks, func(i, j int) bool { return networks[i].Name < networks[j].Name })
	sort.Strings(external)

	secrets, err := convert.Secrets(ns, cfg.Secrets)
	if err != nil {
		return nil, fmt.Errorf("converting secrets: %w", err)
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })

	configs, err := convert.Configs(ns, cfg.Configs)
	if err != nil {
		return nil, fmt.Errorf("converting configs: %w", err)
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].Name < configs[j].Name })

	return &Stack{
		Namespace:        ns,
		Services:         services,
		Networks:         networks,
		Configs:          configs,
		Secrets:          secrets,
		ExternalNetworks: external,
	}, nil
}

// declaredNetworks is the set of networks the services reference, which is what
// decides which of the manifest's networks are actually created. A service that
// names none joins "default", exactly as `docker stack deploy` has it.
func declaredNetworks(services []composetypes.ServiceConfig) map[string]struct{} {
	out := map[string]struct{}{}
	for _, svc := range services {
		if len(svc.Networks) == 0 {
			out["default"] = struct{}{}
			continue
		}
		for nw := range svc.Networks {
			out[nw] = struct{}{}
		}
	}
	return out
}

// checkBindSources refuses a relative bind mount before the loader can make one
// absolute.
//
// A swarm bind mount names a path on whichever node runs the task, so a
// relative source has no referent at all: there is no manifest file to resolve
// it against, and the controller's own filesystem is not where the container
// runs. The loader would quietly resolve it against WorkingDir and produce a
// bind to a directory nobody named, discovered only when the container starts
// with the wrong contents.
//
// This reads the parsed document rather than the loaded config because that is
// the last point at which the source is still what the manifest said.
func checkBindSources(dict map[string]any) error {
	services, _ := dict["services"].(map[string]any)
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		svc, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		volumes, _ := svc["volumes"].([]any)
		for _, v := range volumes {
			source, ok := bindSource(v)
			if !ok || source == "" || filepath.IsAbs(source) {
				continue
			}
			return fmt.Errorf("service %q: bind source %q is relative; a swarm bind mount names a path "+
				"on the node that runs the task, so it must be absolute (or use a named volume)", name, source)
		}
	}
	return nil
}

// bindSource returns the host side of a volume entry, and whether that entry is
// a bind at all. A short-syntax entry is a bind when its source looks like a
// path; anything else is a named volume, which has no host side to check.
func bindSource(v any) (string, bool) {
	switch entry := v.(type) {
	case string:
		source, _, ok := strings.Cut(entry, ":")
		if !ok {
			// An anonymous volume: just a container path.
			return "", false
		}
		if !strings.ContainsAny(source, "/.~") {
			return "", false
		}
		return source, true
	case map[string]any:
		if t, _ := entry["type"].(string); t != "bind" {
			return "", false
		}
		source, _ := entry["source"].(string)
		return source, true
	default:
		return "", false
	}
}
