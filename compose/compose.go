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
	if err := checkFileSources(dict); err != nil {
		return nil, err
	}

	cfg, err := loader.Load(composetypes.ConfigDetails{
		// A rendered manifest is not a file, so there is no directory for a
		// relative path to mean anything against; checkBindSources and
		// checkFileSources have already refused all three of the places the
		// loader would have used this.
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

// unresolvedID is the id ConvertUnresolved reports for every reference it is
// asked about. Not a plausible-looking one on purpose: a spec that reached a
// swarm carrying this would be rejected outright, which is the failure worth
// having if one of those specs is ever applied by mistake.
const unresolvedID = "unresolved"

// ConvertUnresolved converts a manifest as though every config and secret it
// references already existed.
//
// It exists to break a circle. A stack that reaches for one of the controller's
// own secrets or configs has to be refused whole, before anything is created
// (swarmcli-cd#63) — and what a service mounts is only legible from its
// converted spec. But converting a service resolves each reference to the id
// Swarm addresses it by, so the conversion that gets applied cannot run until
// the resources exist, and a chart's own config does not exist until this
// controller creates it (swarmcli-cd#84). One conversion cannot be both.
//
// So this one answers the lookup instead of making it. Every name it produces is
// the name Convert produces, because a reference's name comes from the manifest
// and the namespace — namespace.Scope(source), or the top-level entry's own
// name: — and nothing in that asks the daemon. Only the id does.
//
// **The result must not be applied.** Its references carry unresolvedID rather
// than the id of anything on the swarm; it is for reading names from, and
// Convert is what a deploy applies.
func ConvertUnresolved(ctx context.Context, manifest, stack string, api client.APIClient) (*Stack, error) {
	return Convert(ctx, manifest, stack, assumeResolved{api})
}

// assumeResolved is a client that reports every config and secret asked for as
// existing. It wraps the real one rather than replacing it because conversion
// also reads the negotiated API version, which gates a few spec fields: a
// conversion done against a made-up version would differ from the real one in
// more than the ids.
//
// The two overridden methods are the whole of what conversion asks a daemon —
// docker/cli's ParseSecrets and ParseConfigs list by name filter to turn each
// reference's name into an id — so a docker/cli bump that starts calling
// somewhere new surfaces here as a real call against the real client rather than
// as a silent wrong answer.
type assumeResolved struct{ client.APIClient }

func (assumeResolved) ConfigList(_ context.Context, o swarm.ConfigListOptions) ([]swarm.Config, error) {
	names := o.Filters.Get("name")
	out := make([]swarm.Config, 0, len(names))
	for _, name := range names {
		out = append(out, swarm.Config{ID: unresolvedID, Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: name},
		}})
	}
	return out, nil
}

func (assumeResolved) SecretList(_ context.Context, o swarm.SecretListOptions) ([]swarm.Secret, error) {
	names := o.Filters.Get("name")
	out := make([]swarm.Secret, 0, len(names))
	for _, name := range names {
		out = append(out, swarm.Secret{ID: unresolvedID, Spec: swarm.SecretSpec{
			Annotations: swarm.Annotations{Name: name},
		}})
	}
	return out, nil
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

// checkBindSources refuses the two bind mounts a reconciled stack may not have:
// a relative source, and one that reaches the docker daemon's own socket.
//
// # A relative source
//
// A swarm bind mount names a path on whichever node runs the task, so a
// relative source has no referent at all: there is no manifest file to resolve
// it against, and the controller's own filesystem is not where the container
// runs. The loader would quietly resolve it against WorkingDir and produce a
// bind to a directory nobody named, discovered only when the container starts
// with the wrong contents. Refused before the loader runs, because making it
// absolute is what the loader does.
//
// # The daemon socket
//
// A bind of /var/run/docker.sock is root on the node it lands on, and a chart
// chooses where its services run: `deploy.placement.constraints: [node.role ==
// manager]` puts the task on a manager, where that socket is the swarm's control
// plane. From there every other refusal in this package and in backend is
// decoration — the stack can read the controller's secrets straight out of its
// own service spec, write any spec it likes over any service, or delete the
// controller.
//
// It is refused because **a chart author is a lower privilege than an app-set
// author**, deliberately: the app-set repository is root-equivalent and is meant
// to be protected as such, so that "being able to change an app's chart is not
// the same permission as being able to add an app" (docs/configuration.md). A
// socket bind collapses the two, and collapsing them is also what would have made
// #99 pointless — a chart that can reach the daemon has no need to read the
// controller's filesystem for a credential it can ask the daemon for.
//
// The refusal is flat, with no allowlist and no per-application policy. The cost
// is real and worth naming: the charts that genuinely want the socket — Traefik's
// swarm provider, a Portainer agent, an autoheal sidecar — cannot be reconciled
// by this controller and have to be deployed beside it. A mixed-workload policy
// is a design of its own (#64) rather than a flag on this one.
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
			if !ok || source == "" {
				continue
			}
			if !filepath.IsAbs(source) {
				return fmt.Errorf("service %q: bind source %q is relative; a swarm bind mount names a path "+
					"on the node that runs the task, so it must be absolute (or use a named volume)", name, source)
			}
			if sock, reaches := reachesDockerSocket(source); reaches {
				return fmt.Errorf("service %q: bind source %q reaches the docker daemon's socket (%s); a "+
					"reconciled stack may not mount it. A chart chooses where its services run, so with a "+
					"manager placement that socket is root over the whole swarm — this controller's own "+
					"credentials, every other application's services, and the controller itself. A workload "+
					"that genuinely needs the daemon has to be deployed beside this controller rather than "+
					"reconciled by it", name, source, sock)
			}
		}
	}
	return nil
}

