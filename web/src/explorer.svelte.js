// Thread Explorer store. The explorer is a singleton surface (one read-only
// catalog view), so this is a single module-level state object plus command
// functions. Its row arrays (dataset, activeItems, windowItems and
// historyScopes) are raw: every write replaces them whole, never edits a row
// in place. Reading some 600 rows through deep proxies made each sidebar
// update cost about 50 ms of main thread, which typing waited behind.
// The seam with the backend is the versioned HTTP contract under /api/v1; this
// module never reads provider files or invents metadata defaults.

import { buildWindowQuery, mergePages, DEFAULT_FETCH, DEFAULT_VIEW } from './explorer_query.js'
import { reopenIfClosed } from './lib/shared/stream_reopen.js'
import { loadTranscript, closeTranscript, nudgeTabHistories, nudgeTranscriptTail } from './transcript_store.svelte.js'
import { applyMailDelta, resetMailCounts } from './lib/mail/mail.svelte.js'
import { dispatchTranscriptAuditRequest } from './transcript_audit_events.js'
import { railLink } from './lib/status/freshness.js'
import { noteNanoContact } from './connection.svelte.js'

// Fallback poll cadence — used ONLY while the SSE stream is broken. A healthy
// stream drives updates by push, so no timer runs (docs/decisions/sse-catalog-push.md).
const SCAN_POLL_MS = 2000 // while starting/scanning
const IDLE_POLL_MS = 10000 // while ready/degraded
const REFRESH_FEEDBACK_MS = 2000
const CATALOG_EVENTS_URL = '/api/v1/thread-index/events'
const MAX_PAGE_CACHE = 32
export const ACTIVE_REFRESH_MS = 250
// A failed whole read of the active list is retried after this long: a quiet
// stream and an unchanged catalog version would otherwise never ask again.
const ACTIVE_REFRESH_RETRY_MS = 5000
const ACTIVE_FLEET_LABELS = new Set(['working', 'needs_you', 'stale'])

class ExplorerState {
  fetchParams = $state({ ...DEFAULT_FETCH }) // change what the server sends → refetch
  view = $state({ ...DEFAULT_VIEW }) // sort/selections/ranges — applied locally, instant
  dataset = $state.raw([]) // every thread of the fetched window, held whole in memory
  datasetComplete = $state(false) // false while window pages are still streaming in
  datasetScope = $state('empty') // empty | active | window
  datasetSource = $state('')
  sidebarBootState = $state('idle') // idle | loading | ready | error
  sidebarFallbackReason = $state('')
  activeItems = $state.raw([])
  windowItems = $state.raw([])
  historicalCount = $state(0)
  historyScopes = $state.raw({})
  listState = $state('idle') // idle | loading | error
  listError = $state(null)
  catalogVersion = $state(null) // version the current dataset was fetched at
  index = $state(null) // GET /thread-index/status payload
  indexError = $state(null) // { op: 'status' | 'refresh', message } — which operation failed
  selectedId = $state(null)
  detail = $state(null)
  detailState = $state('idle') // idle | loading | error
  detailError = $state(null)
  refreshRequested = $state(false) // transient feedback for the manual refresh button
  link = $state({ epoch: 0, up: true }) // the catalog stream's freshness (lib/status/freshness.js railLink)
}

export const explorer = new ExplorerState()

// Latest-request-wins dataset fetching. loadDataset aborts whatever window
// fetch is in flight and starts fresh; the sequence number is belt-and-braces
// — even if an aborted response still resolves (or a fetch mock ignores the
// signal), only the newest fetch loop may write dataset state.
let listAbort = null
let listSeq = 0
let detailAbort = null
let detailSeq = 0
let windowAnchor = null
let sidebarBootstrap = null
let sidebarBootstrapInFlight = false
let sidebarWindowRequested = false
let activeRefreshTimer = null
let activeRefreshSeq = 0
let activeRefreshAbort = null
// The materialized sidebar revision the active list reflects (0: none read).
// Each `sidebar` event names the revision it produces; one that follows this
// number is applied, anything else is a gap closed by one full read. Only a
// read that began on the live stream and lands on it can set this number.
let sidebarRevision = 0
// The server process whose revisions those are: a restarted Nano counts from
// 1 again, so a revision alone proves nothing across a restart.
let sidebarGeneration = ''
let activeRefreshRetry = null
const pageCache = new Map() // request URL -> last full page body
const pageETags = new Map() // request URL -> validator for the cached body

async function apiError(res) {
  try {
    const body = await res.json()
    return new Error(body?.error?.message || `request failed (${res.status})`)
  } catch {
    return new Error(`request failed (${res.status})`)
  }
}

async function fetchThreads(cursor, signal, now) {
  const url = `/api/v1/threads?${buildWindowQuery(explorer.fetchParams, cursor, now)}`
  const etag = pageETags.get(url)
  const headers = etag ? { 'If-None-Match': etag } : undefined
  let res = await fetch(url, { signal, ...(headers ? { headers } : {}) })
  if (res.status === 304 && pageCache.has(url)) return pageCache.get(url)
  if (!res.ok) throw await apiError(res)
  const page = await res.json()
  pageCache.delete(url)
  pageCache.set(url, page)
  while (pageCache.size > MAX_PAGE_CACHE) pageCache.delete(pageCache.keys().next().value)
  const nextETag = res.headers?.get?.('ETag')
  if (nextETag) pageETags.set(url, nextETag)
  else pageETags.delete(url)
  return page
}

export function __resetExplorerPageCacheForTest() {
  pageCache.clear()
  pageETags.clear()
  windowAnchor = null
}

