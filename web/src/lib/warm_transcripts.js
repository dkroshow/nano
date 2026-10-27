import { mergeTranscriptItems } from '../transcript_query.js'
import { noteNanoContact } from '../connection.svelte.js'

// The browser's end of the warm stream: one stream per page that carries the
// saved Enhanced rows of the threads the page shows in its sidebar, so
// selecting one paints at once. The page names the set in the order it
// renders it, and the window it draws into (lines x cols of the message
// font); the server sends each thread the newest rows that fill that window.
// A pane opening on a thread seeds its own stream from the entry
// (openFileTranscript), whose first tail then replaces it whole; the entry
// stays and keeps following, so the next open is instant too. When the stream
// reopens (a membership change, a larger window, the page shown again), each
// held entry rides along as a marker and the server sends only what it lacks.
// Entries are plain objects, not reactive state, since nothing renders them
// until a pane takes one. The stream is open only while the page is visible.

const REOPEN_DEBOUNCE_MS = 1000
// After the connection drops, the stream reopens itself with fresh markers
// after this long; the browser's own retry would reuse the old URL.
const RECONNECT_MS = 2000
// WARM_SET_MAX is the most threads the server takes from one page.
const WARM_SET_MAX = 100
// The window's lines and cols must stay inside the server's bounds.
const LINES_RANGE = [5, 1000]
const COLS_RANGE = [20, 2000]
// The page's horizontal chrome beside a message body, in pixels.
const SIDE_CHROME_PX = 56

const entries = new Map() // hosted thread id -> { items, turns, projection }
let source = null
let started = false
let setOf = () => []
let sent = { set: [], fill: { lines: 0, cols: 0 } } // what the open stream was asked for
let reopenTimer = null

// warmTranscript hands a pane opening on a thread its warm Enhanced rows, or
// null. The entry stays: the pane copies what it needs, and the stream keeps
// the entry current for the next open.
export function warmTranscript(ref) {
  return entries.get(ref) ?? null
}

// startWarmTranscripts opens the page's warm stream, and reopens it each time
// the page becomes visible again; set() names the threads the page shows, in
// order. A resize or text-size change reopens it when the window grew.
export function startWarmTranscripts(set) {
  if (started || typeof EventSource === 'undefined') return
  started = true
  setOf = set
  const follow = () => {
    if (document.visibilityState === 'visible') {
      if (!source) open()
      return
    }
    close()
  }
  document.addEventListener('visibilitychange', follow)
  globalThis.addEventListener?.('resize', () => scheduleReopen())
  globalThis.addEventListener?.('nano-text-size', () => scheduleReopen())
  follow()
}

// warmSetChanged tells the stream the page's rendered threads changed. Only a
// membership change reopens it (debounced); a reorder costs nothing.
export function warmSetChanged() {
  scheduleReopen()
}

function scheduleReopen() {
  if (!started || !source) return
  clearTimeout(reopenTimer)
  reopenTimer = setTimeout(() => {
    reopenTimer = null
    if (!source || !needsReopen()) return
    close()
    open()
  }, REOPEN_DEBOUNCE_MS)
}

// needsReopen: the set's membership changed, or the window grew. A smaller
// window keeps the stream: extra rows cost only memory.
function needsReopen() {
  const next = currentSet()
  if (next.length !== sent.set.length || next.some((ref) => !sent.set.includes(ref))) return true
  const fill = measureWindow()
  return fill.lines > sent.fill.lines || fill.cols > sent.fill.cols
}

function currentSet() {
  const out = []
  for (const ref of setOf() ?? []) {
    if (ref && !out.includes(ref)) out.push(ref)
    if (out.length === WARM_SET_MAX) break
  }
  return out
}

function close() {
  source?.close()
  source = null
}

function open() {
  const set = currentSet()
  const fill = measureWindow()
  const markers = {}
  for (const ref of set) {
    const entry = entries.get(ref)
    const key = entry?.projection?.key
    if (!entry?.items.length || !key) continue
    markers[ref] = {
      key,
      first: entry.items[0].projectedSeq,
      last: entry.items[entry.items.length - 1].projectedSeq,
      digest: heldDigest(entry.items),
    }
  }
  const params = new URLSearchParams()
  if (set.length) params.set('set', set.join(','))
  if (fill.lines && fill.cols) {
    params.set('lines', String(fill.lines))
    params.set('cols', String(fill.cols))
  }
  if (Object.keys(markers).length) params.set('markers', JSON.stringify(markers))
  sent = { set, fill }
  const query = params.toString()
  source = new EventSource(`/api/v1/warm-transcripts${query ? `?${query}` : ''}`)
  // A dropped connection reopens with the entries as they are now; the
  // browser's own retry would ask again with the markers of the first open.
  source.onerror = () => {
    close()
    clearTimeout(reopenTimer)
    reopenTimer = setTimeout(() => {
      reopenTimer = null
      if (!source && document.visibilityState === 'visible') open()
    }, RECONNECT_MS)
  }
  source.onmessage = (event) => {
    noteNanoContact()
    let message
    try {
      message = JSON.parse(event.data)
    } catch {
      return
    }
    apply(message)
  }
}

