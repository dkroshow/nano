// Resume gating, provider payloads and navigation at the Explorer boundary.
// A conflict opens the existing attachment; other failures remain inline.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { resumeGate, resolveThreadIdentity, createResumeAction } from './thread_resume.svelte.js'
import { app } from '../app.svelte.js'
import { __resetTranscriptForTest } from '../transcript_store.svelte.js'

const RESUME_PAYLOAD = '{"provider":"codex","mode":"resume","threadId":"nat-abc"}'

const known = (value) => ({ state: 'known', value, evidence: 'session_meta' })

// The catalog row a resume travels on: native id (the target), nano id (the
// durable-history key) and the recorded metadata a claude respawn needs.
function codexRow(extra = {}) {
  return { provider: 'codex', nativeId: 'nat-abc', id: 'th_1', archived: false, kind: 'main', ...extra }
}

function claudeRow(extra = {}) {
  return {
    provider: 'claude',
    nativeId: 'nat-abc',
    id: 'th_1',
    archived: false,
    kind: 'main',
    source: { status: 'present' },
    metadata: { cwd: known('/home/user/proj') },
    ...extra,
  }
}

function grokRow(extra = {}) {
  return {
    provider: 'grok', nativeId: 'grok-native', id: 'th_grok', archived: false, kind: 'main',
    source: { status: 'present' }, metadata: { cwd: known('/home/user/proj') }, ...extra,
  }
}

beforeEach(() => {
  app.sessions = []
  app.activeId = null
  app.explorerOpen = true
  app.spawning = false
})

afterEach(() => {
  vi.unstubAllGlobals()
  __resetTranscriptForTest()
})

describe('resumeGate', () => {
  it('plain codex thread → enabled, no note', () => {
    expect(resumeGate(codexRow())).toEqual({ enabled: true, note: null })
  })

  it('archived codex thread → disabled with a visible note', () => {
    const gate = resumeGate(codexRow({ archived: true }))
    expect(gate.enabled).toBe(false)
    expect(gate.note).toMatch(/archived/)
  })

  it('subagent codex thread → disabled with a visible note', () => {
    const gate = resumeGate(codexRow({ kind: 'subagent' }))
    expect(gate.enabled).toBe(false)
    expect(gate.note).toMatch(/subagent/)
  })

  it('codex gating ignores claude-only evidence — no cwd, purged sources, still enabled', () => {
    expect(resumeGate(codexRow({ metadata: {}, source: { status: 'source_missing' } })))
      .toEqual({ enabled: true, note: null })
  })

  it('claude thread with a recorded cwd and present sources → enabled', () => {
    expect(resumeGate(claudeRow())).toEqual({ enabled: true, note: null })
  })

  it('claude subagent → no affordance at all, not a disabled button', () => {
    expect(resumeGate(claudeRow({ kind: 'subagent' }))).toBeNull()
  })

  it('archived claude thread → disabled with the archived note', () => {
    const gate = resumeGate(claudeRow({ archived: true }))
    expect(gate.enabled).toBe(false)
    expect(gate.note).toMatch(/archived/)
  })

  it('claude thread with no recorded cwd → disabled; nano may not guess the project', () => {
    const gate = resumeGate(claudeRow({ metadata: { cwd: { state: 'unknown', value: null } } }))
    expect(gate.enabled).toBe(false)
    expect(gate.note).toMatch(/working directory/)
  })

  it('claude thread with a metadata object but no cwd in it → disabled; nano may not guess the project', () => {
    const gate = resumeGate(claudeRow({ metadata: {} }))
    expect(gate.enabled).toBe(false)
    expect(gate.note).toMatch(/working directory/)
  })

  it('claude thread with no metadata at all → offered, not disabled with a claim nano cannot support', () => {
    // A row outside the Explorer's window (or a tab whose catalog row has not
    // landed) carries no metadata object. That is missing evidence, not
    // evidence of a missing cwd — the server still has the thread's attachment
    // record, and if it does not, it refuses in a sentence.
    const { metadata, ...noMetadata } = claudeRow()
    expect(resumeGate(noMetadata)).toEqual({ enabled: true, note: null })
  })

  it('claude thread whose native session file was purged → disabled with an honest note', () => {
    const gate = resumeGate(claudeRow({ source: { status: 'source_missing' } }))
    expect(gate.enabled).toBe(false)
    expect(gate.note).toMatch(/purged/)
  })

  it('grok main session with recorded cwd → enabled', () => {
    expect(resumeGate(grokRow())).toEqual({ enabled: true, note: null })
  })

  it('grok session without recorded cwd → disabled rather than guessing', () => {
    const gate = resumeGate(grokRow({ metadata: {} }))
    expect(gate.enabled).toBe(false)
    expect(gate.note).toMatch(/working directory/)
  })

  it('grok subagent → no independent resume affordance', () => {
    expect(resumeGate(grokRow({ kind: 'subagent' }))).toBeNull()
  })

  it('no record at hand → no affordance', () => {
    expect(resumeGate(null)).toBeNull()
  })

  it('an unknown provider gets no affordance', () => {
    expect(resumeGate({ provider: 'gemini', nativeId: 'x' })).toBeNull()
  })
})

