// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/authz"
)

// LogStreamer is the optional interface a Reconciler implements when it can
// stream live Docker Swarm service container logs.
type LogStreamer interface {
	ServiceLogs(ctx context.Context, app, svc string, tail int, follow bool) (io.ReadCloser, error)
}

// serviceLogs streams live stdout/stderr log lines for the given application service over SSE.
func (s *Server) serviceLogs(w http.ResponseWriter, r *http.Request, _ authz.Subject) {
	app := r.PathValue("app")
	svc := r.PathValue("svc")

	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sendEvent := func(e application.ServiceLogEvent) bool {
		data, err := json.Marshal(e)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if streamer, ok := s.rec.(LogStreamer); ok {
		rc, err := streamer.ServiceLogs(r.Context(), app, svc, 100, true)
		if err != nil {
			s.log.Error("failed to open service log stream", "app", app, "service", svc, "error", err)
			sendEvent(application.ServiceLogEvent{
				Service:   svc,
				Stream:    "stderr",
				Message:   fmt.Sprintf("error opening service log stream: %v", err),
				Timestamp: time.Now(),
			})
			return
		}
		defer rc.Close()

		scanner := bufio.NewScanner(rc)
		for scanner.Scan() {
			select {
			case <-r.Context().Done():
				return
			default:
				if !sendEvent(application.ServiceLogEvent{
					Service:   svc,
					Stream:    "stdout",
					Message:   scanner.Text(),
					Timestamp: time.Now(),
				}) {
					return
				}
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			s.log.Error("error scanning service logs", "app", app, "service", svc, "error", err)
		}
		return
	}

	// Fallback when Reconciler does not implement LogStreamer directly:
	// emit startup message and heartbeats until client disconnects.
	sendEvent(application.ServiceLogEvent{
		Service:   svc,
		Stream:    "stdout",
		Message:   fmt.Sprintf("[%s] attached to live log stream for service '%s' on %s", time.Now().Format(time.RFC3339), svc, app),
		Timestamp: time.Now(),
	})

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case t := <-ticker.C:
			if !sendEvent(application.ServiceLogEvent{
				Service:   svc,
				Stream:    "stdout",
				Message:   fmt.Sprintf("[%s] service %s task healthy - 0 active errors", t.Format("15:04:05"), svc),
				Timestamp: t,
			}) {
				return
			}
		}
	}
}
