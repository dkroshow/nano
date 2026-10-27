// The session list (~1.6 MB) used to be re-read on every runtime event, so one
// tab pulled ~125 KB/s in light traffic and MB/s during a restart. Runtime
// events now patch app.sessions in place; the list is read only when an event
// says so or when session identity no longer matches the rows. Fake timers
// from the first line: startPolling's fallback interval is installed once per
// module, and this file owns it.
import { describe, it, expect, vi, beforeAll, beforeEach } from 'vitest'
import { app, openSession, startPolling } from './app.svelte.js'
import { startPolling as startCatalogStream } from './explorer.svelte.js'
import { getSessionStore } from './state.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    this.listeners = {}
    MockEventSource.instances.push(this)
  }
  addEventListener(type, fn) {
    ;(this.listeners[type] ??= []).push(fn)
  }
  removeEventListener() {}
  close() {}
  emit(type, data) {
    for (const fn of this.listeners[type] ?? []) fn({ data: JSON.stringify(data) })
  }
}
MockEventSource.instances = []

const session = { id: 's1', provider: 'claude', threadId: 'n1', status: 'ready', turnActive: false, hostedThreadId: 'nht_1' }
const row = (runtime) => ({ provider: 'claude', nativeId: 'n1', runtime: { attached: true, detached: false, sessionId: 's1', status: 'ready', ...runtime } })

let listResponses = []
let metadataResponses = []
let stateResponses = []
const listReads = () => fetchMock.mock.calls.filter(([url]) => String(url) === '/api/sessions').length
const metadataReads = () => fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/metadata')).length
let fetchMock
let stream

beforeAll(async () => {
  vi.useFakeTimers()
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('document', { visibilityState: 'visible', addEventListener: () => {}, removeEventListener: () => {} })
  fetchMock = vi.fn(async (url) => {
    const u = String(url)
    if (u === '/api/sessions') {
      const next = listResponses.shift()
      if (next === 'fail') throw new TypeError('network down')
      if (next instanceof Promise) return next
      return { ok: true, json: async () => ({ sessions: next ?? [session], detached: [], defaults: {} }) }
    }
    if (u.endsWith('/metadata')) {
      const next = await metadataResponses.shift()
      if (next === 'fail') return { ok: false, status: 503, json: async () => ({}) }
      return { ok: true, json: async () => ({ revision: next ?? 7 }) }
    }
    if (u.endsWith('/state') && stateResponses.length) {
      const body = await stateResponses.shift()
      return { ok: true, json: async () => body }
    }
    if (u.includes('/api/v1/threads?')) return { ok: true, status: 200, json: async () => ({ items: [], nextCursor: null, catalogVersion: 1 }) }
    return { ok: true, status: 200, json: async () => ({ state: 'ready', catalogVersion: null, roots: [] }) }
  })
  vi.stubGlobal('fetch', fetchMock)
  await startPolling()
  startCatalogStream()
  stream = MockEventSource.instances.at(-1)
  stream.emit('open') // opens the stream; the snapshot read follows
  await vi.advanceTimersByTimeAsync(0)
})

beforeEach(async () => {
  listResponses = []
  metadataResponses = []
  await vi.advanceTimersByTimeAsync(0)
  fetchMock.mockClear()
})

