// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package drift

import (
	"reflect"
	"testing"
	"time"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/compose"
)

// defaultNetwork is the attachment a service that names no networks still gets:
// convert.convertServiceNetworks defaults to {default: {}} and scopes it, so a
// converted spec always carries at least one.
const defaultNetwork = "s_default"

// networkID is the id the daemon stores for a network the manifest named. What
// matters about it is only that it is not the name — the resolution step exists
// because the two are different strings.
func networkID(name string) string { return "netid-" + name }

// swarmNets is the reconciler's read of the swarm, as drift.Live receives it.
//
// Each network is named under both forms deliberately. A live spec the daemon
// returned carries the id, which is the case that matters; a spec built by hand
// in a test and handed straight back carries the name. Naming both means a test
// about some other field compares its attachments rather than quietly taking the
// decline path and proving less than it looks like it does.
var swarmNets = NetworkNames{
	networkID(defaultNetwork): defaultNetwork,
	defaultNetwork:            defaultNetwork,
	networkID("s_extra"):      "s_extra",
}

// desiredSpec is a service as compose conversion produces it: pointers left nil
// wherever the manifest said nothing, which is exactly where the daemon fills a
// default in and where a naive comparison goes wrong.
func desiredSpec(name, image string) swarm.ServiceSpec {
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: "s_" + name,
			Labels: map[string]string{
				convert.LabelNamespace: "s",
				convert.LabelImage:     image,
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: image,
				// Also never absent from a converted spec: convert.Service builds
				// a Privileges for every service, usually holding nothing but a
				// nil credential spec.
				Privileges: &swarm.Privileges{},
			},
			// Never absent from a converted spec, so never absent here: a
			// service naming no networks is attached to the stack's default one.
			Networks: []swarm.NetworkAttachmentConfig{{Target: defaultNetwork}},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}},
	}
}

func stackOf(specs ...swarm.ServiceSpec) *compose.Stack {
	st := &compose.Stack{Namespace: convert.NewNamespace("s")}
	for _, spec := range specs {
		st.Services = append(st.Services, compose.Service{Name: spec.Name, Spec: spec})
	}
	return st
}

func liveOf(specs ...swarm.ServiceSpec) map[string]swarm.Service {
	out := make(map[string]swarm.Service, len(specs))
	for _, spec := range specs {
		out[spec.Name] = swarm.Service{ID: "id-" + spec.Name, Spec: spec}
	}
	return out
}

func uint64p(v uint64) *uint64 { return &v }

func boolp(v bool) *bool { return &v }

func durationp(v time.Duration) *time.Duration { return &v }

