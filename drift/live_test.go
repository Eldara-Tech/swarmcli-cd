// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package drift

import (
	"reflect"
	"testing"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/compose"
)

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
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: image}},
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}},
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

// The test the whole allowlist exists for.
//
// Every entry here is a way the daemon legitimately returns something other
// than what was written. If any of them reported drift, a controller would
// converge a service nobody touched — on every tick, forever — and the right
// response from an operator would be to turn the feature off.
func TestNormalisedDifferencesAreNotDrift(t *testing.T) {
	for name, mutate := range map[string]func(live *swarm.ServiceSpec){
		"replicas defaulted from nil to one": func(live *swarm.ServiceSpec) {
			live.Mode.Replicated.Replicas = uint64p(1)
		},
		"image resolved to a digest": func(live *swarm.ServiceSpec) {
			live.TaskTemplate.ContainerSpec.Image = "nginx:1.2@sha256:aaaa"
		},
		"force update counter advanced": func(live *swarm.ServiceSpec) {
			live.TaskTemplate.ForceUpdate = 3
		},
		"labels returned as an empty map rather than nil": func(live *swarm.ServiceSpec) {
			live.TaskTemplate.ContainerSpec.Labels = map[string]string{}
		},
		"resources returned as zero structs rather than nil": func(live *swarm.ServiceSpec) {
			live.TaskTemplate.Resources = &swarm.ResourceRequirements{
				Limits:       &swarm.Limit{},
				Reservations: &swarm.Resources{},
			}
		},
		"placement returned as a zero struct rather than nil": func(live *swarm.ServiceSpec) {
			live.TaskTemplate.Placement = &swarm.Placement{}
		},
		"endpoint resolution mode defaulted to vip": func(live *swarm.ServiceSpec) {
			live.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP}
		},
		"published port assigned by the daemon": func(live *swarm.ServiceSpec) {
			live.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{
				TargetPort: 80, PublishedPort: 30000, Protocol: "tcp", PublishMode: "ingress",
			}}}
		},
		"platforms filled from the image manifest": func(live *swarm.ServiceSpec) {
			live.TaskTemplate.Placement = &swarm.Placement{
				Platforms: []swarm.Platform{{Architecture: "amd64", OS: "linux"}},
			}
		},
		"network target resolved from a name to an id": func(live *swarm.ServiceSpec) {
			live.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "abc123netid"}}
		},
		"update config defaulted by swarmkit": func(live *swarm.ServiceSpec) {
			live.UpdateConfig = &swarm.UpdateConfig{Parallelism: 1, FailureAction: "pause", Order: "stop-first"}
		},
		"isolation defaulted": func(live *swarm.ServiceSpec) {
			live.TaskTemplate.ContainerSpec.Isolation = "default"
		},
		"privileges populated": func(live *swarm.ServiceSpec) {
			live.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{}
		},
		"runtime defaulted to container": func(live *swarm.ServiceSpec) {
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

			mutate(&live)

			got := Live(stackOf(want), liveOf(live))
			if got.State != application.DriftStateNone {
				t.Errorf("state = %q with %v, want none — this is normalisation, not drift",
					got.State, got.Services)
			}
		})
	}
}

func TestLiveReportsNoDriftOnAnIdenticalStack(t *testing.T) {
	spec := desiredSpec("web", "nginx:1.2")
	got := Live(stackOf(spec), liveOf(spec))
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
	} {
		t.Run(name, func(t *testing.T) {
			want, live := freshPair()
			tc.mutate(&want, &live)

			got := Live(stackOf(want), liveOf(live))
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

// A mode change makes the replica counts incomparable, so saying both would say
// the same thing twice and one of the two would be nonsense.
func TestModeChangeSuppressesReplicas(t *testing.T) {
	want, live := freshPair()
	want.Mode.Replicated.Replicas = uint64p(3)
	live.Mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}

	fields := Live(stackOf(want), liveOf(live)).Services[0].Fields
	if len(fields) != 1 || fields[0].Field != "mode" {
		t.Errorf("fields = %+v, want only the mode change", fields)
	}
}

// The health axis only notices when every service of a release is gone, so one
// service missing from a multi-service stack is invisible without this.
func TestMissingServiceIsReported(t *testing.T) {
	web := desiredSpec("web", "nginx:1.2")
	db := desiredSpec("db", "postgres:16")

	got := Live(stackOf(web, db), liveOf(web))
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

	got := Live(stackOf(web), liveOf(web, stray))
	if len(got.Services) != 1 {
		t.Fatalf("services = %+v, want one", got.Services)
	}
	if want := (application.ServiceDrift{Name: "s_old", Reason: application.DriftUnexpected}); !reflect.DeepEqual(got.Services[0], want) {
		t.Errorf("got %+v, want s_old unexpected", got.Services[0])
	}
}

// An unbounded field list would be served on every poll of the detail view.
func TestFieldsAreCapped(t *testing.T) {
	want, live := freshPair()
	for i := range 30 {
		key := "VAR" + string(rune('A'+i))
		want.TaskTemplate.ContainerSpec.Env = append(want.TaskTemplate.ContainerSpec.Env, key+"=x")
	}

	svc := Live(stackOf(want), liveOf(live)).Services[0]
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
		got := Live(stackOf(want), liveOf(live)).Services[0].Fields
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

	if got := Live(stackOf(want), liveOf(live)); got.State != application.DriftStateNone {
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
