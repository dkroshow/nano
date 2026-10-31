import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  __resetExplorerPageCacheForTest,
  __resetSidebarBootstrapForTest,
  bootstrapSidebar,
  explorer,
  loadParentHistory,
  mergeActiveAndHistory,
  scheduleActiveRefresh,
  sidebarMergeKey,
} from './explorer.svelte.js'
import { applyView, collapseWorkerGroups, threadIdentityKeys, threadParentIdentityKeys } from './explorer_local.js'

const response = (body, ok = true) => ({
  ok,
  status: ok ? 200 : 500,
  headers: { get: () => null },
  json: async () => body,
})

const activeBody = {
  items: [
    {
      id: 'nth_a',
      provider: 'codex',
      nativeId: 'a',
      updatedAt: '2026-08-21T00:00:00Z',
      sidebar: {
        parentId: null,
        rootId: 'nth_a',
        ancestorIds: [],
        childrenIds: [],
        depth: 0,
        active: true,
        historicalDescendantCount: 2,
        descendantActivity: { total: 2, active: 0, workers: 0 },
        sort: { title: 'alpha', project: '/w/a', createdAt: 1, updatedAt: 100, liveness: 1 },
      },
    },
  ],
  roots: ['nth_a'],
  historicalCount: 4,
  catalogVersion: 10,
}