// The test the whole allowlist exists for.
//
// Every entry here is a way the daemon legitimately returns something other
// than what was written. If any of them reported drift, a controller would
// converge a service nobody touched — on every tick, forever — and the right
// response from an operator would be to turn the feature off.
func TestNormalisedDifferencesAreNotDrift(t *testing.T) {
	for name, mutate := range map[string]func(want, live *swarm.ServiceSpec){
		"replicas defaulted from nil to one": func(_, live *swarm.ServiceSpec) {
			live.Mode.Replicated.Replicas = uint64p(1)
		},
		"image resolved to a digest": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.ContainerSpec.Image = "nginx:1.2@sha256:aaaa"
		},
		// The client's own rewrite, which happens to every spec that reaches a
		// daemon and to none that does not: imageWithTagString runs
		// FamiliarString(TagNameOnly(ref)) before the request is built, whatever
		// resolve mode the deploy asked for. `image: nginx` therefore reads back
		// as `nginx:latest` for ever, on a service nobody has touched.
		"image tagged latest by the client": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Image = "nginx"
			live.TaskTemplate.ContainerSpec.Image = "nginx:latest"
		},
		// The other half of the same rewrite: the default registry and the
		// library namespace are dropped on the way out.
		"image familiarised by the client": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Image = "docker.io/library/nginx:1.25"
			live.TaskTemplate.ContainerSpec.Image = "nginx:1.25"
		},
		"image tagged by the client and then resolved to a digest": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Image = "myreg.io/foo/bar"
			live.TaskTemplate.ContainerSpec.Image = "myreg.io/foo/bar:latest@sha256:aaaa"
		},
		// A manifest that pins its own digest is already what the daemon stores,
		// registry prefix aside: TagNameOnly leaves a canonical reference alone.
		"image pinned by digest in the manifest": func(want, live *swarm.ServiceSpec) {
			const pinned = "nginx@sha256:0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"
			want.TaskTemplate.ContainerSpec.Image = "docker.io/library/" + pinned
			live.TaskTemplate.ContainerSpec.Image = pinned
		},
		"force update counter advanced": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.ForceUpdate = 3
		},
		"labels returned as an empty map rather than nil": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.ContainerSpec.Labels = map[string]string{}
		},
		"resources returned as zero structs rather than nil": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.Resources = &swarm.ResourceRequirements{
				Limits:       &swarm.Limit{},
				Reservations: &swarm.Resources{},
			}
		},
		"placement returned as a zero struct rather than nil": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.Placement = &swarm.Placement{}
		},
		"endpoint resolution mode defaulted to vip": func(_, live *swarm.ServiceSpec) {
			live.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP}
		},
		// Not "the daemon assigned a published port", which is the natural
		// assumption and does not happen to a *spec*: swarmkit allocates a
		// dynamic publish into the service's runtime Endpoint and leaves
		// Spec.Endpoint alone, so a zero stays zero on both sides. What the
		// round trip does change is the two enums, which is what this pins.
		"protocol and publish mode defaulted from empty": func(want, live *swarm.ServiceSpec) {
			want.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{
				TargetPort: 80, PublishedPort: 8080,
			}}}
			live.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{
				TargetPort: 80, PublishedPort: 8080, Protocol: "tcp", PublishMode: "ingress",
			}}}
		},
		"a dynamic publish stays zero on both sides": func(want, live *swarm.ServiceSpec) {
			want.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{TargetPort: 80}}}
			live.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{
				TargetPort: 80, Protocol: "tcp", PublishMode: "ingress",
			}}}
		},
		"platforms filled from the image manifest": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.Placement = &swarm.Placement{
				Platforms: []swarm.Platform{{Architecture: "amd64", OS: "linux"}},
			}
		},
		// The desired side names the network; the live side is the same
		// attachment after the daemon rewrote its target to the id it stored.
		// Not "an attachment appeared against none", which the previous version
		// of this case asserted and which cannot happen: a converted spec always
		// carries at least the stack's default network.
		"network target resolved from a name to an id": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{
				{Target: networkID(defaultNetwork)},
			}
		},
		// Not "swarmkit filled one in", which the previous version of this case
		// asserted and which does not happen: updateConfigToGRPC returns nil for
		// nil and updateConfigFromGRPC returns nil for nil, so a manifest that
		// declared no update_config reads one back. The defaulting is one level
		// down — inside a struct that *was* declared, where the two unstated enums
		// come back under the names of their defaults.
		"an update config's unstated fields defaulted": func(want, live *swarm.ServiceSpec) {
			want.UpdateConfig = &swarm.UpdateConfig{Parallelism: 1}
			live.UpdateConfig = &swarm.UpdateConfig{
				Parallelism:   1,
				FailureAction: swarm.UpdateFailureActionPause,
				Order:         swarm.UpdateOrderStopFirst,
			}
		},
		"a rollback config's unstated fields defaulted": func(want, live *swarm.ServiceSpec) {
			want.RollbackConfig = &swarm.UpdateConfig{Parallelism: 2}
			live.RollbackConfig = &swarm.UpdateConfig{
				Parallelism:   2,
				FailureAction: swarm.UpdateFailureActionPause,
				Order:         swarm.UpdateOrderStopFirst,
			}
		},
		// restartPolicyFromGRPC takes the address of the stored count
		// unconditionally, so the live side is never nil where the manifest left
		// it so — and an unstated condition comes back named, like the enums above.
		"a restart policy's unstated fields defaulted": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{}
			live.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{
				Condition:   swarm.RestartPolicyConditionAny,
				MaxAttempts: uint64p(0),
			}
		},
		// compose builds an empty preference slice for every service; the daemon
		// returns nil for a spec that placed nothing.
		"placement preferences returned as nil rather than an empty slice": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.Placement = &swarm.Placement{Preferences: []swarm.PlacementPreference{}}
			live.TaskTemplate.Placement = &swarm.Placement{}
		},
		"a healthcheck absent from both sides": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Healthcheck = nil
			live.TaskTemplate.ContainerSpec.Healthcheck = nil
		},
		// The asymmetry runs the opposite way to the obvious guess: compose builds
		// a BindOptions for a manifest that named a `bind:` block, and
		// containerToGRPC drops the whole struct when its propagation is empty,
		// so the desired side has one and the live side does not.
		"a bind mount's options dropped on the round trip": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
				Type: mount.TypeBind, Source: "/etc/hostname", Target: "/etc/host-name",
				BindOptions: &mount.BindOptions{},
			}}
			live.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
				Type: mount.TypeBind, Source: "/etc/hostname", Target: "/etc/host-name",
			}}
		},
		// Consistency has no field in swarmapi.Mount at all, so whatever compose
		// parsed is simply gone by the time the spec is read back.
		"mount consistency dropped entirely": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
				Type: mount.TypeVolume, Source: "s_data", Target: "/data", Consistency: "cached",
			}}
			live.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
				Type: mount.TypeVolume, Source: "s_data", Target: "/data",
			}}
		},
		"a named volume's options are the daemon's business": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
				Type: mount.TypeVolume, Source: "s_data", Target: "/data",
				VolumeOptions: &mount.VolumeOptions{},
			}}
			live.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
				Type: mount.TypeVolume, Source: "s_data", Target: "/data",
				VolumeOptions: &mount.VolumeOptions{Labels: map[string]string{"com.docker.stack.namespace": "s"}},
			}}
		},
		"mount type defaulted to bind": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Source: "/src", Target: "/dst"}}
			live.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
				Type: mount.TypeBind, Source: "/src", Target: "/dst",
			}}
		},
		// The reference's id is resolved on both sides and is not compared: a
		// config recreated with identical content is a new id under the same name,
		// and its name is where a content change would show.
		"a reference resolved to a different id": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{
				SecretID: "old", SecretName: "s_token",
				File: &swarm.SecretReferenceFileTarget{Name: "token", UID: "0", GID: "0", Mode: 0o444},
			}}
			live.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{
				SecretID: "new", SecretName: "s_token",
				File: &swarm.SecretReferenceFileTarget{Name: "token", UID: "0", GID: "0", Mode: 0o444},
			}}
		},
		"references returned as empty slices rather than nil": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{}
			live.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{}
			live.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{}
		},
		// docker/cli sorts capabilities on the way out and the daemon stores what
		// it is given, so an operator's order is theirs alone. A chart template
		// that reorders one has changed nothing about what runs.
		"capabilities in a different order": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.CapabilityAdd = []string{"CAP_NET_ADMIN", "CAP_SYS_TIME"}
			live.TaskTemplate.ContainerSpec.CapabilityAdd = []string{"CAP_SYS_TIME", "CAP_NET_ADMIN"}
		},
		"extra hosts in a different order": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Hosts = []string{"10.0.0.1 a", "10.0.0.2 b"}
			live.TaskTemplate.ContainerSpec.Hosts = []string{"10.0.0.2 b", "10.0.0.1 a"}
		},
		// ulimitsFromGRPC builds its slice with make(..., len(u)), so a service
		// with none reads back an empty non-nil slice against the manifest's nil.
		"ulimits returned as an empty slice rather than nil": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.ContainerSpec.Ulimits = []*container.Ulimit{}
		},
		"an unstated init flag on both sides": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Init = nil
			live.TaskTemplate.ContainerSpec.Init = nil
		},
		// Not "the daemon populated one against a manifest that sent none", which
		// the previous version of this case asserted and which cannot happen:
		// convert.Service builds a Privileges for every service. The struct is
		// stored whenever it is sent and read back the same way, so an empty one
		// on both sides is what an ordinary service looks like.
		"privileges round-trip as an empty struct": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{}
			live.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{}
		},
		"isolation defaulted from empty": func(want, live *swarm.ServiceSpec) {
			want.TaskTemplate.ContainerSpec.Isolation = ""
			live.TaskTemplate.ContainerSpec.Isolation = container.IsolationDefault
		},
		"runtime defaulted to container": func(_, live *swarm.ServiceSpec) {
			live.TaskTemplate.Runtime = swarm.RuntimeContainer
		},
	} {
		t.Run(name, func(t *testing.T) {
			want := desiredSpec("web", "nginx:1.2")
			live := desiredSpec("web", "nginx:1.2")
			// Deep-copy the two pointers the mutators reach through, so a
			// mutation cannot reach the desired side and hide its own drift.
			cs := *live.TaskTemplate.ContainerSpec
			live.TaskTemplate.ContainerSpec = &cs
			mode := *live.Mode.Replicated
			live.Mode.Replicated = &mode

			mutate(&want, &live)

			got := Live(stackOf(want), liveOf(live), swarmNets)
			if got.State != application.DriftStateNone {
				t.Errorf("state = %q with %v, want none — this is normalisation, not drift",
					got.State, got.Services)
			}
		})
	}
}

