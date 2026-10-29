// @vitest-environment jsdom
//
// The failure surface is the mounted live pane: creation begins without a
// provider UUID, then the canonical identity arrives asynchronously. State-
// only tests cannot prove the bottom bar replaced its starting text.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, waitFor } from '@testing-library/svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { app } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { dropSessionStore, getSessionStore } from '../../state.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
  }
  close() {}
}

const UUID_ONE = '11111111-aaaa-4bbb-8ccc-dddddddddddd'
const UUID_TWO = '22222222-aaaa-4bbb-8ccc-dddddddddddd'

function jsonResponse(body, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: async () => body,
    text: async () => '',
  }
}

function identity(hostedThreadId, nativeThreadId, provisional = false) {
  return {
    displayName: '',
    nativeThreadId,
    nativeThreadIdProvisional: provisional,
    hostedThreadId,
    provider: 'claude',
    matchCount: 1,
  }
}

function summary(overrides = {}) {
  return {
    id: 'identity-session',
    provider: 'claude',
    hostedThreadId: 'nht_identity',
    threadId: 'summary-id-must-not-render',
    cwd: '/workspace/nano',
    status: 'starting',
    turnActive: false,
    metadata: { revision: 1, fields: {} },
    ...overrides,
  }
}

function tab(overrides = {}) {
  return {
    id: 1,
    catalogId: '',
    sessionId: 'identity-session',
    provider: 'claude',
    nativeId: 'summary-id-must-not-render',
    title: 'Identity timing',
    ...overrides,
  }
}

async function flush() {
  for (let i = 0; i < 4; i += 1) await Promise.resolve()
}

beforeEach(() => {
  vi.stubGlobal('EventSource', MockEventSource)
  localStorage.clear()
  localStorage.setItem('nano-view', 'clean')
  explorer.dataset = []
  app.sessions = []
})

afterEach(() => {
  cleanup()
  dropSessionStore('identity-session')
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('live bottom-bar native identity', () => {
  it('shows a starting state, then the resolver UUID without refresh or remount', async () => {
    let releaseIdentity
    const lateIdentity = new Promise((resolve) => { releaseIdentity = resolve })
    const live = summary()
    app.sessions = [live]
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      const target = String(url)
      if (target.startsWith('/api/v1/identity/resolve')) return lateIdentity
      if (target.endsWith('/state')) return jsonResponse(live)
      return jsonResponse({})
    }))

    const { container } = render(ThreadPaneView, { tab: tab() })
    const identityText = () => container.querySelector('.info-bar-item.identity .info-bar-field')?.textContent

    await waitFor(() => expect(identityText()).toBe('native id pending…'))
    expect(container.querySelector('.info-bar')?.textContent).not.toContain('summary-id-must-not-render')

    releaseIdentity(jsonResponse(identity('nht_identity', UUID_ONE)))

    await waitFor(() => expect(identityText()).toBe(UUID_ONE))
  })

  it('keeps a provisional Claude id pending, then resolves immediately on a metadata hint', async () => {
    const resolverBodies = [
      identity('nht_identity', 'cs_transport-only', true),
      identity('nht_identity', UUID_ONE),
    ]
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      const target = String(url)
      if (target.startsWith('/api/v1/identity/resolve')) return jsonResponse(resolverBodies.shift())
      if (target.endsWith('/state')) return jsonResponse({})
      if (target.includes('/metadata')) return jsonResponse({ revision: 2, fields: {} })
      return jsonResponse({})
    }))

    const session = getSessionStore('identity-session', summary())
    await flush()
    expect(session.store.nativeIdentityStatus).toBe('pending')
    expect(session.store.resolvedNativeThreadId).toBe('')

    session.applySummary(summary({ metadata: { revision: 2, fields: {} } }))
    await waitFor(() => expect(session.store.resolvedNativeThreadId).toBe(UUID_ONE))
  })

  it('retries transient 404 and 5xx responses and never displays the runtime hint', async () => {
    vi.useFakeTimers()
    let attempts = 0
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      const target = String(url)
      if (target.startsWith('/api/v1/identity/resolve')) {
        attempts += 1
        if (attempts === 1) return jsonResponse({}, 404)
        if (attempts === 2) return jsonResponse({}, 503)
        return jsonResponse(identity('nht_identity', UUID_ONE))
      }
      if (target.endsWith('/state')) return jsonResponse({})
      return jsonResponse({})
    }))

    const session = getSessionStore('identity-session', summary())
    await flush()
    expect(session.store.nativeIdentityStatus).toBe('pending')
    expect(session.store.resolvedNativeThreadId).toBe('')

    await vi.advanceTimersByTimeAsync(250)
    await flush()
    expect(session.store.nativeIdentityStatus).toBe('pending')
    await vi.advanceTimersByTimeAsync(500)
    await flush()
    expect(session.store.resolvedNativeThreadId).toBe(UUID_ONE)
    expect(session.store.resolvedNativeThreadId).not.toBe('summary-id-must-not-render')
  })

  it('clears a prior UUID across hosted-reference replacement and ignores a late disconnected response', async () => {
    let releaseReplacement
    const replacement = new Promise((resolve) => { releaseReplacement = resolve })
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      const target = String(url)
      if (target.includes('ref=nht_identity_next')) return replacement
      if (target.startsWith('/api/v1/identity/resolve')) return jsonResponse(identity('nht_identity', UUID_ONE))
      if (target.endsWith('/state')) return jsonResponse({})
      return jsonResponse({})
    }))

    const session = getSessionStore('identity-session', summary())
    await waitFor(() => expect(session.store.resolvedNativeThreadId).toBe(UUID_ONE))

    session.applySummary(summary({ hostedThreadId: 'nht_identity_next', metadata: { revision: 1, fields: {} } }))
    expect(session.store.nativeIdentityStatus).toBe('pending')
    expect(session.store.resolvedNativeThreadId).toBe('')

    session.disconnect()
    releaseReplacement(jsonResponse(identity('nht_identity_next', UUID_TWO)))
    await flush()
    expect(session.store.resolvedNativeThreadId).toBe('')
  })
})
