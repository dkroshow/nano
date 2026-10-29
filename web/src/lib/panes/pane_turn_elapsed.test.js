// @vitest-environment jsdom
//
// The pane's half of the live elapsed. A component test because the whole
// point is what the operator sees on the thinking surface, and because the
// anchor arrives through the SESSION SUMMARY rather than the wire — a path a
// state-level test would happily satisfy while the surface stayed blank.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, waitFor } from '@testing-library/svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { app } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { dropSessionStore } from '../../state.svelte.js'
import { createFileTranscriptDriver, projectionItem } from '../../test_projection.js'

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

const NOW = 1_800_000_000_000

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

function session(overrides = {}) {
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

const tab = { id: 1, catalogId: 'nth_1', sessionId: 'session-1', provider: 'codex', nativeId: 'native-1', title: 'Elapsed' }

function replay(events) {
  for (const event of events) {
    const message = event.msg
    if (message.method === 'turn/started') {
      messageDriver.send({
        op: 'turn.upsert',
        runtime: { turnActive: true, threadStatus: 'working', turnID: message.params.turn?.id ?? 'turn_1', turnStartedAtMs: app.sessions[0]?.turnStartedAtMs ?? 0 },
        turn: { id: message.params.turn?.id ?? 'turn_1', status: 'in_progress' },
      })
    } else if (message.method === 'turn/completed') {
      const turn = message.params.turn ?? {}
      messageDriver.send({
        op: 'turn.upsert',
        runtime: { turnActive: false, threadStatus: 'idle' },
        turn: { id: turn.id, status: turn.status, finalItemID: turn.status === 'completed' ? 'msg_1' : '', durationMs: turn.status === 'completed' ? turn.durationMs ?? 0 : 0 },
      })
    } else if (message.method === 'item/started' || message.method === 'item/completed') {
      const item = message.params.item
      messageDriver.send({
        op: message.method === 'item/started' ? 'item.upsert' : 'item.complete',
        item: projectionItem({ id: item.id, type: item.type, text: item.text ?? '', done: message.method === 'item/completed', item }),
      })
    } else if (message.method === 'nano/interaction/pending') {
      // Nano publishes a parked dialog as needs_you with the card.
      messageDriver.send({ op: 'turn.upsert', runtime: { turnActive: true, threadStatus: 'needs_you', turnID: 'turn_1', turnStartedAtMs: app.sessions[0]?.turnStartedAtMs ?? 0 }, turn: { id: 'turn_1', status: 'in_progress' } })
      messageDriver.send({ op: 'interaction.upsert', interaction: message.params.interaction })
    }
  }
}

let messageDriver

const turnStarted = { seq: 1, ts: '2026-07-31T10:00:00Z', dir: 'recv', msg: { method: 'turn/started', params: { threadId: 'native-1', turn: { id: 'turn_1' } } } }
const reasoning = {
  seq: 2,
  ts: '2026-07-31T10:00:01Z',
  dir: 'recv',
  msg: { method: 'item/started', params: { threadId: 'native-1', item: { id: 'r1', type: 'reasoning' } } },
}
const dialogPending = {
  seq: 3,
  ts: '2026-07-31T10:00:02Z',
  dir: 'recv',
  msg: {
    method: 'nano/interaction/pending',
    params: {
      interaction: {
        id: '7',
        method: 'item/commandExecution/requestApproval',
        kind: 'commandApproval',
        createdAt: '2026-07-31T10:00:02Z',
        params: { command: 'rm -rf /' },
      },
    },
  },
}

beforeEach(() => {
  vi.stubGlobal('EventSource', MockEventSource)
  MockEventSource.instances = []
  localStorage.clear()
  localStorage.setItem('nano-view', 'clean')
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url) => {
      const target = String(url)
      if (target.includes('/transcript')) return jsonResponse(emptyTranscript)
      if (target.includes('/state')) {
        return jsonResponse({ sessionId: 'session-1', provider: 'codex', threadId: 'native-1', status: 'ready' })
      }
      return jsonResponse({})
    }),
  )
  explorer.dataset = [{ id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'Elapsed', metadata: {} }]
  messageDriver = createFileTranscriptDriver()
})

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('the pane while a turn is in flight', () => {
  it('times the turn from the server anchor, not from when the tab opened', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    // The anchor is two minutes old: the turn began long before this pane did,
    // which is exactly what a browser-side Date.now() clock got wrong.
    app.sessions = [session({ turnActive: true, turnStartedAtMs: NOW - 125_000 })]
    const { container } = render(ThreadPaneView, { props: { tab } })
    replay([turnStarted, reasoning])
    await vi.advanceTimersByTimeAsync(0)

    await waitFor(() => expect(container.querySelector('.thinking-elapsed')).not.toBeNull())
    expect(container.querySelector('.thinking-elapsed').textContent).toBe('2m 5s')

    await vi.advanceTimersByTimeAsync(2_000)
    expect(container.querySelector('.thinking-elapsed').textContent).toBe('2m 7s')
  })

  // D2/D8 in the pane: a re-attach that recovered no anchor shows the activity
  // label with no clock rather than starting one from zero.
  it('shows no clock when the server has no anchor', async () => {
    app.sessions = [session({ turnActive: true })]
    const { container } = render(ThreadPaneView, { props: { tab } })
    replay([turnStarted, reasoning])

    await waitFor(() => expect(container.querySelector('.thinking')).not.toBeNull())
    expect(container.querySelector('.thinking-elapsed')).toBeNull()
  })

  // D15: the agent has stopped and is waiting on the operator. A clock still
  // counting up says the opposite of what the card below it is asking for.
  it('replaces the clock with "waiting" while a dialog is parked', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    app.sessions = [session({ turnActive: true, turnStartedAtMs: NOW - 125_000 })]
    const { container } = render(ThreadPaneView, { props: { tab } })
    replay([turnStarted, reasoning])
    await vi.advanceTimersByTimeAsync(0)
    await waitFor(() => expect(container.querySelector('.thinking-elapsed').textContent).toBe('2m 5s'))

    replay([dialogPending])
    await vi.advanceTimersByTimeAsync(0)
    await waitFor(() => expect(container.querySelector('.thinking-elapsed').textContent).toBe('waiting'))
    expect(container.querySelector('.thinking-elapsed').className).toContain('waiting')
  })
})