func TestLiveReportsNoDriftOnAnIdenticalStack(t *testing.T) {
	spec := desiredSpec("web", "nginx:1.2")
	got := Live(stackOf(spec), liveOf(spec), swarmNets)
	if got.State != application.DriftStateNone || len(got.Services) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

// One case per allowlisted field, asserting both the verdict and the rendering:
// the field name and the two values are what an operator reads to decide whether
// the change was theirs.
func TestLiveReportsEachComparedField(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(want, live *swarm.ServiceSpec)
		want   application.FieldDrift
	}{
		"replicas": {
			func(want, live *swarm.ServiceSpec) {
				want.Mode.Replicated.Replicas = uint64p(1)
				live.Mode.Replicated.Replicas = uint64p(3)
			},
			application.FieldDrift{Field: "replicas", Desired: "1", Live: "3"},
		},
		"replicas against an unstated default": {
			func(want, live *swarm.ServiceSpec) {
				live.Mode.Replicated.Replicas = uint64p(5)
			},
			application.FieldDrift{Field: "replicas", Desired: "1", Live: "5"},
		},
		"mode": {
			func(want, live *swarm.ServiceSpec) {
				live.Mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}
			},
			application.FieldDrift{Field: "mode", Desired: "replicated", Live: "global"},
		},
		"image": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.ContainerSpec.Image = "nginx:9.9@sha256:bbbb"
			},
			application.FieldDrift{Field: "image", Desired: "nginx:1.2", Live: "nginx:9.9@sha256:bbbb"},
		},
		// The normalisation the desired side needs must not become blindness. A
		// manifest naming no tag is compared as `nginx:latest`, and an image
		// somebody swapped underneath it is still a different image.
		"image changed behind a tag the manifest left unstated": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Image = "nginx"
				live.TaskTemplate.ContainerSpec.Image = "nginx:9.9@sha256:bbbb"
			},
			application.FieldDrift{Field: "image", Desired: "nginx", Live: "nginx:9.9@sha256:bbbb"},
		},
		// The case that makes normalising only the desired side load-bearing.
		// `docker service update --image nginx@sha256:…` leaves the live spec
		// untagged, so stripping its digest gives `nginx` — which is the string
		// the manifest wrote, and would match if the live side were normalised
		// too. The image would then be the one change a converge could not see.
		"image repinned by hand to a bare digest": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Image = "nginx"
				live.TaskTemplate.ContainerSpec.Image =
					"nginx@sha256:0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"
			},
			application.FieldDrift{
				Field:   "image",
				Desired: "nginx",
				Live:    "nginx@sha256:0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff",
			},
		},
		"cpu limit": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.Resources = &swarm.ResourceRequirements{Limits: &swarm.Limit{NanoCPUs: 500_000_000}}
				live.TaskTemplate.Resources = &swarm.ResourceRequirements{Limits: &swarm.Limit{NanoCPUs: 2_000_000_000}}
			},
			application.FieldDrift{Field: "resources.limits.nanoCPUs", Desired: "500000000", Live: "2000000000"},
		},
		"memory reservation": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.Resources = &swarm.ResourceRequirements{Reservations: &swarm.Resources{MemoryBytes: 1 << 20}}
			},
			application.FieldDrift{Field: "resources.reservations.memoryBytes", Desired: "1048576", Live: "0"},
		},
		"environment variable removed": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Env = []string{"LOG_LEVEL=info"}
			},
			application.FieldDrift{Field: "env[LOG_LEVEL]", Desired: "set", Live: "absent"},
		},
		"environment variable added": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.ContainerSpec.Env = []string{"DEBUG=1"}
			},
			application.FieldDrift{Field: "env[DEBUG]", Desired: "absent", Live: "set"},
		},
		"environment value changed": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Env = []string{"LOG_LEVEL=info"}
				live.TaskTemplate.ContainerSpec.Env = []string{"LOG_LEVEL=debug"}
			},
			application.FieldDrift{Field: "env[LOG_LEVEL]", Desired: "set", Live: "changed"},
		},
		"constraints": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.Placement = &swarm.Placement{Constraints: []string{"node.role==worker"}}
				live.TaskTemplate.Placement = &swarm.Placement{Constraints: []string{"node.role==manager"}}
			},
			application.FieldDrift{Field: "constraints", Desired: "node.role==worker", Live: "node.role==manager"},
		},
		"label changed": {
			func(want, live *swarm.ServiceSpec) {
				want.Labels["tier"] = "web"
				live.Labels["tier"] = "edge"
			},
			application.FieldDrift{Field: "labels[tier]", Desired: "web", Live: "edge"},
		},
		"label removed": {
			func(want, live *swarm.ServiceSpec) {
				want.Labels["tier"] = "web"
			},
			application.FieldDrift{Field: "labels[tier]", Desired: "web", Live: "(absent)"},
		},
		"published port removed": {
			func(want, live *swarm.ServiceSpec) {
				want.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{
					TargetPort: 80, PublishedPort: 8080, Protocol: "tcp", PublishMode: "ingress",
				}}}
			},
			application.FieldDrift{Field: "ports[8080:80/tcp/ingress]", Desired: "set", Live: "absent"},
		},
		"published port added": {
			func(want, live *swarm.ServiceSpec) {
				live.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{
					TargetPort: 443, PublishedPort: 8443, Protocol: "tcp", PublishMode: "host",
				}}}
			},
			application.FieldDrift{Field: "ports[8443:443/tcp/host]", Desired: "absent", Live: "set"},
		},
		"a publish the manifest left dynamic": {
			func(want, live *swarm.ServiceSpec) {
				want.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{TargetPort: 80}}}
			},
			application.FieldDrift{Field: "ports[dynamic:80/tcp/ingress]", Desired: "set", Live: "absent"},
		},
		"endpoint mode": {
			func(want, live *swarm.ServiceSpec) {
				live.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeDNSRR}
			},
			application.FieldDrift{Field: "endpointMode", Desired: "vip", Live: "dnsrr"},
		},
		"network detached": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.Networks = nil
			},
			application.FieldDrift{Field: "networks", Desired: defaultNetwork, Live: ""},
		},
		"mount removed": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
					Type: mount.TypeVolume, Source: "s_data", Target: "/data",
				}}
			},
			application.FieldDrift{Field: "mounts[/data]", Desired: "volume:s_data", Live: "(absent)"},
		},
		"mount source changed": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
					Type: mount.TypeVolume, Source: "s_data", Target: "/data",
				}}
				live.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
					Type: mount.TypeVolume, Source: "somebody_elses", Target: "/data",
				}}
			},
			application.FieldDrift{Field: "mounts[/data]", Desired: "volume:s_data", Live: "volume:somebody_elses"},
		},
		"mount made writable by hand": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
					Type: mount.TypeBind, Source: "/etc/hostname", Target: "/etc/host-name", ReadOnly: true,
				}}
				live.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
					Type: mount.TypeBind, Source: "/etc/hostname", Target: "/etc/host-name",
				}}
			},
			application.FieldDrift{
				Field:   "mounts[/etc/host-name]",
				Desired: "bind:/etc/hostname (read-only)",
				Live:    "bind:/etc/hostname",
			},
		},
		"anonymous volume": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
					Type: mount.TypeVolume, Target: "/scratch",
				}}
			},
			application.FieldDrift{Field: "mounts[/scratch]", Desired: "volume:(anonymous)", Live: "(absent)"},
		},
		"secret removed": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{
					SecretID: "id", SecretName: "s_token",
					File: &swarm.SecretReferenceFileTarget{Name: "token"},
				}}
			},
			application.FieldDrift{Field: "secrets[s_token]", Desired: "token", Live: "(absent)"},
		},
		"config remounted somewhere else": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{{
					ConfigID: "id", ConfigName: "s_site",
					File: &swarm.ConfigReferenceFileTarget{Name: "/etc/site.conf"},
				}}
				live.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{{
					ConfigID: "id", ConfigName: "s_site",
					File: &swarm.ConfigReferenceFileTarget{Name: "/etc/elsewhere.conf"},
				}}
			},
			application.FieldDrift{
				Field:   "configs[s_site]",
				Desired: "/etc/site.conf",
				Live:    "/etc/elsewhere.conf",
			},
		},
		// A credential spec is consumed by the runtime and mounted nowhere, so it
		// has no file target to render — and unlike a secret, the daemon keeps it.
		"runtime config reference removed": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{{
					ConfigID: "id", ConfigName: "s_credspec",
					Runtime: &swarm.ConfigReferenceRuntimeTarget{},
				}}
			},
			application.FieldDrift{Field: "configs[s_credspec]", Desired: "(runtime)", Live: "(absent)"},
		},
		"healthcheck removed by hand": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{
					Test: []string{"CMD-SHELL", "true"},
				}
			},
			application.FieldDrift{Field: "healthcheck", Desired: "set", Live: "absent"},
		},
		"healthcheck interval": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{Interval: 5 * time.Second}
				live.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{Interval: time.Minute}
			},
			application.FieldDrift{Field: "healthcheck.interval", Desired: "5s", Live: "1m0s"},
		},
		"healthcheck command": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{
					Test: []string{"CMD-SHELL", "true"},
				}
				live.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{
					Test: []string{"NONE"},
				}
			},
			application.FieldDrift{Field: "healthcheck.test", Desired: "CMD-SHELL true", Live: "NONE"},
		},
		"update parallelism": {
			func(want, live *swarm.ServiceSpec) {
				want.UpdateConfig = &swarm.UpdateConfig{Parallelism: 1}
				live.UpdateConfig = &swarm.UpdateConfig{Parallelism: 5}
			},
			application.FieldDrift{Field: "updateConfig.parallelism", Desired: "1", Live: "5"},
		},
		"update order against an unstated default": {
			func(want, live *swarm.ServiceSpec) {
				want.UpdateConfig = &swarm.UpdateConfig{}
				live.UpdateConfig = &swarm.UpdateConfig{Order: swarm.UpdateOrderStartFirst}
			},
			application.FieldDrift{Field: "updateConfig.order", Desired: "stop-first", Live: "start-first"},
		},
		"update failure ratio": {
			func(want, live *swarm.ServiceSpec) {
				want.UpdateConfig = &swarm.UpdateConfig{MaxFailureRatio: 0.3}
				live.UpdateConfig = &swarm.UpdateConfig{}
			},
			application.FieldDrift{Field: "updateConfig.maxFailureRatio", Desired: "0.3", Live: "0"},
		},
		// The same struct under its other name, which is what makes one function
		// with a prefix worth having.
		"rollback config removed": {
			func(want, live *swarm.ServiceSpec) {
				want.RollbackConfig = &swarm.UpdateConfig{Parallelism: 1}
			},
			application.FieldDrift{Field: "rollbackConfig", Desired: "set", Live: "absent"},
		},
		"restart condition against an unstated default": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{}
				live.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{
					Condition: swarm.RestartPolicyConditionOnFailure,
				}
			},
			application.FieldDrift{Field: "restartPolicy.condition", Desired: "any", Live: "on-failure"},
		},
		"restart max attempts against a nil the daemon returns as zero": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{MaxAttempts: uint64p(3)}
				live.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{}
			},
			application.FieldDrift{Field: "restartPolicy.maxAttempts", Desired: "3", Live: "0"},
		},
		"placement preferences": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.Placement = &swarm.Placement{
					Preferences: []swarm.PlacementPreference{
						{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.zone"}},
					},
				}
			},
			application.FieldDrift{
				Field:   "placement.preferences",
				Desired: "node.labels.zone",
				Live:    "",
			},
		},
		"placement max replicas per node": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.Placement = &swarm.Placement{MaxReplicas: 3}
			},
			application.FieldDrift{Field: "placement.maxReplicas", Desired: "3", Live: "0"},
		},
		// Swarm's Command is compose's `entrypoint`, and its Args is compose's
		// `command` — the opposite of what the field names suggest, which is why
		// both are reported under the names an operator would recognise.
		"entrypoint replaced": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Command = []string{"/bin/sleep"}
				live.TaskTemplate.ContainerSpec.Command = []string{"/bin/sh", "-c"}
			},
			application.FieldDrift{Field: "command", Desired: "/bin/sleep", Live: "/bin/sh -c"},
		},
		"arguments replaced": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Args = []string{"3600"}
				live.TaskTemplate.ContainerSpec.Args = []string{"while true; do :; done"}
			},
			application.FieldDrift{Field: "args", Desired: "3600", Live: "while true; do :; done"},
		},
		"user": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.User = "1000:1000"
			},
			application.FieldDrift{Field: "user", Desired: "1000:1000", Live: ""},
		},
		"read-only root filesystem cleared": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.ReadOnly = true
			},
			application.FieldDrift{Field: "readOnly", Desired: "true", Live: "false"},
		},
		// Three readings, not two: a nil is the daemon's own default, which is
		// configurable, so folding it into false would be inventing an answer.
		"init flag set where the manifest stated none": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.ContainerSpec.Init = boolp(true)
			},
			application.FieldDrift{Field: "init", Desired: "(unset)", Live: "true"},
		},
		"stop grace period": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.StopGracePeriod = durationp(5 * time.Second)
			},
			application.FieldDrift{Field: "stopGracePeriod", Desired: "5s", Live: "0s"},
		},
		"capability added by hand": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.ContainerSpec.CapabilityAdd = []string{"CAP_SYS_ADMIN"}
			},
			application.FieldDrift{Field: "capabilityAdd", Desired: "", Live: "CAP_SYS_ADMIN"},
		},
		// A v3.9 manifest has no `group_add`, so the desired side is always empty
		// here — which is exactly what makes a hand-added group visible.
		"supplementary group added by hand": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.ContainerSpec.Groups = []string{"docker"}
			},
			application.FieldDrift{Field: "groups", Desired: "", Live: "docker"},
		},
		"sysctl changed": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Sysctls = map[string]string{"net.core.somaxconn": "1024"}
				live.TaskTemplate.ContainerSpec.Sysctls = map[string]string{"net.core.somaxconn": "4096"}
			},
			application.FieldDrift{Field: "sysctls[net.core.somaxconn]", Desired: "1024", Live: "4096"},
		},
		"ulimit raised by hand": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Ulimits = []*container.Ulimit{
					{Name: "nofile", Soft: 1024, Hard: 2048},
				}
				live.TaskTemplate.ContainerSpec.Ulimits = []*container.Ulimit{
					{Name: "nofile", Soft: 8192, Hard: 8192},
				}
			},
			application.FieldDrift{Field: "ulimits[nofile]", Desired: "1024:2048", Live: "8192:8192"},
		},
		"dns removed entirely": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.DNSConfig = &swarm.DNSConfig{Nameservers: []string{"1.1.1.1"}}
			},
			application.FieldDrift{Field: "dns", Desired: "set", Live: "absent"},
		},
		// convertDNSConfig never fills the options list, so the desired side is
		// always empty — and a --dns-option-add is therefore always visible.
		"dns option added by hand": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.DNSConfig = &swarm.DNSConfig{Nameservers: []string{"1.1.1.1"}}
				live.TaskTemplate.ContainerSpec.DNSConfig = &swarm.DNSConfig{
					Nameservers: []string{"1.1.1.1"},
					Options:     []string{"ndots:2"},
				}
			},
			application.FieldDrift{Field: "dns.options", Desired: "", Live: "ndots:2"},
		},
		"log driver option": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.LogDriver = &swarm.Driver{
					Name: "json-file", Options: map[string]string{"max-size": "10m"},
				}
				live.TaskTemplate.LogDriver = &swarm.Driver{
					Name: "json-file", Options: map[string]string{"max-size": "50m"},
				}
			},
			application.FieldDrift{Field: "logDriver.options[max-size]", Desired: "10m", Live: "50m"},
		},
		// The container's labels, not the service's: two different sets, two
		// different flags, and only these reach the running container.
		"container label changed": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Labels = map[string]string{"role": "web"}
				live.TaskTemplate.ContainerSpec.Labels = map[string]string{"role": "edge"}
			},
			application.FieldDrift{Field: "containerLabels[role]", Desired: "web", Live: "edge"},
		},
		"isolation": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.ContainerSpec.Isolation = container.IsolationProcess
			},
			application.FieldDrift{Field: "isolation", Desired: "default", Live: "process"},
		},
		"oom score": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.ContainerSpec.OomScoreAdj = 500
			},
			application.FieldDrift{Field: "oomScoreAdj", Desired: "0", Live: "500"},
		},
		"stdin held open": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.OpenStdin = true
			},
			application.FieldDrift{Field: "openStdin", Desired: "true", Live: "false"},
		},
		"pids limit": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.Resources = &swarm.ResourceRequirements{Limits: &swarm.Limit{Pids: 100}}
			},
			application.FieldDrift{Field: "resources.limits.pids", Desired: "100", Live: "0"},
		},
		"generic resource reservation": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.Resources = &swarm.ResourceRequirements{
					Reservations: &swarm.Resources{GenericResources: []swarm.GenericResource{
						{DiscreteResourceSpec: &swarm.DiscreteGenericResource{Kind: "SSD", Value: 2}},
					}},
				}
			},
			application.FieldDrift{
				Field:   "resources.reservations.genericResources",
				Desired: "SSD=2",
				Live:    "",
			},
		},
		"named generic resource": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.Resources = &swarm.ResourceRequirements{
					Reservations: &swarm.Resources{GenericResources: []swarm.GenericResource{
						{NamedResourceSpec: &swarm.NamedGenericResource{Kind: "GPU", Value: "uuid-1"}},
					}},
				}
			},
			application.FieldDrift{
				Field:   "resources.reservations.genericResources",
				Desired: "",
				Live:    "GPU=uuid-1",
			},
		},
		// Only the credential spec has a service flag; the rest are reachable
		// through the API alone, which is why a change to one is worth seeing.
		"no-new-privileges cleared behind the controller's back": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{NoNewPrivileges: true}
			},
			application.FieldDrift{Field: "privileges.noNewPrivileges", Desired: "true", Live: "false"},
		},
		"credential spec": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{
					CredentialSpec: &swarm.CredentialSpec{Config: "s_credspec"},
				}
			},
			application.FieldDrift{Field: "privileges.credentialSpec", Desired: "config=s_credspec", Live: ""},
		},
		"seccomp profile swapped for unconfined": {
			func(want, live *swarm.ServiceSpec) {
				want.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{
					Seccomp: &swarm.SeccompOpts{Mode: swarm.SeccompModeCustom, Profile: []byte("{}")},
				}
				live.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{
					Seccomp: &swarm.SeccompOpts{Mode: swarm.SeccompModeUnconfined},
				}
			},
			application.FieldDrift{
				Field:   "privileges.seccomp",
				Desired: "custom (custom profile)",
				Live:    "unconfined",
			},
		},
		"apparmor disabled": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{
					AppArmor: &swarm.AppArmorOpts{Mode: swarm.AppArmorModeDisabled},
				}
			},
			application.FieldDrift{Field: "privileges.apparmor", Desired: "", Live: "disabled"},
		},
		"selinux label": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{
					SELinuxContext: &swarm.SELinuxContext{
						User: "system_u", Role: "object_r", Type: "svirt_t", Level: "s0",
					},
				}
			},
			application.FieldDrift{
				Field:   "privileges.selinux",
				Desired: "",
				Live:    "system_u:object_r:svirt_t:s0",
			},
		},
		"network attached that the manifest does not declare": {
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.Networks = append(live.TaskTemplate.Networks,
					swarm.NetworkAttachmentConfig{Target: networkID("s_extra")})
			},
			application.FieldDrift{
				Field:   "networks",
				Desired: defaultNetwork,
				Live:    defaultNetwork + ", s_extra",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			want, live := freshPair()
			tc.mutate(&want, &live)

			got := Live(stackOf(want), liveOf(live), swarmNets)
			if got.State != application.DriftStateDetected {
				t.Fatalf("state = %q, want detected", got.State)
			}
			if len(got.Services) != 1 || got.Services[0].Reason != application.DriftModified {
				t.Fatalf("services = %+v, want one modified", got.Services)
			}
			if fields := got.Services[0].Fields; !reflect.DeepEqual(fields, []application.FieldDrift{tc.want}) {
				t.Errorf("fields = %+v, want %+v", fields, tc.want)
			}
			if got.Services[0].Name != "s_web" {
				t.Errorf("name = %q, want the scoped name an operator would type", got.Services[0].Name)
			}
		})
	}
}

