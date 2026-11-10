// @vitest-environment jsdom
//
// Operator buckets as the operator sees them in the compact rail: no bucket
// leaves the rail as it was; buckets sit first in the order the operator set,
// an empty one keeps its heading, a thread brings its subagents into its
// bucket, a change from another device arrives on the event stream, dragging
// a heading is the only thing that moves a bucket, a thread drags in and out
// of a bucket, and the thread details picker puts a thread in a bucket.
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

// The browser's drag events. jsdom has no DragEvent, so a pointer event
// carries the drag's height and its dataTransfer. A drop lands before or
// after a bucket by that height; jsdom boxes are zero-sized, so clientY 0 is
// the top half and 1 the bottom.
const dataTransfer = () => ({ setData() {}, effectAllowed: '', dropEffect: '' })
function dragEvent(type, transfer, clientY = 0) {
  const event = new MouseEvent(type, { bubbles: true, cancelable: true, clientY })
  Object.defineProperty(event, 'dataTransfer', { value: transfer })
  return event
}
async function dragOnto(source, target, clientY = 0) {
  const transfer = dataTransfer()
  await fireEvent(source, dragEvent('dragstart', transfer))
  await fireEvent(target, dragEvent('dragover', transfer, clientY))
  await fireEvent(target, dragEvent('drop', transfer, clientY))
  await fireEvent(source, dragEvent('dragend', transfer))
}
const bucketHeader = (container, name) => [...container.querySelectorAll('[data-testid="sidebar-bucket"]')]
  .find((node) => node.querySelector('.sidebar-bucket-name').textContent === name)
