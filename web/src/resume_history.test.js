// Resume metadata and failure state survive retirement of the history bridge.
import { describe, it, expect, vi, afterEach } from 'vitest'
import {
  getSessionStore,
  dropSessionStore,
  syncSessionSummaries,
} from './state.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    this.closed = false
    MockEventSource.instances.push(this)
  }
  close() {
    this.closed = true
  }
  message(evt) {
    this.onmessage?.({ data: JSON.stringify(evt) })
  }
}
MockEventSource.instances = []

const flush = async () => {
  for (let i = 0; i < 4; i++) await new Promise((resolve) => setTimeout(resolve))
}

const headers = { get: () => null }

// Resume metadata carries status and an initially empty native snapshot.
function codexMeta(status = 'starting') {
  return {
    provider: 'codex',
    threadId: 'nat-1',
    cwd: '/w',
    status,
    thread: { model: 'gpt-x', cwd: '/w', thread: { id: 'nat-1', turns: [] } },
  }
}

// Resume tests read session metadata.
function stubFetch({ meta = codexMeta() } = {}) {
  const fetchMock = vi.fn(async (url) => {
    const u = String(url)
    if (u.endsWith('/state')) return { ok: true, headers, json: async () => meta }
    return { ok: true, headers, json: async () => ({}) }
  })
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const openSessions = []
function openStore(id) {
  openSessions.push(id)
  return getSessionStore(id)
}

afterEach(() => {
  while (openSessions.length) dropSessionStore(openSessions.pop())

  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('resume metadata', () => {
  it('exited resume surfaces the summary error on the existing lastError chip', async () => {
    stubFetch()
    const session = openStore('rh_exited')
    await flush()

    syncSessionSummaries([{ id: 'rh_exited', status: 'exited', error: 'resume failed: no rollout' }])

    expect(session.store.sessionStatus).toBe('exited')
    expect(session.store.lastError).toBe('resume failed: no rollout')
  })

  it('syncSessionSummaries only touches stores it knows', () => {
    stubFetch()
    const session = openStore('rh_sync')
    expect(() => syncSessionSummaries([{ id: 'rh_sync', status: 'ready' }, { id: 'ghost', status: 'exited' }])).not.toThrow()
    expect(session.store.sessionStatus).toBe('ready')
  })

  it('a thread/resume reply on the wire triggers a prompt /state refetch', async () => {
    vi.useFakeTimers()
    const fetchMock = stubFetch()
    const session = openStore('rh_reply')
    await vi.advanceTimersByTimeAsync(0)
    const stateCalls = () => fetchMock.mock.calls.filter(([u]) => String(u).endsWith('/state')).length
    expect(stateCalls()).toBe(1)

    const sessionFeed = () => {
      session.connectFeed()
      return MockEventSource.instances.find((es) => es.url.endsWith('/api/sessions/rh_reply/events'))
    }
    const es = sessionFeed()
    es.message({ seq: 1, ts: 0, dir: 'send', msg: { jsonrpc: '2.0', id: 7, method: 'thread/resume', params: {} } })
    es.message({ seq: 2, ts: 0, dir: 'recv', msg: { jsonrpc: '2.0', id: 7, result: {} } })
    await vi.advanceTimersByTimeAsync(1000)

    expect(stateCalls()).toBe(2)
    expect(session.store.sessionStatus).toBe('starting') // from the refreshed /state
  })
})
