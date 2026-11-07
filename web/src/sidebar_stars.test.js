// @vitest-environment jsdom
//
// Starred sidebar sections as the operator sees them: the compact rail splits
// Starred above the rest, hides an empty Starred heading, keeps a backgrounded
// star hidden until the disclosure, and the row star control toggles without
// opening the thread.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render } from '@testing-library/svelte'
import { tick } from 'svelte'
import App from './App.svelte'
import CompactThreadRow from './lib/explorer/CompactThreadRow.svelte'
import { app } from './app.svelte.js'
import { explorer } from './explorer.svelte.js'
import { DEFAULT_VIEW } from './explorer_query.js'
import { projectSidebar } from './lib/explorer/sidebar_projection.js'
import { __resetPanesForTest } from './panes.svelte.js'
import { workerDisclosure } from './lib/explorer/worker_disclosure.svelte.js'

class MockEventSource {
  constructor() {
    this.listeners = {}
    MockEventSource.instances.push(this)
  }
  addEventListener(type, fn) { (this.listeners[type] ??= []).push(fn) }
  removeEventListener() {}
  close() {}
  emit(type, event = {}) { for (const fn of this.listeners[type] ?? []) fn(event) }
}
MockEventSource.instances = []

function response(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

function activeThread(id, title, over = {}) {
  return {
    id: `nth_${id}`,
    provider: 'codex',
    nativeId: `native-${id}`,
    kind: 'thread',
    title,
    createdAt: '2026-08-03T10:00:00Z',
    updatedAt: '2026-08-03T12:00:00Z',
    metadata: {
      cwd: { state: 'known', value: '/workspace/nano' },
      model: { state: 'known', value: 'gpt-5.6-sol' },
    },
    runtime: {
      attached: true,
      sessionId: `session-${id}`,
      status: 'ready',
      turnActive: false,
      ...over.runtime,
    },
    ...over,
  }
}

function titles(container) {
  return [...container.querySelectorAll('.thread-list .t-title-text')].map((node) => node.textContent)
}

beforeEach(() => {
  localStorage.removeItem('nano:active-thread-view')
  localStorage.removeItem('nano:sidebar-width')
  localStorage.removeItem('nano:subagent-disclosure-view')
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', vi.fn(async (url, init) => {
    if (String(url).endsWith('/starred') && init?.method === 'PUT') {
      const body = JSON.parse(init.body)
      const id = String(url).split('/').at(-2)
      const item = explorer.dataset.find((row) => row.id === id) ?? { id, provider: 'codex', nativeId: id }
      return response({ ...item, starred: body.starred })
    }
    if (String(url) === '/api/sessions') {
      return response({ sessions: app.sessions, detached: [], defaults: {} })
    }
    return response({})
  }))
  __resetPanesForTest()
  workerDisclosure.expanded = []
  workerDisclosure.historicalExpanded = []
  explorer.view = { ...DEFAULT_VIEW }
  explorer.datasetComplete = true
  explorer.sidebarBootState = 'ready'
  app.activeId = null
  app.sessions = [
    { id: 'session-older', provider: 'codex', threadId: 'native-older', status: 'ready', turnActive: false },
    { id: 'session-newer', provider: 'codex', threadId: 'native-newer', status: 'ready', turnActive: false },
  ]
  explorer.dataset = [
    activeThread('older', 'Older run'),
    activeThread('newer', 'Newer run'),
  ]
})

afterEach(() => {
  cleanup()
  workerDisclosure.expanded = []
  workerDisclosure.historicalExpanded = []
  vi.unstubAllGlobals()
})

