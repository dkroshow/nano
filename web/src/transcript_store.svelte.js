import {
  TRANSCRIPT_CACHE_BYTES,
	TRANSCRIPT_BODY_CACHE_BYTES,
  canonicalHiddenCategories,
  defaultHiddenCategories,
  mergeTranscriptItems,
  normalizeTranscriptView,
  transcriptEvidenceURL,
	transcriptBodyURL,
  transcriptProfileKey,
  transcriptURL,
} from './transcript_query.js'

export const transcript = $state({
  threadId: null,
  items: [],
  turns: [],
  page: null,
  projection: null,
  state: 'idle', // idle | loading | ready | empty | pending | degraded | retained | error
  error: null,
  earlierError: null,
  loadingEarlier: false,
  view: 'enhanced',
  hiddenCategories: defaultHiddenCategories('enhanced'),
  taxonomy: null,
  evidence: {},
	body: {},
	newerAvailable: false,
})

const cache = new Map()
let cacheBytes = 0
let requestAbort = null
let earlierAbort = null
// Live tail follow (Slice G): one tail fetch runs at a time; nudges that arrive
// while it is in flight set tailDirty, which drives exactly one follow-up fetch.
let tailAbort = null
let tailDirty = false
const evidenceControllers = new Map()
const bodyControllers = new Map()
const activeBodyJobs = new Map()
const bodyCache = new Map()
let bodyCacheBytes = 0
let bodyCacheLimit = TRANSCRIPT_BODY_CACHE_BYTES
const bodyQueue = []
let activeBodyLoads = 0
let bodyGeneration = 0
const MAX_BODY_LOADS = 4
let requestSeq = 0

async function apiError(response) {
  try {
    const body = await response.json()
    const error = new Error(body?.error?.message || `request failed (${response.status})`)
    error.code = body?.error?.code
    error.status = response.status
    return error
  } catch {
    const error = new Error(`request failed (${response.status})`)
    error.status = response.status
    return error
  }
}

function cacheKey(threadId, view, hiddenCategories) {
  return `${threadId}\u0000${transcriptProfileKey(view, hiddenCategories)}`
}

function touchCache(key, value, bytes) {
  const prior = cache.get(key)
  if (prior) cacheBytes -= prior.bytes
  cache.delete(key)
  if (bytes > TRANSCRIPT_CACHE_BYTES) return
  cache.set(key, { ...value, bytes })
  cacheBytes += bytes
  while (cacheBytes > TRANSCRIPT_CACHE_BYTES && cache.size > 1) {
    const [oldestKey, oldest] = cache.entries().next().value
    cache.delete(oldestKey)
    cacheBytes -= oldest.bytes
  }
}

function apply(threadId, value) {
  if (transcript.threadId !== threadId) return
	const priorProjection = transcript.projection?.key ?? transcript.projection?.epoch
	const nextProjection = value.projection?.key ?? value.projection?.epoch
	if (priorProjection && nextProjection && priorProjection !== nextProjection) {
		abortEvidenceRequests()
		abortBodyRequests()
		transcript.evidence = {}
		transcript.body = {}
	}
	discardObsoleteBodyStates(value.items ?? [], value.projection ?? null)
  transcript.items = value.items ?? []
  transcript.turns = value.turns ?? []
  transcript.page = value.page ?? null
  transcript.projection = value.projection ?? null
  transcript.taxonomy = value.taxonomy ?? null
  if (value.view) transcript.view = normalizeTranscriptView(value.view)
  if (Array.isArray(value.hiddenCategories)) {
    transcript.hiddenCategories = canonicalHiddenCategories(value.hiddenCategories)
  }
  transcript.state = value.state ?? (transcript.items.length ? 'ready' : 'empty')
  transcript.error = null
}

function stateFrom(body) {
  const state = body?.projection?.state
  if (state && state !== 'ready') return state
  return body?.items?.length ? 'ready' : 'empty'
}

