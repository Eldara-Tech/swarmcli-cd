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
//
// One piece is not upstream's: a config's conversion, because upstream's reads
// the config's file: from disk and this package reads no path. convertConfigs
// builds the same spec from the bytes the chart engine hands over instead, and a
// parity test holds the two together.
package compose

import (
	"context"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/cli/cli/compose/loader"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli-cd/application"
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
//
// files are the chart files the manifest names, keyed by their chart-relative
// path — charts.DeployRequest.Files, or a stored revision's Release.Files — and
// are the only place a config's content comes from; see checkFileSources. Nil
// means the manifest was rendered with none, so one whose configs name a file is
// refused.
//
// allow is the application's own permissions, carried here because the one thing
// this package refuses on their strength — a bind mount — is legible from the
// parsed document and from nowhere later. The caller is a backend scoped to one
// application; the rest of the same value is read there, over the names
// conversion produces (backend.rejectForbiddenResources).
func Convert(ctx context.Context, manifest, stack string, files map[string][]byte, api client.APIClient, allow application.Allow) (*Stack, error) {
	dict, err := loader.ParseYAML([]byte(manifest))
	if err != nil {
		return nil, fmt.Errorf("parsing the manifest: %w", err)
	}
	// First, because every check after it walks the document as map[string]any
	// and would step over a block that is not one.
	if err := checkStringKeys(dict, ""); err != nil {
		return nil, err
	}
	// Before the checks below, so they read the strings the swarm will get.
	unescapeDollars(dict)
	if err := checkBindSources(dict, allow); err != nil {
		return nil, err
	}
	sources, err := checkFileSources(dict, files)
	if err != nil {
		return nil, err
	}

	cfg, err := loader.Load(composetypes.ConfigDetails{
		// A rendered manifest is not a file, so there is no directory for a
		// relative path to mean anything against, and nothing reads relative to
		// this: a bind source must be absolute (checkBindSources), env_file: and
		// a secret's file: are refused (checkFileSources), and a config's file:
		// is looked up in files by convertConfigs rather than opened. The loader
		// still joins a config's path onto it, and nothing reads the result.
		WorkingDir:  "/",
		ConfigFiles: []composetypes.ConfigFile{{Config: dict}},
	}, func(o *loader.Options) {
		// A chart resolved its own templating before this manifest existed, so
		// a surviving ${FOO} is either a literal the chart meant or an
		// accident. Interpolating it would make the controller's environment —
		// whatever its own stack.yml happens to give it — an invisible input to
		// every application's deployment, and would silently substitute the
		// empty string for anything unset. Schema validation stays on, and the
		// one half of interpolation that is not substitution — the `$$` escape —
		// is done above, by unescapeDollars.
		o.SkipInterpolation = true
	})
	if err != nil {
		return nil, fmt.Errorf("loading the manifest: %w", err)
	}
	if err := checkSecretSources(cfg.Secrets); err != nil {
		return nil, err
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

	configs, err := convertConfigs(ns, cfg.Configs, sources, files)
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
func ConvertUnresolved(ctx context.Context, manifest, stack string, files map[string][]byte, api client.APIClient, allow application.Allow) (*Stack, error) {
	return Convert(ctx, manifest, stack, files, assumeResolved{api}, allow)
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

// unescapeDollars rewrites Compose's `$$` escape, in place, to the single `$` it
// stands for, over every string value in a parsed manifest.
//
// This is the half of interpolation that belongs to the file format rather than
// to substitution. `$$` means one `$` whatever any environment holds, and a
// chart writes `export PASS="$$(cat /run/secrets/db)"` precisely so that the
// *container's* shell expands it and the plaintext never lands in the manifest
// or in `docker inspect`. Leaving the escape in place hands `$$` to that shell,
// which reads it as the pid of the process it is running in — so the password
// becomes "1(cat /run/secrets/db)" and the application fails somewhere far away
// holding a credential nobody wrote (Eldara-Tech/swarmcli-charts#108). Seven of
// the charts in that repository depend on this: for secrets, for a `$apr1$`
// htpasswd hash, and for shell variables a healthcheck expands at runtime.
//
// Values only, matching interpolation.recursiveInterpolate, which walks a map's
// values and leaves its keys alone. Replacement is left-to-right and
// non-overlapping, so `$$$$` reduces to `$$` and a lone `$` is untouched — the
// same reduction template.SubstituteWith performs for its escape group. What
// this deliberately does not do is the other half: `$FOO` and `${FOO}` survive
// as literals, for the reason Convert gives.
func unescapeDollars(v any) {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if s, ok := e.(string); ok {
				v[k] = strings.ReplaceAll(s, "$$", "$")
				continue
			}
			unescapeDollars(e)
		}
	case []any:
		for i, e := range v {
			if s, ok := e.(string); ok {
				v[i] = strings.ReplaceAll(s, "$$", "$")
				continue
			}
			unescapeDollars(e)
		}
	}
}

// checkBindSources refuses the two bind mounts a reconciled stack may not have:
// a relative source, and one this application was not permitted.
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
// That one is not a permission and is not gated. It is refused for every
// application, including one whose allowlist happens to name where the loader
// would have put it, because a chart author who wrote "./data" did not mean a
// path on a node they have never seen.
//
// A Windows-style source (C:\data, \\server\share) lands here too, by the same
// test: this controller reconciles a Linux swarm, and an allowlist entry is a
// Linux path, so PermitsPath comparing prefixes of a drive path would be a
// comparison with no meaning. Refusing it says so; CE, which runs on the
// operator's own machine and has no allowlist to consult, deploys it as written.
//
// # A host path this application may not bind
//
// A bind names a path on whichever node runs the task, and a chart chooses where
// its services run — `deploy.placement.constraints: [node.role == manager]` puts
// the task on a manager. So the paths a chart may bind are the paths its author
// may read and write across the swarm, and two of them are the swarm itself:
// /var/run/docker.sock is the control plane, and /var/lib/docker holds the
// contents of every named volume on the node, this controller's own included.
//
// #103 refused the socket by name and left every other host path open, which was
// a floor rather than a policy — and one with a cost it named: the charts that
// genuinely want the daemon, Traefik's swarm provider, a Portainer agent, an
// autoheal sidecar, could not be reconciled by this controller at all. The rule
// is now the application's own allowlist (#64). Nothing unless the app set says
// so, and the socket when it does.
//
// The allowlist is the **app-set author's**, never the chart author's. The
// app-set repository is root-equivalent and is meant to be protected as such, so
// that "being able to change an app's chart is not the same permission as being
// able to add an app" (docs/configuration.md); a chart that could widen its own
// permissions collapses the two, and collapsing them is also what would have made
// #99 pointless — a chart that can reach the daemon has no need to read the
// controller's filesystem for a credential it can ask the daemon for.
//
// There is deliberately no rule naming the socket left here. It would be a
// second, weaker copy of a decision Allow.PermitsPath already makes: permitting
// /var/run permits the socket in it, because a bind of a directory is everything
// under it — a fact about bind mounts rather than about docker — and two rules
// that could disagree about the same path is how one of them ends up not
// mattering.
//
// This reads the parsed document rather than the loaded config because that is
// the last point at which the source is still what the manifest said.
func checkBindSources(dict map[string]any, allow application.Allow) error {
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
				return fmt.Errorf("service '%s': bind source '%s' is relative; a swarm bind mount names a path "+
					"on the node that runs the task, so it must be absolute (or use a named volume)", name, source)
			}
			if !allow.PermitsPath(source) {
				return fmt.Errorf("service '%s': bind source '%s' is not a host path this application may "+
					"bind; list the paths its charts may reach in the application's allow.hostPaths. A "+
					"swarm bind mount names a path on whichever node runs the task and a chart chooses "+
					"where its services run, so a bind is that node's filesystem handed to whoever writes "+
					"the chart — /var/run/docker.sock on a manager is the swarm's control plane, and "+
					"/var/lib/docker is the contents of every named volume on the node. A listed path "+
					"permits everything under it", name, source)
			}
		}
	}
	return nil
}

