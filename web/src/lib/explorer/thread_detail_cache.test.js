import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  cachedThreadDetail,
  prepareThreadDetail,
  refreshThreadDetail,
  resetThreadDetailCacheForTest,
} from './thread_detail_cache.js'

beforeEach(() => {
  resetThreadDetailCacheForTest()
  vi.restoreAllMocks()
})

describe('thread detail cache', () => {
  it('serves the last detail while a later revalidation runs', async () => {
    const first = { id: 'thread-1', currentResolution: { state: 'ready' } }
    const second = { id: 'thread-1', currentResolution: { state: 'stale' } }
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => first })
      .mockResolvedValueOnce({ ok: true, json: async () => second })
    vi.stubGlobal('fetch', fetchMock)

    await refreshThreadDetail('thread-1')
    expect(cachedThreadDetail('thread-1')).toBe(first)

    const revalidation = refreshThreadDetail('thread-1')
    expect(cachedThreadDetail('thread-1')).toBe(first)
    await revalidation
    expect(cachedThreadDetail('thread-1')).toBe(second)
  })

  it('deduplicates concurrent detail requests', async () => {
    let resolve
    const response = new Promise((done) => { resolve = done })
    const fetchMock = vi.fn(() => response)
    vi.stubGlobal('fetch', fetchMock)

    const first = refreshThreadDetail('thread-1')
    const second = refreshThreadDetail('thread-1')
    resolve({ ok: true, json: async () => ({ id: 'thread-1' }) })

    await Promise.all([first, second])
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('opens an already prepared detail without another request', async () => {
    const detail = { id: 'thread-1', currentResolution: { state: 'ready' } }
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => detail })
    vi.stubGlobal('fetch', fetchMock)

    await refreshThreadDetail('thread-1')
    expect(await prepareThreadDetail('thread-1')).toBe(detail)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})
