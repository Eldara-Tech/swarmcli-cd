// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

/**
 * Where the Projects nav item goes, until the screen behind it is built.
 *
 * Both this and the nav item render only when features["projects"] is true, so
 * a free build has neither the route nor the link and its router table is the
 * one Phase B shipped. It exists because a NavLink to a path no route claims
 * matches nothing at all — which unmounts the layout route with it, and presents
 * as the entire UI disappearing when somebody clicks a nav item.
 *
 * The list itself reads GET /api/v1/projects, which docs/extensibility.md
 * reserves for the companion and no build serves yet; that screen is its own
 * issue.
 */
export function Projects() {
  return (
    <section className="screen">
      <h1>Projects</h1>
      <p className="empty">This build grants projects. The screen that lists them has not been built yet.</p>
    </section>
  )
}
