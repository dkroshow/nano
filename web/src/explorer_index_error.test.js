// Supervisor-specified regression tests for the catalog error surface: the
// failed operation (status poll vs refresh POST) is tracked, a background
// status poll must not silently clear a refresh failure, and Retry re-issues
// the operation that actually failed.
import { describe, it, expect, vi, afterEach } from 'vitest'
import {
  explorer, requestRefresh, retryIndexError, startPolling, stopPolling,
} from './explorer.svelte.js'

function deferred() {
  let resolve, reject
  const promise = new Promise((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

const okJson = (data, status = 200) => ({ ok: true, status, json: async () => data })
const errJson = (message, status = 500) => ({ ok: false, status, json: async () => ({ error: { message } }) })
const readyStatus = { state: 'ready', catalogVersion: null, roots: [] }
const flush = () => new Promise((r) => setTimeout(r))

// A catalog SSE event drives the status-strip refresh; stub EventSource (absent
// in the node test environment) so startPolling can open one and the test can
// fire a 'catalog' event to trigger a status fetch.
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

// A catalog event triggers the shared status refresh — the new-model equivalent
// of the old background status poll.
function pushCatalogEvent() {
  MockEventSource.instances.at(-1).emit('catalog', { data: '{}' })
}

function stubEnvironment() {
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('document', {
    visibilityState: 'visible',
    addEventListener: () => {},
    removeEventListener: () => {},
  })
}

// Queue-based fetch stub: request N resolves via deferreds[N-1].
function stubFetchQueue(deferreds) {
  const calls = []
  vi.stubGlobal('fetch', vi.fn((url, opts) => {
    calls.push({ url: String(url), method: opts?.method ?? 'GET' })
    return deferreds[calls.length - 1].promise
  }))
  return calls
}

afterEach(() => {
  stopPolling()
  vi.unstubAllGlobals()
  explorer.indexError = null
})

describe('index error op tracking', () => {
  it('a failed refresh survives a successful status refresh and clears only when the retried POST succeeds', async () => {
    stubEnvironment()
    const refreshFail = deferred()
    const statusOk = deferred()
    const refreshOk = deferred()
    const calls = stubFetchQueue([refreshFail, statusOk, refreshOk])

    // 1. Refresh POST rejects → op 'refresh' + message recorded.
    refreshFail.resolve(errJson('refresh exploded'))
    await requestRefresh()
    expect(explorer.indexError).toEqual({ op: 'refresh', message: 'refresh exploded' })

    // 2. A subsequent successful status refresh (via a catalog event) does NOT clear it.
    statusOk.resolve(okJson(readyStatus))
    startPolling()
    pushCatalogEvent()
    await flush()
    expect(explorer.index?.state).toBe('ready')
    expect(explorer.indexError).toEqual({ op: 'refresh', message: 'refresh exploded' })

    // 3. Retry dispatches to the refresh POST; a 202 clears the error.
    retryIndexError()
    expect(calls[2]).toEqual({ url: '/api/v1/thread-index/refresh', method: 'POST' })
    refreshOk.resolve(okJson({ accepted: true }, 202))
    await flush()
    expect(explorer.indexError).toBeNull()
    expect(explorer.refreshRequested).toBe(true)
  })

  it('a failed status refresh records op status, and Retry re-fetches and clears it on success', async () => {
    stubEnvironment()
    const statusFail = deferred()
    const statusOk = deferred()
    const calls = stubFetchQueue([statusFail, statusOk])

    statusFail.resolve(errJson('status exploded'))
    startPolling()
    pushCatalogEvent()
    await flush()
    expect(explorer.indexError).toEqual({ op: 'status', message: 'status exploded' })

    retryIndexError()
    expect(calls[1]).toEqual({ url: '/api/v1/thread-index/status', method: 'GET' })
    statusOk.resolve(okJson(readyStatus))
    await flush()
    expect(explorer.indexError).toBeNull()
  })
})
