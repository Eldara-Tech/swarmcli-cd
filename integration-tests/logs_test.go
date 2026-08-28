// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
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

	events, err := rec.ServiceLogs(ctx, "logs", service, 100, true)
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
