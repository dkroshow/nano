// Runtime SSE deltas patch volatile overlays in place; identity changes request
// one conditional window refresh. Legacy empty payloads still exercise the
// fallback refetch path, and refreshes defer-never-drop while a load is in
// flight. EventSource and fetch are stubbed per explorer_stream.test.js.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import {
  __resetExplorerPageCacheForTest,
  addRuntimeListener,
  explorer,
  startPolling,
  stopPolling,
} from './explorer.svelte.js'
import {
  __resetTranscriptForTest,
  createDurableHistory,
  registerTabHistory,
} from './transcript_store.svelte.js'
import { addTranscriptAuditListener, __resetTranscriptAuditListenersForTest } from './transcript_audit_events.js'

const readyStatus = { state: 'ready', catalogVersion: null, roots: [] }
const emptyPage = { items: [], nextCursor: null, catalogVersion: 1 }

class MockEventSource {
  constructor(url) {
    this.url = url
    this.listeners = {}
    this.closed = false
    MockEventSource.instances.push(this)
  }
  addEventListener(type, fn) {
    ;(this.listeners[type] ??= []).push(fn)
  }
  removeEventListener(type, fn) {
    this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn)
  }
  close() {
    this.closed = true
  }
  emit(type, event = {}) {
    for (const fn of this.listeners[type] ?? []) fn(event)
  }
}

const threadsFetches = (fetchMock) => fetchMock.mock.calls.filter(([url]) => String(url).includes('/api/v1/threads?'))
const statusFetches = (fetchMock) => fetchMock.mock.calls.filter(([url]) => String(url).includes('/thread-index/status'))
const flush = () => new Promise((resolve) => setTimeout(resolve))

let fetchMock

beforeEach(() => {
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('document', {
    visibilityState: 'visible',
    addEventListener: () => {},
    removeEventListener: () => {},
  })
  fetchMock = vi.fn(async (url) => {
    if (String(url).includes('/api/v1/threads?')) return { ok: true, status: 200, json: async () => emptyPage }
    if (String(url).includes('/api/v1/sidebar/threads')) {
      return { ok: true, status: 200, json: async () => ({ items: explorer.activeItems, historicalCount: 0, catalogVersion: explorer.catalogVersion ?? 1 }) }
    }
    return { ok: true, status: 200, json: async () => readyStatus }
  })
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  stopPolling()
  __resetExplorerPageCacheForTest()
  __resetTranscriptForTest()
  __resetTranscriptAuditListenersForTest()
  vi.unstubAllGlobals()
  explorer.dataset = []
  explorer.datasetComplete = false
  explorer.datasetScope = 'empty'
  explorer.windowItems = []
  explorer.activeItems = []
  explorer.listState = 'idle'
  explorer.catalogVersion = null
  explorer.index = null
  explorer.indexError = null
})