// bindSource returns the host side of a volume entry, and whether that entry
// names a host path at all.
//
// A short-syntax entry is classified by the parser that decides — ParseVolume
// wraps volumespec.Parse, which is what the loader itself runs a few lines
// later. Reading it as "cut at the first colon, then look for /.~ in the source"
// was wrong in both directions (#153): compose asks isFilePath, which reads the
// source's FIRST character and nothing else, so `my.data:/data` is a legal named
// volume that the dot got refused as a relative bind; and not every colon
// separates, so `C:\data:/d` cut to a source of "C" and was skipped by this
// check and by the allowlist both, while compose read it as a bind.
//
// An entry ParseVolume cannot read names nothing here. loader.Load refuses it a
// few lines after this runs, with a better message than this could give.
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
		// An anonymous volume — a container path and nothing else — parses as a
		// volume, so it needs no case of its own.
		v, err := loader.ParseVolume(entry)
		if err != nil || v.Type != "bind" {
			return "", false
		}
		return v.Source, true
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

// checkStringKeys refuses a manifest holding a mapping whose keys are not all
// strings, anywhere in it.
//
// YAML allows any scalar as a key, and the parser hands a mapping with even one
// such key back as a map[any]any — the whole mapping, not the one entry. Every
// check in this file walks the document by asserting map[string]any, so a block
// of that shape would be stepped over by all of them, siblings included, and
// the loader would be the first thing to look inside it. Compose has no use for
// a non-string key, so refusing one here, before any other check runs, is what
// lets the rest of this file treat a block it cannot read as absent.
//
// at is where v sits in the document, for the message.
func checkStringKeys(v any, at string) error {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(v) {
			next := k
			if at != "" {
				next = at + "." + k
			}
			if err := checkStringKeys(v[k], next); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range v {
			if err := checkStringKeys(e, fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	case map[any]any:
		return fmt.Errorf("'%s' has a key that is not a string; every compose key is one, so quote it", at)
	}
	return nil
}

// chartFilesRule is the sentence every refusal of a config's file: carries.
const chartFilesRule = "a config's file: names content the chart ships in files/, or a value in values/"

// checkFileSources refuses every key that would have the controller read a path,
// and returns, for each config whose content comes from a file, the key in files
// that content is under.
//
// Three compose keys name a path: a config's file:, a secret's file:, and a
// service's env_file:. The loader resolves all three against WorkingDir, and a
// rendered manifest is a string rather than a file in a checkout, so the only
// filesystem any of them could name is the controller's own — the one holding
// the Docker socket, the application set and /run/secrets. So none of them is
// ever read here, and that is the guarantee this function exists to keep: a
// chart declaring
//
//	secrets:
//	  loot:
//	    file: /run/secrets/swarmcli-cd-token
//
// once had the controller read its own credential into a swarm secret of the
// chart's naming (swarmcli-cd#99).
//
// # A config's file:
//
// The chart engine resolves a config's file: itself, while the chart is still in
// scope, and hands the bytes over with the manifest — charts.DeployRequest.Files
// for a deploy, Release.Files on every stored revision. Those bytes are the only
// content a config here can have. A path is accepted when it names one of them,
// and is then looked up rather than opened: convertConfigs reads the map, not the
// disk. So a path is refused, in this order, when it
//
//   - is not a string;
//   - contains a $. The engine keys files by the path as the manifest wrote it,
//     before the $$ escape is undone, and this reads it after, so a $$ path would
//     name a key that cannot exist. And an interpolation-shaped path is not one
//     any chart means: interpolation is off (see Convert), so it would only ever
//     be a literal;
//   - is absolute, or escapes the chart once cleaned;
//   - is outside files/ and values/;
//   - is not in files.
//
// The containment is the chart engine's own rule, reproduced exactly: path and
// not filepath, because the value came from YAML, which is slash-separated
// whatever the host; and path.Clean resolves every interior "..", so a leading
// one afterwards is the complete test for leaving the chart. That makes the key
// computed here the key the engine stored the bytes under. The map is not trusted
// to have been built that way — a stored revision is only as trustworthy as
// Docker access to the store — which is why the path is validated on every
// conversion rather than once, whatever produced files.
//
// # A secret's file:
//
// Refused. A chart's secrets are created outside it and referenced external:, or
// are driver-backed. Content shipped in the chart would be stored in the release
// record, a Docker config anyone with Docker access can read, and a stored
// secret's data cannot be read back to compare, so a changed file under an
// unchanged name would silently leave the old secret in place. The refusal is of
// the key, so it holds beside external: too; checkSecretSources covers a secret
// that names no source at all.
//
// # env_file:
//
// Refused, whatever it names: environment: says the same thing in the manifest.
//
// This reads the parsed document rather than the loaded config because the
// loader is what would read env_file:, so a check that ran afterwards would run
// too late — and because the loaded config no longer holds a config's path as the
// manifest wrote it.
func checkFileSources(dict map[string]any, files map[string][]byte) (map[string]string, error) {
	sources := map[string]string{}
	configs, _ := dict["configs"].(map[string]any)
	for _, name := range sortedKeys(configs) {
		obj, ok := configs[name].(map[string]any)
		if !ok {
			continue
		}
		raw, sourced := obj["file"]
		if !sourced {
			continue
		}
		key, err := chartFile(raw, files)
		if err != nil {
			return nil, fmt.Errorf("config '%s': %w", name, err)
		}
		sources[name] = key
	}

	secrets, _ := dict["secrets"].(map[string]any)
	for _, name := range sortedKeys(secrets) {
		obj, ok := secrets[name].(map[string]any)
		if !ok {
			continue
		}
		if _, sourced := obj["file"]; sourced {
			return nil, secretRefused(name, "file: is refused")
		}
	}

	services, _ := dict["services"].(map[string]any)
	for _, name := range sortedKeys(services) {
		svc, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		if _, sourced := svc["env_file"]; sourced {
			return nil, fmt.Errorf("service '%s': env_file: is refused; set variables with environment:", name)
		}
	}
	return sources, nil
}

// chartFile returns the key in files a config's file: names, or why it names
// none. The checks and their order are checkFileSources'.
func chartFile(raw any, files map[string][]byte) (string, error) {
	p, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("file: must be a path, not a %T; %s", raw, chartFilesRule)
	}
	if strings.Contains(p, "$") {
		return "", fmt.Errorf("file: '%s' is interpolation-shaped, and a path containing $ is refused; %s", p, chartFilesRule)
	}
	if path.IsAbs(p) {
		return "", fmt.Errorf("file: '%s' is an absolute path; %s", p, chartFilesRule)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("file: '%s' escapes the chart; %s", p, chartFilesRule)
	}
	if !strings.HasPrefix(clean, "files/") && !strings.HasPrefix(clean, "values/") {
		return "", fmt.Errorf("file: '%s' is outside files/ and values/; %s", p, chartFilesRule)
	}
	if _, ok := files[clean]; !ok {
		return "", fmt.Errorf("file: '%s' is not shipped by the chart; %s", p, chartFilesRule)
	}
	return clean, nil
}

// checkSecretSources refuses a secret that is neither external: nor
// driver-backed.
//
// checkFileSources has already refused a secret's file:, and this is not a second
// copy of that. A secret naming no source at all still gets one: the loader fills
// the missing file: in as WorkingDir itself, and convert.Secrets would then try to
// read it. So every secret reaching convert.Secrets has to be one it does not
// read for — external, which it skips, or driver-backed, which it builds without
// content — and that is decided here, on the loaded config, because both are
// properties of a value rather than of a key: external: false and driver: "" are
// present, and both mean no.
//
// Loading reads no secret's file, so after the load is still before any read.
func checkSecretSources(secrets map[string]composetypes.SecretConfig) error {
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		if s := secrets[name]; !s.External.External && s.Driver == "" {
			return secretRefused(name, "it is neither external: nor driver-backed")
		}
	}
	return nil
}

