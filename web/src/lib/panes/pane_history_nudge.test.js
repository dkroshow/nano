// @vitest-environment jsdom
//
// An open pane re-reads its saved history only when its own thread changed.
// Catalog and fleet events name the threads that changed; other panes stay
// put. Catches the waste where every event re-read every pane: about 1,000
// reads a minute, 98% unchanged, 13-21% of Nano's CPU. A live file-only pane
// still follows its own thread, because its saved history is what the pane
// shows once the session ends. Boundary: the real pane views and their
// history channels, driven by the real catalog stream handler.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, waitFor } from '@testing-library/svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { app } from '../../app.svelte.js'
import { explorer, startPolling, stopPolling } from '../../explorer.svelte.js'
import { dropSessionStore } from '../../state.svelte.js'
import { __resetPanesForTest } from '../../panes.svelte.js'

class FakeStream {
  constructor(url) {
    this.url = url
    this.listeners = {}
    FakeStream.opened.push(this)
  }
  addEventListener(type, fn) { (this.listeners[type] ??= []).push(fn) }
  removeEventListener(type, fn) { this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn) }
  close() {}
  emit(type, payload) {
    for (const fn of this.listeners[type] ?? []) fn({ data: JSON.stringify(payload) })
  }
}
FakeStream.opened = []

const rows = ['a', 'b', 'c'].map((key) => ({
  id: `nth_${key}`, provider: key === 'c' ? 'claude' : 'codex', nativeId: `native-${key}`, title: `Thread ${key}`, metadata: {},
}))
const fileOnlySession = {
  id: 'session-c', hostedThreadId: 'nht-c', provider: 'claude', threadId: 'native-c', cwd: '/w',
  status: 'ready', turnActive: false, fileTranscript: true,
}

let historyReads
let savedRows
let holdHistory
function readsOf(threadId) {
  return historyReads.filter((id) => id === threadId).length
}

function tab(key, extra = {}) {
  const row = rows.find((candidate) => candidate.id === `nth_${key}`)
  return { id: key, catalogId: row.id, sessionId: '', provider: row.provider, nativeId: row.nativeId, title: row.title, ...extra }
}

beforeEach(() => {
  __resetPanesForTest()
  FakeStream.opened = []
  vi.stubGlobal('EventSource', FakeStream)
  historyReads = []
  savedRows = {}
  holdHistory = false
  vi.stubGlobal('fetch', vi.fn(async (url) => {
    const target = String(url)
    const history = target.match(/\/api\/v1\/threads\/([^/?]+)\/transcript/)
    if (history) historyReads.push(history[1])
    if (history && holdHistory) return new Promise(() => {})
    const body = history
      ? { items: savedRows[history[1]] ?? [], turns: [], page: { hasMoreBefore: false }, projection: { state: 'ready', epoch: 'tpe_1' } }
      : target.startsWith('/api/v1/threads?') ? { items: rows.filter((row) => target.includes(row.id)) } : {}
    return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
  }))
  explorer.dataset = rows
  app.sessions = [fileOnlySession]
  localStorage.setItem('nano-view', 'clean')
})

afterEach(() => {
  vi.useRealTimers()
  cleanup()
  stopPolling()
  dropSessionStore('session-c')
  vi.unstubAllGlobals()
})

function savedAnswer(id, text) {
  return { id, type: 'agent_message', category: 'messages', text }
}

async function openStream() {
  startPolling()
  const stream = FakeStream.opened.find((source) => source.url.includes('/thread-index/events'))
  stream.emit('open', {})
  return stream
}

// Lets every queued read answer, so counts compare settled states.
const settle = () => new Promise((resolve) => setTimeout(resolve, 50))

describe('pane history re-reads', () => {
  it('re-reads only the panes whose thread an event names', async () => {
    render(ThreadPaneView, { tab: tab('a') })
    render(ThreadPaneView, { tab: tab('b') })
    const stream = await openStream()
    await waitFor(() => expect(readsOf('nth_a')).toBeGreaterThan(0))
    await settle()
    const before = { a: readsOf('nth_a'), b: readsOf('nth_b') }

    stream.emit('catalog', { threadIds: ['nth_a'], catalogVersion: 2 })
    stream.emit('fleet', { threadId: 'nth_a', from: 'working', to: 'idle' })
    await settle()

    expect({ a: readsOf('nth_a') > before.a, b: readsOf('nth_b') - before.b }).toEqual({ a: true, b: 0 })
  })

  it('keeps a live file-only pane\'s saved history current, so ending the session keeps its newest answer', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    savedRows.nth_c = [savedAnswer('old', 'older answer')]
    const { container } = render(ThreadPaneView, { tab: tab('c', { sessionId: 'session-c' }) })
    const stream = await openStream()
    await waitFor(() => expect(readsOf('nth_c')).toBeGreaterThan(0))
    await settle()

    savedRows.nth_c = [savedAnswer('old', 'older answer'), savedAnswer('new', 'newer answer')]
    const read = readsOf('nth_c')
    stream.emit('catalog', { threadIds: ['nth_c'], catalogVersion: 2 })
    await waitFor(() => expect(readsOf('nth_c')).toBeGreaterThan(read))
    await settle()

    // The session ends while the read-only view's own refresh is still out.
    holdHistory = true
    app.sessions = []
    await vi.advanceTimersByTimeAsync(6000)

    await waitFor(() => expect(container.textContent).toContain('newer answer'))
  })
})
