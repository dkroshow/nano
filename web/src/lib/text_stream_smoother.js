const DEFAULT_PUBLISH_INTERVAL_MS = 40
const DEFAULT_BATCH_INTERVAL_MS = 120
const DEFAULT_MAX_RATE = 1600
const BACKLOG_CORRECTION = 7
const RESERVE_BATCHES = 1.1
const COMPLETE_HORIZON_MS = 240
const MAX_FRAME_ELAPSED_MS = 100
const RATE_SAMPLE_MIN_MS = 30
const RATE_SAMPLE_MAX_MS = 1500
const RATE_SAMPLE_MIN = 30
const RATE_SAMPLE_MAX = 2000

const PROVIDER_RATE_SEEDS = {
  codex: 220,
  claude: 300,
  grok: 240,
}
const learnedProviderRates = new Map()

function clamp(value, minimum, maximum) {
  return Math.min(maximum, Math.max(minimum, value))
}

function defaultNow() {
  return typeof performance !== 'undefined' ? performance.now() : Date.now()
}

function defaultRequestFrame(callback) {
  if (typeof requestAnimationFrame === 'function') return requestAnimationFrame(callback)
  return setTimeout(() => callback(defaultNow()), 16)
}

function defaultCancelFrame(handle) {
  if (typeof cancelAnimationFrame === 'function') cancelAnimationFrame(handle)
  else clearTimeout(handle)
}

export function providerStreamRate(provider) {
  const key = String(provider || '').toLowerCase()
  return learnedProviderRates.get(key) ?? PROVIDER_RATE_SEEDS[key] ?? 240
}

export function __resetStreamRatesForTest() {
  learnedProviderRates.clear()
}

function learnProviderRate(provider, sample) {
  const key = String(provider || '').toLowerCase()
  if (!key) return
  const previous = providerStreamRate(key)
  learnedProviderRates.set(key, previous + 0.18 * (sample - previous))
}

export function graphemes(value, segmenter = null) {
  const text = String(value || '')
  if (!text) return []
  const active = segmenter ?? (
    typeof Intl !== 'undefined' && Intl.Segmenter
      ? new Intl.Segmenter(undefined, { granularity: 'grapheme' })
      : null
  )
  return active ? [...active.segment(text)].map((part) => part.segment) : Array.from(text)
}

