// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  __resetThreadReferenceNavigationForTest,
  app,
  openThreadReference,
  recoverThreadReference,
  refreshSessions,
  startPolling,
  startThreadReferenceNavigation,
} from './app.svelte.js'
import { activeTab, __resetPanesForTest, panes } from './panes.svelte.js'
import { explorer } from './explorer.svelte.js'
import { fileReferenceLinks } from './lib/transcript/file_references.js'

const native = '11111111-2222-4333-8444-555555555555'
const session = {
  id: 'live-session', hostedThreadId: 'nht_live', provider: 'codex',
  threadId: native, status: 'ready', turnActive: false, presentationMode: 'shared',
}

function response(body, ok = true, status = 200) {
  return { ok, status, json: async () => body, text: async () => '' }
}

const flush = async () => {
  await Promise.resolve()
  await Promise.resolve()
  await new Promise((resolve) => setTimeout(resolve))
}

beforeEach(() => {
  __resetThreadReferenceNavigationForTest()
  __resetPanesForTest()
  app.sessions = []
  app.activeId = null
  app.explorerOpen = false
  explorer.dataset = []
  window.history.replaceState({}, '', `/?thread=${native}`)
})

afterEach(() => {
  __resetThreadReferenceNavigationForTest()
  __resetPanesForTest()
  app.explorerOpen = false
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  window.history.replaceState({}, '', '/')
})