// freshPair returns two independent specs that compare equal, so a mutator can
// change one side without the other moving with it.
func freshPair() (swarm.ServiceSpec, swarm.ServiceSpec) {
	build := func() swarm.ServiceSpec {
		s := desiredSpec("web", "nginx:1.2")
		cs := *s.TaskTemplate.ContainerSpec
		s.TaskTemplate.ContainerSpec = &cs
		mode := *s.Mode.Replicated
		s.Mode.Replicated = &mode
		return s
	}
	return build(), build()
}

// A replicated job counts differently from a service, so the two counts under it
// need their own case: the mode branch is exclusive, and a job never reaches the
// replicas comparison at all.
func TestJobCountsAreCompared(t *testing.T) {
	job := func(maxConcurrent, total *uint64) swarm.ServiceMode {
		return swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{
			MaxConcurrent: maxConcurrent, TotalCompletions: total,
		}}
	}

	t.Run("a count the manifest left unstated", func(t *testing.T) {
		// `deploy: {mode: replicated-job}` with no `replicas:`. Compose leaves
		// both pointers nil; ServiceSpecToGRPC stores 1 for a nil MaxConcurrent
		// and MaxConcurrent for a nil TotalCompletions, and serviceSpecFromGRPC
		// addresses both stored values unconditionally — so the live side is a
		// pointer to one, never to zero.
		want, live := freshPair()
		want.Mode = job(nil, nil)
		live.Mode = job(uint64p(1), uint64p(1))

		if got := Live(stackOf(want), liveOf(live), swarmNets); got.State != application.DriftStateNone {
			t.Errorf("got %+v, want none — an unstated count is not a difference", got.Services)
		}
	})

	// The daemon's second default is not "one": a job told to run four at a time
	// and nothing else completes four. A spec that states only the concurrency
	// cannot come out of compose — convertDeployMode fills both counts from the
	// one `replicas:` pointer — but it is what the API accepts and what the
	// daemon then stores, so the desired side has to read it the daemon's way.
	t.Run("a total completions the daemon defaulted to the concurrency", func(t *testing.T) {
		want, live := freshPair()
		want.Mode = job(uint64p(4), nil)
		live.Mode = job(uint64p(4), uint64p(4))

		if got := Live(stackOf(want), liveOf(live), swarmNets); got.State != application.DriftStateNone {
			t.Errorf("got %+v, want none — an unstated total is the concurrency", got.Services)
		}
	})

	// The other half of the same default: an unstated count is one, so a job
	// somebody scaled by hand is still a difference rather than a flattened zero.
	t.Run("a count raised by hand against an unstated default", func(t *testing.T) {
		want, live := freshPair()
		want.Mode = job(nil, nil)
		live.Mode = job(uint64p(4), uint64p(9))

		fields := Live(stackOf(want), liveOf(live), swarmNets).Services[0].Fields
		want1 := []application.FieldDrift{
			{Field: "maxConcurrent", Desired: "1", Live: "4"},
			{Field: "totalCompletions", Desired: "1", Live: "9"},
		}
		if !reflect.DeepEqual(fields, want1) {
			t.Errorf("fields = %+v, want %+v", fields, want1)
		}
	})

	t.Run("concurrency raised by hand", func(t *testing.T) {
		want, live := freshPair()
		want.Mode = job(uint64p(1), uint64p(10))
		live.Mode = job(uint64p(4), uint64p(10))

		fields := Live(stackOf(want), liveOf(live), swarmNets).Services[0].Fields
		want1 := []application.FieldDrift{{Field: "maxConcurrent", Desired: "1", Live: "4"}}
		if !reflect.DeepEqual(fields, want1) {
			t.Errorf("fields = %+v, want %+v", fields, want1)
		}
	})

	t.Run("total completions changed", func(t *testing.T) {
		want, live := freshPair()
		want.Mode = job(uint64p(1), uint64p(10))
		live.Mode = job(uint64p(1), uint64p(99))

		fields := Live(stackOf(want), liveOf(live), swarmNets).Services[0].Fields
		want1 := []application.FieldDrift{{Field: "totalCompletions", Desired: "10", Live: "99"}}
		if !reflect.DeepEqual(fields, want1) {
			t.Errorf("fields = %+v, want %+v", fields, want1)
		}
	})
}

