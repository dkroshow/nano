// @vitest-environment jsdom
//
// Operator buckets as the operator sees them in the compact rail: no bucket
// leaves the rail as it was; buckets sit first in the order the operator set,
// an empty one keeps its heading, a thread brings its subagents into its
// bucket, a change from another device arrives on the event stream, the
// arrows are the only thing that moves a bucket, and the thread details
// picker puts a thread in a bucket.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render } from '@testing-library/svelte'
import { tick } from 'svelte'
import App from './App.svelte'
import { app } from './app.svelte.js'
import { explorer } from './explorer.svelte.js'
import { DEFAULT_VIEW } from './explorer_query.js'
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

function response(body, status = 200) {
  return { ok: status < 300, status, headers: { get: () => null }, json: async () => body, text: async () => JSON.stringify(body) }
}

function thread(id, title, over = {}) {
  return {
    id: `nth_${id}`,
    provider: 'codex',
    nativeId: `native-${id}`,
    kind: 'thread',
    title,
    createdAt: '2026-08-03T10:00:00Z',
    updatedAt: '2026-08-03T12:00:00Z',
    metadata: { cwd: { state: 'known', value: '/workspace/nano' } },
    runtime: { attached: true, sessionId: `session-${id}`, status: 'ready', turnActive: false },
    ...over,
  }
}

const member = (id) => ({ provider: 'codex', nativeId: `native-${id}` })

// The rail's content, top to bottom: bucket headings as `[name]`, rows by title.
function rail(container) {
  return [...container.querySelectorAll('.thread-list [data-testid="sidebar-bucket"], .thread-list .t-title-text')]
    .map((node) => node.matches('[data-testid="sidebar-bucket"]')
      ? `[${node.querySelector('.sidebar-bucket-name').textContent}]`
      : node.textContent)
}

let serverBuckets

// The real server answers a change and then sends the whole state on the
// event stream; the stream is what the rail shows.
function publish() {
  const stream = MockEventSource.instances.findLast((es) => es.listeners.buckets)
  queueMicrotask(() => stream?.emit('buckets', { data: JSON.stringify({ buckets: serverBuckets }) }))
  return response({ buckets: serverBuckets })
}

beforeEach(() => {
  localStorage.removeItem('nano:active-thread-view')
  localStorage.removeItem('nano:subagent-disclosure-view')
  vi.stubGlobal('EventSource', MockEventSource)
  serverBuckets = []
  vi.stubGlobal('fetch', vi.fn(async (url, init) => {
    const target = String(url)
    if (target === '/api/v1/sidebar/buckets/order' && init?.method === 'PUT') {
      const ids = JSON.parse(init.body).bucketIds
      serverBuckets = ids.map((id) => serverBuckets.find((bucket) => bucket.id === id))
      return publish()
    }
    if (target.endsWith('/bucket') && init?.method === 'PUT') {
      const native = `native-${target.split('/').at(-2).replace('nth_', '')}`
      const { bucketId } = JSON.parse(init.body)
      serverBuckets = serverBuckets.map((bucket) => ({
        ...bucket,
        threads: [
          ...bucket.threads.filter((one) => one.nativeId !== native),
          ...(bucket.id === bucketId ? [{ provider: 'codex', nativeId: native }] : []),
        ],
      }))
      return publish()
    }
    if (target === '/api/sessions') return response({ sessions: app.sessions, detached: [], defaults: {} })
    return response({})
  }))
  __resetPanesForTest()
  workerDisclosure.expanded = ['codex:native-owner']
  workerDisclosure.historicalExpanded = []
  explorer.view = { ...DEFAULT_VIEW }
  explorer.datasetComplete = true
  explorer.sidebarBootState = 'ready'
  explorer.buckets = []
  app.activeId = null
  explorer.dataset = [
    thread('loose', 'Loose run'),
    thread('owner', 'Audit owner'),
    thread('dash', 'Dashboard child', { kind: 'subagent', parentNativeId: 'native-owner' }),
    thread('other', 'Other run'),
  ]
  app.sessions = explorer.dataset.map((row) => ({
    id: row.runtime.sessionId, provider: 'codex', threadId: row.nativeId, status: 'ready', turnActive: false,
  }))
})