export function __resetSidebarBootstrapForTest() {
  sidebarBootstrap = null
  sidebarBootstrapInFlight = false
  sidebarWindowRequested = false
  activeRefreshTimer = null
  activeRefreshSeq += 1
  activeRefreshAbort?.abort()
  activeRefreshAbort = null
  clearTimeout(activeRefreshRetry)
  activeRefreshRetry = null
  sidebarRevision = 0
  sidebarGeneration = ''
  explorer.datasetScope = 'empty'
  explorer.datasetSource = ''
  explorer.sidebarBootState = 'idle'
  explorer.sidebarFallbackReason = ''
  explorer.activeItems = []
  explorer.windowItems = []
  explorer.historicalCount = 0
  explorer.historyScopes = {}
}

export function sidebarMergeKey(row) {
  return row?.nativeId ? `${row.provider}:${row.nativeId}` : row?.id
}

export function mergeActiveAndHistory(activeItems, historyScopes) {
  const activeKeys = new Set((activeItems ?? []).map(sidebarMergeKey).filter(Boolean))
  const historyKeys = new Set()
  const historyRows = []
  for (const scope of Object.values(historyScopes ?? {})) {
    for (const row of scope.items ?? []) {
      const key = sidebarMergeKey(row)
      if (key && (activeKeys.has(key) || historyKeys.has(key))) continue
      if (key) historyKeys.add(key)
      historyRows.push(row)
    }
  }
  return [...(activeItems ?? []), ...historyRows]
}

export function mergeWindowWithActive(windowItems, activeItems) {
  const byKey = new Map()
  const order = []
  for (const row of windowItems ?? []) {
    const key = sidebarMergeKey(row)
    if (!key || byKey.has(key)) continue
    byKey.set(key, row)
    order.push(key)
  }
  for (const row of activeItems ?? []) {
    const key = sidebarMergeKey(row)
    if (!key) continue
    if (byKey.has(key)) byKey.set(key, row)
    else {
      byKey.set(key, row)
      order.push(key)
    }
  }
  return order.map((key) => byKey.get(key))
}

function rebuildRenderDataset() {
  if (explorer.datasetComplete && explorer.datasetScope === 'window') {
    explorer.dataset = mergeWindowWithActive(explorer.windowItems, explorer.activeItems)
    return
  }
  explorer.dataset = mergeActiveAndHistory(explorer.activeItems, explorer.historyScopes)
  explorer.datasetScope = explorer.activeItems.length || explorer.dataset.length ? 'active' : 'empty'
  explorer.datasetSource = 'sidebar-active'
}

// commitActiveSidebar replaces the active list with one whole read that
// began in epoch. A read is live when it began on the stream subscription
// that is up now; only a live read starts the revision sequence the deltas
// continue, and it is dropped when it is older than a delta the list has
// applied. Any other read (the bootstrap read after a drop, a read begun
// during an outage or before a reconnect) says nothing about now: it fills
// the last-seen rows if no live read has landed, and is dropped otherwise.
function commitActiveSidebar(body, epoch) {
  const revision = Number(body?.revision) || 0
  const generation = String(body?.generation ?? '')
  const live = epoch === explorer.link.epoch && explorer.link.up
  if (!live && sidebarRevision) return
  if (live && revision && generation === sidebarGeneration && revision < sidebarRevision) return
  explorer.activeItems = body?.items ?? []
  if (live) {
    sidebarRevision = revision
    sidebarGeneration = generation
  }
  commitActiveSidebarCounts(body)
}

// applySidebarDelta patches the active list with one `sidebar` event: the rows
// it carries replace or join the list, and its removed ids leave it. Rows the
// event does not name are unchanged. An event that names no row (the server
// sends one when only the catalog version moved) updates the counts and
// version and leaves the rows, so the sidebar is not rebuilt for it.
function applySidebarDelta(payload) {
  const removed = new Set(payload.removedIds ?? [])
  const incoming = payload.rows ?? []
  const rowsChanged = incoming.length > 0 || removed.size > 0
  if (rowsChanged) {
    const byId = new Map(incoming.map((row) => [row.id, row]))
    const next = explorer.activeItems.filter((row) => !removed.has(row.id)).map((row) => byId.get(row.id) ?? row)
    const present = new Set(next.map((row) => row.id))
    for (const row of incoming) if (!present.has(row.id)) next.push(row)
    explorer.activeItems = next
  }
  sidebarRevision = Number(payload.revision)
  commitActiveSidebarCounts(payload, rowsChanged)
}

function commitActiveSidebarCounts(body, rowsChanged = true) {
  explorer.historicalCount = Number(body?.historicalCount) || 0
  // A full-history window owns its own repair version. Advancing it from an
  // active-only response could make a same-version event for a window-only row
  // look stale and skip that row's targeted patch.
  if (body?.catalogVersion != null && explorer.datasetScope !== 'window') {
    explorer.catalogVersion = Number(body.catalogVersion)
  }
  if (rowsChanged) rebuildRenderDataset()
}

// On (re-)entering the explorer view: fetch only if nothing is cached yet.
// A cached dataset renders immediately; the status poll that starts alongside
// this reconciles a stale catalogVersion with a silent window refetch.
export function ensureLoaded() {
  if (sidebarBootstrapInFlight) {
    sidebarWindowRequested = true
    return
  }
  if (!explorer.datasetComplete && !listAbort) loadDataset() // in-flight boot prefetch already covers this
}

