// Prompt-loss hardening, Unit A: a submitted prompt must never silently die.
//
// sendTurn/submitTurn report a structured outcome ('sent' | 'queued' |
// 'failed' | 'uncertain'), failures land on store.sendError (not lastError, which any
// background fetch overwrites), and the wireTurnSeen latch releases turn-state
// authority back to the server snapshots when a send fails — a thrown fetch
// used to hold the optimistic turnActive lock forever, wedging every later
// prompt into an in-memory queue that died with the page.
import { describe, it, expect, vi, afterEach } from 'vitest'
import {
  getSessionStore,
  dropSessionStore,
  setMissingSessionRecovery,
} from './state.svelte.js'

// The kinds this file's sends recorded, so a test can tell which path a send
// took the way a Feedback ring would.
const diagnosticKinds = vi.hoisted(() => [])
vi.mock('./browser_diagnostics.js', async (importOriginal) => {
  const actual = await importOriginal()
  return {
    ...actual,
    recordBrowserDiagnostic: (kind, fields) => {
      diagnosticKinds.push(kind)
      return actual.recordBrowserDiagnostic(kind, fields)
    },
  }
})

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() {}
  message(evt) {
    this.onmessage?.({ data: JSON.stringify(evt) })
  }
}
MockEventSource.instances = []

const headers = { get: () => null }
const claudeMeta = { provider: 'claude', threadId: 'nat-send', cwd: '/w', status: 'ready' }

