// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package drift

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/compose"
)

// Live compares what a release's manifest declares against what is running.
//
// # Why this is an allowlist
//
// A ServiceSpec read back from the daemon is not the one that was written.
// Swarmkit defaults fields the manifest never mentioned, resolves images to
// digests, and returns empty maps where the caller sent nil. Comparing whole
// specs therefore reports permanent drift on a service nobody has touched,
// which is worse than no drift detection at all — it is the failure that makes
// an operator turn the feature off, and after that it detects nothing.
//
// So this compares a named set of fields and nothing else. The boundary is what
// `docker service update` can change, which is not an arbitrary line: it is the
// out-of-band mutation surface this exists to observe. A field is added to the
// list only with a normalisation that a real swarm has been observed to
// satisfy — see integration-tests/live_drift_test.go, which deploys a stack
// using every one of them and asserts that an untouched deploy reports nothing.
//
// Everything outside the list is documented as not compared rather than
// silently missed; docs/configuration.md carries that list.
//
// # Why not YAML
//
// Diffing a reconstructed compose file against the manifest would read better
// and be wrong: compose → ServiceSpec is lossy and one-way, so the
// reconstruction manufactures differences that do not exist. Established with a
// reproduction in swarmcli-cd#1 and restated in CLAUDE.md.
//
// # Naming what the daemon stores as an id
//
// One compared field cannot be read without help. A service's attached networks
// are stored by id where the manifest named a network, so nets carries the
// swarm's networks by id and the comparison names them before comparing. Nil is
// allowed and means the backend could not answer: attachments are then not
// compared, which loses one field rather than the whole report.
func Live(desired *compose.Stack, live map[string]swarm.Service, nets NetworkNames) *application.ReleaseDrift {
	out := &application.ReleaseDrift{State: application.DriftStateNone}
	if desired == nil {
		return out
	}

	declared := make(map[string]struct{}, len(desired.Services))
	for _, svc := range desired.Services {
		// The scoped name: what the live spec carries, and the only key the two
		// sides can be matched on.
		name := svc.Spec.Name
		declared[name] = struct{}{}

		cur, ok := live[name]
		if !ok {
			out.Services = append(out.Services, application.ServiceDrift{
				Name:   name,
				Reason: application.DriftMissing,
			})
			continue
		}
		if fields, truncated := compareService(svc.Spec, cur.Spec, nets); len(fields) > 0 {
			reason, message := application.DriftModified, ""
			if why, ok := rolledBack(cur); ok {
				reason, message = application.DriftRolledBack, why
			}
			out.Services = append(out.Services, application.ServiceDrift{
				Name:      name,
				Reason:    reason,
				Fields:    fields,
				Truncated: truncated,
				Message:   message,
			})
		}
	}

	// A service carrying the stack's namespace label that the manifest does not
	// declare. Reported, never removed: applying deletes nothing, so this is
	// also how a service dropped from a chart's template — which lingers on the
	// swarm forever today — becomes visible at all.
	var extra []string
	for name := range live {
		if _, ok := declared[name]; !ok {
			extra = append(extra, name)
		}
	}
	slices.Sort(extra)
	for _, name := range extra {
		out.Services = append(out.Services, application.ServiceDrift{
			Name:   name,
			Reason: application.DriftUnexpected,
		})
	}

	if len(out.Services) > 0 {
		out.State = application.DriftStateDetected
	}
	return out
}

// rolledBack reports whether Swarm reverted this service's spec, and the
// daemon's own account of why.
//
// It is the difference between the two writes live drift cannot otherwise tell
// apart. A service whose spec no longer matches the repository was either changed
// by somebody — redeploy it — or reverted by the platform because the spec this
// controller wrote would not converge, in which case redeploying is the one
// answer that cannot work: Swarm has already judged it (swarmcli-cd#90).
//
// Only the two rollback states count. `paused` is the *default* failure_action
// and leaves the spec Swarm accepted in place, so it produces no difference here
// and is a convergence question — which is syncPolicy.wait's. `updating` is a
// rollout in progress and says nothing about the outcome.
//
// Deliberately only consulted when the specs already differ. UpdateStatus
// persists on a service until its next update, so a rollback somebody's own
// `docker service update` provoked and that restored what the repository asks for
// would otherwise be reported for ever, on a release that is exactly where it
// should be.
func rolledBack(live swarm.Service) (string, bool) {
	if live.UpdateStatus == nil {
		return "", false
	}
	switch live.UpdateStatus.State {
	case swarm.UpdateStateRollbackCompleted, swarm.UpdateStateRollbackPaused:
		return live.UpdateStatus.Message, true
	default:
		return "", false
	}
}

// maxFields bounds one service's reported differences. A service whose every
// environment variable was replaced would otherwise put an unbounded list into
// a status payload that is served on every poll.
const maxFields = 20

// NetworkNames maps a network's id to its name.
//
// Nil means the backend could not list the swarm's networks, in which case
// attached networks are not compared. That is the degradation the sweep's
// resourceLister already takes: lose the one field that needed the read, not the
// whole comparison.
type NetworkNames map[string]string