function mergeRevalidatedTail(cached, body) {
  const cachedKey = cached?.projection?.epoch ?? cached?.projection?.key
  const nextKey = body?.projection?.epoch ?? body?.projection?.key
  const firstTailSeq = body?.items?.[0]?.projectedSeq
  if (!cached || !cachedKey || cachedKey !== nextKey || firstTailSeq == null) return body
  // A matching projection epoch does not prove these two windows touch. After
  // reconnect the provider-backed tail may have advanced beyond the browser's
  // cached prefix. Without one shared durable row, retaining that prefix would
  // invent continuity and could strand the intervening rows behind its stale
  // cursor (or stale hasMoreBefore=false). Adopt the refreshed page and cursor;
  // ordinary backward paging can then recover every earlier window.
  const refreshedIDs = new Set((body.items ?? []).map((item) => item?.id).filter(Boolean))
  const overlaps = (cached.items ?? []).some((item) => item?.id && refreshedIDs.has(item.id))
  if (!overlaps) return body
  const retained = (cached.items ?? []).filter((item) => item.projectedSeq < firstTailSeq)
  if (retained.length === 0) return body
  return {
    ...body,
    items: mergeTranscriptItems(retained, body.items),
    turns: mergeTranscriptItems(cached.turns, body.turns),
    // The oldest loaded page owns the next backward cursor. Revalidating the
    // tail must not discard that paging position.
		page: {
			...(body.page ?? {}),
			startSeq: cached.page?.startSeq ?? body.page?.startSeq,
			hasMoreBefore: cached.page?.hasMoreBefore ?? body.page?.hasMoreBefore,
			beforeCursor: cached.page?.beforeCursor ?? cached.page?.nextBeforeCursor ?? body.page?.beforeCursor,
			nextBeforeCursor: cached.page?.nextBeforeCursor ?? cached.page?.beforeCursor ?? body.page?.nextBeforeCursor,
		},
  }
}

function abortEvidenceRequests() {
  for (const controller of evidenceControllers.values()) controller.abort()
  evidenceControllers.clear()
}

function abortBodyRequests() {
	bodyGeneration++
	for (const controller of bodyControllers.values()) controller.abort()
	bodyControllers.clear()
	for (const job of activeBodyJobs.values()) job.counted = false
	activeBodyJobs.clear()
	for (const job of bodyQueue.splice(0)) job.resolve?.()
	activeBodyLoads = 0
}

function bodyKeyFor(projectionValue, item) {
	const projection = projectionValue?.key ?? projectionValue?.epoch ?? ''
	return `${projection}\u0000${item.id}\u0000${item.contentRef ?? ''}`
}

function bodyKey(item) { return bodyKeyFor(transcript.projection, item) }

function discardObsoleteBodyStates(items, projection) {
	let next = null
	for (const [itemId, state] of Object.entries(transcript.body)) {
		const item = items.find((candidate) => candidate.id === itemId)
		const key = item ? bodyKeyFor(projection, item) : null
		if (state?.key === key) continue
		if (state?.key) cancelBodyKey(state.key)
		next ??= { ...transcript.body }
		delete next[itemId]
	}
	if (next) transcript.body = next
}

function currentBodyKey(itemId) {
	const item = transcript.items.find((candidate) => candidate.id === itemId)
	return item ? bodyKey(item) : `${transcript.projection?.key ?? transcript.projection?.epoch ?? ''}\u0000${itemId}\u0000manual`
}

function currentBodyItem(itemId) {
	return transcript.items.find((candidate) => candidate.id === itemId) ?? null
}

function releaseBodySlot(job) {
	if (!job.counted) return
	job.counted = false
	activeBodyLoads = Math.max(0, activeBodyLoads - 1)
	pumpBodyQueue()
}

function cancelBodyKey(key) {
	const controller = bodyControllers.get(key)
	if (controller) {
		controller.abort()
		bodyControllers.delete(key)
	}
	const active = activeBodyJobs.get(key)
	if (active) {
		activeBodyJobs.delete(key)
		releaseBodySlot(active)
	}
	for (let index = bodyQueue.length - 1; index >= 0; index--) {
		if (bodyQueue[index].key !== key) continue
		bodyQueue.splice(index, 1)[0].resolve?.()
	}
}

