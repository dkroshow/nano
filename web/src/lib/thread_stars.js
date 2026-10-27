import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'

export function starRequestId(row) {
  return row?.click?.catalogId || row?.id || row?.nativeId || ''
}

export async function setThreadStarred(id, starred) {
  const res = await fetch(`/api/v1/threads/${encodeURIComponent(id)}/starred`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ starred: Boolean(starred) }),
  })
  if (!res.ok) {
    let message = `request failed (${res.status})`
    try {
      const body = await res.json()
      message = body?.error?.message || message
    } catch {
      // keep the status fallback
    }
    throw new Error(message)
  }
  const body = await res.json()
  const next = Boolean(body?.starred)
  patchThreadStarred({
    id: body?.id || id,
    nativeId: body?.nativeId,
    provider: body?.provider,
    starred: next,
  })
  return next
}

export function patchThreadStarred({ id, nativeId, provider, starred } = {}) {
  const wanted = Boolean(starred)
  const matchItem = (item) => (
    (id && item.id === id) ||
    (nativeId && item.nativeId === nativeId && (!provider || item.provider === provider))
  )
  const idx = explorer.dataset.findIndex(matchItem)
  if (idx >= 0) {
    const next = explorer.dataset.slice()
    next[idx] = { ...next[idx], starred: wanted }
    explorer.dataset = next
  }
  if (explorer.detail && matchItem(explorer.detail)) {
    explorer.detail = { ...explorer.detail, starred: wanted }
  }
  app.sessions = (app.sessions ?? []).map((session) => {
    if (nativeId && session.threadId === nativeId && (!provider || session.provider === provider)) {
      return { ...session, starred: wanted }
    }
    if (id && session.id === id) return { ...session, starred: wanted }
    return session
  })
}
