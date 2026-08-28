// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/reconcile"
)

// logChartFiles is a chart whose one service writes to both streams and then
// stays up.
//
// Both, because the whole reason the reader demultiplexes rather than reading
// the stream flat is that stdout and stderr have to arrive distinguishable —
// and a fixture that only writes to one proves nothing about that. The sleep is
// what keeps the task running long enough for a following tail to attach; the
// two lines are already in the backlog by then, which is what `tail` is for.
func logChartFiles(release string) map[string]string {
	return map[string]string{
		"swarmcli-release.yaml": "" +
			"apiVersion: v1\n" +
			"releases:\n" +
			"  - name: " + release + "\n" +
			"    chart: ./charts/app\n",
		"charts/app/Chart.yaml":  "apiVersion: v1\nname: app\nversion: 0.1.0\n",
		"charts/app/values.yaml": "{}\n",
		"charts/app/templates/stack.yaml": "" +
			"version: \"3.9\"\n" +
			"services:\n" +
			"  app:\n" +
			"    image: busybox:1.36\n" +
			"    command: [\"sh\", \"-c\", \"echo out-line; echo err-line >&2; sleep 3600\"]\n" +
			"    deploy:\n" +
			"      replicas: 1\n" +
			"      labels:\n" +
			"        com.swarmcli.release: {{ .Release.Name }}\n",
	}
}

