// @vitest-environment jsdom
//
// Active-thread stability as the operator sees it in the real compact rail.
// The pure comparator is covered in explorer_local.test.js; this catches the
// wrong-subtree/wrong-pipeline class by proving App actually renders that order
// and preserves it when streamed activity changes updatedAt.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, within } from '@testing-library/svelte'
import { tick } from 'svelte'
import App from './App.svelte'
import { app } from './app.svelte.js'
import { __resetSidebarBootstrapForTest, explorer, loadDataset, scheduleActiveRefresh } from './explorer.svelte.js'
import { DEFAULT_VIEW } from './explorer_query.js'
import { __resetPanesForTest } from './panes.svelte.js'
import { resetWorkerDisclosure, workerDisclosure } from './lib/worker_disclosure.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
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

function activeThread(id, title, startedAtMs, updatedAt) {
  return {
    id: `nth_${id}`,
    provider: 'codex',
    nativeId: `native-${id}`,
    kind: 'thread',
    title,
    createdAt: '2026-08-03T10:00:00Z',
    updatedAt,
    metadata: {
      cwd: { state: 'known', value: '/workspace/nano' },
      model: { state: 'known', value: 'gpt-5.6-sol' },
    },
    runtime: {
      attached: true,
      sessionId: `session-${id}`,
      status: 'ready',
      turnActive: true, threadStatus: 'working',
      turnStartedAtMs: startedAtMs,
    },
  }
}

function titles(container) {
  return [...container.querySelectorAll('.thread-list .t-title-text')].map((node) => node.textContent)
}

const sessions = ['older', 'newer'].map((id) => ({
  id: `session-${id}`, provider: 'codex', threadId: `native-${id}`,
  cwd: '/workspace/nano', model: 'gpt-5.6-sol', status: 'ready', turnActive: true, threadStatus: 'working',
}))

beforeEach(() => {
  localStorage.removeItem('nano:active-thread-view')
  localStorage.removeItem('nano:sidebar-width')
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', vi.fn(async (url) => {
    if (String(url) === '/api/sessions') return response({ sessions, detached: [], defaults: {} })
    return response({})
  }))
  __resetPanesForTest()
  workerDisclosure.expanded = []
  workerDisclosure.historicalExpanded = []
  app.sessions = sessions
  explorer.view = { ...DEFAULT_VIEW }
  explorer.datasetComplete = true
  explorer.datasetScope = 'window'
  explorer.sidebarBootState = 'ready'
  explorer.dataset = [
    activeThread('older', 'Older run', 1000, '2026-08-03T13:00:00Z'),
    activeThread('newer', 'Newer run', 2000, '2026-08-03T12:00:00Z'),
  ]
})

afterEach(() => {
  cleanup()
  workerDisclosure.expanded = []
  workerDisclosure.historicalExpanded = []
  vi.unstubAllGlobals()
})