export function bootstrapSidebar(initialSessionsPromise) {
  if (explorer.datasetComplete) {
    explorer.datasetScope = 'window'
    explorer.sidebarBootState = 'ready'
    return Promise.resolve('preset')
  }
  if (sidebarBootstrap) return sidebarBootstrap
  sidebarBootstrapInFlight = true
  const epoch = explorer.link.epoch
  explorer.sidebarBootState = 'loading'
  explorer.sidebarFallbackReason = ''
  explorer.listError = null
  const controller = new AbortController()
  sidebarBootstrap = Promise.all([
    fetch('/api/v1/sidebar/threads', { cache: 'no-store', signal: controller.signal })
      .then(async (response) => {
        if (!response.ok) throw await apiError(response)
        return response.json()
      }),
    Promise.resolve(initialSessionsPromise),
  ]).then(([body]) => {
    if (explorer.datasetComplete || explorer.datasetScope === 'window') {
      explorer.sidebarBootState = 'ready'
      return 'window'
    }
    commitActiveSidebar(body, epoch)
    explorer.sidebarBootState = 'ready'
    return 'active'
  }).catch((error) => {
    if (explorer.datasetComplete || explorer.datasetScope === 'window') {
      explorer.sidebarBootState = 'ready'
      return 'window'
    }
    explorer.sidebarBootState = 'error'
    explorer.sidebarFallbackReason = error?.message ?? 'Active sidebar could not be loaded.'
    explorer.listError = explorer.sidebarFallbackReason
    explorer.dataset = []
    explorer.datasetScope = 'empty'
    return 'error'
  }).finally(() => {
    sidebarBootstrapInFlight = false
    if (sidebarWindowRequested) {
      sidebarWindowRequested = false
      if (!listAbort) loadDataset()
    }
  })
  return sidebarBootstrap
}

async function fetchActiveSidebar(signal) {
  const res = await fetch('/api/v1/sidebar/threads', { cache: 'no-store', signal })
  if (!res.ok) throw await apiError(res)
  return res.json()
}

export function scheduleActiveRefresh() {
  if (activeRefreshTimer) clearTimeout(activeRefreshTimer)
  activeRefreshTimer = setTimeout(() => {
    activeRefreshTimer = null
    void refreshActiveSidebar()
  }, ACTIVE_REFRESH_MS)
}

async function refreshActiveSidebar() {
  const seq = ++activeRefreshSeq
  const epoch = explorer.link.epoch
  activeRefreshAbort?.abort()
  const controller = new AbortController()
  activeRefreshAbort = controller
  try {
    const body = await fetchActiveSidebar(controller.signal)
    if (seq !== activeRefreshSeq) return
    clearTimeout(activeRefreshRetry)
    activeRefreshRetry = null
    commitActiveSidebar(body, epoch)
    if (explorer.sidebarBootState !== 'ready') explorer.sidebarBootState = 'ready'
  } catch (error) {
    if (seq !== activeRefreshSeq || error?.name === 'AbortError') return
    // The list is still owed: the sequence it followed is void, and the read
    // is retried until one lands.
    sidebarRevision = 0
    clearTimeout(activeRefreshRetry)
    activeRefreshRetry = setTimeout(() => {
      activeRefreshRetry = null
      scheduleActiveRefresh()
    }, ACTIVE_REFRESH_RETRY_MS)
    if (!explorer.activeItems.length) {
      explorer.sidebarBootState = 'error'
      explorer.listError = error?.message ?? String(error)
    }
  }
}

export function requestFullHistory() {
  if (explorer.datasetComplete && explorer.datasetScope === 'window') return
  loadDataset()
}

export async function loadParentHistory(row) {
  const id = row?.id
  if (!id) return
  const current = explorer.historyScopes[id]
  if (current?.state === 'loading') return
  if (current?.state === 'ready') return
  await readHistoryScope(id)
}

// readHistoryScope reads one parent's inactive descendants, keeping the rows
// already shown while it reads. The newest read of a scope wins.
const historyReads = new Map()
async function readHistoryScope(id) {
  const current = explorer.historyScopes[id]
  const read = (historyReads.get(id) ?? 0) + 1
  historyReads.set(id, read)
  explorer.historyScopes = {
    ...explorer.historyScopes,
    [id]: { ...(current ?? { items: [] }), state: 'loading', error: '' },
  }
  try {
    const items = []
    let cursor = null
    do {
      const params = new URLSearchParams()
      params.set('limit', '100')
      if (cursor) params.set('cursor', cursor)
      const res = await fetch(`/api/v1/sidebar/threads/${encodeURIComponent(id)}/history?${params}`, { cache: 'no-store' })
      if (!res.ok) throw await apiError(res)
      const page = await res.json()
      items.push(...(page.items ?? []))
      cursor = page.nextCursor ?? null
    } while (cursor)
    if (historyReads.get(id) !== read) return
    explorer.historyScopes = {
      ...explorer.historyScopes,
      [id]: { items, nextCursor: null, state: 'ready', error: '' },
    }
    rebuildRenderDataset()
  } catch (error) {
    if (historyReads.get(id) !== read) return
    explorer.historyScopes = {
      ...explorer.historyScopes,
      [id]: { items: current?.items ?? [], nextCursor: current?.nextCursor ?? null, state: 'error', error: error?.message ?? String(error) },
    }
  }
}

