// Historical detail is a read-mostly projection. Keep the last successful
// response in the browser and revalidate it without blanking the pane.
const cache = new Map()
const inflight = new Map()
const MAX_DETAILS = 100

export function cachedThreadDetail(id) {
  const detail = cache.get(id) ?? null
  if (detail) {
    cache.delete(id)
    cache.set(id, detail)
  }
  return detail
}

function remember(id, detail) {
  cache.delete(id)
  cache.set(id, detail)
  while (cache.size > MAX_DETAILS) cache.delete(cache.keys().next().value)
}

export async function refreshThreadDetail(id) {
  if (inflight.has(id)) return inflight.get(id)
  const request = (async () => {
    const res = await fetch(`/api/v1/threads/${encodeURIComponent(id)}`)
    if (!res.ok) {
      let message = `Thread detail failed (${res.status})`
      try {
        const body = await res.json()
        message = body?.error?.message || message
      } catch {
        // Keep the status-based message.
      }
      throw new Error(message)
    }
    const detail = await res.json()
    remember(id, detail)
    return detail
  })()
  inflight.set(id, request)
  try {
    return await request
  } finally {
    if (inflight.get(id) === request) inflight.delete(id)
  }
}

function resolutionReady(detail) {
  return Boolean(detail) && detail.currentResolution?.state !== 'resolving'
}

export async function prepareThreadDetail(id) {
  let detail = cachedThreadDetail(id)
  if (detail && resolutionReady(detail)) return detail
  for (let attempt = 0; attempt <= 8; attempt += 1) {
    detail = await refreshThreadDetail(id)
    if (resolutionReady(detail)) return detail
    const delay = Math.min(4000, 250 * (2 ** attempt))
    await new Promise((resolve) => setTimeout(resolve, delay))
  }
  return detail
}

export function prefetchThreadDetail(id) {
  const detail = id ? cachedThreadDetail(id) : null
  if (!id || resolutionReady(detail)) return
  void prepareThreadDetail(id).catch(() => {
    // Opening Details retries and reports any durable error.
  })
}

export function resetThreadDetailCacheForTest() {
  cache.clear()
  inflight.clear()
}
