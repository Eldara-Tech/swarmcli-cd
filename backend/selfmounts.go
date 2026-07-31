// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/swarm"
)

// serviceIDLabel is what Swarm stamps on every task container it starts. Its
// presence is how a process tells "I am a service task" from "I am a container
// somebody ran by hand", and its value is what makes the service spec reachable.
const serviceIDLabel = "com.docker.swarm.service.id"

// selfMounts is what Swarm has mounted into this controller, by the names a
// reference resolves by, and the stack it mounted them for.
type selfMounts struct {
	// namespace is the stack this controller was deployed as — the
	// com.docker.stack.namespace label on its own service, which for the shipped
	// stack.yml is "swarmcli-cd". Empty for a controller that is not a swarm
	// service.
	//
	// It is not a mount, and it is here because it comes off the same read: the
	// service spec below already has to be fetched for the configs, and a release
	// name is compared against this on every deploy and every removal. A second
	// round trip for a label sitting beside one this already reads would be two
	// caches to keep honest instead of one (#102).
	namespace string
	secrets   map[string]struct{}
	configs   map[string]struct{}
}

// selfMountCache holds that answer for the life of the process.
//
// It is behind a pointer because every With* method copies the Backend, and a
// per-copy cache would re-read once per application. A mutex rather than a
// sync.Once so that a failure is not cached: a daemon that was briefly
// unreachable would otherwise poison the guard for the life of the controller,
// and the failure mode of this particular cache is "refuse every deploy".
//
// That only holds while a read that never reached the daemon comes back as an
// error, which is an invariant of readSelfMounts rather than of the mutex: only a
// nil error sets loaded. Reporting an unreachable daemon as an empty set instead
// would be worse than the sync.Once this avoids, because an empty set is a valid
// answer and therefore caches — leaving the guard off silently, permanently, and
// with a healthy daemon, rather than merely stuck refusing.
type selfMountCache struct {
	mu     sync.Mutex
	loaded bool
	val    selfMounts
}

// mounts names the secrets and configs Swarm has mounted into this controller,
// and the stack namespace it deployed it under, reading its own service spec the
// first time it is asked.
//
// # Why the daemon rather than the filesystem
//
// A secret arrives at /run/secrets/<name>, so listing that directory very nearly
// answers the question for secrets — and MountedSecretNames does exactly that.
// But the name it yields is the mount *target*, and compose's long form can
// rename it: a controller declaring `{source: swarmcli-cd-token, target: token}`
// derives "token", never matches the `external` reference a tenant stack would
// actually make against "swarmcli-cd-token", and therefore has no guard at all
// while appearing to have one.
//
// A config has no equivalent path to read. It lands wherever `target:` puts it —
// /etc/swarmcli-cd/applications.yaml in the shipped stack.yml — and nothing on
// the filesystem ties that path back to the config's name.
//
// The service spec carries both, under the names a reference resolves by, which
// is the only form worth comparing.
//
// # Why at deploy time and not at startup
//
// Reading this once at startup would make the controller's boot depend on a
// daemon round trip, which contradicts the rule the rest of the startup path
// follows — an app set that cannot be read logs and retries rather than exiting,
// because exiting restart-loops the controller and takes down the API that could
// have explained it.
//
// It would also leave a hole. A daemon hiccup at boot would produce an empty set
// and a controller that then deployed every stack unguarded until somebody
// restarted it. Resolved here, a failure fails the deploy that needed it and
// nothing else, and the next deploy tries again.
//
// # When it answers nothing
//
// A controller that is not a swarm service — a `go run` during development, a
// plain `docker run` — has nothing mounted by Swarm and no stack of its own, and
// gets empty sets, an empty namespace and no error. That is the true answer
// rather than a failure, and it is cached as one: both guards built on it go
// quiet, because there is nothing there for a release to claim.
//
// A daemon that could not be asked is not that answer. It arrives here as an
// error, is not cached, and fails the deploy that needed it — the two are easy to
// conflate because neither leaves anything to report, and readSelfMounts is where
// they are kept apart.
func (b *Backend) mounts(ctx context.Context) (selfMounts, error) {
	b.self.mu.Lock()
	defer b.self.mu.Unlock()
	if b.self.loaded {
		return b.self.val, nil
	}

	val, err := b.readSelfMounts(ctx)
	if err != nil {
		return selfMounts{}, err
	}
	b.self.val, b.self.loaded = val, true
	return val, nil
}

func (b *Backend) readSelfMounts(ctx context.Context) (selfMounts, error) {
	// In a container the hostname is the container id unless somebody overrode
	// it, which is what makes the rest of this resolvable from the inside.
	host, err := os.Hostname()
	if err != nil {
		return selfMounts{}, fmt.Errorf("reading this container's hostname: %w", err)
	}

	self, err := b.api.ContainerInspect(ctx, host)
	// Not a container this daemon knows: this is not a running swarm task and
	// there is nothing mounted to protect. A controller that really is one cannot
	// reach this — the daemon it is asking is the one that started it — so it is a
	// `go run` during development or a plain `docker run`, and an answer.
	if errdefs.IsNotFound(err) {
		return selfMounts{}, nil
	}
	// Anything else is the daemon not having been asked rather than an answer, and
	// the difference is the whole guard. A connection failure in particular used to
	// land in the branch above, on the reasoning that a real controller could not
	// reach it either — but that only holds for a daemon that is permanently
	// absent. A dockerd restart, or a socket unavailable for a few hundred
	// milliseconds, produces exactly that error while this container keeps running
	// with the controller's own secrets and configs still mounted. Called an empty
	// set it would have cached, and from then on every stack could mount the
	// application set and any secret whose target was renamed (#101).
	if err != nil {
		return selfMounts{}, fmt.Errorf("inspecting this container: %w", err)
	}
	if self.Config == nil || self.Config.Labels[serviceIDLabel] == "" {
		return selfMounts{}, nil
	}

	svc, _, err := b.api.ServiceInspectWithRaw(ctx, self.Config.Labels[serviceIDLabel], swarm.ServiceInspectOptions{})
	if errdefs.IsNotFound(err) {
		// The task outlived its service, which is a teardown rather than a
		// misconfiguration. Nothing to protect and nothing to complain about.
		return selfMounts{}, nil
	}
	// The same split as above, and it is a second round trip: the daemon that
	// answered the first read can be gone by this one.
	if err != nil {
		return selfMounts{}, fmt.Errorf("inspecting this controller's own service: %w", err)
	}

	// Read before the container spec is looked at, and kept whatever that turns
	// out to be: the namespace is a label on the service, so a controller running
	// under a runtime this applier does not model still gets the name that must
	// not be deployed or removed.
	out := selfMounts{namespace: svc.Spec.Labels[convert.LabelNamespace]}

	cs := svc.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return out, nil
	}
	out.secrets = make(map[string]struct{}, len(cs.Secrets))
	out.configs = make(map[string]struct{}, len(cs.Configs))
	for _, ref := range cs.Secrets {
		out.secrets[ref.SecretName] = struct{}{}
	}
	for _, ref := range cs.Configs {
		out.configs[ref.ConfigName] = struct{}{}
	}
	return out, nil
}
