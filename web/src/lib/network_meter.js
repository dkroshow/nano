// A debug meter of the bytes this tab exchanges with Nano, by endpoint and
// stream, since page load and per minute. It is off by default: when off,
// nothing is installed. When on, it reads only the browser's own counters and
// sends nothing.
//
// Received bytes of ordinary requests come from Resource Timing transferSize
// (headers plus the encoded body; 0 for a cache hit). A stream has no timing
// entry until it closes, so its bytes are counted as each event arrives (the
// decoded event data, which overstates a compressed stream's wire bytes), and
// the closing entry of a connection that opened is skipped so it is not
// counted twice. An entry belongs to the open that fired during its 200
// response; an attempt that never opened counts by its entry. Sent bytes
// are the request bodies fetch sends, plus an estimate for the headers the
// browser adds to every request, which a page cannot read. A minute holds a
// request when it completes, and a stream event when it arrives.

export const NETWORK_METER_KEY = 'nano-network-meter'
// About what Chrome sends for one same-origin request line and its headers.
export const SENT_HEADER_ESTIMATE = 500
const MINUTES_KEPT = 15

const routes = new Map() // key -> { key, stream, requests, received, sent, minutes: Map(minute -> { received, sent }) }
const totals = new Map() // minute -> { received, sent }
const streamOpens = new Map() // stream URL -> open times (performance.now) whose timing entry has not arrived
let installed = false

export function networkMeterEnabled() {
  try {
    return localStorage.getItem(NETWORK_METER_KEY) === 'on'
  } catch {
    return false
  }
}

// setNetworkMeter saves the choice and reloads, so the meter counts from page
// load when on and leaves nothing installed when off.
export function setNetworkMeter(on) {
  try {
    if (on) localStorage.setItem(NETWORK_METER_KEY, 'on')
    else localStorage.removeItem(NETWORK_METER_KEY)
  } catch {
    return
  }
  location.reload()
}

// routeKey groups requests by endpoint: the path without its query, with id
// segments (Nano ids, uuids, hashes, short session ids) folded to ":id".
export function routeKey(url) {
  let path
  try {
    path = new URL(url, location.href).pathname
  } catch {
    return String(url)
  }
  return path.split('/').map((segment) => {
    if (/^index-[\w-]+\.(js|css)$/.test(segment)) return segment.replace(/-[\w-]+\./, '-*.')
    if (segment.length >= 8 && /\d/.test(segment) && /^[\w-]+$/.test(segment)) return ':id'
    return segment
  }).join('/')
}

function minuteNow() {
  return Math.floor(performance.now() / 60_000)
}

function add(key, stream, { received = 0, sent = 0, request = false }, minute = minuteNow()) {
  let route = routes.get(key)
  if (!route) {
    route = { key, stream, requests: 0, received: 0, sent: 0, minutes: new Map() }
    routes.set(key, route)
  }
  if (request) route.requests += 1
  route.received += received
  route.sent += sent
  for (const bucket of [route.minutes, totals]) {
    const entry = bucket.get(minute) ?? { received: 0, sent: 0 }
    entry.received += received
    entry.sent += sent
    bucket.set(minute, entry)
    for (const old of bucket.keys()) if (old <= minute - MINUTES_KEPT) bucket.delete(old)
  }
}

const encoder = new TextEncoder()
function textBytes(text) {
  return typeof text === 'string' ? encoder.encode(text).length : 0
}

function bodyBytes(body) {
  if (body == null) return 0
  if (typeof body === 'string') return textBytes(body)
  if (body instanceof Blob) return body.size
  if (body instanceof ArrayBuffer || ArrayBuffer.isView(body)) return body.byteLength
  if (body instanceof URLSearchParams) return textBytes(body.toString())
  if (body instanceof FormData) {
    let size = 0
    for (const [name, value] of body) size += textBytes(name) + (typeof value === 'string' ? textBytes(value) : value.size)
    return size
  }
  return 0
}

