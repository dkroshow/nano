// @vitest-environment jsdom
//
// What the operator sees on a file-only thread: saved rows and live text from
// the pane's own stream (file_transcript.svelte.js). The server sends a saved
// row and the removal of its live text in one message, and every connect
// starts with a fresh tail. These check the mounted rows, where a pane that
// applied a message in parts, or kept a stale page, would show it.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor, within } from '@testing-library/svelte'
import { tick } from 'svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import senderFooterFixture from './fixtures/sender-footer.json'
import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import { dropSessionStore, getSessionStore, syncSessionSummaries } from '../state.svelte.js'
import { projectionSnapshot, sendFileSnapshot } from '../test_projection.js'
import { __resetPanesForTest, openThreadTab, panes, tabTarget } from '../panes.svelte.js'
import { __resetBrowserDiagnosticsForTest, flushBrowserDiagnostics, recordBrowserDiagnostic } from '../browser_diagnostics.js'
import { startWarmTranscripts } from './warm_transcripts.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() {
    this.closed = true
  }
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

function errorResponse(status, code, message) {
  return { ok: false, status, headers: { get: () => null }, json: async () => ({ error: { code, message } }), text: async () => message }
}

function row(seq, type, text) {
  return { id: `row-${seq}`, type, category: 'messages', role: type === 'user_message' ? 'user' : 'assistant', text, projectedSeq: seq }
}

function tail(rows, live = [], page = { hasMoreBefore: false, nextBeforeCursor: null }) {
  return { type: 'tail', threadId: 'nth_1', key: 'tpk_1', rows, turns: [], page, projection: { key: 'tpk_1', state: 'ready' }, live }
}

async function stream(view = 'enhanced') {
  return waitFor(() => {
    const source = MockEventSource.instances.find((es) => !es.closed && es.url.includes('/live?') && es.url.includes(`view=${view}`))
    expect(source).toBeTruthy()
    return source
  })
}

async function send(source, message) {
  source.onmessage({ data: JSON.stringify(message) })
  await tick()
}

function mounted(container) {
  return [...container.querySelectorAll('.convo [data-transcript-row]')]
    .map((el) => `${el.dataset.transcriptSource}:${el.dataset.transcriptId}`)
}

function renderPane(provider = 'claude') {
  app.sessions = [{
    id: 'session-1', provider, threadId: 'native-1', hostedThreadId: 'nht-1', cwd: '/workspace/nano',
    status: 'ready', turnActive: true, fileTranscript: true,
  }]
  return render(ThreadPaneView, {
    tab: { id: 1, catalogId: 'nth_1', sessionId: 'session-1', provider, nativeId: 'native-1', title: 'File only' },
  })
}

function diagnostics(kind) {
  const ring = JSON.parse(sessionStorage.getItem('nano.browser-diagnostics.v1') || '{"events":[]}')
  return ring.events.filter((event) => event.kind === kind)
}

beforeEach(() => {
  sessionStorage.clear()
  __resetBrowserDiagnosticsForTest()
  __resetPanesForTest()
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', vi.fn(async () => jsonResponse({})))
  explorer.dataset = []
  localStorage.setItem('nano-view', 'clean')
})

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')
  vi.unstubAllGlobals()
})

describe('file-only pane status', () => {
  // A file-only pane's status (Working, approval cards) comes only from its
  // own stream. On the shared page feed it rode with a hidden copy of rows,
  // and when that copy stalled a stopped Codex turn read Working for minutes.
  it('takes Working and approval cards from its stream and reads no page snapshot or journal', async () => {
    const pageReads = []
    const journalReads = []
    fetch.mockImplementation(async (url) => {
      const target = String(url)
      if (target.includes('/conversation?view=')) pageReads.push(target)
      if (target.includes('/turn-delivery?')) journalReads.push(target)
      return jsonResponse({})
    })
    const session = {
      id: 'session-1', provider: 'codex', threadId: 'native-1', hostedThreadId: 'nht-1', cwd: '/workspace/nano',
      status: 'ready', turnActive: false, fileTranscript: true,
    }
    app.sessions = [session]
    const tabId = openThreadTab(tabTarget({ item: { id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'File only', metadata: {} }, session }))
    const { container } = render(ThreadPaneView, { tab: panes.tabs[tabId] })
    const source = await stream()
    const status = (revision, runtime, interactions = []) => ({ type: 'status', status: { revision, runtime: { sessionStatus: 'ready', ...runtime }, interactions } })
    await send(source, status(4, { turnActive: false, threadStatus: 'idle' }))
    await send(source, tail([row(1, 'user_message', 'Run the build')]))

    await send(source, status(5, { turnActive: true, threadStatus: 'working', turnId: 'turn_1', turnStartedAtMs: Date.now() }))
    await waitFor(() => expect(container.textContent).toContain('Working'))
    const approval = {
      id: '"req-1"', method: 'item/commandExecution/requestApproval', kind: 'commandApproval', threadId: 'native-1',
      turnId: 'turn_1', itemId: 'call_1', createdAt: '2026-10-03T00:00:00Z',
      params: { command: 'make build', cwd: '/workspace/nano', threadId: 'native-1' },
    }
    await send(source, status(6, { turnActive: true, threadStatus: 'needs_you', turnId: 'turn_1' }, [approval]))
    await waitFor(() => expect(container.querySelector('.interaction-command')?.textContent).toBe('make build'))
    await send(source, status(7, { turnActive: true, threadStatus: 'working', turnId: 'turn_1' }))
    await waitFor(() => expect(container.querySelector('.interaction-command')).toBeNull())

    await send(source, status(8, { turnActive: false, threadStatus: 'idle' }))
    await waitFor(() => expect(container.textContent).not.toContain('Working'))
    // The session summary then reports the turn's end, which once pulled a
    // page snapshot.
    syncSessionSummaries([{ ...session, lastTurnEndedAt: '2026-10-03T00:01:00Z' }])
    await tick()
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(container.textContent).not.toContain('Working')
    expect(pageReads).toEqual([])
    // Prompts reach the pane on the same stream: turn edges read nothing more.
    expect(journalReads).toEqual([])
  })
})

