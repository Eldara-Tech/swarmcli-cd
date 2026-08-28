// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package backend

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"

	"github.com/Eldara-Tech/swarmcli-cd/application"
	"github.com/Eldara-Tech/swarmcli-cd/capability"
)

// The daemon's multiplexed framing: an 8-byte header whose first byte names the
// stream and whose last four are a big-endian payload length.
//
// Named here rather than taken from pkg/stdcopy, whose constants are unexported
// apart from the stream types. stdcopy.StdCopy itself is not what this needs:
// it writes stdout and stderr into two separate writers, which loses the
// interleaving that makes a log readable, and reaching a channel through it
// would cost two pipes and two more goroutines.
const (
	logHeaderLen  = 8
	logStreamByte = 0
	logSizeOffset = 4
)

// The stream each frame belongs to, as the daemon's header names it.
const (
	frameStdout    = 1
	frameStderr    = 2
	frameSystemerr = 3
)

// logChunk is how much of one frame is read at a time.
//
// Frames are not read whole. A frame's declared length is the daemon's number,
// and allocating it would let one line decide how much memory this process
// takes; the sink is fed in pieces instead and caps the line itself.
const logChunk = 32 * 1024

// maxLogLine caps one log line.
//
// A container is entitled to emit a line longer than any buffer — a stack
// trace, a serialised request, one line of JSON — and this used to live in the
// API, where bufio's 64KB default made Scan return false on one and ended the
// whole stream silently. Capped rather than unbounded because the producer is
// not ours; past it the line is delivered truncated and marked, which is a
// worse line but not a lost stream.
const maxLogLine = 1 << 20

// logBuffer is how many events a client may fall behind by before lines start
// being dropped.
//
// Dropping is the correct failure, for the reason api/stream.go's
// subscriberBuffer gives: a container in a crash loop outruns a browser, and
// the alternative to dropping is buffering the difference for as long as the
// tab stays open. Larger than that one because these are lines rather than
// reconcile events, and a burst of a few hundred is ordinary.
const logBuffer = 256

// The two attributes kept out of the daemon's details prefix.
//
// Everything else in it is discarded rather than folded into the message.
// `details=1` prefixes each line with every attribute the message carries,
// which includes whatever the operator configured through `--log-opt labels=`
// and `--log-opt env=` — that is, selected environment variable *values* — and
// the console must show the text the container wrote and not that.
const (
	attrNodeID = "com.docker.swarm.node.id"
	attrTaskID = "com.docker.swarm.task.id"
)

// ServiceLogs tails one service's container output.
//
// Timestamps and details are always requested. The first is what makes a
// hundred lines of backlog a hundred instants rather than one; the second is
// the only place the daemon states which task and which node a line came from,
// which for a replicated service is the difference between "one node is
// crash-looping" and "this service is broken".
func (b *Backend) ServiceLogs(ctx context.Context, req capability.ServiceLogRequest) (<-chan application.ServiceLogEvent, error) {
	if req.Service == "" {
		return nil, errors.New("no service was named")
	}

	// Before the stream, because it decides how to read it. A service whose
	// tasks have a TTY is served unframed — the daemon multiplexes only when no
	// selected task has one — and reading that as frames yields nothing but
	// header errors.
	tty, err := b.serviceHasTTY(ctx, req.Service)
	if err != nil {
		return nil, err
	}

	rc, err := b.api.ServiceLogs(ctx, req.Service, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Details:    true,
		Follow:     req.Follow,
		Tail:       strconv.Itoa(req.Tail),
	})
	if err != nil {
		return nil, fmt.Errorf("opening the log stream for service '%s': %w", req.Service, err)
	}

	out := make(chan application.ServiceLogEvent, logBuffer)
	go b.pumpLogs(ctx, rc, req.Service, tty, out)
	return out, nil
}

// serviceHasTTY reports whether the service's tasks are given a terminal.
func (b *Backend) serviceHasTTY(ctx context.Context, service string) (bool, error) {
	svc, _, err := b.api.ServiceInspectWithRaw(ctx, service, swarm.ServiceInspectOptions{})
	if err != nil {
		return false, fmt.Errorf("inspecting service '%s': %w", service, err)
	}
	spec := svc.Spec.TaskTemplate.ContainerSpec
	return spec != nil && spec.TTY, nil
}

