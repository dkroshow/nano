// @vitest-environment jsdom
//
// The header's connection indicator, which the operator reads over a plane's
// wifi: Nano's latency and throughput while Nano answers, Offline the moment
// the connection drops, and no requests at all while the page is hidden.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, waitFor } from '@testing-library/svelte'
import ConnectionStatus from './ConnectionStatus.svelte'
import { startConnectionChecks } from '../connection.svelte.js'
import { startPolling as startCatalogStream, stopPolling as stopCatalogStream } from '../explorer.svelte.js'
import { startWarmTranscripts } from './warm_transcripts.js'
import { dropSessionStore, getSessionStore } from '../state.svelte.js'

// The app-wide catalog stream, driven by hand.
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

let stop = () => {}

afterEach(() => {
  stop()
  stop = () => {}
  stopCatalogStream()
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  vi.useRealTimers()
})

function stubNano() {
  const fetchMock = vi.fn(async (url) => {
    const u = String(url)
    if (u === '/api/v1/connection-test') return { ok: true, status: 200, arrayBuffer: async () => new ArrayBuffer(64 << 10) }
    if (u === '/api/v1/build') return { ok: true, status: 200, json: async () => ({ stamp: 'test' }), text: async () => '{}' }
    return { ok: true, status: 200, json: async () => ({}), text: async () => '{}' }
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('connection indicator', () => {
  it('shows latency and throughput while Nano answers, and Offline when the connection drops', async () => {
    stubNano()
    stop = startConnectionChecks()
    const { container } = render(ConnectionStatus)
    const indicator = () => container.querySelector('[data-testid="connection-status"]')

    await waitFor(() => expect(indicator().textContent).toMatch(/\d+ ms/))
    await waitFor(() => expect(indicator().textContent).toMatch(/Mbps/))
    expect(indicator().classList.contains('connected')).toBe(true)

    window.dispatchEvent(new Event('offline'))
    await waitFor(() => expect(indicator().textContent).toContain('Offline'))
    expect(indicator().classList.contains('offline')).toBe(true)
  })

  // "lets us know in near real time if there's a live connection": the age
  // of the last real contact with Nano, counted every second by the page's
  // own clock, never by asking Nano more often.
  it('counts the seconds since Nano last answered, without asking Nano again', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const fetchMock = stubNano()
    stop = startConnectionChecks()
    const { container } = render(ConnectionStatus)
    const indicator = () => container.querySelector('[data-testid="connection-status"]')
    await waitFor(() => expect(indicator().textContent).toContain('Last success: 0s ago'))
    const requests = fetchMock.mock.calls.length

    await vi.advanceTimersByTimeAsync(4_500)
    expect(indicator().textContent).toContain('Last success: 4s ago')
    expect(fetchMock.mock.calls.length).toBe(requests)
  })

  // Nano is asked only to fill a silence: while its streams keep answering it
  // is not asked at all (within the 30 s latency refresh), and when they go
  // quiet it is asked at most once every 5 s.
  it('asks Nano only after 5 s with no other answer, and never more often', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    MockEventSource.instances = []
    vi.stubGlobal('EventSource', MockEventSource)
    const fetchMock = stubNano()
    stop = startConnectionChecks()
    startCatalogStream()
    render(ConnectionStatus)
    const probes = () => fetchMock.mock.calls.filter(([url]) => url === '/api/v1/build').length
    await waitFor(() => expect(probes()).toBe(1))
    const stream = MockEventSource.instances.find((source) => source.url.includes('/thread-index/events'))

    for (let i = 0; i < 10; i += 1) {
      await vi.advanceTimersByTimeAsync(2_000)
      stream.emit('fleet', { data: JSON.stringify({}) })
    }
    expect(probes()).toBe(1)

    await vi.advanceTimersByTimeAsync(9_000)
    expect(probes()).toBe(2)
    await vi.advanceTimersByTimeAsync(5_000)
    expect(probes()).toBe(3)
  })

  it('counts an event on Nano\'s stream as contact', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    MockEventSource.instances = []
    vi.stubGlobal('EventSource', MockEventSource)
    const fetchMock = stubNano()
    stop = startConnectionChecks()
    startCatalogStream()
    const { container } = render(ConnectionStatus)
    const indicator = () => container.querySelector('[data-testid="connection-status"]')
    await waitFor(() => expect(indicator().textContent).toContain('Last success: 0s ago'))
    // Offline, so the quiet-time check fails and the age keeps counting.
    fetchMock.mockImplementation(async () => { throw new TypeError('Failed to fetch') })
    await vi.advanceTimersByTimeAsync(20_500)
    expect(indicator().textContent).toContain('Last success: 20s ago')

    MockEventSource.instances.find((source) => source.url.includes('/thread-index/events'))
      .emit('fleet', { data: JSON.stringify({}) })
    await waitFor(() => expect(indicator().textContent).toContain('Last success: 0s ago'))
  })

  // Coming back into view or online asks for a read, but not sooner than 5 s
  // after the last one.
  it('keeps 5 s between reads when the page is shown again or goes online', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const fetchMock = stubNano()
    stop = startConnectionChecks()
    render(ConnectionStatus)
    const probes = () => fetchMock.mock.calls.filter(([url]) => url === '/api/v1/build').length
    await waitFor(() => expect(probes()).toBe(1))

    await vi.advanceTimersByTimeAsync(1_000)
    document.dispatchEvent(new Event('visibilitychange'))
    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(1_000)
    expect(probes()).toBe(1)
    await vi.advanceTimersByTimeAsync(3_100)
    expect(probes()).toBe(2)
  })

  // A stream answer after the page comes back stands in for the read the
  // page would have made: Nano is asked only after 5 s with no answer.
  it('lets a stream answer after the page is shown again stand in for a read', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    MockEventSource.instances = []
    vi.stubGlobal('EventSource', MockEventSource)
    const fetchMock = stubNano()
    stop = startConnectionChecks()
    startCatalogStream()
    render(ConnectionStatus)
    const probes = () => fetchMock.mock.calls.filter(([url]) => url === '/api/v1/build').length
    await waitFor(() => expect(probes()).toBe(1))
    const stream = MockEventSource.instances.find((source) => source.url.includes('/thread-index/events'))

    await vi.advanceTimersByTimeAsync(1_000)
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(3_000)
    stream.emit('fleet', { data: JSON.stringify({}) })
    await vi.advanceTimersByTimeAsync(3_000)
    expect(probes()).toBe(1)
    await vi.advanceTimersByTimeAsync(2_100)
    expect(probes()).toBe(2)
  })

  // Only an answer Nano wrote counts: not a file the browser had cached, and
  // not a gateway answering for Nano.
  it('counts a response Nano sent, not a cached file or a gateway error', async () => {
    let deliver
    vi.stubGlobal('PerformanceObserver', class {
      constructor(callback) { deliver = (entries) => callback({ getEntries: () => entries }) }
      observe() {}
      disconnect() {}
    })
    stubNano().mockImplementation(async () => { throw new TypeError('Failed to fetch') })
    stop = startConnectionChecks()
    const { container } = render(ConnectionStatus)
    const indicator = () => container.querySelector('[data-testid="connection-status"]')
    await waitFor(() => expect(indicator().textContent).toContain('Offline'))
    const entry = (path, extra) => ({ name: `${location.origin}${path}`, initiatorType: 'fetch', responseStatus: 200, transferSize: 900, ...extra })

    deliver([entry('/assets/index.js', { transferSize: 0 })])
    deliver([entry('/api/v1/usage-limits', { responseStatus: 502 })])
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(indicator().textContent).toContain('Offline')

    deliver([entry('/api/v1/usage-limits')])
    await waitFor(() => expect(indicator().textContent).toContain('Last success: 0s ago'))
    expect(indicator().textContent).not.toContain('Offline')
  })

  // Every stream the page holds open to Nano is evidence: the warm stream for
  // hidden tabs and a thread's raw Event Feed too.
  for (const stream of ['warm transcripts', 'raw event feed']) {
    it(`counts an event on the ${stream} stream as contact`, async () => {
      MockEventSource.instances = []
      vi.stubGlobal('EventSource', class extends MockEventSource {})
      stubNano().mockImplementation(async () => { throw new TypeError('Failed to fetch') })
      stop = startConnectionChecks()
      const { container } = render(ConnectionStatus)
      const indicator = () => container.querySelector('[data-testid="connection-status"]')
      await waitFor(() => expect(indicator().textContent).toContain('Offline'))

      if (stream === 'warm transcripts') startWarmTranscripts(() => [])
      else getSessionStore('contact-feed').connectFeed()
      const source = MockEventSource.instances.find((s) => s.url.includes(stream === 'warm transcripts' ? '/warm-transcripts' : '/api/sessions/contact-feed/events'))
      source.onmessage({ data: JSON.stringify({ type: 'set', refs: [] }) })
      await waitFor(() => expect(indicator().textContent).toContain('Last success: 0s ago'))
      dropSessionStore('contact-feed')
    })
  }

  it('makes no request while the page is hidden, and checks at once when it is shown', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const fetchMock = stubNano()
    let visibility = 'hidden'
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => visibility)
    stop = startConnectionChecks()
    render(ConnectionStatus)

    await vi.advanceTimersByTimeAsync(10 * 60_000)
    expect(fetchMock).not.toHaveBeenCalled()

    visibility = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
    await waitFor(() => expect(fetchMock.mock.calls.map(([url]) => url)).toContain('/api/v1/build'))
  })
})
