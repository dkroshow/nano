// @vitest-environment jsdom
//
// What the phone layout actually mounts. Every case here is the wrong-subtree
// bug class AGENTS.md warns about, one viewport narrower: the state is right on
// a 390px screen too, and the question is only which pane, which composer and
// which drawer are in the DOM. A media query cannot answer any of them, which
// is why lib/panes/viewport.svelte.js exists — and why these are component tests.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import PanesContainer from './PanesContainer.svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { viewport } from './viewport.svelte.js'
import { app, openThread, spawnSession } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { dropSessionStore } from '../../state.svelte.js'
import {
  __resetPanesForTest,
  addPane,
  focusTab,
  openThreadTab,
  panes,
  restoreActiveThreadView,
  saveActiveThreadView,
  tabTarget,
} from '../../panes.svelte.js'
import { createFileTranscriptDriver } from '../../test_projection.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() {}
  open() { this.onopen?.() }
}
MockEventSource.instances = []

const emptyTranscript = {
  items: [],
  turns: [],
  page: { hasMoreBefore: false, beforeCursor: null },
  projection: { state: 'ready', epoch: 'tpe_1' },
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

function catalogItem(overrides = {}) {
  return { id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'Left pane', metadata: {}, ...overrides }
}

function liveSession(overrides = {}) {
  return {
    id: 'session-1',
    provider: 'codex',
    threadId: 'native-1',
    cwd: '/workspace/nano',
    model: 'gpt-5.6-sol',
    status: 'ready',
    turnActive: false,
    ...overrides,
  }
}

function historicalTab(overrides = {}) {
  return { id: 1, catalogId: 'nth_1', sessionId: '', provider: 'codex', nativeId: 'native-1', title: 'Left pane', ...overrides }
}

function turnPosts() {
  return fetch.mock.calls.filter(([url, opts]) => String(url).endsWith('/turn') && opts?.method === 'POST')
}

function openProjection() {
  MockEventSource.instances.find((source) => source.url.includes('/live?'))?.open()
}

// turnActive is wire state, not summary state: it arrives on the session's SSE
// stream, the same way it does in a browser.
function startTurnOnWire() {
  messageDriver.send({
    op: 'turn.upsert',
    runtime: { turnActive: true, turnStartedAtMs: Date.now() },
    turn: { id: 'turn-1', status: 'in_progress' },
  })
}

let messageDriver

beforeEach(() => {
  vi.stubGlobal('EventSource', MockEventSource)
  MockEventSource.instances = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url) => {
      if (String(url).includes('/transcript')) return jsonResponse(emptyTranscript)
      return jsonResponse({})
    }),
  )
  app.sessions = []
  app.sidebarOpen = false
  explorer.dataset = [catalogItem()]
  localStorage.setItem('nano-view', 'clean')
  localStorage.removeItem('nano:active-thread-view')
  __resetPanesForTest()
  viewport.mobile = true
  viewport.coarse = true
  messageDriver = createFileTranscriptDriver()
})

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')
  vi.unstubAllGlobals()
  // Desktop is the default every other test in the suite runs under.
  viewport.mobile = false
  viewport.coarse = false
  localStorage.removeItem('nano:active-thread-view')
})

