// @vitest-environment jsdom
//
// Component coverage for the window-level shortcuts: ⌥Tab walks finished
// threads newest-first (never landing on a working one), focuses the
// destination composer, and yields to overlays.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import App from './App.svelte'
import { app } from './app.svelte.js'
import { explorer } from './explorer.svelte.js'
import { __resetThreadNavForTest, TAB_WALK_TIMEOUT_MS } from './lib/keyboard.js'
import { __resetPanesForTest, activeTab, openThreadTab, tabTarget } from './panes.svelte.js'
import { closeQuickDetail, openQuickDetail } from './lib/quick_detail.svelte.js'
import { dropSessionStore } from './state.svelte.js'

class MockEventSource {
  constructor(url) { this.url = url }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

const emptyTranscript = {
  items: [], turns: [], page: { hasMoreBefore: false, beforeCursor: null }, projection: { state: 'ready', epoch: 'tpe_1' },
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

function catalogItem(suffix, updatedAt, runtime = {}) {
  return {
    id: `nth_${suffix}`, provider: 'codex', nativeId: `nat-${suffix}`, kind: 'thread', title: `Thread ${suffix}`,
    metadata: { cwd: { state: 'known', value: '/workspace/nano', evidence: 'session_meta' } },
    createdAt: '2026-07-31T08:00:00Z', updatedAt,
    runtime: { attached: true, sessionId: `s_${suffix}`, status: 'ready', turnActive: false, threadStatus: 'idle', lastTurnEndedAt: updatedAt, ...runtime },
  }
}

function session(suffix) {
  return { id: `s_${suffix}`, provider: 'codex', threadId: `nat-${suffix}`, cwd: '/workspace/nano', model: 'gpt-5.6-sol', status: 'ready', turnActive: false, threadStatus: 'idle' }
}

const sessions = [session('a'), session('b'), session('c')]
// The displayed sidebar order is A, B, C, which is also recency order —
// A carries the newest updatedAt. Tests that need the two orders to diverge
// override explorer.dataset locally.
const rows = [
  catalogItem('a', '2026-07-31T12:00:00Z'),
  catalogItem('b', '2026-07-31T11:00:00Z'),
  catalogItem('c', '2026-07-31T10:00:00Z'),
]

function press(key, init = {}, target = window) {
  const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...init })
  target.dispatchEvent(event)
  return event
}

let fetchMock

beforeEach(() => {
  localStorage.removeItem('nano:active-thread-view')
  vi.stubGlobal('EventSource', MockEventSource)
  // The sessions payload is read lazily at json() time: an in-flight refresh
  // from an earlier press must never resolve with a stale pre-mutation
  // snapshot and clobber sessions a test added mid-flight.
  fetchMock = vi.fn(async (url) => String(url).includes('/transcript')
    ? jsonResponse(emptyTranscript)
    : { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ sessions: app.sessions, detached: [], defaults: {} }), text: async () => '' })
  vi.stubGlobal('fetch', fetchMock)
  __resetPanesForTest()
  __resetThreadNavForTest()
  app.sessions = sessions
  app.activeId = null
  app.explorerOpen = false
  explorer.dataset = rows
  explorer.datasetComplete = true
  localStorage.setItem('nano-view', 'clean')
})

afterEach(() => {
  cleanup()
  for (const s of sessions) dropSessionStore(s.id)
  dropSessionStore('s_d') // the mid-walk finisher some tests add
  vi.unstubAllGlobals()
})

