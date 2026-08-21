// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package backend

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	dockerclient "github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/capability"
)

// Self-management against a real daemon.
//
// Everything the unit tests above fake, this asks Swarm. It is here rather than
// in integration-tests/ for one reason: the identity read starts at the
// hostname, so a test can only be this controller's own stack if it can redirect
// that — and the seam must stay unexported, because what it answers decides
// which release is exempt from every guard the others are held to. A package
// nobody can import cannot leak it.
//
// The stand-in is a service, not this binary. What that leaves untested is that
// the replacement controller boots and reconciles; what it covers is every
// decision this package makes about a release that is the stack it runs as,
// taken against a daemon rather than a fake.
const (
	selfNS      = "swarmcli-cd-selftest"
	selfService = selfNS + "_controller"
	standInImg  = "busybox:1.36"
)

// selfManifestFor is the stand-in's stack as a chart would render it: the
// controller's own service, and one beside it that is not this process — which
// is what shows that only the write replacing this controller is held back.
func selfManifestFor(image, command string) string {
	return `
services:
  controller:
    image: ` + image + `
    command: ["sh", "-c", "` + command + `"]
  sidecar:
    image: ` + standInImg + `
    command: ["sh", "-c", "sleep 7200"]
`
}

func TestSelfReleaseAgainstARealSwarm(t *testing.T) {
	cli := swarmClient(t)
	ctx := t.Context()
	containerID := standInController(t, cli)

	b := New(cli, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	b.hostname = func() (string, error) { return containerID, nil }

	var deferred func(context.Context) error
	self := b.WithSelfRelease(func(apply func(context.Context) error) { deferred = apply })

	// The name is the whole claim. A release marked self and named anything else
	// deploys a second controller beside this one, so it is refused before the
	// manifest is even converted.
	t.Run("a self release named something else", func(t *testing.T) {
		err := self.DeployStack(ctx, charts.DeployRequest{
			Name: "not-this-controller", Manifest: selfManifestFor(standInImg, "sleep 7200"), Resolve: ResolveNever,
		})
		if err == nil {
			t.Fatal("DeployStack = nil, want a self release refused for not being this controller's stack")
		}
		if !strings.Contains(err.Error(), selfNS) {
			t.Errorf("error %q does not name the stack this controller runs as", err)
		}
	})

	// And the same name without the marking is the collision the guard has
	// always refused, now answered off a real service spec.
	t.Run("this stack, deployed as an ordinary release", func(t *testing.T) {
		err := b.DeployStack(ctx, charts.DeployRequest{
			Name: selfNS, Manifest: selfManifestFor(standInImg, "sleep 7200"), Resolve: ResolveNever,
		})
		if !errors.Is(err, capability.ErrOwnStack) {
			t.Fatalf("DeployStack = %v, want it refused as the controller's own stack", err)
		}
	})

	// The four losses, one of each kind that this fixture can express.
	t.Run("a manifest that drops this controller's service", func(t *testing.T) {
		err := self.DeployStack(ctx, charts.DeployRequest{
			Name: selfNS, Resolve: ResolveNever, Manifest: `
services:
  sidecar:
    image: ` + standInImg + `
    command: ["sh", "-c", "sleep 7200"]
`})
		if err == nil || !strings.Contains(err.Error(), "start a second one beside it") {
			t.Fatalf("DeployStack = %v, want the manifest refused for declaring no service under that name", err)
		}
	})

	t.Run("a manifest that takes this controller backwards", func(t *testing.T) {
		err := self.DeployStack(ctx, charts.DeployRequest{
			Name: selfNS, Manifest: selfManifestFor("busybox:1.35", "sleep 7200"), Resolve: ResolveNever,
		})
		if err == nil || !strings.Contains(err.Error(), "1.35") {
			t.Fatalf("DeployStack = %v, want the downgrade refused", err)
		}
	})

	// Removal is not exempt and must not be: deploying onto this controller's
	// services is what upgrading it is, deleting them is not.
	t.Run("removing this controller's own stack", func(t *testing.T) {
		if err := self.RemoveStack(ctx, selfNS); !errors.Is(err, capability.ErrOwnStack) {
			t.Fatalf("RemoveStack = %v, want the controller's own stack refused", err)
		}
	})

	// And the upgrade itself. The stand-in stack has no release record — it was
	// put there by hand, exactly as `docker stack deploy` leaves the real one —
	// so this is the adoption the self release is exempt for, and the write that
	// would replace this process is handed back rather than made.
	t.Run("the upgrade", func(t *testing.T) {
		err := self.DeployStack(ctx, charts.DeployRequest{
			Name: selfNS, Manifest: selfManifestFor(standInImg, "sleep 7201"), Resolve: ResolveNever,
		})
		if err != nil {
			t.Fatalf("DeployStack = %v, want the controller's own release adopted and applied", err)
		}
		if deferred == nil {
			t.Fatal("nothing was handed back; the write replacing this controller was made inside the deploy")
		}
		if _, _, err := cli.ServiceInspectWithRaw(ctx, selfNS+"_sidecar", swarm.ServiceInspectOptions{}); err != nil {
			t.Errorf("the service beside this controller was not deployed: %v", err)
		}
		if got := controllerCommand(t, cli); !strings.Contains(got, "sleep 3600") {
			t.Fatalf("this controller's own service was already written by the deploy: %q", got)
		}

		if err := deferred(ctx); err != nil {
			t.Fatalf("issuing the deferred write = %v, want it applied", err)
		}
		if got := controllerCommand(t, cli); !strings.Contains(got, "sleep 7201") {
			t.Errorf("command = %q, want the revision the deploy held back", got)
		}
	})
}

// swarmClient is a client on a swarm manager, or a skip. The suite in
// integration-tests/ has one of its own; this is a sibling rather than a shared
// helper because the two are in different modules' worth of packages and neither
// exports it.
func swarmClient(t *testing.T) *dockerclient.Client {
	t.Helper()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	info, err := cli.Info(t.Context())
	if err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	if info.Swarm.LocalNodeState != swarm.LocalNodeStateActive || !info.Swarm.ControlAvailable {
		t.Skip("daemon is not a swarm manager; run `docker swarm init` to enable the integration tests")
	}
	return cli
}

// standInController puts a stack on the swarm shaped like the controller's own —
// one service carrying the namespace label, deployed by hand and with no release
// record — and returns the id of the container running it, which is what this
// process claims as itself.
func standInController(t *testing.T, cli *dockerclient.Client) string {
	t.Helper()
	ctx := t.Context()
	removeStandIn(t, cli)
	t.Cleanup(func() { removeStandIn(t, cli) })

	replicas := uint64(1)
	created, err := cli.ServiceCreate(ctx, swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   selfService,
			Labels: map[string]string{convert.LabelNamespace: selfNS},
		},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:   standInImg,
			Command: []string{"sh", "-c", "sleep 3600"},
		}},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
	}, swarm.ServiceCreateOptions{})
	if err != nil {
		t.Fatalf("creating the stand-in controller: %v", err)
	}

	// The container id is what ties this process to that service, so the test
	// cannot start until the task has one.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		tasks, err := cli.TaskList(ctx, swarm.TaskListOptions{
			Filters: filters.NewArgs(filters.Arg("service", created.ID)),
		})
		if err != nil {
			t.Fatalf("listing the stand-in's tasks: %v", err)
		}
		for _, task := range tasks {
			if id := task.Status.ContainerStatus; id != nil && id.ContainerID != "" && task.Status.State == swarm.TaskStateRunning {
				return id.ContainerID
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in controller never started a task")
		}
		time.Sleep(time.Second)
	}
}