// Fetch every page of the current window off-screen, then replace the dataset
// once. Counts and filters must never observe a partial background refresh.
export async function loadDataset() {
  // A complete window supersedes any row repairs that were queued before it
  // started. Catalog changes arriving after this point queue separately and
  // run once the window commits.
  pendingCatalogThreadIDs.clear()
  pendingCatalogThreadVersion = null
  catalogPatchAbort?.abort()
  catalogPatchAbort = null
  listAbort?.abort()
  listAbort = new AbortController()
  const seq = ++listSeq
  const signal = listAbort.signal
  windowAnchor ??= new Date()
  const wasComplete = explorer.datasetComplete
  explorer.listState = 'loading'
  explorer.listError = null
  explorer.datasetComplete = false
  runtimeDirty = false // this load fetches fresh runtime overlays too
  try {
    let cursor = null
    let nextDataset = []
    let nextCatalogVersion = null
    do {
      const page = await fetchThreads(cursor, signal, windowAnchor)
      if (seq !== listSeq) return // superseded while resolving
      // Build the replacement off-screen. Sidebar counts derive directly from
      // dataset, so exposing each page would make the counts collapse and grow
      // during every background refresh.
      nextDataset = mergePages(nextDataset, page.items ?? [])
      nextCatalogVersion = page.catalogVersion ?? nextCatalogVersion
      cursor = page.nextCursor ?? null
    } while (cursor)
    explorer.windowItems = nextDataset
    explorer.catalogVersion = nextCatalogVersion
    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    explorer.datasetSource = 'catalog'
    explorer.listState = 'idle'
    rebuildRenderDataset()
  } catch (e) {
    if (seq !== listSeq || e?.name === 'AbortError') return
    // A failed background refresh must not turn the last complete window into
    // a partial one. Keep serving the old dataset and report the failure.
    explorer.datasetComplete = wasComplete
    explorer.listState = 'error'
    explorer.listError = e.message ?? String(e)
  } finally {
    if (seq === listSeq) {
      listAbort = null
      // A version pushed while this load was in flight was deferred, not
      // dropped: catch up now if the response didn't already cover it. Without
      // this, the last bump of a burst (the final message of a turn) leaves
      // the list stale until unrelated activity bumps the version again.
      reconcileVersion(deferredVersion)
      // Same deferral for runtime nudges: if one arrived mid-flight and the
      // version reconcile didn't already start a catch-up load, run one now.
      if (runtimeDirty && !listAbort) scheduleDatasetRefresh()
      scheduleCatalogPatch()
    }
  }
}

// Fetch-time parameters change what the server sends: refetch the window.
export function setFetchParam(patch) {
  explorer.fetchParams = { ...explorer.fetchParams, ...patch }
  windowAnchor = null
  clearSelection()
  loadDataset()
}

// View parameters (sort, facet selections, date ranges) are local: the
// component re-derives the visible list from the in-memory dataset — no
// request, no latency.
export function setView(patch) {
  explorer.view = { ...explorer.view, ...patch }
}

export async function select(id) {
  detailAbort?.abort()
  detailAbort = new AbortController()
  const seq = ++detailSeq
  explorer.selectedId = id
  explorer.detail = null
  explorer.detailState = 'loading'
  explorer.detailError = null
  loadTranscript(id)
  try {
    const res = await fetch(`/api/v1/threads/${encodeURIComponent(id)}`, { signal: detailAbort.signal })
    if (!res.ok) throw await apiError(res)
    const body = await res.json()
    if (seq !== detailSeq || explorer.selectedId !== id) return // stale response for a superseded selection
    explorer.detail = body
    explorer.detailState = 'idle'
  } catch (e) {
    if (seq !== detailSeq || e?.name === 'AbortError' || explorer.selectedId !== id) return
    explorer.detailState = 'error'
    explorer.detailError = e.message ?? String(e)
  } finally {
    if (seq === detailSeq) detailAbort = null
  }
}

// Legacy selection toggle retained for non-UI callers. The active UI uses
// openThread(), which always stages history in the main workspace.
export function toggleSelect(id) {
  if (explorer.selectedId === id) closeDetail()
  else select(id)
}

export function closeDetail() {
  clearSelection()
}

function clearSelection() {
  detailAbort?.abort()
  detailAbort = null
  detailSeq += 1
  closeTranscript()
  explorer.selectedId = null
  explorer.detail = null
  explorer.detailState = 'idle'
  explorer.detailError = null
}

let refreshTimer = null
export async function requestRefresh() {
  try {
    const res = await fetch('/api/v1/thread-index/refresh', { method: 'POST' })
    if (!res.ok) throw await apiError(res)
    // A successful POST resolves a prior refresh failure; a pending status
    // error still needs a successful poll to clear.
    if (explorer.indexError?.op === 'refresh') explorer.indexError = null
    explorer.refreshRequested = true
    clearTimeout(refreshTimer)
    refreshTimer = setTimeout(() => { explorer.refreshRequested = false }, REFRESH_FEEDBACK_MS)
  } catch (e) {
    explorer.indexError = { op: 'refresh', message: e.message ?? String(e) }
  }
}

// --- Rename write path (Slice E) -------------------------------------------
// PUT/DELETE the user-assigned name straight to nano's truth file. On success
// the SSE catalog chain refreshes the whole dataset within ~a second; we also
// patch the single edited row (and the open detail) from the AUTHORITATIVE
// response record — never local input text — so the rename shows without lag.

export async function renameThread(id, name) {
  const res = await fetch(`/api/v1/threads/${encodeURIComponent(id)}/name`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  })
  if (!res.ok) throw await apiError(res)
  const body = await res.json()
  const record = body?.name ?? null
  patchThreadName(id, record)
  return record
}

export async function clearThreadName(id) {
  const res = await fetch(`/api/v1/threads/${encodeURIComponent(id)}/name`, { method: 'DELETE' })
  if (!res.ok) throw await apiError(res)
  patchThreadName(id, null)
}

// Patch one thread's name overlay from the API response record (or null on
// clear). The SSE refetch reconciles the rest of the dataset shortly after.
function patchThreadName(id, record) {
  const patch = (row) => ({ ...row, name: record })
  patchStoredRows([id], patch)
  const idx = explorer.dataset.findIndex((item) => item.id === id)
  if (idx >= 0) {
    const next = explorer.dataset.slice()
    next[idx] = patch(next[idx])
    explorer.dataset = next
  }
  if (explorer.detail?.id === id) explorer.detail = { ...explorer.detail, name: record }
}

