// The Grok Build model catalog, mirroring codex_models.js's loader shape.
// /api/grok/models projects the current/available models straight off ACP
// initialize's modelState (grok.go); a failing probe (binary missing, no
// auth, timeout) answers non-2xx, which degrades to an empty catalog here so
// callers fall back to free text meaning "inherit Grok's own default" rather
// than inventing model ids.
let catalogPromise = null

export function normalizeGrokCatalog(body) {
  return (body?.data ?? []).filter((m) => !m.hidden)
}

export function loadGrokModels() {
  if (!catalogPromise) {
    catalogPromise = fetch('/api/grok/models')
      .then((res) => (res.ok ? res.json() : Promise.reject(new Error(`grok models ${res.status}`))))
      .then(normalizeGrokCatalog)
      .catch(() => []) // catalog unavailable: callers fall back to inherit/free text
  }
  return catalogPromise
}

export function __resetGrokModelsForTest() {
  catalogPromise = null
}
