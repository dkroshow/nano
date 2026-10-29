// The composer's while-busy behavior. A provider that takes running-turn input
// (Codex `turn/steer`) receives Send into the working turn, as its own app
// does; a visible Queue control holds the message for the next turn instead.
// Other providers, and any turn waiting on a dialog, queue.
//
// Several prompts in one turn stay distinct: each keeps its own provider
// identity and settles by its own saved row.
//
// Nano now owns the live durable queue. The pure entry helpers remain useful
// to its browser view, while the localStorage helpers below exist only to
// migrate queues written by bundles deployed before 2026-08-05.

export function queueEntry(value) {
  if (value && typeof value === 'object') {
    const text = String(value.text ?? '').trim()
    if (!text || !value.clientActionId) return null
    return { ...value, text, status: value.status || 'queued' }
  }
  if (typeof value !== 'string') return null
  const text = value.trim()
  if (!text) return null
  return {
    clientActionId: `queued-${Date.now()}-${Math.random().toString(16).slice(2)}`,
    text,
    status: 'queued',
    createdAt: Date.now(),
  }
}

// Every queued submission gets its own action id. Repeated text is legitimate
// and must not collapse into one operation.
export function enqueue(queue, value) {
  const entry = queueEntry(value)
  if (!entry) return queue ?? []
  return [...(queue ?? []), entry]
}

// dequeue returns [next, rest]. Empty queue yields [null, []].
export function dequeue(queue) {
  const list = queue ?? []
  if (!list.length) return [null, []]
  return [queueEntry(list[0]), list.slice(1).map(queueEntry).filter(Boolean)]
}

// --- Legacy persistence migration -----------------------------------------
// These keys are no longer written for new submissions. They are read once so
// an upgrade can POST each old entry to Nano with its original action id, then
// removed after the server acknowledges ownership.

const STORAGE_PREFIX = 'nano-queue:'

export function queueKey({ provider, threadId, sessionId } = {}) {
  if (provider && threadId) return `${STORAGE_PREFIX}${provider}:${threadId}`
  return sessionId ? `${STORAGE_PREFIX}session:${sessionId}` : ''
}

export function loadQueue(key) {
  if (!key) return []
  try {
    const parsed = JSON.parse(localStorage.getItem(key) ?? '[]')
    return Array.isArray(parsed) ? parsed.map(queueEntry).filter(Boolean) : []
  } catch {
    return [] // private mode / storage disabled / corrupt value
  }
}

export function saveQueue(key, queue) {
  if (!key) return
  try {
    if (queue?.length) localStorage.setItem(key, JSON.stringify(queue))
    else localStorage.removeItem(key)
  } catch {
    // storage full or unavailable; the queue just isn't persisted
  }
}

// queuedLabel is the chip text: enough of the message to recognize it, never
// enough to reflow the composer.
export function queuedLabel(message, max = 72) {
  const line = String(message?.text ?? message ?? '').replace(/\s+/g, ' ').trim()
  return line.length > max ? line.slice(0, max - 1) + '…' : line
}