// --- Catalog liveness: SSE push, poll fallback -----------------------------
// An EventSource carries catalog_version changes and runtime overlays. An
// ordinary catalog update names the affected thread, which the browser fetches
// and patches in place. Structural changes and reconnect repair reload the
// complete window. Runtime events use their own compact overlay path. The
// status poll is demoted to a fallback that runs only while the stream is
// broken, plus one check on tab-refocus.
//
// The stream runs for the app's lifetime, not the Explorer view's: App.svelte
// starts it at boot because the sidebar is a projection of this dataset too, so
// a view switch must never close it. startPolling/stopPolling keep their names;
// startPolling is idempotent (the `streaming` guard) and stopPolling exists for
// teardown and tests.
//
// Poll retirement (unified-thread-catalog.md, item 5): the sidebar's session
// list used to run its own 5s /api/sessions poll. It now refreshes on the
// same runtime nudge that refreshes this catalog window instead — one push
// signal, two consumers — via addRuntimeListener below. app.svelte.js's
// refreshSessions rides that seam; /api/sessions keeps a slow fallback poll
// of its own for a broken stream (see startPolling in app.svelte.js).

let streaming = false
let source = null
let streamOpen = false
let pollTimer = null
let reopenTimer = null

// Runtime-listener registry: the sidebar's session list rides the same
// payload-free `runtime` nudge that refreshes this catalog window, so it
// needs no poll of its own (app.svelte.js registers refreshSessions here).
// addRuntimeListener returns an unsubscribe function for teardown/tests.
const runtimeListeners = new Set()
const streamOpenListeners = new Set()

export function catalogStreamOpen() {
  return streamOpen
}

export function addRuntimeListener(fn) {
  runtimeListeners.add(fn)
  return () => runtimeListeners.delete(fn)
}

// setRuntimePosition registers where the session list stands in the runtime
// stream (app.svelte.js). Each connect names it, and the server first replays
// the runtime deltas after it, or a refresh when it cannot (a Nano restart).
let runtimePosition = null
export function setRuntimePosition(fn) {
  runtimePosition = fn
}

// Stream-open listeners run on every open, the first and each reconnect.
export function addStreamOpenListener(fn) {
  streamOpenListeners.add(fn)
  return () => streamOpenListeners.delete(fn)
}

function scanning() {
  const s = explorer.index?.state
  return s === 'scanning' || s === 'starting'
}

// The newest version seen from any signal (SSE event or status fetch) while a
// window load was in flight. Checked again when that load completes.
let deferredVersion = null

// A runtime nudge that arrived while a window load was in flight. Runtime
// overlays are volatile and unversioned (outside catalog_version), so a nudge
// always warrants a refetch — deferred, never dropped, like version bumps.
let runtimeDirty = false
let refreshMicrotaskPending = false
let catalogPatchAbort = null
let catalogPatchMicrotaskPending = false
let pendingCatalogThreadVersion = null
const pendingCatalogThreadIDs = new Set()

function scheduleDatasetRefresh() {
  if (explorer.datasetScope !== 'window') {
    scheduleActiveRefresh()
    return
  }
  if (listAbort || refreshMicrotaskPending) return
  refreshMicrotaskPending = true
  queueMicrotask(() => {
    refreshMicrotaskPending = false
    if (!listAbort) loadDataset()
  })
}

function activeSetHas(id) {
  return (explorer.activeItems ?? []).some((row) => row.id === id)
}

function storedSetHas(id) {
  if (activeSetHas(id) || (explorer.windowItems ?? []).some((row) => row.id === id)) return true
  return Object.values(explorer.historyScopes ?? {}).some((scope) => (
    scope?.items?.some((row) => row.id === id)
  ))
}

function patchStoredRowsWhere(matches, patch) {
  const apply = (rows) => rows.map((row) => (matches(row) ? patch(row) : row))
  explorer.activeItems = apply(explorer.activeItems)
  explorer.windowItems = apply(explorer.windowItems)
  const scopes = { ...explorer.historyScopes }
  let scopesChanged = false
  for (const [key, scope] of Object.entries(scopes)) {
    if (!scope?.items?.some(matches)) continue
    scopes[key] = { ...scope, items: apply(scope.items) }
    scopesChanged = true
  }
  if (scopesChanged) explorer.historyScopes = scopes
}

function patchStoredRows(ids, patch) {
  const idSet = new Set(ids)
  patchStoredRowsWhere((row) => idSet.has(row.id), patch)
}

// A quick-detail placement write must update the stores that rebuild dataset,
// not only dataset itself. Otherwise the next unrelated sidebar rebuild can
// briefly restore the old invoker edge until a full refresh lands.
export function patchSidebarPlacement(hostedThreadId, sidebarPlacement) {
  if (!hostedThreadId) return
  const matches = (row) => row?.hostedThreadId === hostedThreadId
  const patch = (row) => ({ ...row, sidebarPlacement })
  patchStoredRowsWhere(matches, patch)
  explorer.dataset = explorer.dataset.map((row) => (matches(row) ? patch(row) : row))
  if (matches(explorer.detail)) explorer.detail = patch(explorer.detail)
}

function reconcileVersion(version) {
  // Versions are monotonic; only strictly newer than the dataset's recorded
  // version warrants a refetch (a stale deferred value must not re-load).
  if (version == null || version <= (explorer.catalogVersion ?? -1)) return
  if (listAbort) {
    deferredVersion = version // defer, never drop — resolved on load completion
    return
  }
  deferredVersion = null
  scheduleDatasetRefresh() // silent window refetch; local view state is untouched
}