describe('compact sidebar active ordering', () => {
  it('keeps the hard-refresh model empty until the canonical sidebar snapshot is ready', async () => {
    const fetchMock = vi.mocked(fetch)
    const bootSessions = sessions.map((session, index) => ({ ...session, name: index ? 'Newer pending' : 'Older pending' }))
    let resolveSidebar
    fetchMock.mockImplementation(async (url) => {
      if (String(url) === '/api/sessions') return response({ sessions: bootSessions, detached: [], defaults: {} })
      if (String(url) === '/api/v1/sidebar/threads') {
        return await new Promise((resolve) => { resolveSidebar = resolve })
      }
      return response({})
    })
    app.sessions = []
    explorer.dataset = []
    explorer.datasetComplete = false
    __resetSidebarBootstrapForTest()

    const { container } = render(App)
    await tick()

    const validationBodies = () => fetchMock.mock.calls
      .filter(([url, options]) => String(url) === '/api/v1/browser-validations' && options?.method === 'POST')
      .map(([, options]) => JSON.parse(options.body))
      .filter((body) => body.surface === 'sidebar')
    expect(titles(container)).toEqual([])
    await vi.waitFor(() => expect(validationBodies().some((body) =>
      body.status === 'match' && body.expectedRows.length === 0 && body.mountedRows.length === 0
    )).toBe(true))

    resolveSidebar(response({
      items: [
        activeThread('older', 'Older canonical', 1000, '2026-08-03T13:00:00Z'),
        activeThread('newer', 'Newer canonical', 2000, '2026-08-03T12:00:00Z'),
      ],
      historicalCount: 0,
      catalogVersion: 1,
    }))
    await vi.waitFor(() => expect(explorer.sidebarBootState).toBe('ready'))

    expect(titles(container)).toEqual(['Older canonical', 'Newer canonical'])
    await vi.waitFor(() => expect(validationBodies().some((body) =>
      body.status === 'match' && body.expectedRows.length === 2 &&
      body.expectedRows.every((row) => row.type === 'attached')
    )).toBe(true))
    const firstNonEmpty = validationBodies().find((body) => body.expectedRows.length > 0)
    expect(firstNonEmpty.expectedRows.map((row) => row.type)).toEqual(['attached', 'attached'])
  })

  it('keeps canonical rows fixed through the server-restart resume sequence', async () => {
    const canonical = [
      activeThread('older', 'Older canonical', 1000, '2026-08-03T13:00:00Z'),
      activeThread('newer', 'Newer canonical', 2000, '2026-08-03T12:00:00Z'),
    ].map((thread) => ({ ...thread, runtime: { attached: false, detached: true } }))
    explorer.dataset = canonical
    app.sessions = []

    const { container } = render(App)
    await tick()
    const canonicalTitles = ['Older canonical', 'Newer canonical']
    expect(titles(container)).toEqual(canonicalTitles)

    const intermediateSessions = canonical.map((thread, index) => ({
      id: `restoring-${index}`,
      provider: thread.provider,
      threadId: '',
      hostedThreadId: `nht_restoring_${index}`,
      name: `Restoring ${index}`,
      status: 'starting',
      spawnMode: 'resume',
      turnActive: false, threadStatus: 'idle',
    }))
    for (const visibleCount of [1, intermediateSessions.length]) {
      app.sessions = intermediateSessions.slice(0, visibleCount)
      await tick()
      expect(titles(container)).toEqual(canonicalTitles)
      expect([...container.querySelectorAll('.side-compact [data-sidebar-row]')].map((row) => row.dataset.sidebarType)).toEqual([
        'detached', 'detached',
      ])
    }

    app.sessions = canonical.map((thread, index) => ({
      id: `session-${index}`,
      provider: thread.provider,
      threadId: thread.nativeId,
      hostedThreadId: `nht_restoring_${index}`,
      status: 'ready',
      spawnMode: 'resume',
      turnActive: false, threadStatus: 'idle',
    }))
    await tick()
    expect(titles(container)).toEqual(canonicalTitles)
    expect([...container.querySelectorAll('.side-compact [data-sidebar-row]')].map((row) => row.dataset.sidebarType)).toEqual([
      'attached', 'attached',
    ])
  })

  it('automatically publishes the mounted sidebar row state for agents', async () => {
    const fetchMock = vi.mocked(fetch)
    render(App)

    const validationBodies = () => fetchMock.mock.calls
      .filter(([url, options]) => String(url) === '/api/v1/browser-validations' && options?.method === 'POST')
      .map(([, options]) => JSON.parse(options.body))
      .filter((body) => body.surface === 'sidebar')
    await vi.waitFor(() => expect(validationBodies().some((body) =>
      body.status === 'match' && body.viewMode === 'compact-nano' && body.expectedRows.length === 2
    )).toBe(true))
    expect(validationBodies().some((body) => body.status === 'mismatch')).toBe(false)
    expect(JSON.stringify(validationBodies())).not.toContain('Older run')
  })

  it('starts Nano-only and keeps external work in the All view', async () => {
    const idleNano = activeThread('newer', 'Newer run', 2000, '2026-08-03T12:00:00Z')
    idleNano.runtime.turnActive = false
    idleNano.runtime.threadStatus = 'idle'
    idleNano.runtime.turnStartedAtMs = 0
    explorer.dataset = [
      {
        id: 'nth_external',
        provider: 'claude',
        nativeId: 'native-external',
        kind: 'subagent',
        title: 'External run',
        updatedAt: '2026-08-03T14:00:00Z',
        metadata: {},
        fleet: { label: 'working' },
      },
      activeThread('older', 'Older run', 1000, '2026-08-03T13:00:00Z'),
      idleNano,
    ]

    const { container } = render(App)

    expect(container.querySelector('.side-chip[aria-pressed="true"]')?.textContent).toContain('Nano')
    expect(titles(container)).toEqual(['Older run', 'Newer run'])

    const allChip = [...container.querySelectorAll('.side-chip')].find((button) => button.textContent.includes('All'))
    await fireEvent.click(allChip)
    expect(titles(container)).toEqual(['Older run', 'Newer run', 'External run'])
  })

  // Feedback "agent is done, but left sidebar is showing still working": the
  // pane's thread had gone idle while the rail still counted work. Captured
  // shape: the finished agent and its supervisor, both attached and idle. The
  // supervisor's descendantActivity.active counts its attached child, which
  // is "current", not working.
  it('counts nothing as working once the pane says the agent is done', async () => {
    const supervisor = activeThread('supervisor', 'Build owner', 0, '2026-10-14T14:11:00Z')
    supervisor.hostedThreadId = 'nht_supervisor'
    const done = activeThread('done', 'Coder: CPU release', 0, '2026-10-14T14:11:41Z')
    done.hostedThreadId = 'nht_done'
    done.invokerHostedThreadId = 'nht_supervisor'
    for (const row of [supervisor, done]) {
      row.runtime = { attached: true, sessionId: `session-${row.nativeId}`, status: 'ready', threadStatus: 'idle' }
      row.fleet = { label: 'idle' }
    }
    supervisor.sidebar = { descendantActivity: { total: 1, active: 1, workers: 0 } }
    explorer.dataset = [supervisor, done]
    app.sessions = [supervisor, done].map((row) => ({
      id: row.runtime.sessionId, provider: 'codex', threadId: row.nativeId, hostedThreadId: row.hostedThreadId,
      status: 'ready', turnActive: false, threadStatus: 'idle',
    }))

    const { container } = render(App)
    const working = [...container.querySelectorAll('.side-chip')].find((button) => button.textContent.includes('Working'))
    expect(working.querySelector('.side-chip-n').textContent).toBe('0')
    expect(container.querySelector('.thread-row.working, .t-dot.working')).toBeNull()
  })

  // Failure caught (2026-10-19): a single working row read "offline" on a
  // connected page. A row kept its own freshness stamp, so a working row
  // nothing restamped after a reconnect read "offline". A lost connection is the
  // page's state, not a row's: one banner says so, and rows keep what they
  // last showed. Boundary: the rendered sidebar.
  it('shows one reconnecting banner while the stream is down and never a per-row offline', async () => {
    const external = {
      id: 'nth_external', provider: 'claude', nativeId: 'native-external', kind: 'subagent',
      title: 'External run', updatedAt: '2026-08-03T14:00:00Z', metadata: {}, fleet: { label: 'working' },
    }
    const working = [...explorer.dataset, external]
    explorer.dataset = working
    explorer.windowItems = working
    explorer.activeItems = []
    const sidebarReads = []
    vi.mocked(fetch).mockImplementation(async (url) => {
      const target = String(url)
      if (target === '/api/sessions') return response({ sessions, detached: [], defaults: {} })
      if (target === '/api/v1/sidebar/threads') return new Promise((resolve) => sidebarReads.push(resolve))
      if (target.startsWith('/api/v1/threads?')) return response({ items: working, catalogVersion: 1 })
      return response({})
    })
    const { container } = render(App)
    await fireEvent.click([...container.querySelectorAll('.side-chip')].find((button) => button.textContent.includes('All')))
    const catalog = await vi.waitFor(() => {
      const source = MockEventSource.instances.findLast((es) => es.listeners.runtime)
      expect(source).toBeTruthy()
      return source
    })
    const banner = () => container.querySelector('.reconnecting-banner')
    const offlineRows = () => container.querySelectorAll('.t-offline, .t-dot.stale')
    catalog.emit('open')
    await vi.waitFor(() => expect(sidebarReads).toHaveLength(1))
    expect(container.querySelectorAll('.thread-row.working')).toHaveLength(3)
    expect(banner()).toBeNull()

    catalog.emit('error')
    await tick()
    expect(banner()?.textContent).toContain('Reconnecting to Nano')
    expect(container.querySelectorAll('.thread-row.working')).toHaveLength(3)
    expect(offlineRows()).toHaveLength(0)

    // Reopened: a read begun before the drop answers late, and nothing else
    // restamps these rows. The page is connected, so no row is offline.
    catalog.emit('open')
    await tick()
    sidebarReads[0](response({ items: working, historicalCount: 0, catalogVersion: 1 }))
    await vi.waitFor(() => expect(explorer.activeItems).toHaveLength(3))
    await tick()
    expect(banner()).toBeNull()
    expect(offlineRows()).toHaveLength(0)
    expect(container.querySelectorAll('.thread-row.working')).toHaveLength(3)
  })

  // Expanded history rows are shown copies too: a reconnect rereads every
  // open history scope, and a row the active read leaves out keeps what it
  // last showed until its own scope has been read again.
  it('replaces an expanded history row only when its own scope is read after reconnect', async () => {
    const external = {
      id: 'nth_external', provider: 'claude', nativeId: 'native-external', kind: 'subagent',
      title: 'External run', updatedAt: '2026-08-03T14:00:00Z', metadata: {}, fleet: { label: 'idle' },
    }
    const held = [...explorer.dataset]
    const sidebarReads = []
    const historyReads = []
    vi.mocked(fetch).mockImplementation(async (url) => {
      const target = String(url)
      if (target === '/api/sessions') return response({ sessions, detached: [], defaults: {} })
      if (target === '/api/v1/sidebar/threads') return new Promise((resolve) => sidebarReads.push(resolve))
      if (target.startsWith('/api/v1/sidebar/threads/parent/history')) return new Promise((resolve) => historyReads.push(resolve))
      return response({})
    })
    const { container } = render(App)
    await fireEvent.click([...container.querySelectorAll('.side-chip')].find((button) => button.textContent.includes('All')))
    const catalog = await vi.waitFor(() => {
      const source = MockEventSource.instances.findLast((es) => es.listeners.runtime)
      expect(source).toBeTruthy()
      return source
    })
    explorer.datasetComplete = false
    explorer.datasetScope = 'active'
    explorer.activeItems = held
    explorer.historyScopes = { parent: { items: [external], state: 'ready' } }
    explorer.dataset = [...held, external]
    catalog.emit('open')
    await vi.waitFor(() => expect(sidebarReads).toHaveLength(1))
    // The child starts working: a fleet event updates its cached history row.
    catalog.emit('fleet', { data: JSON.stringify({ threadId: external.id, from: 'idle', to: 'working' }) })
    await tick()
    catalog.emit('error')
    await tick()
    const externalRow = () => container.querySelector('[data-sidebar-id="claude:native-external"]')
    expect(externalRow().classList.contains('working')).toBe(true)

    catalog.emit('open')
    await vi.waitFor(() => expect(sidebarReads).toHaveLength(2))
    const idleHeld = held.map((row) => ({ ...row, runtime: { ...row.runtime, turnActive: false, threadStatus: 'idle' } }))
    // The child finished during the outage, so the active read leaves it out.
    sidebarReads[1](response({ items: idleHeld, historicalCount: 1, catalogVersion: 2 }))
    await vi.waitFor(() => expect(explorer.activeItems[0]?.runtime.threadStatus).toBe('idle'))
    await tick()
    expect(externalRow().classList.contains('working')).toBe(true)

    await vi.waitFor(() => expect(historyReads).toHaveLength(1))
    historyReads[0](response({ items: [external], nextCursor: null }))
    await vi.waitFor(() => expect(externalRow().classList.contains('working')).toBe(false))
  })

  // A pending row's source is the session registry: after a reconnect it keeps
  // what it last showed until that registry is read again.
  it('replaces a pending registry row only when the registry is read again after reopen', async () => {
    const pending = { ...sessions[0], id: 'session-pending', threadId: 'native-pending', name: 'Pending run' }
    explorer.dataset = []
    explorer.windowItems = []
    explorer.activeItems = []
    app.sessions = [pending]
    const registryReads = []
    vi.mocked(fetch).mockImplementation(async (url) => {
      const target = String(url)
      if (target === '/api/sessions') return new Promise((resolve) => registryReads.push(resolve))
      if (target.startsWith('/api/v1/sidebar/threads') || target.startsWith('/api/v1/threads?')) return new Promise(() => {})
      return response({})
    })
    const { container } = render(App)
    const catalog = await vi.waitFor(() => {
      const source = MockEventSource.instances.findLast((es) => es.listeners.runtime)
      expect(source).toBeTruthy()
      return source
    })
    // These reads name no stream position, so the server answers each
    // connect with a refresh.
    catalog.emit('open')
    catalog.emit('runtime', { data: JSON.stringify({ rows: [], refresh: true }) })
    await vi.waitFor(() => expect(registryReads.length).toBeGreaterThan(0))
    for (const read of registryReads.splice(0)) read(response({ sessions: [pending], detached: [], defaults: {} }))
    await vi.waitFor(() => expect(container.querySelector('.thread-row.working')).not.toBeNull())
    catalog.emit('error')
    await tick()
    expect(container.querySelector('.thread-row.working')).not.toBeNull()

    // The run finished during an outage the server cannot replay (a restart):
    // its connect refresh asks for the read, and only that read says so.
    const before = registryReads.length
    catalog.emit('open')
    await tick()
    expect(registryReads.length).toBe(before)
    catalog.emit('runtime', { data: JSON.stringify({ rows: [], refresh: true }) })
    await tick()
    expect(container.querySelector('.thread-row.working')).not.toBeNull()
    await vi.waitFor(() => expect(registryReads.length).toBeGreaterThan(before))
    registryReads.at(-1)(response({ sessions: [{ ...pending, turnActive: false, threadStatus: 'idle' }], detached: [], defaults: {} }))
    await vi.waitFor(() => expect(container.querySelector('.thread-row.working')).toBeNull())
  })

  // A read begun while the stream was down cannot have seen what the stream
  // missed: the live read after the reopen decides, even if the outage read
  // answers last.
  it('lets the live read after a reopen win over a read begun during the outage', async () => {
    const working = [...explorer.dataset]
    const idle = working.map((row) => (row.runtime
      ? { ...row, runtime: { ...row.runtime, turnActive: false, threadStatus: 'idle' } }
      : { ...row, fleet: { label: 'idle' } }))
    const reads = []
    vi.mocked(fetch).mockImplementation(async (url) => {
      const target = String(url)
      if (target === '/api/sessions') return response({ sessions, detached: [], defaults: {} })
      if (target === '/api/v1/sidebar/threads') return new Promise((resolve) => reads.push(resolve))
      if (target.startsWith('/api/v1/threads?')) return new Promise(() => {})
      return response({})
    })
    const { container } = render(App)
    const catalog = await vi.waitFor(() => {
      const source = MockEventSource.instances.findLast((es) => es.listeners.runtime)
      expect(source).toBeTruthy()
      return source
    })
    catalog.emit('open')
    await vi.waitFor(() => expect(reads).toHaveLength(1))
    catalog.emit('error')
    await tick()
    scheduleActiveRefresh() // refocus and catalog events refresh while the stream is down too
    await vi.waitFor(() => expect(reads).toHaveLength(2))
    catalog.emit('open')
    await vi.waitFor(() => expect(reads).toHaveLength(3))
    reads[2](response({ items: idle, revision: 2, historicalCount: 0, catalogVersion: 2 }))
    await vi.waitFor(() => expect(container.querySelector('.thread-row.working')).toBeNull())
    // The outage read answers late with the work it saw before the drop.
    reads[1](response({ items: working, revision: 1, historicalCount: 0, catalogVersion: 1 }))
    await tick()
    await tick()
    expect(container.querySelector('.thread-row.working')).toBeNull()
  })

  // A history repair that fails keeps its rows on screen, so the next
  // reconnect must try it again.
  it('rereads a history scope whose repair failed on the next reconnect', async () => {
    const external = {
      id: 'nth_external', provider: 'claude', nativeId: 'native-external', kind: 'subagent',
      title: 'External run', updatedAt: '2026-08-03T14:00:00Z', metadata: {}, fleet: { label: 'working' },
    }
    const held = [...explorer.dataset]
    const historyReads = []
    vi.mocked(fetch).mockImplementation(async (url) => {
      const target = String(url)
      if (target === '/api/sessions') return response({ sessions, detached: [], defaults: {} })
      if (target === '/api/v1/sidebar/threads') return new Promise(() => {})
      if (target.startsWith('/api/v1/sidebar/threads/parent/history')) return new Promise((resolve) => historyReads.push(resolve))
      return response({})
    })
    const { container } = render(App)
    await fireEvent.click([...container.querySelectorAll('.side-chip')].find((button) => button.textContent.includes('All')))
    const catalog = await vi.waitFor(() => {
      const source = MockEventSource.instances.findLast((es) => es.listeners.runtime)
      expect(source).toBeTruthy()
      return source
    })
    explorer.datasetComplete = false
    explorer.datasetScope = 'active'
    explorer.activeItems = held
    explorer.historyScopes = { parent: { items: [external], state: 'ready' } }
    explorer.dataset = [...held, external]
    catalog.emit('open')
    catalog.emit('error')
    catalog.emit('open')
    await vi.waitFor(() => expect(historyReads).toHaveLength(1))
    historyReads[0]({ ...response({ error: { code: 'unavailable', message: 'history unavailable' } }), ok: false, status: 503 })
    await vi.waitFor(() => expect(explorer.historyScopes.parent.state).toBe('error'))
    catalog.emit('error')
    catalog.emit('open')
    await vi.waitFor(() => expect(historyReads).toHaveLength(2))
  })

  // A row read returns the whole current row and replaces the old one. A
  // source it leaves out must not survive underneath it: the runtime of a
  // session that ended during the outage, or the fleet label of an outside
  // thread the server could not classify.
  it.each([
    {
      omitted: 'runtime of a session that ended',
      before: (row) => row,
      current: (row) => { delete row.runtime; row.fleet = { label: 'idle' } },
    },
    {
      omitted: 'fleet label the server could not classify',
      before: (row) => { delete row.runtime; row.fleet = { label: 'working' } },
      current: (row) => { delete row.runtime; delete row.fleet },
    },
  ])('does not keep an old $omitted when the current full catalog row omits it', async ({ before, current: shapeCurrent }) => {
    const working = explorer.dataset.map((row) => {
      const copy = { ...row }
      before(copy)
      return copy
    })
    const current = { ...working[0] }
    shapeCurrent(current)
    let holdRegistry = false
    const heldRegistry = []
    const sidebarReads = []
    const rowReads = []
    vi.mocked(fetch).mockImplementation(async (url) => {
      const target = String(url)
      if (target === '/api/sessions') {
        return holdRegistry ? new Promise((resolve) => heldRegistry.push(resolve)) : response({ sessions: [], detached: [], defaults: {} })
      }
      if (target === '/api/v1/sidebar/threads') return new Promise((resolve) => sidebarReads.push(resolve))
      if (target.startsWith('/api/v1/threads?')) {
        rowReads.push(target)
        return response({ items: [current], catalogVersion: 2 })
      }
      return response({})
    })
    app.sessions = []
    const { container } = render(App)
    await fireEvent.click([...container.querySelectorAll('.side-chip')].find((button) => button.textContent.includes('All')))
    const catalog = await vi.waitFor(() => {
      const source = MockEventSource.instances.findLast((es) => es.listeners.runtime)
      expect(source).toBeTruthy()
      return source
    })
    // Earlier cases leave a window read unanswered on purpose; a row patch
    // waits behind any window load, so finish one before starting.
    await loadDataset()
    rowReads.length = 0
    explorer.datasetComplete = false
    explorer.datasetScope = 'active'
    explorer.activeItems = working
    explorer.windowItems = []
    explorer.historyScopes = {}
    explorer.dataset = working
    explorer.catalogVersion = 1
    catalog.emit('open')
    await vi.waitFor(() => expect(sidebarReads).toHaveLength(1))
    holdRegistry = true
    catalog.emit('error')
    catalog.emit('open')
    await tick()
    const row = container.querySelector('[data-sidebar-id="codex:native-older"]')
    expect(row.classList.contains('working')).toBe(true)
    catalog.emit('catalog', { data: JSON.stringify({ threadIds: [current.id], catalogVersion: 2 }) })
    await vi.waitFor(() => expect(rowReads).toHaveLength(1))
    await vi.waitFor(() => expect(explorer.catalogVersion).toBe(2))
    await tick()
    expect(explorer.activeItems[0].runtime).toBeUndefined()
    expect(explorer.activeItems[0].fleet?.label ?? 'idle').toBe('idle')
    expect(row.classList.contains('working')).toBe(false)
    for (const read of heldRegistry) read(response({ sessions: [], detached: [], defaults: {} }))
    await tick()
  })

  it('does not reorder working rows when a newer wire event changes updatedAt', async () => {
    const { container } = render(App)
    expect(titles(container).slice(0, 2)).toEqual(['Older run', 'Newer run'])

    explorer.dataset = [
      activeThread('older', 'Older run', 1000, '2026-08-03T13:00:00Z'),
      activeThread('newer', 'Newer run', 2000, '2026-08-03T14:00:00Z'),
    ]
    await tick()

    expect(titles(container).slice(0, 2)).toEqual(['Older run', 'Newer run'])
  })

  it('starts wider and persists drag and keyboard resizing', async () => {
    const { container, getByRole } = render(App)
    const shell = container.querySelector('.shell')
    const resize = getByRole('button', { name: /Resize thread sidebar/ })

    expect(shell.style.getPropertyValue('--rail-w')).toBe('320px')
    await fireEvent.pointerDown(resize, { clientX: 320 })
    await fireEvent.pointerMove(window, { clientX: 400 })
    expect(shell.style.getPropertyValue('--rail-w')).toBe('400px')
    await fireEvent.pointerUp(window)
    expect(localStorage.getItem('nano:sidebar-width')).toBe('400')

    await fireEvent.keyDown(resize, { key: 'ArrowLeft' })
    expect(shell.style.getPropertyValue('--rail-w')).toBe('384px')
    expect(localStorage.getItem('nano:sidebar-width')).toBe('384')
  })

  it('starts every subagent group closed and lets local and global toggles coexist', async () => {
    const parent = activeThread('parent', 'Parent run', 0, '2026-08-03T13:00:00Z')
    parent.hostedThreadId = 'nht_parent'
    parent.runtime.turnActive = false
    parent.runtime.threadStatus = 'idle'
    const child = activeThread('child', 'Child run', 1000, '2026-08-03T12:00:00Z')
    child.hostedThreadId = 'nht_child'
    child.invokerHostedThreadId = 'nht_parent'
    child.assignment = { assignmentRole: 'worker', focus: 'Complete the child task.' }
    explorer.dataset = [parent, child]
    app.sessions = [
      { id: 'session-parent', provider: 'codex', threadId: 'native-parent', hostedThreadId: 'nht_parent', status: 'ready', turnActive: false, threadStatus: 'idle' },
      { id: 'session-child', provider: 'codex', threadId: 'native-child', hostedThreadId: 'nht_child', status: 'ready', turnActive: true, threadStatus: 'working' },
    ]
    resetWorkerDisclosure()

    const { container, getByRole } = render(App)
    const compactSidebar = within(container.querySelector('.side-compact'))
    expect(titles(container)).toEqual(['Parent run'])

    await fireEvent.click(compactSidebar.getByRole('button', { name: 'Show subagents of Parent run' }))
    expect(titles(container)).toEqual(['Parent run', 'Child run'])
    expect(workerDisclosure.expanded).toEqual(['hosted:nht_parent'])

    await fireEvent.click(getByRole('button', { name: 'Collapse all agents' }))
    expect(titles(container)).toEqual(['Parent run'])
    expect(workerDisclosure.expanded).toEqual([])

    await fireEvent.click(getByRole('button', { name: 'Expand all agents' }))
    expect(titles(container)).toEqual(['Parent run', 'Child run'])
    expect(workerDisclosure.expanded).toEqual(['*'])
  })

  it('keeps all 53 descendants behind one Nano parent and labels the other 52 as historical', async () => {
    const parent = activeThread('ff-sol', 'ff sol', 0, '2026-08-03T13:00:00Z')
    parent.hostedThreadId = 'nht_ff_sol'
    parent.runtime.turnActive = false
    parent.runtime.threadStatus = 'idle'
    const children = Array.from({ length: 53 }, (_, index) => ({
      id: `nth_ff_child_${index}`,
      provider: 'codex',
      nativeId: `native-ff-child-${index}`,
      hostedThreadId: index === 0 ? 'nht_attached_child' : null,
      parentNativeId: parent.nativeId,
      kind: index === 0 ? 'main' : 'subagent',
      title: index === 0 ? 'Attached idle child' : null,
      preview: index === 0 ? 'Attached idle child' : `Historical child ${index}`,
      createdAt: '2026-08-03T10:00:00Z',
      updatedAt: `2026-08-03T12:${String(index).padStart(2, '0')}:00Z`,
      metadata: {
        cwd: { state: 'known', value: '/workspace/nano' },
        model: { state: 'known', value: 'gpt-5.6-sol' },
      },
      source: { status: 'present' },
      runtime: index === 0 ? {
        attached: true, detached: false, sessionId: 'session-attached-child', status: 'ready', turnActive: false, threadStatus: 'idle',
      } : undefined,
      fleet: { label: 'idle' },
    }))
    explorer.dataset = [parent, ...children]
    const localSessions = [{
      id: 'session-ff-sol', provider: 'codex', threadId: parent.nativeId,
      hostedThreadId: 'nht_ff_sol', status: 'ready', turnActive: false, threadStatus: 'idle',
    }]
    app.sessions = localSessions
    vi.mocked(fetch).mockImplementation(async (url) => {
      if (String(url) === '/api/sessions') return response({ sessions: localSessions, detached: [], defaults: {} })
      return response({})
    })

    const { container } = render(App)
    const compact = within(container.querySelector('.side-compact'))
    expect(titles(container)).toEqual(['ff sol'])

    await fireEvent.click(compact.getByRole('button', { name: 'Show subagents of ff sol' }))
    expect(titles(container)).toEqual(['ff sol', 'Attached idle child'])
    const currentChildRow = [...container.querySelectorAll('[data-sidebar-row]')]
      .find((row) => row.textContent.includes('Attached idle child'))
    const historical = currentChildRow?.nextElementSibling
    expect(historical?.getAttribute('aria-label')).toBe('Show historical subagents of ff sol')
    expect(historical?.textContent).toBe('Show historical (52)')

    await fireEvent.click(historical)
    expect(titles(container)).toHaveLength(54)
    const lastHistoricalRow = [...container.querySelectorAll('[data-sidebar-row]')].at(-1)
    const hideHistorical = lastHistoricalRow?.nextElementSibling
    expect(hideHistorical?.getAttribute('aria-label')).toBe('Hide historical subagents of ff sol')
    expect(hideHistorical?.textContent).toBe('Hide historical (52)')
  })

  it('keeps the root fixed while active children reorder inside its subtree', async () => {
    const parent = activeThread('tree-a', 'Tree A parent', 0, '2026-08-03T12:00:00Z')
    parent.hostedThreadId = 'nht_tree_a'
    parent.runtime.turnActive = false
    parent.runtime.threadStatus = 'idle'
    const firstChild = activeThread('tree-a-child-1', 'First child', 1500, '2026-08-03T14:00:00Z')
    firstChild.hostedThreadId = 'nht_tree_a_child_1'
    firstChild.invokerHostedThreadId = 'nht_tree_a'
    firstChild.assignment = { assignmentRole: 'worker', focus: 'Complete the first task.' }
    const secondChild = activeThread('tree-a-child-2', 'Second child', 0, '2026-08-03T13:00:00Z')
    secondChild.hostedThreadId = 'nht_tree_a_child_2'
    secondChild.invokerHostedThreadId = 'nht_tree_a'
    secondChild.assignment = { assignmentRole: 'worker', focus: 'Complete the second task.' }
    secondChild.runtime.turnActive = false
    secondChild.runtime.threadStatus = 'idle'
    const other = activeThread('tree-b', 'Tree B parent', 0, '2026-08-03T13:00:00Z')
    other.hostedThreadId = 'nht_tree_b'
    other.runtime.turnActive = false
    other.runtime.threadStatus = 'idle'
    explorer.dataset = [parent, firstChild, secondChild, other]
    app.sessions = [
      { id: 'session-tree-a', provider: 'codex', threadId: 'native-tree-a', hostedThreadId: 'nht_tree_a', status: 'ready', turnActive: false, threadStatus: 'idle' },
      { id: 'session-tree-a-child-1', provider: 'codex', threadId: 'native-tree-a-child-1', hostedThreadId: 'nht_tree_a_child_1', status: 'ready', turnActive: true, threadStatus: 'working' },
      { id: 'session-tree-a-child-2', provider: 'codex', threadId: 'native-tree-a-child-2', hostedThreadId: 'nht_tree_a_child_2', status: 'ready', turnActive: false, threadStatus: 'idle' },
      { id: 'session-tree-b', provider: 'codex', threadId: 'native-tree-b', hostedThreadId: 'nht_tree_b', status: 'ready', turnActive: false, threadStatus: 'idle' },
    ]
    workerDisclosure.expanded = ['hosted:nht_tree_a']

    const { container } = render(App)
    expect(titles(container)).toEqual(['Tree B parent', 'Tree A parent', 'First child', 'Second child'])

    explorer.dataset = [
      parent,
      { ...firstChild, runtime: { ...firstChild.runtime, turnActive: false, threadStatus: 'idle', turnStartedAtMs: 0 } },
      { ...secondChild, runtime: { ...secondChild.runtime, turnActive: true, threadStatus: 'working', turnStartedAtMs: 1600 } },
      other,
    ]
    await tick()

    expect(titles(container)).toEqual(['Tree B parent', 'Tree A parent', 'Second child', 'First child'])
    const rows = [...container.querySelectorAll('.thread-list .thread-row')]
    expect(rows[2].classList.contains('worker-child')).toBe(true)
    expect(rows[3].classList.contains('worker-child')).toBe(true)
  })

  it('mounts background work behind a disclosure while promoting attention', async () => {
    const shared = activeThread('older', 'Shared run', 1000, '2026-08-03T13:00:00Z')
    const background = activeThread('newer', 'Background run', 2000, '2026-08-03T12:00:00Z')
    background.runtime.presentationMode = 'background'
    background.runtime.turnActive = false
    background.runtime.threadStatus = 'idle'
    const localSessions = [
      { ...sessions[0], presentationMode: 'shared' },
      { ...sessions[1], presentationMode: 'background', turnActive: false, threadStatus: 'idle' },
    ]
    explorer.dataset = [shared, background]
    app.sessions = localSessions
    vi.mocked(fetch).mockImplementation(async (url) => {
      if (String(url) === '/api/sessions') return response({ sessions: localSessions, detached: [], defaults: {} })
      return response({})
    })

    const { container } = render(App)
    expect(titles(container)).toEqual(['Shared run'])

    const disclosure = container.querySelector('[data-testid="background-disclosure"]')
    expect(disclosure).not.toBeNull()
    expect(disclosure.getAttribute('aria-expanded')).toBe('false')
    await fireEvent.click(disclosure)
    expect(titles(container)).toEqual(['Shared run', 'Background run'])

    await fireEvent.click(disclosure)
    background.runtime.needsAttention = true
    explorer.dataset = [shared, { ...background }]
    await tick()
    expect(titles(container)).toEqual(['Shared run', 'Background run'])
  })

  it('reveals a background child from its parent agent count', async () => {
    const parent = activeThread('older', 'Parent run', 1000, '2026-08-03T13:00:00Z')
    parent.hostedThreadId = 'nht_parent'
    parent.runtime.turnActive = false
    parent.runtime.threadStatus = 'idle'
    const child = activeThread('newer', 'Background child', 2000, '2026-08-03T12:00:00Z')
    child.hostedThreadId = 'nht_child'
    child.invokerHostedThreadId = 'nht_parent'
    child.assignment = { assignmentRole: 'worker', focus: 'Implement the child task.' }
    child.runtime.presentationMode = 'background'
    child.runtime.turnActive = false
    child.runtime.threadStatus = 'idle'
    const localSessions = [
      { ...sessions[0], hostedThreadId: 'nht_parent', presentationMode: 'shared', turnActive: false, threadStatus: 'idle' },
      {
        ...sessions[1], hostedThreadId: 'nht_child', invokerHostedThreadId: 'nht_parent',
        presentationMode: 'background', turnActive: false, threadStatus: 'idle',
      },
    ]
    explorer.dataset = [parent, child]
    app.sessions = localSessions
    vi.mocked(fetch).mockImplementation(async (url) => {
      if (String(url) === '/api/sessions') return response({ sessions: localSessions, detached: [], defaults: {} })
      return response({})
    })

    const { container } = render(App)
    expect(titles(container)).toEqual(['Parent run'])
    expect(container.querySelector('[data-testid="background-disclosure"]')).toBeNull()

    const compact = within(container.querySelector('.side-compact'))
    const toggle = compact.getByRole('button', { name: 'Show subagents of Parent run' })
    expect(toggle.textContent).toContain('subagents: 1 (0) · 0 working')
    await fireEvent.click(toggle)
    expect(titles(container)).toEqual(['Parent run', 'Background child'])
    expect(container.querySelector('.worker-child .role-badge')).toBeNull()

    await fireEvent.click(compact.getByRole('button', { name: 'Hide subagents of Parent run' }))
    expect(titles(container)).toEqual(['Parent run'])
  })

  it('keeps a closed hosted coordinator as the Nano tree owner of an attached background grandchild', async () => {
    const closed = activeThread('closed', 'Closed coordinator', 0, '2026-08-03T12:00:00Z')
    closed.hostedThreadId = 'nht_closed'
    closed.assignment = { assignmentRole: 'auditor' }
    closed.runtime = {}
    const grandchild = activeThread('grandchild', 'Surviving background worker', 2_000, '2026-08-03T14:00:00Z')
    grandchild.hostedThreadId = 'nht_grandchild'
    grandchild.invokerHostedThreadId = 'nht_closed'
    grandchild.assignment = { assignmentRole: 'worker' }
    grandchild.runtime.presentationMode = 'background'
    grandchild.runtime.turnActive = false
    grandchild.runtime.threadStatus = 'idle'
    const localSessions = [{
      id: 'session-grandchild', provider: 'codex', threadId: 'native-grandchild',
      hostedThreadId: 'nht_grandchild', invokerHostedThreadId: 'nht_closed',
      cwd: '/workspace/nano', model: 'gpt-5.6-sol', status: 'ready',
      presentationMode: 'background', turnActive: false, threadStatus: 'idle',
    }]
    explorer.dataset = [closed, grandchild]
    app.sessions = localSessions
    vi.mocked(fetch).mockImplementation(async (url) => {
      if (String(url) === '/api/sessions') return response({ sessions: localSessions, detached: [], defaults: {} })
      return response({})
    })

    const { container } = render(App)
    expect(titles(container)).toEqual(['Closed coordinator'])
    expect([...container.querySelectorAll('.thread-row')].find((row) => row.textContent.includes('Closed coordinator'))?.classList.contains('dim')).toBe(true)
    expect(container.querySelector('[data-testid="background-disclosure"]')).toBeNull()

    const compact = within(container.querySelector('.side-compact'))
    await fireEvent.click(compact.getByRole('button', { name: 'Show subagents of Closed coordinator' }))
    expect(titles(container)).toEqual(['Closed coordinator', 'Surviving background worker'])
    expect(container.querySelector('.worker-child .role-badge')).toBeNull()
  })
})