// The identity a surface resumes on, once a claude `--resume` has forked the
// thread's native id out from under its catalog row.
describe('resolveThreadIdentity', () => {
  const predecessor = claudeRow({ id: 'th_1', nativeId: 'nat-abc' })
  const successor = claudeRow({ id: 'th_2', nativeId: 'nat-xyz' })

  it('an ordinary tab resolves to its own row', () => {
    const identity = resolveThreadIdentity({
      nativeId: 'nat-abc', catalogId: 'th_1', provider: 'claude', dataset: [predecessor],
    })
    expect(identity).toMatchObject({ nativeId: 'nat-abc', rowId: 'th_1', provider: 'claude' })
    expect(identity.row).toBe(predecessor)
  })

  it('after the handoff the successor row wins over the tab’s stale catalog id', () => {
    const identity = resolveThreadIdentity({
      nativeId: 'nat-xyz', catalogId: 'th_1', provider: 'claude', dataset: [predecessor, successor],
    })
    expect(identity.nativeId).toBe('nat-xyz')
    expect(identity.rowId).toBe('th_2')
    expect(identity.row).toBe(successor)
  })

  it('before the successor is indexed, the predecessor’s row lends nothing but context', () => {
    // Its metadata is still the right posture evidence (same project, same
    // posture), but its nano id addresses the predecessor's transcript — so the
    // resume travels with no nano id rather than the wrong one.
    const identity = resolveThreadIdentity({
      nativeId: 'nat-xyz', catalogId: 'th_1', provider: 'claude', dataset: [predecessor],
    })
    expect(identity.nativeId).toBe('nat-xyz')
    expect(identity.rowId).toBe('')
    expect(identity.row).toBe(predecessor)
  })

  it('a tab whose row the current window does not hold keeps the ids it knows', () => {
    expect(resolveThreadIdentity({ nativeId: 'nat-abc', catalogId: 'th_1', provider: 'claude', dataset: [] }))
      .toMatchObject({ row: null, nativeId: 'nat-abc', rowId: 'th_1', provider: 'claude' })
  })

  it('a tab with no native id yet takes the row’s', () => {
    expect(resolveThreadIdentity({ catalogId: 'th_1', dataset: [predecessor] }))
      .toMatchObject({ nativeId: 'nat-abc', rowId: 'th_1' })
  })

  it('a native id belonging to another provider is not this thread', () => {
    expect(resolveThreadIdentity({
      nativeId: 'nat-abc', catalogId: '', provider: 'codex', dataset: [predecessor],
    })).toMatchObject({ row: null, nativeId: 'nat-abc', rowId: '' })
  })
})

