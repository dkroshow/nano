// @vitest-environment jsdom
//
// The dialog card where the operator actually meets it: at a pane's live edge.
// A component test because every failure mode here is about what is mounted —
// a card that never appears for a parked request, one that stays after another
// tab answered it, or an Approve button that posts a verb the schema retired.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import {
  dropSessionStore,
} from '../state.svelte.js'
import { __resetCodexModelsForTest } from './codex_models.js'
import { createFileTranscriptDriver } from '../test_projection.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() {}
}
MockEventSource.instances = []

const emptyTranscript = {
  items: [],
  turns: [],
  page: { hasMoreBefore: false, beforeCursor: null },
  projection: { state: 'ready', epoch: 'tpe_1' },
}

function jsonResponse(body, status = 200) {
  return { ok: status < 400, status, headers: { get: () => null }, json: async () => body, text: async () => JSON.stringify(body) }
}

const commandApproval = (overrides = {}) => ({
  id: '"req-1"',
  method: 'item/commandExecution/requestApproval',
  kind: 'commandApproval',
  threadId: 'native-1',
  turnId: 'turn_1',
  itemId: 'call_1',
  createdAt: '2026-07-30T10:00:00Z',
  params: { command: 'rm -rf build', cwd: '/workspace/nano', threadId: 'native-1' },
  ...overrides,
})

const userInputRequest = (overrides = {}) => ({
  id: '"req-2"',
  method: 'item/tool/requestUserInput',
  kind: 'userInput',
  threadId: 'native-1',
  itemId: 'call_2',
  createdAt: '2026-07-30T10:00:02Z',
  params: {
    questions: [
      {
        id: 'q1',
        header: 'Indentation',
        question: 'Tabs or spaces?',
        options: [
          { label: 'Spaces (Recommended)', description: 'Matches the repo' },
          { label: 'Tabs', description: 'Go style' },
        ],
      },
    ],
  },
  ...overrides,
})

let pendingInteractions
let answered
let answerStatus

function stubFetch() {
  return vi.fn(async (url, init) => {
    const target = String(url)
    if (target.includes('/transcript')) return jsonResponse(emptyTranscript)
    if (target.includes('/api/codex/models')) return jsonResponse({ data: [] })
    if (target.includes('/interactions/')) {
      answered.push({ url: target, body: JSON.parse(init.body) })
      if (answerStatus === 409) return jsonResponse({ error: 'already_resolved' }, 409)
      if (answerStatus !== 200) return jsonResponse({ error: 'answer delivery failed' }, answerStatus)
      return jsonResponse({ answered: true })
    }
    if (target.includes('/state')) {
      return jsonResponse({ sessionId: 'session-1', provider: 'codex', status: 'ready', pendingInteractions })
    }
    return jsonResponse({})
  })
}

function liveSession(overrides = {}) {
  return {
    id: 'session-1', provider: 'codex', threadId: 'native-1', cwd: '/workspace/nano',
    model: 'gpt-5.6-sol', status: 'ready', turnActive: false, ...overrides,
  }
}

const paneTab = (overrides = {}) => ({
  id: 1, catalogId: 'nth_1', sessionId: 'session-1', provider: 'codex',
  nativeId: 'native-1', title: 'Dialogs', ...overrides,
})

// The nano-origin feed events that carry server-owned interaction state.
const feedEvent = (seq, method, params) => ({
  seq, ts: '2026-07-30T10:00:00Z', dir: 'recv', msg: { jsonrpc: '2.0', method, params },
})

function replay(events) {
  for (const event of events) {
    const method = event.msg.method
    if (method === 'nano/interaction/pending') {
      messageDriver.send({ op: 'interaction.upsert', interaction: event.msg.params.interaction })
    } else if (method === 'nano/interaction/resolved') {
      messageDriver.send({ op: 'interaction.remove', requestID: event.msg.params.id })
    }
  }
}

let messageDriver

beforeEach(() => {
  __resetCodexModelsForTest()
  vi.stubGlobal('EventSource', MockEventSource)
  MockEventSource.instances = []
  pendingInteractions = []
  answered = []
  answerStatus = 200
  vi.stubGlobal('fetch', stubFetch())
  app.sessions = [liveSession()]
  explorer.dataset = [{ id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'Dialogs', metadata: {} }]
  localStorage.setItem('nano-view', 'clean')
  messageDriver = createFileTranscriptDriver()
})

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')
  vi.unstubAllGlobals()
})

