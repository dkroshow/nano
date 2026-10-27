// Lazy re-attach (session-durability slice, provider-generic): GET /api/sessions
// carries a `detached` array that refreshSessions lands on app.detached;
// reattachThread resumes through the normal provider resume path (409 → jump);
// dismissDetached waits for confirmed closure, matched on provider +
// nativeThreadId. fetch is mocked; app state is asserted directly.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { app, refreshSessions, reattachThread, dismissDetached, closeSession } from './app.svelte.js'
import { __resetPanesForTest, detachSessionTabs, openThreadTab, panes, tabTarget } from './panes.svelte.js'

beforeEach(() => {
  app.sessions = []
  app.detached = []
  app.activeId = null
  app.explorerOpen = true
  app.spawning = false
  __resetPanesForTest()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('refreshSessions', () => {
  it('stages the newest live session on a fresh page load', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({
        sessions: [{ id: 'sess_older' }, { id: 'sess_newer' }],
        detached: [],
        defaults: {},
      }),
    })))

    await refreshSessions()

    expect(app.activeId).toBe('sess_newer')
  })

  it('does not let a newer background session take over a fresh page load', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({
        sessions: [
          { id: 'sess_shared', presentationMode: 'shared' },
          { id: 'sess_background', presentationMode: 'background' },
        ],
        detached: [],
        defaults: {},
      }),
    })))

    await refreshSessions()

    expect(app.activeId).toBe('sess_shared')
  })

  it('leaves the workspace alone when only background sessions exist', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({
        sessions: [{ id: 'sess_background', presentationMode: 'background' }],
        detached: [],
        defaults: {},
      }),
    })))

    await refreshSessions()

    expect(app.activeId).toBeNull()
  })

  it('lands the detached array from GET /api/sessions', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({
        sessions: [],
        detached: [{ provider: 'codex', nativeThreadId: 'nat-1', cwd: '/repo/app', model: 'gpt-5' }],
        defaults: {},
      }),
    })))
    await refreshSessions()
    expect(app.detached).toHaveLength(1)
    expect(app.detached[0].nativeThreadId).toBe('nat-1')
  })

  it('defaults detached to empty when the field is absent', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({ sessions: [], defaults: {} }),
    })))
    await refreshSessions()
    expect(app.detached).toEqual([])
  })
})

describe('reattachThread', () => {
  it('POSTs the bare codex resume payload and opens the new session', async () => {
    const fetchMock = vi.fn(async (url, opts) => {
      if (opts?.method === 'POST') return { ok: true, json: async () => ({ id: 'sess_new' }), text: async () => '' }
      return { ok: true, json: async () => ({ sessions: [{ id: 'sess_new' }], detached: [], defaults: {} }) }
    })
    vi.stubGlobal('fetch', fetchMock)

    const err = await reattachThread('codex', 'nat-1')
    expect(err).toBeNull()

    const [url, opts] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/sessions')
    expect(JSON.parse(opts.body)).toEqual({ provider: 'codex', mode: 'resume', threadId: 'nat-1' })
    expect(app.explorerOpen).toBe(false)
    expect(app.activeId).toBe('sess_new')
  })

  it('POSTs the claude resume payload with provider "claude"', async () => {
    const fetchMock = vi.fn(async (url, opts) => {
      if (opts?.method === 'POST') return { ok: true, json: async () => ({ id: 'sess_new' }), text: async () => '' }
      return { ok: true, json: async () => ({ sessions: [{ id: 'sess_new' }], detached: [], defaults: {} }) }
    })
    vi.stubGlobal('fetch', fetchMock)

    const err = await reattachThread('claude', 'nat-2')
    expect(err).toBeNull()

    const [, opts] = fetchMock.mock.calls[0]
    expect(JSON.parse(opts.body)).toEqual({ provider: 'claude', mode: 'resume', threadId: 'nat-2' })
  })

  it('on 409 thread_attached jumps to the existing session instead of erroring', async () => {
    const conflictBody = JSON.stringify({ error: { code: 'thread_attached', message: 'Thread is already attached', details: { existingSessionId: 'sess_existing' } } })
    vi.stubGlobal('fetch', vi.fn(async (url, opts) => {
      if (opts?.method === 'POST') return { ok: false, status: 409, text: async () => conflictBody }
      return { ok: true, json: async () => ({ sessions: [{ id: 'sess_existing' }], detached: [], defaults: {} }) }
    }))

    const err = await reattachThread('codex', 'nat-1')
    expect(err).toBeNull()
    expect(app.activeId).toBe('sess_existing')
    expect(app.explorerOpen).toBe(false)
  })

  it('surfaces a non-409 error to the caller', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 502, text: async () => 'daemon unreachable' })))
    const err = await reattachThread('codex', 'nat-1')
    expect(err).toBe('daemon unreachable')
  })
})

