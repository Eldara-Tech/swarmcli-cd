// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// The one Node global the browser run reads, declared rather than installed.
//
// @types/node would answer this too, and would put every Node global into
// whichever program included it — for a spec whose entire use of Node is four
// environment variables. This project's dependency budget is small on purpose
// (web-ui.md §4.4), and the narrower declaration is also the more honest one:
// what e2e/ may reach for is exactly this, and anything more should have to
// argue for itself.
//
// Not in src/'s program. tsconfig.e2e.json is what keeps it out; see the note
// there.
declare const process: { env: Record<string, string | undefined> }
