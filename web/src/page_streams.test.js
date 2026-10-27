// @vitest-environment jsdom
//
// Each pane reads its own thread's stream. The page once also opened a
// page-level message feed that carried a second copy of every open thread's
// rows; it opened for any thread a session summary did not mark file-only.
// Boundary: the mounted app with a restored thread tab.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, waitFor } from '@testing-library/svelte'
import App from './App.svelte'
import { app } from './app.svelte.js'
import { explorer } from './explorer.svelte.js'
import { dropSessionStore } from './state.svelte.js'
import { __resetPanesForTest, openThreadTab, tabTarget } from './panes.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.urls.push(url)
  }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

const session = {
  id: 'session-1', provider: 'codex', threadId: 'native-1', hostedThreadId: 'nht-1', cwd: '/workspace/nano',
  status: 'ready', turnActive: false,
}

beforeEach(() => {
  __resetPanesForTest()
  MockEventSource.urls = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', vi.fn(async (url) => String(url) === '/api/sessions'
    ? jsonResponse({ sessions: [session], detached: [], defaults: {} })
    : jsonResponse({})))
  explorer.dataset = []
  localStorage.setItem('nano-view', 'clean')
})

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')
  vi.unstubAllGlobals()
})

describe('page streams', () => {
  it('opens no page-level message feed, only the pane stream', async () => {
    app.sessions = [session]
    openThreadTab(tabTarget({ item: { id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'Restored', metadata: {} }, session }))
    render(App)
    await waitFor(() => expect(MockEventSource.urls.some((url) => url.includes('/hosted-threads/nht-1/live?'))).toBe(true))
    expect(MockEventSource.urls.filter((url) => url.startsWith('/api/conversations'))).toEqual([])
  })
})
