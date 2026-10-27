// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { app, openSession } from './app.svelte.js'
import { explorer } from './explorer.svelte.js'
import { __resetPanesForTest, activeTab, openThreadTab, tabTarget } from './panes.svelte.js'

class MockEventSource {
  close() {}
  addEventListener() {}
  removeEventListener() {}
}

function response(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

const oldSession = {
  id: 'session-old', hostedThreadId: 'nht-old', provider: 'codex', threadId: 'native-old', cwd: '/workspace', status: 'ready', turnActive: false,
}
const nextSession = {
  id: 'session-next', hostedThreadId: 'nht-next', provider: 'codex', threadId: 'native-next', cwd: '/workspace', status: 'ready', turnActive: true,
}

beforeEach(() => {
  vi.stubGlobal('EventSource', MockEventSource)
  __resetPanesForTest()

  app.sessions = [oldSession, nextSession]
  app.activeId = oldSession.id
  explorer.dataset = []
  openThreadTab(tabTarget({ session: oldSession }))
})

afterEach(() => {
  vi.unstubAllGlobals()
  __resetPanesForTest()

})

describe('warm-cache live thread navigation', () => {
  it('switches immediately without fetching a destination snapshot', async () => {
    const fetchMock = vi.fn(async (url) => {
      if (String(url) === '/api/sessions') return response({ sessions: [oldSession, nextSession], detached: [], defaults: {} })
      return response({})
    })
    vi.stubGlobal('fetch', fetchMock)

    const opening = openSession(nextSession.id, { requireFresh: true })
    expect(activeTab()?.sessionId).toBe(nextSession.id)
    await opening

    expect(fetchMock.mock.calls.some(([url]) => String(url).includes('/conversation'))).toBe(false)
  })

  it('rapid selections finish on the latest thread without network races', async () => {
    const thirdSession = { ...nextSession, id: 'session-third', hostedThreadId: 'nht-third', threadId: 'native-third' }
    app.sessions = [oldSession, nextSession, thirdSession]
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      const target = String(url)
      if (target === '/api/sessions') return response({ sessions: app.sessions, detached: [], defaults: {} })
      return response({})
    }))

    const first = openSession(nextSession.id, { requireFresh: true })
    const second = openSession(thirdSession.id, { requireFresh: true })
    await Promise.all([first, second])

    expect(activeTab()?.sessionId).toBe(thirdSession.id)
  })
})
