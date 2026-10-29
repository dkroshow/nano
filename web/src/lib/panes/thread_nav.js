// Keyboard navigation over the projected sidebar rows (lib/explorer/sidebar_projection.js):
// recency-ordered cycling over finished threads behind ⌥Tab / ⌥⇧Tab, and
// non-wrapping visual-order stepping behind ⌥↓ / ⌥↑.
// Pure functions only — the whole ordering and cycle contract is testable
// without Svelte or a DOM; lib/panes/keyboard.js owns the wiring into panes and the
// app's open-a-thread paths.
//
// The projected rows are the one source for all of it. They already merge the
// catalog's runtime overlay with the `/api/sessions` summaries (the same merge
// the sidebar dots read); ⌥↓/⌥↑ follow their visual order, while the ⌥Tab walk
// reorders them by recency and ignores sidebar position.

// Visibility is the one membership rule (Operator, 2026-08-25): the caller hands
// in the rows the sidebar actually renders, and everything rendered is
// walkable. Background threads leave the walk exactly when suppression or a
// collapsed group removes their row — never by mode while still on screen.
// The earlier mode-based skip hid visible, finished subagents from
// the walk and made it "skip down" past fresh threads.

// The ⌥Tab walk order: finished threads only, newest first. `updatedAt` is the
// same stamp the sidebar's age timer renders, so the walk and the visible
// timers always agree. Working threads are dropped entirely — the walk's job
// is "take me to what finished", never to a thread still running. Rows with
// equal or missing stamps keep their projected order (sort is stable).
export function recencyOrderedRows(rows) {
  return (rows ?? [])
    .filter((row) => !row.working)
    .sort((a, b) => rowStamp(b) - rowStamp(a))
}

function rowStamp(row) {
  const at = new Date(row.updatedAt ?? '').getTime()
  return Number.isFinite(at) ? at : 0
}

// The ⌥` jump target: the row with the newest updatedAt, working or not —
// that jump cares about activity, not completion, so it deliberately skips
// the finished-only filter the ⌥Tab walk applies. Ties keep the first
// projected row.
export function newestUpdatedRow(rows) {
  let best = null
  for (const row of rows ?? []) {
    if (!best || rowStamp(row) > rowStamp(best)) best = row
  }
  return best
}

// Cycle through an ordered row list, wrapping at either end. A workspace tab
// that has no matching row starts at the front (or back in reverse) rather
// than reintroducing a second, hidden navigation order.
export function cycleRowKey(rows, activeKey, step) {
  if (!rows?.length) return null
  const direction = step < 0 ? -1 : 1
  const index = rows.findIndex((row) => row.key === activeKey)
  if (index < 0) return direction > 0 ? rows[0]?.key ?? null : rows.at(-1)?.key ?? null
  return rows[(index + direction + rows.length) % rows.length]?.key ?? null
}

// The row above or below the active one in the sidebar's own order. No wrap at
// either end; with nothing active, stepping down opens the top row.
export function neighborKey(rows, activeKey, step) {
  const index = rows.findIndex((row) => row.key === activeKey)
  if (index < 0) return step > 0 ? rows[0]?.key ?? null : null
  return rows[index + step]?.key ?? null
}

// Which row the workspace is showing, read off the active pane tab exactly as
// the row's own highlight is (CompactThreadRow → isActiveSession /
// isActiveCatalogThread). There is no separate cursor to keep in sync.
export function activeRowKey(rows, tab) {
  if (!tab) return null
  const row = rows.find(
    (candidate) =>
      (tab.sessionId && candidate.click.sessionId === tab.sessionId) ||
      (tab.catalogId && candidate.click.catalogId === tab.catalogId),
  )
  return row?.key ?? null
}