// removeStandIn deletes the stack by hand. RemoveStack cannot: it refuses this
// namespace, which is one of the things being tested.
func removeStandIn(t *testing.T, cli *dockerclient.Client) {
	t.Helper()
	ctx := context.Background()
	label := filters.NewArgs(filters.Arg("label", convert.LabelNamespace+"="+selfNS))

	services, err := cli.ServiceList(ctx, swarm.ServiceListOptions{Filters: label})
	if err != nil {
		t.Fatalf("listing the stand-in's services: %v", err)
	}
	for _, svc := range services {
		if err := cli.ServiceRemove(ctx, svc.ID); err != nil {
			t.Errorf("removing %s: %v", svc.Spec.Name, err)
		}
	}
	networks, err := cli.NetworkList(ctx, network.ListOptions{Filters: label})
	if err != nil {
		t.Fatalf("listing the stand-in's networks: %v", err)
	}
	// By name as well as by label. The default network a stack gets is created
	// by the converter rather than declared by the manifest, and a runner that
	// leaked one would fail the next run on a name that already exists.
	if byName, err := cli.NetworkInspect(ctx, selfNS+"_default", network.InspectOptions{}); err == nil {
		networks = append(networks, network.Summary{ID: byName.ID})
	}
	for _, nw := range networks {
		// A network with an attached task is removed on retry, once swarm has
		// finished tearing the service down.
		for range 30 {
			if err := cli.NetworkRemove(ctx, nw.ID); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
	}
}

func controllerCommand(t *testing.T, cli *dockerclient.Client) string {
	t.Helper()
	svc, _, err := cli.ServiceInspectWithRaw(t.Context(), selfService, swarm.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("inspecting the stand-in controller: %v", err)
	}
	cs := svc.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		t.Fatal("the stand-in controller has no container spec")
	}
	return strings.Join(append(append([]string{}, cs.Command...), cs.Args...), " ")
}