beforeEach(() => {
  __resetExplorerPageCacheForTest()
  __resetSidebarBootstrapForTest()
  explorer.dataset = []
  explorer.datasetComplete = false
  explorer.catalogVersion = null
  explorer.listState = 'idle'
  explorer.listError = null
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('active sidebar boot', () => {
  it('commits one active response without fetching historical /api/v1/threads pages', async () => {
    const fetchMock = vi.fn(async (url) => {
      if (String(url).startsWith('/api/v1/sidebar/threads')) return response(activeBody)
      throw new Error(`unexpected fetch ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    await bootstrapSidebar(Promise.resolve({ ok: true, sessions: [] }))

    expect(explorer.dataset.map((row) => row.id)).toEqual(['nth_a'])
    expect(explorer.datasetScope).toBe('active')
    expect(explorer.sidebarBootState).toBe('ready')
    expect(explorer.historicalCount).toBe(4)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('shows an explicit error and does not auto-fetch historical threads when the first load fails', async () => {
    const fetchMock = vi.fn(async (url) => {
      if (String(url).startsWith('/api/v1/sidebar/threads')) return response({ error: { message: 'nope' } }, false)
      throw new Error(`unexpected fetch ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    await bootstrapSidebar(Promise.resolve({ ok: true, sessions: [] }))

    expect(explorer.dataset).toEqual([])
    expect(explorer.sidebarBootState).toBe('error')
    expect(explorer.listError).toMatch(/nope|failed/)
    expect(fetchMock.mock.calls.every((call) => !String(call[0]).startsWith('/api/v1/threads?'))).toBe(true)
  })

  it('merges mem_ to nth_ by provider:nativeId so only the active durable row remains', () => {
    const active = [{ id: 'nth_live', provider: 'codex', nativeId: 'native-1', sidebar: { active: true } }]
    const history = {
      parent: { items: [{ id: 'mem_sess', provider: 'codex', nativeId: 'native-1' }] },
    }
    expect(mergeActiveAndHistory(active, history).map((row) => row.id)).toEqual(['nth_live'])
    expect(sidebarMergeKey({ provider: 'codex', nativeId: 'native-1', id: 'mem_x' })).toBe('codex:native-1')
  })

  it('deduplicates one historical row returned through overlapping parent scopes', () => {
    const duplicate = { id: 'nth_old', provider: 'codex', nativeId: 'old-1' }
    const history = {
      ancestor: { items: [duplicate] },
      descendant: { items: [{ ...duplicate }] },
    }

    expect(mergeActiveAndHistory([], history).map((row) => row.id)).toEqual(['nth_old'])
  })

  it('loads every cursor page for one parent only after that parent is opened', async () => {
    const fetchMock = vi.fn(async (url) => {
      const value = String(url)
      if (!value.includes('/api/v1/sidebar/threads/nth_parent/history?')) {
        throw new Error(`unexpected fetch ${url}`)
      }
      if (!value.includes('cursor=')) {
        return response({ items: [{ id: 'nth_old_1', provider: 'codex', nativeId: 'old-1' }], nextCursor: 'page-2' })
      }
      expect(value).toContain('cursor=page-2')
      return response({ items: [{ id: 'nth_old_2', provider: 'codex', nativeId: 'old-2' }], nextCursor: null })
    })
    vi.stubGlobal('fetch', fetchMock)

    expect(fetchMock).not.toHaveBeenCalled()
    await loadParentHistory({ id: 'nth_parent' })

    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(explorer.historyScopes.nth_parent.state).toBe('ready')
    expect(explorer.dataset.map((row) => row.id)).toEqual(['nth_old_1', 'nth_old_2'])
  })

  it('keeps a failed parent history scope retryable', async () => {
    let attempt = 0
    const fetchMock = vi.fn(async () => {
      attempt += 1
      if (attempt === 1) return response({ error: { message: 'temporary' } }, false)
      return response({ items: [{ id: 'nth_old', provider: 'codex', nativeId: 'old' }], nextCursor: null })
    })
    vi.stubGlobal('fetch', fetchMock)

    await loadParentHistory({ id: 'nth_parent' })
    expect(explorer.historyScopes.nth_parent.state).toBe('error')
    expect(explorer.historyScopes.nth_parent.error).toMatch(/temporary|failed/)

    await loadParentHistory({ id: 'nth_parent' })
    expect(explorer.historyScopes.nth_parent.state).toBe('ready')
    expect(explorer.dataset.map((row) => row.id)).toEqual(['nth_old'])
  })

  it('coalesces membership nudges into one current active-graph replacement', async () => {
    explorer.activeItems = [{ id: 'nth_before', provider: 'codex', nativeId: 'before' }]
    explorer.dataset = explorer.activeItems
    explorer.datasetScope = 'active'
    const fetchMock = vi.fn(async () => response({
      ...activeBody,
      items: [{ id: 'nth_after', provider: 'codex', nativeId: 'after', sidebar: { active: true } }],
      catalogVersion: 11,
    }))
    vi.stubGlobal('fetch', fetchMock)

    scheduleActiveRefresh()
    scheduleActiveRefresh()
    await vi.advanceTimersByTimeAsync(249)
    expect(fetchMock).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)

    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(explorer.dataset.map((row) => row.id)).toEqual(['nth_after'])
    expect(explorer.catalogVersion).toBe(11)
  })
})

describe('server hierarchy integration', () => {
  it('emits catalog id identity and prefers sidebar.parentId', () => {
    const row = { id: 'nth_child', provider: 'codex', nativeId: 'child', sidebar: { parentId: 'nth_parent' }, parentNativeId: 'legacy' }
    expect(threadIdentityKeys(row)).toContain('nth_child')
    expect(threadParentIdentityKeys(row)[0]).toBe('nth_parent')
  })

  it('sorts mixed server/legacy rows with equal timestamps by id', () => {
    const a = { id: 'nth_b', updatedAt: '2026-08-21T00:00:00Z', sidebar: { sort: { updatedAt: Date.parse('2026-08-21T00:00:00Z'), title: 'b', project: '', createdAt: 0, liveness: 3 } }, metadata: {} }
    const b = { id: 'nth_a', updatedAt: '2026-08-21T00:00:00Z', metadata: { cwd: { state: 'unknown' } } }
    expect(applyView([a, b], { sort: 'updated', dir: 'desc', projects: null, providers: null, models: null, efforts: null, createdRange: null, updatedRange: null }).map((row) => row.id)).toEqual(['nth_b', 'nth_a'])
  })

  it('makes an all-historical parent collapsible from the server count', () => {
    const parent = {
      id: 'nth_parent', nativeId: 'parent', provider: 'codex', title: 'Supervisor',
      sidebar: { active: true, historicalDescendantCount: 3, descendantActivity: { total: 3, active: 0, workers: 0 } },
    }
    const [row] = collapseWorkerGroups([parent], ['*'], [])
    expect(row.workersCollapsible).toBe(true)
    expect(row.historicalAgentCount).toBe(3)
  })
})