// compareService returns the allowlisted fields that differ, and how many more
// were found beyond the cap.
func compareService(want, got swarm.ServiceSpec, nets NetworkNames) ([]application.FieldDrift, int) {
	var fields []application.FieldDrift
	add := func(name, desired, live string) {
		fields = append(fields, application.FieldDrift{Field: name, Desired: desired, Live: live})
	}

	// Mode first: the counts beneath it mean nothing across a mode change, and
	// reporting both would say the same thing twice.
	wantMode, gotMode := modeKind(want.Mode), modeKind(got.Mode)
	switch {
	case wantMode != gotMode:
		add("mode", wantMode, gotMode)
	case wantMode == modeReplicated:
		if w, g := replicas(want.Mode), replicas(got.Mode); w != g {
			add("replicas", strconv.FormatUint(w, 10), strconv.FormatUint(g, 10))
		}
	case wantMode == modeReplicatedJob:
		compareJob(want.Mode.ReplicatedJob, got.Mode.ReplicatedJob, add)
	}

	compareImage(want, got, add)
	compareResources(want.TaskTemplate.Resources, got.TaskTemplate.Resources, add)
	compareEnv(containerSpec(want).Env, containerSpec(got).Env, add)
	compareConstraints(want.TaskTemplate.Placement, got.TaskTemplate.Placement, add)
	compareLabels(want.Labels, got.Labels, add)
	comparePorts(want.EndpointSpec, got.EndpointSpec, add)
	compareEndpointMode(want.EndpointSpec, got.EndpointSpec, add)
	compareNetworks(want, got, nets, add)
	compareMounts(containerSpec(want).Mounts, containerSpec(got).Mounts, add)
	compareSecretRefs(containerSpec(want).Secrets, containerSpec(got).Secrets, add)
	compareConfigRefs(containerSpec(want).Configs, containerSpec(got).Configs, add)
	compareHealthcheck(containerSpec(want).Healthcheck, containerSpec(got).Healthcheck, add)
	compareUpdateConfig("updateConfig", want.UpdateConfig, got.UpdateConfig, add)
	compareUpdateConfig("rollbackConfig", want.RollbackConfig, got.RollbackConfig, add)
	compareRestartPolicy(want.TaskTemplate.RestartPolicy, got.TaskTemplate.RestartPolicy, add)
	comparePlacement(want.TaskTemplate.Placement, got.TaskTemplate.Placement, add)
	compareContainer(containerSpec(want), containerSpec(got), add)
	compareLogDriver(want.TaskTemplate.LogDriver, got.TaskTemplate.LogDriver, add)

	if len(fields) > maxFields {
		return fields[:maxFields], len(fields) - maxFields
	}
	return fields, 0
}

const (
	modeReplicated    = "replicated"
	modeReplicatedJob = "replicated-job"
)

// compareJob reports the two counts that make a replicated job, which are to a
// job what replicas are to a service.
//
// Both sides flatten a nil to zero, because serviceSpecFromGRPC addresses the
// stored value unconditionally — the same shape RestartPolicy.MaxAttempts has —
// so the live side is never nil where the manifest left a count unstated.
//
// Zero is reported as zero rather than as the one swarmkit would actually run.
// This reports spec values throughout, and a rendering that guessed the effective
// count would be one more thing that could be wrong about a number nobody can
// then check.
func compareJob(want, got *swarm.ReplicatedJob, add func(name, desired, live string)) {
	if want == nil || got == nil {
		return
	}
	compareUint64("maxConcurrent", orZeroUint64(want.MaxConcurrent), orZeroUint64(got.MaxConcurrent), add)
	compareUint64("totalCompletions", orZeroUint64(want.TotalCompletions), orZeroUint64(got.TotalCompletions), add)
}

// modeKind names which of the four service modes is set. Replicated is the
// default for the same reason compose conversion makes it one: an unset mode is
// a replicated service.
func modeKind(m swarm.ServiceMode) string {
	switch {
	case m.Global != nil:
		return "global"
	case m.ReplicatedJob != nil:
		return "replicated-job"
	case m.GlobalJob != nil:
		return "global-job"
	default:
		return modeReplicated
	}
}

// replicas is the replica count with the daemon's default applied.
//
// Compose leaves Replicas nil when the manifest says nothing
// (cli/compose/convert.convertDeployMode), and the daemon stores 1 for a nil
// pointer and always reads one back
// (moby/daemon/cluster/convert.ServiceSpecToGRPC). Comparing the pointers
// directly would report drift on every service that did not name a count.
func replicas(m swarm.ServiceMode) uint64 {
	if m.Replicated == nil || m.Replicated.Replicas == nil {
		return 1
	}
	return *m.Replicated.Replicas
}

// compareImage reports an image the manifest did not ask for.
//
// The live spec's image is not comparable to the manifest's as written: with
// image resolution on, the daemon appends the digest it resolved the tag to. So
// a live image matches when it is either exactly what we asked for — which
// covers a manifest that pins a digest itself — or that same reference with a
// digest appended.
//
// What this must catch is `docker service update --image`, and note what it
// does NOT rely on: the stack image label. That update rewrites the spec's
// image and leaves the label at whatever the last deploy wrote, so comparing
// the two labels would agree in exactly the case this is looking for.
func compareImage(want, got swarm.ServiceSpec, add func(name, desired, live string)) {
	wanted := containerSpec(want).Image
	running := containerSpec(got).Image
	if wanted == "" {
		return
	}
	if running == wanted || imageTag(running) == wanted {
		return
	}
	add("image", wanted, running)
}

// imageTag strips a digest suffix. LastIndex rather than Cut so a reference
// containing more than one "@" loses only the digest.
func imageTag(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		return image[:i]
	}
	return image
}

// compareResources reports changed CPU and memory limits and reservations.
//
// Raw spec values rather than a friendly rendering: the field names say the
// unit, the numbers are exactly what `docker service inspect` shows, and a
// formatter is one more thing that can be wrong about a number nobody can then
// check.
func compareResources(want, got *swarm.ResourceRequirements, add func(name, desired, live string)) {
	wl, gl := limits(want), limits(got)
	compareInt64("resources.limits.nanoCPUs", wl.NanoCPUs, gl.NanoCPUs, add)
	compareInt64("resources.limits.memoryBytes", wl.MemoryBytes, gl.MemoryBytes, add)
	compareInt64("resources.limits.pids", wl.Pids, gl.Pids, add)

	wr, gr := reservations(want), reservations(got)
	compareInt64("resources.reservations.nanoCPUs", wr.NanoCPUs, gr.NanoCPUs, add)
	compareInt64("resources.reservations.memoryBytes", wr.MemoryBytes, gr.MemoryBytes, add)
	compareStringSet("resources.reservations.genericResources",
		genericResources(wr), genericResources(gr), add)
}

