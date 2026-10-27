// The one app-owned view of this browser's connection to Nano: the header's
// indicator reads it, and a send waiting to be retried wakes when Nano is
// reachable again. It measures this browser to Nano, not general internet
// speed. Any answer from Nano the page already gets (a stream event, a
// response) counts as contact. Only while the page is visible, and at most
// every 5 s, it asks Nano itself: with a tiny read when nothing else has
// answered for 5 s, or when its latency figure is 30 s old, and with a 64 KiB
// read at most every 2 minutes for throughput.

const QUIET_MS = 5_000 // also the shortest gap between two reads
const LATENCY_MS = 30_000
const PROBE_TIMEOUT_MS = 10_000
const SPEED_MS = 2 * 60_000
const SPEED_TIMEOUT_MS = 20_000
const WINDOW_MS = 5 * 60_000
const TREND_WINDOW_MS = 2 * 60_000
const TREND_THRESHOLD = 0.2

export const connection = $state({
  status: 'checking', // checking | connected | offline
  lastSuccessAt: 0, // the last real answer from Nano, of any kind
  offlineSinceAt: 0,
  latencySamples: [], // { at, value } in ms
  speedSamples: [], // { at, value } in Mbps
})

const reachableListeners = new Set()
let active = false
let timer = null
let probing = null
let speedTestAt = 0
let latencyAt = 0
let latencyTriedAt = 0
let responses = null

// onReachable calls fn each time Nano answers after it could not be reached.
export function onReachable(fn) {
  reachableListeners.add(fn)
  return () => reachableListeners.delete(fn)
}

function visible() {
  return typeof document === 'undefined' || document.visibilityState !== 'hidden'
}

function record(key, value, at) {
  connection[key] = [...connection[key].filter((sample) => sample.at >= at - WINDOW_MS), { at, value }]
}

function markConnected(at) {
  const wasDown = connection.status !== 'connected'
  connection.status = 'connected'
  connection.lastSuccessAt = at
  connection.offlineSinceAt = 0
  if (!wasDown) return
  for (const fn of reachableListeners) {
    try { fn() } catch { /* one listener must not stop the others */ }
  }
}

// noteNanoContact records an answer from Nano the page got anyway: an event
// on one of its streams.
export function noteNanoContact() {
  markConnected(Date.now())
}

// A response Nano wrote to a request the page made, seen through the
// browser's own request timings. A file from the browser's cache carries no
// bytes from Nano, and a gateway's 5xx (Tailscale Serve while Nano restarts)
// is not Nano; streams are counted by their own events. A browser that does
// not report status counts streams and probes only.
function observeResponses() {
  if (typeof PerformanceObserver === 'undefined') return null
  try {
    const observer = new PerformanceObserver((list) => {
      for (const entry of list.getEntries()) {
        if ((entry.initiatorType === 'fetch' || entry.initiatorType === 'xmlhttprequest') && entry.transferSize > 0 &&
          entry.responseStatus >= 200 && entry.responseStatus < 500 && entry.name.startsWith(location.origin)) {
          noteNanoContact()
          return
        }
      }
    })
    observer.observe({ type: 'resource' })
    return observer
  } catch {
    return null
  }
}

function markOffline(at) {
  if (connection.status !== 'offline') connection.offlineSinceAt = at
  connection.status = 'offline'
}

async function timed(url, timeoutMs, read) {
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), timeoutMs)
  const startedAt = performance.now()
  try {
    const res = await fetch(url, { cache: 'no-store', signal: controller.signal })
    if (!res.ok) throw new Error(`HTTP ${res.status}`)
    const body = await read(res)
    return { body, elapsedMs: performance.now() - startedAt }
  } finally {
    clearTimeout(timeout)
  }
}

async function measureSpeed() {
  speedTestAt = Date.now()
  try {
    const { body, elapsedMs } = await timed('/api/v1/connection-test', SPEED_TIMEOUT_MS, (res) => res.arrayBuffer())
    if (body.byteLength && elapsedMs > 0) record('speedSamples', (body.byteLength * 8) / (elapsedMs * 1_000), Date.now())
  } catch {
    // A failed sample says nothing the latency probe does not.
  }
}