// A mode change makes the replica counts incomparable, so saying both would say
// the same thing twice and one of the two would be nonsense.
func TestModeChangeSuppressesReplicas(t *testing.T) {
	want, live := freshPair()
	want.Mode.Replicated.Replicas = uint64p(3)
	live.Mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}

	fields := Live(stackOf(want), liveOf(live), swarmNets).Services[0].Fields
	if len(fields) != 1 || fields[0].Field != "mode" {
		t.Errorf("fields = %+v, want only the mode change", fields)
	}
}

// Attachments are the one field that cannot be read without naming what the
// daemon stored, so there are three ways to be unable to compare them — and every
// one of them must report nothing rather than guess. Guessing here means claiming
// a detachment nobody performed, on the axis that redeploys unattended.
func TestNetworksAreNotComparedWhenTheyCannotBeNamed(t *testing.T) {
	// Each case detaches the service's only network, which is real drift, so a
	// case that stopped declining would fail loudly rather than silently.
	for name, tc := range map[string]struct {
		nets    NetworkNames
		prepare func(want, live *swarm.ServiceSpec)
	}{
		"the backend could not list the swarm's networks": {
			nil,
			func(want, live *swarm.ServiceSpec) { live.TaskTemplate.Networks = nil },
		},
		"an attachment names a network the listing did not": {
			swarmNets,
			func(want, live *swarm.ServiceSpec) {
				live.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{
					{Target: "netid-created-since-the-listing"},
				}
			},
		},
		// Below API 1.29 compose writes the deprecated field instead, which would
		// otherwise leave the desired side empty against a populated live one and
		// report a detachment on every service of every release, for ever.
		"the deprecated ServiceSpec.Networks is in use": {
			swarmNets,
			func(want, live *swarm.ServiceSpec) {
				//nolint:staticcheck // ignore SA1019: the deprecated field is the case.
				want.Networks = want.TaskTemplate.Networks
				want.TaskTemplate.Networks = nil
				live.TaskTemplate.Networks = nil
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			want, live := freshPair()
			tc.prepare(&want, &live)

			got := Live(stackOf(want), liveOf(live), tc.nets)
			if got.State != application.DriftStateNone {
				t.Errorf("got %+v, want none: an attachment that cannot be named proves nothing",
					got.Services)
			}
		})
	}
}