// genericResources names each reserved node resource as "kind=value", the form
// `--generic-resource-add` takes.
//
// Sorted rather than positional, like the other sets: the order a service
// reserves a node's resources in carries no meaning.
func genericResources(r swarm.Resources) []string {
	out := make([]string, 0, len(r.GenericResources))
	for _, g := range r.GenericResources {
		switch {
		case g.NamedResourceSpec != nil:
			out = append(out, g.NamedResourceSpec.Kind+"="+g.NamedResourceSpec.Value)
		case g.DiscreteResourceSpec != nil:
			out = append(out, g.DiscreteResourceSpec.Kind+"="+
				strconv.FormatInt(g.DiscreteResourceSpec.Value, 10))
		}
	}
	return out
}

// limits and reservations flatten the pointer chain the manifest leaves nil and
// the daemon returns as a zero struct. Both readings mean "no limit".
func limits(r *swarm.ResourceRequirements) swarm.Limit {
	if r == nil || r.Limits == nil {
		return swarm.Limit{}
	}
	return *r.Limits
}

func reservations(r *swarm.ResourceRequirements) swarm.Resources {
	if r == nil || r.Reservations == nil {
		return swarm.Resources{}
	}
	return *r.Reservations
}

func compareInt64(name string, want, got int64, add func(name, desired, live string)) {
	if want != got {
		add(name, strconv.FormatInt(want, 10), strconv.FormatInt(got, 10))
	}
}

func compareUint64(name string, want, got uint64, add func(name, desired, live string)) {
	if want != got {
		add(name, strconv.FormatUint(want, 10), strconv.FormatUint(got, 10))
	}
}

func compareString(name, want, got string, add func(name, desired, live string)) {
	if want != got {
		add(name, want, got)
	}
}

func compareBool(name string, want, got bool, add func(name, desired, live string)) {
	if want != got {
		add(name, strconv.FormatBool(want), strconv.FormatBool(got))
	}
}

// compareOptionalBool compares a flag that may be unstated, keeping the three
// readings apart rather than folding nil into false.
func compareOptionalBool(name string, want, got *bool, add func(name, desired, live string)) {
	compareString(name, renderOptionalBool(want), renderOptionalBool(got), add)
}

func renderOptionalBool(v *bool) string {
	if v == nil {
		return unsetBool
	}
	return strconv.FormatBool(*v)
}

// compareDuration renders through time.Duration.String, so a difference reads
// "30s" rather than as the nanoseconds the spec stores.
func compareDuration(name string, want, got time.Duration, add func(name, desired, live string)) {
	if want != got {
		add(name, want.String(), got.String())
	}
}

// compareFloat32 is for the one non-integral number in the compared set, an
// update config's failure ratio. Formatted at 32-bit precision, which is the
// width the field actually has: widening it to a float64 would render 0.3 as
// 0.30000001192092896.
func compareFloat32(name string, want, got float32, add func(name, desired, live string)) {
	if want != got {
		add(name, formatFloat32(want), formatFloat32(got))
	}
}

func formatFloat32(v float32) string {
	return strconv.FormatFloat(float64(v), 'g', -1, 32)
}

// Renderings for a difference in something that is either there or not.
//
// Environment variables are the reason the first two exist and the reason a
// value is never one of them: what is running is whatever an operator typed,
// this is served to anyone with read scope, and an environment variable is
// exactly where a credential would be. They are not env-specific, though —
// anything compared as a set renders this way, because a set entry has no key to
// hang a value on. Note the other rendering of "not there", `absent` below, which
// is what a *keyed* value shows; the two have been spelled differently since the
// first version of this file.
const (
	valueSet    = "set"
	valueAbsent = "absent"
	envChanged  = "changed"
)

// compareEnv reports environment variables added, removed or changed, by name.
func compareEnv(want, got []string, add func(name, desired, live string)) {
	wm, gm := envMap(want), envMap(got)
	for _, key := range unionKeys(wm, gm) {
		wv, inWant := wm[key]
		gv, inGot := gm[key]
		switch {
		case inWant && !inGot:
			add("env["+key+"]", valueSet, valueAbsent)
		case !inWant && inGot:
			add("env["+key+"]", valueAbsent, valueSet)
		case wv != gv:
			add("env["+key+"]", valueSet, envChanged)
		}
	}
}

// envMap splits "KEY=value" entries. An entry with no "=" is a variable passed
// through from the environment, which has a name and no value here.
func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		key, value, _ := strings.Cut(e, "=")
		out[key] = value
	}
	return out
}

// compareConstraints reports the placement constraints as a whole rather than
// one entry at a time: they are read together, and "want a, b; got a" is
// clearer than two lines about a single expression.
//
// Sorted on both sides because neither the manifest's order nor the daemon's is
// meaningful, and an order-sensitive comparison would report drift on a
// reordered chart template.
func compareConstraints(want, got *swarm.Placement, add func(name, desired, live string)) {
	w, g := placementConstraints(want), placementConstraints(got)
	if !slices.Equal(w, g) {
		add("constraints", strings.Join(w, ", "), strings.Join(g, ", "))
	}
}

func placementConstraints(p *swarm.Placement) []string {
	if p == nil {
		return nil
	}
	out := slices.Clone(p.Constraints)
	slices.Sort(out)
	return out
}

// absent is how a label that is not set renders, so that a difference always
// has two sides to read.
const absent = "(absent)"

// compareLabels reports service labels added, removed or changed.
//
// Values are shown here, unlike environment variables: a label is declared in
// the chart and is not where anyone puts a credential.
//
// The com.docker.stack.* labels are excluded because they are ours, not the
// operator's: the namespace label is how a stack is identified at all, and the
// image label deliberately records the tag we asked for rather than what is
// running, which compareImage depends on.
func compareLabels(want, got map[string]string, add func(name, desired, live string)) {
	compareKeyed("labels", ownLabels(want), ownLabels(got), add)
}

func ownLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if k == convert.LabelNamespace || k == convert.LabelImage {
			continue
		}
		out[k] = v
	}
	return out
}

