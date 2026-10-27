// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// Fakes for the browser counters the meter reads: an EventSource the test
// can push events into, and a PerformanceObserver the test can feed entries.
class FakeEventSource extends EventTarget {
  constructor(url) {
    super()
    this.url = new URL(url, 'http://nano.test/').href
    this._onmessage = null
  }
  // Like the native handler, it registers without calling the instance's
  // own (possibly overridden) addEventListener.
  set onmessage(fn) {
    if (this._onmessage) EventTarget.prototype.removeEventListener.call(this, 'message', this._onmessage)
    this._onmessage = fn
    if (fn) EventTarget.prototype.addEventListener.call(this, 'message', fn)
  }
  get onmessage() { return this._onmessage }
  push(type, data) {
    this.dispatchEvent(Object.assign(new Event(type), { data, lastEventId: '' }))
  }
}

let observers
class FakePerformanceObserver {
  constructor(callback) { this.callback = callback }
  observe({ type }) { this.type = type; observers.push(this) }
}
// Each observer sees only resource entries of its own type, as in a browser.
const deliver = (entries) => observers.filter((o) => o.type === 'resource').forEach((o) => o.callback({ getEntries: () => entries }))

let nativeFetch
beforeEach(() => {
  vi.resetModules()
  observers = []
  nativeFetch = vi.fn(async () => new Response('{}'))
  vi.stubGlobal('fetch', nativeFetch)
  vi.stubGlobal('EventSource', FakeEventSource)
  vi.stubGlobal('PerformanceObserver', FakePerformanceObserver)
  vi.stubGlobal('location', new URL('http://nano.test/'))
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  localStorage.clear()
})

describe('network meter', () => {
  // The requirement: debug-only and off by default, adding nothing to a
  // normal page. Catches a meter that wraps fetch or streams while off.
  it('installs nothing while off', async () => {
    const meter = await import('./network_meter.js')
    expect(meter.installNetworkMeter()).toBe(false)
    expect(globalThis.fetch).toBe(nativeFetch)
    expect(globalThis.EventSource).toBe(FakeEventSource)
    expect(observers).toHaveLength(0)
  })

  // A stream's bytes arrive as events; its timing entry appears only when a
  // connection ends and repeats the same bytes. Catches double counting
  // (including a failed same-URL attempt taking an open connection's place), a
  // missed onmessage-only stream, failed attempts that vanish, aborted
  // requests counted as sent, and requests missing their header estimate.
  it('counts each stream event once, failed attempts, request timings, and request bodies', async () => {
    localStorage.setItem('nano-network-meter', 'on')
    const meter = await import('./network_meter.js')
    const H = meter.SENT_HEADER_ESTIMATE
    expect(meter.installNetworkMeter()).toBe(true)

    let now = 0
    vi.spyOn(performance, 'now').mockImplementation(() => now)
    const live = new EventSource('/api/v1/hosted-threads/nht_0123456789abcdef/live?limit=40')
    live.onmessage = () => {}
    live.addEventListener('rows', () => {})
    deliver([{ name: live.url, initiatorType: 'other', responseStatus: 503, transferSize: 650, startTime: 0, responseStart: 50, responseEnd: 60 }]) // failed before open
    now = 200
    live.push('open')
    live.push('message', 'héllo') // 6 bytes in UTF-8
    live.push('rows', '1234')
    // A second pane's stream to the same URL fails, its response spanning the
    // first one's open.
    deliver([{ name: live.url, initiatorType: 'other', responseStatus: 503, transferSize: 650, startTime: 100, responseStart: 150, responseEnd: 300 }])
    deliver([{ name: live.url, initiatorType: 'other', responseStatus: 200, transferSize: 999_999, startTime: 150, responseStart: 199, responseEnd: 900 }]) // the first ends
    now = 1_000
    live.push('open') // the browser reconnects
    live.push('message', 'ab')
    deliver([{ name: live.url, initiatorType: 'other', responseStatus: 200, transferSize: 999_999, startTime: 990, responseStart: 999, responseEnd: 1_500 }])

    const catalog = new EventSource('/api/v1/thread-index/events')
    catalog.onmessage = () => {} // the onmessage-only path
    catalog.push('open')
    catalog.push('message', 'xyz')

    await fetch('/api/v1/browser-diagnostics', { method: 'POST', body: '{"a":1}' })
    const aborted = new AbortController()
    aborted.abort()
    await fetch('/api/v1/browser-validations', { method: 'POST', body: 'x'.repeat(10_000), signal: aborted.signal })
    deliver([
      { name: 'http://nano.test/', initiatorType: 'navigation', transferSize: 1_500, responseEnd: 1 },
      { name: 'http://nano.test/api/sessions', initiatorType: 'fetch', transferSize: 2_000, responseEnd: 20 },
      { name: 'http://nano.test/assets/index-AbC123.js', initiatorType: 'script', transferSize: 0, responseEnd: 30 }, // cache hit
    ])

    const { routes, received } = meter.networkMeterSnapshot()
    const byKey = Object.fromEntries(routes.map((route) => [route.key, route]))
    expect(byKey['/api/v1/hosted-threads/:id/live']).toMatchObject({ stream: true, requests: 4, received: 650 + 650 + 10 + 2, sent: 4 * H })
    expect(byKey['/api/v1/thread-index/events']).toMatchObject({ requests: 1, received: 3 })
    expect(byKey['/']).toMatchObject({ requests: 1, received: 1_500, sent: H })
    expect(byKey['/api/sessions']).toMatchObject({ requests: 1, received: 2_000, sent: 0 })
    expect(byKey['/assets/index-*.js']).toMatchObject({ requests: 1, received: 0, sent: 0 })
    expect(byKey['/api/v1/browser-diagnostics'].sent).toBe(H + 7)
    expect(byKey['/api/v1/browser-validations']).toBeUndefined()
    expect(received).toBe(1_312 + 3 + 1_500 + 2_000)
  })
})