// The health axis only notices when every service of a release is gone, so one
// service missing from a multi-service stack is invisible without this.
func TestMissingServiceIsReported(t *testing.T) {
	web := desiredSpec("web", "nginx:1.2")
	db := desiredSpec("db", "postgres:16")

	got := Live(stackOf(web, db), liveOf(web), swarmNets)
	if len(got.Services) != 1 {
		t.Fatalf("services = %+v, want one", got.Services)
	}
	if want := (application.ServiceDrift{Name: "s_db", Reason: application.DriftMissing}); !reflect.DeepEqual(got.Services[0], want) {
		t.Errorf("got %+v, want s_db missing with no field list", got.Services[0])
	}
}

// Applying deletes nothing, so a service dropped from a chart's template stays
// on the swarm forever. This is the only thing that says so.
func TestUnexpectedServiceIsReported(t *testing.T) {
	web := desiredSpec("web", "nginx:1.2")
	stray := desiredSpec("old", "nginx:1.0")

	got := Live(stackOf(web), liveOf(web, stray), swarmNets)
	if len(got.Services) != 1 {
		t.Fatalf("services = %+v, want one", got.Services)
	}
	if want := (application.ServiceDrift{Name: "s_old", Reason: application.DriftUnexpected}); !reflect.DeepEqual(got.Services[0], want) {
		t.Errorf("got %+v, want s_old unexpected", got.Services[0])
	}
}

