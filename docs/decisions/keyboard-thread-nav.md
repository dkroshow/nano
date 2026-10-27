# Keyboard thread navigation — recency walk (2026-07-31, revised 2026-08-20)

## Decision

Global shortcuts, active even while the composer has focus:

- `⌥Tab` / `⌥⇧Tab` — walk finished threads newest-first by `updatedAt`, the
  same stamp the sidebar's age timer renders, ignoring sidebar position. A
  fresh press jumps to the most recently updated finished thread (or the one
  after it when that is already on screen); presses within the walk timeout
  (`TAB_WALK_TIMEOUT_MS`, 4 s) step onward; `⌥⇧Tab` steps back; the walk
  wraps. A longer pause restarts from the top. Working threads are never
  targets. The list is read fresh on every press with the cursor tracked by
  thread identity. A thread *finishing* mid-walk restarts the walk: the
  finisher takes the head of the recency order, and a changed head means the
  next press goes to it instead of stepping past it (Operator's rule,
  2026-08-25). A thread merely *starting* work drops out of the list without
  teleporting the cursor.
- `` ⌥` `` — jump to the most recently updated thread, working or not, and
  reset the walk. Matched by physical key (`event.code === 'Backquote'`)
  because Option+` is a dead key on the US layout; the cost is that composing
  accents via Option+` is unavailable while the app has focus.
- `⌥↓` / `⌥↑` — open the row below / above the active one in the sidebar's
  projected order. No wrap anywhere.

Visibility is the one membership rule: the walk and the
jump cover exactly the rows the sidebar renders — post-suppression,
post-collapse, post-cap. A collapsed or suppressed thread is unreachable; an
expanded subagent or a background thread the tree keeps on screen is a
legitimate target like any other. (The interim mode-based skip hid visible
finished subagents and made the walk drop past fresh threads; replaced
2026-08-25.) Backgrounding a thread still removes it from the shortcuts
whenever suppression actually hides it. An existing thread is flipped with
`PUT /api/v1/hosted-threads/{ref}/presentation` (quick-detail card: "Move to
background" / "Bring to sidebar"); nano_threads.sqlite is the single durable
authority for the mode.

Implementation: `web/src/lib/thread_nav.js` (pure ordering and target rules) +
`web/src/lib/keyboard.js` (walk state, timeout, the one `<svelte:window>`
handler in `App.svelte`). Keyboard and click share one open path
(`openSidebarRow` in `app.svelte.js`).

## Why these keys

Focus lives in the composer textarea when the operator wants to jump, so bare
keys (vim j/k) are out. `⌘` chords belong to the browser (`⌘1-9` = browser
tabs, `⌘J` = downloads); `⌃`-letter chords collide with macOS readline in text
fields (`⌃K` kills the line). Option + non-character keys report clean `e.key`
values on every layout (Option+letter arrives as `∆`, `ø`, …) and carry no
system bindings on macOS 15 — Sequoia tiling is Fn/Ctrl+arrows, Mission Control
is Ctrl+arrows, Rectangle/Magnet default to Ctrl+Option+arrows. Safari binds
Option+Tab to in-page focus-nav, but that is a page-level default action and is
suppressed by `preventDefault()`; `⌥↑/↓` follows the Slack/Discord channel-nav
convention. `` ⌥` `` was chosen for proximity: it sits directly above Tab, so
the walk and the jump are one hand shape apart, and it echoes the Cmd+`
window-cycle habit. `⌥Return` was rejected (the composer submits on Enter and
the operator wanted the key next to Tab); `⌥←/→` and `⌥Backspace` are word
navigation while typing.

## Why recency, not sidebar order or a completion stack

The sidebar order is a visual arrangement (liveness bands, then dataset
order), not an attention queue. The earlier completion-stack design tracked
`turnActive` edges client-side to order finished threads exactly; it was
process-local, invisible after a reload, and diverged from the timer the
operator actually reads. `updatedAt` is visible on every row, survives reloads,
and orders working and finished threads alike, so the walk and the timers can
never disagree.

## Revisit when

- A manual click within the walk timeout should restart the walk: today the
  cursor survives a click for up to 4 s, so the next `⌥Tab` continues the old
  walk. Bounded by the timeout; reset on any manual navigation if it grates.
- Anything wants to *render* the jump list: the walk state in `keyboard.js` is
  deliberately non-reactive and would need to become a rune.
- Catalog-only threads (never nano sessions) need backgrounding: suppression
  and the keyboard rule both key off `row.runtime.presentationMode`, which
  those rows lack.
