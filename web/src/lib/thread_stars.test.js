import { beforeEach, describe, expect, it, vi } from 'vitest'
import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import { patchThreadStarred, setThreadStarred, starRequestId } from './thread_stars.js'

beforeEach(() => {
  explorer.dataset = [
    { id: 'nth_1', provider: 'codex', nativeId: 'nat-1', starred: false, title: 'One' },
    { id: 'nth_2', provider: 'claude', nativeId: 'nat-2', starred: true, title: 'Two' },
  ]
  explorer.detail = { id: 'nth_1', provider: 'codex', nativeId: 'nat-1', starred: false }
  app.sessions = [{ id: 's1', provider: 'codex', threadId: 'nat-1', starred: false }]
})

describe('starRequestId', () => {
  it('prefers the catalog id the API can load', () => {
    expect(starRequestId({ click: { catalogId: 'nth_1' }, nativeId: 'nat-1' })).toBe('nth_1')
    expect(starRequestId({ nativeId: 'nat-1' })).toBe('nat-1')
  })
})

describe('patchThreadStarred', () => {
  it('updates the catalog row, detail, and matching session', () => {
    patchThreadStarred({ id: 'nth_1', nativeId: 'nat-1', provider: 'codex', starred: true })
    expect(explorer.dataset[0].starred).toBe(true)
    expect(explorer.detail.starred).toBe(true)
    expect(app.sessions[0].starred).toBe(true)
    expect(explorer.dataset[1].starred).toBe(true)
  })
})

describe('setThreadStarred', () => {
  it('PUTs starred and patches from the response record', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ id: 'nth_1', provider: 'codex', nativeId: 'nat-1', starred: true }),
    })))
    await setThreadStarred('nth_1', true)
    expect(fetch).toHaveBeenCalledWith('/api/v1/threads/nth_1/starred', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ starred: true }),
    })
    expect(explorer.dataset[0].starred).toBe(true)
    vi.unstubAllGlobals()
  })

  it('throws the server message on failure', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: false,
      status: 409,
      json: async () => ({ error: { message: 'Thread has no provider-native id to star yet' } }),
    })))
    await expect(setThreadStarred('mem_1', true)).rejects.toThrow(/provider-native id/)
    vi.unstubAllGlobals()
  })
})
