// @vitest-environment jsdom
//
// What a pane renders for a thread, which is where the resume flow actually
// failed: a tab bound to a live session kept rendering read-only history, so
// there was no composer to type into. The bug was invisible to logic tests —
// the state was right, the wrong subtree was mounted. Pairs with the
// adoptSessionTabs cases in panes.test.js, which cover binding the session;
// these cover what the operator then sees.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import PanesContainer from './PanesContainer.svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import senderFooterFixture from './fixtures/sender-footer.json'
import { app, refreshSessions } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import {
  dropSessionStore,
  getSessionStore,
  syncSessionSummaries,
} from '../state.svelte.js'
import { __resetPanesForTest, openThreadTab, panes, tabTarget } from '../panes.svelte.js'
import { projectionItem, projectionSnapshot, sendProjection, sendProjectionSnapshot } from '../test_projection.js'
import { __resetDescendantCloseForTest, cancelDescendantClose, descendantClose } from './descendant_close.svelte.js'
import { viewport } from './viewport.svelte.js'
import { dispatchTranscriptAuditRequest } from '../transcript_audit_events.js'
import { browserDiagnosticIdentity } from '../browser_diagnostics.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() { this.closed = true }
}
MockEventSource.instances = []

const emptyTranscript = {
  items: [],
  turns: [],
  page: { hasMoreBefore: false, beforeCursor: null },
  projection: { state: 'ready', epoch: 'tpe_1' },
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

function errorResponse(status, code, message) {
  return {
    ok: false,
    status,
    headers: { get: () => null },
    json: async () => ({ error: { code, message } }),
    text: async () => message,
  }
}

function catalogItem(overrides = {}) {
  return {
    id: 'nth_1',
    provider: 'codex',
    nativeId: 'native-1',
    title: 'Pane mechanics',
    metadata: {},
    ...overrides,
  }
}

// A claude row carries the two pieces of evidence its gate reads: the project
// it ran in, and whether its native session file is still there.
function claudeItem(overrides = {}) {
  return catalogItem({
    provider: 'claude',
    metadata: { cwd: { state: 'known', value: '/workspace/nano', evidence: 'session_meta' } },
    source: { status: 'present' },
    ...overrides,
  })
}

// The child a `--resume` spawns: same conversation, new session, new native id.
function claudeSuccessor(overrides = {}) {
  return {
    id: 'session-2',
    provider: 'claude',
    threadId: 'native-2',
    cwd: '/workspace/nano',
    model: 'claude-fable-5',
    status: 'ready',
    turnActive: false,
    ...overrides,
  }
}

function historicalTab(overrides = {}) {
  return {
    id: 1,
    catalogId: 'nth_1',
    sessionId: '',
    provider: 'codex',
    nativeId: 'native-1',
    title: 'Pane mechanics',
    ...overrides,
  }
}

function liveSession(overrides = {}) {
  return {
    id: 'session-1',
    provider: 'codex',
    threadId: 'native-1',
    cwd: '/workspace/nano',
    model: 'gpt-5.6-sol',
    status: 'ready',
    turnActive: false,
    ...overrides,
  }
}

// The browser receives typed projection updates. These helpers keep the test
// descriptions compact while making the provider-neutral boundary explicit.
function wireEvent(_seq, method, params) {
  return { kind: 'runtime', method, params }
}

function wireItemEvent(_seq, item, method = 'item/completed') {
  return { kind: 'item', item, method }
}

function wireDurableConfirm(itemID, durableItemID, projectedSeq) {
  return { kind: 'confirm', itemID, durableItemID, projectedSeq }
}

// Only what the transcript kept. The ephemeral thinking surface renders inside
// .convo as well, so reading the scroller wholesale would count it as record.
function transcriptText(container) {
  return [...container.querySelectorAll('.convo .item')].map((el) => el.textContent).join('\n')
}

function transcriptAuditFetch(reference, postedResults, { auditId = 'tau-component', catalogThreadId = 'nth_1', submittedBodies = [], history = emptyTranscript } = {}) {
  return vi.fn(async (url, options = {}) => {
    const target = String(url)
    if (target.includes('/transcript') && !target.includes('/transcript-audits')) return jsonResponse(history)
    if (target === '/api/v1/transcript-audits' && options.method === 'POST') {
      const request = JSON.parse(options.body)
      return jsonResponse({
        auditId, clientActionId: request.clientActionId, threadId: request.threadRef, catalogThreadId,
        tabId: request.tabId, pageId: request.pageId, paneId: request.paneId, scopes: request.scopes, status: 'pending',
      })
    }
    if (target.includes(`/transcript-audits/${auditId}/reference`)) return jsonResponse(reference)
    if (target.endsWith(`/transcript-audits/${auditId}/result`) && options.method === 'POST') {
      const submitted = JSON.parse(options.body)
      submittedBodies.push(submitted)
      postedResults.push(submitted.result)
      return jsonResponse({ status: submitted.result.status, result: submitted.result })
    }
    return jsonResponse({})
  })
}

function fileStream() {
  return MockEventSource.instances.filter((source) => !source.closed && source.url.includes('/live?')).at(-1)
}

function sendHistory(history) {
  const source = fileStream()
  source.__fileProjection = history.projection
  sendProjectionSnapshot(source, { items: history.items.map((item) => ({
    ...item, type: item.type === 'agent_message' ? 'agentMessage' : item.type === 'user_message' ? 'userMessage' : item.type,
    done: true, item: item.detail ?? {},
  })) })
  source.onmessage({ data: JSON.stringify({
    type: 'tail', threadId: 'nth_1', rows: history.items, turns: history.turns ?? [], live: [],
    page: history.page, projection: history.projection,
  }) })
}

async function requestAudit({ threadRef = 'nth_1' } = {}) {
  const browser = browserDiagnosticIdentity()
  const response = await fetch('/api/v1/transcript-audits', {
    method: 'POST', body: JSON.stringify({ clientActionId: 'audit-component', threadRef,
      tabId: browser.tabId, pageId: browser.pageId, paneId: '1', scopes: ['messages'] }),
  })
  dispatchTranscriptAuditRequest(await response.json())
}

function replayToSession(events) {
  const stream = fileStream()
  for (const event of events) {
    if (event.kind === 'confirm') {
      if (stream) {
        const item = stream.__projectionState?.items.find((item) => item.id === event.itemID)
        if (item) sendProjection(stream, { item: { ...item, durableItemID: event.durableItemID, projectedSeq: event.projectedSeq } })
      }
      continue
    }
    if (event.kind === 'item') {
      const raw = event.item
      const text = raw.text ?? (
        raw.type === 'reasoning'
          ? (raw.summary ?? []).map((part) => typeof part === 'string' ? part : part.text ?? '').join('')
          : raw.type === 'userMessage'
            ? (raw.content ?? []).map((part) => part.text ?? '').join('')
            : ''
      )
      const item = projectionItem({
          id: raw.id,
          type: raw.type,
          text,
          done: event.method !== 'item/started',
          durableItemID: raw.durableItemID,
          projectedSeq: raw.projectedSeq,
          liveSeq: raw.liveSeq,
          providerConfirmed: raw.providerConfirmed,
          clientActionId: raw.clientActionId,
          status: raw.status,
          item: raw,
        })
      if (stream) sendProjection(stream, {
        op: event.method === 'item/started' ? 'item.upsert' : 'item.complete',
        item,
      })
      continue
    }
    const active = event.method === 'turn/started'
    if (stream) sendProjection(stream, {
      op: 'turn.upsert',
      runtime: { turnActive: active, threadStatus: active ? 'working' : 'idle', turnStartedAtMs: active ? Date.now() : 0 },
      turn: { id: 'turn-1', status: active ? 'in_progress' : 'completed' },
    })
  }
}

async function waitForDetailStream() {
  await waitFor(() => expect(
    MockEventSource.instances.some((es) => es.url.includes('/live?')),
  ).toBe(true))
}

beforeEach(() => {
  __resetPanesForTest()
  vi.stubGlobal('EventSource', MockEventSource)
  MockEventSource.instances = []
  // Every network call a pane makes on mount is a read: durable history, the
  // detail prefetch, /state. An empty-but-valid body keeps them all quiet.
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url) => {
      if (String(url).includes('/transcript')) return jsonResponse(emptyTranscript)
      return jsonResponse({})
    }),
  )
  app.sessions = []
  app.detached = []
  explorer.dataset = [catalogItem()]
  __resetDescendantCloseForTest()
  // Conversation reads the persisted transcript view once at init, and the
  // toggle writes it back — pin the default so one test's click cannot decide
  // what the next one renders.
  localStorage.setItem('nano-view', 'clean')
})