describe('thread-reference navigation', () => {
  // Also catches an ordinary load reading the 0.5 MB session list twice
  // before its stream opens (App.svelte's start order).
  it('includes startup time when the first registry read opens the default thread', async () => {
    window.history.replaceState({}, '', '/')
    const fetchMock = vi.fn(async () => response({ sessions: [session], detached: [], defaults: {} }))
    vi.stubGlobal('fetch', fetchMock)
    startThreadReferenceNavigation()
    await startPolling()
    expect(activeTab()).toMatchObject({ openTrigger: 'load', openStartedAt: 0 })
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === '/api/sessions')).toHaveLength(1)
  })

  it('gates newest-session staging, then replaces a live UUID with its hosted continuation', async () => {
    let resolveReference
    const pendingReference = new Promise((resolve) => { resolveReference = resolve })
    vi.stubGlobal('fetch', vi.fn((url) => {
      if (String(url).startsWith('/api/v1/thread-references/')) return pendingReference
      if (String(url) === '/api/sessions') return Promise.resolve(response({ sessions: [session], detached: [], defaults: {} }))
      return Promise.resolve(response({}))
    }))

    startThreadReferenceNavigation()
    expect(app.threadReferenceResolving).toBe(true)
    await refreshSessions()
    expect(activeTab()).toBeNull()

    resolveReference(response({ state: 'resolved', target: {
      route: 'live', hostedThreadId: 'nht_live', sessionId: 'live-session', provider: 'codex', nativeThreadId: native,
    } }))
    await flush()

    expect(app.threadReferenceResolving).toBe(false)
    expect(activeTab()?.sessionId).toBe('live-session')
    expect(activeTab()).toMatchObject({ openTrigger: 'load', openStartedAt: 0 })
    expect(new URL(window.location.href).searchParams.get('thread')).toBe('nht_live')
  })

  // Also catches a failed link reading the 0.5 MB session list again after
  // the page's own read: the link waits for that read, then stages.
  it('shows a failure, releases normal staging, and does not fabricate a history tab', async () => {
    const fetchMock = vi.fn(async (url) => {
      if (String(url).startsWith('/api/v1/thread-references/')) {
        return response({ state: 'unresolved', reason: 'unknown' })
      }
      if (String(url) === '/api/sessions') return response({ sessions: [session], detached: [], defaults: {} })
      return response({})
    })
    vi.stubGlobal('fetch', fetchMock)

    startThreadReferenceNavigation()
    void refreshSessions() // the page's own first read
    await flush()

    expect(app.threadReferenceResolving).toBe(false)
    expect(app.threadReferenceError).toContain('does not know')
    expect(activeTab()?.sessionId).toBe('live-session')
    expect(Object.values(panes.tabs)).toHaveLength(1)
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === '/api/sessions')).toHaveLength(1)
  })

  it('handles popstate historical targets without adding another URL entry', async () => {
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      if (String(url).startsWith('/api/v1/thread-references/')) {
        return response({ state: 'resolved', target: {
          route: 'historical', catalogThreadId: 'nth_exact', provider: 'codex', nativeThreadId: native,
        } })
      }
      if (String(url) === '/api/sessions') return response({ sessions: [], detached: [], defaults: {} })
      return response({})
    }))
    startThreadReferenceNavigation()
    await flush()
    const historyLength = window.history.length

    window.history.replaceState({}, '', `/?thread=${native}`)
    window.dispatchEvent(new PopStateEvent('popstate'))
    await flush()

    expect(activeTab()?.catalogId).toBe('nth_exact')
    expect(activeTab()?.openTrigger).toBe('tab')
    expect(window.history.length).toBe(historyLength)
    expect(new URL(window.location.href).searchParams.get('thread')).toBe(native)
  })

  it('opens Thread Explorer as the recovery action for an unresolved link', () => {
    app.threadReferenceError = 'Nano does not know this exact thread reference.'
    app.explorerOpen = false

    recoverThreadReference()

    expect(app.threadReferenceError).toBe('')
    expect(app.explorerOpen).toBe(true)
  })

  it('opens a local live transcript click without calling the shared resolver', async () => {
    app.sessions = [session]
    const fetchMock = vi.fn(async (url) => {
      if (String(url) === '/api/sessions') return response({ sessions: [session], detached: [], defaults: {} })
      return response({})
    })
    vi.stubGlobal('fetch', fetchMock)
    const pushState = window.history.pushState.bind(window.history)
    let stagedSessionId = ''
    vi.spyOn(window.history, 'pushState').mockImplementation((...args) => {
      stagedSessionId = activeTab()?.sessionId || ''
      return pushState(...args)
    })
    const root = document.createElement('div')
    root.innerHTML = `<a class="nano-thread-link" href="?thread=${native}" data-nano-thread-reference="${native}">open</a>`
    document.body.append(root)
    const action = fileReferenceLinks(root)
    await new Promise((resolve) => queueMicrotask(resolve))

    root.querySelector('a').dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
    await flush()

    expect(fetchMock.mock.calls.some(([url]) => String(url).startsWith('/api/v1/thread-references/'))).toBe(false)
    expect(activeTab()?.sessionId).toBe('live-session')
    expect(stagedSessionId).toBe('live-session')
    expect(new URL(window.location.href).searchParams.get('thread')).toBe('nht_live')
    action.destroy()
    root.remove()
  })

  it('leaves URL state untouched and shows recovery when a clicked reference is unknown', async () => {
    const unknown = 'nht_unknown'
    const before = window.location.href
    const historyLength = window.history.length
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      if (String(url).startsWith('/api/v1/thread-references/')) return response({ state: 'unresolved', reason: 'unknown' })
      if (String(url) === '/api/sessions') return response({ sessions: [], detached: [], defaults: {} })
      return response({})
    }))

    openThreadReference(unknown)
    await flush()

    expect(window.location.href).toBe(before)
    expect(window.history.length).toBe(historyLength)
    expect(app.threadReferenceError).toContain('does not know')
  })

  it('stages a resolved transcript click before writing its URL', async () => {
    const hosted = 'nht_click'
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      if (String(url).startsWith('/api/v1/thread-references/')) {
        return response({ state: 'resolved', target: {
          route: 'historical', catalogThreadId: 'nth_click', provider: 'codex', nativeThreadId: native,
        } })
      }
      if (String(url) === '/api/sessions') return response({ sessions: [], detached: [], defaults: {} })
      return response({})
    }))
    const pushState = window.history.pushState.bind(window.history)
    let stagedCatalogId = ''
    vi.spyOn(window.history, 'pushState').mockImplementation((...args) => {
      stagedCatalogId = activeTab()?.catalogId || ''
      return pushState(...args)
    })
    const root = document.createElement('div')
    root.innerHTML = `<a class="nano-thread-link" href="?thread=${hosted}" data-nano-thread-reference="${hosted}">open</a>`
    document.body.append(root)
    const action = fileReferenceLinks(root)
    await new Promise((resolve) => queueMicrotask(resolve))

    const event = new MouseEvent('click', { bubbles: true, cancelable: true })
    root.querySelector('a').dispatchEvent(event)
    await flush()

    expect(event.defaultPrevented).toBe(true)
    expect(activeTab()?.catalogId).toBe('nth_click')
    expect(stagedCatalogId).toBe('nth_click')
    expect(new URL(window.location.href).searchParams.get('thread')).toBe(hosted)
    action.destroy()
    root.remove()
  })
})
