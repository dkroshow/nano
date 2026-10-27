import { describe, it, expect, vi, afterEach } from 'vitest'
import { getSessionStore, dropSessionStore } from './state.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
  }
  close() {}
}

const headers = { get: () => null }
const grokMeta = { provider: 'grok', threadId: 'nat-grok', cwd: '/w', status: 'ready' }

function stubFetch(turn) {
  const fetchMock = vi.fn(async (url, options) => {
    const u = String(url)
    if (u.endsWith('/state')) return { ok: true, headers, json: async () => grokMeta }
    if (u.includes('/turn-delivery')) return { ok: true, headers, json: async () => ({ operations: [] }) }
    if (u.endsWith('/turn')) return turn(options, u)
    return { ok: true, headers, json: async () => ({}) }
  })
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const openSessions = []
function openStore(id) {
  openSessions.push(id)
  const session = getSessionStore(id)
  session.applySummary({ status: 'ready', provider: 'grok' })
  return session
}

afterEach(() => {
  while (openSessions.length) dropSessionStore(openSessions.pop())
  vi.unstubAllGlobals()
  localStorage.clear()
})

describe('grok send outcomes', () => {
  it('marks the turn active immediately on send, mirroring the claude optimistic lock', async () => {
    stubFetch(() => ({ ok: true, headers, text: async () => '' }))
    const session = openStore('grok-send-ok')

    const outcome = session.sendTurn('hello grok')
    // Set synchronously before the POST resolves — the composer dot must not
    // wait on network round-trip to show the turn is running.
    expect(session.store.turnActive).toBe(true)
    await expect(outcome).resolves.toBe('sent')
    expect(session.store.turnActive).toBe(true)
  })

  it('unwinds the optimistic lock on a definitive rejection, same as every other provider', async () => {
    stubFetch(() => ({ ok: false, status: 400, headers, text: async () => 'rejected' }))
    const session = openStore('grok-send-rejected')

    await expect(session.sendTurn('hello grok')).resolves.toBe('failed')
    expect(session.store.turnActive).toBe(false)
  })
})