describe('interaction card', () => {
  it('renders a parked approval a late-opening tab could never replay', async () => {
    // The whole point of server-owned pending state: this tab connects AFTER
    // the request landed, and the wire buffer is bounded.
    pendingInteractions = [commandApproval()]
    const { container } = render(ThreadPaneView, { tab: paneTab() })

    await waitFor(() => expect(container.querySelector('.interaction')).not.toBeNull())
    expect(container.querySelector('.interaction-command').textContent).toBe('rm -rf build')
    expect(container.querySelector('.interaction-detail').textContent).toContain('/workspace/nano')
    expect(container.querySelector('.interaction-kind').textContent).toContain('command')
  })

  it('appears live from the feed nudge for a tab that was already open', async () => {
    const { container } = render(ThreadPaneView, { tab: paneTab() })
    await tick()
    expect(container.querySelector('.interaction')).toBeNull()

    replay([feedEvent(1, 'nano/interaction/pending', { interaction: commandApproval() })])
    await tick()
    expect(container.querySelector('.interaction')).not.toBeNull()
  })

  it('posts a uniform decision and collapses as soon as Nano acknowledges it', async () => {
    pendingInteractions = [commandApproval()]
    const { container, getByRole } = render(ThreadPaneView, { tab: paneTab() })
    await waitFor(() => expect(container.querySelector('.interaction')).not.toBeNull())

    await fireEvent.click(getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(answered).toHaveLength(1))
    expect(answered[0].body.decision).toBe('accept')
    // M1 sent approve/approve_for_session/deny and mapped deny→reject. The
    // server owns the translation now; a provider verb here would mean the
    // browser had drifted back into speaking schema.
    expect(answered[0].body.decision).not.toBe('approve')
    // The request id rides the path encoded — codex ids are JSON-quoted.
    expect(answered[0].url).toContain(encodeURIComponent('"req-1"'))
    await waitFor(() => expect(container.querySelector('.interaction')).toBeNull())
  })

  it('keeps the card available when Nano cannot deliver the answer', async () => {
    answerStatus = 502
    pendingInteractions = [commandApproval()]
    const { container, getByRole } = render(ThreadPaneView, { tab: paneTab() })
    await waitFor(() => expect(container.querySelector('.interaction')).not.toBeNull())

    await fireEvent.click(getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(container.querySelector('.interaction-error')?.textContent).toContain('answer delivery failed'))
    expect(container.querySelector('.interaction')).not.toBeNull()
  })

  it('collapses silently when another tab already answered (409)', async () => {
    answerStatus = 409
    pendingInteractions = [commandApproval()]
    const { container, getByRole } = render(ThreadPaneView, { tab: paneTab() })
    await waitFor(() => expect(container.querySelector('.interaction')).not.toBeNull())

    await fireEvent.click(getByRole('button', { name: 'Deny' }))
    await waitFor(() => expect(container.querySelector('.interaction')).toBeNull())
    // Silent: losing the race is not the operator's problem to read about.
    expect(container.textContent).not.toMatch(/failed|error/i)
  })

  it('drops the card when the server resolves it, wherever the answer came from', async () => {
    pendingInteractions = [commandApproval()]
    const { container } = render(ThreadPaneView, { tab: paneTab() })
    await waitFor(() => expect(container.querySelector('.interaction')).not.toBeNull())

    replay([feedEvent(2, 'nano/interaction/resolved', { id: '"req-1"', outcome: 'accept' })])
    await tick()
    expect(container.querySelector('.interaction')).toBeNull()
  })

  it('renders a question as options with the Recommended badge, and sends the picked answer', async () => {
    pendingInteractions = [userInputRequest()]
    const { container, getByRole } = render(ThreadPaneView, { tab: paneTab() })
    await waitFor(() => expect(container.querySelector('.interaction')).not.toBeNull())

    expect(container.querySelector('.interaction-recommended')).not.toBeNull()
    // Scoped to the card: the composer has a Send of its own, and the two must
    // not be confused for each other.
    const cardSend = () =>
      [...container.querySelectorAll('.interaction-btn')].find((b) => b.textContent.trim() === 'Send')
    // The response shape requires an answer per question, so an unanswered
    // Send would be a lie about what the operator decided.
    expect(cardSend().disabled).toBe(true)

    await fireEvent.click(getByRole('radio', { name: /Spaces/ }))
    await tick()
    expect(cardSend().disabled).toBe(false)

    await fireEvent.click(cardSend())
    await waitFor(() => expect(answered).toHaveLength(1))
    expect(answered[0].body).toEqual({ decision: 'accept', answers: { q1: ['Spaces'] } })
  })

  it('hides a request the app-server has already auto-resolved by timeout', async () => {
    pendingInteractions = [userInputRequest({ expiresAt: new Date(Date.now() - 1000).toISOString() })]
    const { container } = render(ThreadPaneView, { tab: paneTab() })
    // Give /state time to land; the card must still never appear.
    await waitFor(() => expect(container.querySelector('.convo')).not.toBeNull())
    await tick()
    await tick()
    expect(container.querySelector('.interaction')).toBeNull()
  })
})

