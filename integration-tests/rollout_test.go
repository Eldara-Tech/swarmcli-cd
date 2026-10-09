// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	dockerclient "github.com/docker/docker/client"
)

// sequentialChartFiles is a chart with two services labelled for a sequential
// rollout and one that is not, each carrying rev in its environment so that a
// new revision changes all three.
func sequentialChartFiles(release string, rev int) map[string]string {
	files := chartFiles(release, 1)
	files["charts/app/values.yaml"] = "rev: " + itoa(rev) + "\n"
	service := func(name, label string) string {
		return "" +
			"  " + name + ":\n" +
			"    image: busybox:1.36\n" +
			"    command: [\"sleep\", \"3600\"]\n" +
			"    environment:\n" +
			"      REV: \"{{ .Values.rev }}\"\n" +
			"    deploy:\n" +
			"      update_config:\n" +
			"        monitor: 5s\n" +
			"        order: stop-first\n" +
			"      labels:\n" +
			"        com.swarmcli.release: {{ .Release.Name }}\n" + label
	}
	marked := "        com.swarmcli.rollout: sequential\n"
	files["charts/app/templates/stack.yaml"] = "version: \"3.9\"\nservices:\n" +
		service("a", marked) + service("b", marked) + service("plain", "")
	return files
}

// revTaskCreated is when swarm created the service's task carrying REV=rev.
// Not the service's UpdateStatus: each later stage of the rollout re-sends the
// services already rolled, unchanged, which clears theirs.
func revTaskCreated(t *testing.T, cli *dockerclient.Client, service string, rev int) time.Time {
	t.Helper()
	tasks, err := cli.TaskList(context.Background(), swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", service)),
	})
	if err != nil {
		t.Fatalf("listing the tasks of %q: %v", service, err)
	}
	var created time.Time
	for _, task := range tasks {
		for _, e := range task.Spec.ContainerSpec.Env {
			if e == "REV="+itoa(rev) && task.CreatedAt.After(created) {
				created = task.CreatedAt
			}
		}
	}
	if created.IsZero() {
		t.Fatalf("service %q never ran a task at REV=%d", service, rev)
	}
	return created
}

// Services labelled com.swarmcli.rollout: sequential update one at a time
// through this controller's backend, as they do through docker stack deploy:
// b's update starts only once a's new task has outlived its 5s monitor window,
// and the unmarked service goes first. The chart engine rolls them out like this
// only for a backend asserting charts.OmittedServicesPreserver; without it every
// service updates at once, which is what this catches.
func TestSequentialServicesRollOneAtATime(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-seqroll"
	repo := gitRepo(t, sequentialChartFiles(release, 1))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, releaseApp("seqroll", repo, true))
	ctx := context.Background()
	if err := rec.SyncNow(ctx, "seqroll"); err != nil {
		t.Fatalf("initial SyncNow = %v, want nil", err)
	}
	commitChange(t, repo, sequentialChartFiles(release, 2))
	if err := rec.SyncNow(ctx, "seqroll"); err != nil {
		t.Fatalf("SyncNow of the new revision = %v, want nil", err)
	}

	for _, name := range []string{"a", "b", "plain"} {
		env := serviceOf(t, cli, release+"_"+name).Spec.TaskTemplate.ContainerSpec.Env
		found := false
		for _, e := range env {
			found = found || e == "REV=2"
		}
		if !found {
			t.Fatalf("service %s was not updated to REV=2 (env %v)", name, env)
		}
	}
	b := serviceOf(t, cli, release+"_b")
	if b.UpdateStatus == nil || b.UpdateStatus.StartedAt == nil {
		t.Fatal("b has no update status; its update was never started")
	}
	aNew := revTaskCreated(t, cli, release+"_a", 2)
	if b.UpdateStatus.StartedAt.Before(aNew.Add(5 * time.Second)) {
		t.Errorf("b's update started at %s, before a's new task (created %s) had outlived its monitor window",
			b.UpdateStatus.StartedAt, aNew)
	}
	if plain := revTaskCreated(t, cli, release+"_plain", 2); !plain.Before(aNew) {
		t.Errorf("the unmarked service's new task (%s) was not created before a's (%s); it belongs in the first deploy", plain, aNew)
	}
}
