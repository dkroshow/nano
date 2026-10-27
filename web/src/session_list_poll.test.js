// Poll retirement (unified-thread-catalog.md, item 5): app.svelte.js's
// startPolling no longer runs its own 5s /api/sessions timer — it registers
// refreshSessions on explorer.svelte.js's runtime-listener registry (the same
// nudge that refreshes the catalog window) plus a slow 30s fallback interval
// for a broken stream. EventSource/fetch stubbed as in the explorer stream
// tests so a real SSE 'runtime' event can drive refreshSessions end to end.
import { describe, it, expect, vi, afterEach } from 'vitest'
import { app, startPolling } from './app.svelte.js'
import { startPolling as startCatalogStream, stopPolling as stopCatalogStream } from './explorer.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    this.listeners = {}
    MockEventSource.instances.push(this)
  }
  addEventListener(type, fn) {
    ;(this.listeners[type] ??= []).push(fn)
  }
  removeEventListener(type, fn) {
    this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn)
  }
  close() {}
  emit(type, event = {}) {
    for (const fn of this.listeners[type] ?? []) fn(event)
  }
}
MockEventSource.instances = []

const sessionsFetches = (fetchMock) => fetchMock.mock.calls.filter(([url]) => String(url) === '/api/sessions')
const flush = () => new Promise((resolve) => setTimeout(resolve))

function stubEnvironment() {
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('document', {
    visibilityState: 'visible',
    addEventListener: () => {},
    removeEventListener: () => {},
  })
  const fetchMock = vi.fn(async (url) => {
    const u = String(url)
    if (u.includes('/api/v1/threads?')) return { ok: true, status: 200, json: async () => ({ items: [], nextCursor: null, catalogVersion: 1 }) }
    if (u === '/api/sessions') return { ok: true, json: async () => ({ sessions: [], detached: [], defaults: {}, runtimeStream: { generation: 'g1', seq: 7 } }) }
    return { ok: true, status: 200, json: async () => ({ state: 'ready', catalogVersion: null, roots: [] }) }
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

afterEach(() => {
  stopCatalogStream()
  vi.unstubAllGlobals()
})

describe('sidebar session list rides the runtime nudge', () => {
  // A page load read the list twice: at startup and again when its stream
  // opened, since runtime events were not replayed. The stream now connects
  // from the read's position and the server replays what the read missed, so
  // opening reads nothing; a refresh still reads, and a reconnect names the
  // position the events advanced to.
  it('connects from the list read position and reads again only on a refresh', async () => {
    const fetchMock = stubEnvironment()

    startPolling() // app.svelte.js: the one startup read + registers the listener
    await flush()
    startCatalogStream() // explorer.svelte.js: opens the SSE stream carrying the nudge
    await flush()
    const before = sessionsFetches(fetchMock).length
    expect(before).toBe(1)
    const stream = MockEventSource.instances.at(-1)
    expect(stream.url).toBe('/api/v1/thread-index/events?runtimeGeneration=g1&runtimeSeq=7')

    stream.emit('open')
    await flush()
    expect(sessionsFetches(fetchMock).length).toBe(before)

    stream.emit('runtime', { data: JSON.stringify({ rows: [], refresh: true, seq: 9 }) })
    await flush()
    expect(sessionsFetches(fetchMock).length).toBe(before + 1)

    stream.emit('runtime', { data: JSON.stringify({ rows: [], seq: 12 }) })
    stopCatalogStream()
    startCatalogStream()
    expect(MockEventSource.instances.at(-1).url).toBe('/api/v1/thread-index/events?runtimeGeneration=g1&runtimeSeq=12')
  })

  // The registry is ~1.5 MB. Refetching it on every runtime nudge let six
  // downloads overlap and queue a prompt POST behind them past its 15 s limit.
  it('keeps one session-list fetch in flight and folds a burst into one follow-up', async () => {
    const fetchMock = stubEnvironment()
    startPolling()
    startCatalogStream()
    await flush()
    const releases = []
    fetchMock.mockImplementation(async (url) => {
      if (String(url) === '/api/sessions') {
        await new Promise((resolve) => releases.push(resolve))
        return { ok: true, json: async () => ({ sessions: [], detached: [], defaults: {} }) }
      }
      return { ok: true, status: 200, json: async () => ({ items: [], nextCursor: null, catalogVersion: 1, state: 'ready', roots: [] }) }
    })
    const stream = MockEventSource.instances.at(-1)
    const before = sessionsFetches(fetchMock).length

    for (let i = 0; i < 5; i += 1) stream.emit('runtime', { data: '{}' })
    await flush()
    expect(sessionsFetches(fetchMock).length).toBe(before + 1)

    releases.shift()()
    await flush()
    expect(sessionsFetches(fetchMock).length).toBe(before + 2)

    releases.shift()()
    await flush()
    expect(sessionsFetches(fetchMock).length).toBe(before + 2)
  })
})
