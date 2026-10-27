// @vitest-environment jsdom
//
// Slice 1's operator-facing half: the stop affordance and the per-turn
// model/effort controls. These are component tests because the failure modes
// are all "the state was right, the wrong thing was on screen" — a Stop button
// that never replaces Send while a turn runs, an effort list that empties when
// the model changes, or an override the control shows but the next turn will
// not send.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import { dropSessionStore, getSessionStore } from '../state.svelte.js'
import { __resetCodexModelsForTest } from './codex_models.js'
import { createFileTranscriptDriver } from '../test_projection.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() {}
  open() { this.onopen?.() }
}
MockEventSource.instances = []

const emptyTranscript = {
  items: [],
  turns: [],
  page: { hasMoreBefore: false, beforeCursor: null },
  projection: { state: 'ready', epoch: 'tpe_1' },
}

const MODEL_CATALOG = {
  data: [
    {
      id: 'gpt-5.6-sol',
      model: 'gpt-5.6-sol',
      displayName: 'GPT-5.6 Sol',
      serviceTiers: [{ id: 'priority', name: 'Fast' }],
      supportedReasoningEfforts: [{ reasoningEffort: 'low' }, { reasoningEffort: 'high' }, { reasoningEffort: 'ultra' }],
    },
    {
      id: 'gpt-5.6-luna',
      model: 'gpt-5.6-luna',
      displayName: 'GPT-5.6 Luna',
      supportedReasoningEfforts: [{ reasoningEffort: 'low' }, { reasoningEffort: 'high' }],
    },
  ],
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

function liveSession(overrides = {}) {
  return {
    id: 'session-1',
    provider: 'codex',
    threadId: 'native-1',
    cwd: '/workspace/nano',
    model: 'gpt-5.6-sol',
		posture: { model: 'gpt-5.6-sol', effort: 'high', serviceTier: 'default' },
    status: 'ready',
    turnActive: false,
    ...overrides,
  }
}

function paneTab(overrides = {}) {
  return {
    id: 1,
    catalogId: 'nth_1',
    sessionId: 'session-1',
    provider: 'codex',
    nativeId: 'native-1',
    title: 'Turn controls',
    ...overrides,
  }
}

// turnOptions is server-owned: /state carries it, and every POST answers with
// the new truth. The stub keeps one copy so the component's reads and writes
// meet the same object the server would have.
let turnOptions
let modelCatalog
let postedTurnOptions
let interruptResponse
let stateProvider
let controlInit
let postedTurns
let postedActions
let postedNextTurn
let canceledQueuedActions
let resentActions

function diagnosticEvents() {
  return JSON.parse(sessionStorage.getItem('nano.browser-diagnostics.v1') || '{"events":[]}').events
}
let serverTurnActive
let turnOptionsFailure

function stubFetch() {
  return vi.fn(async (url, init) => {
    const target = String(url)
    if (target.includes('/transcript')) {
	  return jsonResponse(emptyTranscript)
	}
    if (target.includes('/api/codex/models')) return jsonResponse(modelCatalog)
    if (target.includes('/turn-options')) {
      if (turnOptionsFailure) return { ok: false, status: 502, json: async () => ({}), text: async () => 'unavailable' }
      const patch = JSON.parse(init.body)
      postedTurnOptions.push(patch)
      turnOptions = { ...turnOptions, ...patch }
      return jsonResponse(turnOptions)
    }
    if (target.includes('/turn-delivery/') && target.endsWith('/resend')) {
      const actionID = target.split('/').at(-2)
      resentActions.push(actionID)
      return jsonResponse({ clientActionId: `resend-${actionID}`, state: 'queued' })
    }
    if (target.includes('/turn-delivery/') && init?.method === 'DELETE') {
      const actionID = target.split('/').at(-1)
      canceledQueuedActions.push(actionID)
      return jsonResponse({ canceled: true, clientActionId: actionID, state: 'expired' })
    }
    if (target.endsWith('/turn')) {
	  const body = JSON.parse(init.body)
      postedTurns.push(body.text)
	  postedActions.push(body.clientActionId)
	  postedNextTurn.push(body.nextTurn === true)
	  if (serverTurnActive) {
		return {
		  ok: true, status: 202, headers: { get: () => null },
		  json: async () => ({ pending: true, status: 'queued', reason: 'queued_for_next_turn' }),
		  text: async () => '',
		}
	  }
      return jsonResponse({ accepted: true })
    }
    if (target.includes('/state')) {
      return jsonResponse({
        sessionId: 'session-1', provider: stateProvider, threadId: 'native-1', status: 'ready', turnOptions,
		posture: app.sessions[0]?.posture ?? {},
        ...(controlInit ? { controlInit } : {}),
      })
    }
    return jsonResponse({})
  })
}

// Claude has no turn/started: the send opens the turn and the child's result
// line closes it.
const claudeResultLine = (extra = {}) => ({ seq: 2, ts: '2026-07-30T10:00:09Z', dir: 'recv', msg: { type: 'result', subtype: 'success', ...extra } })

const CLAUDE_CONTROL_INIT = {
  models: [
    { value: 'default', resolvedModel: 'claude-opus-5[1m]', displayName: 'Default (recommended)' },
    { value: 'haiku', resolvedModel: 'claude-haiku-4-5', displayName: 'Haiku' },
  ],
}

// Drive the wire the way the server does: turn/started opens a turn and
// carries the id, turn/completed closes it.
function replay(events) {
  if (messageDriver.state.provider !== stateProvider && messageDriver.state.projectionRevision === 0) {
    messageDriver = newMessageDriver(stateProvider)
  }
  for (const event of events) {
    const message = event.msg
    if (message.method === 'turn/started' || message.method === 'turn/completed') {
      const turn = message.params.turn ?? {}
      const active = message.method === 'turn/started'
	  serverTurnActive = active
      messageDriver.send({
        op: 'turn.upsert',
        runtime: { turnActive: active, threadStatus: active ? 'working' : 'idle', turnID: turn.id ?? 'turn_1', turnStartedAtMs: active ? Date.now() : 0 },
        turn: { id: turn.id ?? 'turn_1', status: active ? 'in_progress' : (turn.status ?? 'completed') },
      })
    } else if (message.type === 'result') {
      const aborted = String(message.terminal_reason ?? '').startsWith('aborted')
      messageDriver.send({
        op: 'turn.upsert',
        runtime: {
          turnActive: false,
          ...(message.is_error && !aborted ? { lastError: message.subtype ?? 'turn failed' } : {}),
        },
        turn: { id: 'turn_1', status: aborted ? 'interrupted' : 'completed' },
      })
    }
  }
}

let messageDriver

function newMessageDriver(provider) {
  return createFileTranscriptDriver({ provider })
}

const turnStarted = (id) => ({ seq: 1, ts: '2026-07-30T10:00:00Z', dir: 'recv', msg: { method: 'turn/started', params: { threadId: 'native-1', turn: { id } } } })
const turnCompleted = (id) => ({ seq: 2, ts: '2026-07-30T10:00:09Z', dir: 'recv', msg: { method: 'turn/completed', params: { threadId: 'native-1', turn: { id, status: 'completed' } } } })

beforeEach(() => {
  __resetCodexModelsForTest()
  vi.stubGlobal('EventSource', MockEventSource)
  MockEventSource.instances = []
  turnOptions = { model: '', effort: '', serviceTier: '', summary: 'detailed', threadModel: 'gpt-5.6-sol' }
  modelCatalog = MODEL_CATALOG
  postedTurnOptions = []
  postedTurns = []
  postedActions = []
  postedNextTurn = []
  canceledQueuedActions = []
  resentActions = []
  serverTurnActive = false
  turnOptionsFailure = false
  stateProvider = 'codex'
  controlInit = null
  interruptResponse = { interrupted: true, turnId: 'turn_1' }
  localStorage.clear()
  vi.stubGlobal('fetch', stubFetch())
  app.sessions = [liveSession()]
  explorer.dataset = [{ id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'Turn controls', metadata: {} }]
  localStorage.setItem('nano-view', 'clean')
  messageDriver = newMessageDriver(stateProvider)
})

afterEach(() => {
  vi.useRealTimers()
  cleanup()
  dropSessionStore('session-1')
  vi.unstubAllGlobals()
})

// The selects live behind the info bar's ⋯ menu (2026-07-31): they change the
// next turn, not this one, so they are an option rather than a permanent row.
// The mechanism below them is unchanged — every assertion in this file still
// holds — but reaching them now takes two clicks, which is the point.
async function openTurnControls(result) {
  await fireEvent.click(result.getByRole('button', { name: 'thread menu' }))
  // The entry only exists once /state has said which provider this is — the
  // same asynchrony the catalog wait below covers.
  const entry = await waitFor(() => result.getByRole('button', { name: /^Change model/ }))
  await fireEvent.click(entry)
  await tick()
}

// The controls read their overrides from /state and their choices from the
// model catalog, both asynchronous — wait for the catalog, which lands last.
async function renderPane(props = {}) {
  const result = render(ThreadPaneView, { tab: paneTab(), ...props })
  MockEventSource.instances.find((source) => source.url.includes('/live?'))?.open()
  await openTurnControls(result)
  // Wait for a model only the catalog can supply: an override alone also puts
  // a second option in the select, so counting options would race the fetch.
  await waitFor(() => {
    const values = [...(result.container.querySelector('.turn-controls select')?.options ?? [])].map((o) => o.value)
    expect(values).toContain('gpt-5.6-luna')
  })
  await tick()
  return result
}

describe('live turn controls', () => {
  it('offers Stop instead of Send while a codex turn is running, and Send again after', async () => {
    const { container, getByRole } = await renderPane()
    expect(getByRole('button', { name: 'Send' })).not.toBeNull()

    replay([turnStarted('turn_1')])
    await tick()

    // The regression this guards: turnActive flips, the composer locks, but
    // the only button still says "Working" and there is no way to stop.
	expect(container.querySelector('.composer button.stop')?.textContent).toContain('Stop')
    expect(container.querySelector('.composer button.stop')).not.toBeNull()

    replay([turnCompleted('turn_1')])
    await tick()
    expect(getByRole('button', { name: 'Send' })).not.toBeNull()
  })

  it('keeps ordinary Stop interruption-only', async () => {
    interruptResponse = { interrupted: false, reason: 'turn not found' }
    const { container } = await renderPane()
    replay([turnStarted('turn_1')])
    await tick()

	await fireEvent.click(container.querySelector('.composer button.stop'))
    await waitFor(() =>
	  expect(fetch).toHaveBeenCalledWith('/api/sessions/session-1/interrupt', expect.objectContaining({ method: 'POST' })),
    )
	expect(fetch.mock.calls.some(([url]) => String(url).includes('/transcript-repair'))).toBe(false)
  })


  it('offers Stop on a claude pane too, once its control channel exists', async () => {
    const { container, getByRole, getByPlaceholderText } = await renderClaudePane()
    expect(container.querySelector('.composer button.stop')).toBeNull()

    // Sent the way the operator sends it: claude has no turn/started, so the
    // send itself is what opens the turn.
    await fireEvent.input(getByPlaceholderText('Message the agent'), { target: { value: 'go' } })
    await fireEvent.keyDown(getByPlaceholderText('Message the agent'), { key: 'Enter' })
    await tick()
	expect(container.querySelector('.composer button.stop')?.textContent).toContain('Stop')

    // The interrupted turn's own result line is what clears the state — claude
    // needs no synthesized transition, because an interrupt always emits one.
    replay([claudeResultLine({ is_error: true, subtype: 'error_during_execution', terminal_reason: 'aborted_streaming' })])
    await tick()
    expect(getByRole('button', { name: 'Send' })).not.toBeNull()
    // A deliberate stop is not a failure. The interrupted turn's result line
    // says is_error, so without the terminal_reason check every single Stop
    // would light the error chip.
    expect(getSessionStore('session-1').store.lastError).toBeFalsy()

    // A turn that genuinely failed still reports itself.
    replay([claudeResultLine({ seq: 3, is_error: true, subtype: 'error_max_turns', terminal_reason: 'completed' })])
    await tick()
    expect(getSessionStore('session-1').store.lastError).toBe('error_max_turns')
  })

  it('switches the claude model from the catalog the control channel supplied', async () => {
    const { container } = await renderClaudePane()
    const selects = container.querySelectorAll('.turn-controls select')
    // One control, not two: claude has no control-channel equivalent for
    // effort, so an effort select would be a control that cannot act.
    expect(selects.length).toBe(1)
    expect(selects[0].options[0].textContent).toContain('claude-fable-5')
    expect([...selects[0].options].map((o) => o.value)).toEqual(['', 'default', 'haiku'])

    await fireEvent.change(selects[0], { target: { value: 'haiku' } })
    await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ model: 'haiku' }))
  })

  async function renderClaudePane() {
    stateProvider = 'claude'
    controlInit = CLAUDE_CONTROL_INIT
    turnOptions = { model: '', effort: '', summary: '', threadModel: 'claude-fable-5' }
	app.sessions = [liveSession({ provider: 'claude', model: 'claude-fable-5', posture: { model: 'claude-fable-5' } })]
    const result = render(ThreadPaneView, { tab: paneTab({ provider: 'claude' }) })
    MockEventSource.instances.find((source) => source.url.includes('/live?'))?.open()
    // Nano's first message on every stream is the thread's status.
    messageDriver = newMessageDriver('claude')
    messageDriver.send({ op: 'status', runtime: { turnActive: false, threadStatus: 'idle' } })
    await openTurnControls(result)
    await waitFor(() => {
      const values = [...(result.container.querySelector('.turn-controls select')?.options ?? [])].map((o) => o.value)
      expect(values).toContain('haiku')
    })
    await tick()
    return result
  }
  it('names the thread default rather than leaving the model control blank', async () => {
    const { container } = await renderPane()
    const modelSelect = container.querySelector('.turn-controls select')
    expect(modelSelect.value).toBe('')
    expect(modelSelect.options[0].textContent).toContain('gpt-5.6-sol')
    expect([...modelSelect.options].map((o) => o.value)).toContain('gpt-5.6-luna')
  })

  it('sends the chosen model and effort as per-turn overrides', async () => {
    const { container } = await renderPane()
    const [modelSelect, effortSelect] = container.querySelectorAll('.turn-controls select')

    await fireEvent.change(modelSelect, { target: { value: 'gpt-5.6-luna' } })
    await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ model: 'gpt-5.6-luna' }))
    await tick()

    await fireEvent.change(effortSelect, { target: { value: 'high' } })
    await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ effort: 'high' }))
    await tick()
    expect(effortSelect.value).toBe('high')
  })

  it('changes an existing codex session between Standard and Priority', async () => {
    const { container } = await renderPane()
    const prioritySelect = [...container.querySelectorAll('.turn-controls select')]
      .find((select) => select.getAttribute('aria-label') === 'speed for this session')

    expect(prioritySelect).not.toBeNull()
    expect(prioritySelect.value).toBe('default')

    await fireEvent.change(prioritySelect, { target: { value: 'priority' } })
    await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ serviceTier: 'priority' }))
    await tick()
    expect(prioritySelect.value).toBe('priority')
  })

  it('drops an effort the newly chosen model does not advertise', async () => {
    turnOptions = { model: 'gpt-5.6-sol', effort: 'ultra', summary: 'detailed', threadModel: 'gpt-5.6-sol' }
    const { container } = await renderPane()
    const [modelSelect, effortSelect] = container.querySelectorAll('.turn-controls select')
    expect(effortSelect.value).toBe('ultra')

    // Luna advertises no `ultra`; carrying the override across would have the
    // app-server reject the next turn/start instead of nano fixing it here.
    await fireEvent.change(modelSelect, { target: { value: 'gpt-5.6-luna' } })
    await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ model: 'gpt-5.6-luna', effort: '' }))
    await tick()
    expect(effortSelect.value).toBe('')
  })

  // Catches: GPT-6.1 Sol or GPT-6 Sol/Luna advertised by the app-server but
  // missing, misordered, or carrying the wrong efforts in the bottom-right
  // next-turn control. Boundary: the pane's model/effort selects and the
  // turn-options POST.
  it('offers GPT-6.1 Sol and GPT-6 Sol and Luna for the next turn with their advertised efforts', async () => {
    const efforts = (...names) => names.map((reasoningEffort) => ({ reasoningEffort }))
    modelCatalog = {
      data: [
        { id: 'gpt-6.1-sol', model: 'gpt-6.1-sol', displayName: 'GPT-6.1-Sol', supportedReasoningEfforts: efforts('low', 'medium', 'high', 'xhigh', 'max', 'ultra') },
        { id: 'gpt-6-sol', model: 'gpt-6-sol', displayName: 'GPT-6-Sol', supportedReasoningEfforts: efforts('low', 'medium', 'high', 'xhigh', 'max', 'ultra') },
        { id: 'gpt-6-luna', model: 'gpt-6-luna', displayName: 'GPT-6-Luna', supportedReasoningEfforts: efforts('low', 'medium', 'high', 'xhigh', 'max') },
        ...MODEL_CATALOG.data,
      ],
    }
    const { container } = await renderPane()
    const [modelSelect, effortSelect] = container.querySelectorAll('.turn-controls select')
    const values = [...modelSelect.options].map((o) => o.value).filter(Boolean)
    expect(values.slice(0, 5)).toEqual(['gpt-6.1-sol', 'gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-sol'])
    expect(values).toContain('gpt-5.6-luna')

    await fireEvent.change(modelSelect, { target: { value: 'gpt-6.1-sol' } })
    await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ model: 'gpt-6.1-sol' }))
    await tick()

    await fireEvent.change(modelSelect, { target: { value: 'gpt-6-sol' } })
    await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ model: 'gpt-6-sol' }))
    await tick()
    expect([...effortSelect.options].map((o) => o.value)).toContain('ultra')
    await fireEvent.change(effortSelect, { target: { value: 'ultra' } })
    await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ effort: 'ultra' }))
    await tick()

    // Luna advertises no ultra, so the override is cleared, not carried over.
    await fireEvent.change(modelSelect, { target: { value: 'gpt-6-luna' } })
    await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ model: 'gpt-6-luna', effort: '' }))
    await tick()
    expect([...effortSelect.options].map((o) => o.value)).not.toContain('ultra')
  })

  it('keeps an override the catalog no longer advertises visible instead of silently reselecting', async () => {
    turnOptions = { model: 'gpt-5.4-retired', effort: '', summary: 'detailed', threadModel: 'gpt-5.6-sol' }
    const { container } = await renderPane()
    const modelSelect = container.querySelector('.turn-controls select')
    expect(modelSelect.value).toBe('gpt-5.4-retired')
  })
})