function clearVisibleCachedBody(entry) {
	const visible = transcript.body[entry.itemId]
	if (visible?.state !== 'ready' || visible.data !== entry.data) return
	const next = { ...transcript.body }
	delete next[entry.itemId]
	transcript.body = next
}

function touchBodyCache(key, data, itemId) {
	const bytes = new TextEncoder().encode(JSON.stringify(data)).byteLength
	const prior = bodyCache.get(key)
	if (prior) bodyCacheBytes -= prior.bytes
	bodyCache.delete(key)
	if (bytes > bodyCacheLimit) {
		clearVisibleCachedBody({ itemId, data })
		return
	}
	bodyCache.set(key, { data, bytes, itemId }); bodyCacheBytes += bytes
	while (bodyCacheBytes > bodyCacheLimit && bodyCache.size > 1) {
		const [oldestKey, oldest] = bodyCache.entries().next().value
		bodyCache.delete(oldestKey); bodyCacheBytes -= oldest.bytes
		clearVisibleCachedBody(oldest)
	}
}

export async function loadTranscript(threadId, {
  force = false,
  view = transcript.view,
  hiddenCategories = transcript.hiddenCategories,
} = {}) {
  const normalizedView = normalizeTranscriptView(view)
  const normalizedHidden = canonicalHiddenCategories(hiddenCategories)
  const profileKey = cacheKey(threadId, normalizedView, normalizedHidden)
  requestAbort?.abort()
  earlierAbort?.abort()
  tailAbort?.abort()
  abortEvidenceRequests()
	abortBodyRequests()
  earlierAbort = null
  tailAbort = null
  tailDirty = false // a fresh load fetches the newest window itself
  requestAbort = new AbortController()
  const seq = ++requestSeq
  transcript.threadId = threadId
  transcript.view = normalizedView
  transcript.hiddenCategories = normalizedHidden
  transcript.evidence = {}
	transcript.body = {}
	transcript.newerAvailable = false
  transcript.error = null
  transcript.earlierError = null
  transcript.loadingEarlier = false

  const cached = !force && cache.get(profileKey)
  if (cached) {
    apply(threadId, cached)
    cache.delete(profileKey)
    cache.set(profileKey, cached)
  } else {
    transcript.items = []
    transcript.turns = []
    transcript.page = null
    transcript.projection = null
    transcript.state = 'loading'
  }

  try {
    const response = await fetch(transcriptURL(threadId, null, {
      view: normalizedView,
      hiddenCategories: normalizedHidden,
    }), {
      signal: requestAbort.signal,
      headers: cached?.etag ? { 'If-None-Match': cached.etag } : {},
    })
    if (response.status === 304 && cached) return
    if (!response.ok) throw await apiError(response)
    const body = await response.json()
    if (seq !== requestSeq || transcript.threadId !== threadId) return
    const merged = mergeRevalidatedTail(cached, body)
    const value = { ...merged, state: stateFrom(merged), etag: response.headers?.get?.('etag') }
    // Cache weight follows the value we retain, not merely the newest HTTP
    // page. A revalidated tail may preserve many explicitly loaded older pages.
    touchCache(profileKey, value, new TextEncoder().encode(JSON.stringify(value)).byteLength)
    apply(threadId, value)
  } catch (error) {
    if (seq !== requestSeq || error?.name === 'AbortError' || transcript.threadId !== threadId) return
    transcript.state = 'error'
    transcript.error = error?.status === 409
      ? 'The transcript was re-indexed. Reload to see the current history.'
      : error?.message ?? String(error)
  } finally {
    if (seq === requestSeq) requestAbort = null
  }
}

// --- Live tail follow (Slice G) --------------------------------------------
// The SSE catalog nudge (explorer.svelte.js) calls this while a transcript is
// open. It fetches the tail window and appends only items newer than the last
// rendered one — already-rendered items are never re-created, reordered, or
// mutated, and a no-growth response touches nothing (no state churn, no scroll
// effect). One tail fetch runs at a time; nudges during a fetch mark dirty and
// yield exactly one follow-up. A projection reset, or a tail window that has
// slid entirely past the rendered tail, can't be merged item-for-item, so it
// repairs by cleanly reloading the tail window (the only case allowed to drop
// paged-back history).
export async function nudgeTranscriptTail() {
  if (!transcript.threadId) return // no modal open → zero cost
  if (tailAbort || requestAbort || transcript.loadingEarlier) {
    tailDirty = true // a fetch is already in flight; coalesce into one follow-up
    return
  }
  await runTailFetch()
}