describe('file-only pane status ownership', () => {
  const codexSession = {
    id: 'session-1', provider: 'codex', threadId: 'native-1', hostedThreadId: 'nht-1', cwd: '/workspace/nano',
    status: 'ready', turnActive: false, fileTranscript: true,
  }
  const status = (revision, runtime, interactions = [], epoch = 'lpe_1') => ({
    type: 'status', status: { epoch, revision, runtime: { sessionStatus: 'ready', ...runtime }, interactions },
  })
  // Boundary: component fixture transport. Exact keys and nesting must match
  // this suite's file-stream fixtures, never the retired owner DTO shape.
  it('pins the projection fixture adapter to file-stream status and tail shapes', () => {
    const messages = []
    const source = { url: '/api/v1/hosted-threads/nht-1/live?view=enhanced',
      onmessage: ({ data }) => messages.push(JSON.parse(data)) }
    sendFileSnapshot(source, projectionSnapshot({ epoch: 'lpe_1', revision: 4,
      runtime: { sessionStatus: 'ready' },
      items: [
        { id: 'row-1', type: 'userMessage', text: 'Tell me about rain', done: true },
        { id: 'reply:msg_1:0', type: 'agentMessage', text: 'Rain', done: false },
      ],
    }))
    expect(messages).toEqual([
      status(4, { threadStatus: 'idle', turnActive: false }),
      { ...tail([
        { ...row(1, 'user_message', 'Tell me about rain'), createdAt: null, filterTags: null,
          synthetic: false, sidechain: false, isFinalAnswer: false, contentTruncated: false },
      ], [{ id: 'reply:msg_1:0', kind: 'reply', text: 'Rain' }], {
        startSeq: 1, endSeq: 1, latestSeq: 1, snapshotHeadSeq: 1,
        hasMoreBefore: false, startsMidTurn: false, nextBeforeCursor: null,
      }), key: 'test-file-key', projection: { key: 'test-file-key', schemaVersion: 0,
        state: 'ready', sourceStatus: 'present', lastIndexedAt: null, evidenceAvailable: false, itemCount: 1 } },
    ])
  })

  // The browser drops a stream's connection and reconnects it.
  async function reconnect(source) {
    MockEventSource.CLOSED = 2
    source.readyState = 0
    source.onerror()
    await tick()
  }
  const approval = {
    id: '"req-1"', method: 'item/commandExecution/requestApproval', kind: 'commandApproval', threadId: 'native-1',
    turnId: 'turn_1', itemId: 'call_1', createdAt: '2026-10-03T00:00:00Z',
    params: { command: 'make build', cwd: '/workspace/nano', threadId: 'native-1' },
  }
  const tab = { id: 1, catalogId: 'nth_1', sessionId: 'session-1', provider: 'codex', nativeId: 'native-1', title: 'File only' }
  async function streams(count) {
    return waitFor(() => {
      const open = MockEventSource.instances.filter((es) => !es.closed && es.url.includes('/live?'))
      expect(open).toHaveLength(count)
      return open
    })
  }

  // Two panes of one thread share its status. A slower pane's older status
  // must not undo a newer Stop the other pane already applied, and one pane
  // closing does not mark the other's stream closed.
  it('orders two panes\' statuses by revision', async () => {
    app.sessions = [codexSession]
    const first = render(ThreadPaneView, { tab })
    const second = render(ThreadPaneView, { tab: { ...tab, id: 2 } })
    const [a, b] = await streams(2)
    await send(a, status(4, { turnActive: false, threadStatus: 'idle' }))
    await send(b, status(4, { turnActive: false, threadStatus: 'idle' }))
    await send(a, status(5, { turnActive: true, threadStatus: 'working', turnId: 'turn_1' }))
    await send(b, status(6, { turnActive: true, threadStatus: 'needs_you', turnId: 'turn_1' }, [approval]))
    await send(a, status(8, { turnActive: false, threadStatus: 'idle' }))
    await send(b, status(7, { turnActive: true, threadStatus: 'needs_you', turnId: 'turn_1' }, [approval]))
    for (const pane of [first, second]) {
      expect(pane.container.textContent).not.toContain('Working')
      expect(pane.container.querySelector('.interaction-command')).toBeNull()
    }
    // A restart: one pane reconnects to the new projection, then the other
    // pane's delayed first message from the old one arrives. It must not
    // bring back the old turn.
    await reconnect(a)
    await send(a, status(2, { turnActive: false, threadStatus: 'idle' }, [], 'lpe_2'))
    await reconnect(b)
    await send(b, status(11, { turnActive: true, threadStatus: 'needs_you', turnId: 'turn_1' }, [approval]))
    for (const pane of [first, second]) {
      expect(pane.container.textContent).not.toContain('Working')
      expect(pane.container.querySelector('.interaction-command')).toBeNull()
    }
    first.unmount()
    expect(getSessionStore('session-1').store.streamConnected).toBe(true)
  })

  // While a stream owns the status, a late /state answer or registry summary
  // cannot replace the cards or session status it brought.
  it('keeps the stream\'s cards over a late /state answer', async () => {
    let answerState
    fetch.mockImplementation(async (url) => {
      if (String(url).includes('/state')) {
        await new Promise((resolve) => { answerState = resolve })
        return jsonResponse({ status: 'ready', turnActive: false, pendingInteractions: [] })
      }
      return jsonResponse({})
    })
    app.sessions = [codexSession]
    const { container } = render(ThreadPaneView, { tab })
    const [source] = await streams(1)
    await send(source, status(5, { turnActive: true, threadStatus: 'needs_you', turnId: 'turn_1' }, [approval]))
    await waitFor(() => expect(container.querySelector('.interaction-command')?.textContent).toBe('make build'))
    await waitFor(() => expect(answerState).toBeTypeOf('function'))
    answerState()
    syncSessionSummaries([{ ...codexSession, status: 'exited', error: 'connection lost' }])
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(container.querySelector('.interaction-command')?.textContent).toBe('make build')
    const store = getSessionStore('session-1').store
    expect([store.sessionStatus, store.lastError]).toEqual(['ready', ''])
  })

  // The elapsed clock runs from the turn start the stream brought, not from
  // an older summary's.
  // In-flight wifi: the pane cannot hear Nano, and Stop could not reach it.
  // It keeps the last state it heard, marked offline, until its stream comes
  // back with the server's next status.
  it('shows the last heard state as offline with no Stop while its stream is down', async () => {
    app.sessions = [{ ...codexSession, turnActive: true, threadStatus: 'working' }]
    const { container, getByRole } = render(ThreadPaneView, { tab })
    const [source] = await streams(1)
    await send(source, status(5, { turnActive: true, threadStatus: 'working', turnId: 'turn_2', turnStartedAtMs: Date.now() - 5_000 }))
    await waitFor(() => expect(container.querySelector('button.stop')).not.toBeNull())

    await reconnect(source)
    await waitFor(() => expect(container.textContent).toContain('Offline — last seen working'))
    expect(container.querySelector('button.stop')).toBeNull()

    // A view switch replaces the stream; a new stream is not news.
    await fireEvent.click(getByRole('button', { name: 'All', exact: true }))
    const [replacement] = await streams(1)
    expect(replacement).not.toBe(source)
    await tick()
    expect(container.textContent).toContain('Offline — last seen working')
    expect(container.querySelector('button.stop')).toBeNull()

    await send(replacement, status(6, { turnActive: false, threadStatus: 'idle' }))
    await waitFor(() => expect(container.textContent).not.toContain('Offline'))
    expect(container.textContent).not.toContain('Working')
  })

  // Failure caught (2026-10-19): Nano restarted behind Tailscale Serve, whose
  // 502 made the browser close the pane's stream for good; the pane stopped
  // following its thread until a reload. Boundary: the pane's own stream.
  it('opens a new stream when the browser has closed the old one', async () => {
    app.sessions = [{ ...codexSession, turnActive: true, threadStatus: 'working' }]
    const { container } = render(ThreadPaneView, { tab })
    const [source] = await streams(1)
    await send(source, status(5, { turnActive: true, threadStatus: 'working', turnId: 'turn_2', turnStartedAtMs: Date.now() - 5_000 }))

    MockEventSource.CLOSED = 2
    source.readyState = 2 // an HTTP error on reconnect
    source.onerror()
    await waitFor(() => expect(container.textContent).toContain('Offline — last seen working'))
    const replacement = await waitFor(() => {
      const open = MockEventSource.instances.filter((es) => !es.closed && es.url.includes('/live?'))
      expect(open).toHaveLength(1)
      expect(open[0]).not.toBe(source)
      return open[0]
    }, { timeout: 5_000 })

    await send(replacement, status(6, { turnActive: false, threadStatus: 'idle' }))
    await waitFor(() => expect(container.textContent).not.toContain('Offline'))
  })

  // A server error ends the stream for good (live_text.go sends it on a
  // failed read): the pane has heard its last status.
  it('shows the last heard state as offline with no Stop when its stream ends on an error', async () => {
    app.sessions = [{ ...codexSession, turnActive: true, threadStatus: 'working' }]
    const { container } = render(ThreadPaneView, { tab })
    const [source] = await streams(1)
    await send(source, status(5, { turnActive: true, threadStatus: 'working', turnId: 'turn_2', turnStartedAtMs: Date.now() - 5_000 }))
    await waitFor(() => expect(container.querySelector('button.stop')).not.toBeNull())

    await send(source, { type: 'error', error: 'transcript read failed' })
    await waitFor(() => expect(container.textContent).toContain('Offline — last seen working'))
    expect(container.querySelector('button.stop')).toBeNull()
  })

  // The pane shows the server's turn state, not its own send: while a POST
  // waits for Nano's answer the published state is still idle, and the
  // pending prompt row says what is happening.
  it('keeps the published idle state while its own send waits for Nano', async () => {
    app.sessions = [{ ...codexSession, threadStatus: 'idle' }]
    const { container } = render(ThreadPaneView, { tab })
    const [source] = await streams(1)
    await send(source, status(5, { turnActive: false, threadStatus: 'idle' }))
    let release
    fetch.mockImplementation(async (url) => {
      if (String(url).endsWith('/turn')) return new Promise((resolve) => { release = resolve })
      return jsonResponse({})
    })
    const session = getSessionStore('session-1')
    const submitting = session.sendTurn('please investigate')
    await waitFor(() => expect(release).toBeTypeOf('function'))
    await tick()
    try {
      expect(container.querySelector('.thinking-label')?.textContent ?? '').not.toBe('Working')
    } finally {
      release({ ...jsonResponse({}), status: 202 })
      await submitting
    }
    await send(source, status(6, { turnActive: true, threadStatus: 'working', turnId: 'turn_3', turnStartedAtMs: Date.now() }))
    await waitFor(() => expect(container.querySelector('.thinking-label')?.textContent).toBe('Working'))
  })

  it('times Working from the streamed turn start', async () => {
    app.sessions = [{ ...codexSession, turnActive: true, turnStartedAtMs: Date.now() - 2 * 60 * 60 * 1000 }]
    const { container } = render(ThreadPaneView, { tab })
    const [source] = await streams(1)
    await send(source, status(5, { turnActive: true, threadStatus: 'working', turnId: 'turn_2', turnStartedAtMs: Date.now() - 125_000 }))
    await waitFor(() => expect(container.textContent).toMatch(/2m \d+s/))
    expect(container.textContent).not.toMatch(/2h 0m/)
  })
})

