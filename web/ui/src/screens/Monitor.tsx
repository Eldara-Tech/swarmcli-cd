// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

import { TerminalStream } from '../components/TerminalStream'
import { useLive } from '../live'

/**
 * Monitor: the controller's reconcile events, live and full-height.
 *
 * It reads the tab's one stream through useLive rather than opening its own —
 * the scrollback is what useEventStream has already accumulated (bounded, and
 * lost across a reconnect, which is why the documents and not this are
 * authoritative). This is the controller's own activity, not container output:
 * per-service logs are a later phase and the note below says so rather than
 * leaving the gap to be discovered.
 */
export function Monitor() {
  const live = useLive()

  return (
    <section className="screen">
      <header className="screen-header">
        <h1>Monitor</h1>
        <p className="muted">Controller reconcile events, as they happen.</p>
      </header>

      <TerminalStream title="Controller stream" events={live.log} className="monitor-terminal" />

      <p className="phase2-note">
        This is the controller&rsquo;s own event stream. Per-service container logs are a later phase — the
        controller does not expose them yet.
      </p>
    </section>
  )
}