async function runTailFetch() {
  const threadId = transcript.threadId
  const view = transcript.view
  const hiddenCategories = canonicalHiddenCategories(transcript.hiddenCategories)
  tailAbort = new AbortController()
  const controller = tailAbort
  try {
    const response = await fetch(transcriptURL(threadId, null, { view, hiddenCategories }), {
      signal: controller.signal,
    })
    if (!response.ok) throw await apiError(response)
    const body = await response.json()
    if (transcript.threadId !== threadId || transcript.view !== view) return
    mergeTail({ threadId, view, hiddenCategories, body, etag: response.headers?.get?.('etag') })
  } catch {
    // A transient tail-follow failure must not disturb the rendered transcript;
    // the next nudge retries. Aborts (thread switch, close) are ignored too.
  } finally {
    if (tailAbort === controller) tailAbort = null
    const followUp = tailDirty && transcript.threadId === threadId
    tailDirty = false
    if (followUp) void nudgeTranscriptTail()
  }
}

function mergeTail({ threadId, view, hiddenCategories, body, etag }) {
  const currentKey = transcript.projection?.epoch ?? transcript.projection?.key
  const nextKey = body?.projection?.epoch ?? body?.projection?.key
  // Item ids are projection-scoped: a rebuild/reset can't be spliced onto the
  // old ids, so reload the tail window instead of merging.
  if (currentKey && nextKey && currentKey !== nextKey) {
    return reloadTail({ threadId, view, hiddenCategories, body, etag })
  }
  const tail = body?.items ?? []
  if (transcript.items.length === 0) {
    // Nothing rendered yet (e.g. an empty transcript that just gained items) —
    // adopt the window wholesale.
    if (tail.length) reloadTail({ threadId, view, hiddenCategories, body, etag })
    return
  }
  if (tail.length === 0) return // no-op
  const lastSeq = transcript.items[transcript.items.length - 1].projectedSeq
  // The tail window is the newest bounded slice of visible items. If even its
  // oldest item is newer than everything rendered, the window has slid past our
  // tail and a merge would silently drop the items between — repair instead.
  if (tail[0].projectedSeq > lastSeq) {
    return reloadTail({ threadId, view, hiddenCategories, body, etag })
  }
  const fresh = tail.filter((item) => item.projectedSeq > lastSeq)
  if (fresh.length === 0) return // no growth → no state churn, no scroll effect
  const items = [...transcript.items, ...fresh] // existing item objects reused
  const turns = mergeTranscriptItems(transcript.turns, body.turns)
  // Advance the tail head only; keep the backward paging extent already loaded.
  const page = {
    ...transcript.page,
    endSeq: body.page?.endSeq ?? transcript.page?.endSeq,
    latestSeq: body.page?.latestSeq ?? transcript.page?.latestSeq,
    snapshotHeadSeq: body.page?.snapshotHeadSeq ?? transcript.page?.snapshotHeadSeq,
  }
  const value = { ...body, items, turns, page, state: stateFrom({ ...body, items }), etag }
  const profileKey = cacheKey(threadId, view, hiddenCategories)
  touchCache(profileKey, value, new TextEncoder().encode(JSON.stringify(value)).byteLength)
  apply(threadId, value)
}

function reloadTail({ threadId, view, hiddenCategories, body, etag }) {
  const value = { ...body, state: stateFrom(body), etag }
  const profileKey = cacheKey(threadId, view, hiddenCategories)
  touchCache(profileKey, value, new TextEncoder().encode(JSON.stringify(value)).byteLength)
  apply(threadId, value)
}