// stubFetch routes /state and lets each test decide what POST /turn does.
function stubFetch(turn) {
  const fetchMock = vi.fn(async (url, options) => {
    const u = String(url)
    if (u.endsWith('/state')) return { ok: true, headers, json: async () => claudeMeta }
    if (u.endsWith('/turn')) return turn(options, u)
    return { ok: true, headers, json: async () => ({}) }
  })
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const openSessions = []
function openStore(id) {
  openSessions.push(id)
  const session = getSessionStore(id)
  // Provider must be known before /state resolves — the summary poll supplies
  // it in the app; tests do the same.
  session.applySummary({ status: 'ready', provider: 'claude' })
  return session
}

afterEach(() => {
  diagnosticKinds.length = 0
  while (openSessions.length) dropSessionStore(openSessions.pop())
  setMissingSessionRecovery(null)
  vi.unstubAllGlobals()
  vi.useRealTimers()
  localStorage.clear()
})

describe('send outcomes', () => {
  it('returns sent on a 2xx and clears any earlier send error', async () => {
    stubFetch(() => ({ ok: true, headers, text: async () => '' }))
    const session = openStore('send-ok')
    session.store.sendError = { reason: 'stale', fromQueue: false }

    await expect(session.sendTurn('hello')).resolves.toBe('sent')
    expect(session.store.sendError).toBeNull()
    expect(session.store.turnActive).toBe(true) // optimistic lock until the result line
  })

  it('returns failed on a non-2xx, surfaces sendError, and unwinds the optimistic lock', async () => {
    stubFetch(() => ({ ok: false, status: 404, headers, text: async () => JSON.stringify({ error: { code: 'not_found', message: 'no such session' } }) }))
    const session = openStore('send-404')

    await expect(session.sendTurn('hello')).resolves.toBe('failed')
    expect(session.store.sendError).toEqual({ reason: 'no such session', fromQueue: false })
    expect(session.store.turnActive).toBe(false)
  })

  it('refreshes a rotated session id and retries the same action once', async () => {
    const turn = vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 404, headers, text: async () => JSON.stringify({ error: { code: 'not_found', message: 'no such session' } }) })
      .mockResolvedValueOnce({ ok: true, status: 200, headers, json: async () => ({ accepted: true }) })
    stubFetch(turn)
    const recover = vi.fn(async () => ({
      id: 'send-successor', provider: 'claude', threadId: 'nat-send', status: 'ready',
    }))
    setMissingSessionRecovery(recover)
    const session = openStore('send-stale')
    session.applySummary({ status: 'ready', provider: 'claude', threadId: 'nat-send' })

    await expect(session.sendTurn('survive the restart')).resolves.toBe('sent')

    expect(recover).toHaveBeenCalledWith({
      sessionId: 'send-stale', provider: 'claude', threadId: 'nat-send',
    })
    expect(turn).toHaveBeenCalledTimes(2)
    expect(turn.mock.calls[0][1]).toBe('/api/sessions/send-stale/turn')
    expect(turn.mock.calls[1][1]).toBe('/api/sessions/send-successor/turn')
    const first = JSON.parse(turn.mock.calls[0][0].body)
    const second = JSON.parse(turn.mock.calls[1][0].body)
    expect(second.clientActionId).toBe(first.clientActionId)
    expect(session.store.sendError).toBeNull()
  })

  it('retries once through a restart-window 502 against the same session id', async () => {
    const turn = vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 502, headers, text: async () => '' })
      .mockResolvedValueOnce({ ok: true, status: 200, headers, json: async () => ({ accepted: true }) })
    stubFetch(turn)
    // The registry still holds the id after the refresh: the 502 was the dev
    // proxy answering for a Nano that came right back — same session, retry it.
    const recover = vi.fn(async () => ({
      id: 'send-blip', provider: 'claude', threadId: 'nat-send', status: 'ready',
    }))
    setMissingSessionRecovery(recover)
    const session = openStore('send-blip')
    session.applySummary({ status: 'ready', provider: 'claude', threadId: 'nat-send' })

    await expect(session.sendTurn('survive the blip')).resolves.toBe('sent')

    expect(turn).toHaveBeenCalledTimes(2)
    expect(turn.mock.calls[0][1]).toBe('/api/sessions/send-blip/turn')
    expect(turn.mock.calls[1][1]).toBe('/api/sessions/send-blip/turn')
    const first = JSON.parse(turn.mock.calls[0][0].body)
    const second = JSON.parse(turn.mock.calls[1][0].body)
    expect(second.clientActionId).toBe(first.clientActionId)
    expect(session.store.sendError).toBeNull()
  })

  it('retries a restart-window 502 against the rotated successor id', async () => {
    const turn = vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 502, headers, text: async () => '' })
      .mockResolvedValueOnce({ ok: true, status: 200, headers, json: async () => ({ accepted: true }) })
    stubFetch(turn)
    const recover = vi.fn(async () => ({
      id: 'send-successor', provider: 'claude', threadId: 'nat-send', status: 'ready',
    }))
    setMissingSessionRecovery(recover)
    const session = openStore('send-rotated')
    session.applySummary({ status: 'ready', provider: 'claude', threadId: 'nat-send' })

    await expect(session.sendTurn('survive the restart')).resolves.toBe('sent')

    expect(turn).toHaveBeenCalledTimes(2)
    expect(turn.mock.calls[1][1]).toBe('/api/sessions/send-successor/turn')
  })

  // A gateway (Tailscale Serve, the dev proxy) answers 502/503/504 for a Nano
  // it cannot reach, as during a deploy: that is no answer from Nano, so the
  // send is kept and retried under its own action id, never handed back as
  // refused. Boundary: the send path's reading of a failed POST's body.
  // JSON that is not Nano's {error: {code, message}} envelope is a front's too.
  for (const [status, body] of [[502, ''], [503, '<html><body>Service Unavailable</body></html>'], [504, 'Gateway Timeout'],
    [503, '{"message":"upstream unavailable"}'], [502, '[]']]) {
    it(`keeps a send a gateway answered ${status} ${body ? body.slice(0, 12) : '(empty)'} for, to retry under the same id`, async () => {
      const turn = vi.fn().mockResolvedValue({ ok: false, status, headers, text: async () => body })
      stubFetch(turn)
      setMissingSessionRecovery(vi.fn(async () => null)) // the registry read failed too
      const session = openStore('send-gateway')
      session.applySummary({ status: 'ready', provider: 'claude', threadId: 'nat-send' })

      await expect(session.sendTurn('through the deploy')).resolves.toBe('uncertain')

      expect(session.store.unsent.map((send) => [send.text, send.retry])).toEqual([['through the deploy', true]])
      expect(session.store.restoreDraft).toBeNull()
      expect(diagnosticKinds).toContain('send.post.gateway')
      expect(diagnosticKinds).not.toContain('send.post.refused')
    })
  }

  // The same statuses carrying Nano's own JSON error are Nano's answer: a
  // refusal, with the text handed back.
  it('treats a 502 carrying Nano\'s own JSON error as Nano\'s refusal', async () => {
    const turn = vi.fn().mockResolvedValue({
      ok: false, status: 502, headers,
      text: async () => JSON.stringify({ error: { code: 'provider_unavailable', message: 'Claude is unavailable' } }),
    })
    stubFetch(turn)
    setMissingSessionRecovery(vi.fn(async () => ({ id: 'send-nano-502', provider: 'claude', threadId: 'nat-send', status: 'ready' })))
    const session = openStore('send-nano-502')
    session.applySummary({ status: 'ready', provider: 'claude', threadId: 'nat-send' })

    await expect(session.sendTurn('refused by Nano')).resolves.toBe('failed')

    expect(turn).toHaveBeenCalledTimes(1)
    expect(session.store.sendError).toEqual({ reason: 'Claude is unavailable', fromQueue: false })
    expect(session.store.unsent).toEqual([])
    expect(session.store.restoreDraft.text).toBe('refused by Nano')
    expect(diagnosticKinds).toContain('send.post.refused')
    expect(diagnosticKinds).not.toContain('send.post.gateway')
  })

  // Catches a send that meets a stopping Nano, retries at once into the closed
  // port and ends as an "uncertain" prompt nobody recorded. Boundary: the
  // send path's handling of Nano's own 503 restarting answer.
  it('treats a 503 restarting answer as a definite refusal with no instant retry', async () => {
    const message = 'Nano is restarting. Nothing was recorded; retry in a few seconds.'
    const turn = vi.fn().mockResolvedValue({
      ok: false, status: 503, headers,
      text: async () => JSON.stringify({ error: { code: 'restarting', message } }),
    })
    stubFetch(turn)
    const recover = vi.fn(async () => ({ id: 'send-stop', provider: 'claude', threadId: 'nat-send', status: 'ready' }))
    setMissingSessionRecovery(recover)
    const session = openStore('send-stop')
    session.applySummary({ status: 'ready', provider: 'claude', threadId: 'nat-send' })

    await expect(session.sendTurn('during the restart')).resolves.toBe('failed')

    expect(turn).toHaveBeenCalledTimes(1)
    expect(recover).not.toHaveBeenCalled()
    expect(session.store.sendError).toEqual({ reason: message, fromQueue: false })
    expect(session.store.unsent).toEqual([])
  })

  it('posts once while a turn is active and leaves the queueing to Nano', async () => {
    const fetchMock = stubFetch(() => ({
      ok: true, status: 202, headers,
      json: async () => ({ pending: true, status: 'queued', reason: 'queued_for_next_turn' }),
    }))
    const session = openStore('send-queue')
    session.applySummary({ status: 'ready', turnActive: true })

    await expect(session.sendTurn('next thing')).resolves.toBe('sent')
    expect(fetchMock.mock.calls.filter(([u]) => String(u).endsWith('/turn'))).toHaveLength(1)
  })

  // Holding a message for the next turn is part of its request: Nano hashes
  // it into the action's identity, so a Resend that dropped it would be
  // refused as a different message, and one that changed it would steer.
  it('keeps the next-turn choice on the first post and on a Resend of the same action', async () => {
    const turn = vi.fn()
      .mockRejectedValueOnce(new Error('connection reset'))
      .mockReturnValue(new Promise(() => {}))
    stubFetch(turn)
    const session = openStore('send-next-turn')
    session.applySummary({ status: 'ready', turnActive: true, runningTurnInput: true })
    expect(session.store.runningTurnInput).toBe(true)

    await expect(session.sendTurn('after this turn', { nextTurn: true })).resolves.toBe('uncertain')
    const actionID = session.store.unsent[0].clientActionId
    void session.resendPendingSend(actionID)

    const bodies = turn.mock.calls.map(([options]) => JSON.parse(options.body))
    expect(bodies).toEqual([
      { text: 'after this turn', clientActionId: actionID, nextTurn: true },
      { text: 'after this turn', clientActionId: actionID, nextTurn: true },
    ])
  })

  // The composer drops its saved draft at Send. If the operation record then
  // cannot be saved (storage full), a reload would lose the text.
  it('refuses a send before posting when the browser cannot save it', async () => {
    const fetchMock = stubFetch(() => ({ ok: true, headers, text: async () => '' }))
    const setItem = localStorage.setItem.bind(localStorage)
    vi.spyOn(localStorage, 'setItem').mockImplementation((key, value) => {
      if (String(key).startsWith('nano-unsent:')) throw new Error('QuotaExceededError')
      return setItem(key, value)
    })
    const session = openStore('send-storage-full')

    await expect(session.sendTurn('too big to keep')).resolves.toBe('failed')
    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/turn'))).toEqual([])
    expect(session.store.unsent).toEqual([])
    vi.restoreAllMocks()
  })

  // Another tab's Discard removed the record; this tab's Resend then posted
  // with no stored copy, so a failed resend was lost on reload.
  // A send's stored record is shared by every tab. Gone, it means another
  // tab sent or discarded it: this tab's Resend posts nothing and the row goes.
  it('posts nothing on Resend once another tab has ended the stored send', async () => {
    const turn = vi.fn()
      .mockRejectedValueOnce(new Error('connection reset'))
      .mockReturnValueOnce(new Promise(() => {}))
    stubFetch(turn)
    const session = openStore('send-resend-restore')
    await expect(session.sendTurn('resend me')).resolves.toBe('uncertain')
    const actionID = session.store.unsent[0].clientActionId
    localStorage.removeItem(`nano-unsent:${actionID}`)

    void session.resendPendingSend(actionID)

    expect(turn).toHaveBeenCalledTimes(1)
    expect(session.store.unsent).toEqual([])
  })

  // Two Resends of one send overlapped, and two refusals each returned the
  // same text to the composer.
  it('lets one Resend of an unsent send post at a time', async () => {
    const turn = vi.fn()
      .mockRejectedValueOnce(new Error('connection reset'))
      .mockReturnValue(new Promise(() => {}))
    stubFetch(turn)
    const session = openStore('send-double-resend')
    await expect(session.sendTurn('twice clicked')).resolves.toBe('uncertain')
    const actionID = session.store.unsent[0].clientActionId

    void session.resendPendingSend(actionID)
    void session.resendPendingSend(actionID)

    expect(turn).toHaveBeenCalledTimes(2)
  })

  // A send made before the native thread id landed was stored under its
  // session id; after a reload under a rotated session id nothing found it.
  it('finds a pre-identity unsent send after identity lands and the session rotates', async () => {
    const fetchMock = stubFetch(() => Promise.reject(new Error('connection reset')))
    const base = fetchMock.getMockImplementation()
    fetchMock.mockImplementation(async (url, options) => String(url).endsWith('/state')
      ? { ok: true, headers, json: async () => ({ provider: 'claude', cwd: '/w', status: 'ready' }) }
      : base(url, options))
    const first = getSessionStore('send-pre-identity')
    openSessions.push('send-pre-identity')
    first.applySummary({ status: 'ready', provider: 'claude' })
    await expect(first.sendTurn('before identity')).resolves.toBe('uncertain')
    first.applySummary({ status: 'ready', provider: 'claude', threadId: 'nat-late' })
    dropSessionStore(openSessions.pop())

    const successor = openStore('send-rotated')
    successor.applySummary({ status: 'ready', provider: 'claude', threadId: 'nat-late' })

    expect(successor.store.unsent).toEqual([
      expect.objectContaining({ text: 'before identity', threadId: 'nat-late' }),
    ])
  })

  it('drops legacy browser pending keys instead of restoring them', () => {
    localStorage.setItem('nano-pending:claude:nat-send', JSON.stringify([{
      clientActionId: 'restored-action', provider: 'claude', threadId: 'nat-send',
      sessionId: 'old-session', text: 'already delivered', normalizedText: 'already delivered',
      status: 'sending', createdAt: Date.now(), updatedAt: Date.now(),
    }]))
    stubFetch(() => ({ ok: true, headers, text: async () => '' }))
    const session = openStore('send-restored')
    session.applySummary({ status: 'ready', provider: 'claude', threadId: 'nat-send' })

    expect(session.store.unsent).toEqual([])
  })

})