// Tailing a real service on a real swarm.
//
// Everything the unit tests hand-build — the frames, the details prefix, the
// timestamps — is the daemon's here, which is the only place they are facts
// rather than fixtures. Three of the four assertions below cannot be made
// against a fake at all: that the daemon frames the stream the way the reader
// expects, that `details=1` really does carry the task and node of a swarm
// task, and that the instants are the container's rather than this process's.
func TestServiceLogsAgainstARealSwarm(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-logs"
	repo := gitRepo(t, logChartFiles(release))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, releaseApp("logs", repo, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rec.SyncNow(ctx, "logs"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	// The name the tail is opened with is the one the application's own status
	// reports, which is the rule the reconciler enforces and the reason a
	// subject scoped to one application cannot name another's service.
	view, ok := rec.View("logs")
	if !ok {
		t.Fatal("no view for the application after a sync")
	}
	service := onlyService(t, view)

	// Read before the tail, so "the container's instant" below is a comparison
	// against something and not against the assertion's own clock.
	opened := time.Now().UTC()

	events, err := rec.ServiceLogs(ctx, "logs", service, application.ServiceLogRequest{Tail: 100, Follow: true})
	if err != nil {
		t.Fatalf("ServiceLogs = %v, want nil", err)
	}

	byStream := map[string]application.ServiceLogEvent{}
	deadline := time.After(60 * time.Second)
	for len(byStream) < 2 {
		select {
		case e, more := <-events:
			if !more {
				t.Fatalf("the stream ended having seen %d of the 2 streams", len(byStream))
			}
			if e.Notice {
				t.Errorf("the controller dropped or truncated something: %q", e.Message)
				continue
			}
			switch strings.TrimSpace(e.Message) {
			case "out-line", "err-line":
				byStream[e.Stream] = e
			}
		case <-deadline:
			t.Fatalf("only %d of the 2 streams arrived: %+v", len(byStream), byStream)
		}
	}

	out, ok := byStream["stdout"]
	if !ok {
		t.Fatal("nothing arrived on stdout")
	}
	err2, ok := byStream["stderr"]
	if !ok {
		t.Fatal("nothing arrived on stderr; the two streams are not being told apart")
	}
	if strings.TrimSpace(out.Message) != "out-line" || strings.TrimSpace(err2.Message) != "err-line" {
		t.Errorf("messages = %q / %q, want out-line / err-line", out.Message, err2.Message)
	}

	// The daemon's details prefix is the only place a swarm task's identity
	// appears, and these two fields were declared and never populated until the
	// seam could carry them.
	for _, e := range []application.ServiceLogEvent{out, err2} {
		if e.TaskID == "" || e.NodeID == "" {
			t.Errorf("%s line has task %q and node %q; one of them is not being read",
				e.Stream, e.TaskID, e.NodeID)
		}
	}
	if out.TaskID != err2.TaskID {
		t.Errorf("one replica produced two task ids: %q and %q", out.TaskID, err2.TaskID)
	}

	// #266. Both are resolved by the reader against the live swarm — the slot
	// from the task listing and the hostname from the node roster — and neither
	// can be checked against a fake, where both maps are whatever the fixture
	// said. A single-replica service is slot 1: swarm numbers replicas from
	// one, and 0 is what it reports for a global service that has no slot.
	for _, e := range []application.ServiceLogEvent{out, err2} {
		if e.Slot != 1 {
			t.Errorf("%s line has slot %d, want 1 for the only replica of a replicated service", e.Stream, e.Slot)
		}
		if e.NodeHostname == "" {
			t.Errorf("%s line names node %q and no hostname; the roster lookup did not happen", e.Stream, e.NodeID)
		}
	}
	if hostname := nodeHostname(t, cli, out.NodeID); out.NodeHostname != hostname {
		t.Errorf("the line says it came from %q and the daemon says that node is %q",
			out.NodeHostname, hostname)
	}

	// The container's instant, not the read time. Asserted as *before* the tail
	// was opened rather than merely non-zero: a reader that had gone back to
	// stamping time.Now() would pass a non-zero check on every line.
	if !out.Timestamp.Before(opened) {
		t.Errorf("timestamp %v is not before the tail was opened at %v; this is the read time",
			out.Timestamp, opened)
	}

	// Cancelling is the whole of how a stream is stopped, so the channel has to
	// close on its own once it is.
	cancel()
	closed := time.After(30 * time.Second)
	for {
		select {
		case _, more := <-events:
			if !more {
				return
			}
		case <-closed:
			t.Fatal("the stream outlived the context it was opened with")
		}
	}
}

// A window narrower than the service is old returns less than an unbounded read
// of the same service.
//
// This is the assertion #267 exists for, and it is the one that cannot be made
// against a fake: `since` is applied by the log driver on the node holding the
// task, after the daemon has already selected the last `tail` lines, and no
// fixture can be wrong about that ordering the way a real daemon can. Asserted
// as a comparison between two reads of one service rather than as a line count,
// because how much a busybox writes is not this test's business.
func TestALogWindowReadsLessThanAnUnboundedTail(t *testing.T) {
	cli := dockerClient(t)
	const release = "e2e-logs-window"
	repo := gitRepo(t, chattyChartFiles(release))
	t.Cleanup(func() { removeStack(t, release) })

	rec := reconciler(t, releaseApp("window", repo, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rec.SyncNow(ctx, "window"); err != nil {
		t.Fatalf("SyncNow = %v, want nil", err)
	}
	waitForRunning(t, cli, release, 1)

	view, ok := rec.View("window")
	if !ok {
		t.Fatal("no view for the application after a sync")
	}
	service := onlyService(t, view)

	// The service writes its lines and then sleeps, so a few seconds is enough
	// for all of them to be in the log and none of them to be recent.
	time.Sleep(10 * time.Second)

	whole := countTail(t, ctx, rec, service, application.ServiceLogRequest{Tail: 1000})
	if whole < 2 {
		t.Fatalf("the unbounded read returned %d lines; the fixture is not writing", whole)
	}

	// One second of history, of a service that wrote everything ten seconds ago.
	narrow := countTail(t, ctx, rec, service, application.ServiceLogRequest{
		Tail:  1000,
		Since: time.Now().Add(-1 * time.Second),
	})
	if narrow >= whole {
		t.Errorf("a one-second window returned %d of the %d lines; since is not reaching the daemon",
			narrow, whole)
	}
}

// chattyChartFiles is a chart whose service writes several lines and then stays
// up, so that a window can exclude some of them.
func chattyChartFiles(release string) map[string]string {
	files := logChartFiles(release)
	files["charts/app/templates/stack.yaml"] = "" +
		"version: \"3.9\"\n" +
		"services:\n" +
		"  app:\n" +
		"    image: busybox:1.36\n" +
		"    command: [\"sh\", \"-c\", \"for i in 1 2 3 4 5; do echo line-$i; done; sleep 3600\"]\n" +
		"    deploy:\n" +
		"      replicas: 1\n" +
		"      labels:\n" +
		"        com.swarmcli.release: {{ .Release.Name }}\n"
	return files
}

// countTail reads one non-following tail to its end and counts the container's
// own lines, ignoring anything the controller said.
func countTail(t *testing.T, ctx context.Context, rec *reconcile.Reconciler, service string, req application.ServiceLogRequest) int {
	t.Helper()
	read, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	events, err := rec.ServiceLogs(read, "window", service, req)
	if err != nil {
		t.Fatalf("ServiceLogs = %v, want nil", err)
	}
	n := 0
	for e := range events {
		if !e.Notice && strings.HasPrefix(strings.TrimSpace(e.Message), "line-") {
			n++
		}
	}
	return n
}

// nodeHostname asks the daemon what it calls a node, so the label on a line is
// compared against the swarm rather than against itself.
func nodeHostname(t *testing.T, cli client.APIClient, id string) string {
	t.Helper()
	node, _, err := cli.NodeInspectWithRaw(context.Background(), id)
	if err != nil {
		t.Fatalf("inspecting node %s: %v", id, err)
	}
	return node.Description.Hostname
}

// onlyService returns the single service the application reports, failing if
// there is not exactly one — which would mean the fixture, not the reader,
// changed.
func onlyService(t *testing.T, view application.View) string {
	t.Helper()
	var names []string
	for _, release := range view.Status.Releases {
		for _, svc := range release.Services {
			names = append(names, svc.Name)
		}
	}
	if len(names) != 1 {
		t.Fatalf("the application reports %v, want exactly one service", names)
	}
	return names[0]
}