export async function loadEarlier() {
  const threadId = transcript.threadId
  const view = transcript.view
  const hiddenCategories = canonicalHiddenCategories(transcript.hiddenCategories)
  const profileKey = cacheKey(threadId, view, hiddenCategories)
  const cursor = transcript.page?.beforeCursor ?? transcript.page?.nextBeforeCursor
  if (!threadId || !cursor || transcript.loadingEarlier) return
  earlierAbort = new AbortController()
  const controller = earlierAbort
  transcript.loadingEarlier = true
  transcript.earlierError = null
  const seq = requestSeq
  try {
    const response = await fetch(transcriptURL(threadId, cursor, { view, hiddenCategories }), { signal: controller.signal })
    if (!response.ok) throw await apiError(response)
    const body = await response.json()
    if (seq !== requestSeq || transcript.threadId !== threadId) return
    const currentKey = transcript.projection?.epoch ?? transcript.projection?.key
    const nextKey = body?.projection?.epoch ?? body?.projection?.key
    if (currentKey && nextKey && currentKey !== nextKey) {
      await loadTranscript(threadId, { force: true })
      return
    }
    const value = {
      ...body,
      items: mergeTranscriptItems(body.items, transcript.items),
      turns: mergeTranscriptItems(body.turns, transcript.turns),
      state: stateFrom({ ...body, items: mergeTranscriptItems(body.items, transcript.items) }),
      etag: response.headers?.get?.('etag'),
    }
    const bytes = new TextEncoder().encode(JSON.stringify(value)).byteLength
    touchCache(profileKey, value, bytes)
    apply(threadId, value)
  } catch (error) {
    if (seq !== requestSeq || error?.name === 'AbortError' || transcript.threadId !== threadId) return
    if (error?.status === 409) await loadTranscript(threadId, { force: true })
    else transcript.earlierError = error?.message ?? String(error)
  } finally {
    if (earlierAbort === controller) earlierAbort = null
    if (seq === requestSeq && transcript.threadId === threadId) transcript.loadingEarlier = false
  }
}

export function setTranscriptView(view) {
  const normalizedView = normalizeTranscriptView(view)
  const hiddenCategories = defaultHiddenCategories(normalizedView)
  if (!transcript.threadId) {
    transcript.view = normalizedView
    transcript.hiddenCategories = hiddenCategories
    return
  }
  return loadTranscript(transcript.threadId, { view: normalizedView, hiddenCategories })
}

export function setTranscriptCategoryVisible(category, visible) {
  const hidden = new Set(transcript.hiddenCategories)
  if (visible) hidden.delete(category)
  else hidden.add(category)
  const hiddenCategories = canonicalHiddenCategories(hidden)
  if (!transcript.threadId) {
    transcript.hiddenCategories = hiddenCategories
    return
  }
  return loadTranscript(transcript.threadId, {
    view: transcript.view,
    hiddenCategories,
  })
}

export function showAllTranscriptCategories() {
  if (!transcript.threadId) {
    transcript.hiddenCategories = []
    return
  }
  return loadTranscript(transcript.threadId, {
    view: transcript.view,
    hiddenCategories: [],
  })
}

export async function loadTranscriptEvidence(itemId) {
  const threadId = transcript.threadId
  if (!threadId || !itemId) return
  const existing = transcript.evidence[itemId]
  if (existing?.state === 'loading' || existing?.state === 'ready') return

  // Native rows may be as large as 8 MiB. Keep exactly one disclosure body
  // resident so opening several Dev details cannot grow browser memory by one
  // maximum-sized row each.
  abortEvidenceRequests()
  const controller = new AbortController()
  evidenceControllers.set(itemId, controller)
  const projectionKey = transcript.projection?.key ?? transcript.projection?.epoch ?? null
  transcript.evidence = {
    [itemId]: { state: 'loading', data: null, error: null },
  }
  try {
    const response = await fetch(transcriptEvidenceURL(threadId, itemId), {
      signal: controller.signal,
      cache: 'no-store',
    })
    if (!response.ok) throw await apiError(response)
    const data = await response.json()
    if (evidenceControllers.get(itemId) !== controller || controller.signal.aborted ||
        transcript.threadId !== threadId ||
        (projectionKey && projectionKey !== (transcript.projection?.key ?? transcript.projection?.epoch))) return
    transcript.evidence = {
      [itemId]: { state: 'ready', data, error: null },
    }
  } catch (error) {
    if (evidenceControllers.get(itemId) !== controller || controller.signal.aborted ||
        error?.name === 'AbortError' || transcript.threadId !== threadId) return
    transcript.evidence = {
      [itemId]: { state: 'error', data: null, error: error?.message ?? String(error) },
    }
  } finally {
    if (evidenceControllers.get(itemId) === controller) evidenceControllers.delete(itemId)
  }
}