// Claude's dialogs reach the operator through the SAME card, which is the whole
// reason slice 3 built no second surface. These tests are the proof: a claude
// request with a claude-shaped payload renders and answers correctly with no
// provider branch in the component.
describe('claude dialogs on the shared card', () => {
  const claudeQuestion = (overrides = {}) => ({
    id: 'req-c1',
    method: 'can_use_tool',
    kind: 'userInput',
    threadId: 'native-1',
    createdAt: '2026-07-30T10:00:03Z',
    params: {
      subtype: 'can_use_tool',
      tool_name: 'AskUserQuestion',
      input: {
        questions: [
          {
            question: 'Which files should I touch?',
            header: 'Scope',
            multiSelect: true,
            options: [{ label: 'a.go' }, { label: 'b.go' }, { label: 'c.go' }],
          },
        ],
      },
    },
    ...overrides,
  })

  it('answers a multi-select question with every label the operator picked', async () => {
    pendingInteractions = [claudeQuestion()]
    const { container, getByRole } = render(ThreadPaneView, { tab: paneTab({ provider: 'claude' }) })
    await waitFor(() => expect(container.querySelector('.interaction')).not.toBeNull())

    // Multi-select toggles rather than replaces: picking a second option must
    // not silently discard the first, which is what a radio group would do.
    await fireEvent.click(getByRole('checkbox', { name: /a\.go/ }))
    await fireEvent.click(getByRole('checkbox', { name: /c\.go/ }))
    await tick()
    // The card's own Send, not the composer's — both are on screen.
    await fireEvent.click(container.querySelector('.interaction-btn.accept'))

    await waitFor(() => expect(answered).toHaveLength(1))
    // Keyed by the question TEXT — the CLI has no question ids, and a wrong key
    // produces a successful turn in which nothing was answered.
    expect(answered[0].body.answers).toEqual({ 'Which files should I touch?': ['a.go', 'c.go'] })
  })

  it('de-selects on a second click, so a mistaken pick is recoverable', async () => {
    pendingInteractions = [claudeQuestion()]
    const { container, getByRole } = render(ThreadPaneView, { tab: paneTab({ provider: 'claude' }) })
    await waitFor(() => expect(container.querySelector('.interaction')).not.toBeNull())

    await fireEvent.click(getByRole('checkbox', { name: /a\.go/ }))
    await fireEvent.click(getByRole('checkbox', { name: /b\.go/ }))
    await fireEvent.click(getByRole('checkbox', { name: /a\.go/ }))
    await tick()
    await fireEvent.click(container.querySelector('.interaction-btn.accept'))

    await waitFor(() => expect(answered).toHaveLength(1))
    expect(answered[0].body.answers).toEqual({ 'Which files should I touch?': ['b.go'] })
  })

  it('offers Allow/Deny for a tool permission, with the command shown verbatim', async () => {
    pendingInteractions = [{
      id: 'req-c2',
      method: 'can_use_tool',
      kind: 'toolPermission',
      threadId: 'native-1',
      createdAt: '2026-07-30T10:00:04Z',
      params: { subtype: 'can_use_tool', tool_name: 'Bash', display_name: 'Bash', input: { command: 'rm -rf build' } },
    }]
    const { container, getByRole, queryByRole } = render(ThreadPaneView, { tab: paneTab({ provider: 'claude' }) })
    await waitFor(() => expect(container.querySelector('.interaction')).not.toBeNull())

    expect(container.querySelector('.interaction-command').textContent).toBe('rm -rf build')
    // No "Always allow": claude's control channel has no session-scoped grant,
    // so a button offering one would be a lie about what the click did.
    expect(queryByRole('button', { name: 'Always allow' })).toBeNull()
    await fireEvent.click(getByRole('button', { name: 'Deny' }))
    await waitFor(() => expect(answered[0].body.decision).toBe('decline'))
  })
})