// pumpLogs owns the response body and the channel for the life of the stream.
//
// Both are released here and nowhere else, on every exit path, which is what
// makes cancelling the caller's context sufficient to end the stream: the
// daemon request is made with that context, so a cancel fails the read, which
// ends the loop, which closes both.
func (b *Backend) pumpLogs(ctx context.Context, rc io.ReadCloser, service string, tty bool, out chan<- application.ServiceLogEvent) {
	defer close(out)
	defer func() { _ = rc.Close() }()

	sink := &logSink{ctx: ctx, out: out, service: service, now: b.now}

	var err error
	if tty {
		err = readRawLog(rc, sink)
	} else {
		err = readFramedLog(rc, sink)
	}
	sink.flush()

	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// The daemon closed, which is what a --no-follow tail and a removed
		// service both look like.
	case ctx.Err() != nil:
		// The caller went away. Not a failure and not worth a line.
	default:
		// Logged and not sent. The daemon's words name socket paths and
		// internal state, and this stream is reachable by every subject an
		// authorizer grants ActionLogs — the same reasoning api.go applies to
		// the error from a failed open.
		b.log.Error("the service log stream ended with an error", "service", service, "error", err)
	}
}

// readFramedLog reads the daemon's multiplexed stream.
//
// A Systemerr frame is the daemon reporting a failure inside a stream it has
// already begun — "Error grabbing logs: …" — and ends the stream with that text
// as the error rather than delivering it as a log line, which is what it would
// look like to an operator otherwise.
func readFramedLog(r io.Reader, sink *logSink) error {
	var header [logHeaderLen]byte
	buf := make([]byte, logChunk)
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return err
		}
		size := int(binary.BigEndian.Uint32(header[logSizeOffset:]))

		var stream string
		switch header[logStreamByte] {
		case frameStdout:
			stream = "stdout"
		case frameStderr:
			stream = "stderr"
		case frameSystemerr:
			text, err := readN(r, buf, size)
			if err != nil {
				return err
			}
			return fmt.Errorf("the daemon reported an error in the log stream: %s", strings.TrimSpace(text))
		default:
			return fmt.Errorf("unrecognised log frame header %d", header[logStreamByte])
		}

		for size > 0 {
			n := min(size, len(buf))
			if _, err := io.ReadFull(r, buf[:n]); err != nil {
				return err
			}
			sink.write(stream, buf[:n])
			size -= n
		}
	}
}

// readRawLog reads a TTY service's unframed stream.
//
// Every line is stdout, because there is nothing in the bytes to say otherwise:
// the daemon multiplexes only when no selected task has a terminal, and with
// one the two sources were joined before they reached the socket. That is a
// property of the daemon rather than a choice made here, and stdout is the
// honest default for a reader that cannot tell them apart.
func readRawLog(r io.Reader, sink *logSink) error {
	buf := make([]byte, logChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sink.write("stdout", buf[:n])
		}
		if err != nil {
			return err
		}
	}
}

// readN reads exactly n bytes, capped, discarding the remainder. For a frame
// whose content is an error message rather than a log line.
func readN(r io.Reader, buf []byte, n int) (string, error) {
	var b strings.Builder
	for n > 0 {
		c := min(n, len(buf))
		if _, err := io.ReadFull(r, buf[:c]); err != nil {
			return b.String(), err
		}
		if b.Len() < maxLogLine {
			b.Write(buf[:c])
		}
		n -= c
	}
	return b.String(), nil
}

// logSink turns the bytes of one stream into events.
//
// It holds one partial line per stream, because a frame is not guaranteed to
// end on a newline and two frames of different streams can interleave around
// one. Everything about a line — the cap, the parse, the send — happens here so
// that the framed and unframed readers above stay readers.
type logSink struct {
	ctx     context.Context
	out     chan<- application.ServiceLogEvent
	service string
	now     func() time.Time

	// partial is what has arrived of a line that has not ended yet, per stream.
	partial map[string][]byte
	// overrun marks a stream whose current line passed the cap and was already
	// delivered truncated: the rest of it is discarded up to the next newline,
	// rather than arriving as a second line that was never written as one.
	overrun map[string]bool
	// dropped counts the lines the channel had no room for. Reported to the
	// caller as a notice the moment there is room to report it, so a console
	// showing a gap says that it is one.
	dropped int
}

func (s *logSink) write(stream string, p []byte) {
	if s.partial == nil {
		s.partial = map[string][]byte{}
		s.overrun = map[string]bool{}
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.hold(stream, p)
			return
		}
		s.hold(stream, p[:i])
		if s.overrun[stream] {
			s.overrun[stream] = false
		} else {
			s.emit(stream, string(s.partial[stream]))
		}
		s.partial[stream] = s.partial[stream][:0]
		p = p[i+1:]
	}
}

// hold appends to the line being assembled, delivering it truncated and marked
// if it passes the cap rather than growing to whatever the container writes.
func (s *logSink) hold(stream string, p []byte) {
	if s.overrun[stream] {
		return
	}
	s.partial[stream] = append(s.partial[stream], p...)
	if len(s.partial[stream]) <= maxLogLine {
		return
	}
	s.emit(stream, string(s.partial[stream][:maxLogLine]))
	s.notice("a log line longer than 1MiB was truncated")
	s.partial[stream] = s.partial[stream][:0]
	s.overrun[stream] = true
}