function observeTimings() {
  if (typeof PerformanceObserver === 'undefined') return
  const record = (entry) => {
    const opens = streamOpens.get(entry.name)
    // Only a 200 response opens a stream; any other status is a failed attempt.
    // A browser that does not report status falls back to timing alone.
    if (opens && (entry.responseStatus === undefined || entry.responseStatus === 200)) {
      // A connection's open fires right after its response starts, so the
      // nearest open inside the response is that connection's.
      const start = entry.responseStart || entry.startTime
      let match = -1
      opens.forEach((at, i) => {
        if (at >= start && at <= entry.responseEnd && (match < 0 || at < opens[match])) match = i
      })
      if (match >= 0) {
        opens.splice(match, 1) // counted event by event
        return
      }
    }
    const received = entry.transferSize ?? 0
    // fetch charged its own headers; a cache hit sent nothing.
    const sent = entry.initiatorType !== 'fetch' && received > 0 ? SENT_HEADER_ESTIMATE : 0
    add(routeKey(entry.name), Boolean(opens), { received, sent, request: true }, Math.floor(entry.responseEnd / 60_000))
  }
  for (const type of ['navigation', 'resource']) {
    try {
      new PerformanceObserver((list) => list.getEntries().forEach(record)).observe({ type, buffered: true })
    } catch {
      // A browser without this entry type counts the rest.
    }
  }
}

function meterFetch() {
  const nativeFetch = globalThis.fetch
  globalThis.fetch = function meteredFetch(input, init) {
    const url = input instanceof Request ? input.url : String(input)
    // An already aborted request never reaches the network.
    if ((init?.signal ?? (input instanceof Request ? input.signal : null))?.aborted) return nativeFetch.call(this, input, init)
    // A Request's own body is a stream the page cannot size without reading it.
    add(routeKey(url), false, { sent: SENT_HEADER_ESTIMATE + bodyBytes(init?.body) })
    return nativeFetch.call(this, input, init)
  }
}

// Each stream counts every data event once, whatever listeners the page
// adds: one counting listener per event type, added the first time the page
// listens for that type. A page cannot see an event type it never listens
// for, so those bytes are not counted (an opened stream's closing timing
// entry is skipped as a whole). The wire also carries the field names and line
// breaks, a few bytes per event, which are not counted.
function meterEventSource() {
  const NativeEventSource = globalThis.EventSource
  if (!NativeEventSource) return
  globalThis.EventSource = class MeteredEventSource extends NativeEventSource {
    #counted = new Set()
    #key

    constructor(url, init) {
      super(url, init)
      this.#key = routeKey(this.url)
      if (!streamOpens.has(this.url)) streamOpens.set(this.url, [])
      // Each open is one request, including the browser's own reconnects.
      super.addEventListener('open', () => {
        streamOpens.get(this.url).push(performance.now())
        add(this.#key, true, { sent: SENT_HEADER_ESTIMATE, request: true })
      })
    }

    #count(type) {
      if (type === 'open' || type === 'error' || this.#counted.has(type)) return
      this.#counted.add(type)
      super.addEventListener(type, (event) => {
        add(this.#key, true, { received: textBytes(event.data) + textBytes(event.lastEventId) })
      })
    }

    addEventListener(type, listener, options) {
      this.#count(type)
      super.addEventListener(type, listener, options)
    }

    set onmessage(listener) {
      this.#count('message')
      super.onmessage = listener
    }

    get onmessage() {
      return super.onmessage
    }
  }
}

// installNetworkMeter runs once at startup, before the app opens any request
// or stream, and does nothing unless the meter is on.
export function installNetworkMeter() {
  if (installed || !networkMeterEnabled()) return false
  installed = true
  observeTimings()
  meterFetch()
  meterEventSource()
  return true
}

// networkMeterSnapshot returns the routes by bytes received, the totals since
// load, and the per-minute totals of the minutes kept, newest first.
export function networkMeterSnapshot() {
  const current = minuteNow()
  const lastMinute = current - 1
  const list = [...routes.values()].map((route) => ({
    key: route.key,
    stream: route.stream,
    requests: route.requests,
    received: route.received,
    sent: route.sent,
    lastMinuteReceived: route.minutes.get(lastMinute)?.received ?? 0,
    lastMinuteSent: route.minutes.get(lastMinute)?.sent ?? 0,
  })).sort((a, b) => b.received - a.received || b.sent - a.sent)
  const minutes = []
  for (let minute = current; minute > current - MINUTES_KEPT && minute >= 0; minute -= 1) {
    minutes.push({ minute, ...(totals.get(minute) ?? { received: 0, sent: 0 }) })
  }
  return {
    received: list.reduce((sum, route) => sum + route.received, 0),
    sent: list.reduce((sum, route) => sum + route.sent, 0),
    routes: list,
    minutes,
  }
}