// Presentation-only pacing. The controller measures the arrival cadence and
// aims to preserve roughly one incoming batch of reserve. That keeps the
// visible stream flowing across Nano's 120 ms snapshot gaps instead of racing
// to zero after every batch. Authoritative projection state remains untouched.
export function createTextStreamSmoother(options = {}) {
  const provider = String(options.provider || '').toLowerCase()
  const onReplace = options.onReplace ?? (() => {})
  const onAppend = options.onAppend ?? (() => {})
  const onSettled = options.onSettled ?? (() => {})
  const requestFrame = options.requestFrame ?? defaultRequestFrame
  const cancelFrame = options.cancelFrame ?? defaultCancelFrame
  const now = options.now ?? defaultNow
  const publishIntervalMs = options.publishIntervalMs ?? DEFAULT_PUBLISH_INTERVAL_MS
  const maxRate = options.maxRate ?? DEFAULT_MAX_RATE
  const segmenter = options.segmenter ?? (
    typeof Intl !== 'undefined' && Intl.Segmenter
      ? new Intl.Segmenter(undefined, { granularity: 'grapheme' })
      : null
  )

  let initialized = false
  let destroyed = false
  let target = ''
  let displayed = ''
  let pending = []
  let frameHandle = null
  let lastFrameAt = null
  let lastPublishAt = null
  let lastArrivalAt = null
  let revealCredit = 0
  let completing = false
  let settledNotified = false
  let estimatedRate = options.initialRate ?? providerStreamRate(provider)
  let batchIntervalMs = DEFAULT_BATCH_INTERVAL_MS
  let averageBatchSize = 0
  const startedAt = now()
  const metrics = {
    batches: 0,
    arrivalGraphemes: 0,
    maxBacklog: 0,
    maxRate: 0,
    publishes: 0,
    snaps: 0,
  }

  function summary() {
    return {
      ...metrics,
      provider,
      backlog: pending.length,
      arrivalRateCps: Math.round(estimatedRate),
      batchIntervalMs: Math.round(batchIntervalMs),
      batchSize: Math.round(averageBatchSize),
      durationMs: Math.max(0, Math.round(now() - startedAt)),
    }
  }

  function cancelScheduledFrame() {
    if (frameHandle == null) return
    cancelFrame(frameHandle)
    frameHandle = null
  }

  function resetPacing() {
    pending = []
    revealCredit = 0
    lastFrameAt = null
    lastPublishAt = null
    completing = false
    cancelScheduledFrame()
  }

  function snap(next, reason, animate = false) {
    resetPacing()
    target = next
    displayed = next
    initialized = true
    metrics.snaps += 1
    onReplace(next, { animate, reason })
  }

  function observeArrival(count, at) {
    if (count <= 0) return
    metrics.batches += 1
    metrics.arrivalGraphemes += count
    averageBatchSize = averageBatchSize === 0 ? count : averageBatchSize + 0.25 * (count - averageBatchSize)
    if (lastArrivalAt != null) {
      const elapsed = at - lastArrivalAt
      if (elapsed >= RATE_SAMPLE_MIN_MS && elapsed <= RATE_SAMPLE_MAX_MS) {
        batchIntervalMs += 0.25 * (elapsed - batchIntervalMs)
        const sample = clamp((count * 1000) / elapsed, RATE_SAMPLE_MIN, RATE_SAMPLE_MAX)
        estimatedRate += 0.3 * (sample - estimatedRate)
        learnProviderRate(provider, sample)
      }
    }
    lastArrivalAt = at
  }

  function enqueue(next, suffix, at) {
    const additions = graphemes(suffix, segmenter)
    // Re-segment the whole undisplayed boundary. Provider snapshots can split
    // one grapheme (notably emoji ZWJ sequences) across arrivals; appending a
    // separately segmented suffix would expose the broken first half.
    pending = graphemes(next.slice(displayed.length), segmenter)
    target = next
    settledNotified = false
    observeArrival(additions.length, at)
    metrics.maxBacklog = Math.max(metrics.maxBacklog, pending.length)
    if (lastFrameAt == null) {
      lastFrameAt = at
      lastPublishAt = at - publishIntervalMs
    }
  }

  function notifySettled() {
    if (!completing || settledNotified) return
    settledNotified = true
    completing = false
    onSettled(displayed, summary())
  }

  function schedule() {
    if (destroyed || frameHandle != null || pending.length === 0) return
    frameHandle = requestFrame(step)
  }

  function revealRate(backlog) {
    const reserve = Math.max(4, (estimatedRate * clamp(batchIntervalMs, 80, 400) * RESERVE_BATCHES) / 1000)
    let rate = estimatedRate + BACKLOG_CORRECTION * (backlog - reserve)
    rate = clamp(rate, Math.max(30, estimatedRate * 0.25), maxRate)
    if (completing) rate = Math.max(rate, (backlog * 1000) / COMPLETE_HORIZON_MS)
    return Math.min(maxRate, rate)
  }

  function step(timestamp) {
    frameHandle = null
    if (destroyed || pending.length === 0) {
      notifySettled()
      return
    }
    const at = Number.isFinite(timestamp) ? timestamp : now()
    if (lastFrameAt == null) lastFrameAt = at
    if (lastPublishAt == null) lastPublishAt = at - publishIntervalMs
    const elapsed = Math.min(MAX_FRAME_ELAPSED_MS, Math.max(0, at - lastFrameAt))
    lastFrameAt = at
    const backlog = pending.length
    const rate = revealRate(backlog)
    metrics.maxBacklog = Math.max(metrics.maxBacklog, backlog)
    metrics.maxRate = Math.max(metrics.maxRate, rate)
    revealCredit += (elapsed * rate) / 1000

    if (at - lastPublishAt >= publishIntervalMs && revealCredit >= 1) {
      const count = Math.min(backlog, Math.max(1, Math.floor(revealCredit)))
      const delta = pending.splice(0, count).join('')
      revealCredit = Math.max(0, revealCredit - count)
      displayed += delta
      lastPublishAt = at
      metrics.publishes += 1
      onAppend(delta, displayed, { backlog: pending.length, rate, estimatedRate })
    }

    if (pending.length > 0) schedule()
    else {
      lastFrameAt = null
      lastPublishAt = null
      revealCredit = 0
      notifySettled()
    }
  }

  function sync({ text = '', animate = true, complete = false, animateInitial = false } = {}) {
    if (destroyed) return
    const next = String(text || '')
    const at = now()
    if (!initialized) {
      if (animate && animateInitial && !complete && next) {
        initialized = true
        target = ''
        displayed = ''
        onReplace('', { animate: true, reason: 'initial-live' })
        enqueue(next, next, at)
        completing = complete
        schedule()
      } else {
        snap(next, 'initial', animate && !complete)
      }
      return
    }
    if (!animate) {
      if (next !== displayed || pending.length) snap(next, 'inactive')
      else target = next
      // A row can leave the live edge after its backlog drains, then complete
      // with byte-identical text. It still needs the canonical renderer and
      // one completion diagnostic even though there is nothing left to reveal.
      if (complete) {
        completing = true
        notifySettled()
      }
      return
    }
    if (!next.startsWith(target)) {
      snap(next, 'correction')
      return
    }
    if (next !== target) enqueue(next, next.slice(target.length), at)
    if (complete) {
      completing = true
      if (pending.length === 0) notifySettled()
      else schedule()
      return
    }
    schedule()
  }

  function destroy() {
    destroyed = true
    resetPacing()
  }

  return {
    destroy,
    metrics: summary,
    state: () => ({ displayed, target, backlog: pending.length, completing }),
    sync,
  }
}