describe('keyboard shortcuts', () => {
  it('⌥Tab jumps to the newest thread and focuses the destination composer', async () => {
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    const { container } = render(App)
    const event = press('Tab', { altKey: true }, container.querySelector('.composer textarea'))

    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
    await waitFor(() => expect(document.activeElement?.matches('.composer textarea')).toBe(true))
    expect(document.activeElement?.closest('.thread-pane.focused')).not.toBeNull()
    expect(event.defaultPrevented).toBe(true)
  })

  it('⌥Tab keeps walking A, B, C — newest finished first', async () => {
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    render(App)
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_c'))
  })

  it('⌥Tab follows recency even when the sidebar shows another order', async () => {
    explorer.dataset = [rows[1], rows[2], rows[0]]
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    render(App)
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
  })

  it('⌥Tab never lands on a working thread', async () => {
    // A is the newest but is mid-turn, so the walk starts at B and wraps
    // straight back over it.
    explorer.dataset = [
      catalogItem('a', '2026-07-31T12:00:00Z', { turnActive: true, threadStatus: 'working' }),
      rows[1],
      rows[2],
    ]
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    render(App)
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_c'))
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
  })

  it('⌥⇧Tab walks the same recency order in reverse', async () => {
    openThreadTab(tabTarget({ item: rows[0], session: session('a') }))
    render(App)
    press('Tab', { altKey: true, shiftKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_c'))
    press('Tab', { altKey: true, shiftKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
  })

  it('⌥` jumps to the most recently updated thread, working included', async () => {
    // A is the newest and mid-turn: the ⌥Tab walk skips it, ⌥` lands on it.
    explorer.dataset = [
      catalogItem('a', '2026-07-31T12:00:00Z', { turnActive: true, threadStatus: 'working' }),
      rows[1],
      rows[2],
    ]
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    render(App)
    // On the US layout ⌥` reports key 'Dead'; the binding matches the code.
    press('Dead', { altKey: true, code: 'Backquote' })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
  })

  it('⌥` resets the ⌥Tab walk', async () => {
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    render(App)
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
    press('Dead', { altKey: true, code: 'Backquote' })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
    // Mid-walk the next stop after B would be C; the jump made it fresh, so
    // ⌥Tab steps from the top instead.
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
  })

  it('a thread finishing mid-walk restarts the walk at that thread', async () => {
    // Operator's rule (2026-08-25): a finish resets the order. D is mid-turn
    // (excluded from the walk) while the operator walks a → b; then D
    // finishes with the newest stamp, and the next press goes to D, not c.
    app.sessions = [...sessions, session('d')]
    const workingD = catalogItem('d', '2026-07-31T13:00:00Z', { turnActive: true, threadStatus: 'working' })
    explorer.dataset = [workingD, ...rows]
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    render(App)
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
    explorer.dataset = [catalogItem('d', '2026-07-31T13:00:00Z'), ...rows]
    await tick()
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_d'))
    // The reset is complete: the walk continues from the new top.
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
  })

  it('a keyboard landing scrolls its sidebar row into view', async () => {
    // jsdom has no scrollIntoView; the spy doubles as the implementation.
    const scrollSpy = vi.fn()
    Element.prototype.scrollIntoView = scrollSpy
    try {
      openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
      render(App)
      press('Tab', { altKey: true })
      await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
      await waitFor(() => expect(scrollSpy).toHaveBeenCalledWith({ block: 'nearest' }))
      const revealed = scrollSpy.mock.instances.at(-1) ?? scrollSpy.mock.contexts?.at(-1)
      expect(revealed?.getAttribute?.('data-sidebar-id')).toBe('codex:nat-a')
    } finally {
      delete Element.prototype.scrollIntoView
    }
  })

  it('a pause restarts the walk at the newest finished thread', async () => {
    let clock = 1_000_000
    __resetThreadNavForTest(() => clock)
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    render(App)
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
    clock += 1000
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
    clock += TAB_WALK_TIMEOUT_MS + 1
    press('Tab', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_a'))
  })

  it('⌥↑ and ⌥↓ keep their non-wrapping sidebar behavior', async () => {
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    render(App)
    await tick()
    press('ArrowDown', { altKey: true })
    await tick()
    expect(activeTab()?.sessionId).toBe('s_c')
    press('ArrowUp', { altKey: true })
    await waitFor(() => expect(activeTab()?.sessionId).toBe('s_b'))
  })

  it('stands down while a modal is open', async () => {
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    const { getByRole } = render(App)
    await fireEvent.click(getByRole('button', { name: '+ New Window' }))
    const modalEvent = press('Tab', { altKey: true })
    await tick()
    expect(activeTab()?.sessionId).toBe('s_c')
    expect(modalEvent.defaultPrevented).toBe(false)
  })

  it('stands down while the quick-detail overlay is open', async () => {
    openThreadTab(tabTarget({ item: rows[2], session: session('c') }))
    render(App)
    openQuickDetail({ catalogId: 'nth_a', provider: 'codex', nativeId: 'nat-a', sessionId: 's_a' })
    await tick()
    const detailEvent = press('Tab', { altKey: true })
    await tick()
    expect(activeTab()?.sessionId).toBe('s_c')
    expect(detailEvent.defaultPrevented).toBe(false)
    closeQuickDetail()
  })
})