// dockerSockets is where the daemon listens, by both names it answers to.
//
// Two spellings rather than one because /var/run is a symlink to /run on every
// systemd host, so both name the same file — and the resolution that would
// collapse them cannot happen here: the path names a filesystem on whichever node
// runs the task, which is the one filesystem this controller cannot read. A
// rootless daemon's socket under $XDG_RUNTIME_DIR is deliberately absent; a
// rootless swarm manager is not a deployment this repository supports, and
// guessing at the path would be a host-path policy rather than a fact.
var dockerSockets = []string{"/run/docker.sock", "/var/run/docker.sock"}

// reachesDockerSocket reports whether binding source into a container puts the
// daemon's socket inside it, and which socket that is.
//
// The socket's own path is the obvious case. A directory holding it is the same
// thing one `ls` further in, so `- /var/run:/var/run` is refused too, and so is
// `/`. The rule is exactly "a path from which docker.sock is reachable by name",
// and it stops there on purpose: /var/lib/docker is root-equivalent by a
// different route — it holds the contents of every named volume on the node —
// and refusing it would be the general host-path policy this is deliberately not.
// Which means the volume guard in backend is a guard on a *name*: a stack that
// binds the daemon's data root still reaches the bytes. Closing that is the same
// deferred design (#64).
//
// Lexical only. filepath.Clean folds `..`, doubled separators and a trailing
// slash, which is the whole of what a manifest can vary without naming something
// on a node this controller cannot see — a symlink there is answered by listing
// both spellings above, not by resolving anything here.
func reachesDockerSocket(source string) (string, bool) {
	dir := filepath.Clean(source)
	for _, sock := range dockerSockets {
		if dir == sock || dir == "/" || strings.HasPrefix(sock, dir+"/") {
			return sock, true
		}
	}
	return "", false
}

// bindSource returns the host side of a volume entry, and whether that entry
// names a host path at all. A short-syntax entry does when its source looks like
// a path; anything else is a named volume, which has no host side to check.
//
// The long form is read by its `type:`, and two of the six types name a path:
// bind, and npipe. npipe is here because it is what the check would otherwise be
// one word away from missing — `{type: npipe, source: /var/run/docker.sock}`
// converts without complaint (convert.convertVolumeToMount), and while a Linux
// daemon then refuses the mount when the task starts, "the node happens to reject
// it" is not a guard. The other four name no host path: volume and cluster take a
// volume name, image an image reference, and tmpfs no source at all.
//
// A long-form entry with no `type:` needs no case. The schema makes it required,
// so loader.Load refuses one a few lines after this runs.
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
		switch t, _ := entry["type"].(string); t {
		case "bind", "npipe":
			source, _ := entry["source"].(string)
			return source, true
		default:
			return "", false
		}
	default:
		return "", false
	}
}

// checkFileSources refuses a manifest that takes content from a path, before the
// loader can read one.
//
// Three keys make the loader read: a config's file:, a secret's file:, and a
// service's env_file:. All three are resolved against the same WorkingDir as a
// bind source, and for the same reason none of them can mean what a chart author
// thinks: a rendered manifest is a string, not a file in a checkout, so the only
// filesystem any of these paths can name is the controller's own. That one holds
// the Docker socket, the application set and /run/secrets, so
//
//	secrets:
//	  loot:
//	    file: /run/secrets/swarmcli-cd-token
//
// had the controller read its own credential, create a swarm secret holding it
// and mount that into the chart's container — under a name of the chart's own
// choosing, which is why rejectForbiddenResources, comparing names, saw nothing
// to object to (swarmcli-cd#99).
//
// So this refuses the key rather than the value: there is no path on the
// controller a chart has any business reading, and a chart resolving one to
// something harmless today is one commit away from resolving it elsewhere. A
// resource whose content a chart cannot carry is external: — an operator creates
// it on the swarm, and the stack references what it is given.
//
// This reads the parsed document rather than the loaded config because loading
// is the read, so a check that ran afterwards would run too late.
func checkFileSources(dict map[string]any) error {
	for _, kind := range []struct{ key, what string }{{"configs", "config"}, {"secrets", "secret"}} {
		objects, _ := dict[kind.key].(map[string]any)
		for _, name := range sortedKeys(objects) {
			obj, ok := objects[name].(map[string]any)
			if !ok {
				continue
			}
			if _, sourced := obj["file"]; !sourced {
				continue
			}
			return fmt.Errorf("%s %q: file: reads a path on the controller's own filesystem, not the "+
				"chart's; a rendered manifest is a string, so there is no chart directory for the path "+
				"to resolve against — declare the %s external: and have an operator create it on the "+
				"swarm", kind.what, name, kind.what)
		}
	}

	services, _ := dict["services"].(map[string]any)
	for _, name := range sortedKeys(services) {
		svc, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		if _, sourced := svc["env_file"]; !sourced {
			continue
		}
		return fmt.Errorf("service %q: env_file: reads a path on the controller's own filesystem, not "+
			"the chart's; a rendered manifest is a string, so there is no chart directory for the path "+
			"to resolve against — set the variables with environment: instead", name)
	}
	return nil
}

// sortedKeys is a manifest section's names in a fixed order. A manifest with two
// faults must be refused for the same one every time: which one an operator is
// shown is not Go's map iteration order's to decide.
func sortedKeys(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
