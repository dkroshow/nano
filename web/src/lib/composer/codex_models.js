// The Codex model catalog, shared by New Window (which model to start a thread
// with) and the pane's per-turn control (which model the NEXT turn runs on).
// Both must offer the same list in the same order, or the same thread reads as
// two different catalogs depending on where you look at it.
//
// model/list is normally authoritative, but a long-running app-server can
// retain a pre-release catalog. The pinned entries below keep the current
// GPT-6 Astra and the GPT-5.6 family selectable while that catalog catches up; when the wire
// advertises one of them, its live metadata replaces the fallback.
// GPT-6.1 Sol and GPT-6 Sol and Luna are ordered below but never pinned (see CODEX_MODEL_ORDER).
export const PINNED_CODEX_MODELS = [
  {
    id: 'gpt-6-astra',
    model: 'gpt-6-astra',
    displayName: 'GPT-6-Astra',
    description: 'Our most capable model for complex, demanding work.',
    hidden: false,
    defaultReasoningEffort: 'low',
    supportedReasoningEfforts: ['low', 'medium', 'high', 'xhigh', 'max', 'ultra'].map((reasoningEffort) => ({ reasoningEffort })),
  },
  {
    id: 'gpt-5.6-sol',
    model: 'gpt-5.6-sol',
    displayName: 'GPT-5.6 Sol',
    description: 'Flagship capability model.',
    hidden: false,
    defaultReasoningEffort: 'low',
    supportedReasoningEfforts: ['low', 'medium', 'high', 'xhigh', 'max', 'ultra'].map((reasoningEffort) => ({ reasoningEffort })),
  },
  {
    id: 'gpt-5.6-terra',
    model: 'gpt-5.6-terra',
    displayName: 'GPT-5.6 Terra',
    description: 'Balanced agentic coding model for everyday work.',
    hidden: false,
    defaultReasoningEffort: 'low',
    supportedReasoningEfforts: ['low', 'medium', 'high', 'xhigh', 'max', 'ultra'].map((reasoningEffort) => ({ reasoningEffort })),
  },
  {
    id: 'gpt-5.6-luna',
    model: 'gpt-5.6-luna',
    displayName: 'GPT-5.6 Luna',
    description: 'Efficient, high-volume agentic coding model.',
    hidden: false,
    defaultReasoningEffort: 'low',
    supportedReasoningEfforts: ['low', 'medium', 'high', 'xhigh', 'max'].map((reasoningEffort) => ({ reasoningEffort })),
  },
]

// Display order for the current families. GPT-6.1 Sol and GPT-6 Sol and Luna
// are ranked but deliberately not pinned: the provider rejects each from Codex
// clients older than the release that introduced it (0.159.0 for GPT-6.1 Sol,
// 0.155.1 for GPT-6 Sol and Luna), so they must appear only when the app-server
// itself advertises them, never as a fallback the running daemon cannot serve.
const CODEX_MODEL_ORDER = ['gpt-6.1-sol', 'gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna']

const pinnedRank = new Map(CODEX_MODEL_ORDER.map((model, i) => [model, i]))

export function mergePinnedCodexModels(catalog) {
  const advertised = new Set(catalog.map((model) => model.model))
  return [...catalog, ...PINNED_CODEX_MODELS.filter((model) => !advertised.has(model.model))]
}

export function orderCodexModels(catalog) {
  return catalog
    .map((model, index) => ({ model, index }))
    .sort((a, b) => {
      const ar = pinnedRank.get(a.model.model)
      const br = pinnedRank.get(b.model.model)
      if (ar != null || br != null) return (ar ?? CODEX_MODEL_ORDER.length) - (br ?? CODEX_MODEL_ORDER.length)
      return a.index - b.index
    })
    .map(({ model }) => model)
}

// normalizeCodexCatalog: the /api/codex/models body → the selectable list.
export function normalizeCodexCatalog(body) {
  return orderCodexModels(mergePinnedCodexModels((body?.data ?? []).filter((m) => !m.hidden)))
}

// Efforts a model advertises, in the catalog's own order. Unknown model (a
// thread started before the catalog knew it, or a free-text id) → empty, and
// the caller falls back to free text rather than inventing a list.
export function effortsForModel(catalog, model) {
  const entry = catalog?.find((m) => m.model === model)
  return (entry?.supportedReasoningEfforts ?? []).map((e) => e.reasoningEffort).filter(Boolean)
}

export function labelForModel(catalog, model) {
  return catalog?.find((m) => m.model === model)?.displayName || model
}

// Cached reads share a page catalog. Explicit demand-driven refreshes share
// one request and reject visibly, rather than calling stale capabilities fresh.
let catalogPromise = null
let refreshPromise = null

export function loadCodexModels(refresh = false) {
  if (refreshPromise) return refreshPromise
  if (!refresh && catalogPromise) return catalogPromise
  const request = fetch(`/api/codex/models${refresh ? '?refresh=true' : ''}`)
    .then((res) => {
      if (res.ok === false) throw new Error('Codex model capabilities could not be refreshed')
      return res.json()
    })
    .then(normalizeCodexCatalog)
  if (refresh) {
    refreshPromise = request.finally(() => { refreshPromise = null })
    refreshPromise.then((catalog) => { catalogPromise = Promise.resolve(catalog) }, () => {})
    return refreshPromise
  }
  catalogPromise = request.catch(() => {
    catalogPromise = null
    return []
  })
  return catalogPromise
}

export function __resetCodexModelsForTest() {
  catalogPromise = null
  refreshPromise = null
}