afterEach(() => {
  cleanup()
  explorer.buckets = []
  workerDisclosure.expanded = []
  vi.unstubAllGlobals()
})

describe('compact sidebar buckets', () => {
  it('leaves the rail as it was until a bucket exists', () => {
    const { container } = render(App)
    expect(rail(container)).toEqual(['Audit owner', 'Dashboard child', 'Other run', 'Loose run'])
    expect(container.querySelector('[data-testid="sidebar-bucket"]')).toBeNull()
    expect(container.querySelector('[data-testid="bucket-divider"]')).toBeNull()
  })

  it('puts buckets first in the operator order, with subagents inside their parent bucket and stars first', () => {
    explorer.dataset = explorer.dataset.map((row) => (row.nativeId === 'native-other' ? { ...row, starred: true } : row))
    explorer.buckets = [
      { id: 'bkt_empty', name: 'Later', threads: [] },
      { id: 'bkt_audit', name: 'Machine audit', threads: [member('owner'), member('other')] },
    ]
    const { container } = render(App)
    expect(rail(container)).toEqual([
      '[Later]',
      '[Machine audit]', 'Other run', 'Audit owner', 'Dashboard child',
      'Loose run',
    ])
    expect(container.querySelector('[data-testid="bucket-divider"]')).not.toBeNull()
  })

  it('applies a bucket change from another device and moves a bucket only by its arrows', async () => {
    const { container } = render(App)
    serverBuckets = [
      { id: 'bkt_a', name: 'First', threads: [member('loose')] },
      { id: 'bkt_b', name: 'Second', threads: [] },
    ]
    const stream = MockEventSource.instances.findLast((es) => es.listeners.buckets)
    stream.emit('buckets', { data: JSON.stringify({ buckets: serverBuckets }) })
    await tick()
    expect(rail(container)).toEqual(['[First]', 'Loose run', '[Second]', 'Audit owner', 'Dashboard child', 'Other run'])

    await fireEvent.click(container.querySelector('[aria-label="Move Second up"]'))
    await vi.waitFor(() => expect(rail(container)[0]).toBe('[Second]'))
    expect(fetch).toHaveBeenCalledWith('/api/v1/sidebar/buckets/order', expect.objectContaining({
      method: 'PUT', body: JSON.stringify({ bucketIds: ['bkt_b', 'bkt_a'] }),
    }))
    expect(rail(container)).toEqual(['[Second]', '[First]', 'Loose run', 'Audit owner', 'Dashboard child', 'Other run'])
  })

  it('puts a thread in a bucket from its details', async () => {
    serverBuckets = [{ id: 'bkt_a', name: 'Machine audit', threads: [] }]
    explorer.buckets = serverBuckets
    const { container } = render(App)
    const row = [...container.querySelectorAll('[data-sidebar-row]')].find((node) => node.textContent.includes('Audit owner'))
    await fireEvent.click(row.querySelector('.t-detail'))
    const picker = await vi.waitFor(() => {
      const select = document.querySelector('select[aria-label="Bucket"]')
      expect(select).not.toBeNull()
      return select
    })
    picker.value = 'bkt_a'
    await fireEvent.change(picker)
    await vi.waitFor(() => expect(rail(container)).toEqual([
      '[Machine audit]', 'Audit owner', 'Dashboard child', 'Other run', 'Loose run',
    ]))
    expect(fetch).toHaveBeenCalledWith('/api/v1/threads/nth_owner/bucket', expect.objectContaining({
      method: 'PUT', body: JSON.stringify({ bucketId: 'bkt_a' }),
    }))
  })

  // Closing a parent promotes its open subagent to a root of its own; the
  // server still names the closed threads above it as its lineage. Through
  // two closures and a reload, the survivor stays in the owner's bucket, and
  // a standalone thread the server gives no lineage stays out of it.
  it('keeps a subagent in its bucket after the threads above it close, and leaves a standalone thread out', async () => {
    // The server's sidebar block: parentId is the nearest open ancestor.
    const block = (parent, ...ids) => ({ active: true, parentId: parent ? `nth_${parent}` : null, lineageThreads: ids.map(member) })
    explorer.dataset = [
      thread('owner', 'Audit owner', { sidebar: block(null) }),
      thread('worker', 'Worker', { kind: 'subagent', parentNativeId: 'native-owner', sidebar: block('owner', 'owner') }),
      thread('child', 'Grandchild', { kind: 'subagent', parentNativeId: 'native-worker', sidebar: block('worker', 'owner', 'worker') }),
      thread('solo', 'Standalone run', { parentNativeId: 'native-owner', sidebarPlacement: 'standalone', sidebar: block(null) }),
    ]
    app.sessions = explorer.dataset.map((row) => ({
      id: row.runtime.sessionId, provider: 'codex', threadId: row.nativeId, status: 'ready', turnActive: false,
    }))
    workerDisclosure.expanded = ['codex:native-owner', 'codex:native-worker']
    explorer.buckets = [{ id: 'bkt_audit', name: 'Machine audit', threads: [member('owner')] }]
    const first = render(App)
    expect(rail(first.container)).toEqual(['[Machine audit]', 'Audit owner', 'Worker', 'Grandchild', 'Standalone run'])

    const close = (id) => {
      explorer.dataset = explorer.dataset
        .filter((row) => row.nativeId !== `native-${id}`)
        .map((row) => (row.sidebar.parentId === `nth_${id}` ? { ...row, sidebar: { ...row.sidebar, parentId: null } } : row))
      app.sessions = app.sessions.filter((one) => one.threadId !== `native-${id}`)
    }
    close('owner')
    await tick()
    expect(rail(first.container)).toEqual(['[Machine audit]', 'Worker', 'Grandchild', 'Standalone run'])
    close('worker')
    await tick()
    expect(rail(first.container)).toEqual(['[Machine audit]', 'Grandchild', 'Standalone run'])

    cleanup()
    const reloaded = render(App)
    expect(rail(reloaded.container)).toEqual(['[Machine audit]', 'Grandchild', 'Standalone run'])
  })

  // A slow answer to this browser's change must not undo a newer change
  // another device made meanwhile.
  it('keeps the newer state when an older change answers late', async () => {
    serverBuckets = [{ id: 'bkt_a', name: 'First', threads: [] }, { id: 'bkt_b', name: 'Second', threads: [] }]
    explorer.buckets = serverBuckets
    let answer
    vi.mocked(fetch).mockImplementation(async (url) => {
      if (String(url) === '/api/v1/sidebar/buckets/order') return new Promise((resolve) => { answer = resolve })
      if (String(url) === '/api/sessions') return response({ sessions: app.sessions, detached: [], defaults: {} })
      return response({})
    })
    const { container } = render(App)
    await fireEvent.click(container.querySelector('[aria-label="Move Second up"]'))
    const stream = MockEventSource.instances.findLast((es) => es.listeners.buckets)
    stream.emit('buckets', { data: JSON.stringify({ buckets: [serverBuckets[1], serverBuckets[0]] }) })
    stream.emit('buckets', { data: JSON.stringify({ buckets: [{ ...serverBuckets[0], name: 'Renamed elsewhere' }, serverBuckets[1]] }) })
    await tick()
    answer(response({ buckets: [serverBuckets[1], serverBuckets[0]] }))
    await new Promise((resolve) => setTimeout(resolve, 0))
    await tick()
    expect(rail(container).filter((entry) => entry.startsWith('['))).toEqual(['[Renamed elsewhere]', '[Second]'])
  })
})
