// Stream-vs-fallback tests (Slice D, gate D2): a healthy SSE stream drives
// updates by push with the status poll OFF; an EventSource error resumes the
// fallback poll; a reopen turns the poll back off. EventSource is stubbed
// because the node test environment has none.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { __resetSidebarBootstrapForTest, bootstrapSidebar, explorer, scheduleActiveRefresh, startPolling, stopPolling } from './explorer.svelte.js'

const IDLE_POLL_MS = 10000
const readyStatus = { state: 'ready', catalogVersion: null, roots: [] }

// MockEventSource records listeners and lets a test drive open/error/catalog.
class MockEventSource {
  static CLOSED = 2
  constructor(url) {
    this.url = url
    this.listeners = {}
    this.closed = false
    this.readyState = 0
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

function statusFetchCount(fetchMock) {
  return fetchMock.mock.calls.filter(([url]) => String(url).includes('/thread-index/status')).length
}

let fetchMock

beforeEach(() => {
  vi.useFakeTimers()
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('document', {
    visibilityState: 'visible',
    addEventListener: () => {},
    removeEventListener: () => {},
  })
  fetchMock = vi.fn(async (url) => {
    if (String(url).includes('/api/v1/sidebar/threads')) {
      return { ok: true, status: 200, json: async () => ({ items: explorer.activeItems, historicalCount: 0, catalogVersion: explorer.catalogVersion ?? 0 }) }
    }
    return { ok: true, status: 200, json: async () => readyStatus }
  })
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  stopPolling()
  __resetSidebarBootstrapForTest()
  vi.unstubAllGlobals()
  vi.useRealTimers()
  explorer.index = null
  explorer.indexError = null
  explorer.dataset = []
  explorer.datasetComplete = false
  explorer.datasetScope = 'empty'
  explorer.activeItems = []
  explorer.windowItems = []
  explorer.catalogVersion = null
})

describe('catalog stream vs poll fallback', () => {
  it('opens one EventSource and does not poll status while the stream is healthy', async () => {
    startPolling()
    expect(MockEventSource.instances).toHaveLength(1)
    expect(MockEventSource.instances[0].url).toBe('/api/v1/thread-index/events')

    MockEventSource.instances[0].emit('open')
    await vi.advanceTimersByTimeAsync(IDLE_POLL_MS * 3)
    expect(statusFetchCount(fetchMock)).toBe(0) // healthy stream → zero polls
  })

  it('a catalog event refreshes the status strip without a timer', async () => {
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    es.emit('catalog', { data: '{"catalogVersion": 7}' })
    await vi.advanceTimersByTimeAsync(0)
    expect(statusFetchCount(fetchMock)).toBe(1)
    expect(explorer.index?.state).toBe('ready')
  })

  it('falls back to polling on stream error and stops again when it reopens', async () => {
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')

    // Stream breaks → the fallback poll resumes at the current cadence.
    es.emit('error')
    await vi.advanceTimersByTimeAsync(IDLE_POLL_MS)
    expect(statusFetchCount(fetchMock)).toBe(1)
    await vi.advanceTimersByTimeAsync(IDLE_POLL_MS)
    expect(statusFetchCount(fetchMock)).toBe(2) // still polling while broken

    // Reopen → the fallback poll stops; no further status fetches.
    es.emit('open')
    await vi.advanceTimersByTimeAsync(IDLE_POLL_MS * 3)
    expect(statusFetchCount(fetchMock)).toBe(2)
  })

  // Failure caught (2026-10-19): Nano restarted behind Tailscale Serve, whose
  // 502 made the browser close the catalog stream for good. The page polled
  // on, but its rail never heard again, so working rows read "Offline — last
  // seen working" until a reload. Boundary: the catalog stream's lifecycle.
  it('opens a new stream when the browser closed the old one after an error', async () => {
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    expect(explorer.link.up).toBe(true)

    es.readyState = MockEventSource.CLOSED // an HTTP error on reconnect
    es.emit('error')
    expect(explorer.link.up).toBe(false)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(es.closed).toBe(true)
    expect(MockEventSource.instances).toHaveLength(2)

    MockEventSource.instances[1].emit('open')
    expect(explorer.link.up).toBe(true)
  })

  it('leaves a stream the browser is still retrying to the browser', async () => {
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    es.readyState = 0 // CONNECTING: the browser retries on its own
    es.emit('error')
    await vi.advanceTimersByTimeAsync(10_000)
    expect(MockEventSource.instances).toHaveLength(1)
  })

  it('stopPolling closes the stream and cancels any fallback timer', async () => {
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('error') // schedule a fallback poll
    stopPolling()
    expect(es.closed).toBe(true)
    await vi.advanceTimersByTimeAsync(IDLE_POLL_MS * 2)
    expect(statusFetchCount(fetchMock)).toBe(0)
  })
})

describe('active-boundary fleet transitions', () => {
  const activeFetches = () => fetchMock.mock.calls.filter(([url]) => String(url) === '/api/v1/sidebar/threads')

  it('removes a needs_you row after its transition to idle', async () => {
    const row = { id: 'nth_leave', provider: 'codex', nativeId: 'leave', fleet: { label: 'needs_you' }, sidebar: { active: true } }
    explorer.activeItems = [row]
    explorer.dataset = [row]
    explorer.datasetScope = 'active'
    explorer.catalogVersion = 1
    fetchMock.mockImplementation(async (url) => {
      if (String(url) === '/api/v1/sidebar/threads') {
        return { ok: true, status: 200, json: async () => ({ items: [], historicalCount: 1, catalogVersion: 2 }) }
      }
      return { ok: true, status: 200, json: async () => readyStatus }
    })
    startPolling()

    MockEventSource.instances[0].emit('fleet', { data: '{"threadId":"nth_leave","from":"needs_you","to":"idle"}' })
    await vi.advanceTimersByTimeAsync(249)
    expect(activeFetches()).toHaveLength(0)
    await vi.advanceTimersByTimeAsync(1)

    expect(activeFetches()).toHaveLength(1)
    expect(explorer.activeItems).toEqual([])
    expect(explorer.dataset).toEqual([])
  })

  it('inserts an unknown row after its transition from idle to working', async () => {
    const arriving = { id: 'nth_enter', provider: 'codex', nativeId: 'enter', fleet: { label: 'working' }, sidebar: { active: true } }
    explorer.datasetScope = 'active'
    explorer.catalogVersion = 1
    fetchMock.mockImplementation(async (url) => {
      if (String(url) === '/api/v1/sidebar/threads') {
        return { ok: true, status: 200, json: async () => ({ items: [arriving], historicalCount: 0, catalogVersion: 2 }) }
      }
      return { ok: true, status: 200, json: async () => readyStatus }
    })
    startPolling()

    MockEventSource.instances[0].emit('fleet', { data: '{"threadId":"nth_enter","from":"idle","to":"working"}' })
    await vi.advanceTimersByTimeAsync(250)

    expect(activeFetches()).toHaveLength(1)
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_enter'])
    expect(explorer.dataset.map((row) => row.id)).toEqual(['nth_enter'])
  })
})

describe('mid-flight version bumps defer, never drop', () => {
  it('a bump during an in-flight window load triggers one catch-up load on completion', async () => {
    // fetch: window loads resolve controllably; status fetches resolve fast.
    let resolveLoad
    const loads = []
    fetchMock.mockImplementation(async (url) => {
      const u = String(url)
      if (u.includes('/api/v1/sidebar/threads')) {
        return { ok: true, status: 200, json: async () => ({ items: [], historicalCount: 0, catalogVersion: explorer.catalogVersion ?? 0 }) }
      }
      if (u.includes('/api/v1/threads?')) {
        loads.push(u)
        return await new Promise((res) => { resolveLoad = (body) => res({ ok: true, json: async () => body }) })
      }
      return { ok: true, status: 200, json: async () => readyStatus }
    })

    explorer.dataset = []
    explorer.catalogVersion = 100
    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    startPolling()
    const es = MockEventSource.instances.at(-1)
    es.emit('open')

    // bump 101 → starts a load
    es.emit('catalog', { data: JSON.stringify({ catalogVersion: 101 }) })
    await Promise.resolve()
    expect(loads).toHaveLength(1)

    // bump 102 arrives mid-flight → deferred, no second request yet
    es.emit('catalog', { data: JSON.stringify({ catalogVersion: 102 }) })
    await Promise.resolve()
    expect(loads).toHaveLength(1)

    // the in-flight load resolves carrying only version 101 → catch-up load fires
    resolveLoad({ items: [{ id: 'a' }], nextCursor: null, catalogVersion: 101 })
    await vi.waitFor(() => expect(loads).toHaveLength(2))

    // catch-up resolves at 102; no further loads
    resolveLoad({ items: [{ id: 'a' }, { id: 'b' }], nextCursor: null, catalogVersion: 102 })
    await vi.waitFor(() => expect(explorer.catalogVersion).toBe(102))
    expect(loads).toHaveLength(2)
  })

  it('no catch-up when the in-flight response already covers the deferred version', async () => {
    let resolveLoad
    const loads = []
    fetchMock.mockImplementation(async (url) => {
      const u = String(url)
      if (u.includes('/api/v1/sidebar/threads')) {
        return { ok: true, status: 200, json: async () => ({ items: [], historicalCount: 0, catalogVersion: explorer.catalogVersion ?? 0 }) }
      }
      if (u.includes('/api/v1/threads?')) {
        loads.push(u)
        return await new Promise((res) => { resolveLoad = (body) => res({ ok: true, json: async () => body }) })
      }
      return { ok: true, status: 200, json: async () => readyStatus }
    })

    explorer.dataset = []
    explorer.catalogVersion = 100
    explorer.datasetComplete = true
    explorer.datasetScope = 'window'
    startPolling()
    const es = MockEventSource.instances.at(-1)
    es.emit('open')

    es.emit('catalog', { data: JSON.stringify({ catalogVersion: 101 }) })
    await Promise.resolve()
    es.emit('catalog', { data: JSON.stringify({ catalogVersion: 102 }) })
    await Promise.resolve()
    expect(loads).toHaveLength(1)

    // response already carries 102 → deferred bump is satisfied, no extra load
    resolveLoad({ items: [{ id: 'a' }], nextCursor: null, catalogVersion: 102 })
    await vi.waitFor(() => expect(explorer.catalogVersion).toBe(102))
    expect(loads).toHaveLength(1)
  })
})

// The server pushes the rows that changed as `sidebar` events. Until the page
// applied them, every runtime and catalog event re-read the whole list.
describe('sidebar events patch the active list', () => {
  const activeFetches = () => fetchMock.mock.calls.filter(([url]) => String(url) === '/api/v1/sidebar/threads')
  const rowA = { id: 'nth_a', provider: 'codex', nativeId: 'a', sidebar: { active: true } }
  const rowB = { id: 'nth_b', provider: 'codex', nativeId: 'b', sidebar: { active: true } }

  // A connect marker at an unknown revision reads the list once, which sets
  // the revision the following events are judged against.
  async function openAtRevision(revision, items) {
    fetchMock.mockImplementation(async (url) => {
      if (String(url) === '/api/v1/sidebar/threads') {
        return { ok: true, status: 200, json: async () => ({ items, revision, historicalCount: 0, catalogVersion: 1 }) }
      }
      return { ok: true, status: 200, json: async () => readyStatus }
    })
    explorer.datasetScope = 'active'
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    es.emit('sidebar', { data: JSON.stringify({ revision, refresh: true, historicalCount: 0, catalogVersion: 1 }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(activeFetches()).toHaveLength(1)
    return es
  }

  it('applies the next revision without a read: rows replace, join and leave', async () => {
    const es = await openAtRevision(5, [rowA])
    const changedA = { ...rowA, name: { value: 'renamed' } }
    es.emit('sidebar', { data: JSON.stringify({ revision: 6, rows: [changedA, rowB], removedIds: [], historicalCount: 3, catalogVersion: 2 }) })
    expect(explorer.activeItems.map((row) => [row.id, row.name?.value])).toEqual([['nth_a', 'renamed'], ['nth_b', undefined]])
    expect(explorer.dataset.map((row) => row.id)).toEqual(['nth_a', 'nth_b'])
    expect(explorer.historicalCount).toBe(3)
    expect(explorer.catalogVersion).toBe(2)

    es.emit('sidebar', { data: JSON.stringify({ revision: 7, rows: [], removedIds: ['nth_a'], historicalCount: 4, catalogVersion: 3 }) })
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_b'])
    await vi.advanceTimersByTimeAsync(250)
    expect(activeFetches()).toHaveLength(1)
  })

  it('a skipped revision or a marker past the known one reads once; a marker at it does not', async () => {
    const es = await openAtRevision(5, [rowA])
    es.emit('sidebar', { data: JSON.stringify({ revision: 5, refresh: true }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(activeFetches()).toHaveLength(1)

    es.emit('sidebar', { data: JSON.stringify({ revision: 8, rows: [rowB], removedIds: [] }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(activeFetches()).toHaveLength(2)
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_a']) // the read, not the gap event, decides

    es.emit('sidebar', { data: JSON.stringify({ revision: 9, refresh: true }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(activeFetches()).toHaveLength(3)
  })

  it('runtime and catalog events no longer re-read the active list', async () => {
    const es = await openAtRevision(5, [rowA])
    es.emit('runtime', { data: JSON.stringify({ rows: [{ provider: 'grok', nativeId: 'background', runtime: { detached: true } }], refresh: true }) })
    es.emit('catalog', { data: JSON.stringify({ catalogVersion: 9, refresh: true }) })
    es.emit('catalog', { data: JSON.stringify({ catalogVersion: 10, threadIds: ['nth_unknown'] }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(activeFetches()).toHaveLength(1)
  })
})

// A stream drop ends the revision sequence: a restarted Nano counts from 1
// again, so a marker at the old number proves nothing. A read that began
// before a delta is older than it.
describe('sidebar list after a drop and around a read', () => {
  const activeFetches = () => fetchMock.mock.calls.filter(([url]) => String(url) === '/api/v1/sidebar/threads')
  const rowA = { id: 'nth_a', provider: 'codex', nativeId: 'a', sidebar: { active: true }, runtime: { attached: true, threadStatus: 'working' } }
  const rowB = { id: 'nth_b', provider: 'codex', nativeId: 'b', sidebar: { active: true } }
  const listing = (revision, items) => ({ ok: true, status: 200, json: async () => ({ items, revision, historicalCount: 0, catalogVersion: 1 }) })

  it('reads the list again after a drop even at the same revision', async () => {
    fetchMock.mockImplementation(async (url) => (String(url) === '/api/v1/sidebar/threads' ? listing(1, [rowA]) : { ok: true, status: 200, json: async () => readyStatus }))
    explorer.datasetScope = 'active'
    explorer.link = { epoch: 0, up: true }
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    es.emit('sidebar', { data: JSON.stringify({ revision: 1, refresh: true }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(activeFetches()).toHaveLength(1)

    es.emit('error')
    fetchMock.mockImplementation(async (url) => (String(url) === '/api/v1/sidebar/threads' ? listing(1, [rowB]) : { ok: true, status: 200, json: async () => readyStatus }))
    es.emit('open')
    es.emit('sidebar', { data: JSON.stringify({ revision: 1, refresh: true }) })
    await vi.advanceTimersByTimeAsync(250)

    expect(activeFetches()).toHaveLength(2)
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_b'])
  })

  it('a read that began before a delta cannot roll the delta back', async () => {
    fetchMock.mockImplementation(async (url) => (String(url) === '/api/v1/sidebar/threads' ? listing(5, [rowA]) : { ok: true, status: 200, json: async () => readyStatus }))
    explorer.datasetScope = 'active'
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    es.emit('sidebar', { data: JSON.stringify({ revision: 4, refresh: true }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(activeFetches()).toHaveLength(1)

    let release
    fetchMock.mockImplementation((url) => (String(url) === '/api/v1/sidebar/threads' ? new Promise((resolve) => { release = resolve }) : Promise.resolve({ ok: true, status: 200, json: async () => readyStatus })))
    es.emit('sidebar', { data: JSON.stringify({ revision: 9, refresh: true }) }) // a gap: one read starts
    await vi.advanceTimersByTimeAsync(250)
    es.emit('sidebar', { data: JSON.stringify({ revision: 6, rows: [{ ...rowA, name: { value: 'new' } }], removedIds: [] }) })
    expect(explorer.activeItems[0].name?.value).toBe('new')
    release(listing(5, [rowA]))
    await vi.advanceTimersByTimeAsync(0)

    expect(explorer.activeItems[0].name?.value).toBe('new')
  })
})

// Only a read that began on the live stream and lands on it may start the
// revision sequence. The bootstrap read and a read begun during an outage
// are not such reads, and a failed gap repair stays owed.
describe('reads that cannot start the sidebar sequence', () => {
  const rowA = { id: 'nth_a', provider: 'codex', nativeId: 'a', sidebar: { active: true } }
  const rowB = { id: 'nth_b', provider: 'codex', nativeId: 'b', sidebar: { active: true } }
  const listing = (revision, items) => ({ ok: true, status: 200, json: async () => ({ items, revision, historicalCount: 0, catalogVersion: 1 }) })
  const serve = (revision, items) => fetchMock.mockImplementation(async (url) => (String(url) === '/api/v1/sidebar/threads' ? listing(revision, items) : { ok: true, status: 200, json: async () => readyStatus }))

  it('a bootstrap read held past a reconnect cannot overwrite the new sequence', async () => {
    serve(50, [rowA])
    explorer.link = { epoch: 0, up: true }
    let releaseSessions
    const boot = bootstrapSidebar(new Promise((resolve) => { releaseSessions = resolve }))
    await vi.advanceTimersByTimeAsync(0)
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    es.emit('error')
    serve(1, [rowB])
    es.emit('open')
    es.emit('sidebar', { data: JSON.stringify({ revision: 1, refresh: true }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_b'])

    releaseSessions()
    await boot
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_b'])
    es.emit('sidebar', { data: JSON.stringify({ revision: 2, rows: [rowA], removedIds: [] }) })
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_b', 'nth_a']) // the sequence is the new server's
  })

  it('a read begun during an outage cannot restart the old sequence', async () => {
    serve(50, [rowA])
    explorer.link = { epoch: 0, up: true }
    await bootstrapSidebar(Promise.resolve())
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    es.emit('error')
    scheduleActiveRefresh()
    await vi.advanceTimersByTimeAsync(250)
    serve(1, [rowB])
    es.emit('open')
    es.emit('sidebar', { data: JSON.stringify({ revision: 1, refresh: true }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_b'])
  })

  it('a failed gap repair is retried until a read lands', async () => {
    serve(5, [rowA])
    explorer.link = { epoch: 0, up: true }
    await bootstrapSidebar(Promise.resolve())
    startPolling()
    const es = MockEventSource.instances[0]
    es.emit('open')
    fetchMock.mockImplementation(async (url) => {
      if (String(url) === '/api/v1/sidebar/threads') throw new TypeError('temporary read failure')
      return { ok: true, status: 200, json: async () => readyStatus }
    })
    es.emit('sidebar', { data: JSON.stringify({ revision: 7, refresh: true }) })
    await vi.advanceTimersByTimeAsync(250)
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_a'])
    serve(7, [rowB])
    await vi.advanceTimersByTimeAsync(6000)
    expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_b'])
  })
})

// The bootstrap read lands before the first stream connection, so a restart
// between the two causes no stream error. The server names its process on
// the list and on every sidebar event; a marker from another process is a
// gap even at the same revision, and the new process's lower revision is not
// "older" than the old one's.
describe('a restart between the bootstrap read and the first connection', () => {
  const rowA = { id: 'nth_a', provider: 'codex', nativeId: 'a', sidebar: { active: true } }
  const rowB = { id: 'nth_b', provider: 'codex', nativeId: 'b', sidebar: { active: true } }
  const serve = (generation, revision, items) => fetchMock.mockImplementation(async (url) => (String(url) === '/api/v1/sidebar/threads'
    ? { ok: true, status: 200, json: async () => ({ items, generation, revision, historicalCount: 0, catalogVersion: 1 }) }
    : { ok: true, status: 200, json: async () => readyStatus }))

  for (const oldRevision of [1, 50]) {
    it(`reads the new process's list after bootstrapping from the old one at revision ${oldRevision}`, async () => {
      serve('old', oldRevision, [rowA])
      explorer.link = { epoch: 0, up: true }
      await bootstrapSidebar(Promise.resolve())
      expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_a'])
      serve('new', 1, [rowB])
      startPolling()
      const es = MockEventSource.instances[0]
      es.emit('open')
      es.emit('sidebar', { data: JSON.stringify({ generation: 'new', revision: 1, refresh: true }) })
      await vi.advanceTimersByTimeAsync(250)
      expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_b'])
      es.emit('sidebar', { data: JSON.stringify({ generation: 'new', revision: 2, rows: [rowA], removedIds: [] }) })
      expect(explorer.activeItems.map((row) => row.id)).toEqual(['nth_b', 'nth_a'])
    })
  }
})