function queueCatalogThreadPatches(threadIDs, version) {
  if (!Array.isArray(threadIDs) || threadIDs.length === 0) {
    if (explorer.datasetScope === 'window') reconcileVersion(version)
    return
  }
  if (version == null || version <= (explorer.catalogVersion ?? -1)) return
  const normalized = threadIDs
    .filter((id) => typeof id === 'string' && id.trim())
    .map((id) => id.trim())
  const patchIDs = explorer.datasetScope === 'window'
    ? normalized
    : normalized.filter((id) => storedSetHas(id))
  for (const id of patchIDs) {
    pendingCatalogThreadIDs.add(id)
  }
  if (!patchIDs.length) return
  pendingCatalogThreadVersion = Math.max(pendingCatalogThreadVersion ?? -1, version)
  scheduleCatalogPatch()
}

function scheduleCatalogPatch() {
  if (listAbort || catalogPatchAbort || catalogPatchMicrotaskPending || pendingCatalogThreadIDs.size === 0) return
  catalogPatchMicrotaskPending = true
  queueMicrotask(() => {
    catalogPatchMicrotaskPending = false
    if (!listAbort && !catalogPatchAbort && pendingCatalogThreadIDs.size > 0) void flushCatalogThreadPatches()
  })
}

async function fetchCatalogThreadPatch(id, signal) {
  // Keep the current window bound. The public exact-search helper normally
  // drops it so an operator can find old threads, but a background patch must
  // not pull an old row into the 30-day sidebar merely because its source file
  // changed.
  const query = new URLSearchParams(buildWindowQuery({ ...explorer.fetchParams, threadId: '' }, null, windowAnchor ?? new Date()))
  query.set('threadId', id)
  const url = `/api/v1/threads?${query}`
  const res = await fetch(url, { signal })
  if (!res.ok) throw await apiError(res)
  return await res.json()
}

async function flushCatalogThreadPatches() {
  const ids = [...pendingCatalogThreadIDs]
  const requestedVersion = pendingCatalogThreadVersion
  pendingCatalogThreadIDs.clear()
  pendingCatalogThreadVersion = null
  const controller = new AbortController()
  catalogPatchAbort = controller
  try {
    const pages = await Promise.all(ids.map((id) => fetchCatalogThreadPatch(id, controller.signal)))
    if (catalogPatchAbort !== controller) return
    let next = explorer.windowItems.filter((item) => !ids.includes(item.id))
    for (const page of pages) {
      next = mergePages(next, page.items ?? [])
    }
    explorer.windowItems = next
    const patched = pages.flatMap((page) => page.items ?? [])
    const byId = new Map(patched.map((item) => [item.id, item]))
    // The read returns the whole current row, so it replaces the old one: a
    // field it leaves out (a runtime overlay, a fleet label it could not
    // classify) is gone, never kept under the new stamp. Only the sidebar
    // block carries over: this read does not supply it, and it is structure,
    // not turn state. Every other field the browser writes on a row (name,
    // star, placement, assignment, presentation, label, runtime) is an early
    // copy of a server field this row now supplies.
    const mergePatch = (row) => {
      const incoming = byId.get(row.id)
      if (!incoming) return row
      return { ...incoming, sidebar: incoming.sidebar ?? row.sidebar }
    }
    explorer.activeItems = explorer.activeItems.map(mergePatch)
    const scopes = { ...explorer.historyScopes }
    let scopesChanged = false
    for (const [key, scope] of Object.entries(scopes)) {
      if (!scope?.items?.length) continue
      if (!scope.items.some((row) => byId.has(row.id))) continue
      scopes[key] = { ...scope, items: scope.items.map(mergePatch) }
      scopesChanged = true
    }
    if (scopesChanged) explorer.historyScopes = scopes
    if (explorer.datasetScope !== 'window' && ids.some((id) => !byId.has(id))) {
      scheduleActiveRefresh()
    }
    rebuildRenderDataset()
    // The exact-row response can observe a newer global version than this
    // event. Only advance through the version whose named identities we have
    // actually repaired; later events must still patch their own rows.
    explorer.catalogVersion = requestedVersion
  } catch (e) {
    if (e?.name !== 'AbortError') reconcileVersion(requestedVersion)
  } finally {
    if (catalogPatchAbort === controller) catalogPatchAbort = null
    scheduleCatalogPatch()
  }
}

// While a history view is open, a catalog bump also nudges the selected thread's
// transcript to follow live activity (Slice G). No modal → zero cost. The
// transcript store coalesces overlapping nudges into one in-flight tail fetch.
// threadIds names the threads that changed; null means any may have.
function nudgeOpenTranscript(threadIds = null) {
  if (explorer.selectedId && (!threadIds || threadIds.includes(explorer.selectedId))) nudgeTranscriptTail()
  nudgeTabHistories(threadIds)
}

// fetchStatus refreshes the index-status strip and reconciles the dataset
// version. It is the shared body of both a catalog event and a fallback poll,
// so it always reconciles — the poll is the only version signal while the
// stream is down.
async function fetchStatus(reconcileCatalog = true) {
  try {
    const res = await fetch('/api/v1/thread-index/status')
    if (!res.ok) throw await apiError(res)
    const status = await res.json()
    explorer.index = status
    // A successful fetch resolves only status-op errors. A refresh failure must
    // not be wiped here — the operator's requested refresh never happened; only
    // a successful re-POST (or Retry) clears it.
    if (explorer.indexError?.op === 'status') explorer.indexError = null
    if (scanning()) explorer.refreshRequested = false // a real scan supersedes the button feedback
    if (reconcileCatalog) reconcileVersion(status.catalogVersion)
  } catch (e) {
    // Don't clobber a pending refresh error with a status error; the refresh
    // failure is the operator-facing one until it is resolved.
    if (explorer.indexError?.op !== 'refresh') {
      explorer.indexError = { op: 'status', message: e.message ?? String(e) }
    }
  }
}

function scheduleFallbackPoll() {
  clearTimeout(pollTimer)
  if (!streaming || streamOpen) return // a healthy (or absent) stream needs no timer
  pollTimer = setTimeout(runFallbackPoll, scanning() ? SCAN_POLL_MS : IDLE_POLL_MS)
}

