// Live inbox-count state for the mail surfaces. Counts are seeded
// from catalog rows (the server stamps a read-time `mail` overlay on every
// hosted row) and patched by `mail` SSE events from the thread-index stream.
// The SSE payload always carries a thread's complete current counts, so merge
// is last-value-wins per hosted thread.

export const mailState = $state({
  // hostedThreadId -> {total, unread, seen}. Only threads a delta has named;
  // rows fall back to their catalog-stamped counts until then.
  counts: {},
  // Global {total, unread, seen} across every inbox, from the latest delta or
  // global listing fetch. Null until either arrives.
  summary: null,
  // Bumped once per applied delta so open mail panels know to refetch.
  revision: 0,
})

export function applyMailDelta(delta) {
  if (!delta || typeof delta !== 'object') return
  const next = { ...mailState.counts }
  for (const [id, counts] of Object.entries(delta.counts ?? {})) {
    next[id] = counts
  }
  mailState.counts = next
  if (delta.summary) mailState.summary = delta.summary
  mailState.revision += 1
}

// A stream reconnect can have missed any number of deltas. The window reload
// that follows re-stamps fresh counts on every row, so stale live entries must
// yield rather than override them.
export function resetMailCounts() {
  mailState.counts = {}
}

export function liveMailCounts(hostedThreadId) {
  if (!hostedThreadId) return null
  return mailState.counts[hostedThreadId] ?? null
}
