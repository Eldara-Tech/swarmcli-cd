// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

/**
 * PageTitle names the browser tab after the screen showing in it.
 *
 * React hoists a `<title>` rendered anywhere into the head, ahead of the static
 * one in index.html, which stays behind as the fallback for a screen that is
 * still loading. One per screen: two mounted at once would leave the tab to
 * whichever React inserted last.
 */
export function PageTitle({ page }: { page: string }) {
  return <title>{`${page} · SwarmCLI CD`}</title>
}
