import { recordBrowserDiagnostic } from './browser_diagnostics.js'

// Observe only the opening interval. The transcript components expose their
// own readiness; DOM geometry proves the rows actually reached the viewport.
export function observeThreadOpen(element, fields, { startedAt = 0, trigger = 'load', preserveHistory = false } = {}) {
  const started = startedAt
  const event = { ...fields, trigger, actionId: `open-${crypto.randomUUID()}` }
  let finished = false
  let frame = 0
  let paintFrame = 0
  const resources = new Map()
  const now = () => performance.now()
  const historyEntry = (entry) => {
    if (entry.startTime < started || entry.responseEnd <= 0) return
    const url = new URL(entry.name, location.href)
    if (url.origin !== location.origin) return
    if (fields.catalogId && url.pathname === `/api/v1/threads/${encodeURIComponent(fields.catalogId)}/transcript` ||
        fields.sessionId && url.pathname === `/api/sessions/${encodeURIComponent(fields.sessionId)}/conversation/transcript`) {
      resources.set(`${entry.name}:${entry.startTime}`, entry)
    }
  }
  for (const entry of performance.getEntriesByType?.('resource') ?? []) historyEntry(entry)
  const resourceObserver = typeof PerformanceObserver === 'function' ? new PerformanceObserver((list) => {
    for (const entry of list.getEntries()) historyEntry(entry)
  }) : null
  resourceObserver?.observe({ type: 'resource' })

  const abandon = () => finish('abandoned')

  function finish(outcome) {
    if (finished) return
    finished = true
    observer.disconnect()
    element.removeEventListener('scroll', schedule, true)
    document.removeEventListener('visibilitychange', schedule)
    window.removeEventListener('pagehide', abandon, true)
    for (const entry of resourceObserver?.takeRecords() ?? []) historyEntry(entry)
    resourceObserver?.disconnect()
    clearTimeout(timeout)
    cancelAnimationFrame(frame)
    cancelAnimationFrame(paintFrame)
    for (const entry of performance.getEntriesByType?.('resource') ?? []) historyEntry(entry)
    const scroller = element.querySelector('[data-thread-open-state]')
    const source = scroller?.dataset.threadOpenHistory
    const view = scroller?.dataset.threadOpenView
    event.view = view
    if (scroller?.dataset.threadOpenWarm) event.warm = true
    const entries = [...resources.values()].filter((entry) => {
      const url = new URL(entry.name, location.href)
      if (url.origin !== location.origin || ['before', 'beforeTurn', 'beforeNativeTurn'].some((key) => url.searchParams.has(key))) return false
      if ((url.searchParams.get('view') ?? 'enhanced') !== view) return false
      return source === 'catalog' && fields.catalogId && url.pathname === `/api/v1/threads/${encodeURIComponent(fields.catalogId)}/transcript` ||
        source === 'session' && fields.sessionId && url.pathname === `/api/sessions/${encodeURIComponent(fields.sessionId)}/conversation/transcript`
    })
    if (entries.length) {
      event.historyMs = Math.round(Math.max(...entries.map((entry) => entry.responseEnd - entry.requestStart)))
      event.historyBytes = entries.reduce((sum, entry) => sum + entry.encodedBodySize, 0)
    }
    recordBrowserDiagnostic('thread_open', { ...event, outcome, durationMs: Math.round(now() - started) })
  }

  function inspect() {
    if (document.visibilityState !== 'visible' || !element.isConnected || element.classList.contains('hidden')) return null
    const scroller = element.querySelector('[data-thread-open-state]')
    if (!scroller || scroller.classList.contains('initial-position') || scroller.classList.contains('hidden')) return null
    const box = scroller.getBoundingClientRect()
    const area = { top: Math.max(0, box.top), bottom: Math.min(innerHeight, box.bottom), width: box.width, height: box.height }
    if (area.width <= 0 || area.height <= 0 || area.bottom <= area.top) return null
    const rows = [...scroller.querySelectorAll(':scope > .item')]
    const inView = (row) => {
      const rect = row.getBoundingClientRect()
      return rect.height > 0 && rect.bottom > area.top && rect.top < area.bottom
    }
    return {
      state: scroller.dataset.threadOpenState,
      count: rows.length,
      visible: rows.filter(inView).length,
      newest: rows.length > 0 && inView(rows.at(-1)),
    }
  }

  function schedule() {
    if (finished || frame || paintFrame) return
    frame = requestAnimationFrame(() => {
      frame = 0
      const before = inspect()
      if (!before) return
      // A second frame gives the first frame's DOM a paint opportunity. Check
      // again: navigation or late layout must not produce a false success.
      paintFrame = requestAnimationFrame(() => {
        paintFrame = 0
        const after = inspect()
        if (!after || finished) return
        event.itemCount = after.count
        event.visibleItemCount = after.visible
        if (before.visible && after.visible) event.firstRowsMs ??= Math.round(now() - started)
        if (after.state === 'error') return finish('error')
        if (after.state !== 'ready') return
        if (before.state !== 'ready' || before.count !== after.count || before.newest !== after.newest) return schedule()
        if (!after.count) return finish('empty')
        if (before.newest && after.newest) {
          event.newestRowsMs = Math.round(now() - started)
          return finish('ready')
        }
        if (preserveHistory && before.visible && after.visible) finish('preserved')
      })
    })
  }
  const observer = new MutationObserver(schedule)
  observer.observe(element, { subtree: true, childList: true, attributes: true, characterData: true })
  element.addEventListener('scroll', schedule, true)
  document.addEventListener('visibilitychange', schedule)
  window.addEventListener('pagehide', abandon, true)
  const timeout = setTimeout(() => finish('timeout'), 30000)
  schedule()
  return () => finish('abandoned')
}