// heldDigest names the rows an entry holds, as the server computes it over
// its own rows (warmHeldDigest): every field the Enhanced view draws, per
// row in order, through 64-bit FNV-1a: strings as they are, numbers in
// decimal, booleans as 1 or 0, an absent value as nothing. Any change to a
// held row made while the page was away then shows as a different digest,
// and the server sends a tail instead of trusting the marker.
const DIGEST_FIELDS = ['id', 'projectedSeq', 'turnId', 'nativeItemId', 'type', 'category', 'role', 'status', 'text',
  'createdAt', 'phase', 'isFinalAnswer', 'sidechain', 'contentRef', 'contentTruncated', 'originalBytes']
const DIGEST_FLAGS = new Set(['isFinalAnswer', 'sidechain', 'contentTruncated'])
function heldDigest(items) {
  const field = (name, value) => {
    if (DIGEST_FLAGS.has(name)) return value ? '1' : '0'
    if (name === 'originalBytes') return value ? String(value) : ''
    return value == null ? '' : String(value)
  }
  const bytes = new TextEncoder().encode(items
    .map((row) => DIGEST_FIELDS.map((name) => `${field(name, row[name])}\n`).join(''))
    .join(''))
  let hash = 0xcbf29ce484222325n
  for (const byte of bytes) {
    hash ^= BigInt(byte)
    hash = (hash * 0x100000001b3n) & 0xffffffffffffffffn
  }
  return hash.toString(16).padStart(16, '0')
}

// measureWindow is the window in message lines and characters, from a probe
// set in the message font at the current text size. Zero when the page
// cannot measure (no layout); the server then uses its fallback window.
function measureWindow() {
  try {
    const probe = document.createElement('div')
    probe.style.cssText = 'position:absolute;visibility:hidden;white-space:pre;font-family:var(--mono);font-size:var(--mono-size);line-height:1.55'
    probe.textContent = `${'M'.repeat(100)}\n${'M'.repeat(100)}`
    document.body.appendChild(probe)
    const rect = probe.getBoundingClientRect()
    probe.remove()
    const charWidth = rect.width / 100
    const lineHeight = rect.height / 2
    if (!(charWidth > 0) || !(lineHeight > 0)) return { lines: 0, cols: 0 }
    const clamp = (value, [low, high]) => Math.min(high, Math.max(low, value))
    return {
      lines: clamp(Math.floor(globalThis.innerHeight / lineHeight), LINES_RANGE),
      cols: clamp(Math.floor((globalThis.innerWidth - SIDE_CHROME_PX) / charWidth), COLS_RANGE),
    }
  } catch {
    return { lines: 0, cols: 0 }
  }
}

function apply(message) {
  switch (message.type) {
    case 'set': {
      const kept = new Set(message.refs ?? [])
      for (const ref of entries.keys()) if (!kept.has(ref)) entries.delete(ref)
      break
    }
    case 'tail':
      entries.set(message.ref, held(message.rows ?? [], message.turns ?? [], message.projection ?? null, message.page?.startSeq))
      break
    case 'update': {
      const entry = entries.get(message.ref)
      if (!entry) break
      const removed = new Set(message.removed ?? [])
      const items = mergeTranscriptItems(entry.items, message.rows ?? []).filter((row) => !removed.has(row.id))
      entries.set(message.ref, held(items, mergeTranscriptItems(entry.turns, message.turns ?? []), message.projection ?? entry.projection, message.page?.startSeq))
      break
    }
    case 'error':
      entries.delete(message.ref)
      break
  }
}

// held is an entry: the rows from the window's first sequence on, in order,
// and the turns they are in.
function held(rows, turns, projection, startSeq) {
  const items = [...rows]
    .sort((a, b) => a.projectedSeq - b.projectedSeq)
    .filter((row) => startSeq == null || row.projectedSeq >= startSeq)
  const inTurns = new Set(items.map((row) => row.turnId))
  return { items, turns: turns.filter((turn) => inTurns.has(turn.id)), projection }
}
