// Resume-latency slice: what the session store must NOT wait for.
//
// Three couplings used to sit between a Resume click and a usable session view:
// the wire stream waited on /state, /state waited on a multi-second MCP
// inventory call, and the starting→ready flip waited on the 5s list poll. These
// tests pin the decoupled behavior — stream opens immediately, the tool registry
// is fetched on demand, ready comes off the wire — plus the non-destructive
// /state failure path. fetch and EventSource are stubbed.
import { describe, it, expect, vi, afterEach } from 'vitest'
import { beginMetadataRead, getSessionStore, dropSessionStore } from './state.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    this.closed = false
    MockEventSource.instances.push(this)
  }
  close() {
    this.closed = true
  }
  message(evt) {
    this.onmessage?.({ data: JSON.stringify(evt) })
  }
}
MockEventSource.instances = []

const flush = async () => {
  for (let i = 0; i < 4; i++) await new Promise((resolve) => setTimeout(resolve))
}

const headers = { get: () => null }

const codexMeta = (status = 'starting') => ({
  provider: 'codex',
  threadId: 'nat-1',
  cwd: '/w',
  status,
  thread: { model: 'gpt-x', cwd: '/w', thread: { id: 'nat-1' } },
})

const inventory = { servers: [{ name: 'docs', tools: ['fetch_doc', 'list_endpoints'] }] }