export async function loadTranscriptBody(itemId) {
	const threadId = transcript.threadId
	if (!threadId || !itemId) return
	const key = currentBodyKey(itemId)
	const existing = transcript.body[itemId]
	if (existing?.key === key && (existing.state === 'queued' || existing.state === 'loading' || existing.state === 'ready')) return
	if (existing?.key && existing.key !== key) cancelBodyKey(existing.key)
	const cached = bodyCache.get(key)
	if (cached) {
		bodyCache.delete(key); bodyCache.set(key, cached)
		transcript.body = { ...transcript.body, [itemId]: { state: 'ready', key, data: cached.data, error: null } }
		return
	}
	transcript.body = { ...transcript.body, [itemId]: { state: 'queued', key, data: null, error: null } }
	return new Promise((resolve) => {
		bodyQueue.push({ itemId, threadId, projectionKey: transcript.projection?.key ?? transcript.projection?.epoch ?? null, key, resolve, generation: bodyGeneration, counted: false })
		pumpBodyQueue()
	})
}

export function scheduleTranscriptBody(itemId) { void loadTranscriptBody(itemId) }

function pumpBodyQueue() {
	while (activeBodyLoads < MAX_BODY_LOADS && bodyQueue.length) void runBodyLoad(bodyQueue.shift())
}

async function runBodyLoad(job) {
	if (job.generation !== bodyGeneration || transcript.threadId !== job.threadId ||
		job.projectionKey !== (transcript.projection?.key ?? transcript.projection?.epoch ?? null) || currentBodyKey(job.itemId) !== job.key) { job.resolve?.(); return }
	activeBodyLoads++
	job.counted = true
	const controller = new AbortController()
	bodyControllers.set(job.key, controller)
	activeBodyJobs.set(job.key, job)
	transcript.body = { ...transcript.body, [job.itemId]: { state: 'loading', key: job.key, data: null, error: null } }
	try {
		const response = await fetch(transcriptBodyURL(job.threadId, job.itemId), { signal: controller.signal, cache: 'no-store' })
		if (!response.ok) throw await apiError(response)
		const data = await response.json()
		const item = currentBodyItem(job.itemId)
		if (bodyControllers.get(job.key) !== controller || controller.signal.aborted || transcript.threadId !== job.threadId ||
			job.projectionKey !== (transcript.projection?.key ?? transcript.projection?.epoch ?? null) ||
			currentBodyKey(job.itemId) !== job.key || (job.projectionKey && data.projectionKey !== job.projectionKey) ||
			data.contentRef !== item?.contentRef) return
		transcript.body = { ...transcript.body, [job.itemId]: { state: 'ready', key: job.key, data, error: null } }
		touchBodyCache(job.key, data, job.itemId)
	} catch (error) {
		if (bodyControllers.get(job.key) !== controller || controller.signal.aborted || error?.name === 'AbortError' || currentBodyKey(job.itemId) !== job.key) return
		transcript.body = { ...transcript.body, [job.itemId]: { state: 'error', key: job.key, data: null, error: error?.message ?? String(error) } }
	} finally {
		if (bodyControllers.get(job.key) === controller) bodyControllers.delete(job.key)
		if (activeBodyJobs.get(job.key) === job) activeBodyJobs.delete(job.key)
		releaseBodySlot(job)
		job.resolve?.()
	}
}