describe('session list follows runtime events', () => {
  it('patches a runtime change without reading the list', async () => {
    stream.emit('runtime', { rows: [row({ turnActive: true, status: 'ready' })] })
    await vi.advanceTimersByTimeAsync(0)

    expect(listReads()).toBe(0)
    expect(app.sessions.find((s) => s.id === 's1')).toMatchObject({ turnActive: true })
  })

  // A rotation (or spawn, exit, close) is only in the list; the rows show the
  // mismatch, so one read follows, and none once the list agrees again.
  it('reads the list when identity no longer matches the rows', async () => {
    listResponses = [[{ ...session, id: 's2' }]]
    stream.emit('runtime', { rows: [row({ sessionId: 's2' })] })
    await vi.advanceTimersByTimeAsync(0)
    expect(listReads()).toBe(1)
    expect(app.sessions.map((s) => s.id)).toEqual(['s2'])

    stream.emit('runtime', { rows: [row({ sessionId: 's2', turnActive: true })] })
    await vi.advanceTimersByTimeAsync(0)
    expect(listReads()).toBe(1)

    listResponses = [[session]]
    stream.emit('runtime', { rows: [row({})] })
    await vi.advanceTimersByTimeAsync(0)
    expect(listReads()).toBe(2)
  })

  // A list read started before an event used to overwrite that event's patch.
  it('keeps a patch that arrives while a list read is in flight', async () => {
    let release
    listResponses = [new Promise((resolve) => { release = resolve })]
    stream.emit('runtime', { rows: [row({})], refresh: true })
    stream.emit('runtime', { rows: [row({ status: 'reconnecting' })] })
    release({ ok: true, json: async () => ({ sessions: [{ ...session, status: 'ready' }], detached: [], defaults: {} }) })
    await vi.advanceTimersByTimeAsync(0)

    expect(app.sessions.find((s) => s.id === 's1')).toMatchObject({ status: 'reconnecting' })
  })

  // The always-on 30 s poll cost ~53 KB/s per tab with a healthy stream; a
  // failed read must still be retried, or a rename in another tab is lost.
  it('polls only while a read is owed', async () => {
    stream.emit('runtime', { rows: [row({ status: 'ready' })] })
    await vi.advanceTimersByTimeAsync(60_000)
    expect(listReads()).toBe(0)

    listResponses = ['fail']
    stream.emit('runtime', { rows: [row({})], refresh: true })
    await vi.advanceTimersByTimeAsync(0)
    await vi.advanceTimersByTimeAsync(30_000)
    expect(listReads()).toBe(2)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(listReads()).toBe(2)
  })

  // The hint rides every event, so a failed read is retried on the next one;
  // a hint during a read gets one follow-up read; and a list read that started
  // earlier, carrying older metadata or reporting it unavailable, never
  // replaces what a later read found.
  it('keeps an open thread\'s metadata at the newest revision without list reads', async () => {
    const store = getSessionStore('s1', session).store
    metadataResponses = ['fail', 7]
    stream.emit('runtime', { rows: [row({ metadataRevision: 7 })] })
    await vi.advanceTimersByTimeAsync(0)
    stream.emit('runtime', { rows: [row({ metadataRevision: 7 })] })
    await vi.advanceTimersByTimeAsync(0)
    expect(metadataReads()).toBe(2)
    expect(store.hostedMetadata?.revision).toBe(7)
    expect(listReads()).toBe(0)

    let releaseRead
    metadataResponses = [new Promise((resolve) => { releaseRead = resolve }), 9]
    stream.emit('runtime', { rows: [row({ metadataRevision: 8 })] })
    stream.emit('runtime', { rows: [row({ metadataRevision: 9 })] })
    releaseRead(8)
    await vi.advanceTimersByTimeAsync(0)
    expect(store.hostedMetadata?.revision).toBe(9)

    for (const [revision, stale] of [[10, { metadata: { revision: 6 } }], [11, { metadataUnavailable: true }]]) {
      let releaseList
      listResponses = [new Promise((resolve) => { releaseList = resolve })]
      stream.emit('runtime', { rows: [row({})], refresh: true })
      metadataResponses = [revision]
      stream.emit('runtime', { rows: [row({ metadataRevision: revision })] })
      await vi.advanceTimersByTimeAsync(0)
      releaseList({ ok: true, json: async () => ({ sessions: [{ ...session, ...stale }], detached: [], defaults: {} }) })
      await vi.advanceTimersByTimeAsync(0)
      expect(store.hostedMetadata?.revision, JSON.stringify(stale)).toBe(revision)
    }

    // A /state read that started before a metadata read lands after it.
    let releaseState
    stateResponses = [new Promise((resolve) => { releaseState = resolve })]
    const other = getSessionStore('s9', { ...session, id: 's9', hostedThreadId: 'nht_9' })
    metadataResponses = [4]
    other.noteMetadataRevision(4)
    await vi.advanceTimersByTimeAsync(0)
    releaseState({ provider: 'claude', status: 'ready', metadata: { revision: 1 } })
    await vi.advanceTimersByTimeAsync(0)
    expect(other.store.hostedMetadata?.revision).toBe(4)

    // Start order is not server order: a later-started read can still answer
    // with an older revision than the store already holds.
    metadataResponses = [3]
    other.noteMetadataRevision(5)
    await vi.advanceTimersByTimeAsync(0)
    expect(other.store.hostedMetadata?.revision).toBe(4)
  })
})

// A delta names only the threads that changed, so a session the delta does
// not name is not evidence of a change. Read as one, every partial delta
// re-read the list.
describe('partial runtime deltas', () => {
  it('reads the list only for a named row that disagrees with it', async () => {
    listResponses = [[session]]
    stream.emit('runtime', { rows: [row({})], refresh: true })
    await vi.advanceTimersByTimeAsync(0)
    fetchMock.mockClear()

    stream.emit('runtime', { rows: [{ provider: 'claude', nativeId: 'n9', runtime: { attached: false, detached: true } }] })
    await vi.advanceTimersByTimeAsync(0)
    expect(listReads()).toBe(0)
    expect(app.sessions.map((s) => s.id)).toEqual(['s1'])

    listResponses = [[]]
    stream.emit('runtime', { rows: [{ provider: 'claude', nativeId: 'n1', runtime: null }] })
    await vi.advanceTimersByTimeAsync(0)
    expect(listReads()).toBe(1)
  })
})