describe('compact sidebar starring', () => {
  it('hides the Starred heading when nothing is starred', () => {
    const { container } = render(App)
    expect(container.querySelector('[data-testid="starred-section"]')).toBeNull()
    expect(container.querySelector('[data-testid="starred-divider"]')).toBeNull()
    expect(titles(container)).toEqual(['Older run', 'Newer run'])
  })

  it('renders starred rows above the rest with a visible divider', () => {
    explorer.dataset = [
      activeThread('older', 'Older run'),
      activeThread('newer', 'Newer run', { starred: true }),
    ]
    const { container } = render(App)
    expect(container.querySelector('[data-testid="starred-section"]')?.textContent).toBe('Starred')
    expect(container.querySelector('[data-testid="starred-divider"]')).not.toBeNull()
    expect(titles(container)).toEqual(['Newer run', 'Older run'])
  })

  it('keeps a backgrounded starred thread hidden until the disclosure', async () => {
    explorer.dataset = [
      activeThread('older', 'Older run'),
      activeThread('newer', 'Newer run', {
        starred: true,
        runtime: { attached: true, sessionId: 'session-newer', status: 'ready', turnActive: false, presentationMode: 'background' },
      }),
    ]
    app.sessions[1].presentationMode = 'background'
    const { container } = render(App)
    expect(titles(container)).toEqual(['Older run'])
    expect(container.querySelector('[data-testid="starred-section"]')).toBeNull()
    await fireEvent.click(container.querySelector('[data-testid="background-disclosure"]'))
    expect(titles(container)).toEqual(['Newer run', 'Older run'])
    expect(container.querySelector('[data-testid="starred-section"]')).not.toBeNull()
  })

  it('stars a row from the rail without opening the thread', async () => {
    const { container } = render(App)
    expect(titles(container)).toEqual(['Older run', 'Newer run'])
    const stars = container.querySelectorAll('[data-testid="thread-star"]')
    expect(stars).toHaveLength(2)
    await fireEvent.click(stars[1])
    await tick()
    expect(fetch).toHaveBeenCalledWith('/api/v1/threads/nth_newer/starred', expect.objectContaining({
      method: 'PUT',
      body: JSON.stringify({ starred: true }),
    }))
    expect(titles(container)).toEqual(['Newer run', 'Older run'])
    expect(container.querySelector('[data-testid="starred-section"]')).not.toBeNull()
    expect(app.activeId).toBeNull()
  })

  // A history row's only source is its parent's history scope. Patching just
  // the rendered dataset left that scope unstarred, so the next sidebar event
  // rebuilt the row and the star the operator had just set vanished.
  it('keeps a star set on a historical subagent row through the next sidebar rebuild', async () => {
    const parent = activeThread('parent', 'Parent run', { hostedThreadId: 'nht_parent', starred: true })
    const closed = {
      id: 'nth_closed', provider: 'codex', nativeId: 'native-closed', parentNativeId: parent.nativeId,
      kind: 'subagent', title: 'Closed child', createdAt: '2026-08-03T10:00:00Z', updatedAt: '2026-08-03T11:00:00Z',
      metadata: {}, source: { status: 'present' }, fleet: { label: 'idle' },
    }
    const reread = { ...parent, updatedAt: '2026-08-03T12:30:00Z' }
    explorer.dataset = [parent, closed]
    app.sessions = [{ id: 'session-parent', provider: 'codex', threadId: 'native-parent', hostedThreadId: 'nht_parent', status: 'ready', turnActive: false }]
    workerDisclosure.expanded = ['hosted:nht_parent']
    workerDisclosure.historicalExpanded = ['hosted:nht_parent']
    vi.mocked(fetch).mockImplementation(async (url, init) => {
      const target = String(url)
      if (target.endsWith('/starred') && init?.method === 'PUT') return response({ ...closed, starred: JSON.parse(init.body).starred })
      if (target === '/api/v1/sidebar/threads') return response({ items: [reread], historicalCount: 1, catalogVersion: 1 })
      if (target === '/api/sessions') return response({ sessions: app.sessions, detached: [], defaults: {} })
      return response({})
    })
    const closedStar = () => [...container.querySelectorAll('[data-sidebar-row]')]
      .find((row) => row.textContent.includes('Closed child'))
      ?.querySelector('[data-testid="thread-star"]')

    const { container } = render(App)
    // The Nano view's dataset is built from the active list plus the open
    // history scopes; switch to that shape once the page has booted.
    explorer.datasetComplete = false
    explorer.datasetScope = 'active'
    explorer.activeItems = [parent]
    explorer.historyScopes = { [parent.id]: { items: [closed], state: 'ready' } }
    await tick()
    expect(closedStar()?.getAttribute('aria-pressed')).toBe('false')
    await fireEvent.click(closedStar())
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledWith('/api/v1/threads/nth_closed/starred', expect.anything()))
    await vi.waitFor(() => expect(closedStar()?.disabled).toBe(false))
    expect(closedStar()?.getAttribute('aria-pressed')).toBe('true')

    const stream = MockEventSource.instances.findLast((es) => es.listeners.sidebar)
    stream.emit('sidebar', { data: '{}' })
    await vi.waitFor(() => expect(explorer.activeItems[0]?.updatedAt).toBe(reread.updatedAt))
    await tick()
    expect(closedStar()?.getAttribute('aria-pressed')).toBe('true')
  })

  it('moves a starred parent and its visible subagent subtree together', () => {
    explorer.dataset = [
      activeThread('parent', 'Parent run', { hostedThreadId: 'nht_parent', starred: true }),
      activeThread('child', 'Child run', {
        hostedThreadId: 'nht_child',
        invokerHostedThreadId: 'nht_parent',
      }),
      activeThread('historical', 'Historical child', {
        hostedThreadId: 'nht_historical',
        invokerHostedThreadId: 'nht_parent',
        runtime: { attached: false },
      }),
    ]
    app.sessions = [
      { id: 'session-parent', provider: 'codex', threadId: 'native-parent', status: 'ready', turnActive: false },
      { id: 'session-child', provider: 'codex', threadId: 'native-child', status: 'ready', turnActive: false },
    ]
    workerDisclosure.expanded = ['hosted:nht_parent']

    const { container } = render(App)
    expect(titles(container)).toEqual(['Parent run', 'Child run'])
    expect(container.querySelector('[data-testid="starred-section"]')).not.toBeNull()
    expect(container.querySelector('[data-testid="starred-divider"]')).toBeNull()
    expect(container.querySelector('.thread-list .worker-history-toggle')?.textContent)
      .toBe('Show historical (1)')
    expect([...container.querySelectorAll('[data-testid="thread-star"]')].map((button) => button.getAttribute('aria-label')))
      .toEqual(['Unstar thread', 'Star thread'])
  })

  it('moves an unstarred parent as context for a starred child without filling the parent star', () => {
    explorer.dataset = [
      activeThread('parent', 'Parent run', { hostedThreadId: 'nht_parent' }),
      activeThread('child', 'Starred child', {
        hostedThreadId: 'nht_child',
        invokerHostedThreadId: 'nht_parent',
        starred: true,
      }),
      activeThread('sibling', 'Ordinary sibling', {
        hostedThreadId: 'nht_sibling',
        invokerHostedThreadId: 'nht_parent',
      }),
    ]
    app.sessions = [
      { id: 'session-parent', provider: 'codex', threadId: 'native-parent', status: 'ready', turnActive: false },
      { id: 'session-child', provider: 'codex', threadId: 'native-child', status: 'ready', turnActive: false },
      { id: 'session-sibling', provider: 'codex', threadId: 'native-sibling', status: 'ready', turnActive: false },
    ]
    workerDisclosure.expanded = ['hosted:nht_parent']

    const { container } = render(App)
    expect(titles(container)).toEqual(['Parent run', 'Starred child', 'Ordinary sibling'])
    expect(container.querySelector('[data-testid="starred-divider"]')).not.toBeNull()
    expect([...container.querySelectorAll('.thread-list .worker-summary')].map((node) => [
      node.closest('.thread-row')?.querySelector('.t-title-text')?.textContent,
      node.textContent.trim(),
    ])).toEqual([['Parent run', '▾subagents: 1 (0) · 0 working']])
    expect([...container.querySelectorAll('[data-testid="thread-star"]')].map((button) => button.getAttribute('aria-label')))
      .toEqual(['Star thread', 'Unstar thread', 'Star thread'])
  })
})

describe('compact row star control', () => {
  it('PUTs starred from the row star button', async () => {
    const [row] = projectSidebar([activeThread('solo', 'Solo run')], [{
      id: 'session-solo', provider: 'codex', threadId: 'native-solo', status: 'ready', turnActive: false,
    }])
    const { getByRole } = render(CompactThreadRow, { props: { row } })
    await fireEvent.click(getByRole('button', { name: 'Star thread' }))
    expect(vi.mocked(fetch).mock.calls.some(([url]) => String(url) === '/api/v1/threads/nth_solo/starred')).toBe(true)
  })
})