describe('file-only pane', () => {
  // The mounted boundary catches a dropped adapter field or a wrong view
  // gate; the stream test owns the admission-time lookup itself.
  it('shows a live agent submission line only in Enhanced and removes it at saved handoff', async () => {
    const { container, getByRole } = renderPane()
    const prompt = { id: 'prompt:agent-1', kind: 'prompt', text: 'Inspect the parser.',
      actionId: 'agent-1', state: 'queued', senderSubmittedAt: '2026-08-27T15:22:14.843Z' }
    const source = await stream()
    await send(source, tail([], [prompt]))
    const line = 'Submitted Aug 27, 8:22:14 AM PDT'
    expect(container.querySelector('.item-submission-timestamp')?.textContent).toBe(line)
    expect(container.querySelector('[data-transcript-content]')?.textContent.trim()).toBe('Inspect the parser.')

    await fireEvent.click(getByRole('button', { name: 'All', exact: true }))
    const all = await stream('dev')
    await send(all, tail([], [prompt]))
    expect(container.querySelector('.item-submission-timestamp')).toBeNull()

    await fireEvent.click(getByRole('button', { name: 'Enhanced', exact: true }))
    const enhanced = await stream()
    await send(enhanced, tail([], [prompt]))
    expect(container.querySelector('.item-submission-timestamp')?.textContent).toBe(line)
    await send(enhanced, { type: 'update', key: 'tpk_1', rows: [row(1, 'user_message', 'Inspect the parser.')], turns: [], live: [] })
    expect(mounted(container)).toEqual(['durable:row-1'])
    expect(container.querySelector('.item-submission-timestamp')).toBeNull()
  })

  // The mounted boundary owns the saved decoration and view gate, while the
  // stream owns the atomic handoff. The saved prompt keeps its footer bytes.
  it('keeps exactly one Submitted line through saved footer handoff in Enhanced only', async () => {
    const { container, getByRole } = renderPane()
    const { text, submittedAt } = senderFooterFixture
    const prompt = { id: 'prompt:agent-footer', kind: 'prompt', text: 'Inspect the parser.',
      actionId: 'agent-footer', state: 'queued', senderSubmittedAt: submittedAt }
    const source = await stream()
    const line = 'Submitted Oct 7, 6:29:25 PM PDT'
    await send(source, tail([], [prompt]))
    expect([...container.querySelectorAll('.item-submission-timestamp')].map((el) => el.textContent)).toEqual([line])
    const rows = [row(1, 'user_message', text), row(2, 'user_message', 'A person asks a question.')]
    await send(source, { type: 'update', key: 'tpk_1', rows, turns: [], live: [] })
    expect(mounted(container)).toEqual(['durable:row-1', 'durable:row-2'])
    expect([...container.querySelectorAll('.item-submission-timestamp')].map((el) => el.textContent)).toEqual([line])
    expect(container.querySelector('[data-transcript-id="row-1"] [data-transcript-content]').textContent).toBe(text)
    expect(container.querySelector('[data-transcript-id="row-2"] .item-submission-timestamp')).toBeNull()

    await fireEvent.click(getByRole('button', { name: 'All', exact: true }))
    await send(await stream('dev'), tail(rows))
    expect(container.querySelector('.item-submission-timestamp')).toBeNull()
    await fireEvent.click(getByRole('button', { name: 'Enhanced', exact: true }))
    await send(await stream(), tail(rows))
    expect([...container.querySelectorAll('.item-submission-timestamp')].map((el) => el.textContent)).toEqual([line])
  })

  it('records one load after URL resolution and the file tail paint, excluding an unused catalog fetch', async () => {
    const geometry = vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function () {
      return this.classList.contains('item')
        ? { top: 100, bottom: 150, width: 500, height: 50 }
        : { top: 0, bottom: 200, width: 500, height: 200 }
    })
    try {
      app.threadReferenceResolving = true
      renderPane()
      const source = await stream()
      expect(diagnostics('thread_open')).toEqual([])
      await send(source, tail([row(1, 'agent_message', 'A visible preview')]))
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)))
      expect(diagnostics('thread_open')).toEqual([])
      app.threadReferenceResolving = false
      await waitFor(() => expect(diagnostics('thread_open')).toHaveLength(1))
      const event = diagnostics('thread_open')[0]
      expect(event).toMatchObject({ outcome: 'ready', trigger: 'load', itemCount: 1 })
      expect(event.firstRowsMs).toBeGreaterThanOrEqual(0)
      expect(event.newestRowsMs).toBeGreaterThanOrEqual(event.firstRowsMs)
      expect(event).not.toHaveProperty('historyMs')
    } finally { app.threadReferenceResolving = false; geometry.mockRestore() }
  })

  // Enhanced shows a saved message whatever its status; an interrupted or
  // failed reply keeps its error category and still replaces its live text.
  it.each([
    ['completed', 'messages'],
    ['interrupted', 'error'],
    ['failed', 'error'],
  ])('swaps a reply’s live text for its saved %s row in one render, never both and never neither', async (status, category) => {
    const { container } = renderPane()
    const source = await stream()
    await send(source, tail([row(1, 'user_message', 'Tell me about rain')], [{ id: 'reply:msg_1:0', kind: 'reply', text: 'Rain' }]))
    await send(source, { type: 'append', id: 'reply:msg_1:0', append: ' falls.' })
    expect(mounted(container)).toEqual(['durable:row-1', 'live:reply:msg_1:0'])

    const saved = { ...row(2, 'agent_message', 'Rain falls.'), status, category }
    await send(source, { type: 'update', key: 'tpk_1', rows: [saved], turns: [], live: [] })

    expect(mounted(container)).toEqual(['durable:row-1', 'durable:row-2'])
    expect(container.querySelector('[data-transcript-id="row-2"] [data-transcript-content]')?.textContent.trim()).toBe('Rain falls.')
  })

  // The validation a served browser publishes is what agents check. A reply
  // whose live text stayed beside its saved row once read as a match there,
  // because the two rows' ids differ; the message's native id joins them.
  it('reports a reply shown as both its saved row and its live text as one message twice', async () => {
    const { container } = renderPane()
    const source = await stream()
    const saved = { ...row(2, 'agent_message', 'Rain falls.'), nativeItemId: 'msg_1:0' }
    await send(source, tail([row(1, 'user_message', 'Tell me about rain'), saved], [{ id: 'reply:msg_1:0', kind: 'reply', text: 'Rain falls.', state: 'done' }]))
    expect(mounted(container)).toEqual(['durable:row-1', 'durable:row-2', 'live:reply:msg_1:0'])

    const validations = () => fetch.mock.calls
      .filter(([url, options]) => String(url) === '/api/v1/browser-validations' && options?.method === 'POST')
      .map(([, options]) => JSON.parse(options.body))
    await waitFor(() => expect(validations().at(-1)).toMatchObject({ status: 'mismatch', duplicateSemanticIdCount: 1 }))
  })

  it('keeps the pane and what it shows until a fresh tail, then replaces the rows whole', async () => {
    const { container, getByRole } = renderPane()
    const source = await stream()
    await send(source, tail([row(1, 'user_message', 'Earlier'), row(2, 'agent_message', 'Answer')]))
    const composer = container.querySelector('.composer textarea')

    // A reconnect on the same stream: the newest page no longer holds row 1.
    await send(source, tail([row(2, 'agent_message', 'Answer'), row(3, 'user_message', 'Next')]))
    expect(mounted(container)).toEqual(['durable:row-2', 'durable:row-3'])

    // A profile switch opens a new stream; the old rows stay until its tail.
    await fireEvent.click(getByRole('button', { name: 'All' }))
    const all = await stream('dev')
    expect(source.closed).toBe(true)
    expect(mounted(container)).toEqual(['durable:row-2', 'durable:row-3'])
    await send(all, tail([row(3, 'user_message', 'Next'), row(4, 'agent_message', 'Thinking aloud')]))
    expect(mounted(container)).toEqual(['durable:row-3', 'durable:row-4'])
    expect(container.querySelector('.composer textarea')).toBe(composer)

    // A new stream that fails before its tail leaves the shown rows, with its error.
    await fireEvent.click(getByRole('button', { name: 'Enhanced' }))
    const failed = await stream()
    await send(failed, { type: 'error', error: 'the saved transcript could not be read' })
    expect(mounted(container)).toEqual(['durable:row-3', 'durable:row-4'])
    expect(container.querySelector('.convo').textContent).toContain('the saved transcript could not be read')
  })

  it('ignores an older page that answers after a fresh tail', async () => {
    let answer
    vi.mocked(fetch).mockImplementation(async (url) => String(url).includes('before=c1')
      ? new Promise((resolve) => { answer = resolve })
      : jsonResponse({}))
    const { container } = renderPane()
    const source = await stream()
    await send(source, tail([row(5, 'agent_message', 'Newest')], [], { hasMoreBefore: true, nextBeforeCursor: 'c1' }))
    await fireEvent.scroll(container.querySelector('.convo'))
    await waitFor(() => expect(answer).toBeTypeOf('function'))

    await send(source, tail([row(6, 'agent_message', 'After a rebuild')]))
    answer(jsonResponse({ items: [row(1, 'user_message', 'Stale')], turns: [], page: { hasMoreBefore: false } }))
    await new Promise((resolve) => setTimeout(resolve, 0))
    await tick()

    expect(mounted(container)).toEqual(['durable:row-6'])
  })

  it('repairs by reloading the pane from Nano\'s saved transcript and nothing else, and not while an agent works', async () => {
    const { container, getByRole, queryByRole } = renderPane()
    const source = await stream()
    await send(source, tail([row(1, 'user_message', 'Before repair')]))
    expect(queryByRole('button', { name: 'Validate' })).toBeNull()
    expect(queryByRole('button', { name: 'Audit transcript' })).toBeNull()
    const session = getSessionStore('session-1')

    session.store.turnActive = true
    await fireEvent.click(getByRole('button', { name: 'Repair transcript' }))
    expect(container.textContent).toContain("Repair transcript isn't available while an agent is working.")
    expect(source.closed).toBeFalsy()

    session.store.turnActive = false
    await fireEvent.click(getByRole('button', { name: 'Repair transcript' }))
    expect(source.closed).toBe(true)
    const reloaded = await stream()
    expect(mounted(container)).toEqual(['durable:row-1'])
    await send(reloaded, tail([row(1, 'user_message', 'Before repair'), row(2, 'agent_message', 'From the file')]))
    expect(mounted(container)).toEqual(['durable:row-1', 'durable:row-2'])
    expect(vi.mocked(fetch).mock.calls.some(([url]) => String(url).includes('transcript-repair'))).toBe(false)
  })

  // The Feedback note is the operator's bug report. Save must leave Nano
  // holding the tab's whole retained ring before the note: events Nano
  // already acknowledged are sent again (its shared ring may have evicted
  // them), and a restart part way through sends the whole ring again under
  // the new boot, which the note then names. A failed upload saves nothing
  // and keeps the typed text for a retry under the same id. A busy tab's
  // events make batches larger than the 64 KiB a browser allows keepalive
  // requests, which once failed every such Save.
  it('saves a Feedback note only after Nano holds the whole diagnostic ring, and confirms it in local time', async () => {
    const posts = []
    let diagnostics = 'down'
    let feedbackAnswers = [errorResponse(503, 'feedback_unavailable', 'Feedback store unavailable')]
    vi.mocked(fetch).mockImplementation(async (url, init) => {
      if (init?.keepalive && init.body.length > 64 << 10) throw new TypeError('Failed to fetch')
      const target = String(url)
      const post = init?.method === 'POST' ? { url: target, body: JSON.parse(init.body) } : null
      if (post) posts.push(post)
      if (target === '/api/v1/browser-diagnostics') {
        if (diagnostics === 'down') return errorResponse(503, 'restarting', 'restarting')
        post.bootId = diagnostics === 'boot-2' ? 'boot-2' : 'boot-1'
        // This batch reaches the old boot; Nano restarts before the next one.
        if (diagnostics === 'restart-after-one') diagnostics = 'boot-2'
        return jsonResponse({ accepted: post.body.events.length, bootId: post.bootId })
      }
      if (target === '/api/v1/feedback') return feedbackAnswers.shift()
      return jsonResponse({})
    })
    const { container, getByRole, getByLabelText, queryByLabelText } = renderPane()
    const source = await stream()
    await send(source, { type: 'status', status: { revision: 1, runtime: { sessionStatus: 'ready', turnActive: true, threadStatus: 'working' }, interactions: [] } })
    await send(source, tail([row(1, 'user_message', 'Before feedback')]))
    const repair = getByRole('button', { name: 'Repair transcript' })
    const button = getByRole('button', { name: 'Feedback' })
    expect(button.parentElement.parentElement).toBe(repair.parentElement)

    await fireEvent.click(button)
    await fireEvent.input(getByLabelText('Feedback note'), { target: { value: 'reply vanished' } })
    await fireEvent.click(getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(container.textContent).toContain('Feedback not saved: the browser logs could not be sent to Nano'))
    expect(posts.some((post) => post.url === '/api/v1/feedback')).toBe(false)

    diagnostics = 'up'
    await flushBrowserDiagnostics()
    await fireEvent.click(getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(container.textContent).toContain('Feedback not saved: Feedback store unavailable'))
    expect(getByLabelText('Feedback note').value).toBe('reply vanished')

    // More than one batch, so a restart can fall between two of them.
    for (let index = 0; index < 150; index++) {
      recordBrowserDiagnostic('send.visibility', {
        threadId: 'native-1', sessionId: 's'.repeat(160), actionId: 'a'.repeat(160), epoch: 'e'.repeat(160),
      })
    }
    diagnostics = 'restart-after-one'
    const createdAt = '2026-10-09T01:07:51Z'
    feedbackAnswers = [{ ...jsonResponse({ id: 'fb_1', createdAt }), status: 201 }]
    const beforeLastSave = posts.length
    await fireEvent.click(getByRole('button', { name: 'Save' }))
    const savedAt = new Date(createdAt).toLocaleString([], { dateStyle: 'medium', timeStyle: 'medium' })
    await waitFor(() => expect(container.textContent).toContain(`Feedback saved at ${savedAt}`))
    expect(queryByLabelText('Feedback note')).toBeNull()

    const saves = posts.filter((post) => post.url === '/api/v1/feedback')
    expect(saves).toHaveLength(2)
    expect(saves[1].body.clientActionId).toBe(saves[0].body.clientActionId)
    expect(saves[1].body).toMatchObject({
      note: 'reply vanished', bootId: 'boot-2', hostedThreadId: 'nht-1', threadId: 'native-1', provider: 'claude', view: 'enhanced',
      pane: { working: true, pendingPrompts: 0, lastSavedRowId: 'row-1' },
      tabId: expect.any(String),
    })
    const heldByNewBoot = new Set(posts.slice(beforeLastSave)
      .filter((post) => post.url === '/api/v1/browser-diagnostics' && post.bootId === 'boot-2')
      .flatMap((post) => post.body.events.map((event) => event.eventId)))
    const retained = JSON.parse(sessionStorage.getItem('nano.browser-diagnostics.v1')).events
    expect(retained.length).toBeGreaterThan(150)
    expect(retained.filter((event) => !heldByNewBoot.has(event.eventId))).toEqual([])
  })

  it('shows a failed read and stops instead of reconnecting', async () => {
    const { container } = renderPane()
    const source = await stream()
    await send(source, tail([row(1, 'user_message', 'Before')]))

    await send(source, { type: 'error', error: 'the saved transcript could not be read' })

    expect(source.closed).toBe(true)
    expect(container.querySelector('.convo').textContent).toContain('the saved transcript could not be read')
    expect(mounted(container)).toEqual(['durable:row-1'])
  })

  it('times a prompt from its first live text to its saved row, and only live text the server names as saved', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    renderPane()
    const source = await stream()
    await send(source, tail([row(1, 'user_message', 'Earlier')], [
      { id: 'prompt:sent', kind: 'prompt', text: 'Sent from here', actionId: 'sent', state: 'accepted' },
      { id: 'prompt:cancelled', kind: 'prompt', text: 'Cancelled', actionId: 'cancelled', state: 'queued' },
    ]))
    vi.setSystemTime(Date.now() + 5000)
    // The cancelled prompt leaves beside an unrelated saved row.
    await send(source, { type: 'update', key: 'tpk_1', rows: [row(2, 'agent_message', 'Unrelated')], turns: [],
      live: [{ id: 'prompt:sent', kind: 'prompt', text: 'Sent from here', actionId: 'sent', state: 'accepted' }] })
    await send(source, { type: 'update', key: 'tpk_1', rows: [row(3, 'user_message', 'Sent from here')], turns: [],
      live: [{ id: 'reply:msg_1:0', kind: 'reply', text: 'Answer', state: 'done' }], saved: ['prompt:sent'] })
    // A replacement tail names what it saved too.
    await send(source, { ...tail([row(3, 'user_message', 'Sent from here'), row(4, 'agent_message', 'Answer')]), saved: ['reply:msg_1:0'] })

    await waitFor(() => expect(diagnostics('file.lag.prompt')).not.toEqual([]))
    const lag = diagnostics('file.lag.prompt')
    expect(lag.map((event) => event.actionId)).toEqual(['sent'])
    expect(lag[0].durationMs).toBeGreaterThanOrEqual(5000)
    await waitFor(() => expect(diagnostics('file.lag.reply')).toHaveLength(1))
  })
})