describe('runtime SSE event', () => {
  it('delivers a content-free transcript audit request through the existing browser stream', async () => {
    const heard = []
    addTranscriptAuditListener((request) => heard.push(request))
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('transcript-audit', { data: '{"auditId":"tau_1","tabId":"tab-1","pageId":"page-1","paneId":"pane-2","scopes":["messages"]}' })
    await flush()

    expect(heard).toEqual([{ auditId: 'tau_1', tabId: 'tab-1', pageId: 'page-1', paneId: 'pane-2', scopes: ['messages'] }])
    expect(threadsFetches(fetchMock)).toHaveLength(0)
  })

  it('patches a runtime delta in place without refetching the window', async () => {
    explorer.dataset = [{
      id: 'nth_1',
      provider: 'codex',
      nativeId: 'native-1',
      runtime: { attached: true, status: 'ready', turnActive: false },
    }]
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    es.emit('runtime', {
      data: JSON.stringify({
        rows: [{
          provider: 'codex',
          nativeId: 'native-1',
          runtime: { attached: true, status: 'ready', turnActive: true },
        }],
      }),
    })
    await flush()

    expect(threadsFetches(fetchMock)).toHaveLength(0)
    expect(explorer.dataset[0].runtime.turnActive).toBe(true)
  })


  it('one runtime event → exactly one window refetch, no status fetch', async () => {
    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    expect(threadsFetches(fetchMock)).toHaveLength(0)

    es.emit('runtime', { data: '{}' })
    await flush()

    expect(threadsFetches(fetchMock)).toHaveLength(1)
    expect(statusFetches(fetchMock)).toHaveLength(0) // runtime is not a status signal
    expect(explorer.catalogVersion).toBe(1) // the refetched page landed
  })

  it('sends the cached page validator and accepts a 304 response', async () => {
    let listCall = 0
    fetchMock.mockImplementation(async (url, options = {}) => {
      if (String(url).includes('/api/v1/sidebar/threads')) {
        return { ok: true, status: 200, json: async () => ({ items: [], historicalCount: 0, catalogVersion: 1 }) }
      }
      if (String(url).includes('/api/v1/threads?')) {
        listCall += 1
        if (listCall === 1) {
          return {
            ok: true,
            status: 200,
            headers: { get: (name) => (name === 'ETag' ? '"catalog-page"' : null) },
            json: async () => emptyPage,
          }
        }
        expect(options.headers['If-None-Match']).toBe('"catalog-page"')
        return { ok: false, status: 304, headers: { get: () => '"catalog-page"' } }
      }
      return { ok: true, status: 200, json: async () => readyStatus }
    })

    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    es.emit('runtime', { data: '{}' })
    await vi.waitFor(() => expect(listCall).toBe(1))
    await vi.waitFor(() => expect(explorer.listState).toBe('idle'))
    es.emit('runtime', { data: '{}' })
    await vi.waitFor(() => expect(listCall).toBe(2))

    expect(explorer.listState, JSON.stringify(explorer.listError)).toBe('idle')
    expect(explorer.catalogVersion).toBe(1)
  })

  it('runtime nudges during an in-flight load defer to exactly one follow-up', async () => {
    let resolveLoad
    fetchMock.mockImplementation(async (url) => {
      if (String(url).includes('/api/v1/sidebar/threads')) {
        return { ok: true, status: 200, json: async () => ({ items: [], historicalCount: 0, catalogVersion: 1 }) }
      }
      if (String(url).includes('/api/v1/threads?')) {
        return await new Promise((res) => {
          resolveLoad = (body) => res({ ok: true, json: async () => body })
        })
      }
      return { ok: true, status: 200, json: async () => readyStatus }
    })

    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    es.emit('runtime', { data: '{}' }) // starts a load
    await flush()
    expect(threadsFetches(fetchMock)).toHaveLength(1)

    es.emit('runtime', { data: '{}' })
    es.emit('runtime', { data: '{}' }) // both arrive mid-flight → one dirty flag
    await flush()
    expect(threadsFetches(fetchMock)).toHaveLength(1)

    resolveLoad({ items: [], nextCursor: null, catalogVersion: 1 })
    await vi.waitFor(() => expect(threadsFetches(fetchMock)).toHaveLength(2))

    resolveLoad({ items: [], nextCursor: null, catalogVersion: 1 })
    await flush()
    expect(threadsFetches(fetchMock)).toHaveLength(2) // no third load
  })

  it('catalog events keep their version-gated refetch + status refresh behavior', async () => {
    explorer.catalogVersion = 5
    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    es.emit('catalog', { data: '{"catalogVersion": 5}' }) // not newer → no window refetch
    await flush()
    expect(threadsFetches(fetchMock)).toHaveLength(0)
    expect(statusFetches(fetchMock)).toHaveLength(1) // status strip still refreshes

    es.emit('catalog', { data: '{"catalogVersion": 6}' }) // newer → window refetch
    await flush()
    expect(threadsFetches(fetchMock)).toHaveLength(1)
    expect(statusFetches(fetchMock)).toHaveLength(2)
  })

  it('patches the named catalog row instead of reloading the complete window', async () => {
    explorer.dataset = [
      { id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'before' },
      { id: 'nth_2', provider: 'claude', nativeId: 'native-2', title: 'unchanged' },
    ]
    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    explorer.windowItems = explorer.dataset
    explorer.catalogVersion = 5
    fetchMock.mockImplementation(async (url) => {
      const value = String(url)
      if (value.includes('/api/v1/sidebar/threads')) {
        return { ok: true, status: 200, json: async () => ({ items: explorer.dataset, historicalCount: 0, catalogVersion: 6 }) }
      }
      if (value.includes('/api/v1/threads?')) {
        expect(value).toContain('threadId=nth_1')
        return {
          ok: true,
          status: 200,
          json: async () => ({
            items: [{ id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'after' }],
            nextCursor: null,
            catalogVersion: 99,
          }),
        }
      }
      return { ok: true, status: 200, json: async () => ({ ...readyStatus, catalogVersion: 6 }) }
    })
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    es.emit('catalog', {
      data: JSON.stringify({ catalogVersion: 6, threadIds: ['nth_1'] }),
    })
    await vi.waitFor(() => expect(explorer.catalogVersion).toBe(6))

    expect(threadsFetches(fetchMock)).toHaveLength(1)
    expect(explorer.dataset).toHaveLength(2)
    expect(explorer.dataset.find((item) => item.id === 'nth_1')?.title).toBe('after')
    expect(explorer.dataset.find((item) => item.id === 'nth_2')?.title).toBe('unchanged')
  })

  it('patches a named active row without replacing the active graph', async () => {
    const before = {
      id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'before',
      sidebar: { parentId: null, rootId: 'nth_1', active: true },
    }
    explorer.dataset = [before]
    explorer.activeItems = [before]
    explorer.datasetScope = 'active'
    explorer.catalogVersion = 5
    fetchMock.mockImplementation(async (url) => {
      const value = String(url)
      if (value.includes('/api/v1/threads?')) {
        expect(value).toContain('threadId=nth_1')
        return {
          ok: true,
          status: 200,
          json: async () => ({
            items: [{ id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'after' }],
            nextCursor: null,
            catalogVersion: 99,
          }),
        }
      }
      return { ok: true, status: 200, json: async () => ({ ...readyStatus, catalogVersion: 6 }) }
    })
    startPolling()
    const es = MockEventSource.instances[0]

    es.emit('catalog', {
      data: JSON.stringify({ catalogVersion: 6, threadIds: ['nth_1'] }),
    })
    await vi.waitFor(() => expect(explorer.dataset[0]?.title).toBe('after'))

    expect(threadsFetches(fetchMock)).toHaveLength(1)
    expect(explorer.dataset[0].sidebar).toEqual(before.sidebar)
    expect(explorer.catalogVersion).toBe(6)
  })
})