async function runFallbackPoll() {
  if (!streaming || streamOpen) return
  await fetchStatus()
  nudgeOpenTranscript() // a broken stream still follows an open transcript, at poll cadence
  scheduleFallbackPoll()
}

function onStreamOpen() {
  streamOpen = true
  const afterDrop = !explorer.link.up
  explorer.link = railLink(explorer.link, 'opened')
  // After a drop every shown row is last-seen (lib/status/freshness.js) until a read
  // started now replaces it: the active sidebar below, the window when the rail
  // holds one, and each history scope that has rows to show, whatever its last
  // read did (a failed or unanswered read keeps its rows on screen).
  if (afterDrop) {
    if (explorer.datasetScope === 'window') loadDataset()
    for (const [id, scope] of Object.entries(explorer.historyScopes ?? {})) {
      if (scope?.items?.length) void readHistoryScope(id)
    }
  }
  clearTimeout(pollTimer) // stream recovered → stop the fallback poll
  // Mail deltas are not replayed; the reload below re-stamps fresh counts on
  // every row, so live entries from before the disconnect must not override.
  resetMailCounts()
  // A drop ended the revision sequence (onStreamError), so the list is read
  // again and that read starts the next one. On a page's first open the
  // bootstrap read already did, and the server's connect marker reads only
  // if the revision moved meanwhile.
  if (!sidebarRevision) scheduleActiveRefresh()
  for (const fn of streamOpenListeners) {
    try {
      fn()
    } catch {
      // isolate one consumer from the recovered catalog stream
    }
  }
}

function onCatalogEvent(event) {
  let payload = null
  try {
    payload = JSON.parse(event.data)
  } catch {
    payload = null
  }
  if (payload?.refresh || !Array.isArray(payload?.threadIds) || payload.threadIds.length === 0) {
    if (explorer.datasetScope === 'window') reconcileVersion(payload?.catalogVersion ?? null)
  } else {
    queueCatalogThreadPatches(payload.threadIds, payload.catalogVersion ?? null)
  }
  // The event already carries the exact repair instruction. Refresh the status
  // strip without turning its same version back into a full-window reload.
  fetchStatus(false)
  // and follow the open transcripts of the threads it names
  nudgeOpenTranscript(payload?.refresh || !payload?.threadIds?.length ? null : payload.threadIds)
}

// The active sidebar follows the server's materialized state by `sidebar`
// events: each carries the rows that changed and the revision that results.
// A revision that follows the list's own is applied; a refresh marker at the
// list's revision means nothing was missed; anything else (a gap, a marker
// after a conflated burst, a Nano restart) is closed by one full read. Until
// this, every runtime and catalog event re-read the whole list instead.
function onSidebarEvent(event) {
  let payload = null
  try {
    payload = JSON.parse(event.data)
  } catch {
    payload = null
  }
  const revision = Number(payload?.revision) || 0
  if (!revision || !sidebarRevision || String(payload.generation ?? '') !== sidebarGeneration) {
    scheduleActiveRefresh()
    return
  }
  if (payload.refresh) {
    if (revision !== sidebarRevision) scheduleActiveRefresh()
    return
  }
  if (revision === sidebarRevision) return
  if (revision !== sidebarRevision + 1) {
    scheduleActiveRefresh()
    return
  }
  applySidebarDelta(payload)
}

// Runtime attach/detach/turnActive flips push a compact `runtime` delta on the
// same stream (Slice R1). Ordinary turn-state changes patch the visible window
// in place. Identity changes refetch the catalog window; the active sidebar
// follows its own `sidebar` events.
function onRuntimeEvent(event) {
  let payload = null
  try {
    payload = JSON.parse(event.data)
  } catch {
    payload = null
  }
  for (const fn of runtimeListeners) {
    try {
      fn(payload)
    } catch {
      // isolate a listener failure from the window refetch below
    }
  }
  const hasDelta = Array.isArray(payload?.rows)
  const runtimeDecision = hasDelta ? patchRuntimeRows(payload.rows) : { changed: false, refresh: Boolean(payload?.refresh) }
  if (hasDelta && !payload.refresh && !runtimeDecision.refresh) return
  if (explorer.datasetScope !== 'window') return
  if (listAbort) {
    runtimeDirty = true // defer, never drop — resolved on load completion
    return
  }
  scheduleDatasetRefresh()
}

function patchRuntimeRows(rows) {
  const byKey = new Map()
  for (const row of rows) {
    if (!row?.provider || !row.nativeId) continue
    byKey.set(`${row.provider}\u0000${row.nativeId}`, row.runtime ?? null)
  }
  if (!explorer.dataset.length) return { changed: false, refresh: true }
  let changed = false
  let attachedToAbsent = false
  let unknownIdentity = false
  const known = new Set(explorer.dataset.map((item) => `${item.provider ?? ''}\u0000${item.nativeId ?? ''}`))
  for (const key of byKey.keys()) {
    if (!known.has(key)) unknownIdentity = true
  }
  const next = explorer.dataset.map((item) => {
    const key = `${item.provider ?? ''}\u0000${item.nativeId ?? ''}`
    // A delta names only the threads whose runtime changed; a named null is
    // a thread Nano no longer holds.
    if (!byKey.has(key)) return item
    const runtime = byKey.get(key)
    if (runtime) {
      if (item.runtime?.attached && runtime.attached === false) attachedToAbsent = true
      if (JSON.stringify(item.runtime ?? null) === JSON.stringify(runtime)) return item
      changed = true
      return { ...item, runtime }
    }
    if (!item.runtime) return item
    if (item.runtime.attached) attachedToAbsent = true
    changed = true
    const copy = { ...item }
    delete copy.runtime
    return copy
  })
  if (changed) {
    explorer.dataset = next
    const byId = new Map(next.map((row) => [row.id, row]))
    explorer.activeItems = explorer.activeItems.map((row) => byId.get(row.id) ?? row)
    explorer.windowItems = explorer.windowItems.map((row) => byId.get(row.id) ?? row)
  }
  return { changed, refresh: Boolean(attachedToAbsent || unknownIdentity) }
}