// The live half's turn parentheticals: the wire's own duration evidence lands
// on the final answer the moment the turn completes — the operator does not
// wait for the durable window to catch up (the operator, 2026-07-31: "you need to build
// the timer for both").
describe('the live turn duration parenthetical', () => {
  const agentAnswer = {
    seq: 4,
    ts: '2026-07-31T10:00:10Z',
    dir: 'recv',
    msg: { method: 'item/completed', params: { threadId: 'native-1', item: { id: 'msg_1', type: 'agentMessage', text: 'done' } } },
  }
  const turnCompleted = (turn) => ({
    seq: 5,
    ts: '2026-07-31T10:00:11Z',
    dir: 'recv',
    msg: { method: 'turn/completed', params: { threadId: 'native-1', turn } },
  })

  it('renders the native duration on the message that ended the turn', async () => {
    app.sessions = [session({ turnActive: true })]
    const { container } = render(ThreadPaneView, { props: { tab } })
    replay([turnStarted, agentAnswer, turnCompleted({ id: 'turn_1', status: 'completed', durationMs: 11_195 })])

    await waitFor(() => expect(container.querySelector('.item.agentMessage .turn-took')).not.toBeNull())
    expect(container.querySelector('.item.agentMessage .turn-took').textContent).toBe('(11s)')
  })

  // Matching the durable rule: an interrupted turn records no duration there
  // (no task_complete fires), so the live half must not invent one either.
  it('renders nothing for an interrupted turn', async () => {
    app.sessions = [session({ turnActive: true })]
    const { container } = render(ThreadPaneView, { props: { tab } })
    replay([turnStarted, agentAnswer, turnCompleted({ id: 'turn_1', status: 'interrupted', durationMs: 11_195 })])

    await waitFor(() => expect(container.querySelector('.item.agentMessage')).not.toBeNull())
    expect(container.querySelector('.item.agentMessage .turn-took')).toBeNull()
  })
})

// Reattach can deliver status before any transcript rows. A file-only pane
// must show Working and the server-anchored clock from that first status.
describe('a re-attached pane before transcript text', () => {
  it('shows Working and the anchored clock from its first file-stream status', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    app.sessions = [session()]
    const { container } = render(ThreadPaneView, { props: { tab } })
    messageDriver.send({ op: 'turn.upsert', runtime: {
      sessionStatus: 'ready', turnActive: true, threadStatus: 'working', turnStartedAtMs: NOW - 65_000,
    }, turn: { id: 'turn_1', status: 'in_progress' } })
    await vi.advanceTimersByTimeAsync(50)
    await waitFor(() => expect(container.querySelector('.thinking')).not.toBeNull())
    expect(container.querySelector('.thinking').textContent).toContain('Working')
    expect(container.querySelector('.thinking-elapsed').textContent).toBe('1m 5s')
    expect(container.querySelector('.convo .item')).toBeNull()
  })
})
