
const PREFIX = 'nano-draft:'

export function draftKey({ provider, threadId, sessionId } = {}) {
  if (provider && threadId) return `${PREFIX}${provider}:${threadId}`
  return sessionId ? `${PREFIX}session:${sessionId}` : ''
}

export function loadDraft(key) {
  if (!key) return ''
  try {
    return localStorage.getItem(key) ?? ''
  } catch {
    return '' // private mode / storage disabled: drafts are a convenience
  }
}

// saveDraft treats an empty draft as a deletion, so a sent message does not
// leave an empty key behind for every thread the operator ever opened.
export function saveDraft(key, text) {
  if (!key) return
  try {
    if (String(text ?? '').trim()) localStorage.setItem(key, text)
    else localStorage.removeItem(key)
  } catch {
    // storage full or unavailable; the draft just isn't persisted
  }
}