// Fleet transitions include durable worker working↔idle/stale changes. They
// are unversioned like runtime overlays, so re-read the window immediately;
// unlike runtime events they do not imply /api/sessions changed. An open
// worker transcript also gets a tail nudge so the selected pane follows it.
function onFleetEvent(event) {
  let transition = null
  try {
    transition = JSON.parse(event?.data)
  } catch {
    transition = null
  }
  const index = transition?.threadId
    ? explorer.dataset.findIndex((row) => row.id === transition.threadId)
    : -1
  const fromActive = ACTIVE_FLEET_LABELS.has(transition?.from)
  const toActive = ACTIVE_FLEET_LABELS.has(transition?.to)
  const oneSidedActive = Boolean(fromActive) !== Boolean(toActive)
  if (index >= 0 && transition?.to) {
    const relabel = (row) => ({ ...row, fleet: { ...(row.fleet ?? {}), label: transition.to } })
    const next = explorer.dataset.slice()
    next[index] = relabel(next[index])
    explorer.dataset = next
    patchStoredRows([transition.threadId], relabel)
  }
  if (index < 0 || oneSidedActive) scheduleActiveRefresh()
  if (explorer.datasetScope === 'window' && index < 0) {
    if (listAbort) runtimeDirty = true
    else scheduleDatasetRefresh()
  }
  nudgeOpenTranscript(transition?.threadId ? [transition.threadId] : null)
}

// Mail count deltas carry each changed thread's complete current
// counts plus the global summary — pure last-value-wins state for the sidebar
// badges and any open inbox panel. No window refetch: catalog rows re-stamp
// their own mail overlay on the next ordinary reload.
function onMailEvent(event) {
  let payload = null
  try {
    payload = JSON.parse(event.data)
  } catch {
    payload = null
  }
  if (payload) applyMailDelta(payload)
}

function onTranscriptAuditEvent(event) {
  try {
    dispatchTranscriptAuditRequest(JSON.parse(event.data))
  } catch {
    // A malformed diagnostic request must not disturb catalog liveness.
  }
}

function onStreamError() {
  // EventSource auto-reconnects; until it reopens, fall back to polling.
  streamOpen = false
  explorer.link = railLink(explorer.link, 'dropped')
  // The revision sequence the active list followed ends here: a restarted
  // Nano counts from 1 again, so a marker at the old number proves nothing.
  sidebarRevision = 0
  scheduleFallbackPoll()
  reopenTimer ??= reopenIfClosed(source, () => {
    reopenTimer = null
    if (!streaming) return
    closeStream()
    openStream()
  })
}

// Every event on this stream is an answer from Nano (connection.svelte.js).
const CONTACT_EVENTS = ['open', 'catalog', 'sidebar', 'runtime', 'fleet', 'mail', 'transcript-audit']

function openStream() {
  const EventSourceCtor = globalThis.EventSource
  if (!EventSourceCtor) {
    // No EventSource (e.g. server-side or a stripped runtime): poll instead.
    streamOpen = false
    scheduleFallbackPoll()
    return
  }
  const position = runtimePosition?.()
  source = new EventSourceCtor(position
    ? `${CATALOG_EVENTS_URL}?${new URLSearchParams({ runtimeGeneration: position.generation, runtimeSeq: String(position.seq) })}`
    : CATALOG_EVENTS_URL)
  source.addEventListener('open', onStreamOpen)
  source.addEventListener('catalog', onCatalogEvent)
  source.addEventListener('sidebar', onSidebarEvent)
  source.addEventListener('runtime', onRuntimeEvent)
  source.addEventListener('fleet', onFleetEvent)
  source.addEventListener('mail', onMailEvent)
  source.addEventListener('transcript-audit', onTranscriptAuditEvent)
  source.addEventListener('error', onStreamError)
  for (const type of CONTACT_EVENTS) source.addEventListener(type, noteNanoContact)
}

function closeStream() {
  if (!source) return
  source.removeEventListener('open', onStreamOpen)
  source.removeEventListener('catalog', onCatalogEvent)
  source.removeEventListener('sidebar', onSidebarEvent)
  source.removeEventListener('runtime', onRuntimeEvent)
  source.removeEventListener('fleet', onFleetEvent)
  source.removeEventListener('mail', onMailEvent)
  source.removeEventListener('transcript-audit', onTranscriptAuditEvent)
  source.removeEventListener('error', onStreamError)
  for (const type of CONTACT_EVENTS) source.removeEventListener(type, noteNanoContact)
  source.close()
  source = null
}

function onVisibility() {
  if (document.visibilityState !== 'visible') return
  fetchStatus() // one check on refocus
}

// Manual retry dispatches on the failed operation: a failed refresh POST is
// re-issued; a failed status fetch is retried immediately.
export function retryIndexError() {
  if (explorer.indexError?.op === 'refresh') {
    requestRefresh()
  } else {
    fetchStatus()
  }
}

export function startPolling() {
  if (streaming) return
  streaming = true
  streamOpen = false
  document.addEventListener('visibilitychange', onVisibility)
  openStream()
}

export function stopPolling() {
  streaming = false
  streamOpen = false
  clearTimeout(pollTimer)
  clearTimeout(reopenTimer)
  reopenTimer = null
  document.removeEventListener('visibilitychange', onVisibility)
  closeStream()
}
