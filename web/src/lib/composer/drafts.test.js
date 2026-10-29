// @vitest-environment jsdom
import { describe, it, expect, beforeEach } from 'vitest'
import { draftKey, loadDraft, saveDraft } from './drafts.js'

describe('composer drafts', () => {
  beforeEach(() => localStorage.clear())

  it('keys by thread, so a re-attach with a fresh session id keeps the draft', () => {
    const before = draftKey({ provider: 'claude', threadId: 'native-1', sessionId: 'aaaa' })
    const after = draftKey({ provider: 'claude', threadId: 'native-1', sessionId: 'bbbb' })
    expect(before).toBe(after)
  })

  it('falls back to the session while the thread id has not landed yet', () => {
    expect(draftKey({ provider: 'claude', sessionId: 'aaaa' })).toBe('nano-draft:session:aaaa')
    expect(draftKey({})).toBe('')
  })

  it('separates providers that could share a native id', () => {
    expect(draftKey({ provider: 'codex', threadId: 'x' })).not.toBe(draftKey({ provider: 'claude', threadId: 'x' }))
  })

  it('treats an emptied draft as a deletion instead of leaving a key per thread', () => {
    const key = draftKey({ provider: 'codex', threadId: 'native-1' })
    saveDraft(key, 'half a thought')
    expect(loadDraft(key)).toBe('half a thought')
    saveDraft(key, '   ')
    expect(localStorage.getItem(key)).toBeNull()
    expect(loadDraft(key)).toBe('')
  })

  it('is a no-op without a key rather than writing a shared bucket', () => {
    saveDraft('', 'orphan')
    expect(localStorage.length).toBe(0)
    expect(loadDraft('')).toBe('')
  })
})
