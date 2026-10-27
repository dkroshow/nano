// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  __resetBrowserDiagnosticsForTest,
  classifyJavaScriptFailure,
  flushBrowserDiagnostics,
  recordBrowserDiagnostic,
  startBrowserDiagnostics,
} from './browser_diagnostics.js'

describe('browser diagnostics flight recorder', () => {
  beforeEach(() => {
    __resetBrowserDiagnosticsForTest()
    sessionStorage.clear()
    document.head.innerHTML = '<script type="module" src="/assets/index-test123.js"></script>'
  })

  afterEach(() => {
    __resetBrowserDiagnosticsForTest()
    vi.unstubAllGlobals()
    vi.useRealTimers()
  })

  it('uploads only the content-free schema with the loaded bundle hash', async () => {
    const requests = []
    vi.stubGlobal('fetch', vi.fn(async (_url, options) => {
      requests.push(JSON.parse(options.body))
      return { ok: true, json: async () => ({ accepted: 1, bootId: 'boot-1' }) }
    }))

    recordBrowserDiagnostic('projection.message', {
      threadId: 'thread-1', sessionId: 'session-1', revision: 42,
      trigger: 'sidebar', firstRowsMs: 0, newestRowsMs: 32, historyMs: 8, historyBytes: 1024,
      view: 'enhanced', boundaryName: 'b_0123456789abcdef',
      interactionId: 'request-5', op: 'item.complete', text: 'must never leave the tab',
      activeRenderSource: 'messageItems', visibleItemCount: 3, matchingDOMActionRowCount: 0,
      expectedRowCount: 4, mountedRowCount: 3, missingRowCount: 1, extraRowCount: 0,
      duplicateRowCount: 0, semanticDuplicateRowCount: 1, blankRowCount: 0, typeMismatchCount: 0, orderMismatchCount: 0,
      arrivalRateCps: 275, batchIntervalMs: 121, batchSize: 33,
      layoutSequence: 4, rowIndex: 0, previousRowIndex: 3, topPx: 0, previousTopPx: 120,
      movementPx: 120, movementCount: 7, maxMovementPx: 240, reorderCount: 5,
      addedCount: 1, removedCount: 2, workingCount: 3, starredCount: 1,
      scrollTop: 0, scrollHeight: 1200, viewportHeight: 640, warm: true,
    })
    await flushBrowserDiagnostics()

    expect(requests).toHaveLength(1)
    expect(requests[0].events[0]).toMatchObject({
      kind: 'projection.message', threadId: 'thread-1', sessionId: 'session-1',
      trigger: 'sidebar', firstRowsMs: 0, newestRowsMs: 32, historyMs: 8, historyBytes: 1024,
      revision: 42, interactionId: 'request-5',
      view: 'enhanced', boundaryName: 'b_0123456789abcdef', op: 'item.complete', build: 'index-test123.js',
      activeRenderSource: 'messageItems', visibleItemCount: 3, matchingDOMActionRowCount: 0,
      expectedRowCount: 4, mountedRowCount: 3, missingRowCount: 1, extraRowCount: 0,
      duplicateRowCount: 0, semanticDuplicateRowCount: 1, blankRowCount: 0, typeMismatchCount: 0, orderMismatchCount: 0,
      arrivalRateCps: 275, batchIntervalMs: 121, batchSize: 33,
      layoutSequence: 4, rowIndex: 0, previousRowIndex: 3, topPx: 0, previousTopPx: 120,
      movementPx: 120, movementCount: 7, maxMovementPx: 240, reorderCount: 5,
      addedCount: 1, removedCount: 2, workingCount: 3, starredCount: 1,
      scrollTop: 0, scrollHeight: 1200, viewportHeight: 640, warm: true,
    })
    expect(requests[0].events[0]).not.toHaveProperty('text')
  })

  it('classifies renderer failures without retaining the error message', () => {
    expect(classifyJavaScriptFailure(new Error('Keyed each block has duplicate key msg-secret')))
      .toBe('duplicate_key')
    expect(classifyJavaScriptFailure(new TypeError('operator prompt must not be stored'))).toBe('typeerror')
  })

  it('replays the bounded tab history after a hard-refresh-style module reset', async () => {
    const requests = []
    vi.stubGlobal('fetch', vi.fn(async (_url, options) => {
      requests.push(JSON.parse(options.body))
      return { ok: true, json: async () => ({ accepted: 1, bootId: requests.length === 1 ? 'boot-1' : 'boot-2' }) }
    }))

    const first = recordBrowserDiagnostic('navigation.commit', { threadId: 'thread-1' })
    await flushBrowserDiagnostics()
    __resetBrowserDiagnosticsForTest() // sessionStorage survives a real reload
    const second = recordBrowserDiagnostic('page.load', { visible: true })
    await flushBrowserDiagnostics()

    expect(second.tabId).toBe(first.tabId)
    expect(second.clientSeq).toBe(first.clientSeq + 1)
    expect(requests.at(-1).events.map((event) => event.eventId)).toContain(first.eventId)
  })

  // Only the pagehide flush must outlive the page, so only it uses keepalive,
  // and a browser refuses a keepalive body over 64 KiB. A busy tab's 100
  // events are larger than that.
  it('keeps the pagehide flush of a busy tab under the keepalive cap', async () => {
    const requests = []
    vi.stubGlobal('fetch', vi.fn(async (_url, options) => {
      requests.push(options)
      if (options.keepalive && options.body.length > 64 << 10) throw new TypeError('Failed to fetch')
      return { ok: true, json: async () => ({ accepted: 1, bootId: 'boot-1' }) }
    }))
    for (let index = 0; index < 150; index++) {
      recordBrowserDiagnostic('send.visibility', { sessionId: 's'.repeat(160), actionId: 'a'.repeat(160), epoch: 'e'.repeat(160) })
    }

    startBrowserDiagnostics()
    window.dispatchEvent(new Event('pagehide'))

    expect(requests).toHaveLength(1)
    expect(requests[0].keepalive).toBe(true)
    expect(requests[0].body.length).toBeLessThanOrEqual(64 << 10)
    expect(JSON.parse(requests[0].body).events.length).toBeGreaterThan(50)
  })

  it('batches synchronous recorder bursts into one sessionStorage write', () => {
    vi.useFakeTimers()
    const values = new Map()
    const storageWrite = vi.fn((key, value) => values.set(String(key), String(value)))
    vi.stubGlobal('sessionStorage', {
      getItem: (key) => values.get(String(key)) ?? null,
      setItem: storageWrite,
      clear: () => values.clear(),
    })
    __resetBrowserDiagnosticsForTest()

    recordBrowserDiagnostic('registry.session', { sessionId: 'session-1' })
    recordBrowserDiagnostic('registry.session', { sessionId: 'session-2' })
    recordBrowserDiagnostic('registry.session', { sessionId: 'session-3' })

    expect(storageWrite).not.toHaveBeenCalled()
    vi.runOnlyPendingTimers()
    expect(storageWrite).toHaveBeenCalledTimes(1)
  })
})