describe('wireTurnSeen release on failure', () => {
  it('lets the next summary clear the phantom lock after a thrown fetch', async () => {
    stubFetch(() => Promise.reject(new Error('connection refused')))
    const session = openStore('send-thrown')

    await expect(session.sendTurn('hello')).resolves.toBe('uncertain')
    // The lock is kept (the server may have accepted the request)...
    expect(session.store.turnActive).toBe(true)
    expect(session.store.sendError?.reason).toContain('connection refused')
    expect(session.store.unsent).toHaveLength(1)
    // ...but server truth reseeds it on the very next snapshot.
    session.applySummary({ status: 'ready', turnActive: false })
    expect(session.store.turnActive).toBe(false)
  })

  it('mints a new action id for a deliberate resend after an ambiguous send', async () => {
    const turn = vi.fn()
      .mockRejectedValueOnce(new Error('connection reset'))
      .mockResolvedValueOnce({ ok: true, status: 200, headers, json: async () => ({ accepted: true }) })
    stubFetch(turn)
    const session = openStore('send-retry')

    await expect(session.sendTurn('same operation')).resolves.toBe('uncertain')
    session.applySummary({ status: 'ready', turnActive: false })
    // It waits behind the first, which has no answer yet, under its own id.
    await expect(session.sendTurn('same operation')).resolves.toBe('held')

    const ids = session.store.unsent.map((send) => send.clientActionId)
    expect(ids).toHaveLength(2)
    expect(ids[0]).toBeTruthy()
    expect(ids[1]).not.toBe(ids[0])
  })

  it('keeps wire authority after a non-2xx when the wire had already spoken', async () => {
    stubFetch(() => ({ ok: false, status: 500, headers, text: async () => 'boom' }))
    const session = openStore('send-wire-authority')
    // The wire decides turn state (a completed turn)...
    session.applyStreamStatus({}, {
      epoch: 'status', revision: 1,
      runtime: { turnActive: false }, interactions: [],
    }, true)
    expect(session.store.turnActive).toBe(false)

    await expect(session.sendTurn('hello')).resolves.toBe('failed')
    // ...so a stale snapshot claiming an active turn still cannot overwrite it.
    session.applySummary({ status: 'ready', turnActive: true })
    expect(session.store.turnActive).toBe(false)
  })
})