async function measureLatency() {
  const startedAt = Date.now()
  latencyTriedAt = startedAt
  try {
    const { elapsedMs } = await timed('/api/v1/build', PROBE_TIMEOUT_MS, (res) => res.text())
    const at = Date.now()
    latencyAt = at
    record('latencySamples', elapsedMs, at)
    markConnected(at)
  } catch {
    // Another answer while this one failed (a stream event) still stands.
    if (connection.lastSuccessAt < startedAt) markOffline(Date.now())
  }
}

// check asks Nano only when the page has no fresher evidence (nothing has
// answered for 5 s, or the latency figure is 30 s old), never within 5 s of
// the last read. Coming back into view or online needs no rule of its own: a
// page that heard nothing meanwhile is due at once.
function check() {
  if (!active || !visible() || probing) return
  clearTimeout(timer)
  const run = {}
  probing = run
  void (async () => {
    try {
      if (Date.now() >= nextLatencyAt()) await measureLatency()
      if (connection.status === 'connected' && visible() && Date.now() - speedTestAt >= SPEED_MS) await measureSpeed()
    } finally {
      // A stop and a new start while this ran leave the new loop alone.
      if (probing === run) {
        probing = null
        schedule()
      }
    }
  })()
}

// The next read is due 5 s after the last answer of any kind, or when the
// latency figure is 30 s old, and never within 5 s of the last read.
function nextLatencyAt() {
  return Math.max(latencyTriedAt + QUIET_MS, Math.min(connection.lastSuccessAt + QUIET_MS, latencyAt + LATENCY_MS))
}

// recheck re-times the next check now; one already running does so itself.
function recheck() {
  if (!probing) schedule()
}

function schedule() {
  clearTimeout(timer)
  if (!active || !visible()) return
  timer = setTimeout(check, Math.max(0, nextLatencyAt() - Date.now()))
}

function onVisibility() {
  if (visible()) recheck()
  else clearTimeout(timer)
}

function onOnline() {
  recheck()
}

function onOffline() {
  markOffline(Date.now())
}

// startConnectionChecks starts the app's one probe loop and returns its stop.
export function startConnectionChecks() {
  if (active) return () => {}
  active = true
  Object.assign(connection, { status: 'checking', lastSuccessAt: 0, offlineSinceAt: 0, latencySamples: [], speedSamples: [] })
  speedTestAt = 0
  latencyAt = 0
  latencyTriedAt = 0
  probing = null
  document.addEventListener('visibilitychange', onVisibility)
  window.addEventListener('online', onOnline)
  window.addEventListener('offline', onOffline)
  responses = observeResponses()
  recheck()
  return () => {
    active = false
    clearTimeout(timer)
    responses?.disconnect()
    responses = null
    document.removeEventListener('visibilitychange', onVisibility)
    window.removeEventListener('online', onOnline)
    window.removeEventListener('offline', onOffline)
  }
}

function average(samples, windowMs, now) {
  const recent = samples.filter((sample) => sample.at >= now - windowMs)
  if (!recent.length) return null
  return recent.reduce((total, sample) => total + sample.value, 0) / recent.length
}

// trend compares the last two minutes with the last five: a change of 20%
// or more in quality either way is a trend.
function trend(samples, lowerIsBetter, now) {
  const all = average(samples, WINDOW_MS, now)
  const recent = average(samples, TREND_WINDOW_MS, now)
  if (!all || recent == null) return 'stable'
  const change = lowerIsBetter ? (all - recent) / all : (recent - all) / all
  if (change >= TREND_THRESHOLD) return 'improving'
  if (change <= -TREND_THRESHOLD) return 'worsening'
  return 'stable'
}

// connectionMetrics is what the indicator shows: each measure's five-minute
// average and its trend.
export function connectionMetrics(now = Date.now()) {
  const metrics = []
  const latency = average(connection.latencySamples, WINDOW_MS, now)
  if (latency != null) {
    metrics.push({
      name: 'Latency',
      value: latency >= 1_000 ? `${(latency / 1_000).toFixed(1)} s` : `${Math.round(latency)} ms`,
      trend: trend(connection.latencySamples, true, now),
    })
  }
  const speed = average(connection.speedSamples, WINDOW_MS, now)
  if (speed != null) {
    metrics.push({
      name: 'Throughput',
      value: `${speed.toFixed(speed >= 100 ? 0 : speed >= 10 ? 1 : 2)} Mbps`,
      trend: trend(connection.speedSamples, false, now),
    })
  }
  return metrics
}