describe('fleet SSE event', () => {
  it('patches an active-row fleet label in place', async () => {
    explorer.dataset = [{ id: 'nth_worker', provider: 'claude', nativeId: 'worker', fleet: { label: 'idle' } }]
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    es.emit('fleet', { data: '{"threadId":"nth_worker","to":"working"}' })
    await flush()

    expect(explorer.dataset[0].fleet.label).toBe('working')
    expect(threadsFetches(fetchMock)).toHaveLength(0)
  })

  it('a worker transition refetches the window without refreshing session summaries', async () => {
    const heard = []
    const unsubscribe = addRuntimeListener(() => heard.push('runtime'))
    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    es.emit('fleet', { data: '{"threadId":"nth_worker","to":"working"}' })
    await flush()

    expect(threadsFetches(fetchMock)).toHaveLength(1)
    expect(statusFetches(fetchMock)).toHaveLength(0)
    expect(heard).toEqual([])
    unsubscribe()
  })

  it('nudges the tab-owned transcript used to monitor an open worker', async () => {
    const channel = createDurableHistory('nth_worker', {
      items: [{ id: 'i1', projectedSeq: 1 }], turns: [],
      projection: { key: 'p1' }, page: {}, etag: 'W/"one"',
    })
    registerTabHistory('nth_worker', channel)
    await channel.load()

    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    es.emit('fleet', { data: '{"threadId":"nth_worker","to":"working"}' })

    await vi.waitFor(() => {
      const transcriptFetches = fetchMock.mock.calls.filter(([url]) => String(url).includes('/nth_worker/transcript'))
      expect(transcriptFetches).toHaveLength(1)
    })
  })
})

// Poll retirement (unified-thread-catalog.md, item 5): app.svelte.js's
// refreshSessions rides this same runtime nudge instead of its own 5s poll.
// addRuntimeListener is the seam — pin its dispatch and error isolation here,
// independent of app.svelte.js.
describe('addRuntimeListener', () => {
  const unsubscribes = []
  function register(fn) {
    unsubscribes.push(addRuntimeListener(fn))
  }

  afterEach(() => {
    while (unsubscribes.length) unsubscribes.pop()()
  })

  it('a runtime event invokes every registered listener', async () => {
    const heard = []
    register(() => heard.push('a'))
    register(() => heard.push('b'))
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    es.emit('runtime', { data: '{}' })
    await flush()
    expect(heard).toEqual(['a', 'b'])

    unsubscribes.pop()() // drop the 'b' listener only
    heard.length = 0
    es.emit('runtime', { data: '{}' })
    await flush()
    expect(heard).toEqual(['a']) // unsubscribed listener no longer fires
  })

  it('a throwing listener does not stop the window refetch or other listeners', async () => {
    const heard = []
    register(() => {
      throw new Error('boom')
    })
    register(() => heard.push('after'))
    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    es.emit('runtime', { data: '{}' })
    await flush()

    expect(heard).toEqual(['after']) // isolated: the throw didn't skip the next listener
    expect(threadsFetches(fetchMock)).toHaveLength(1) // and loadDataset still ran
  })
})

// A delta names only the threads whose runtime changed. Applied as a snapshot,
// one small delta cleared every other row's state.
describe('partial runtime deltas', () => {
  it('leaves unnamed rows alone and clears a row named with null', async () => {
    explorer.dataset = [
      { id: 'nth_1', provider: 'codex', nativeId: 'native-1', runtime: { attached: true, status: 'ready', turnActive: true } },
      { id: 'nth_2', provider: 'codex', nativeId: 'native-2', runtime: { attached: true, status: 'ready', turnActive: false } },
    ]
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    es.emit('runtime', { data: JSON.stringify({ rows: [{ provider: 'codex', nativeId: 'native-2', runtime: null }] }) })
    await flush()

    expect(explorer.dataset[0].runtime).toEqual({ attached: true, status: 'ready', turnActive: true })
    expect(explorer.dataset[1].runtime).toBeUndefined()
  })
})
