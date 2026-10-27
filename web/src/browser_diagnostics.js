// Content-free browser flight recorder. The hard refresh that heals a stale
// pane must not erase the evidence, so a bounded ring stays in sessionStorage
// and is mirrored to Nano for agent-readable incident traces.

const STORAGE_KEY = 'nano.browser-diagnostics.v1'
const MAX_EVENTS = 600
const MAX_BATCH = 100
// A browser refuses a keepalive request once a page's keepalive bodies in
// flight exceed 64 KiB.
const MAX_KEEPALIVE_BYTES = 64 << 10
const TOKEN = /^[A-Za-z0-9._:/-]*$/
const STRING_FIELDS = [
  'paneId', 'provider', 'threadId', 'sessionId', 'previousSessionId',
  'hostedThreadId', 'actionId', 'interactionId', 'streamId', 'epoch', 'status', 'outcome',
  'trigger', 'op', 'code', 'activeRenderSource', 'view', 'boundaryName',
]
const NUMBER_FIELDS = [
  'revision', 'incomingRevision', 'itemCount', 'pendingCount', 'httpStatus',
  'visibleItemCount', 'matchingDOMActionRowCount',
  'expectedRowCount', 'mountedRowCount', 'missingRowCount', 'extraRowCount',
  'duplicateRowCount', 'semanticDuplicateRowCount', 'blankRowCount', 'typeMismatchCount', 'orderMismatchCount',
  'arrivalRateCps', 'batchIntervalMs', 'batchSize', 'batchCount', 'maxBacklog',
  'publishCount', 'durationMs', 'firstRowsMs', 'newestRowsMs', 'historyMs', 'historyBytes',
  'layoutSequence', 'rowIndex', 'previousRowIndex', 'topPx', 'previousTopPx',
  'movementPx', 'movementCount', 'maxMovementPx', 'reorderCount', 'addedCount',
  'removedCount', 'workingCount', 'starredCount', 'detachedCount',
  'scrollTop', 'scrollHeight', 'viewportHeight',
]
const BOOLEAN_FIELDS = ['visible', 'connected', 'turnActive', 'warm']

let memoryState = null
let started = false
let flushTimer = null
let flushInFlight = null
let saveTimer = null
let uploaded = new Set()
let buildCache = null
const pageID = makeID('page')