// flush delivers whatever a stream had assembled when the daemon closed. A
// final line with no newline after it is still a line.
func (s *logSink) flush() {
	for stream, line := range s.partial {
		if len(line) > 0 && !s.overrun[stream] {
			s.emit(stream, string(line))
		}
		s.partial[stream] = nil
	}
}

// emit parses one line and delivers it.
func (s *logSink) emit(stream, line string) {
	at, taskID, nodeID, message := parseLogLine(line, s.now)
	s.send(application.ServiceLogEvent{
		Service:   s.service,
		TaskID:    taskID,
		NodeID:    nodeID,
		Stream:    stream,
		Message:   message,
		Timestamp: at,
	})
}

// notice delivers a line the controller wrote rather than the container.
func (s *logSink) notice(message string) {
	s.send(application.ServiceLogEvent{
		Service: s.service,
		// stderr so the console's stream filter does not hide it from the
		// operator looking for trouble; Notice so nobody reads it as output.
		Stream:    "stderr",
		Message:   message,
		Timestamp: s.now().UTC(),
		Notice:    true,
	})
}

// send delivers an event without ever blocking on the consumer.
//
// A browser that has stopped reading must not be able to stall the goroutine
// draining the daemon connection, so a full channel drops the line and counts
// it. The count is reported as its own notice as soon as there is room, which
// is what keeps a gap in a console visible as a gap.
func (s *logSink) send(e application.ServiceLogEvent) {
	if s.dropped > 0 && !e.Notice {
		select {
		case s.out <- application.ServiceLogEvent{
			Service:   s.service,
			Stream:    "stderr",
			Message:   fmt.Sprintf("%d log lines were dropped because this client could not keep up", s.dropped),
			Timestamp: s.now().UTC(),
			Notice:    true,
		}:
			s.dropped = 0
		default:
			// Still no room. The count keeps rising and the notice goes out
			// with the true total whenever one line finally fits.
		}
	}
	if s.ctx.Err() != nil {
		return
	}
	select {
	case s.out <- e:
	default:
		s.dropped++
	}
}

// parseLogLine splits one line of the daemon's output into the four things the
// wire type carries.
//
// The layout is positional and comes from httputils.WriteLogStream: with
// Details the attributes and a space are prefixed, and with Timestamps the
// instant and a space go in front of those. So it is timestamp, attributes,
// message — and cutting twice is safe *only* while the head parses as an
// instant, which is why a line that does not start with one is taken as a
// message in its entirety rather than having its first two words eaten.
func parseLogLine(line string, now func() time.Time) (at time.Time, taskID, nodeID, message string) {
	head, rest, found := strings.Cut(line, " ")
	if !found {
		return now().UTC(), "", "", clean(line)
	}
	at, err := time.Parse(time.RFC3339Nano, head)
	if err != nil {
		return now().UTC(), "", "", clean(line)
	}

	attrs, msg, found := strings.Cut(rest, " ")
	if !found || !strings.Contains(attrs, "=") {
		// Details produced nothing to parse. The daemon always supplies the
		// three swarm context attributes for a service, so this is the shape a
		// stream opened without them has — and the whole remainder is the
		// message rather than a first word that happened to be eaten.
		return at, "", "", clean(rest)
	}
	taskID, nodeID = swarmContext(attrs)
	return at, taskID, nodeID, clean(msg)
}

// swarmContext picks the task and the node out of the daemon's attribute
// prefix, and drops everything else in it.
func swarmContext(attrs string) (taskID, nodeID string) {
	for _, pair := range strings.Split(attrs, ",") {
		k, v, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		key, err := url.QueryUnescape(k)
		if err != nil {
			continue
		}
		if key != attrTaskID && key != attrNodeID {
			continue
		}
		value, err := url.QueryUnescape(v)
		if err != nil {
			continue
		}
		if key == attrTaskID {
			taskID = value
		} else {
			nodeID = value
		}
	}
	return taskID, nodeID
}

// clean removes the control characters a container can put in a log line.
//
// Not for the browser's sake: React escapes a text child, so the console is
// safe either way. It is for the console's Copy and Export, which write these
// bytes verbatim to the clipboard and to a .txt file — and an operator who then
// opens that file in a terminal gets whatever cursor control, colour and title
// sequences the container chose to emit. Tab survives because log output is
// full of it. DEL and C1 are left alone, deliberately: this is a hardening pass
// on one text field and not a sanitiser, and saying so is better than implying
// a guarantee it does not make.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return -1
		}
		return r
	}, s)
}
