// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { observeThreadOpen } from './thread_open.js'
import { recordBrowserDiagnostic } from './browser_diagnostics.js'
vi.mock('./browser_diagnostics.js', () => ({ recordBrowserDiagnostic: vi.fn() }))

let frames, clock, stop, entries
function paint() {
  clock += 16
  const next = [...frames.values()]
  frames.clear()
  for (const callback of next) callback(clock)
}
function surface(state = 'loading') {
  document.body.innerHTML = `<div id="pane"><div data-thread-open-state="${state}" data-thread-open-history="catalog" data-thread-open-view="enhanced"><div class="item"></div></div></div>`
  const pane = document.querySelector('#pane')
  const scroller = pane.firstElementChild
  scroller.getBoundingClientRect = () => ({ top: 0, bottom: 200, width: 500, height: 200 })
  scroller.firstElementChild.getBoundingClientRect = () => ({ top: 100, bottom: 200, width: 500, height: 100 })
  return { pane, scroller }
}
async function changed() { await Promise.resolve(); paint(); paint() }
beforeEach(() => {
  vi.useFakeTimers()
  frames = new Map(); clock = 100; entries = []; let seq = 0
  vi.stubGlobal('requestAnimationFrame', (callback) => { frames.set(++seq, callback); return seq })
  vi.stubGlobal('cancelAnimationFrame', (id) => frames.delete(id))
  vi.spyOn(performance, 'now').mockImplementation(() => clock)
  vi.stubGlobal('PerformanceObserver', undefined)
  vi.spyOn(performance, 'getEntriesByType').mockImplementation(() => entries)
  recordBrowserDiagnostic.mockClear()
})
afterEach(() => { stop?.(); stop = null; vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); document.body.innerHTML = '' })

it('waits for a painted ready tail, retaining the earlier first-row time, and stops after one event', async () => {
  const { pane, scroller } = surface()
  stop = observeThreadOpen(pane, { catalogId: 'thread-1' }, { startedAt: 100, trigger: 'sidebar' })
  paint(); paint()
  expect(recordBrowserDiagnostic).not.toHaveBeenCalled()
  scroller.dataset.progress = 'changed'
  await Promise.resolve()
  paint()
  scroller.dataset.threadOpenState = 'ready'
  await changed()
  paint()
  expect(recordBrowserDiagnostic).toHaveBeenCalledWith('thread_open', expect.objectContaining({ firstRowsMs: 32, newestRowsMs: 96, outcome: 'ready', itemCount: 1 }))
  scroller.append(document.createElement('span')); await changed(); stop()
  expect(recordBrowserDiagnostic).toHaveBeenCalledTimes(1)
  expect(frames.size).toBe(0)
})

it('does not report paint in a hidden document or let an abandoned action finish its replacement', async () => {
  const { pane } = surface('ready')
  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
  stop = observeThreadOpen(pane, {}, { startedAt: 100 })
  paint(); paint(); expect(recordBrowserDiagnostic).not.toHaveBeenCalled()
  window.dispatchEvent(new Event('pagehide'))
  const oldAction = recordBrowserDiagnostic.mock.calls[0][1].actionId
  stop = observeThreadOpen(pane, {}, { startedAt: clock, trigger: 'sidebar' })
  visibility.mockReturnValue('visible'); document.dispatchEvent(new Event('visibilitychange'))
  paint(); paint()
  const events = recordBrowserDiagnostic.mock.calls.map((call) => call[1])
  expect(events.map((event) => event.outcome)).toEqual(['abandoned', 'ready'])
  expect(events[0]).not.toHaveProperty('firstRowsMs')
  expect(events[1].actionId).not.toBe(oldAction)
})

it.each(['empty', 'error', 'preserved'])('records %s without inventing a newest-row timing', async (outcome) => {
  const { pane, scroller } = surface(outcome === 'error' ? 'error' : 'ready')
  if (outcome === 'empty') scroller.firstElementChild.remove()
  if (outcome === 'preserved') {
    const latest = document.createElement('div'); latest.className = 'item'; latest.getBoundingClientRect = () => ({ top: 300, bottom: 400, height: 100 }); scroller.append(latest)
  }
  stop = observeThreadOpen(pane, {}, { startedAt: 100, preserveHistory: true })
  paint(); paint()
  const event = recordBrowserDiagnostic.mock.calls[0][1]
  expect(event.outcome).toBe(outcome)
  expect(event).not.toHaveProperty('newestRowsMs')
})

it('attributes only current initial history reads for the displayed source and profile', () => {
  const { pane, scroller } = surface('ready')
  const entry = (path, startTime = 110, duration = 7, bytes = 20) => ({ name: new URL(path, location.href).href, startTime, requestStart: startTime, responseEnd: startTime + duration, encodedBodySize: bytes })
  entries = [
    entry('/api/v1/threads/thread-1/transcript?view=enhanced'),
    entry('/api/v1/threads/thread-1/transcript?view=enhanced', 115, 9, 30),
    entry('/api/v1/threads/other/transcript?view=enhanced'),
    entry('/api/v1/threads/thread-1/transcript?view=dev'),
    entry('/api/v1/threads/thread-1/transcript?view=enhanced&before=cursor'),
    entry('/api/v1/threads/thread-1/transcript?view=enhanced', 10),
    entry('/api/v1/threads/thread-1/transcript/body/row'),
    entry('/api/sessions/session-1/conversation/transcript?view=enhanced'),
  ]
  stop = observeThreadOpen(pane, { catalogId: 'thread-1', sessionId: 'session-1' }, { startedAt: 100 })
  paint(); paint()
  expect(recordBrowserDiagnostic.mock.calls[0][1]).toMatchObject({ historyMs: 9, historyBytes: 50 })
  scroller.dataset.threadOpenHistory = 'stream'
  stop = observeThreadOpen(pane, { catalogId: 'thread-1' }, { startedAt: 100 })
  paint(); paint()
  expect(recordBrowserDiagnostic.mock.calls[1][1]).not.toHaveProperty('historyMs')
})