export function closeTranscript() {
  requestAbort?.abort()
  earlierAbort?.abort()
  tailAbort?.abort()
  abortEvidenceRequests()
	abortBodyRequests()
  requestAbort = null
  earlierAbort = null
  tailAbort = null
  tailDirty = false
  requestSeq += 1
  transcript.threadId = null
  transcript.items = []
  transcript.turns = []
  transcript.page = null
  transcript.projection = null
  transcript.state = 'idle'
  transcript.error = null
  transcript.earlierError = null
  transcript.loadingEarlier = false
  transcript.view = 'enhanced'
  transcript.hiddenCategories = defaultHiddenCategories('enhanced')
  transcript.taxonomy = null
  transcript.evidence = {}
	transcript.body = {}
	transcript.newerAvailable = false
}

export function __bodyCacheBytesForTest() { return bodyCacheBytes }
export function __setBodyCacheLimitForTest(value) { bodyCacheLimit = value }

// --- Durable history channel (Slice R3) --------------------------------------
// Historical panes each use a channel of saved transcript paging. It shares
// fetch and merge behavior with the Explorer history singleton above, and owns
// its state independently of that singleton and its cache.

// Tab-owned channels, keyed by nano thread id. ThreadPaneView registers the
// channel its read-only view renders from, so a catalog change to that thread
// re-reads it.
const tabChannels = new Map()

export function registerTabHistory(threadId, channel) {
  tabChannels.set(threadId, channel)
}

export function unregisterTabHistory(threadId, channel) {
  if (tabChannels.get(threadId) === channel) tabChannels.delete(threadId)
}

// Catalog events revalidate only the named historical tabs. A full refresh
// revalidates every registered channel.
export function nudgeTabHistories(threadIds = null) {
  const changed = threadIds ? new Set(threadIds) : null
  const channels = new Set()
  for (const [threadId, channel] of tabChannels) {
    if (!changed || changed.has(threadId)) channels.add(channel)
  }
  for (const channel of channels) {
    channel.rearm()
    void channel.revalidateTail()
  }
}

