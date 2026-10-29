// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render } from '@testing-library/svelte'
import { tick } from 'svelte'
import HistoricalThreadView from './HistoricalThreadView.svelte'
import ThreadPaneView from '../panes/ThreadPaneView.svelte'
import { app } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { createDurableHistory, __resetTranscriptForTest } from '../../transcript_store.svelte.js'

const savedRows = [
  { id: 'first', type: 'user_message', category: 'messages', text: 'First saved prompt' },
  { id: 'latest', type: 'agent_message', category: 'messages', text: 'Latest saved reply' },
]
const savedPage = {
  items: savedRows,
  turns: [],
  page: { hasMoreBefore: false, beforeCursor: null },
  projection: { state: 'ready', epoch: 'saved-page' },
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body }
}

// Flush Svelte's placement microtasks without allowing a frame or timer to run.
async function placementTicks() {
  await tick()
  await tick()
  await tick()
  await tick()
}

function geometry(scroller) {
  // jsdom has no layout. Each mounted row adds a known amount of scroll height.
  Object.defineProperty(scroller, 'scrollHeight', {
    configurable: true, get: () => scroller.querySelectorAll('.item').length * 500,
  })
  Object.defineProperty(scroller, 'clientHeight', { configurable: true, get: () => 200 })
}

function expectShown(scroller) {
  expect(scroller.textContent).toContain('Latest saved reply')
  // Production CSS makes this class visibility:hidden, even with rows mounted.
  expect(scroller.classList.contains('initial-position')).toBe(false)
  expect(scroller.classList.contains('hidden')).toBe(false)
}

async function mountSavedPane() {
  const history = createDurableHistory('saved-thread', savedPage)
  await history.load()
  const view = render(HistoricalThreadView, { history, visible: true })
  const scroller = view.container.querySelector('.pane-transcript')
  geometry(scroller)
  await placementTicks()
  return { history, scroller }
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.stubGlobal('requestAnimationFrame', (callback) => setTimeout(() => callback(performance.now()), 16))
  vi.stubGlobal('cancelAnimationFrame', (handle) => clearTimeout(handle))
  vi.stubGlobal('EventSource', class { close() {} })
  vi.stubGlobal('fetch', vi.fn(async (url) => jsonResponse(
    String(url).includes('/transcript') ? savedPage : {},
  )))
  __resetTranscriptForTest()
  app.sessions = []
  app.detached = []
  explorer.dataset = [{ id: 'saved-thread', provider: 'codex', nativeId: 'saved-native', metadata: {} }]
})

afterEach(() => {
  cleanup()
  __resetTranscriptForTest()
  vi.clearAllTimers()
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('historical pane reveal', () => {
  it("shows a visible pane's rows without waiting for an animation frame", async () => {
    let frame = 0
    vi.stubGlobal('requestAnimationFrame', () => ++frame)
    vi.stubGlobal('cancelAnimationFrame', () => {})
    const { scroller } = await mountSavedPane()

    expect(scroller.scrollTop).toBe(scroller.scrollHeight)
    expectShown(scroller)
  })

  it('shows the rows while new rows keep restarting the settle', async () => {
    const { history, scroller } = await mountSavedPane()
    // A stable settle needs three frames. Merge before its third frame each time.
    for (let index = 0; index < 4; index += 1) {
      await vi.advanceTimersByTimeAsync(32)
      history.state.items = [...history.state.items, {
        id: `new-${index}`, type: 'agent_message', category: 'messages', text: `New saved reply ${index}`,
      }]
      await placementTicks()
    }

    expect(scroller.textContent).toContain('New saved reply 3')
    expect(scroller.scrollTop).toBe(scroller.scrollHeight)
    expectShown(scroller)
  })

  it('shows the rows and keeps the reader\'s place when they scroll during the settle', async () => {
    const { scroller } = await mountSavedPane()
    // Wheel input cancels the only old reveal path before any frame can finish.
    // Check here: an explicit scroll event also reveals rows and would mask it.
    scroller.dispatchEvent(new WheelEvent('wheel', { bubbles: true, deltaY: -200 }))
    await placementTicks()
    expectShown(scroller)

    scroller.scrollTop = 320
    await fireEvent.scroll(scroller)
    await vi.advanceTimersByTimeAsync(200)
    expectShown(scroller)
    expect(scroller.scrollTop).toBe(320)
  })

  it("keeps an unselected tab's pane unrevealed until shown, then pins and shows it", async () => {
    const tab = {
      id: 1, catalogId: 'saved-thread', provider: 'codex', nativeId: 'saved-native',
      sessionId: '', title: 'Saved thread', tailResetNonce: 0,
    }
    const view = render(ThreadPaneView, { tab, visible: false })
    const scroller = view.container.querySelector('.pane-transcript')
    geometry(scroller)
    await placementTicks()
    await vi.advanceTimersByTimeAsync(200)

    expect(scroller.textContent).toContain('Latest saved reply')
    expect(scroller.classList.contains('hidden')).toBe(true)
    expect(scroller.classList.contains('initial-position')).toBe(true)
    expect(scroller.scrollTop).toBe(0)

    await view.rerender({ tab, visible: true })
    await placementTicks()
    expect(scroller.scrollTop).toBe(scroller.scrollHeight)
    // This guard also passes before the fix, once the old settle finishes.
    await vi.advanceTimersByTimeAsync(64)
    expectShown(scroller)
  })
})