// A prompt queued by another client while the pane is hidden has no local
// state. Its streamed state owns the dot and padding.
describe('file prompt pending indicator', () => {
  it.each([
    ['claude', 'clean', 'enhanced'],
    ['claude', 'all', 'dev'],
    ['codex', 'clean', 'enhanced'],
    ['codex', 'all', 'dev'],
  ])('keeps streamed pending state when a %s %s pane returns', async (provider, view, profile) => {
    localStorage.setItem('nano-view', view)
    const pane = renderPane(provider)
    const source = await stream(profile)
    const status = (revision) => ({
      type: 'status',
      status: { epoch: 'lpe_1', revision, runtime: { sessionStatus: 'ready', turnActive: true, turnId: 'turn_1' }, interactions: [] },
    })
    const saved = [row(1, 'user_message', 'Existing saved prompt')]
    await send(source, status(4))
    await send(source, tail(saved))
    await pane.rerender({ visible: false })
    expect(source.closed).toBe(true)
    await pane.rerender({ visible: true })
    const reopened = await stream(profile)
    await send(reopened, status(5))
    const prompt = { id: 'prompt:remote-1', kind: 'prompt', text: 'Queued while hidden', actionId: 'remote-1', state: 'queued' }
    await send(reopened, tail(saved, [prompt]))

    // A prompt still on its way has a filled dot, one Nano holds behind the
    // running turn a hollow dot, and a saved one none. A prompt held for the
    // person shows its label and actions instead. Each dot is named for
    // assistive technology by its role and label.
    const assertIndicator = (state, { dot, pending }) => {
      const rows = pane.container.querySelectorAll('[data-client-action-id="remote-1"]')
      expect(rows).toHaveLength(1)
      const row = rows[0]
      expect(row.textContent).toContain('Queued while hidden')
      const name = { filled: `On its way to ${provider === 'codex' ? 'Codex' : 'Claude'}`, hollow: 'Queued' }[dot]
      if (name) {
        expect(within(row).getByRole('img', { name }).classList.contains('queued')).toBe(dot === 'hollow')
      } else {
        expect(within(row).queryByRole('img')).toBeNull()
      }
      expect(row.dataset.status).toBe(state)
      expect(row.classList.contains('pending-send')).toBe(pending)
      expect(row.querySelector('.msg-user-surface').classList.contains('pending-user')).toBe(dot !== 'none')
    }
    assertIndicator('queued', { dot: 'hollow', pending: true })

    for (const [state, dot, pending] of [['delivering', 'filled', true], ['uncertain', 'none', true], ['parked', 'none', true], ['accepted', 'filled', false], ['confirmed', 'filled', false], ['queued', 'hollow', true]]) {
      await send(reopened, { type: 'live', live: [{ ...prompt, state }] })
      assertIndicator(state, { dot, pending })
    }

    await send(reopened, { type: 'update', key: 'tpk_1', rows: [row(2, 'user_message', prompt.text)], turns: [], live: [], saved: [prompt.id] })
    expect(mounted(pane.container)).toEqual(['durable:row-1', 'durable:row-2'])
    const savedPrompt = pane.container.querySelector('[data-transcript-id="row-2"]')
    expect(savedPrompt.textContent).toContain(prompt.text)
    expect(savedPrompt.querySelector('.pending-user-dot')).toBeNull()
    expect(savedPrompt.querySelector('.pending-user')).toBeNull()
  })
})