export function createDurableHistory(threadId, seed = null) {
  // The channel's profile is fixed at creation. The seed parameter is the
  // direct adoption hook: hand it a window (items + cursor + ETag) and load()
  // costs zero fetches.
  const view = normalizeTranscriptView(seed?.view ?? 'enhanced')
  const hiddenCategories = canonicalHiddenCategories(seed?.hiddenCategories ?? defaultHiddenCategories(view))
  const state = $state({
    threadId,
    items: [],
    turns: [],
    page: null,
    projection: null,
    state: 'idle', // idle | loading | ready | empty | pending | degraded | retained | error
    error: null,
    loadingEarlier: false,
    earlierError: null,
  })
  let etag = null
  let loaded = false
  let revalidated = false
  let revalidateQueued = false
  let revalidateWaiters = []
  let revalidating = false
  let closed = false
  const aborts = new Set()

  function adopt(body, nextEtag) {
    state.items = body.items ?? []
    state.turns = body.turns ?? []
    state.page = body.page ?? null
    state.projection = body.projection ?? null
    state.state = body.state ?? stateFrom(body)
    state.error = null
    etag = nextEtag ?? null
  }

  async function channelFetch(url, headers = {}) {
    const controller = new AbortController()
    aborts.add(controller)
    try {
      return await fetch(url, { signal: controller.signal, headers })
    } finally {
      aborts.delete(controller)
    }
  }

  async function load() {
    if (loaded || closed) return
    if (seed?.items?.length) {
      adopt(seed, seed.etag)
      seed = null // one-shot: any later reload fetches fresh
      loaded = true
    } else {
      seed = null
      state.state = 'loading'
      try {
        const response = await channelFetch(transcriptURL(threadId, null, { view, hiddenCategories }))
        if (!response.ok) throw await apiError(response)
        const body = await response.json()
        if (closed) return
        adopt({ ...body, state: stateFrom(body) }, response.headers?.get?.('etag'))
        loaded = true
      } catch (error) {
        if (closed || error?.name === 'AbortError') return
        state.state = 'error'
        state.error = error?.message ?? String(error)
        loaded = true // no retry storms; a queued revalidation may still repair
      }
    }
    runQueuedRevalidation()
  }

  // Repair path (projection epoch reset / 409): the durable window can't be
  // spliced onto the old item ids, so reload the tail cleanly — the only case
  // allowed to drop paged-back history (same doctrine as the singleton).
  function reload() {
    loaded = false
    return load()
  }

  // Each catalog nudge rearms one ETag tail revalidation. A nudge before the
  // initial page lands runs after it. Failures keep the historical page shown.
  function rearm() {
    revalidated = false
  }

  function queueRevalidation() {
    revalidateQueued = true
    return new Promise((resolve) => revalidateWaiters.push(resolve))
  }

  function runQueuedRevalidation() {
    if (!revalidateQueued || closed) return
    revalidateQueued = false
    const waiters = revalidateWaiters
    revalidateWaiters = []
    rearm()
    void revalidateTail().then((result) => {
      for (const resolve of waiters) resolve(result)
    })
  }

  async function revalidateTail() {
    if (closed) return true
    if (!loaded) {
      // Nothing from this channel is mounted yet. Queue its first fresh tail,
      // and let callers wait for that post-load fetch. Queue admission alone
      // is not evidence that a repaired saved display reached this browser.
      return queueRevalidation()
    }
    if (revalidating) {
      // The current fetch still owns this channel. Queue one post-fetch pass;
      // callers awaiting revalidation need the fresh pass, not the older result.
      return queueRevalidation()
    }
    if (revalidated) return true
    revalidated = true
    revalidating = true
    try {
      const response = await channelFetch(transcriptURL(threadId, null, { view, hiddenCategories }), etag ? { 'If-None-Match': etag } : {})
      if (response.status === 304) return true
      if (!response.ok) throw await apiError(response)
      const body = await response.json()
      if (closed) return
      const current = state.items.length
        ? { items: state.items, turns: state.turns, page: state.page, projection: state.projection }
        : null
      const merged = mergeRevalidatedTail(current, body)
      adopt({ ...merged, state: stateFrom(merged) }, response.headers?.get?.('etag'))
      return true
    } catch {
      // Keep the rendered historical page when a revalidation fails.
      return false
    } finally {
      revalidating = false
      runQueuedRevalidation()
    }
  }

  async function loadEarlier() {
    const cursor = state.page?.beforeCursor ?? state.page?.nextBeforeCursor
    if (!cursor || state.loadingEarlier || closed) return
    state.loadingEarlier = true
    state.earlierError = null
    try {
      const response = await channelFetch(transcriptURL(threadId, cursor, { view, hiddenCategories }))
      if (!response.ok) throw await apiError(response)
      const body = await response.json()
      if (closed) return
      const currentKey = state.projection?.epoch ?? state.projection?.key
      const nextKey = body?.projection?.epoch ?? body?.projection?.key
      if (currentKey && nextKey && currentKey !== nextKey) {
        await reload()
        return
      }
      const items = mergeTranscriptItems(body.items, state.items)
      adopt({
        ...body,
        items,
        turns: mergeTranscriptItems(body.turns, state.turns),
        state: stateFrom({ ...body, items }),
      }, etag)
    } catch (error) {
      if (closed || error?.name === 'AbortError') return
      if (error?.status === 409) {
        await reload()
        return
      }
      state.earlierError = error?.message ?? String(error)
    } finally {
      if (!closed) state.loadingEarlier = false
    }
  }

  function close() {
    closed = true
    for (const controller of aborts) controller.abort()
    aborts.clear()
    for (const resolve of revalidateWaiters) resolve(true)
    revalidateWaiters = []
    revalidateQueued = false
  }

  const channel = { state, load, loadEarlier, revalidateTail, rearm, close }
  return channel
}

export function __resetTranscriptForTest() {
  closeTranscript()
  cache.clear()
  cacheBytes = 0
	bodyCache.clear()
	bodyCacheBytes = 0
	bodyCacheLimit = TRANSCRIPT_BODY_CACHE_BYTES
  tabChannels.clear()
}

export function __transcriptCacheBytesForTest() {
  return cacheBytes
}
