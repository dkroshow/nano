// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { refreshSessions } from './app.svelte.js'
import { __resetBrowserDiagnosticsForTest } from './browser_diagnostics.js'

function response(sessions) {
  return { json: async () => ({ sessions, detached: [], defaults: {} }) }
}

function diagnosticEvents() {
  return JSON.parse(sessionStorage.getItem('nano.browser-diagnostics.v1') || '{"events":[]}').events
}

describe('session registry diagnostics', () => {
  beforeEach(() => {
    __resetBrowserDiagnosticsForTest()
    sessionStorage.clear()
  })

  afterEach(() => {
    __resetBrowserDiagnosticsForTest()
    vi.unstubAllGlobals()
  })

  it('ignores response-order churn and records only sessions whose state changed', async () => {
    const first = [
      { id: 'registry-a', provider: 'codex', threadId: 'thread-a', status: 'ready', turnActive: false },
      { id: 'registry-b', provider: 'claude', threadId: 'thread-b', status: 'ready', turnActive: false },
    ]
    const changed = { ...first[1], turnActive: true }
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response(first))
      .mockResolvedValueOnce(response([...first].reverse()))
      .mockResolvedValueOnce(response([changed, first[0]]))
    vi.stubGlobal('fetch', fetchMock)

    await refreshSessions()
    await new Promise((resolve) => setTimeout(resolve, 0))
    const initialCount = diagnosticEvents().length

    await refreshSessions()
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(diagnosticEvents()).toHaveLength(initialCount)

    await refreshSessions()
    await new Promise((resolve) => setTimeout(resolve, 0))
    const added = diagnosticEvents().slice(initialCount)
    expect(added.filter((event) => event.kind === 'registry.refresh')).toHaveLength(1)
    expect(added.filter((event) => event.kind === 'registry.session').map((event) => event.sessionId))
      .toEqual(['registry-b'])
  })
})