func orAbsent(value string, present bool) string {
	if !present {
		return absent
	}
	return value
}

// portDynamic renders a published port the manifest left the daemon to pick.
const portDynamic = "dynamic"

// comparePorts reports published ports added and removed.
//
// Reported as a set, unlike every other collection here, because a port has no
// stable key to hang a value on: "8080:80" and "8081:80" differ only in the
// published side, so keying on the target would collide, and keying on the
// published port would make a changed one look like a different port entirely. A
// change therefore reads as one entry gone and one arrived — which is exactly
// what `--publish-rm` plus `--publish-add` did.
//
// What the daemon does *not* do here is worth writing down, because the opposite
// is the natural assumption and it is what makes this comparison safe at all: it
// never writes the port it assigned back into the spec. Swarmkit allocates a
// dynamic publish into the service's runtime Endpoint
// (manager/allocator.serviceAllocatePorts) and leaves Spec.Endpoint untouched, so
// a manifest that published without naming a port reads back as zero on both
// sides rather than meeting an assigned 30000-something.
func comparePorts(want, got *swarm.EndpointSpec, add func(name, desired, live string)) {
	comparePresence("ports", portSet(want), portSet(got), add)
}

// portSet is one endpoint's ports in canonical form.
//
// Both enum defaults are applied, because an empty protocol or publish mode is
// stored as the zero enum and read back as its name: "tcp" and "ingress"
// (daemon/cluster/convert.ServiceSpecToGRPC, then swarmPortConfigToAPIPortConfig).
// Comparing the raw strings would report drift on every port that named neither.
func portSet(e *swarm.EndpointSpec) map[string]struct{} {
	if e == nil {
		return nil
	}
	out := make(map[string]struct{}, len(e.Ports))
	for _, p := range e.Ports {
		out[portKey(p)] = struct{}{}
	}
	return out
}

func portKey(p swarm.PortConfig) string {
	published := portDynamic
	if p.PublishedPort != 0 {
		published = strconv.FormatUint(uint64(p.PublishedPort), 10)
	}
	protocol := string(p.Protocol)
	if protocol == "" {
		protocol = string(swarm.PortConfigProtocolTCP)
	}
	mode := string(p.PublishMode)
	if mode == "" {
		mode = string(swarm.PortConfigPublishModeIngress)
	}
	return published + ":" + strconv.FormatUint(uint64(p.TargetPort), 10) + "/" + protocol + "/" + mode
}

// comparePresence reports the entries of one set the other does not have.
func comparePresence(prefix string, want, got map[string]struct{}, add func(name, desired, live string)) {
	for _, key := range unionKeys(want, got) {
		_, inWant := want[key]
		_, inGot := got[key]
		if inWant == inGot {
			continue
		}
		add(prefix+"["+key+"]", presence(inWant), presence(inGot))
	}
}

func presence(in bool) string {
	if in {
		return valueSet
	}
	return valueAbsent
}

// compareEndpointMode reports a changed service discovery mode.
//
// Empty means vip on both sides. Compose passes the manifest's endpoint_mode
// through as written (cli/compose/convert.convertEndpointSpec), so a manifest
// that named none leaves it empty; the daemon stores the zero enum for that and
// reads "vip" back. Comparing the raw values would report drift on every service
// that did not name one.
func compareEndpointMode(want, got *swarm.EndpointSpec, add func(name, desired, live string)) {
	if w, g := resolutionMode(want), resolutionMode(got); w != g {
		add("endpointMode", w, g)
	}
}

func resolutionMode(e *swarm.EndpointSpec) string {
	if e == nil || e.Mode == "" {
		return string(swarm.ResolutionModeVIP)
	}
	return string(e.Mode)
}

// compareNetworks reports the networks a service is attached to, as a whole set
// rather than one at a time: attachments are read together, like constraints, and
// "want a, b; got a" is clearer than two lines about one detachment.
//
// The live side has to be named before it can be compared at all. The manifest
// names a network and the daemon stores an id, rewriting each Target in place as
// it creates or updates the service (daemon/cluster.populateNetworkID) — so this
// is a resolution step before it is a comparison.
//
// Three ways to decline, each reporting nothing rather than guessing:
//
//   - No map at all: the backend could not list the swarm's networks.
//   - An id the map does not name — a network created since the listing, or one
//     this controller cannot see. Silence about one service's attachments beats a
//     report that names an id.
//   - Either side carrying the deprecated ServiceSpec.Networks. Compose writes
//     that field instead of TaskTemplate.Networks below API 1.29
//     (cli/compose/convert.Service), which would leave the desired side empty
//     against a populated live one and report drift on every service of every
//     release, for ever.
//
// One thing that is safe and reads as though it should not be: a service that
// publishes a port is attached to the ingress network, but swarmkit records that
// on the runtime Endpoint's virtual IPs and on the task, never on the spec — so
// the spec's list stays what was written.
func compareNetworks(want, got swarm.ServiceSpec, nets NetworkNames, add func(name, desired, live string)) {
	//nolint:staticcheck // ignore SA1019: reading the deprecated field is the point.
	if nets == nil || len(want.Networks) > 0 || len(got.Networks) > 0 {
		return
	}
	live, ok := resolvedNetworkNames(got.TaskTemplate.Networks, nets)
	if !ok {
		return
	}
	if declared := declaredNetworkNames(want.TaskTemplate.Networks); !slices.Equal(declared, live) {
		add("networks", strings.Join(declared, ", "), strings.Join(live, ", "))
	}
}

// unsetBool renders an optional flag the manifest never stated. Distinct from
// "false" on purpose: what a nil means is the daemon's own default, which is
// configurable, so calling it false would be this package inventing an answer.
const unsetBool = "(unset)"

