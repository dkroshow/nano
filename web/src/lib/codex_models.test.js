// The model catalog New Window and the pane's per-turn control both read. The
// two surfaces must agree about which models exist and in what order — a
// divergence here reads as "the thread changed catalogs when I looked at it
// from the other place".
import { describe, it, expect, afterEach, vi } from 'vitest'
import {
  PINNED_CODEX_MODELS,
  __resetCodexModelsForTest,
  effortsForModel,
  labelForModel,
  loadCodexModels,
  mergePinnedCodexModels,
  normalizeCodexCatalog,
  orderCodexModels,
} from './codex_models.js'

const advertised = (model, extra = {}) => ({ id: model, model, displayName: model, ...extra })

afterEach(() => {
  __resetCodexModelsForTest()
  vi.unstubAllGlobals()
})

describe('codex model catalog', () => {
  it('adds the pinned family when a stale app-server does not advertise it', () => {
    const merged = mergePinnedCodexModels([advertised('gpt-5.2-codex')])
    expect(merged.map((m) => m.model)).toEqual([
      'gpt-5.2-codex',
      'gpt-6-astra',
      'gpt-5.6-sol',
      'gpt-5.6-terra',
      'gpt-5.6-luna',
    ])
  })

  it('lets live metadata win over the pinned fallback for the same model', () => {
    const live = advertised('gpt-5.6-sol', { displayName: 'Sol (live)' })
    const merged = mergePinnedCodexModels([live])
    expect(merged.filter((m) => m.model === 'gpt-5.6-sol')).toEqual([live])
  })

  it('pins the family to the top in its declared order, keeping catalog order below', () => {
    const ordered = orderCodexModels([
      advertised('gpt-5.2-codex'),
      advertised('gpt-5.6-luna'),
      advertised('gpt-5.1'),
      advertised('gpt-5.6-sol'),
      advertised('gpt-6-astra'),
    ])
    expect(ordered.map((m) => m.model)).toEqual([
      'gpt-6-astra',
      'gpt-5.6-sol',
      'gpt-5.6-luna',
      'gpt-5.2-codex',
      'gpt-5.1',
    ])
  })

  // Catches: GPT-6.1 Sol or GPT-6 Sol/Luna sorting out of place (below GPT-5.6,
  // or 6.1 Sol not first), or effort lists invented by Nano instead of read
  // from model/list. Boundary: normalizeCodexCatalog, the one path both New
  // Window and the pane control read.
  it('ranks advertised GPT-6.1 Sol first and GPT-6 Sol and Luna after Astra, with wire efforts', () => {
    const efforts = (...names) => names.map((reasoningEffort) => ({ reasoningEffort }))
    const catalog = normalizeCodexCatalog({
      data: [
        advertised('gpt-6-astra'),
        advertised('gpt-5.6-sol'),
        advertised('gpt-5.5'),
        advertised('gpt-6-luna', { supportedReasoningEfforts: efforts('low', 'medium', 'high', 'xhigh', 'max') }),
        advertised('gpt-6-sol', { supportedReasoningEfforts: efforts('low', 'medium', 'high', 'xhigh', 'max', 'ultra') }),
        advertised('gpt-6.1-sol', { supportedReasoningEfforts: efforts('low', 'medium', 'high', 'xhigh', 'max', 'ultra') }),
      ],
    })
    expect(catalog.map((m) => m.model)).toEqual([
      'gpt-6.1-sol',
      'gpt-6-astra',
      'gpt-6-sol',
      'gpt-6-luna',
      'gpt-5.6-sol',
      'gpt-5.6-terra',
      'gpt-5.6-luna',
      'gpt-5.5',
    ])
    expect(effortsForModel(catalog, 'gpt-6.1-sol')).toEqual(['low', 'medium', 'high', 'xhigh', 'max', 'ultra'])
    expect(effortsForModel(catalog, 'gpt-6-sol')).toEqual(['low', 'medium', 'high', 'xhigh', 'max', 'ultra'])
    expect(effortsForModel(catalog, 'gpt-6-luna')).toEqual(['low', 'medium', 'high', 'xhigh', 'max'])
  })

  // Catches: a display-only GPT-6.1 Sol or GPT-6 Sol/Luna alias shown while the
  // running Codex daemon is too old for the provider to accept it (HTTP 400 for
  // gpt-6-sol on 0.153.3, for gpt-6.1-sol on 0.155.1). Boundary:
  // normalizeCodexCatalog given a catalog that lacks them.
  it('does not invent GPT-6.1 Sol, GPT-6 Sol or Luna when the app-server does not advertise them', () => {
    const models = normalizeCodexCatalog({ data: [advertised('gpt-5.6-sol')] }).map((m) => m.model)
    expect(models).not.toContain('gpt-6.1-sol')
    expect(models).not.toContain('gpt-6-sol')
    expect(models).not.toContain('gpt-6-luna')
  })

  it('drops hidden models when normalizing the API body', () => {
    const body = { data: [advertised('visible'), advertised('secret', { hidden: true })] }
    expect(normalizeCodexCatalog(body).map((m) => m.model)).not.toContain('secret')
  })

  it('reports the efforts a model advertises, and nothing for one it does not know', () => {
    const catalog = normalizeCodexCatalog({ data: [] })
    expect(effortsForModel(catalog, 'gpt-6-astra')).toEqual(['low', 'medium', 'high', 'xhigh', 'max', 'ultra'])
    expect(effortsForModel(catalog, 'gpt-5.6-luna')).toEqual(['low', 'medium', 'high', 'xhigh', 'max'])
    // Every pinned model must carry efforts, or the pane's effort control
    // silently empties for a model the operator can still select.
    for (const pinned of PINNED_CODEX_MODELS) {
      expect(effortsForModel(catalog, pinned.model).length).toBeGreaterThan(0)
    }
    expect(effortsForModel(catalog, 'not-a-model')).toEqual([])
  })

  it('falls back to the raw id when the catalog has no display name', () => {
    const catalog = [advertised('gpt-5.6-sol', { displayName: 'GPT-5.6 Sol' })]
    expect(labelForModel(catalog, 'gpt-5.6-sol')).toBe('GPT-5.6 Sol')
    expect(labelForModel(catalog, 'mystery-model')).toBe('mystery-model')
  })

  it('fetches once for the whole page and degrades to an empty list, never a throw', async () => {
    const fetchMock = vi.fn(async () => ({ json: async () => ({ data: [advertised('gpt-5.2-codex')] }) }))
    vi.stubGlobal('fetch', fetchMock)
    const [first, second] = await Promise.all([loadCodexModels(), loadCodexModels()])
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(first).toBe(second)

    __resetCodexModelsForTest()
    vi.stubGlobal('fetch', vi.fn(async () => { throw new Error('daemon down') }))
    await expect(loadCodexModels()).resolves.toEqual([])
  })
  // The account upgrade changed model/list while both Nano caches stayed stale.
  it('refreshes on demand, shares concurrent refreshes, and rejects failed fresh reads', async () => {
    let data = [advertised('gpt-6-astra', { serviceTiers: [{ id: 'priority', name: 'Fast' }] })]
    const fetchMock = vi.fn(async () => ({ ok: true, json: async () => ({ data }) }))
    vi.stubGlobal('fetch', fetchMock)
    await loadCodexModels()
    data = [advertised('gpt-6-astra', { serviceTiers: [{ id: 'ultrafast', name: 'Ultrafast' }] })]
    const [first, second] = await Promise.all([loadCodexModels(true), loadCodexModels(true)])
    expect(first).toBe(second)
    expect(first.find((m) => m.model === 'gpt-6-astra').serviceTiers[0].id).toBe('ultrafast')
    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(fetchMock).toHaveBeenLastCalledWith('/api/codex/models?refresh=true')
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 502 })))
    await expect(loadCodexModels(true)).rejects.toThrow('could not be refreshed')
  })

})