describe('server-owned queue', () => {
  it('does not post a queued prompt a second time when the blocking turn ends', async () => {
    const fetchMock = stubFetch(() => ({
      ok: true, status: 202, headers,
      json: async () => ({ pending: true, status: 'queued', reason: 'queued_for_next_turn' }),
    }))
    const session = openStore('server-drain')
    session.applySummary({ status: 'ready', turnActive: true })
    await expect(session.sendTurn('parked message')).resolves.toBe('sent')

    session.applyStreamStatus({}, {
      epoch: 'status', revision: 1,
      runtime: { turnActive: false }, interactions: [],
    }, true)
    await Promise.resolve()
    expect(fetchMock.mock.calls.filter(([u]) => String(u).endsWith('/turn'))).toHaveLength(1)
  })

  it('drains an adopted queue when the session transitions to ready with no turn running', async () => {
    localStorage.setItem('nano-queue:session:persist-drain', JSON.stringify(['committed words']))
    const sent = []
    stubFetch((options) => {
      sent.push(JSON.parse(options.body).text)
      return { ok: true, headers, text: async () => '' }
    })
    openStore('persist-drain')
    await vi.waitFor(() => expect(sent).toEqual(['committed words']))
	await vi.waitFor(() => expect(localStorage.getItem('nano-queue:session:persist-drain')).toBeNull())
  })
})
