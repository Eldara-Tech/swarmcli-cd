// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/Eldara-Tech/swarmcli/v2/charts"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
)

// logBackend is a backend that can tail a service — the capability the OSS
// applier implements and a Phase 3 remote one may not.
type logBackend struct {
	charts.Backend
	err error

	got capability.ServiceLogRequest
}

func (l *logBackend) ServiceLogs(_ context.Context, req capability.ServiceLogRequest) (<-chan application.ServiceLogEvent, error) {
	l.got = req
	if l.err != nil {
		return nil, l.err
	}
	ch := make(chan application.ServiceLogEvent)
	close(ch)
	return ch, nil
}

// running gives an application a status naming the services it is reported to
// run, which is the set a log request is resolved against.
func running(t *testing.T, r *Reconciler, app string, services ...string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.apps[app]
	if !ok {
		t.Fatalf("no entry for %q", app)
	}
	out := make([]application.ServiceStatus, 0, len(services))
	for _, s := range services {
		out = append(out, application.ServiceStatus{Name: s})
	}
	e.status.Releases = []application.ReleaseStatus{{Name: "rel", Services: out}}
}

func TestServiceLogsTailsWhatTheApplicationRuns(t *testing.T) {
	b := &logBackend{}
	r := newTestWith(t, []application.Spec{spec("edge", true)}, &fakeEngine{}, nil, fakeRegistry{backend: b})
	running(t, r, "edge", "edge_web")

	ch, err := r.ServiceLogs(t.Context(), "edge", "edge_web", 100, true)
	if err != nil {
		t.Fatalf("ServiceLogs = %v, want nil", err)
	}
	if ch == nil {
		t.Fatal("no channel")
	}
	if b.got.Service != "edge_web" || b.got.Tail != 100 || !b.got.Follow {
		t.Errorf("backend asked for %+v", b.got)
	}
}

// The API resolves the service against the same set before it ever reaches
// here, and this is deliberately the second copy of that check. The rule — a
// caller authorised for one application may name that application's services
// and no others — is a property of the reconciler, and stating it only in one
// HTTP handler would make it a property of that handler.
func TestServiceLogsRefuseAServiceTheApplicationDoesNotRun(t *testing.T) {
	b := &logBackend{}
	r := newTestWith(t, []application.Spec{spec("edge", true)}, &fakeEngine{}, nil, fakeRegistry{backend: b})
	running(t, r, "edge", "edge_web")

	if _, err := r.ServiceLogs(t.Context(), "edge", "other_db", 100, true); err == nil {
		t.Fatal("a service the application does not run was tailed anyway")
	}
	if b.got.Service != "" {
		t.Errorf("the backend was asked for %q", b.got.Service)
	}
}

func TestServiceLogsForAnUnknownApplicationFail(t *testing.T) {
	r := newTestWith(t, nil, &fakeEngine{}, nil, fakeRegistry{backend: &logBackend{}})

	if _, err := r.ServiceLogs(t.Context(), "nope", "svc", 100, true); err == nil {
		t.Fatal("an application that does not exist produced a stream")
	}
}

// Implementing the method is not the same as being able to answer: which
// backend serves a destination is settled per request. A backend with no reader
// is reported as a destination that cannot answer, which the API renders as the
// same 501 a reconciler with no method at all gives.
func TestServiceLogsReportABackendWithoutAReaderAsUnsupported(t *testing.T) {
	r := newTestWith(t, []application.Spec{spec("edge", true)}, &fakeEngine{}, nil, fakeRegistry{})
	running(t, r, "edge", "edge_web")

	_, err := r.ServiceLogs(t.Context(), "edge", "edge_web", 100, true)
	if !errors.Is(err, application.ErrUnsupported) {
		t.Fatalf("ServiceLogs = %v, want application.ErrUnsupported", err)
	}
}

// A failed open must not arrive as the sentinel, or a daemon that blinked is
// reported for ever as a build that cannot stream.
func TestServiceLogsKeepAFailedOpenDistinctFromAnUnsupportedOne(t *testing.T) {
	boom := errors.New("daemon unreachable")
	r := newTestWith(t, []application.Spec{spec("edge", true)}, &fakeEngine{}, nil, fakeRegistry{backend: &logBackend{err: boom}})
	running(t, r, "edge", "edge_web")

	_, err := r.ServiceLogs(t.Context(), "edge", "edge_web", 100, true)
	if !errors.Is(err, boom) {
		t.Fatalf("ServiceLogs = %v, want the daemon's error", err)
	}
	if errors.Is(err, application.ErrUnsupported) {
		t.Error("a failed open was reported as an unsupported destination")
	}
}