// compareContainer reports the container fields a manifest states literally.
//
// This is the cheapest group in the allowlist and the reason is worth recording:
// none of it needs a normalisation. containerToGRPC and containerSpecFromGRPC
// carry every one of these across untouched — plain strings, string slices, maps
// and pointers — and swarmkit defaults none of them. What the manifest declared
// is exactly what reads back.
//
// What they do need is order-insensitivity wherever the spec stores a list that
// is really a set. `--host-add` appends wherever it likes, docker/cli sorts
// capabilities on the way out but whoever typed them did not, and a chart
// template that reorders a list has not changed what runs.
//
// Two spec fields here that a compose v3.9 manifest cannot declare at all:
// Groups, which has no `group_add` in that schema, and the DNS options list,
// which convertDNSConfig never fills. Both are still compared, because
// `--group-add` and `--dns-option-add` can put them there and the desired side
// being empty is exactly what makes that visible.
func compareContainer(want, got swarm.ContainerSpec, add func(name, desired, live string)) {
	// Command is compose's `entrypoint` and Args is its `command`, which is the
	// naming Swarm uses and the opposite of what the field names suggest.
	compareString("command", strings.Join(want.Command, " "), strings.Join(got.Command, " "), add)
	compareString("args", strings.Join(want.Args, " "), strings.Join(got.Args, " "), add)
	compareString("user", want.User, got.User, add)
	compareString("workingDir", want.Dir, got.Dir, add)
	compareString("hostname", want.Hostname, got.Hostname, add)
	compareString("stopSignal", want.StopSignal, got.StopSignal, add)
	compareDuration("stopGracePeriod", orZeroDuration(want.StopGracePeriod), orZeroDuration(got.StopGracePeriod), add)
	compareBool("readOnly", want.ReadOnly, got.ReadOnly, add)
	compareBool("tty", want.TTY, got.TTY, add)
	compareOptionalBool("init", want.Init, got.Init, add)

	compareStringSet("hosts", want.Hosts, got.Hosts, add)
	compareStringSet("groups", want.Groups, got.Groups, add)
	compareStringSet("capabilityAdd", want.CapabilityAdd, got.CapabilityAdd, add)
	compareStringSet("capabilityDrop", want.CapabilityDrop, got.CapabilityDrop, add)

	compareKeyed("sysctls", want.Sysctls, got.Sysctls, add)
	compareUlimits(want.Ulimits, got.Ulimits, add)
	compareDNS(want.DNSConfig, got.DNSConfig, add)

	compareBool("openStdin", want.OpenStdin, got.OpenStdin, add)
	compareInt64("oomScoreAdj", want.OomScoreAdj, got.OomScoreAdj, add)
	compareString("isolation", isolationOf(want.Isolation), isolationOf(got.Isolation), add)
	// The container's own labels, which are not the service's: `--label-add`
	// writes one set and `--container-label-add` the other, and only the second
	// reaches the running container. AddStackLabel stamps the namespace onto
	// both, so the same exclusion applies.
	compareKeyed("containerLabels", ownLabels(want.Labels), ownLabels(got.Labels), add)
	comparePrivileges(want.Privileges, got.Privileges, add)
}

// isolationOf flattens the empty isolation a manifest leaves to the "default" the
// daemon reads back: isolationToGRPC maps anything that is neither hyperv nor
// process onto the default enum, and IsolationFromGRPC names it.
func isolationOf(i container.Isolation) string {
	if i == "" {
		return string(container.IsolationDefault)
	}
	return string(i)
}

// comparePrivileges reports the container's security options.
//
// Compose sets a Privileges struct on every service it converts — always
// non-nil, usually holding nothing but a nil credential spec — and
// containerToGRPC stores one whenever it is given one, so in practice both sides
// have one. Presence is still checked, for a spec that came from somewhere else.
//
// Only the credential spec has a `docker service update` flag; the rest can be
// reached through the API alone. That is the reason to report them rather than a
// reason not to: a no-new-privileges flag cleared behind the controller's back is
// a security change that nothing else here would show.
func comparePrivileges(want, got *swarm.Privileges, add func(name, desired, live string)) {
	if want == nil || got == nil {
		if want != got {
			add("privileges", presence(want != nil), presence(got != nil))
		}
		return
	}
	compareString("privileges.credentialSpec",
		credentialSpecOf(want.CredentialSpec), credentialSpecOf(got.CredentialSpec), add)
	compareBool("privileges.noNewPrivileges", want.NoNewPrivileges, got.NoNewPrivileges, add)
	compareString("privileges.seccomp", seccompOf(want.Seccomp), seccompOf(got.Seccomp), add)
	compareString("privileges.apparmor", apparmorOf(want.AppArmor), apparmorOf(got.AppArmor), add)
	compareString("privileges.selinux", selinuxOf(want.SELinuxContext), selinuxOf(got.SELinuxContext), add)
}

// credentialSpecOf names which of the three sources a Windows credential spec
// comes from, since exactly one of them is ever set.
func credentialSpecOf(c *swarm.CredentialSpec) string {
	switch {
	case c == nil:
		return ""
	case c.Config != "":
		return "config=" + c.Config
	case c.File != "":
		return "file=" + c.File
	case c.Registry != "":
		return "registry=" + c.Registry
	default:
		return ""
	}
}

// seccompOf renders the mode, and says whether a custom profile is attached
// without rendering it: a profile is a JSON document, far too long for a table
// cell, and its presence is the part an operator acts on.
func seccompOf(s *swarm.SeccompOpts) string {
	if s == nil {
		return ""
	}
	if len(s.Profile) > 0 {
		return string(s.Mode) + " (custom profile)"
	}
	return string(s.Mode)
}

func apparmorOf(a *swarm.AppArmorOpts) string {
	if a == nil {
		return ""
	}
	return string(a.Mode)
}

// selinuxOf renders the label a container runs under, in the order the four
// parts are conventionally written.
func selinuxOf(s *swarm.SELinuxContext) string {
	if s == nil {
		return ""
	}
	if s.Disable {
		return "disabled"
	}
	return strings.Join([]string{s.User, s.Role, s.Type, s.Level}, ":")
}

