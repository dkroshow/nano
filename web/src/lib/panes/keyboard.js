// The app's global shortcuts, registered once by App.svelte's <svelte:window>
// and deliberately live everywhere — including while the operator is typing in
// a composer, which is exactly when moving straight to the thread that just
// finished is worth a keystroke.
//
//   ⌥Tab   → the most recently finished thread, then onward through finished
//            threads newest-first; a pause restarts the walk from the top
//   ⌥⇧Tab  → back up the same finished-thread walk
//   ⌥`     → the most recently updated thread, working or not
//   ⌥↓ / ⌥↑ → the row below / above the active one in the sidebar's order
//
// Option + a non-character key reports a clean `e.key` on every layout, which
// is why the bindings avoid Option+letter (those arrive as ø, ∂, …). ⌥` is the
// exception: on the US layout it is a dead key (`e.key === 'Dead'`), so it is
// matched by physical position (`e.code === 'Backquote'`) — chosen anyway
// because it sits directly above Tab, keeping the two jumps one hand shape
// apart. The cost: composing accents via Option+` (è, à …) is unavailable
// while the shortcut is live.

import { openSidebarRow } from '../../app.svelte.js'
import { activeTab } from '../../panes.svelte.js'
import {
  activeRowKey,
  cycleRowKey,
  neighborKey,
  newestUpdatedRow,
  recencyOrderedRows,
} from './thread_nav.js'

// Overlays that own the keyboard while up: both blocking dialogs (New Window,
// Thread Start Parameters — Tab is their focus trap's) and the thread
// quick-detail card (it has its own Escape/Enter handling, and yanking the
// stage out from under it mid-read would be a misfire, not a jump). Each owns
// its open state locally, so presence in the DOM is the one shared signal.
const MODAL_OVERLAY_SELECTOR = '.nw-overlay, .quick-detail-overlay'
const SHORTCUT_KEYS = new Set(['Tab', 'ArrowDown', 'ArrowUp'])
const JUMP_NEWEST_CODE = 'Backquote'

// The ⌥Tab walk is a burst: presses within this window step onward from the
// last landing, so the operator can tap down the finished list. A longer pause
// means the next press is a fresh jump to the newest finished thread again.
export const TAB_WALK_TIMEOUT_MS = 4000

// The walk's position between presses, tracked by thread identity — never by
// list index — so a thread starting work mid-walk drops out of the list
// without teleporting the cursor. A thread FINISHING is different: it takes
// the top of the recency order, and Operator's rule is that a finish restarts
// the walk there. tabWalkHeadKey records who was on top when the walk last
// advanced; a new head means the world moved and the next press goes to it.
let tabCursorKey = null
let tabWalkHeadKey = null
let lastTabPressAt = 0
let now = () => Date.now()

// The reference the settings menu renders. Lives here so the legend and the
// bindings can only change together.
export const SHORTCUT_LEGEND = [
  { keys: '⌥Tab', label: 'Cycle finished threads, newest first' },
  { keys: '⌥⇧Tab', label: 'Reverse the finished-thread cycle' },
  { keys: '⌥`', label: 'Jump to the most recently updated thread' },
  { keys: '⌥↓ / ⌥↑', label: 'Sidebar row below / above' },
]

export function handleThreadShortcut(event, rows) {
  if (event.defaultPrevented || !event.altKey || event.metaKey || event.ctrlKey) return false
  if (!SHORTCUT_KEYS.has(event.key) && event.code !== JUMP_NEWEST_CODE) return false
  if (document.querySelector(MODAL_OVERLAY_SELECTOR)) return false

  // Handled even when it resolves to nothing: at the end of the stack or the
  // list the shortcut is a deliberate no-op, and handing the keystroke back to
  // the browser would move focus or jump the composer's caret instead.
  event.preventDefault()
  const row = targetRow(event, rows)
  if (!row) return false
  openSidebarRow(row.click)
  // The landing row's key, so the caller can reveal it in the rail — the walk
  // ignores sidebar position, so the landing is routinely scrolled offscreen.
  // Still truthy, preserving the boolean contract.
  return row.key ?? true
}

function targetRow(event, rows) {
  if (event.code === JUMP_NEWEST_CODE) {
    // A jump is a context switch, not part of a walk: the next ⌥Tab press
    // starts fresh from the newest finished thread.
    tabCursorKey = null
    tabWalkHeadKey = null
    return newestUpdatedRow(rows)
  }
  if (event.key === 'Tab') {
    const ordered = recencyOrderedRows(rows)
    if (!ordered.length) return null
    const direction = event.shiftKey ? -1 : 1
    const pressedAt = now()
    // Mid-walk only when the last landing is recent, its thread is still a
    // valid target, AND nothing new has finished since: a fresh finisher takes
    // the head of the recency order, and a changed head restarts the walk at
    // the top instead of stepping past it.
    const walking = tabCursorKey !== null
      && pressedAt - lastTabPressAt <= TAB_WALK_TIMEOUT_MS
      && ordered.some((row) => row.key === tabCursorKey)
      && ordered[0].key === tabWalkHeadKey
    lastTabPressAt = pressedAt
    tabWalkHeadKey = ordered[0].key
    let key
    if (walking) {
      key = cycleRowKey(ordered, tabCursorKey, direction)
    } else {
      // A fresh press jumps to the newest finished thread — unless that is
      // already on screen, where the useful move is the one after it.
      const activeKey = activeRowKey(ordered, activeTab())
      key = activeKey === ordered[0].key
        ? cycleRowKey(ordered, activeKey, direction)
        : ordered[0].key
    }
    tabCursorKey = key
    return ordered.find((row) => row.key === key) ?? null
  }
  tabCursorKey = null
  tabWalkHeadKey = null
  const active = activeRowKey(rows, activeTab())
  const key = neighborKey(rows, active, event.key === 'ArrowDown' ? 1 : -1)
  return rows.find((row) => row.key === key) ?? null
}

export function __resetThreadNavForTest(clock) {
  tabCursorKey = null
  tabWalkHeadKey = null
  lastTabPressAt = 0
  now = clock ?? (() => Date.now())
}