describe('dismissDetached', () => {
  it('keeps the entry and tab while closing and after a failed stop', async () => {
    app.detached = [{ provider: 'claude', nativeThreadId: 'nat-1' }]
    const tabId = openThreadTab(tabTarget({ item: { id: 'nth_1', provider: 'claude', nativeId: 'nat-1', metadata: {} } }))
    let answer
    vi.stubGlobal('fetch', vi.fn(() => new Promise((resolve) => { answer = resolve })))
    const pending = dismissDetached('claude', 'nat-1')
    expect(app.detached).toHaveLength(1)
    expect(panes.tabs[tabId]).toBeDefined()
    answer({ ok: false, status: 502, text: async () => 'Claude stop unconfirmed' })
    await expect(pending).rejects.toThrow('Claude stop unconfirmed')
    expect(app.detached).toHaveLength(1)
    expect(panes.tabs[tabId]).toBeDefined()
  })
  it('drops the entry locally and DELETEs the attachment with the provider param', async () => {
    app.detached = [{ provider: 'codex', nativeThreadId: 'nat-1', cwd: '/a' }, { provider: 'codex', nativeThreadId: 'nat-2', cwd: '/b' }]
    const fetchMock = vi.fn(async () => ({ ok: true, json: async () => ({ forgotten: true }) }))
    vi.stubGlobal('fetch', fetchMock)

    await dismissDetached('codex', 'nat-1')

    expect(app.detached.map((d) => d.nativeThreadId)).toEqual(['nat-2'])
    const [url, opts] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/attachments/nat-1?provider=codex')
    expect(opts.method).toBe('DELETE')
  })

  it('matches on provider + nativeThreadId, not nativeThreadId alone', async () => {
    // Two detached records can share a nativeThreadId across providers — a
    // contrived case, but it pins the composite identity dismiss relies on.
    app.detached = [
      { provider: 'codex', nativeThreadId: 'nat-shared', cwd: '/a' },
      { provider: 'claude', nativeThreadId: 'nat-shared', cwd: '/b' },
    ]
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => ({ forgotten: true }) })))

    await dismissDetached('claude', 'nat-shared')

    expect(app.detached).toEqual([{ provider: 'codex', nativeThreadId: 'nat-shared', cwd: '/a' }])
  })

  it('closes the thread’s open tab, if any — forgetting it should not leave a stranded read-only tab', async () => {
    const tabId = openThreadTab(tabTarget({
      item: { id: 'nth_1', provider: 'codex', nativeId: 'nat-1', title: 'Pane mechanics', metadata: {} },
    }))
    detachSessionTabs('session-1') // the thread already went detached before it was forgotten
    app.detached = [{ provider: 'codex', nativeThreadId: 'nat-1', cwd: '/a' }]
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => ({ forgotten: true }) })))

    await dismissDetached('codex', 'nat-1')

    expect(panes.tabs[tabId]).toBeUndefined()
  })
})

describe('closeSession', () => {
  it('closes the thread’s open tab, not just the hosted runtime', async () => {
    const tabId = openThreadTab(tabTarget({
      item: { id: 'nth_1', provider: 'codex', nativeId: 'nat-1', title: 'Pane mechanics', metadata: {} },
      session: { id: 'session-1', provider: 'codex', threadId: 'nat-1' },
    }))
    vi.stubGlobal('fetch', vi.fn(async (url, opts) => {
      if (opts?.method === 'DELETE') return { ok: true, json: async () => ({}) }
      return { ok: true, json: async () => ({ sessions: [], detached: [], defaults: {} }) }
    }))

    await closeSession('session-1')

    expect(panes.tabs[tabId]).toBeUndefined()
  })
})