// stubFetch routes /state and /mcp; `state` may be a function of the call
// index so a test can make a later refresh fail.
function stubFetch({ state = () => ({ ok: true, headers, json: async () => codexMeta() }), mcp = inventory } = {}) {
  let stateCalls = 0
  const fetchMock = vi.fn(async (url) => {
    const u = String(url)
    if (u.endsWith('/state')) return state(stateCalls++)
    if (u.endsWith('/mcp')) return { ok: true, headers, json: async () => mcp }
    return { ok: true, headers, json: async () => ({}) }
  })
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const callsTo = (fetchMock, suffix) => fetchMock.mock.calls.filter(([u]) => String(u).endsWith(suffix)).length

// The replayed thread/resume pair: the send line teaches the store the method
// name, so the reply annotates as label "thread/resume".
const resumeSend = { seq: 1, ts: 0, dir: 'send', msg: { id: 1, method: 'thread/resume', params: { threadId: 'nat-1' } } }
const resumeReply = { seq: 2, ts: 0, dir: 'recv', msg: { id: 1, result: { thread: { id: 'nat-1' } } } }
const mcpStartup = {
  seq: 3,
  ts: 0,
  dir: 'recv',
  msg: { jsonrpc: '2.0', method: 'mcpServer/startupStatus/updated', params: { name: 'docs', status: 'ready' } },
}

const openSessions = []
function openStore(id) {
  openSessions.push(id)
  return getSessionStore(id)
}

afterEach(() => {
  while (openSessions.length) dropSessionStore(openSessions.pop())
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('ready comes off the wire, not the list poll', () => {
  it('flips to ready and refetches meta on the thread/resume reply', async () => {
    const fetchMock = stubFetch()
    const session = openStore('sm-ready')
    await flush()
    expect(session.store.sessionStatus).toBe('starting')
    const before = callsTo(fetchMock, '/state')

    session.connectFeed()
    const es = MockEventSource.instances.find((candidate) => candidate.url.endsWith('/api/sessions/sm-ready/events'))
    es.message(resumeSend)
    es.message(resumeReply)

    // Synchronous: the composer must not wait on another round trip.
    expect(session.store.sessionStatus).toBe('ready')
    await flush()
    expect(callsTo(fetchMock, '/state')).toBe(before + 1)
  })

  it('does not flip to ready on an error reply', async () => {
    stubFetch()
    const session = openStore('sm-ready-err')
    await flush()
    session.connectFeed()
    const es = MockEventSource.instances.find((candidate) => candidate.url.endsWith('/api/sessions/sm-ready-err/events'))
    es.message(resumeSend)
    es.message({ seq: 2, ts: 0, dir: 'recv', msg: { id: 1, error: { message: 'no such thread' } } })
    expect(session.store.sessionStatus).toBe('starting')
    expect(session.store.lastError).toBeNull()
  })
})

describe('/state failure is non-destructive', () => {
  it('keeps the last good meta snapshot when a refresh fails', async () => {
    const fetchMock = stubFetch({
      state: (n) => (n === 0 ? { ok: true, headers, json: async () => codexMeta() } : { ok: false, headers, text: async () => 'boom' }),
    })
    const session = openStore('sm-meta')
    await flush()
    expect(session.store.meta?.thread?.model).toBe('gpt-x')

    session.connectFeed()
    const es = MockEventSource.instances.find((candidate) => candidate.url.endsWith('/api/sessions/sm-meta/events'))
    es.message(resumeSend)
    es.message(resumeReply) // triggers the failing refresh
    await flush()

    expect(callsTo(fetchMock, '/state')).toBe(2)
    expect(session.store.meta?.thread?.model).toBe('gpt-x') // not blanked
    expect(session.store.provider).toBe('codex')
  })
})

describe('MCP tool registry is fetched on demand', () => {
  it('is not fetched on connect and is fetched once on request', async () => {
    const fetchMock = stubFetch()
    const session = openStore('sm-mcp')
    await flush()
    expect(callsTo(fetchMock, '/mcp')).toBe(0)
    expect(session.store.mcp).toBeNull()

    await session.loadMcpServers()
    expect(session.store.mcp).toEqual(inventory)
    expect(callsTo(fetchMock, '/mcp')).toBe(1)

    await session.loadMcpServers() // re-expand: served from the loaded copy
    expect(callsTo(fetchMock, '/mcp')).toBe(1)
  })

  it('surfaces a fetch failure without clobbering a loaded registry', async () => {
    const fetchMock = vi.fn(async (url) => {
      const u = String(url)
      if (u.endsWith('/state')) return { ok: true, headers, json: async () => codexMeta() }
      if (u.endsWith('/mcp')) return { ok: false, headers, text: async () => 'app-server unreachable' }
      return { ok: true, headers, json: async () => ({}) }
    })
    MockEventSource.instances = []
    vi.stubGlobal('EventSource', MockEventSource)
    vi.stubGlobal('fetch', fetchMock)

    const session = openStore('sm-mcp-err')
    await session.loadMcpServers()
    expect(session.store.mcp).toBeNull()
    expect(session.store.mcpError).toBe('app-server unreachable')
  })

  it('refetches on a startup transition only once the registry is shown', async () => {
    vi.useFakeTimers()
    const fetchMock = stubFetch()
    const session = openStore('sm-mcp-invalidate')
    session.connectFeed()
    const es = MockEventSource.instances.find((candidate) => candidate.url.endsWith('/api/sessions/sm-mcp-invalidate/events'))

    // Not loaded: a startup transition costs nothing.
    es.message(mcpStartup)
    await vi.advanceTimersByTimeAsync(1000)
    expect(callsTo(fetchMock, '/mcp')).toBe(0)

    await session.loadMcpServers()
    expect(callsTo(fetchMock, '/mcp')).toBe(1)

    // Loaded: the same transition refreshes it, debounced.
    es.message(mcpStartup)
    expect(callsTo(fetchMock, '/mcp')).toBe(1)
    await vi.advanceTimersByTimeAsync(1000)
    expect(callsTo(fetchMock, '/mcp')).toBe(2)
  })
})

// The skills list left /state; its panel reads /skills when it opens. A cold
// server cache answers an empty list at once while it fetches the real one,
// so a later open must read again rather than keep that first answer.
describe('skills list is fetched when its panel opens', () => {
  it('stays off /state and rereads after a cold first answer', async () => {
    const answers = [{ data: [] }, { data: [{ name: 'some-skill' }] }]
    const fetchMock = stubFetch()
    fetchMock.mockImplementation(async (url) => {
      const u = String(url)
      if (u.endsWith('/state')) return { ok: true, headers, json: async () => codexMeta('ready') }
      if (u.endsWith('/skills')) return { ok: true, headers, json: async () => answers.shift() }
      return { ok: true, headers, json: async () => ({}) }
    })
    const session = openStore('sm-skills')
    await flush()
    expect(callsTo(fetchMock, '/skills')).toBe(0)

    await session.loadSkills()
    expect(session.store.skills.data).toEqual([])
    await session.loadSkills()
    expect(session.store.skills.data).toEqual([{ name: 'some-skill' }])
  })
})

describe('the session list carries displayed values only', () => {
  // The list no longer carries full metadata. Catches a list read that
  // replaces a pane's full speed evidence (from /state) with its displayed
  // values, which would blank the tier control on every list read.
  it('paints posture from the list and keeps the pane\'s own full read', async () => {
    const full = {
      revision: 3,
      fields: {
        model: { displayValue: 'gpt-x' },
        serviceTier: { displayValue: 'priority', effectiveValue: 'priority', effectiveDisposition: 'confirmed', effectiveSource: 'provider' },
      },
    }
    const listed = { revision: 3, fields: { model: { displayValue: 'gpt-x' }, serviceTier: { displayValue: 'priority' } } }
    stubFetch({ state: () => ({ ok: true, headers, json: async () => ({ ...codexMeta('ready'), metadata: full }) }) })
    const summary = { id: 'listed-meta', provider: 'codex', hostedThreadId: 'nht_listed', status: 'ready', metadata: listed }
    openSessions.push(summary.id)
    const s = getSessionStore(summary.id, summary)
    expect(s.store.posture.model).toBe('gpt-x')
    expect(s.store.hostedMetadata).toBe(null)

    await flush() // a new store reads /state at once
    expect(s.store.hostedMetadata.fields.serviceTier.effectiveValue).toBe('priority')

    s.applySummary(summary, beginMetadataRead())
    expect(s.store.hostedMetadata.fields.serviceTier.effectiveValue).toBe('priority')
    expect(s.store.posture.serviceTier).toBe('priority')
  })

  // Catches an older /state failure blanking posture a newer list read
  // painted, and marking metadata unavailable with no read left to fix it.
  it('keeps a newer list read\'s posture when an older /state read fails', async () => {
    let failState
    stubFetch({ state: () => new Promise((resolve) => { failState = () => resolve({ ok: false, headers, json: async () => ({}) }) }) })
    const listed = { revision: 3, fields: { model: { displayValue: 'gpt-x' } } }
    const summary = { id: 'listed-race', provider: 'codex', hostedThreadId: 'nht_race', status: 'ready', metadata: listed }
    openSessions.push(summary.id)
    const s = getSessionStore(summary.id, summary) // its /state read starts now
    await flush()
    s.applySummary(summary, beginMetadataRead())
    failState()
    await flush()
    expect(s.store.posture.model).toBe('gpt-x')
    expect(s.store.metadataUnavailable).toBe(false)
  })
})