describe('phone workspace', () => {
  it('renders the previously focused thread after a page reload', async () => {
    app.sessions = [liveSession()]
    const first = openThreadTab(tabTarget({ item: catalogItem(), session: liveSession() }))
    openThreadTab(tabTarget({ item: catalogItem({ id: 'nth_2', nativeId: 'native-2', title: 'Other thread' }) }))
    focusTab(1, first)
    saveActiveThreadView()

    __resetPanesForTest()
    expect(restoreActiveThreadView()).toBe(true)
    const { container } = render(PanesContainer)
    await tick()

    expect(container.querySelector('.thread-pane-tab-name')?.textContent).toBe('Left pane')
    expect(container.querySelector('.composer textarea')).not.toBeNull()
  })

  it('renders only the focused pane, not every pane in the layout', () => {
    openThreadTab(tabTarget({ item: catalogItem(), session: null }))
    addPane() // a second column, focused by addPane
    openThreadTab(tabTarget({ item: catalogItem({ id: 'nth_2', nativeId: 'native-2', title: 'Right pane' }) }))

    const { container } = render(PanesContainer)

    // Two panes' worth of state, one pane's worth of DOM — and it is the one
    // the operator is in. Rendering both would give each ~195px on a phone.
    expect(container.querySelectorAll('.thread-pane')).toHaveLength(2 - 1)
    expect(container.querySelector('.thread-pane-tab-name')?.textContent).toBe('Right pane')
    // No dividers to drag, no stack columns.
    expect(container.querySelector('.thread-pane-divider')).toBeNull()
  })

  it('still renders every pane on desktop', () => {
    viewport.mobile = false
    openThreadTab(tabTarget({ item: catalogItem() }))
    addPane()
    openThreadTab(tabTarget({ item: catalogItem({ id: 'nth_2', nativeId: 'native-2', title: 'Right pane' }) }))

    const { container } = render(PanesContainer)

    expect(container.querySelectorAll('.thread-pane')).toHaveLength(2)
  })

  it('restores every desktop pane and its tab after a page reload', async () => {
    viewport.mobile = false
    const first = openThreadTab(tabTarget({ item: catalogItem(), session: liveSession() }))
    addPane()
    openThreadTab(tabTarget({
      item: catalogItem({ id: 'nth_2', nativeId: 'native-2', title: 'Right pane' }),
      session: liveSession({ id: 'session-2', threadId: 'native-2' }),
    }))
    focusTab(1, first)
    saveActiveThreadView()

    __resetPanesForTest()
    expect(restoreActiveThreadView()).toBe(true)
    const { container } = render(PanesContainer)
    await tick()

    expect(container.querySelectorAll('.thread-pane')).toHaveLength(2)
    expect([...container.querySelectorAll('.thread-pane-tab-name')].map((node) => node.textContent))
      .toEqual(['Left pane', 'Right pane'])
  })

  it('keeps the composer in the DOM on the phone layout, mid-turn', async () => {
    // The plan's named risk: a mobile layout that swaps surfaces instead of
    // hiding them leaves a live thread with nowhere to type. A turn in flight
    // is the moment it matters most — Stop and Queue both live here.
    app.sessions = [liveSession({ turnActive: true })]
    openThreadTab(tabTarget({ item: catalogItem(), session: liveSession() }))

    const { container } = render(PanesContainer)
    startTurnOnWire()
    await tick()

    expect(container.querySelector('.composer textarea')).not.toBeNull()
    expect(container.querySelector('.composer button.stop')).not.toBeNull()
  })

  it('keeps Thread Details closed but discoverable beside mobile transcript controls', async () => {
    const grok = liveSession({ provider: 'grok', model: 'grok-4.6' })
    const item = catalogItem({ provider: 'grok' })
    app.sessions = [grok]
    explorer.dataset = [item]
    const sourceTabId = openThreadTab(tabTarget({ item, session: grok }))

    const { container, getByRole } = render(PanesContainer)

    expect(viewport.mobile).toBe(true)
    const sourceView = container.querySelector('.thread-pane-view')
    const textarea = sourceView.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'narrow draft' } })
    expect(sourceView.querySelector('.split .pane-right')).toBeNull()

    await fireEvent.click(getByRole('button', { name: 'thread menu' }))
    const open = getByRole('button', { name: 'Open Thread Details' })
    expect(open.closest('.thread-menu-section')).not.toBeNull()
    expect(getByRole('button', { name: 'Detach' }).closest('.thread-menu-section')).toBe(open.closest('.thread-menu-section'))
    await fireEvent.click(open)

    expect(panes.items).toHaveLength(2)
    // A phone mounts only the focused pane; the source thread remains durable
    // in pane state while the details companion occupies the screen.
    expect(container.querySelectorAll('.thread-pane')).toHaveLength(1)
    expect(panes.items[1].detail).toEqual({ tabId: sourceTabId })
    expect(container.querySelector('.thread-detail-panel')).not.toBeNull()
    await fireEvent.click(getByRole('button', { name: 'Close thread details' }))

    expect(panes.activePaneId).toBe(1)
    expect(container.querySelector('.thread-detail-panel')).toBeNull()
    expect(container.querySelector('.split .pane-right')).toBeNull()
    expect(container.querySelector('.composer textarea').value).toBe('narrow draft')
  })

  it('keeps a background tab mounted, so switching tabs does not reload the transcript', async () => {
    // The phone's tab bar is the pane's own tab strip. Flipping between two
    // threads must hide a transcript, never unmount it — the scroll position
    // and the live stream both belong to the mounted node.
    app.sessions = [liveSession()]
    openThreadTab(tabTarget({ item: catalogItem(), session: liveSession() }))
    const secondTabId = openThreadTab(
      tabTarget({ item: catalogItem({ id: 'nth_2', nativeId: 'native-2', title: 'Other thread' }) }),
    )

    const { container } = render(PanesContainer)
    await tick()

    const views = container.querySelectorAll('.thread-pane-view')
    expect(views).toHaveLength(2)
    // Exactly one is visible; the other is CSS-hidden with its subtree intact.
    expect([...views].filter((view) => view.classList.contains('hidden'))).toHaveLength(1)
    expect(panes.tabs[secondTabId]).toBeDefined()
    // And the live thread's composer is still there, behind the tab on top.
    expect(container.querySelector('.composer textarea')).not.toBeNull()
  })

  it('opens narrow Thread Details for the currently selected thread', async () => {
    const firstSession = liveSession({ provider: 'grok', model: 'grok-4.6' })
    const secondSession = liveSession({
      id: 'session-2',
      provider: 'grok',
      threadId: 'native-2',
      model: 'grok-4.6',
    })
    const firstItem = catalogItem({ provider: 'grok' })
    const secondItem = catalogItem({ id: 'nth_2', nativeId: 'native-2', title: 'Other thread', provider: 'grok' })
    app.sessions = [firstSession, secondSession]
    explorer.dataset = [firstItem, secondItem]

    const firstTabId = openThreadTab(tabTarget({ item: firstItem, session: firstSession }))
    const { container } = render(PanesContainer)
    await tick()

    let visibleView = container.querySelector('.thread-pane-view:not(.hidden)')
    await fireEvent.click(visibleView.querySelector('.thread-menu-btn'))
    await fireEvent.click(visibleView.querySelector('.thread-details-toggle'))
    expect(panes.items[1].detail).toEqual({ tabId: firstTabId })
    await fireEvent.click(container.querySelector('[aria-label="Close thread details"]'))

    openThreadTab(tabTarget({ item: secondItem, session: secondSession }))
    await tick()
    visibleView = container.querySelector('.thread-pane-view:not(.hidden)')
    expect(visibleView.querySelector('.pane-right')).toBeNull()
    await fireEvent.click(visibleView.querySelector('.thread-menu-btn'))
    expect(visibleView.querySelector('.thread-details-toggle').getAttribute('aria-label')).toBe('Open Thread Details')
    await fireEvent.click(visibleView.querySelector('.thread-details-toggle'))
    expect(panes.items[1].detail?.tabId).not.toBe(firstTabId)
    expect(panes.tabs[panes.items[1].detail?.tabId]?.nativeId).toBe('native-2')
  })
})