// Each event names only the threads that changed, so the rows applied over a
// list read must be every event's since the read began, not the last one's.
describe('runtime events during a list read', () => {
  it('keeps every thread changed while the list response was in flight', async () => {
    const other = { ...session, id: 's2', threadId: 'n2', hostedThreadId: 'nht_2' }
    const rowFor = (s, turnActive) => ({ provider: s.provider, nativeId: s.threadId, runtime: { attached: true, detached: false, sessionId: s.id, status: 'ready', turnActive } })
    listResponses = [[session, other]]
    stream.emit('runtime', { rows: [rowFor(session, false)], refresh: true })
    await vi.advanceTimersByTimeAsync(0)

    let release
    listResponses = [new Promise((resolve) => { release = resolve })]
    stream.emit('runtime', { rows: [rowFor(session, false)], refresh: true })
    stream.emit('runtime', { rows: [rowFor(session, true)] })
    stream.emit('runtime', { rows: [rowFor(other, true)] })
    release({ ok: true, json: async () => ({ sessions: [session, other], detached: [], defaults: {} }) })
    await vi.advanceTimersByTimeAsync(0)

    expect(app.sessions.map((s) => [s.id, s.turnActive])).toEqual([['s1', true], ['s2', true]])
  })
})

// Failure caught (2026-10-19): every session that started or exited anywhere
// in the fleet made the server ask every page to re-read the whole list (497
// sessions, ~61 KB gzipped); a phone on slow Wi-Fi paid it each time. The
// event now carries the sessions that changed. Boundary: the runtime event on
// the page's session list.
describe('session changes ride the runtime event', () => {
  const joined = { id: 's2', provider: 'claude', threadId: 'n2', status: 'ready', turnActive: false, hostedThreadId: 'nht_2' }
  const joinedRow = { provider: 'claude', nativeId: 'n2', runtime: { attached: true, detached: false, sessionId: 's2', status: 'ready' } }
  const parked = { provider: 'claude', nativeThreadId: 'n3', hostedThreadId: 'nht_3', status: 'reconnect_failed' }

  // A restart's re-attach drains the detached list one thread at a time; each
  // event carries only the detached rows that changed and the keys that left.
  it('adds and removes sessions and detached rows without reading the list', async () => {
    stream.emit('runtime', { rows: [joinedRow], sessions: [joined] })
    await vi.advanceTimersByTimeAsync(0)
    expect(app.sessions.map((s) => s.id)).toContain('s2')

    stream.emit('runtime', { rows: [{ provider: 'claude', nativeId: 'n2', runtime: null }], removedSessions: ['s2'], detachedRows: [parked] })
    await vi.advanceTimersByTimeAsync(0)
    expect(app.sessions.map((s) => s.id)).not.toContain('s2')
    expect(app.detached).toEqual([parked])

    stream.emit('runtime', { rows: [], removedDetached: ['claude:n3'] })
    await vi.advanceTimersByTimeAsync(0)
    expect(app.detached).toEqual([])
    expect(listReads()).toBe(0)
  })
})

describe('session changes during a list read', () => {
  it('keeps a session that joined while an older list read was in flight', async () => {
    let answer
    listResponses = [new Promise((resolve) => { answer = resolve })]
    stream.emit('runtime', { rows: [row({})], refresh: true })
    const late = { id: 's4', provider: 'claude', threadId: 'n4', status: 'ready', turnActive: false, hostedThreadId: 'nht_4' }
    stream.emit('runtime', { rows: [{ provider: 'claude', nativeId: 'n4', runtime: { attached: true, detached: false, sessionId: 's4', status: 'ready' } }], sessions: [late] })
    answer({ ok: true, json: async () => ({ sessions: [session], detached: [], defaults: {} }) })
    await vi.advanceTimersByTimeAsync(0)
    expect(app.sessions.map((s) => s.id)).toContain('s4')
  })
})

// Failure caught (2026-10-19): opening a thread re-read the whole session
// list even when the list already held it, one more full download per open.
// Boundary: openSession.
describe('opening a listed session', () => {
  it('reads no list', async () => {
    await openSession('s1', { writeReference: false })
    await vi.advanceTimersByTimeAsync(0)
    expect(listReads()).toBe(0)
  })
})