// compareStringSet reports a list whose order carries no meaning, as a whole and
// sorted — the same shape as constraints, and for the same reason: an
// order-sensitive comparison would report drift on a reordered chart template.
func compareStringSet(name string, want, got []string, add func(name, desired, live string)) {
	w, g := slices.Sorted(slices.Values(want)), slices.Sorted(slices.Values(got))
	if !slices.Equal(w, g) {
		add(name, strings.Join(w, ", "), strings.Join(g, ", "))
	}
}

// compareUlimits reports resource limits by the name they are set under, which
// is the key `--ulimit-add` and `--ulimit-rm` use.
func compareUlimits(want, got []*container.Ulimit, add func(name, desired, live string)) {
	compareKeyed("ulimits", ulimitsByName(want), ulimitsByName(got), add)
}

func ulimitsByName(ulimits []*container.Ulimit) map[string]string {
	out := make(map[string]string, len(ulimits))
	for _, u := range ulimits {
		if u == nil {
			continue
		}
		out[u.Name] = strconv.FormatInt(u.Soft, 10) + ":" + strconv.FormatInt(u.Hard, 10)
	}
	return out
}

// compareDNS reports the resolver configuration.
//
// Presence first, like the declared structs above it: convertDNSConfig returns
// nil unless the manifest set a nameserver or a search domain, and the daemon
// returns nil for nil, so one side having none is a real difference.
func compareDNS(want, got *swarm.DNSConfig, add func(name, desired, live string)) {
	if want == nil || got == nil {
		if want != got {
			add("dns", presence(want != nil), presence(got != nil))
		}
		return
	}
	compareStringSet("dns.nameservers", want.Nameservers, got.Nameservers, add)
	compareStringSet("dns.search", want.Search, got.Search, add)
	compareStringSet("dns.options", want.Options, got.Options, add)
}

// compareLogDriver reports the service's logging driver and its options.
//
// It lives on the task rather than the container spec, which is the only reason
// it is not in compareContainer beside everything else it behaves like.
func compareLogDriver(want, got *swarm.Driver, add func(name, desired, live string)) {
	if want == nil || got == nil {
		if want != got {
			add("logDriver", presence(want != nil), presence(got != nil))
		}
		return
	}
	compareString("logDriver.name", want.Name, got.Name, add)
	compareKeyed("logDriver.options", want.Options, got.Options, add)
}

// The structs a manifest may declare or leave out entirely — healthcheck, update
// and rollback config, restart policy.
//
// All three round-trip nil as nil: updateConfigToGRPC and restartPolicyToGRPC
// return nil for nil, containerSpecFromGRPC sets a healthcheck only where the
// stored spec had one, and swarmkit fills none of them in. So one present on
// exactly one side is a real difference and is reported as such, once, rather
// than as every field inside it.
//
// The defaulting the issue expected is real but happens one level down: *inside*
// a struct the manifest declared, where an unstated enum is stored as the zero
// value and read back under its name. A manifest that set only an update
// parallelism meets a live struct that also names a failure action and an order,
// and a raw comparison would report drift on it for ever. Each of those is
// flattened below, at the field rather than the struct.

// compareHealthcheck reports a changed container healthcheck.
//
// Test is joined rather than compared element by element, because
// ["CMD-SHELL", "curl -f localhost"] is one command: a per-element report would
// say "healthcheck.test[1]" about something nobody thinks of as a list.
func compareHealthcheck(want, got *container.HealthConfig, add func(name, desired, live string)) {
	if want == nil || got == nil {
		if want != got {
			add("healthcheck", presence(want != nil), presence(got != nil))
		}
		return
	}
	compareString("healthcheck.test", strings.Join(want.Test, " "), strings.Join(got.Test, " "), add)
	compareDuration("healthcheck.interval", want.Interval, got.Interval, add)
	compareDuration("healthcheck.timeout", want.Timeout, got.Timeout, add)
	compareDuration("healthcheck.startPeriod", want.StartPeriod, got.StartPeriod, add)
	compareDuration("healthcheck.startInterval", want.StartInterval, got.StartInterval, add)
	compareInt64("healthcheck.retries", int64(want.Retries), int64(got.Retries), add)
}

// compareUpdateConfig reports a changed update or rollback policy. One function
// and two callers, because the two are the same struct governing opposite
// directions, and a second copy would be a second place to fix a rule.
func compareUpdateConfig(prefix string, want, got *swarm.UpdateConfig, add func(name, desired, live string)) {
	if want == nil || got == nil {
		if want != got {
			add(prefix, presence(want != nil), presence(got != nil))
		}
		return
	}
	compareUint64(prefix+".parallelism", want.Parallelism, got.Parallelism, add)
	compareDuration(prefix+".delay", want.Delay, got.Delay, add)
	compareString(prefix+".failureAction", failureAction(want), failureAction(got), add)
	compareString(prefix+".order", updateOrder(want), updateOrder(got), add)
	compareDuration(prefix+".monitor", want.Monitor, got.Monitor, add)
	compareFloat32(prefix+".maxFailureRatio", want.MaxFailureRatio, got.MaxFailureRatio, add)
}

// failureAction and updateOrder apply the daemon's two enum defaults. An unset
// value is stored as the same enum as the named default and read back under that
// name (daemon/cluster/convert.updateConfigToGRPC), so the manifest's silence and
// the daemon's answer have to be spelled the same way before they are compared.
func failureAction(c *swarm.UpdateConfig) string {
	if c.FailureAction == "" {
		return swarm.UpdateFailureActionPause
	}
	return c.FailureAction
}

func updateOrder(c *swarm.UpdateConfig) string {
	if c.Order == "" {
		return swarm.UpdateOrderStopFirst
	}
	return c.Order
}