const rowOf = (container, title) => [...container.querySelectorAll('[data-sidebar-row]')]
  .find((node) => node.querySelector('.t-title-text').textContent === title)

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
  localStorage.removeItem('nano:sidebar-collapsed-buckets')
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

  it('applies a bucket change from another device and moves a bucket only by dragging its heading', async () => {
    const { container } = render(App)
    serverBuckets = [
      { id: 'bkt_a', name: 'First', threads: [member('loose')] },
      { id: 'bkt_b', name: 'Second', threads: [] },
    ]
    const stream = MockEventSource.instances.findLast((es) => es.listeners.buckets)
    stream.emit('buckets', { data: JSON.stringify({ buckets: serverBuckets }) })
    await tick()
    expect(rail(container)).toEqual(['[First]', 'Loose run', '[Second]', 'Audit owner', 'Dashboard child', 'Other run'])

    expect(container.querySelector('[aria-label="Move Second up"]')).toBeNull()
    await dragOnto(bucketHeader(container, 'Second'), bucketHeader(container, 'First').closest('[data-testid="bucket-group"]'), 0)
    await vi.waitFor(() => expect(rail(container)[0]).toBe('[Second]'))
    expect(fetch).toHaveBeenCalledWith('/api/v1/sidebar/buckets/order', expect.objectContaining({
      method: 'PUT', body: JSON.stringify({ bucketIds: ['bkt_b', 'bkt_a'] }),
    }))
    expect(rail(container)).toEqual(['[Second]', '[First]', 'Loose run', 'Audit owner', 'Dashboard child', 'Other run'])

    await dragOnto(bucketHeader(container, 'Second'), bucketHeader(container, 'First').closest('[data-testid="bucket-group"]'), 1)
    await vi.waitFor(() => expect(rail(container)[0]).toBe('[First]'))
    expect(fetch).toHaveBeenLastCalledWith('/api/v1/sidebar/buckets/order', expect.objectContaining({
      body: JSON.stringify({ bucketIds: ['bkt_a', 'bkt_b'] }),
    }))
  })

  // A new bucket is made first and the thread assigned after. The operator
  // may open another thread's details meanwhile; the thread they chose for
  // must still be the one assigned.
  it('creates a bucket from thread details and assigns that thread, even after the details move on', async () => {
    let created
    const baseFetch = vi.mocked(fetch).getMockImplementation()
    vi.mocked(fetch).mockImplementation(async (url, init) => {
      if (String(url) === '/api/v1/sidebar/buckets' && init?.method === 'POST') {
        serverBuckets = [...serverBuckets, { id: 'bkt_new', name: JSON.parse(init.body).name, threads: [] }]
        return new Promise((resolve) => { created = () => resolve(publish()) })
      }
      return baseFetch(url, init)
    })
    vi.spyOn(window, 'prompt').mockReturnValue('Machine audit')
    const { container } = render(App)
    const details = (title) => fireEvent.click([...container.querySelectorAll('[data-sidebar-row]')]
      .find((node) => node.textContent.includes(title)).querySelector('.t-detail'))
    await details('Audit owner')
    const picker = await vi.waitFor(() => {
      const select = document.querySelector('select[aria-label="Bucket"]')
      expect(select).not.toBeNull()
      return select
    })
    picker.value = '__new'
    await fireEvent.change(picker)
    await vi.waitFor(() => expect(created).toBeTypeOf('function'))
    await fireEvent.click(document.querySelector('[aria-label="Close thread details"]'))
    await details('Loose run')
    created()
    await vi.waitFor(() => expect(rail(container)).toEqual([
      '[Machine audit]', 'Audit owner', 'Dashboard child', 'Other run', 'Loose run',
    ]))
    expect(fetch).toHaveBeenCalledWith('/api/v1/threads/nth_owner/bucket', expect.objectContaining({
      method: 'PUT', body: JSON.stringify({ bucketId: 'bkt_new' }),
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
    // The survivor still follows the owner, so it offers no drag of its own;
    // the standalone thread is its own and can be dragged.
    expect(rowOf(reloaded.container, 'Grandchild').getAttribute('draggable')).toBeNull()
    expect(rowOf(reloaded.container, 'Standalone run').getAttribute('draggable')).toBe('true')
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
    await dragOnto(bucketHeader(container, 'Second'), bucketHeader(container, 'First').closest('[data-testid="bucket-group"]'), 0)
    const stream = MockEventSource.instances.findLast((es) => es.listeners.buckets)
    stream.emit('buckets', { data: JSON.stringify({ buckets: [serverBuckets[1], serverBuckets[0]] }) })
    stream.emit('buckets', { data: JSON.stringify({ buckets: [{ ...serverBuckets[0], name: 'Renamed elsewhere' }, serverBuckets[1]] }) })
    await tick()
    answer(response({ buckets: [serverBuckets[1], serverBuckets[0]] }))
    await new Promise((resolve) => setTimeout(resolve, 0))
    await tick()
    expect(rail(container).filter((entry) => entry.startsWith('['))).toEqual(['[Renamed elsewhere]', '[Second]'])
  })

  // A thread joins a bucket by being dropped anywhere in it, with its
  // subagents; while a bucketed thread is dragged a strip offers the way out.
  // A subagent row is never draggable on its own.
  it('drags a thread into a bucket and back out', async () => {
    serverBuckets = [{ id: 'bkt_a', name: 'Machine audit', threads: [] }, { id: 'bkt_b', name: 'Later', threads: [] }]
    explorer.buckets = serverBuckets
    const { container } = render(App)
    expect(rowOf(container, 'Dashboard child').getAttribute('draggable')).toBeNull()
    expect(container.querySelector('[data-testid="bucket-remove"]')).toBeNull()

    await dragOnto(rowOf(container, 'Audit owner'), bucketHeader(container, 'Machine audit'))
    await vi.waitFor(() => expect(rail(container)).toEqual([
      '[Machine audit]', 'Audit owner', 'Dashboard child', '[Later]', 'Other run', 'Loose run',
    ]))

    const transfer = dataTransfer()
    await fireEvent(rowOf(container, 'Audit owner'), dragEvent('dragstart', transfer))
    const strip = container.querySelector('[data-testid="bucket-remove"]')
    expect(strip?.textContent).toBe('Drop here to remove from bucket')
    await fireEvent(strip, dragEvent('dragover', transfer))
    await fireEvent(strip, dragEvent('drop', transfer))
    await vi.waitFor(() => expect(rail(container)).toEqual([
      '[Machine audit]', '[Later]', 'Audit owner', 'Dashboard child', 'Other run', 'Loose run',
    ]))
    expect(fetch).toHaveBeenLastCalledWith('/api/v1/threads/nth_owner/bucket', expect.objectContaining({
      method: 'PUT', body: JSON.stringify({ bucketId: null }),
    }))
  })

  // Collapsing hides a bucket's threads but keeps its heading, count and
  // place; the heading shows when a thread inside is working; the choice
  // survives a reload; a thread can still be dropped into it.
  it('collapses a bucket and keeps it in place', async () => {
    explorer.dataset = explorer.dataset.map((row) => (row.nativeId === 'native-owner'
      ? { ...row, runtime: { ...row.runtime, threadStatus: 'working', turnStartedAtMs: Date.now() } }
      : row))
    app.sessions = app.sessions.map((one) => (one.threadId === 'native-owner' ? { ...one, threadStatus: 'working', turnStartedAtMs: Date.now() } : one))
    serverBuckets = [
      { id: 'bkt_a', name: 'Machine audit', threads: [member('owner')] },
      { id: 'bkt_b', name: 'Later', threads: [] },
    ]
    explorer.buckets = serverBuckets
    const first = render(App)
    expect(rail(first.container)).toEqual(['[Machine audit]', 'Audit owner', 'Dashboard child', '[Later]', 'Other run', 'Loose run'])
    expect(first.container.querySelector('[data-testid="bucket-state"]')).toBeNull()

    await fireEvent.click(first.container.querySelector('[aria-label="Collapse Machine audit"]'))
    expect(rail(first.container)).toEqual(['[Machine audit]', '[Later]', 'Other run', 'Loose run'])
    expect(bucketHeader(first.container, 'Machine audit').querySelector('.sidebar-bucket-count').textContent).toBe('2')
    expect(first.container.querySelector('[data-testid="bucket-state"]')?.classList.contains('working')).toBe(true)

    cleanup()
    const reloaded = render(App)
    expect(rail(reloaded.container)).toEqual(['[Machine audit]', '[Later]', 'Other run', 'Loose run'])
    await dragOnto(rowOf(reloaded.container, 'Loose run'), bucketHeader(reloaded.container, 'Machine audit'))
    await vi.waitFor(() => expect(rail(reloaded.container)).toEqual(['[Machine audit]', '[Later]', 'Other run']))
    expect(bucketHeader(reloaded.container, 'Machine audit').querySelector('.sidebar-bucket-count').textContent).toBe('3')

    await fireEvent.click(reloaded.container.querySelector('[aria-label="Expand Machine audit"]'))
    expect(rail(reloaded.container)).toEqual(['[Machine audit]', 'Audit owner', 'Dashboard child', 'Loose run', '[Later]', 'Other run'])
  })
})
