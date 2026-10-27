// Pure helpers for the Thread Explorer window fetch. Kept free of Svelte
// runes so the query builder and page merge are unit-testable in a node env.
//
// The list architecture is window-in-memory (the operator decision 2026-07-20): the
// browser eagerly fetches every page of one time window of the catalog and
// holds it whole; sorting, facet selections, date ranges, and facet counts
// are then computed locally (see explorer_local.js) with zero latency. Only
// the parameters below change what the server sends — everything else never
// leaves the browser.

// Fetch-time parameters: the window plus the dataset toggles.
export const DEFAULT_FETCH = {
  // 'thirty' | 'ninety' | 'all' — how far back (by last-message time) the
  // in-memory dataset reaches. 'all' is offered with a performance caveat.
  window: 'thirty',
  archived: 'include',
  // Workers are part of the operator's thread graph, not hidden diagnostics.
  // The local view nests them under their supervisor when both are present.
  includeSubagents: true,
  includeProgrammatic: false,
  threadId: '',
}

// Local view parameters: applied in the browser, never sent to the server.
export const DEFAULT_VIEW = {
  // The sort toggle's default (see SORT_MODES in explorer_local.js). One sort
  // serves both the rail and the expanded table, so whichever surface changes
  // it, the other is already ordered that way when it appears.
  sort: 'liveness-updated',
  dir: 'desc',
  // Facet selections: null = all values; { values: string[], unknown: boolean }
  // = the selected subset of exact values (+ the field-not-known bucket).
  projects: null,
  providers: null,
  models: null,
  efforts: null,
  // Inclusive local-date ranges per time column: null or {from:'', to:''}
  // holding YYYY-MM-DD strings from the date inputs.
  createdRange: null,
  updatedRange: null,
}

export const WINDOW_DAYS = { thirty: 30, ninety: 90 }
export const FETCH_PAGE_LIMIT = 250 // server max; fewer round trips per window

// Build the /api/v1/threads query string for one page of the window fetch.
// The toggles are always sent explicitly (our UI owns them); an exact threadId
// search drops the window bound so threads older than the window stay
// findable by ID.
export function buildWindowQuery(fetchParams, cursor, now = new Date()) {
  const p = new URLSearchParams()
  const threadId = fetchParams.threadId?.trim()
  if (threadId) p.set('threadId', threadId)
  const days = WINDOW_DAYS[fetchParams.window]
  if (days && !threadId) {
    p.set('updatedFrom', new Date(now.getTime() - days * 86_400_000).toISOString())
  }
  p.set('includeSubagents', fetchParams.includeSubagents ? 'true' : 'false')
  p.set('includeProgrammatic', fetchParams.includeProgrammatic ? 'true' : 'false')
  if (fetchParams.archived && fetchParams.archived !== 'include') p.set('archived', fetchParams.archived)
  p.set('limit', String(FETCH_PAGE_LIMIT))
  if (cursor) p.set('cursor', cursor)
  return p.toString()
}

// Append a keyset page onto the dataset, de-duplicating by id and then by
// thread identity. A catalog mutation between pages can resurface a row we
// already hold: last write wins (use the newer copy) but the first occurrence
// keeps its position. Pass an empty `existing` for the first page of a window
// fetch — it replaces the dataset, but still needs the identity dedup below
// because one response can carry both classes of row.
export function mergePages(existing, incoming) {
  const indexById = new Map(existing.map((it, i) => [it.id, i]))
  const merged = existing.slice()
  for (const it of incoming ?? []) {
    const at = indexById.get(it.id)
    if (at == null) {
      indexById.set(it.id, merged.length)
      merged.push(it)
    } else {
      merged[at] = it
    }
  }
  return dropSupersededTransients(merged)
}

// Identity key for the durable-vs-memory dedup. A memory row for a session
// still `starting` has no native id yet, so it can match nothing — such a row
// is always kept (it is the only evidence that session exists).
function nativeKey(item) {
  return item.nativeId ? `${item.provider ?? ''} ${item.nativeId}` : null
}

// Durable rows win over memory rows (`transient: true`) for the same
// (provider, nativeId). The server dedups at response-assembly time, but the
// scanner can index a thread between its existence check and the page we are
// merging, so both classes can legitimately be in flight at once — belt and
// braces against a double row across the handoff seam.
function dropSupersededTransients(rows) {
  const durableKeys = new Set()
  let hasTransient = false
  for (const row of rows) {
    if (row.transient) hasTransient = true
    else {
      const key = nativeKey(row)
      if (key) durableKeys.add(key)
    }
  }
  if (!hasTransient || durableKeys.size === 0) return rows
  return rows.filter((row) => !(row.transient && durableKeys.has(nativeKey(row))))
}
