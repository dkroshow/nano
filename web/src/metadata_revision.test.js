import { afterEach, describe, expect, it, vi } from 'vitest'
import { dropSessionStore, getSessionStore } from './state.svelte.js'

class MockEventSource {
  close() {}
}

const headers = { get: () => null }

describe('hosted metadata revision convergence', () => {
  afterEach(() => {
    dropSessionStore('metadata-a')
    dropSessionStore('metadata-b')
    vi.unstubAllGlobals()
  })

  it('refetches exactly once for a newer summary revision, ignores equal/older revisions, and converges tabs', async () => {
    let revision = 1
    const fetchMock = vi.fn(async (url) => {
      if (String(url).includes('/api/v1/hosted-threads/nht_meta/metadata')) {
        return { ok: true, headers, json: async () => ({
          hostedThreadId: 'nht_meta', revision, fields: {
            serviceTier: { effectiveValue: revision === 1 ? 'priority' : 'default', effectiveDisposition: 'confirmed', displayValue: revision === 1 ? 'priority' : 'default', displayDisposition: 'confirmed', displayFreshness: 'live' },
          },
        }) }
      }
      return { ok: true, headers, json: async () => ({}) }
    })
    vi.stubGlobal('EventSource', MockEventSource)
    vi.stubGlobal('fetch', fetchMock)
    const initial = { id: 'metadata-a', provider: 'codex', threadId: 'native-1', status: 'ready', hostedThreadId: 'nht_meta', posture: { serviceTier: 'priority' }, metadata: { revision: 1, fields: {} } }
    const a = getSessionStore('metadata-a', initial)
    const b = getSessionStore('metadata-b', { ...initial, id: 'metadata-b' })
		await new Promise((resolve) => setTimeout(resolve, 0))
    fetchMock.mockClear()

    revision = 2
    const changed = { ...initial, metadata: { revision: 2, fields: {} }, posture: { serviceTier: 'default' } }
    a.applySummary(changed)
    b.applySummary(changed)
		await new Promise((resolve) => setTimeout(resolve, 0))
    expect(fetchMock.mock.calls.filter(([url]) => String(url).includes('/api/v1/hosted-threads/nht_meta/metadata'))).toHaveLength(2)
    expect(a.store.posture.serviceTier).toBe('default')
    expect(b.store.posture.serviceTier).toBe('default')

    fetchMock.mockClear()
    a.applySummary(changed)
    a.applySummary({ ...changed, metadata: { revision: 1, fields: {} } })
    await Promise.resolve()
    expect(fetchMock.mock.calls.filter(([url]) => String(url).includes('/api/v1/hosted-threads/nht_meta/metadata'))).toHaveLength(0)
  })

  it('marks failed canonical refetch unavailable and clears hosted facts', async () => {
    vi.stubGlobal('EventSource', MockEventSource)
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 503, headers, text: async () => 'unavailable' })))
    const session = getSessionStore('metadata-a', { id: 'metadata-a', provider: 'codex', status: 'ready', hostedThreadId: 'nht_meta', posture: { model: 'known' }, metadata: { revision: 1, fields: {} } })
    session.applySummary({ provider: 'codex', status: 'ready', metadataUnavailable: true, posture: {} })
    expect(session.store.metadataUnavailable).toBe(true)
    expect(session.store.posture).toEqual({})

		// A later summary carrying a readable canonical view is direct recovery
		// evidence. The old failure bit must not remain a browser-side veto.
		session.applySummary({
			provider: 'codex', status: 'ready',
			metadata: { revision: 1, fields: { model: { displayValue: 'known', displayDisposition: 'confirmed', displayFreshness: 'live' } } },
		})
		expect(session.store.metadataUnavailable).toBe(false)
		expect(session.store.posture.model).toBe('known')
  })

  it('uses the server display projection for imported historical intent', async () => {
    vi.stubGlobal('EventSource', MockEventSource)
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      if (String(url).includes('/api/v1/hosted-threads/nht_meta/metadata')) {
        return { ok: true, headers, json: async () => ({
          hostedThreadId: 'nht_meta', revision: 2, fields: {
            model: {
              desiredValue: 'opus', desiredDisposition: 'requested',
              effectiveValue: 'claude-opus-5', effectiveDisposition: 'observed',
              displayValue: 'claude-opus-5', displayDisposition: 'observed', displayFreshness: 'historical',
            },
          },
        }) }
      }
      return { ok: true, headers, json: async () => ({}) }
    }))
    const session = getSessionStore('metadata-a', { id: 'metadata-a', provider: 'claude', status: 'ready', hostedThreadId: 'nht_meta', metadata: { revision: 1, fields: {} } })
    session.applySummary({ provider: 'claude', status: 'ready', hostedThreadId: 'nht_meta', metadata: { revision: 2, fields: {} } })
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(session.store.posture.model).toBe('claude-opus-5')
  })

  it('reconnects by refetching current session state, so a lost change cursor cannot preserve stale facts', async () => {
    let stateReads = 0
    vi.stubGlobal('EventSource', MockEventSource)
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      if (String(url).endsWith('/state')) {
        stateReads += 1
        return { ok: true, headers, json: async () => ({
          provider: 'codex', threadId: 'native-1', status: 'ready',
          posture: { serviceTier: 'default' },
          metadata: { revision: 4, fields: {
            serviceTier: { displayValue: 'default', displayDisposition: 'confirmed', displayFreshness: 'live' },
          } },
        }) }
      }
      return { ok: true, headers, json: async () => ({}) }
    }))
    getSessionStore('metadata-a', { id: 'metadata-a', provider: 'codex', status: 'ready', hostedThreadId: 'nht_meta', posture: { serviceTier: 'priority' }, metadata: { revision: 1, fields: {} } })
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(stateReads).toBe(1)
    dropSessionStore('metadata-a')

    // A fresh connection has no reliable bounded change-ring cursor. It must
    // load the current canonical session view, just as a refetchRequired
    // metadata/changes response instructs the browser to do.
    const resumed = getSessionStore('metadata-a', { id: 'metadata-a', provider: 'codex', status: 'ready', hostedThreadId: 'nht_meta', posture: { serviceTier: 'priority' }, metadata: { revision: 1, fields: {} } })
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(stateReads).toBe(2)
    expect(resumed.store.posture.serviceTier).toBe('default')
  })
})
