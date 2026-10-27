import { describe, it, expect, afterEach, vi } from 'vitest'
import { __resetGrokModelsForTest, loadGrokModels, normalizeGrokCatalog } from './grok_models.js'

afterEach(() => {
  __resetGrokModelsForTest()
  vi.unstubAllGlobals()
})

describe('grok model catalog', () => {
  it('drops hidden models when normalizing the API body', () => {
    const body = { data: [{ model: 'visible' }, { model: 'secret', hidden: true }] }
    expect(normalizeGrokCatalog(body).map((m) => m.model)).toEqual(['visible'])
  })

  it('normalizes a missing/empty body to an empty list rather than throwing', () => {
    expect(normalizeGrokCatalog(undefined)).toEqual([])
    expect(normalizeGrokCatalog({})).toEqual([])
  })

  it('fetches once for the whole page and degrades to an empty list, never a throw', async () => {
    const fetchMock = vi.fn(async () => ({ ok: true, json: async () => ({ data: [{ model: 'grok-1' }] }) }))
    vi.stubGlobal('fetch', fetchMock)
    const [first, second] = await Promise.all([loadGrokModels(), loadGrokModels()])
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(first).toBe(second)

    __resetGrokModelsForTest()
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 404 })))
    await expect(loadGrokModels()).resolves.toEqual([])

    __resetGrokModelsForTest()
    vi.stubGlobal('fetch', vi.fn(async () => { throw new Error('unreachable') }))
    await expect(loadGrokModels()).resolves.toEqual([])
  })
})