// secretRefused is the one refusal both secret checks give: what they refuse
// differs, and what to do instead does not.
func secretRefused(name, why string) error {
	return fmt.Errorf("secret '%s': %s; a chart does not carry a secret's content — declare it external: "+
		"and have an operator create it on the swarm, or give it a driver:", name, why)
}

// convertConfigs is convert.Configs with each config's content taken from files
// rather than read from a path.
//
// Everything else is upstream's, field for field — fileObjectConfig and Configs
// in docker/cli's cli/compose/convert/compose.go: the name is the entry's own
// name: when it set one and the namespace-scoped key otherwise, the labels gain
// the stack's namespace, and template_driver becomes Templating. The parity test
// holds it to that, so a docker/cli bump that changes the upstream conversion
// fails there rather than drifting apart from it here.
//
// sources is checkFileSources' answer — the key in files each entry's file:
// named — and not the loaded config's File, which the loader has rewritten
// against WorkingDir. An entry that is neither external nor in sources named no
// file at all, and there is nothing to give it.
func convertConfigs(ns convert.Namespace, configs map[string]composetypes.ConfigObjConfig, sources map[string]string, files map[string][]byte) ([]swarm.ConfigSpec, error) {
	out := []swarm.ConfigSpec{}
	for _, name := range slices.Sorted(maps.Keys(configs)) {
		obj := configs[name]
		if obj.External.External {
			continue
		}
		key, ok := sources[name]
		if !ok {
			return nil, fmt.Errorf("config '%s' has no content; %s, or declare it external:", name, chartFilesRule)
		}
		scoped := obj.Name
		if scoped == "" {
			scoped = ns.Scope(name)
		}
		spec := swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: scoped, Labels: convert.AddStackLabel(ns, obj.Labels)},
			Data:        files[key],
		}
		if obj.TemplateDriver != "" {
			spec.Templating = &swarm.Driver{Name: obj.TemplateDriver}
		}
		out = append(out, spec)
	}
	return out, nil
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