// compareRestartPolicy reports a changed restart policy.
//
// Two defaults here, and the second is easy to miss. An unset condition is stored
// as the same enum as "any" and read back named — the pattern above. But
// restartPolicyFromGRPC also takes the address of the stored attempt count
// *unconditionally*, so the live side's MaxAttempts is never nil where the
// manifest left it so. Flattening both pointers to their zero is the only
// comparison that does not report drift on a policy which named a condition and
// nothing else.
func compareRestartPolicy(want, got *swarm.RestartPolicy, add func(name, desired, live string)) {
	if want == nil || got == nil {
		if want != got {
			add("restartPolicy", presence(want != nil), presence(got != nil))
		}
		return
	}
	compareString("restartPolicy.condition", restartCondition(want), restartCondition(got), add)
	compareDuration("restartPolicy.delay", orZeroDuration(want.Delay), orZeroDuration(got.Delay), add)
	compareDuration("restartPolicy.window", orZeroDuration(want.Window), orZeroDuration(got.Window), add)
	compareUint64("restartPolicy.maxAttempts", orZeroUint64(want.MaxAttempts), orZeroUint64(got.MaxAttempts), add)
}

func restartCondition(p *swarm.RestartPolicy) string {
	if p.Condition == "" {
		return string(swarm.RestartPolicyConditionAny)
	}
	return string(p.Condition)
}

// comparePlacement reports the placement rules beside the constraints: the
// spread preferences and the per-node replica cap.
//
// Preferences are compared **in order** and never sorted, which is the one place
// this file must not copy compareConstraints. Swarmkit applies them in sequence —
// the first spreads, the next breaks its ties — so a reordered list is a different
// scheduling rule rather than the same one written differently.
//
// A nil Placement flattens to nothing on either side, which both sides need:
// compose always builds one, with an empty preference slice, and the daemon
// returns nil for a spec that placed nothing.
func comparePlacement(want, got *swarm.Placement, add func(name, desired, live string)) {
	if w, g := spreadPreferences(want), spreadPreferences(got); !slices.Equal(w, g) {
		add("placement.preferences", strings.Join(w, ", "), strings.Join(g, ", "))
	}
	compareUint64("placement.maxReplicas", maxReplicas(want), maxReplicas(got), add)
}

// spreadPreferences names each preference by what it spreads over. A preference
// that is not a spread is skipped rather than rendered blank: swarmkit has only
// the one kind today, and placementFromGRPC drops anything else on the way back,
// so a blank would be a difference against something unreadable.
func spreadPreferences(p *swarm.Placement) []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Preferences))
	for _, pref := range p.Preferences {
		if pref.Spread == nil {
			continue
		}
		out = append(out, pref.Spread.SpreadDescriptor)
	}
	return out
}

func maxReplicas(p *swarm.Placement) uint64 {
	if p == nil {
		return 0
	}
	return p.MaxReplicas
}

func orZeroDuration(d *time.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return *d
}

func orZeroUint64(v *uint64) uint64 {
	if v == nil {
		return 0
	}
	return *v
}

// Renderings for a mount whose source Swarm supplies rather than the manifest,
// and for one that cannot be written to.
const (
	anonymousSource = "(anonymous)"
	readOnlySuffix  = " (read-only)"
)

// compareMounts reports mounts added, removed and changed, keyed by the path they
// mount at.
//
// The target is a real key, unlike a published port's: a container cannot mount
// two things at one path. So a mount whose source changed reads as one line
// rather than as one gone and one arrived.
//
// Only the identity is compared — type, source, target and read-only — never the
// options inside, and the first of the three reasons is decisive. They do not
// round-trip: containerToGRPC drops a mount's whole BindOptions when its
// propagation is empty, and Consistency has no field in swarmapi.Mount at all, so
// comparing either would report drift on a stack nobody has touched. Nor is one
// independently mutable — changing a propagation means replacing the mount, which
// this catches through its identity. And a chart that changes one has changed its
// manifest, which the sync axis already reports as an upgrade.
func compareMounts(want, got []mount.Mount, add func(name, desired, live string)) {
	wm, gm := mountsByTarget(want), mountsByTarget(got)
	for _, target := range unionKeys(wm, gm) {
		wv, inWant := wm[target]
		gv, inGot := gm[target]
		if inWant == inGot && wv == gv {
			continue
		}
		add("mounts["+target+"]", orAbsent(wv, inWant), orAbsent(gv, inGot))
	}
}

// mountsByTarget renders each mount as what it mounts and how, keyed by where.
//
// An empty type is a bind, which is the zero of swarmkit's enum and so what the
// daemon reads back for a mount that named none.
func mountsByTarget(mounts []mount.Mount) map[string]string {
	out := make(map[string]string, len(mounts))
	for _, m := range mounts {
		kind := string(m.Type)
		if kind == "" {
			kind = string(mount.TypeBind)
		}
		source := m.Source
		if source == "" {
			// An anonymous volume: Swarm names one per task, and the manifest
			// named nothing for it to be compared against.
			source = anonymousSource
		}
		rendered := kind + ":" + source
		if m.ReadOnly {
			rendered += readOnlySuffix
		}
		out[m.Target] = rendered
	}
	return out
}

// runtimeTarget renders a reference the container runtime consumes rather than
// mounts, which for Swarm means a Windows credential spec.
const runtimeTarget = "(runtime)"

// compareSecretRefs and compareConfigRefs report references added, removed and
// remounted, keyed by the name of the secret or config.
//
// Keyed by name and valued by where it lands, because that pair is what
// `--secret-add` and `--secret-rm` change. The value is a list because the same
// secret may legally be mounted at two paths, and two entries under one key would
// otherwise silently lose one of them.
//
// The ids are not compared, although both sides carry a real one. A config's
// content is hashed into its name, so a change to the content is a different name
// and already a manifest-level difference; comparing ids would add nothing and
// would report drift on a resource recreated with identical content.
//
// File.{UID,GID,Mode} are not compared either — and for once that is not because
// they fail to round-trip. convertFileObject fills all three in before the spec
// exists ("0", "0", 0444), so they are never the daemon's default meeting the
// manifest's silence. They are out because `docker service update` cannot change
// one without removing and re-adding the reference, which this already sees.
func compareSecretRefs(want, got []*swarm.SecretReference, add func(name, desired, live string)) {
	compareKeyed("secrets", secretTargets(want), secretTargets(got), add)
}