afterEach(() => {
  cleanup()
  viewport.mobile = false
  cancelDescendantClose()
  __resetDescendantCloseForTest()
  dropSessionStore('session-1')
  dropSessionStore('session-2')
  vi.unstubAllGlobals()
})

describe('thread pane view', () => {
  it('shows read-only history, and no composer, for a thread with no session', () => {
    const { container } = render(ThreadPaneView, { tab: historicalTab() })

    expect(container.querySelector('.pane-readonly')?.textContent).toContain('Read only')
    expect(container.querySelector('.composer textarea')).toBeNull()
  })

  // The read-only mounted pane owns this decoration; it always uses Enhanced.
  it('shows the saved sender footer submission line in a read-only pane', async () => {
    fetch.mockImplementation(async (url) => String(url).includes('/transcript')
      ? jsonResponse({ ...emptyTranscript, items: [
        { id: 'agent-prompt', type: 'user_message', role: 'user', category: 'messages', text: senderFooterFixture.text },
        { id: 'human-prompt', type: 'user_message', role: 'user', category: 'messages', text: 'A person asks a question.' },
      ] }) : jsonResponse({}))
    const { container } = render(ThreadPaneView, { tab: historicalTab() })
    await waitFor(() => expect(container.querySelector('.item-submission-timestamp')?.textContent).toBe('Submitted Oct 7, 6:29:25 PM PDT'))
    expect(container.querySelectorAll('.item-submission-timestamp')).toHaveLength(1)
    expect(container.querySelector('.pane-readonly')?.textContent).toContain('Read only')
    expect(container.querySelector('.userMessage .bubble').textContent).toBe(senderFooterFixture.text)
  })

  // A bad saved transcript, or a thread whose attachment vanished, shows this
  // read-only pane; the operator must still be able to report it from here,
  // without resuming the thread.
  it('saves a Feedback note from a historical thread without resuming it', async () => {
    const posts = []
    vi.mocked(fetch).mockImplementation(async (url, init) => {
      if (init?.method === 'POST') posts.push({ url: String(url), body: JSON.parse(init.body) })
      if (String(url).includes('/transcript')) return jsonResponse({
        ...emptyTranscript,
        items: [{ id: 'saved-9', type: 'agent_message', category: 'messages', text: 'Saved answer' }],
      })
      if (String(url) === '/api/v1/browser-diagnostics') return jsonResponse({ bootId: 'boot-1' })
      if (String(url) === '/api/v1/feedback') return { ...jsonResponse({ id: 'fb_1', createdAt: '2026-10-09T01:07:51Z' }), status: 201 }
      return jsonResponse({})
    })
    const { container, getByRole, getByLabelText } = render(ThreadPaneView, { tab: historicalTab() })
    await waitFor(() => expect(container.textContent).toContain('Saved answer'))

    await fireEvent.click(getByRole('button', { name: 'Feedback' }))
    await fireEvent.input(getByLabelText('Feedback note'), { target: { value: 'answer is cut off' } })
    await fireEvent.click(getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(container.textContent).toContain('Feedback saved at'))

    const save = posts.find((post) => post.url === '/api/v1/feedback')
    expect(save.body).toMatchObject({
      note: 'answer is cut off', bootId: 'boot-1', threadId: 'native-1', provider: 'codex',
      pane: { working: false, pendingPrompts: 0, lastSavedRowId: 'saved-9' },
    })
    expect(posts.some((post) => /resume|\/sessions/.test(post.url))).toBe(false)
  })

  it('opens a file linked by a historical agent message as a pane-native file tab', async () => {
    vi.mocked(fetch).mockImplementation(async (url) => {
      if (String(url).includes('/transcript')) return jsonResponse({
        ...emptyTranscript,
        items: [{
          id: 'history-file', type: 'agent_message', category: 'messages',
          text: 'Read `docs/lifecycle.md:8` next.',
        }],
      })
      return jsonResponse({})
    })
    const { container } = render(ThreadPaneView, { tab: historicalTab({ cwd: '/workspace/nano' }), paneId: 1 })
    const link = await waitFor(() => {
      const candidate = container.querySelector('a.nano-file-link')
      expect(candidate).toBeTruthy()
      return candidate
    })
    await fireEvent.click(link)

    expect(Object.values(panes.tabs)).toContainEqual(expect.objectContaining({
      kind: 'file', requestedPath: 'docs/lifecycle.md', line: 8,
      cwd: '/workspace/nano', catalogThreadId: 'nth_1',
    }))
  })

  it('shows the live conversation with a composer once the tab is bound to a session', () => {
    app.sessions = [liveSession()]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })

    // The regression: this subtree never mounted, so the thread looked resumed
    // (green dot, attached server-side) with nowhere to type.
    const composer = container.querySelector('.composer textarea')
    expect(composer).not.toBeNull()
    expect(composer.disabled).toBe(false)
    expect(container.querySelector('.pane-readonly')).toBeNull()
  })

  it('opens Thread Details in another pane without splitting or unmounting the source thread', async () => {
    app.sessions = [liveSession({ provider: 'grok', model: 'grok-4.6' })]
    explorer.dataset = [catalogItem({ provider: 'grok' })]
    const sourceTabId = openThreadTab(tabTarget({
      item: catalogItem({ provider: 'grok' }),
      session: liveSession({ provider: 'grok', model: 'grok-4.6' }),
    }))

    const { container, getByRole } = render(PanesContainer)

    const sourceView = container.querySelector('.thread-pane-view')
    const textarea = sourceView.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'keep this draft' } })

    expect(container.querySelectorAll('.thread-pane')).toHaveLength(1)
    expect(sourceView.querySelector('.split .pane-right')).toBeNull()
    const open = getByRole('button', { name: 'Open Thread Details' })
    expect(open.closest('.view-bar')).not.toBeNull()
    expect(getByRole('button', { name: 'Detach' }).closest('.view-bar')).toBe(open.closest('.view-bar'))

    await fireEvent.click(open)
    expect(container.querySelectorAll('.thread-pane')).toHaveLength(2)
    expect(panes.items[1].detail).toEqual({ tabId: sourceTabId })
    expect(sourceView.querySelector('.split .pane-right')).toBeNull()
    expect(container.querySelector('.thread-detail-panel')).not.toBeNull()
    expect(sourceView.querySelector('.composer textarea')).toBe(textarea)
    expect(textarea.value).toBe('keep this draft')
  })

  it('adds Under the Hood when a historical Grok thread becomes live', async () => {
    explorer.dataset = [catalogItem({ provider: 'grok' })]
    const historical = historicalTab({ provider: 'grok' })
    const view = render(ThreadPaneView, { tab: historical })

    expect(view.queryByRole('button', { name: 'Under the Hood' })).toBeNull()
    expect(view.container.querySelector('.pane-readonly')?.textContent).toContain('Read only')

    app.sessions = [liveSession({ provider: 'grok', model: 'grok-4.6' })]
    await view.rerender({ tab: { ...historical, sessionId: 'session-1' } })

    expect(view.getByRole('button', { name: 'Open Thread Details' })).toBeTruthy()
    expect(view.container.querySelector('.pane-right')).toBeNull()
    expect(view.container.querySelector('.composer textarea')).not.toBeNull()
    expect(view.container.querySelector('.pane-readonly')).toBeNull()
  })

  it.each(['codex', 'claude', 'grok'])(
    'reveals an accepted %s prompt after the synchronous tail pin even when animation frames are starved',
    async (provider) => {
      // Production can keep replacing a busy thread's settle loop before its
      // callback fires. The synchronous pin already put the transcript at the
      // tail, so waiting for later frames must not leave the entire conversation
      // under visibility:hidden while the accepted row is mounted.
      let frameID = 0
      vi.stubGlobal('requestAnimationFrame', () => ++frameID)
      vi.stubGlobal('cancelAnimationFrame', () => {})
      app.sessions = [liveSession({ provider })]


      const { container } = render(ThreadPaneView, {
        tab: historicalTab({ sessionId: 'session-1', provider }),
      })
      sendProjectionSnapshot(fileStream(), { provider, items: [projectionItem({
        id: 'nano-user:accepted-action', type: 'userMessage', text: 'test. ignore',
        clientActionId: 'accepted-action', done: true,
      })] })
      await tick()

      const conversation = container.querySelector('.convo')
      expect(conversation.textContent).toContain('test. ignore')
      expect(conversation.classList.contains('initial-position')).toBe(false)
    },
  )

  it.each(['codex', 'claude', 'grok'])(
    'paints the saved file rows for %s in server order',
    async (provider) => {
      const durableItems = [
        {
          id: 'durable-older', type: 'agent_message', category: 'messages',
          text: 'An older durable message.', createdAt: '2026-08-27T03:00:00.000Z',
        },
        {
          id: 'durable-before', type: 'agent_message', category: 'messages',
          text: 'I need a go-ahead before I begin the task.', createdAt: '2026-08-27T03:07:50.199Z',
        },
        {
          id: 'durable-prompt', type: 'user_message', category: 'messages',
          text: 'test. ignore', createdAt: '2026-08-27T04:51:52.259Z',
        },
        {
          id: 'durable-after', type: 'agent_message', category: 'messages',
          text: 'Ignored. Still waiting.', createdAt: '2026-08-27T04:53:24.443Z',
        },
      ]
      app.sessions = [liveSession({ provider })]

      const { container } = render(ThreadPaneView, {
        tab: historicalTab({ sessionId: 'session-1', provider }),
      })

      sendHistory({ ...emptyTranscript, items: durableItems })
      await waitFor(() => expect(
        container.querySelector('[data-transcript-source="durable"]'),
      ).not.toBeNull())
      const rows = [...container.querySelectorAll('.convo .item')]
        .map((row) => row.textContent.replace(/\s+/g, ' ').trim())
      expect(rows).toHaveLength(4)
      expect(rows[0]).toContain(durableItems[0].text)
      expect(rows[1]).toContain(durableItems[1].text)
      expect(rows[2]).toContain(durableItems[2].text)
      expect(rows[3]).toContain(durableItems[3].text)
    },
  )

  it('does not put a star control in the live or historical pane header', () => {
    app.sessions = [liveSession()]
    const live = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    expect(live.container.querySelector('.pane-resume-float [data-testid="thread-star"]')).toBeNull()
    live.unmount()
    const historical = render(ThreadPaneView, { tab: historicalTab() })
    expect(historical.container.querySelector('.pane-resume-float [data-testid="thread-star"]')).toBeNull()
  })

  it('re-measures the composer height once a background tab becomes visible', async () => {
    // The bug: a tab mounted in the background sits under display:none, where
    // browsers report scrollHeight as 0, and the auto-grow effect only re-ran
    // on text changes — so the composer stayed pinned at that stale height
    // even after switching to the tab, until the operator typed a character.
    vi.stubGlobal('requestAnimationFrame', (callback) => setTimeout(() => callback(Date.now()), 0))
    vi.stubGlobal('cancelAnimationFrame', (handle) => clearTimeout(handle))
    app.sessions = [liveSession()]
    const tab = historicalTab({ sessionId: 'session-1' })
    const view = render(ThreadPaneView, { tab, visible: false })
    const textarea = view.container.querySelector('.composer textarea')
    let hidden = true
    Object.defineProperty(textarea, 'scrollHeight', { configurable: true, get: () => (hidden ? 0 : 60) })
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(textarea.style.height).toBe('0px')

    hidden = false
    await view.rerender({ tab, visible: true })
    await new Promise((resolve) => setTimeout(resolve, 20))

    expect(textarea.style.height).toBe('60px')
  })

  it('opens a file linked by an agent message as a pane-native file tab', async () => {
    app.sessions = [liveSession()]
    const tab = historicalTab({ sessionId: 'session-1', cwd: '/workspace/nano' })
    const { container } = render(ThreadPaneView, { tab, paneId: 1 })

    replayToSession([
      wireItemEvent(1, {
        id: 'item-file',
        type: 'agentMessage',
        text: 'Open [architecture](/workspace/nano/docs/lifecycle.md:12).',
      }),
    ])
    await tick()
    const link = await waitFor(() => {
      const candidate = container.querySelector('a.nano-file-link')
      expect(candidate).toBeTruthy()
      return candidate
    })
    await fireEvent.click(link)

    expect(Object.values(panes.tabs)).toContainEqual(expect.objectContaining({
      kind: 'file',
      requestedPath: '/workspace/nano/docs/lifecycle.md',
      line: 12,
      cwd: '/workspace/nano',
      catalogThreadId: 'nth_1',
      sessionId: 'session-1',
    }))
  })

  it('forces a historical sidebar open to the tail but preserves deliberate history on tab return', async () => {
    vi.stubGlobal('requestAnimationFrame', (callback) => setTimeout(() => callback(Date.now()), 0))
    vi.stubGlobal('cancelAnimationFrame', (handle) => clearTimeout(handle))
    const items = [
      { id: 'd1', type: 'user_message', category: 'messages', text: 'first' },
      { id: 'd2', type: 'agent_message', category: 'messages', text: 'latest' },
    ]
    fetch.mockImplementation(async (url) => {
      if (String(url).includes('/transcript')) return jsonResponse({ ...emptyTranscript, items })
      return jsonResponse({})
    })
    let tab = historicalTab({ tailResetNonce: 1, forceTailResetNonce: 1 })
    const view = render(ThreadPaneView, { tab, visible: true })
    const scroller = view.container.querySelector('.pane-transcript')
    Object.defineProperty(scroller, 'scrollHeight', { configurable: true, get: () => 1000 })
    Object.defineProperty(scroller, 'clientHeight', { configurable: true, get: () => 200 })

    await waitFor(() => expect(scroller.scrollTop).toBe(1000))
    await new Promise((resolve) => setTimeout(resolve, 20))

    scroller.scrollTop = 320
    fireEvent.wheel(scroller)
    fireEvent.scroll(scroller)
    await tick()
    tab = { ...tab, tailResetNonce: 2, forceTailResetNonce: 0 }
    await view.rerender({ tab, visible: true })
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(scroller.scrollTop).toBe(320)
    await view.rerender({ tab, visible: false })
    await view.rerender({ tab, visible: true })
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(scroller.scrollTop).toBe(320)

    tab = { ...tab, tailResetNonce: 3, forceTailResetNonce: 3 }
    await view.rerender({ tab, visible: true })
    await waitFor(() => expect(scroller.scrollTop).toBe(1000))
  })

  it('re-pins a live thread on navigation unless the tab return preserves deliberate history', async () => {
    vi.stubGlobal('requestAnimationFrame', (callback) => setTimeout(() => callback(Date.now()), 0))
    vi.stubGlobal('cancelAnimationFrame', (handle) => clearTimeout(handle))
    app.sessions = [liveSession()]
    let tab = historicalTab({ sessionId: 'session-1', tailResetNonce: 1, forceTailResetNonce: 1 })
    const view = render(ThreadPaneView, { tab, visible: true })
    const scroller = view.container.querySelector('.convo')
    Object.defineProperty(scroller, 'scrollHeight', { configurable: true, get: () => 1200 })
    Object.defineProperty(scroller, 'clientHeight', { configurable: true, get: () => 240 })

    replayToSession([
      wireItemEvent(1, { id: 'live-1', type: 'userMessage', content: [{ type: 'text', text: 'first' }] }),
      wireItemEvent(2, { id: 'live-2', type: 'agentMessage', text: 'latest' }),
    ])
    await waitFor(() => expect(scroller.scrollTop).toBe(1200))
    await new Promise((resolve) => setTimeout(resolve, 20))

    scroller.scrollTop = 400
    fireEvent.wheel(scroller)
    fireEvent.scroll(scroller)
    await tick()
    tab = { ...tab, tailResetNonce: 2, forceTailResetNonce: 0 }
    await view.rerender({ tab, visible: false })
    await view.rerender({ tab, visible: true })
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(scroller.scrollTop).toBe(400)

    tab = { ...tab, tailResetNonce: 3, forceTailResetNonce: 3 }
    await view.rerender({ tab, visible: true })
    await waitFor(() => expect(scroller.scrollTop).toBe(1200))
  })

  // File-only panes keep the base loading copy until the first saved tail.
  // The composer still distinguishes creation from resume while handshaking.
  it('shows only history loading while a fresh start awaits its first file tail', () => {
    app.sessions = [liveSession({ threadId: '', status: 'starting', spawnMode: 'start' })]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })

    expect(container.querySelector('.convo-empty')).toBeNull()
    expect(container.querySelector('.history-fallback-note')?.textContent).toBe('loading history…')
    expect(container.querySelector('.composer textarea')?.placeholder).toBe('starting session…')
  })

  it('shows only history loading while a resume awaits its first file tail', () => {
    app.sessions = [liveSession({ status: 'starting', spawnMode: 'resume' })]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })

    expect(container.querySelector('.convo-empty')).toBeNull()
    expect(container.querySelector('.history-fallback-note')?.textContent).toBe('loading history…')
    expect(container.querySelector('.composer textarea')?.placeholder).toBe('attaching to thread…')
  })

  it('surfaces a failed background resume in the pane, not just the Details panel', async () => {
    app.sessions = [liveSession({ status: 'exited' })]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    // The session-list poll delivering the R3 failure contract: exited + error.
    syncSessionSummaries([liveSession({ status: 'exited', error: 'thread/resume: rollout missing' })])
    await tick()

    expect(container.querySelector('.convo-exited-note')?.textContent).toContain('thread/resume: rollout missing')
  })

  it('offers Resume on a historical codex thread without opening Details', () => {
    const { getByRole } = render(ThreadPaneView, { tab: historicalTab() })

    const button = getByRole('button', { name: /resume thread/i })
    expect(button.disabled).toBe(false)
  })

  // Catches an opened thread that waits its turn in the boot queue after a
  // restart: one the active sidebar leaves out, one asked again on every
  // refresh or tab switch, one never asked again after a later restart, and a
  // detached thread re-attached merely because its pane opened. Boundary: the
  // pane's requests for a thread the detached registry reports.
  it('asks Nano once per restart to re-attach first a starting thread its pane opens, and leaves a detached one alone', async () => {
    const prioritized = () => fetch.mock.calls.filter(([url, init]) =>
      String(url) === '/api/v1/hosted-threads/nht-1/prioritize' && init?.method === 'POST')
    const stored = (status) => [{ provider: 'codex', nativeThreadId: 'native-1', hostedThreadId: 'nht-1', status }]
    explorer.dataset = [] // the active sidebar leaves an idle stored thread out
    app.detached = stored('starting')
    const view = render(ThreadPaneView, { tab: historicalTab() })
    await waitFor(() => expect(prioritized()).toHaveLength(1))
    app.detached = stored('starting') // a refresh while it still waits
    await view.rerender({ tab: historicalTab(), visible: false })
    await view.rerender({ tab: historicalTab(), visible: true }) // and a tab switch
    expect(prioritized()).toHaveLength(1)

    // It goes live in the same mounted pane; a later restart leaves it
    // starting again.
    app.detached = []
    app.sessions = [liveSession()]
    await view.rerender({ tab: historicalTab({ sessionId: 'session-1' }) })
    app.sessions = []
    app.detached = stored('starting')
    await view.rerender({ tab: historicalTab({ detached: true }) })
    await waitFor(() => expect(prioritized()).toHaveLength(2))
    cleanup()

    app.detached = stored(undefined)
    render(ThreadPaneView, { tab: historicalTab() })
    await tick()
    expect(prioritized()).toHaveLength(2)
  })

  it('keeps Resume visible but disabled on an archived thread, with the reason attached', () => {
    explorer.dataset = [catalogItem({ archived: true })]
    const { getByRole } = render(ThreadPaneView, { tab: historicalTab() })

    const button = getByRole('button', { name: /resume thread/i })
    expect(button.disabled).toBe(true)
    expect(button.getAttribute('title')).toMatch(/archived/i)
  })

  it('offers Detach on a live pane; detaching returns the tab to read-only history', async () => {
    app.sessions = [liveSession()]
    const { container, getByRole } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })

    await fireEvent.click(getByRole('button', { name: 'Detach' }))

    expect(fetch).toHaveBeenCalledWith('/api/sessions/session-1', { method: 'DELETE' })
    // The refreshed (stubbed, now empty) session list drops the binding and the
    // pane falls back to the durable read-only view.
    await waitFor(() => expect(container.querySelector('.pane-readonly')).not.toBeNull())
    expect(container.querySelector('.composer textarea')).toBeNull()
  })

  it('opens descendant selection before a pane Detach changes runtime state', async () => {
    const parent = catalogItem({ hostedThreadId: 'nht-parent' })
    const child = catalogItem({
      id: 'nth-child', nativeId: 'child-native', hostedThreadId: 'nht-child',
      invokerHostedThreadId: 'nht-parent', title: 'Child worker',
      assignment: { assignmentRole: 'worker' },
    })
    explorer.dataset = [parent, child]
    app.sessions = [
      liveSession({ hostedThreadId: 'nht-parent' }),
      liveSession({ id: 'child-session', threadId: 'child-native', hostedThreadId: 'nht-child' }),
    ]
    const { container, getByRole } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })

    await fireEvent.click(getByRole('button', { name: 'Detach' }))

    await waitFor(() => expect(descendantClose.request?.nodes.map((node) => node.label)).toEqual(['Child worker']))
    expect(fetch).not.toHaveBeenCalledWith('/api/sessions/session-1', { method: 'DELETE' })
    expect(container.querySelector('.composer textarea')).not.toBeNull()
  })

  it('labels the detach affordance per provider: ending a claude session is not a detach', () => {
    app.sessions = [liveSession({ provider: 'claude' })]
    const { getByRole } = render(ThreadPaneView, { tab: historicalTab({ provider: 'claude', sessionId: 'session-1' }) })

    expect(getByRole('button', { name: 'End session' })).not.toBeNull()
  })

  it('offers Resume on a historical claude thread with a recorded working directory', () => {
    explorer.dataset = [claudeItem()]
    const { getByRole } = render(ThreadPaneView, { tab: historicalTab({ provider: 'claude' }) })

    const button = getByRole('button', { name: /resume thread/i })
    expect(button.disabled).toBe(false)
  })

  it('offers no Resume at all on a claude subagent row — not even a disabled one', () => {
    explorer.dataset = [claudeItem({ kind: 'subagent', fleet: { label: 'working' } })]
    const { container, queryByRole } = render(ThreadPaneView, { tab: historicalTab({ provider: 'claude' }) })

    expect(queryByRole('button', { name: /resume thread/i })).toBeNull()
    expect(container.querySelector('.pane-readonly')?.textContent).toContain('Watching worker · live updates')
  })

  it('disables Resume with the reason when the claude session file was purged', () => {
    explorer.dataset = [claudeItem({ source: { status: 'source_missing' } })]
    const { getByRole } = render(ThreadPaneView, { tab: historicalTab({ provider: 'claude' }) })

    const button = getByRole('button', { name: /resume thread/i })
    expect(button.disabled).toBe(true)
    expect(button.getAttribute('title')).toMatch(/purged/i)
  })

  // The identity fork: `claude -p --resume` cannot continue a process, so it
  // spawns a new one that mints a brand-new native session id and announces it
  // asynchronously. Every step of that is a chance to strand the operator in a
  // second tab for the conversation they are already looking at.
  it('keeps one tab across a claude resume’s native-id handoff', async () => {
    __resetPanesForTest()
    explorer.dataset = [claudeItem()]
    const tabId = openThreadTab(tabTarget({ item: claudeItem() }))
    const tab = panes.tabs[tabId]

    // The POST answers before the child has said who it is: no thread id yet.
    let sessions = [claudeSuccessor({ threadId: '', status: 'starting' })]
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url, opts) => {
        if (String(url).includes('/transcript')) return jsonResponse(emptyTranscript)
        if (opts?.method === 'POST') return jsonResponse(claudeSuccessor({ threadId: '', status: 'starting' }))
        if (String(url) === '/api/sessions') return jsonResponse({ sessions, detached: [], defaults: {} })
        return jsonResponse({})
      }),
    )

    const { container, getByRole } = render(ThreadPaneView, { tab })
    await fireEvent.click(getByRole('button', { name: /resume thread/i }))

    await waitFor(() => expect(panes.tabs[tabId].sessionId).toBe('session-2'))
    expect(Object.keys(panes.tabs)).toHaveLength(1)

    // system.init lands: the successor announces its own native id.
    sessions = [claudeSuccessor()]
    await refreshSessions()
    await tick()

    expect(Object.keys(panes.tabs)).toHaveLength(1)
    expect(panes.items).toHaveLength(1)
    expect(panes.items[0].tabIds).toEqual([tabId])
    expect(panes.tabs[tabId]).toMatchObject({ sessionId: 'session-2', nativeId: 'native-2' })
    // And the pane the operator is looking at is the live one.
    expect(container.querySelector('.composer textarea')).not.toBeNull()
  })

  // The other half of the handoff, and the one that bites after the operator
  // walks away: ending the resumed session leaves a read-only pane behind. If
  // that pane keeps resolving its thread from the tab's now-stale catalog id,
  // it shows the predecessor's transcript and offers to resume the
  // predecessor — forking the conversation a second time and leaving behind
  // everything the successor just did.
  it('after a claude resume, id handoff and detach, the pane and its Resume target are the successor', async () => {
    __resetPanesForTest()
    const predecessor = claudeItem()
    const successorRow = claudeItem({ id: 'nth_2', nativeId: 'native-2', title: 'Pane mechanics' })
    explorer.dataset = [predecessor]
    const tabId = openThreadTab(tabTarget({ item: predecessor }))
    const tab = panes.tabs[tabId]

    let sessions = [claudeSuccessor({ threadId: '', status: 'starting' })]
    const transcriptFetches = []
    const posts = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url, opts) => {
        if (String(url).includes('/transcript')) {
          transcriptFetches.push(String(url))
          return jsonResponse(emptyTranscript)
        }
        if (String(url) === '/api/sessions' && opts?.method === 'POST') {
          posts.push(JSON.parse(opts.body))
          return jsonResponse(claudeSuccessor({ threadId: '', status: 'starting' }))
        }
        if (opts?.method === 'DELETE') {
          sessions = []
          return jsonResponse({})
        }
        if (String(url) === '/api/sessions') return jsonResponse({ sessions, detached: [], defaults: {} })
        return jsonResponse({})
      }),
    )

    const { container, getByRole } = render(ThreadPaneView, { tab })
    await fireEvent.click(getByRole('button', { name: /resume thread/i }))
    await waitFor(() => expect(panes.tabs[tabId].sessionId).toBe('session-2'))

    // The successor announces its native id, and the catalog indexes its row.
    sessions = [claudeSuccessor()]
    explorer.dataset = [predecessor, successorRow]
    await refreshSessions()
    await tick()
    expect(panes.tabs[tabId].nativeId).toBe('native-2')

    await fireEvent.click(getByRole('button', { name: 'End session' }))
    await waitFor(() => expect(container.querySelector('.pane-readonly')).not.toBeNull())

    // The surviving read-only view reads the successor's transcript, not the
    // predecessor's snapshot from before the resume.
    expect(transcriptFetches.some((url) => url.includes('/threads/nth_2/transcript'))).toBe(true)

    // And resuming again continues the successor rather than forking the
    // predecessor a second time.
    await fireEvent.click(getByRole('button', { name: /resume thread/i }))
    await waitFor(() => expect(posts).toHaveLength(2))
    expect(posts[1]).toMatchObject({ provider: 'claude', mode: 'resume', threadId: 'native-2' })
  })

  it('filters the live wire block in the enhanced view, and shows it all in All', async () => {
    // The bug this covers: the durable half of a resumed thread was filtered
    // server-side while the live half rendered raw wire, so reasoning and shell
    // items reappeared below the seam — and the pane had no toggle to escape.
    app.sessions = [liveSession()]
    const { container, getByRole } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })

    replayToSession([
      wireItemEvent(1, { id: 'item-1', type: 'agentMessage', text: 'Rebuilt the bundle.' }),
      wireItemEvent(2, { id: 'item-2', type: 'reasoning', summary: [{ type: 'text', text: 'Weighing options' }] }),
      wireItemEvent(3, { id: 'item-3', type: 'commandExecution', command: 'go build ./...', exitCode: 0 }),
      wireItemEvent(4, { id: 'item-4', type: 'provider_event', text: 'Unclassified provider record' }),
    ])
    await tick()

    const live = () => container.querySelector('.convo').textContent
    expect(live()).toContain('Rebuilt the bundle.')
    expect(live()).not.toContain('Weighing options')
    expect(live()).not.toContain('go build ./...')
    expect(live()).not.toContain('Unclassified provider record')
    expect(container.querySelector('.view-hint')).toBeNull()

    await fireEvent.click(getByRole('button', { name: 'All' }))
    await waitForDetailStream()
    replayToSession([
      wireItemEvent(1, { id: 'item-1', type: 'agentMessage', text: 'Rebuilt the bundle.' }),
      wireItemEvent(2, { id: 'item-2', type: 'reasoning', summary: [{ type: 'text', text: 'Weighing options' }] }),
      wireItemEvent(3, { id: 'item-3', type: 'commandExecution', command: 'go build ./...', exitCode: 0 }),
      wireItemEvent(4, { id: 'item-4', type: 'provider_event', text: 'Unclassified provider record' }),
    ])
    await tick()

    expect(live()).toContain('Rebuilt the bundle.')
    expect(live()).toContain('Weighing options')
    expect(live()).toContain('go build ./...')
    expect(live()).toContain('Unclassified provider record')
  })

  it('renders a saved command-output truncation marker in All', async () => {
    app.sessions = [liveSession()]
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    await fireEvent.click(view.getByRole('button', { name: 'All' }))
    const marker = '… [live command output truncated] …'
    sendHistory({ ...emptyTranscript, items: [{
      id: 'bounded-command', type: 'command', category: 'toolCall',
      text: `head\n${marker}\ntail`, contentTruncated: true,
    }] })
    await tick()
    const output = view.container.querySelector('[data-transcript-id="bounded-command"] [data-transcript-content]')
    expect(output.textContent).toBe(`head\n${marker}\ntail`)
  })

  it('checks rendered messages in both Enhanced and All views without scope controls', async () => {
    app.sessions = [liveSession()]
    const postedResults = []
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const target = String(url)
      if (target.includes('/transcript') && !target.includes('/transcript-audits')) return jsonResponse(emptyTranscript)
      if (target === '/api/v1/transcript-audits' && options.method === 'POST') {
        const request = JSON.parse(options.body)
        return jsonResponse({
          auditId: 'tau-component', clientActionId: request.clientActionId, threadId: request.threadRef,
          catalogThreadId: 'nth_1', tabId: request.tabId, pageId: request.pageId, paneId: request.paneId, scopes: request.scopes, status: 'pending',
        })
      }
      if (target.includes('/transcript-audits/tau-component/reference')) {
        return jsonResponse({
          projectionKey: 'projection-component', snapshotHeadSeq: 4, startSeq: 1, endSeq: 4,
          hasMoreBefore: false, capReached: false,
          rows: [
            { id: 'saved-message', projectedSeq: 1, type: 'agent_message', category: 'messages', role: 'assistant', text: '**Rendered answer**', contentTruncated: false },
          ],
        })
      }
      if (target.endsWith('/transcript-audits/tau-component/result') && options.method === 'POST') {
        const submitted = JSON.parse(options.body)
        postedResults.push(submitted.result)
        return jsonResponse({ status: submitted.result.status, result: submitted.result })
      }
      return jsonResponse({})
    }))
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }), paneId: 1 })
    replayToSession([
      wireItemEvent(1, { id: 'live-message', durableItemID: 'saved-message', projectedSeq: 1, type: 'agentMessage', text: '**Rendered answer**' }),
    ])
    await tick()

    await requestAudit()
    await waitFor(() => expect(postedResults).toHaveLength(1))
    expect(postedResults[0].scopes).toEqual(['messages'])
    expect(postedResults[0].counts.contentChecked).toBe(1)
    expect(view.getByTestId('transcript-audit-result').textContent).toContain('Displayed window matches saved transcript')

    await fireEvent.click(view.getByRole('button', { name: 'All' }))
    await waitForDetailStream()
    replayToSession([
      wireItemEvent(1, { id: 'live-message', durableItemID: 'saved-message', projectedSeq: 1, type: 'agentMessage', text: '**Rendered answer**' }),
      wireItemEvent(2, { id: 'live-tool', durableItemID: 'saved-tool', projectedSeq: 2, type: 'commandExecution', command: 'printf ok', aggregatedOutput: 'ok', exitCode: 0, status: 'completed' }),
      wireItemEvent(3, { id: 'live-tool-result', durableItemID: 'saved-tool-result', projectedSeq: 3, type: 'toolResult', text: 'tool output' }),
      wireItemEvent(4, { id: 'live-reason', durableItemID: 'saved-reason', projectedSeq: 4, type: 'reasoning', summary: [] }),
    ])
    await tick()

    const renderedToolResult = view.container.querySelector('[data-transcript-id="saved-tool-result"]')
    expect(renderedToolResult.querySelector('.kind').textContent).toBeTruthy()
    expect(renderedToolResult.querySelector('[data-transcript-content]').textContent).toContain('tool output')

    expect(view.queryByLabelText('tools')).toBeNull()
    expect(view.queryByLabelText('reasoning')).toBeNull()
    await requestAudit()

    await waitFor(() => expect(postedResults).toHaveLength(2))
    expect(postedResults[1].scopes).toEqual(['messages'])
    expect(postedResults[1].status).toBe('match')
    expect(postedResults[1].counts).toMatchObject({ contentChecked: 1, changed: 0 })
  })

  it.each([
    ['newest', [
      { id: 'live-a', durableItemID: 'saved-a', projectedSeq: 1, text: 'one' },
      { id: 'live-b', durableItemID: 'saved-b', projectedSeq: 2, text: 'two' },
    ], 'saved-c'],
    ['oldest', [
      { id: 'live-b', durableItemID: 'saved-b', projectedSeq: 2, text: 'two' },
      { id: 'live-c', durableItemID: 'saved-c', projectedSeq: 3, text: 'three' },
    ], 'saved-a'],
  ])('detects a dropped %s message through the rendered pane', async (_edge, mounted, missingID) => {
    app.sessions = [liveSession()]
    const postedResults = []
    const reference = {
      projectionKey: 'projection-boundary', snapshotHeadSeq: 3, startSeq: 1, endSeq: 3,
      hasMoreBefore: false, capReached: false,
      rows: [
        { id: 'saved-a', projectedSeq: 1, type: 'agent_message', category: 'messages', role: 'assistant', text: 'one', contentTruncated: false },
        { id: 'saved-b', projectedSeq: 2, type: 'agent_message', category: 'messages', role: 'assistant', text: 'two', contentTruncated: false },
        { id: 'saved-c', projectedSeq: 3, type: 'agent_message', category: 'messages', role: 'assistant', text: 'three', contentTruncated: false },
      ],
    }
    const fetchMock = transcriptAuditFetch(reference, postedResults)
    vi.stubGlobal('fetch', fetchMock)
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }), paneId: 1 })
    replayToSession(mounted.map((item, index) => wireItemEvent(index + 1, { ...item, type: 'agentMessage' })))
    await tick()
    await requestAudit()
    await waitFor(() => expect(postedResults).toHaveLength(1))

    expect(postedResults[0].status).toBe('mismatch')
    expect(postedResults[0].differences).toContainEqual(expect.objectContaining({ kind: 'missing', id: missingID }))
    const referenceCall = fetchMock.mock.calls.find(([url]) => String(url).includes('/reference'))
    expect(String(referenceCall[0])).toContain('minProjectedSeq=0')
    expect(String(referenceCall[0])).toContain('maxProjectedSeq=0')
  })

  // Its work is fixed (300 replayed events, 152 mounted rows); only CPU
  // contention changes its time. Measured 2026-09-22 on the deploy machine:
  // alone 1.1 s; full web suite 2.2-3.4 s; two suites at once 4.9-5.2 s;
  // failed deploy gates at load average 49-85, 5.2 s and 6.1 s. The limit is
  // 2.5x the worst observed, so contention passes and a real hang still fails.
  it('handles a row-capped reference with more than 100 mounted saved and confirmed live rows', async () => {
    app.sessions = [liveSession()]
    const postedResults = []
    const savedHistory = {
      ...emptyTranscript,
      projection: { state: 'ready', key: 'projection-cap-component' },
      items: Array.from({ length: 2 }, (_, index) => ({
        id: `saved-${index + 1}`, projectedSeq: index + 1, type: 'agent_message', category: 'messages', role: 'assistant', text: `message ${index + 1}`,
      })),
    }
    const reference = {
      projectionKey: 'projection-cap-component', snapshotHeadSeq: 152, startSeq: 53, endSeq: 152,
      hasMoreBefore: true, capReached: true, capReason: 'rows',
      rows: Array.from({ length: 100 }, (_, index) => ({
        id: `saved-${index + 53}`, projectedSeq: index + 53, type: 'agent_message', category: 'messages', role: 'assistant', text: `message ${index + 53}`, contentTruncated: false,
      })),
    }
    vi.stubGlobal('fetch', transcriptAuditFetch(reference, postedResults, { history: savedHistory }))
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }), paneId: 1 })
    sendHistory(savedHistory)
    const events = []
    for (let liveSeq = 1; liveSeq <= 150; liveSeq += 1) {
      const projectedSeq = liveSeq + 2
      events.push(wireItemEvent(liveSeq, { id: `live-${projectedSeq}`, liveSeq, type: 'agentMessage', text: `message ${projectedSeq}` }))
      events.push(wireDurableConfirm(`live-${projectedSeq}`, `saved-${projectedSeq}`, projectedSeq))
    }
    replayToSession(events)
    await tick()
    while (view.container.querySelector('.load-older')) {
      await fireEvent.click(view.container.querySelector('.load-older'))
      await tick()
    }
    expect(view.container.querySelectorAll('[data-transcript-row]')).toHaveLength(152)
    await requestAudit()
    await waitFor(() => expect(postedResults).toHaveLength(1))

    expect(postedResults[0].status).toBe('inconclusive')
    expect(postedResults[0].counts).toMatchObject({ browser: 152, extra: 0 })
    expect(postedResults[0].exclusions).toContainEqual({ scope: 'messages', reason: 'before_reference_cap', count: 52 })
    expect(postedResults[0].reasons).toContain('reference_row_cap_reached')
  }, 15000)

  it('bounds the unverified saved and confirmed-live prefix of a byte-capped reference', async () => {
    app.sessions = [liveSession()]
    const postedResults = []
    const savedHistory = {
      ...emptyTranscript,
      projection: { state: 'ready', key: 'projection-byte-cap-component' },
      items: [1, 2].map((seq) => ({
        id: `saved-${seq}`, projectedSeq: seq, type: 'agent_message', category: 'messages', role: 'assistant', text: `message ${seq}`,
      })),
    }
    const reference = {
      projectionKey: 'projection-byte-cap-component', snapshotHeadSeq: 8, startSeq: 6, endSeq: 8,
      hasMoreBefore: true, capReached: true, capReason: 'bytes',
      rows: [6, 7, 8].map((seq) => ({
        id: `saved-${seq}`, projectedSeq: seq, type: 'agent_message', category: 'messages', role: 'assistant', text: `message ${seq}`, contentTruncated: false,
      })),
    }
    vi.stubGlobal('fetch', transcriptAuditFetch(reference, postedResults, { history: savedHistory }))
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }), paneId: 1 })
    sendHistory(savedHistory)
    const events = []
    for (let liveSeq = 1; liveSeq <= 6; liveSeq += 1) {
      const projectedSeq = liveSeq + 2
      events.push(wireItemEvent(liveSeq, { id: `live-${projectedSeq}`, liveSeq, type: 'agentMessage', text: `message ${projectedSeq}` }))
      events.push(wireDurableConfirm(`live-${projectedSeq}`, `saved-${projectedSeq}`, projectedSeq))
    }
    replayToSession(events)
    await waitFor(() => expect(view.container.querySelectorAll('[data-transcript-row]')).toHaveLength(8))
    await requestAudit()
    await waitFor(() => expect(postedResults).toHaveLength(1))

    expect(postedResults[0].status).toBe('inconclusive')
    expect(postedResults[0].counts).toMatchObject({ browser: 8, extra: 0 })
    expect(postedResults[0].exclusions).toContainEqual({ scope: 'messages', reason: 'before_reference_cap', count: 5 })
    expect(postedResults[0].reasons).toContain('reference_byte_cap_reached')
  })

  it('stays inconclusive while durable history is pending instead of claiming transcript start', async () => {
    app.sessions = [liveSession()]
    const postedResults = []
    const history = { ...emptyTranscript, projection: { state: 'pending', key: 'projection-pending' } }
    const reference = {
      projectionKey: 'projection-pending', snapshotHeadSeq: 1, startSeq: 1, endSeq: 1,
      hasMoreBefore: false, capReached: false,
      rows: [{ id: 'saved-1', projectedSeq: 1, type: 'agent_message', category: 'messages', role: 'assistant', text: 'not loaded yet', contentTruncated: false }],
    }
    vi.stubGlobal('fetch', transcriptAuditFetch(reference, postedResults, { history }))
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }), paneId: 1 })
    sendHistory(history)
    await tick()
    await requestAudit()
    await waitFor(() => expect(postedResults).toHaveLength(1))

    expect(postedResults[0].status).toBe('inconclusive')
    expect(postedResults[0].reasons).toContain('start_boundary_unknown')
    expect(postedResults[0].counts.missing).toBe(0)
  })

  it('reports all rendered missing rows when difference details hit the output cap', async () => {
    app.sessions = [liveSession()]
    const postedResults = []
    vi.stubGlobal('fetch', transcriptAuditFetch({
      projectionKey: 'projection-many-missing', snapshotHeadSeq: 40, startSeq: 1, endSeq: 40,
      hasMoreBefore: false, capReached: false,
      rows: Array.from({ length: 40 }, (_, index) => ({
        id: `saved-${index + 1}`, projectedSeq: index + 1, type: 'agent_message', category: 'messages',
        role: 'assistant', text: `message ${index + 1}`, contentTruncated: false,
      })),
    }, postedResults))
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }), paneId: 1 })
    replayToSession([
      wireItemEvent(1, { id: 'live-first', durableItemID: 'saved-1', projectedSeq: 1, type: 'agentMessage', text: 'message 1' }),
      wireItemEvent(2, { id: 'live-last', durableItemID: 'saved-40', projectedSeq: 40, type: 'agentMessage', text: 'message 40' }),
    ])
    await tick()
    await requestAudit()
    await waitFor(() => expect(postedResults).toHaveLength(1))

    expect(postedResults[0].counts.missing).toBe(38)
    expect(postedResults[0].differences).toHaveLength(20)
    expect(postedResults[0].differencesTruncated).toBe(true)
  })

  it('aggregates repeated rendered exclusions into a bounded result', async () => {
    app.sessions = [liveSession()]
    const postedResults = []
    vi.stubGlobal('fetch', transcriptAuditFetch({
      projectionKey: 'projection-ambiguous', snapshotHeadSeq: 0, startSeq: 0, endSeq: 0,
      hasMoreBefore: false, capReached: false, rows: [],
    }, postedResults))
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }), paneId: 1 })
    replayToSession(Array.from({ length: 40 }, (_, index) => wireItemEvent(index + 1, {
      id: `unconfirmed-${index + 1}`, type: 'agentMessage', text: `message ${index + 1}`,
    }, 'item/started')))
    await tick()
    await requestAudit()
    await waitFor(() => expect(postedResults).toHaveLength(1))

    expect(postedResults[0].status).toBe('inconclusive')
    expect(postedResults[0].counts.notChecked).toBe(40)
    expect(postedResults[0].exclusions).toEqual([
      { scope: 'messages', reason: 'ambiguous_identity', count: 40 },
    ])
  })

  it('detects different-projection identities and excludes a post-pin rendered row', async () => {
    app.sessions = [liveSession()]
    const postedResults = []
    vi.stubGlobal('fetch', transcriptAuditFetch({
      projectionKey: 'projection-new', snapshotHeadSeq: 2, startSeq: 1, endSeq: 2,
      hasMoreBefore: false, capReached: false,
      rows: [
        { id: 'new-a', projectedSeq: 1, type: 'agent_message', category: 'messages', role: 'assistant', text: 'one', contentTruncated: false },
        { id: 'new-b', projectedSeq: 2, type: 'agent_message', category: 'messages', role: 'assistant', text: 'two', contentTruncated: false },
      ],
    }, postedResults))
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }), paneId: 1 })
    replayToSession([
      wireItemEvent(1, { id: 'live-old-a', durableItemID: 'old-a', projectedSeq: 1, type: 'agentMessage', text: 'one' }),
      wireItemEvent(2, { id: 'live-old-b', durableItemID: 'old-b', projectedSeq: 2, type: 'agentMessage', text: 'two' }),
      wireItemEvent(3, { id: 'live-after', durableItemID: 'after-pin', projectedSeq: 3, type: 'agentMessage', text: 'after pin' }),
    ])
    await tick()
    await requestAudit()
    await waitFor(() => expect(postedResults).toHaveLength(1))

    expect(postedResults[0].status).toBe('mismatch')
    expect(postedResults[0].counts).toMatchObject({ missing: 2, extra: 2 })
    expect(postedResults[0].exclusions).toContainEqual({ scope: 'messages', reason: 'after_pinned_snapshot', count: 1 })
  })

  it('audits a pane without a catalog id by its actual native thread identity', async () => {
    app.sessions = [liveSession()]
    const postedResults = []
    const submittedBodies = []
    vi.stubGlobal('fetch', transcriptAuditFetch({
      projectionKey: 'projection-native', snapshotHeadSeq: 1, startSeq: 1, endSeq: 1,
      hasMoreBefore: false, capReached: false,
      rows: [{ id: 'saved', projectedSeq: 1, type: 'agent_message', category: 'messages', role: 'assistant', text: 'answer', contentTruncated: false }],
    }, postedResults, { submittedBodies }))
    const view = render(ThreadPaneView, { tab: historicalTab({ catalogId: '', sessionId: 'session-1' }), paneId: 1 })
    replayToSession([wireItemEvent(1, { id: 'live', durableItemID: 'saved', projectedSeq: 1, type: 'agentMessage', text: 'answer' })])
    await tick()
    await requestAudit({ threadRef: 'native-1' })
    await waitFor(() => expect(postedResults).toHaveLength(1))

    expect(postedResults[0].status).toBe('match')
    expect(submittedBodies[0].paneThreadRef).toBe('native-1')
    expect(submittedBodies[0]).not.toHaveProperty('catalogThreadId')
    expect(submittedBodies[0]).not.toHaveProperty('threadId')
  })

  it('ignores an audit addressed only by unsupported session or hosted aliases', async () => {
    app.sessions = [liveSession({ threadId: '', hostedThreadId: 'nht-only-alias' })]
    const postedResults = []
    vi.stubGlobal('fetch', transcriptAuditFetch({}, postedResults, { catalogThreadId: 'nht-only-alias' }))
    render(ThreadPaneView, { tab: historicalTab({ catalogId: '', nativeId: '', sessionId: 'session-1' }), paneId: 1 })
    await requestAudit({ threadRef: 'nht-only-alias' })
    await tick()
    expect(postedResults).toEqual([])
  })

  it('reports rendered code whitespace changes and truncated saved content honestly', async () => {
    app.sessions = [liveSession()]
    const postedResults = []
    vi.stubGlobal('fetch', transcriptAuditFetch({
      projectionKey: 'projection-code', snapshotHeadSeq: 2, startSeq: 1, endSeq: 2,
      hasMoreBefore: false, capReached: false,
      rows: [
        { id: 'saved-code', projectedSeq: 1, type: 'user_message', category: 'messages', role: 'user', text: 'line 1\n\nline 2', contentTruncated: false },
        { id: 'saved-truncated', projectedSeq: 2, type: 'agent_message', category: 'messages', role: 'assistant', text: 'bounded', contentTruncated: true },
      ],
    }, postedResults))
    const view = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }), paneId: 1 })
    replayToSession([
      wireItemEvent(1, { id: 'live-code', durableItemID: 'saved-code', projectedSeq: 1, type: 'userMessage', content: [{ type: 'input_text', text: 'line 1\nline 2' }] }),
      wireItemEvent(2, { id: 'live-truncated', durableItemID: 'saved-truncated', projectedSeq: 2, type: 'agentMessage', text: 'bounded' }),
    ])
    await tick()
    await requestAudit()
    await waitFor(() => expect(postedResults).toHaveLength(1))

    expect(postedResults[0].status).toBe('mismatch')
    expect(postedResults[0].differences).toContainEqual(expect.objectContaining({ kind: 'changed', id: 'saved-code' }))
    expect(postedResults[0].exclusions).toContainEqual({ scope: 'messages', reason: 'reference_content_truncated', count: 1 })
  })

  it('opens a fresh unfiltered file stream for All and a filtered stream for Enhanced', async () => {
    app.sessions = [liveSession()]
    const items = [
      projectionItem({ id: 'd1', text: 'durable answer' }),
      projectionItem({ id: 'd2', type: 'reasoning', text: 'durable reasoning' }),
      projectionItem({ id: 'd3', type: 'provider_event', category: 'unclassified', text: 'Unclassified provider record' }),
    ]
    const { container, getByRole } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    sendProjectionSnapshot(fileStream(), { items })
    await waitFor(() => expect(transcriptText(container)).toContain('durable answer'))
    expect(transcriptText(container)).not.toContain('durable reasoning')
    expect(transcriptText(container)).not.toContain('Unclassified provider record')
    await fireEvent.click(getByRole('button', { name: 'All' }))
    expect(fileStream().url).toContain('hide=none')
    sendProjectionSnapshot(fileStream(), { items })
    await waitFor(() => expect(transcriptText(container)).toContain('durable reasoning'))
    expect(transcriptText(container)).toContain('Unclassified provider record')
    await fireEvent.click(getByRole('button', { name: 'Enhanced' }))
    expect(fileStream().url).toContain('view=enhanced')
    sendProjectionSnapshot(fileStream(), { items })
    await waitFor(() => expect(transcriptText(container)).not.toContain('durable reasoning'))
  })

  it('keeps the working strip on screen when a live thread starts a turn after the last message', async () => {
    vi.stubGlobal('requestAnimationFrame', (callback) => setTimeout(() => callback(Date.now()), 0))
    vi.stubGlobal('cancelAnimationFrame', (handle) => clearTimeout(handle))
    app.sessions = [liveSession()]
    const { container } = render(ThreadPaneView, {
      tab: historicalTab({ sessionId: 'session-1', tailResetNonce: 1, forceTailResetNonce: 1 }),
    })
    const scroller = container.querySelector('.convo')
    Object.defineProperty(scroller, 'scrollHeight', {
      configurable: true,
      get: () => scroller.querySelector('.thinking') ? 1280 : 1200,
    })
    Object.defineProperty(scroller, 'clientHeight', { configurable: true, get: () => 240 })

    replayToSession([
      wireItemEvent(1, { id: 'live-1', type: 'userMessage', content: [{ type: 'text', text: 'go' }] }),
      wireItemEvent(2, { id: 'live-2', type: 'agentMessage', text: 'latest' }),
    ])
    await waitFor(() => expect(scroller.scrollTop).toBe(1200))
    await new Promise((resolve) => setTimeout(resolve, 40))
    expect(scroller.scrollTop).toBe(1200)

    // Flip only the turn state. A full projection rewrite would retrigger the
    // item-count effect and hide the gap: Working is not a transcript row.
    getSessionStore('session-1').store.turnActive = true
    getSessionStore('session-1').store.threadStatus = 'working'
    await waitFor(() => expect(container.querySelector('.thinking-label')?.textContent).toBe('Working'))
    await waitFor(() => expect(scroller.scrollTop).toBe(1280))
  })

  it('still shows activity while the command it is hiding runs', async () => {
    // Hiding tool activity must not make a working session look frozen: the
    // ephemeral thinking surface, not the transcript, is what carries liveness
    // in the enhanced view.
    app.sessions = [liveSession()]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    syncSessionSummaries([liveSession()]) // the store learns its provider here

    replayToSession([
      wireEvent(1, 'turn/started', {}),
      wireItemEvent(2, { id: 'item-1', type: 'commandExecution', command: 'go test ./...' }, 'item/started'),
    ])
    await tick()

    expect(container.querySelector('.thinking-label')?.textContent).toBe('Working')
    expect(container.querySelector('.thinking-detail')).toBeNull()
    // The fixed activity bar above the composer is gone; the surface replaced it.
    expect(container.querySelector('.live-turn')).toBeNull()
  })

  it('keeps enhanced activity visible after an intermediate agent message until the turn ends', async () => {
    let frameID = 0
    let frameAt = performance.now()
    const frames = new Map()
    vi.stubGlobal('requestAnimationFrame', (callback) => {
      const id = ++frameID
      frames.set(id, callback)
      return id
    })
    vi.stubGlobal('cancelAnimationFrame', (id) => frames.delete(id))
    app.sessions = [liveSession()]
    const { container } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })
    syncSessionSummaries([liveSession()])

    replayToSession([
      wireEvent(1, 'turn/started', {}),
      wireItemEvent(2, { id: 'item-1', type: 'reasoning', summary: ['**Weighing the options**'] }, 'item/started'),
    ])
    await tick()

    expect(container.querySelector('.thinking-label')?.textContent).toBe('Working')
    expect(container.querySelector('.thinking-stream')).toBeNull()
    expect(container.querySelector('.convo .item')).toBeNull()

    replayToSession([
      wireItemEvent(3, { id: 'item-3', type: 'agentMessage', text: 'Use the second option.' }, 'item/started'),
    ])

    await tick()
    frameAt = performance.now()
    for (let index = 0; index < 100 && !transcriptText(container).includes('Use the second option.'); index += 1) {
      frameAt += 40
      const pending = [...frames.values()]
      frames.clear()
      for (const callback of pending) callback(frameAt)
    }
    expect(transcriptText(container)).toContain('Use the second option.')
    expect(container.querySelector('.thinking-label')?.textContent).toBe('Working')
    expect(transcriptText(container)).toContain('Use the second option.')
    expect(transcriptText(container)).not.toContain('Weighing')

    replayToSession([wireEvent(4, 'turn/completed', {})])
    await waitFor(() => expect(container.querySelector('.thinking')).toBeNull())
  })

  it('loads unknown provider item types only in All', async () => {
    app.sessions = [liveSession()]
    const { container, getByRole } = render(ThreadPaneView, { tab: historicalTab({ sessionId: 'session-1' }) })

    replayToSession([wireItemEvent(1, { id: 'item-1', type: 'someFutureCodexItem', note: 'unmapped-marker' })])
    await tick()
    expect(container.querySelector('.convo').textContent).not.toContain('unmapped-marker')

    await fireEvent.click(getByRole('button', { name: 'All' }))
    await waitForDetailStream()
    replayToSession([wireItemEvent(1, { id: 'item-1', type: 'someFutureCodexItem', note: 'unmapped-marker' })])
    await tick()
    expect(container.querySelector('.convo').textContent).toContain('unmapped-marker')
  })
})