// rolledBackTo is a live service running an older spec because Swarm reverted the
// one this controller wrote — the state a real daemon leaves behind after
// `update_config.failure_action: rollback` fires.
func rolledBackTo(spec swarm.ServiceSpec, state swarm.UpdateState, message string) swarm.Service {
	return swarm.Service{
		ID:           "id-" + spec.Name,
		Spec:         spec,
		UpdateStatus: &swarm.UpdateStatus{State: state, Message: message},
	}
}

// The difference live drift could not previously see: a spec that stopped matching
// the repository because *Swarm* put it back, not because anybody changed it.
//
// Reported as its own reason so that convergeable can refuse it. Correcting it
// would re-apply the spec the platform has already rejected, which fails the same
// way for ever (#90).
func TestARolledBackServiceIsReportedAsSuch(t *testing.T) {
	const why = "update paused due to failure or early termination of task 9xk2"
	// `started` included, which reads like a rollout still deciding and is not
	// one: Updater.rollbackUpdate sets the state and swaps the previous spec back
	// in the same store transaction, so a spec that differs under it has already
	// been reverted. Missing it re-pushed the rejected spec, and that update
	// resets UpdateStatus — losing the evidence and starting the cycle again.
	for _, state := range []swarm.UpdateState{
		swarm.UpdateStateRollbackStarted,
		swarm.UpdateStateRollbackPaused,
		swarm.UpdateStateRollbackCompleted,
	} {
		t.Run(string(state), func(t *testing.T) {
			want := desiredSpec("web", "nginx:1.3")
			running := desiredSpec("web", "nginx:1.2")

			got := Live(stackOf(want), map[string]swarm.Service{
				"s_web": rolledBackTo(running, state, why),
			}, swarmNets)

			if len(got.Services) != 1 {
				t.Fatalf("services = %+v, want one", got.Services)
			}
			svc := got.Services[0]
			if svc.Reason != application.DriftRolledBack {
				t.Errorf("reason = %q, want %q", svc.Reason, application.DriftRolledBack)
			}
			if svc.Message != why {
				t.Errorf("message = %q, want the daemon's own account %q", svc.Message, why)
			}
			// The fields still say what the rejected spec asked for, which is the
			// next thing an operator wants to know.
			if len(svc.Fields) == 0 {
				t.Error("fields = none, want the difference the rejected spec asked for")
			}
			if got.State != application.DriftStateDetected {
				t.Errorf("state = %q, want the release still reported as drifted", got.State)
			}
		})
	}
}