func compareConfigRefs(want, got []*swarm.ConfigReference, add func(name, desired, live string)) {
	compareKeyed("configs", configTargets(want), configTargets(got), add)
}

// compareKeyed reports the entries of two string maps that differ, showing both
// values. It is the shape labels, sysctls, references and log driver options all
// have: a name an operator chose and a value they can read.
func compareKeyed(prefix string, want, got map[string]string, add func(name, desired, live string)) {
	for _, name := range unionKeys(want, got) {
		wv, inWant := want[name]
		gv, inGot := got[name]
		if inWant == inGot && wv == gv {
			continue
		}
		add(prefix+"["+name+"]", orAbsent(wv, inWant), orAbsent(gv, inGot))
	}
}

func secretTargets(refs []*swarm.SecretReference) map[string]string {
	byName := make(map[string][]string, len(refs))
	for _, r := range refs {
		// A secret with no file target is not something Swarm keeps: the daemon
		// drops one on the way back (secretReferencesFromGRPC), so comparing it
		// would report a difference against something that cannot exist.
		if r == nil || r.File == nil {
			continue
		}
		byName[r.SecretName] = append(byName[r.SecretName], r.File.Name)
	}
	return joinTargets(byName)
}

func configTargets(refs []*swarm.ConfigReference) map[string]string {
	byName := make(map[string][]string, len(refs))
	for _, r := range refs {
		if r == nil {
			continue
		}
		switch {
		case r.Runtime != nil:
			byName[r.ConfigName] = append(byName[r.ConfigName], runtimeTarget)
		case r.File != nil:
			byName[r.ConfigName] = append(byName[r.ConfigName], r.File.Name)
		}
	}
	return joinTargets(byName)
}

// joinTargets renders one name's targets as a single value, sorted so that a
// reference mounted twice reads the same on every tick.
func joinTargets(byName map[string][]string) map[string]string {
	out := make(map[string]string, len(byName))
	for name, targets := range byName {
		slices.Sort(targets)
		out[name] = strings.Join(targets, ", ")
	}
	return out
}

// declaredNetworkNames is the desired side's attachments, whose targets are
// already names: convert scopes the name the manifest used, or takes the explicit
// `name:`, and never asks a daemon for anything.
//
// Sorted, like constraints, because neither side's order is meaningful and an
// order-sensitive comparison would report drift on a reordered chart template.
func declaredNetworkNames(attachments []swarm.NetworkAttachmentConfig) []string {
	out := make([]string, 0, len(attachments))
	for _, a := range attachments {
		out = append(out, a.Target)
	}
	slices.Sort(out)
	return out
}

// resolvedNetworkNames names the live side's attachments, and says whether every
// one of them could be named. One that could not makes the whole set unusable:
// a comparison missing an attachment reports a detachment that did not happen.
func resolvedNetworkNames(attachments []swarm.NetworkAttachmentConfig, nets NetworkNames) ([]string, bool) {
	out := make([]string, 0, len(attachments))
	for _, a := range attachments {
		name, ok := nets[a.Target]
		if !ok {
			return nil, false
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, true
}

// unionKeys returns every key of both maps, sorted, so that a service's reported
// differences are in the same order on every tick. An unstable order would make
// a status poll look like a change.
//
// Generic in the value, so that the sets comparePresence works over — where the
// key is the whole of the entry and there is no value — use the same ordering as
// the keyed maps beside them.
func unionKeys[V any](a, b map[string]V) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, m := range []map[string]V{a, b} {
		for k := range m {
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// containerSpec flattens the pointer, so every caller reads a zero spec rather
// than checking for nil. A service with no container spec runs a different
// runtime and has none of the fields compared here.
func containerSpec(s swarm.ServiceSpec) swarm.ContainerSpec {
	if s.TaskTemplate.ContainerSpec == nil {
		return swarm.ContainerSpec{}
	}
	return *s.TaskTemplate.ContainerSpec
}

// Application rolls each release's live drift up to one state for the whole
// application, or nil when no release was compared — which is every application
// running the default manifest mode.
//
// Detected outranks Unknown deliberately, matching how health ranks Degraded
// over Missing: a difference that was found is something to act on now, whereas
// one that could not be read is a gap in the reporting. Both are said; only one
// leads.
func Application(releases []application.ReleaseStatus) *application.Drift {
	out := &application.Drift{State: application.DriftStateNone}
	compared := false

	for _, rel := range releases {
		if rel.Drift == nil {
			continue
		}
		compared = true
		switch rel.Drift.State {
		case application.DriftStateDetected:
			out.Services += len(rel.Drift.Services)
			out.Resources += len(rel.Drift.Resources)
			if out.State != application.DriftStateDetected {
				out.State = application.DriftStateDetected
				out.Message = detectedMessage(rel.Name, rel.Drift)
			}
		case application.DriftStateUnknown:
			if out.State == application.DriftStateNone {
				out.State = application.DriftStateUnknown
				out.Message = fmt.Sprintf("release %q: %s", rel.Name, rel.Drift.Message)
			}
		}
	}

	if !compared {
		return nil
	}
	return out
}

// detectedMessage says what was found in the worst release.
//
// A release with no departed resources reads exactly as it did before the sweep
// learned about them, so an application that has not enabled pruneResources sees
// no change at all. One with nothing but departed resources must not say "0
// service(s) do not match", which is both false and the opposite of the point.
func detectedMessage(release string, d *application.ReleaseDrift) string {
	switch {
	case len(d.Resources) == 0:
		return fmt.Sprintf("release %q: %d service(s) do not match the repository", release, len(d.Services))
	case len(d.Services) == 0:
		return fmt.Sprintf("release %q: %d resource(s) the repository no longer declares", release, len(d.Resources))
	default:
		return fmt.Sprintf("release %q: %d service(s) do not match the repository and %d resource(s) it no longer declares",
			release, len(d.Services), len(d.Resources))
	}
}