function makeID(prefix) {
  const id = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(16).slice(2)}`
  return `${prefix}-${id}`
}

function emptyState() {
  return { tabId: makeID('tab'), nextSeq: 1, serverBootId: '', events: [] }
}

function loadState() {
  if (memoryState) return memoryState
  try {
    const parsed = JSON.parse(sessionStorage.getItem(STORAGE_KEY) || 'null')
    if (parsed?.tabId && Array.isArray(parsed.events)) {
      memoryState = {
        tabId: token(parsed.tabId, 100) || makeID('tab'),
        nextSeq: Math.max(1, Number(parsed.nextSeq) || 1),
        serverBootId: token(parsed.serverBootId, 100),
        events: parsed.events.slice(-MAX_EVENTS),
      }
      return memoryState
    }
  } catch {
    // Private mode and test environments may not expose sessionStorage.
  }
  memoryState = emptyState()
  return memoryState
}

function saveStateNow() {
  if (saveTimer != null) clearTimeout(saveTimer)
  saveTimer = null
  try {
    sessionStorage.setItem(STORAGE_KEY, JSON.stringify(loadState()))
  } catch {
    // The in-memory ring still helps for the life of this page.
  }
}

function saveState() {
  if (saveTimer != null) return
  saveTimer = setTimeout(saveStateNow, 0)
}

function token(value, max = 160) {
  const normalized = String(value ?? '').trim().slice(0, max)
  return TOKEN.test(normalized) ? normalized : ''
}

function loadedBuild() {
  if (buildCache != null) return buildCache
  try {
    const script = [...document.querySelectorAll('script[src]')]
      .map((node) => node.getAttribute('src') || '')
      .find((src) => /assets\/index-[^/]+\.js(?:\?|$)/.test(src))
    buildCache = token(script?.match(/index-[^/?]+\.js/)?.[0] ?? '', 100)
  } catch {
    buildCache = ''
  }
  return buildCache
}

export function browserDiagnosticIdentity() {
  const state = loadState()
  return { tabId: state.tabId, pageId: pageID, build: loadedBuild() }
}

function bool(value) {
  return typeof value === 'boolean' ? value : undefined
}

export function recordBrowserDiagnostic(kind, fields = {}) {
  const state = loadState()
  const safeKind = token(kind, 80)
  if (!safeKind) return null
  const seq = state.nextSeq++
  const event = {
    eventId: `${state.tabId}:${seq}`,
    clientAt: new Date().toISOString(),
    clientSeq: seq,
    monotonicMs: Math.round((globalThis.performance?.now?.() ?? 0) * 10) / 10,
    kind: safeKind,
    tabId: state.tabId,
    pageId: pageID,
    build: loadedBuild(),
  }
  for (const key of STRING_FIELDS) {
    const value = token(fields[key], key === 'provider' ? 32 : 160)
    if (value) event[key] = value
  }
  for (const key of NUMBER_FIELDS) {
    const value = Number(fields[key])
    if (Number.isFinite(value) && value >= 0) event[key] = Math.trunc(value)
  }
  for (const key of BOOLEAN_FIELDS) {
    const value = bool(fields[key])
    if (value !== undefined) event[key] = value
  }
  state.events.push(event)
  if (state.events.length > MAX_EVENTS) state.events.splice(0, state.events.length - MAX_EVENTS)
  saveState()
  if (started) scheduleFlush()
  return event
}

export function classifyJavaScriptFailure(value) {
  const message = String(value?.message ?? value ?? '').toLowerCase()
  if (message.includes('each_key_duplicate') || (message.includes('duplicate') && message.includes('key'))) {
    return 'duplicate_key'
  }
  if (message.includes('hydration')) return 'hydration'
  if (message.includes('maximum call stack')) return 'stack_overflow'
  const name = token(value?.name, 100)
  return name ? name.toLowerCase() : 'unknown'
}

function recordJavaScriptFailure(op, value) {
  recordBrowserDiagnostic('js.error', { op, code: classifyJavaScriptFailure(value) })
}

function scheduleFlush(delay = 250) {
  if (flushTimer != null) return
  flushTimer = setTimeout(() => {
    flushTimer = null
    void flushBrowserDiagnostics()
  }, delay)
}

// postEvents sends one batch and records Nano's answer. A changed boot id
// means Nano lost its copy, so every retained event counts as unsent again.
async function postEvents(state, batch, keepalive = false) {
  const response = await fetch('/api/v1/browser-diagnostics', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ events: batch }),
    keepalive,
  })
  if (!response.ok) throw new Error(`browser diagnostics ${response.status}`)
  const result = await response.json()
  const bootID = token(result.bootId, 100)
  const restarted = Boolean(state.serverBootId && bootID && state.serverBootId !== bootID)
  if (bootID) state.serverBootId = bootID
  if (restarted) uploaded.clear()
  for (const event of batch) uploaded.add(event.eventId)
  saveStateNow()
  return result
}

// Events hold only ASCII tokens, numbers and booleans, so a JSON string's
// length is its size in bytes.
function withinKeepaliveCap(batch) {
  let bytes = JSON.stringify({ events: [] }).length
  const kept = []
  for (const event of batch) {
    bytes += JSON.stringify(event).length + (kept.length ? 1 : 0)
    if (bytes > MAX_KEEPALIVE_BYTES) break
    kept.push(event)
  }
  return kept
}

// Only the pagehide flush must outlive the page, so only it uses keepalive,
// with as many events as fit under the browser's cap.
export async function flushBrowserDiagnostics({ keepalive = false } = {}) {
  const state = loadState()
  saveStateNow()
  if (flushInFlight) return flushInFlight
  let batch = state.events.filter((event) => !uploaded.has(event.eventId)).slice(0, MAX_BATCH)
  if (keepalive) batch = withinKeepaliveCap(batch)
  if (!batch.length || typeof fetch !== 'function') return null
  flushInFlight = postEvents(state, batch, keepalive).then((result) => {
    if (state.events.some((event) => !uploaded.has(event.eventId))) scheduleFlush(0)
    return result
  }).catch(() => {
    // The ring remains in sessionStorage and the next lifecycle/stream event
    // retries. A server restart is an expected failure mode for this recorder.
    scheduleFlush(5_000)
    return null
  }).finally(() => {
    flushInFlight = null
  })
  return flushInFlight
}

// Sends every event this tab retains, acknowledged or not: Nano's shared ring
// may have evicted earlier uploads, and its store skips events it still holds.
// A restart part way through loses the earlier batches, so the whole ring is
// sent again under one boot. Resolves to the boot id that holds the ring, or
// '' when an upload fails or Nano keeps restarting.
export async function uploadBrowserDiagnosticRing() {
  const state = loadState()
  saveStateNow()
  const events = [...state.events]
  try {
    for (let attempt = 0; attempt < 2; attempt++) {
      let bootID = ''
      let stable = true
      for (let start = 0; start < events.length && stable; start += MAX_BATCH) {
        const result = await postEvents(state, events.slice(start, start + MAX_BATCH))
        const batchBoot = token(result.bootId, 100)
        stable = !bootID || batchBoot === bootID
        bootID = batchBoot
      }
      if (stable) return bootID
    }
  } catch {
    // The caller reports the failed upload.
  }
  return ''
}

export function startBrowserDiagnostics() {
  if (started) return
  started = true
  recordBrowserDiagnostic('page.load', {
    visible: typeof document !== 'undefined' ? document.visibilityState === 'visible' : true,
    connected: typeof navigator !== 'undefined' ? navigator.onLine : true,
  })
  if (typeof document !== 'undefined') {
    document.addEventListener('visibilitychange', () => {
      recordBrowserDiagnostic('page.visibility', { visible: document.visibilityState === 'visible' })
    })
  }
  if (typeof window !== 'undefined') {
    window.addEventListener('error', (event) => recordJavaScriptFailure('window.error', event.error ?? event.message))
    window.addEventListener('unhandledrejection', (event) => recordJavaScriptFailure('promise.rejection', event.reason))
    window.addEventListener('online', () => recordBrowserDiagnostic('page.network', { connected: true }))
    window.addEventListener('offline', () => recordBrowserDiagnostic('page.network', { connected: false }))
    window.addEventListener('pagehide', () => {
      recordBrowserDiagnostic('page.hide', { visible: false })
      void flushBrowserDiagnostics({ keepalive: true })
    })
  }
}

export function __resetBrowserDiagnosticsForTest() {
  memoryState = null
  started = false
  uploaded = new Set()
  buildCache = null
  clearTimeout(saveTimer)
  saveTimer = null
  clearTimeout(flushTimer)
  flushTimer = null
  flushInFlight = null
}