// Every other update state is somebody's write, not the platform's verdict.
// `paused` matters most: it is the *default* failure_action, and it leaves the
// spec Swarm accepted in place — so it is a convergence question, which is
// syncPolicy.wait's, and a service still differing under it was changed by hand.
func TestOnlyARollbackStateIsARollback(t *testing.T) {
	for _, state := range []swarm.UpdateState{
		swarm.UpdateStateUpdating,
		swarm.UpdateStatePaused,
		swarm.UpdateStateCompleted,
	} {
		t.Run(string(state), func(t *testing.T) {
			want := desiredSpec("web", "nginx:1.3")
			running := desiredSpec("web", "nginx:1.2")

			got := Live(stackOf(want), map[string]swarm.Service{
				"s_web": rolledBackTo(running, state, "in flight"),
			}, swarmNets)

			if len(got.Services) != 1 || got.Services[0].Reason != application.DriftModified {
				t.Errorf("services = %+v, want one modified service", got.Services)
			}
			if got.Services[0].Message != "" {
				t.Errorf("message = %q, want none for a reason that is not a rollback", got.Services[0].Message)
			}
		})
	}
}

// UpdateStatus persists on a service until its next update, so a rollback is not
// evidence of anything on its own — an operator's own bad `docker service update`,
// rolled back to the spec the repository asks for, leaves one behind for ever. The
// specs agreeing is what makes it history rather than a finding.
func TestARollbackThatRestoredWhatTheRepositoryWantsIsNotDrift(t *testing.T) {
	spec := desiredSpec("web", "nginx:1.2")

	got := Live(stackOf(spec), map[string]swarm.Service{
		"s_web": rolledBackTo(spec, swarm.UpdateStateRollbackCompleted, "somebody else's failed update"),
	}, swarmNets)

	if got.State != application.DriftStateNone || len(got.Services) != 0 {
		t.Errorf("drift = %+v, want none: the running spec is what the repository asks for", got)
	}
}

// An unbounded field list would be served on every poll of the detail view.
func TestFieldsAreCapped(t *testing.T) {
	want, live := freshPair()
	for i := range 30 {
		key := "VAR" + string(rune('A'+i))
		want.TaskTemplate.ContainerSpec.Env = append(want.TaskTemplate.ContainerSpec.Env, key+"=x")
	}

	svc := Live(stackOf(want), liveOf(live), swarmNets).Services[0]
	if len(svc.Fields) != maxFields {
		t.Errorf("reported %d fields, want them capped at %d", len(svc.Fields), maxFields)
	}
	if svc.Truncated != 30-maxFields {
		t.Errorf("truncated = %d, want %d", svc.Truncated, 30-maxFields)
	}
}

// A status poll that reported the same differences in a different order every
// time would look like a change on every tick.
func TestFieldOrderIsStable(t *testing.T) {
	want, live := freshPair()
	want.TaskTemplate.ContainerSpec.Env = []string{"ZED=1", "ALPHA=1", "MIDDLE=1"}

	var first []application.FieldDrift
	for range 5 {
		got := Live(stackOf(want), liveOf(live), swarmNets).Services[0].Fields
		if first == nil {
			first = got
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("field order changed between runs:\n got %+v\nwant %+v", got, first)
		}
	}
	if len(first) != 3 || first[0].Field != "env[ALPHA]" {
		t.Errorf("fields = %+v, want them sorted by name", first)
	}
}

// The stack labels are the controller's own bookkeeping. The image label in
// particular records the tag that was asked for rather than what is running,
// which is what compareImage depends on — reporting it would turn every
// resolved digest into a spurious label difference.
func TestStackOwnLabelsAreNotCompared(t *testing.T) {
	want, live := freshPair()
	live.Labels[convert.LabelImage] = "nginx:0.1"
	live.Labels[convert.LabelNamespace] = "somethingelse"

	if got := Live(stackOf(want), liveOf(live), swarmNets); got.State != application.DriftStateNone {
		t.Errorf("got %+v, want the controller's own labels ignored", got.Services)
	}
}

func TestApplicationRollup(t *testing.T) {
	detected := &application.ReleaseDrift{
		State:    application.DriftStateDetected,
		Services: []application.ServiceDrift{{Name: "s_web"}, {Name: "s_db"}},
	}
	unknown := &application.ReleaseDrift{State: application.DriftStateUnknown, Message: "swarm unreachable"}
	none := &application.ReleaseDrift{State: application.DriftStateNone}

	t.Run("nil when nothing was compared", func(t *testing.T) {
		if got := Application([]application.ReleaseStatus{{Name: "a"}, {Name: "b"}}); got != nil {
			t.Errorf("got %+v, want nil for an application that does not use live mode", got)
		}
	})

	t.Run("none when every comparison agreed", func(t *testing.T) {
		got := Application([]application.ReleaseStatus{{Name: "a", Drift: none}})
		if got == nil || got.State != application.DriftStateNone || got.Services != 0 {
			t.Errorf("got %+v, want none", got)
		}
	})

	t.Run("counts services across releases", func(t *testing.T) {
		got := Application([]application.ReleaseStatus{
			{Name: "a", Drift: none},
			{Name: "b", Drift: detected},
		})
		if got.State != application.DriftStateDetected || got.Services != 2 {
			t.Errorf("got %+v, want detected with 2 services", got)
		}
		if got.Message == "" {
			t.Error("no message naming the release that drifted")
		}
	})

	// Detected outranks Unknown: a difference that was found is something to act
	// on, whereas one that could not be read is a gap in the reporting.
	t.Run("detected outranks unknown", func(t *testing.T) {
		got := Application([]application.ReleaseStatus{
			{Name: "a", Drift: unknown},
			{Name: "b", Drift: detected},
		})
		if got.State != application.DriftStateDetected {
			t.Errorf("state = %q, want detected", got.State)
		}
	})

	t.Run("unknown outranks none", func(t *testing.T) {
		got := Application([]application.ReleaseStatus{
			{Name: "a", Drift: none},
			{Name: "b", Drift: unknown},
		})
		if got.State != application.DriftStateUnknown || got.Message == "" {
			t.Errorf("got %+v, want unknown with the reason", got)
		}
	})
}