// Selecting a thread the warm stream carries paints its saved rows at once;
// the pane's own first tail then replaces them whole, never merged. Only an
// Enhanced pane uses them, only while the thread is in the warm set, and only
// once: the server stops reading a thread on screen, so a later open waits
// for the fresh tail the server sends when the thread leaves the screen.
describe('warm seed', () => {
  // The entry holds the window from the server's first row on, and stays
  // after a pane seeds from it, so the next open of the thread is instant too.
  it('paints warm rows at once and replaces them whole with the pane\'s own tail', async () => {
    startWarmTranscripts(() => [])
    const warm = MockEventSource.instances.find((es) => es.url.startsWith('/api/v1/warm-transcripts'))
    const rows = Array.from({ length: 25 }, (_, i) => row(i + 1, i % 2 ? 'agent_message' : 'user_message', `Message ${i + 1}`))
    await send(warm, { type: 'set', refs: ['nht-1'] })
    await send(warm, { ...tail(rows), ref: 'nht-1', page: { startSeq: 7 } })
    await send(warm, { type: 'update', ref: 'nht-1', key: 'tpk_1', rows: [row(26, 'agent_message', 'Message 26')], turns: [] })
    const newest = Array.from({ length: 20 }, (_, i) => `durable:row-${i + 7}`)
    const scroller = (pane) => pane.container.querySelector('[data-thread-open-state]').dataset

    let pane = renderPane()
    expect(mounted(pane.container)).toEqual(newest)
    expect(scroller(pane)).toMatchObject({ threadOpenState: 'loading', threadOpenWarm: 'true' })
    const source = await stream()
    await send(source, tail([row(25, 'user_message', 'Message 25'), row(27, 'user_message', 'Message 27')]))
    expect(mounted(pane.container)).toEqual(['durable:row-25', 'durable:row-27'])
    expect(scroller(pane).threadOpenState).toBe('ready')
    cleanup()

    pane = renderPane()
    expect(mounted(pane.container)).toEqual(newest) // the entry stays for the next open
    cleanup()

    await send(warm, { ...tail(rows), ref: 'nht-1' })
    localStorage.setItem('nano-view', 'all')
    pane = renderPane()
    expect(mounted(pane.container)).toEqual([])
    expect(scroller(pane).threadOpenWarm).toBeUndefined()
    cleanup()

    localStorage.setItem('nano-view', 'clean')
    await send(warm, { ...tail(rows), ref: 'nht-1' })
    await send(warm, { type: 'set', refs: [] })
    pane = renderPane()
    expect(mounted(pane.container)).toEqual([])
  })
})