describe('touch composer', () => {
  it('uses the selected text-size scale instead of a phone-only font size', () => {
    app.sessions = [liveSession()]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })

    expect(container.querySelector('.composer textarea')?.classList.contains('composer-input')).toBe(true)
  })

  it('grows with multiline input and scrolls only after the standard cap', async () => {
    app.sessions = [liveSession()]
    vi.stubGlobal('requestAnimationFrame', (callback) => {
      callback()
      return 1
    })
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    const textarea = container.querySelector('.composer textarea')
    let measuredHeight = 92
    Object.defineProperty(textarea, 'scrollHeight', {
      configurable: true,
      get: () => measuredHeight,
    })

    await fireEvent.input(textarea, { target: { value: 'first line\nsecond line' } })
    expect(textarea.style.height).toBe('92px')
    expect(textarea.style.overflowY).toBe('hidden')

    measuredHeight = window.innerHeight * 0.4 + 40
    await fireEvent.input(textarea, { target: { value: 'first line\nsecond line\nthird line' } })
    expect(textarea.style.height).toBe(String(window.innerHeight * 0.4) + 'px')
    expect(textarea.style.overflowY).toBe('auto')
  })

  it('inserts a newline instead of sending when Enter comes from a touch keyboard', async () => {
    app.sessions = [liveSession()]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    openProjection()

    const textarea = container.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'half a thought' } })
    await fireEvent.keyDown(textarea, { key: 'Enter' })

    // Return is the only line break a phone keyboard offers, and a turn sent by
    // accident cannot be taken back.
    expect(turnPosts()).toHaveLength(0)
    expect(textarea.value).toBe('half a thought')
  })

  it('consumes the duplicate beforeinput from desktop Enter', async () => {
    viewport.coarse = false
    app.sessions = [liveSession()]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    openProjection()

    const textarea = container.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'ship it' } })
    await fireEvent.keyDown(textarea, { key: 'Enter' })
    const beforeInput = new InputEvent('beforeinput', {
      bubbles: true,
      cancelable: true,
      inputType: 'insertLineBreak',
    })
    await fireEvent(textarea, beforeInput)

    await waitFor(() => expect(turnPosts()).toHaveLength(1))
  })

  it('still sends on Enter from a real keyboard', async () => {
    viewport.coarse = false
    app.sessions = [liveSession()]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    openProjection()

    const textarea = container.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'ship it' } })
    await fireEvent.keyDown(textarea, { key: 'Enter' })

    await waitFor(() => expect(turnPosts()).toHaveLength(1))
  })

  it('sends from the button on touch, which is the only path left', async () => {
    app.sessions = [liveSession()]
    const { container, getByRole } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    openProjection()

    await fireEvent.input(container.querySelector('.composer textarea'), { target: { value: 'ship it' } })
    await fireEvent.click(getByRole('button', { name: 'Send' }))

    await waitFor(() => expect(turnPosts()).toHaveLength(1))
  })
})

describe('drawer dismissal', () => {
  it('closes the drawer when a catalog thread is opened', () => {
    app.sidebarOpen = true

    openThread('nth_1')

    // A drawer left open over the thread it just staged is the whole failure.
    expect(app.sidebarOpen).toBe(false)
    expect(app.explorerOpen).toBe(false)
  })

  it('closes the drawer when a new session is spawned', async () => {
    app.sidebarOpen = true
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url, opts) => {
        if (String(url).includes('/transcript')) return jsonResponse(emptyTranscript)
        if (opts?.method === 'POST') return jsonResponse(liveSession())
        if (String(url) === '/api/sessions') return jsonResponse({ sessions: [liveSession()], detached: [], defaults: {} })
        return jsonResponse({})
      }),
    )

    await spawnSession({ provider: 'codex', mode: 'new' })

    expect(app.sidebarOpen).toBe(false)
  })
})