// The composer's while-busy contract, decided in slice 4 and recorded in
// lib/turn_queue.js: a message typed during a turn is QUEUED, not steered into
// the running turn and not refused. These are component tests because the
// failure mode is entirely visual — the state can be perfectly correct while
// the operator stares at a disabled box with nowhere for the text to go.
describe('composer while busy', () => {
  // Operator's command contract: commands change speed intent, never become agent prompts.
  it('toggles each speed and crosses between Fast and Ultrafast without sending agent text', async () => {
    const { getByPlaceholderText } = await renderPane()
    const box = getByPlaceholderText('Message the agent')
    for (const [command, tier] of [
      ['/ultrafast', 'ultrafast'], ['/fast', 'priority'],
      ['/ultrafast', 'ultrafast'], ['/ultrafast', 'default'],
      ['/fast', 'priority'], ['/fast', 'default'],
    ]) {
      await fireEvent.input(box, { target: { value: command } })
      await fireEvent.keyDown(box, { key: 'Enter' })
      await waitFor(() => expect(postedTurnOptions.at(-1)).toEqual({ serviceTier: tier }))
      await waitFor(() => expect(box.value).toBe(''))
    }
    expect(postedTurns).toEqual([])
  })

  it('keeps unsupported /ultrafast editable and never sends it to the agent', async () => {
    turnOptionsFailure = true
    const { getByPlaceholderText } = await renderPane()
    const box = getByPlaceholderText('Message the agent')
    await fireEvent.input(box, { target: { value: '/ultrafast' } })
    await fireEvent.keyDown(box, { key: 'Enter' })
    await waitFor(() => expect(getSessionStore('session-1').store.lastError).toContain('turn options failed'))
    expect(box.value).toBe('/ultrafast')
    expect(postedTurns).toEqual([])
  })

  it('keeps /fast editable when the Priority change is rejected', async () => {
    turnOptionsFailure = true
    const { getByPlaceholderText } = await renderPane()
    const box = getByPlaceholderText('Message the agent')

    await fireEvent.input(box, { target: { value: '/fast' } })
    await fireEvent.keyDown(box, { key: 'Enter' })
    await waitFor(() => expect(box.value).toBe('/fast'))
    expect(postedTurns).toEqual([])
  })

  it('queues instead of locking, shows the queued message, and sends it when the turn ends', async () => {
    const { container, getByRole, getByPlaceholderText } = await renderPane()
    replay([turnStarted('turn_1')])
    await tick()

    const box = getByPlaceholderText('Queue the next message')
    expect(box.disabled).toBe(false)
    await fireEvent.input(box, { target: { value: 'next thing' } })
    await fireEvent.keyDown(box, { key: 'Enter' })
    await tick()

    // The browser has handed the prompt to Nano, which owns the durable queue;
    // it has not started a second provider turn.
    expect(postedTurns).toEqual(['next thing'])
    messageDriver.send({ op: 'item.upsert', item: {
      id: `row:codex:${postedActions[0]}`, type: 'userMessage', text: 'next thing',
      clientActionId: postedActions[0], status: 'queued', done: true,
    } })
    await tick()
    expect(container.querySelector('.pending-send[data-status="queued"]').textContent).toContain('next thing')
    // Stop is still the action that applies to the turn itself.
	expect(container.querySelector('.composer button.stop')?.textContent).toContain('Stop')

    replay([turnCompleted('turn_1')])
	await tick()
	expect(postedTurns).toEqual(['next thing']) // no browser-side drain POST
    messageDriver.send({ op: 'item.complete', item: {
      id: `row:codex:${postedActions[0]}`, type: 'userMessage', text: 'next thing',
      clientActionId: postedActions[0], status: 'confirmed', providerConfirmed: true, done: true,
    } })
    await waitFor(() => expect(container.querySelector('.pending-send[data-status="queued"]')).toBeNull())
  })

  // On a provider that takes running-turn input, Send joins the working turn,
  // as Codex's own app does; holding a message for the next turn stays one
  // visible click away. Losing either control silently changes where a
  // correction lands.
  it('sends into the running turn, and Queue holds a message for the next turn', async () => {
    app.sessions = [liveSession({ runningTurnInput: true })]
    const { getByRole, getByPlaceholderText } = await renderPane()
    replay([turnStarted('turn_1')])
    await tick()

    const box = getByPlaceholderText('Add to the running turn')
    await fireEvent.input(box, { target: { value: 'correction' } })
    await fireEvent.keyDown(box, { key: 'Enter' })
    await tick()
    await fireEvent.input(box, { target: { value: 'after this turn' } })
    await tick()
    await fireEvent.click(getByRole('button', { name: 'Queue for the next turn' }))
    await tick()

    expect(postedTurns).toEqual(['correction', 'after this turn'])
    expect(postedNextTurn).toEqual([false, true])
  })

  it('lets a queued message be taken back before it is sent', async () => {
    const { container, getByPlaceholderText } = await renderPane()
    replay([turnStarted('turn_1')])
    await tick()

    const box = getByPlaceholderText('Queue the next message')
    await fireEvent.input(box, { target: { value: 'never mind' } })
    await fireEvent.keyDown(box, { key: 'Enter' })
    await tick()

    messageDriver.send({ op: 'item.upsert', item: {
      id: `row:codex:${postedActions[0]}`, type: 'userMessage', text: 'never mind',
      clientActionId: postedActions[0], status: 'queued', allowedActions: ['discard'], label: 'Queued for the next turn', done: true,
    } })
    await tick()
    await fireEvent.click(container.querySelector('.pending-send[data-status="queued"] .pending-send-action'))
	await waitFor(() => expect(canceledQueuedActions).toEqual([postedActions[0]]))
    // Cancellation drops the stream entry.
    messageDriver.send({ op: 'item.remove', itemID: `row:codex:${postedActions[0]}` })
    await tick()
    expect(container.querySelector('.pending-send[data-status="queued"]')).toBeNull()
    expect(container.querySelector(`[data-client-action-id="${postedActions[0]}"]`)).toBeNull()

    replay([turnCompleted('turn_1')])
    await tick()
	expect(postedTurns).toEqual(['never mind']) // accepted by Nano, then canceled there
  })

  // A prompt steered into a turn that ended before the provider read it is
  // never saved. Nano holds it as uncertain and its stream offers Resend and
  // Discard under the server's label, also to a reloaded pane. Each reaches
  // Nano's real endpoints: Discard ends it there and returns the text to the
  // composer; Resend asks Nano, which sends the same text as a new prompt.
  for (const action of ['Discard', 'Resend']) {
    it(`offers ${action} for a delivered prompt the provider never saved`, async () => {
      const { container, getByPlaceholderText } = await renderPane()
      messageDriver.send({ op: 'item.upsert', item: {
        id: 'prompt:lost-steer', type: 'userMessage', text: 'no, budget requests go to me', clientActionId: 'lost-steer',
        status: 'uncertain', allowedActions: ['resend', 'discard'], label: "Codex's turn ended without it", done: true,
      } })
      const row = await waitFor(() => {
        const found = container.querySelector('[data-client-action-id="lost-steer"]')
        expect(found).not.toBeNull()
        return found
      })
      expect(row.textContent).toContain("Codex's turn ended without it")
      const buttons = [...row.querySelectorAll('.pending-send-action')]
      expect(buttons.map((button) => button.textContent)).toEqual(['Resend', 'Discard'])

      await fireEvent.click(buttons.find((button) => button.textContent === action))
      await waitFor(() => expect(action === 'Resend' ? resentActions : canceledQueuedActions).toEqual(['lost-steer']))
      // The click is in the flight recorder, so a feedback note proves what
      // happened, and the prompt's words are not.
      const kind = `send.${action.toLowerCase()}`
      await waitFor(() => expect(diagnosticEvents().some((event) => event.kind === kind && event.actionId === 'lost-steer')).toBe(true))
      expect(JSON.stringify(diagnosticEvents())).not.toContain('budget requests')
      if (action === 'Resend') {
        expect(canceledQueuedActions).toEqual([])
        expect(postedTurns).toEqual([])
      } else {
        await waitFor(() => expect(getByPlaceholderText('Message the agent').value).toBe('no, budget requests go to me'))
        expect(postedTurns).toEqual([])
      }
    })
  }

  // Discard in another tab ends the prompt in Nano, and Nano's live text
  // drops it from every pane: its row and controls go here too, with no
  // request of this tab's own.
  it('clears a prompt another tab discarded', async () => {
    const { container } = await renderPane()
    messageDriver.send({ op: 'item.upsert', item: {
      id: 'row:codex:elsewhere', type: 'userMessage', text: 'discarded in another tab', clientActionId: 'elsewhere',
      status: 'parked', allowedActions: ['resend', 'discard'], label: 'Not delivered', done: true,
    } })
    const controls = () => container.querySelector('[data-client-action-id="elsewhere"] .pending-send-actions')
    await waitFor(() => expect(controls()).not.toBeNull())

    messageDriver.send({ op: 'item.remove', itemID: 'row:codex:elsewhere' })
    await waitFor(() => expect(controls()).toBeNull())
    expect(container.textContent).not.toContain('discarded in another tab')
    expect(canceledQueuedActions).toEqual([])
  })

  // Compacting can run for minutes with nothing else on screen moving. The
  // status line says so and times it from the provider's own start, then
  // returns to the turn's own clock the moment the provider says it ended.
  it('shows Compacting… with its own elapsed while the provider compacts', async () => {
    const { container } = await renderPane()
    replay([turnStarted('turn_1')])
    await tick()
    const compacting = (since) => messageDriver.send({
      op: 'turn.upsert',
      runtime: { turnActive: true, threadStatus: 'working', turnID: 'turn_1', turnStartedAtMs: Date.now() - 600_000, compactingSinceMs: since, liveTurnDurations: {} },
      turn: { id: 'turn_1', status: 'in_progress' },
    })
    compacting(Date.now() - 65_000)
    await waitFor(() => expect(container.querySelector('.thinking-label')?.textContent).toBe('Compacting…'))
    expect(container.querySelector('.thinking-elapsed')?.textContent).toMatch(/^1m \d+s$/)
    compacting(0)
    await waitFor(() => expect(container.querySelector('.thinking-label')?.textContent).toBe('Working'))
    expect(container.querySelector('.thinking-elapsed')?.textContent).toMatch(/^10m \d+s$/)
  })

  it('restores a per-thread draft, so switching away mid-sentence loses nothing', async () => {
    localStorage.setItem('nano-draft:codex:native-1', 'half a thought')
    const { getByPlaceholderText } = await renderPane()
    await waitFor(() => expect(getByPlaceholderText('Message the agent').value).toBe('half a thought'))
  })

  it('lets the operator delete a restored draft without rehydrating it', async () => {
    localStorage.setItem('nano-draft:codex:native-1', 'stuck text')
    const { getByPlaceholderText } = await renderPane()
    const box = await waitFor(() => getByPlaceholderText('Message the agent'))
    await waitFor(() => expect(box.value).toBe('stuck text'))

    await fireEvent.input(box, { target: { value: '' } })
    await tick()

    expect(box.value).toBe('')
    expect(localStorage.getItem('nano-draft:codex:native-1')).toBeNull()
  })

  it('clears the stored draft once the message is sent', async () => {
    const { getByPlaceholderText } = await renderPane()
    const box = getByPlaceholderText('Message the agent')
    await fireEvent.input(box, { target: { value: 'ship it' } })
    await tick()
    expect(localStorage.getItem('nano-draft:codex:native-1')).toBe('ship it')

    await fireEvent.keyDown(box, { key: 'Enter' })
    await waitFor(() => expect(postedTurns).toEqual(['ship it']))
    await tick()
    expect(localStorage.getItem('nano-draft:codex:native-1')).toBeNull()
  })
})
