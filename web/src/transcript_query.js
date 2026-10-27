export const TRANSCRIPT_LIMIT = 40
export const TRANSCRIPT_CACHE_BYTES = 16 * 1024 * 1024
export const TRANSCRIPT_BODY_CACHE_BYTES = 32 * 1024 * 1024

export const TRANSCRIPT_CATEGORIES = [
  { key: 'messages', label: 'Messages' },
  { key: 'reasoning', label: 'Reasoning' },
  { key: 'toolCall', label: 'Tool Calls' },
  { key: 'toolResult', label: 'Tool Results' },
  { key: 'subagent', label: 'Subagent Activity' },
  { key: 'system', label: 'System & Housekeeping' },
  { key: 'error', label: 'Errors' },
  { key: 'unclassified', label: 'Unclassified Provider Events' },
]

// Enhanced shows user and agent messages only: every other category is hidden.
export const ENHANCED_HIDDEN_CATEGORIES = ['reasoning', 'toolCall', 'toolResult', 'subagent', 'system', 'error', 'unclassified']

const CATEGORY_ORDER = new Map(TRANSCRIPT_CATEGORIES.map((category, index) => [category.key, index]))
const KNOWN_CATEGORIES = new Set(CATEGORY_ORDER.keys())

export function normalizeTranscriptView(view) {
  return view === 'dev' ? 'dev' : 'enhanced'
}

export function defaultHiddenCategories(view) {
  return normalizeTranscriptView(view) === 'dev' ? [] : [...ENHANCED_HIDDEN_CATEGORIES]
}

export function canonicalHiddenCategories(categories) {
  return [...new Set(categories ?? [])]
    .filter((category) => KNOWN_CATEGORIES.has(category))
    .sort((a, b) => CATEGORY_ORDER.get(a) - CATEGORY_ORDER.get(b))
}

export function transcriptProfileKey(view, hiddenCategories) {
  const normalizedView = normalizeTranscriptView(view)
  return `${normalizedView}:${canonicalHiddenCategories(hiddenCategories).join(',') || 'none'}`
}

export function transcriptURL(threadId, before = null, profile = {}) {
  return `/api/v1/threads/${encodeURIComponent(threadId)}/transcript?${transcriptQuery(before, profile)}`
}

// transcriptQuery is one page of one transcript profile, as every transcript
// endpoint reads it.
export function transcriptQuery(before = null, {
  view = 'enhanced',
  hiddenCategories = defaultHiddenCategories(view),
} = {}) {
  const normalizedView = normalizeTranscriptView(view)
  const hidden = canonicalHiddenCategories(hiddenCategories)
  const params = new URLSearchParams({
    limit: String(TRANSCRIPT_LIMIT),
    view: normalizedView,
    hide: hidden.length ? hidden.join(',') : 'none',
  })
  if (before) params.set('before', before)
  return params
}

export function transcriptEvidenceURL(threadId, itemId) {
  return `/api/v1/threads/${encodeURIComponent(threadId)}/transcript/evidence/${encodeURIComponent(itemId)}`
}

export function transcriptBodyURL(threadId, itemId) {
  return `/api/v1/threads/${encodeURIComponent(threadId)}/transcript/body/${encodeURIComponent(itemId)}`
}

export function mergeTranscriptItems(older, current) {
  const merged = new Map()
  for (const item of [...(older ?? []), ...(current ?? [])]) {
    const key = item?.id ?? `${item?.projectedSeq ?? ''}:${item?.type ?? ''}`
    merged.set(key, item)
  }
  return [...merged.values()].sort((a, b) =>
    (a.projectedSeq ?? a.ordinal ?? 0) - (b.projectedSeq ?? b.ordinal ?? 0))
}