function stubSpawnFetch() {
  return vi.fn(async (url, opts) => {
    if (opts?.method === 'POST') return { ok: true, json: async () => ({ id: 'sess_new' }), text: async () => '' }
    return { ok: true, json: async () => ({ sessions: [{ id: 'sess_new' }], defaults: {} }) }
  })
}

describe('createResumeAction', () => {
  it('POSTs the bare frozen payload and lands on the new live session', async () => {
    const fetchMock = stubSpawnFetch()
    vi.stubGlobal('fetch', fetchMock)

    const action = createResumeAction()
    const closeModal = await action.resume(codexRow())

    const [url, opts] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/sessions')
    expect(opts.method).toBe('POST')
    expect(opts.body).toBe(RESUME_PAYLOAD) // byte-shape: bare {provider,mode,threadId}, nothing else
    expect(closeModal).toBe(true)
    expect(action.state.error).toBeNull()
    expect(app.activeId).toBe('sess_new')
    expect(app.explorerOpen).toBe(false)
  })

  it('a claude resume carries the thread’s recorded posture, not New Window defaults', async () => {
    const fetchMock = stubSpawnFetch()
    vi.stubGlobal('fetch', fetchMock)

    await createResumeAction().resume(claudeRow({
      metadata: {
        cwd: known('/home/user/proj'),
        model: known('claude-opus-4-1'),
        permission: known('bypassPermissions'),
        effort: known('high'),
      },
    }))

    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
      provider: 'claude',
      mode: 'resume',
      threadId: 'nat-abc',
      cwd: '/home/user/proj',
      model: 'claude-opus-4-1',
      permissionMode: 'bypassPermissions',
      effort: 'high',
    })
  })

  it('a grok resume carries only the recorded cwd needed by session/load', async () => {
    const fetchMock = stubSpawnFetch()
    vi.stubGlobal('fetch', fetchMock)

    await createResumeAction().resume(grokRow())

    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
      provider: 'grok', mode: 'resume', threadId: 'grok-native', cwd: '/home/user/proj',
    })
  })

  it('metadata the catalog does not know is omitted, so the CLI default applies', async () => {
    const fetchMock = stubSpawnFetch()
    vi.stubGlobal('fetch', fetchMock)

    await createResumeAction().resume(claudeRow({
      metadata: {
        cwd: known('/home/user/proj'),
        model: { state: 'unknown', value: null },
        permission: { state: 'unsupported', value: null },
      },
    }))

    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
      provider: 'claude',
      mode: 'resume',
      threadId: 'nat-abc',
      cwd: '/home/user/proj',
    })
  })

  it('409 thread_attached jumps to the existing session instead of erroring', async () => {
    const conflictBody = JSON.stringify({ error: { code: 'thread_attached', message: 'Thread is already attached', details: { existingSessionId: 'sess_existing' } } })
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 409, text: async () => conflictBody })))

    const action = createResumeAction()
    const closeModal = await action.resume(codexRow())

    expect(closeModal).toBe(true)
    expect(action.state.error).toBeNull()
    expect(app.activeId).toBe('sess_existing')
    expect(app.explorerOpen).toBe(false)
  })

  it('other errors stay inline; the view does not move and nothing is handed off', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 500, text: async () => 'daemon unreachable' })))

    const action = createResumeAction()
    const closeModal = await action.resume(codexRow())

    expect(closeModal).toBe(false)
    expect(action.state.error).toBe('daemon unreachable')
    expect(action.state.busy).toBe(false)
    expect(app.explorerOpen).toBe(true)
    expect(app.activeId).toBeNull()
  })

  it('no nativeId → no request', async () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    const action = createResumeAction()
    expect(await action.resume(codexRow({ nativeId: undefined }))).toBe(false)
    expect(fetchMock).not.toHaveBeenCalled()
  })
})
