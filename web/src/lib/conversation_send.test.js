// @vitest-environment jsdom
//
// Prompt ownership and failure behavior at the real composer surface. The
// state-level outcomes are covered in state_send.test.js; these pin the
// browser handoff that decides whether text is still a draft or has become one
// action-bearing pending operation.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { tick } from 'svelte'
import { render, cleanup, fireEvent, waitFor, within } from '@testing-library/svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import { dropSessionStore, getSessionStore, syncSessionSummaries } from '../state.svelte.js'
import { createFileTranscriptDriver, projectionSnapshot } from '../test_projection.js'
import { connection, startConnectionChecks } from '../connection.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() {}
  message(update) {
    this.onmessage?.({ data: JSON.stringify(update) })
  }
  open() {
    this.onopen?.()
  }
}
MockEventSource.instances = []

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

// promptRead is Nano's answer to a read of one prompt it has: its stored
// state and presentation, as the journal lists it.
function promptRead(clientActionId, state, label = '') {
  return jsonResponse({ clientActionId, state, ...(label ? { label } : {}) })
}

const notFound = { ok: false, status: 404, headers: { get: () => null }, json: async () => ({ error: { code: 'not_found', message: 'Nano does not have this prompt' } }), text: async () => '' }

const liveSession = { id: 'session-1', provider: 'claude', threadId: 'native-1', cwd: '/w', status: 'ready', turnActive: false }
const liveTab = { id: 1, catalogId: '', sessionId: 'session-1', provider: 'claude', nativeId: 'native-1', title: 'T' }
const DRAFT_KEY = 'nano-draft:claude:native-1'
const FALLBACK_DRAFT_KEY = 'nano-draft:session:session-1'

// stubTurn decides what POST /turn does; every read stays quiet.
function stubTurn(turn) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url, options) => {
      if (String(url).endsWith('/turn')) return turn(options)
      return jsonResponse({})
    }),
  )
}

async function typeAndSend(container, getByRole, text) {
  const projection = MockEventSource.instances.find((source) => source.url.includes('/live?'))
  projection?.open()
  const textarea = container.querySelector('.composer textarea')
  await fireEvent.input(textarea, { target: { value: text } })
  await fireEvent.click(getByRole('button', { name: 'Send' }))
  return textarea
}

beforeEach(() => {
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  localStorage.clear()
  localStorage.setItem('nano-view', 'clean')
  app.sessions = [liveSession]
  explorer.dataset = []
  messageDriver = createFileTranscriptDriver({ provider: 'claude' })
})

let messageDriver

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  vi.useRealTimers()
})

describe('composer send failure surface', () => {
  it('automatically publishes browser-mounted transcript evidence for agents', async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ accepted: true }))
    vi.stubGlobal('fetch', fetchMock)
    const { container } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const session = getSessionStore('session-1')
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [{
      id: 'agent-check-row', type: 'userMessage', category: 'messages',
      text: 'validate without a human toggle', done: true, item: { type: 'userMessage' },
    }], runtime: { sessionStatus: 'ready', turnActive: session.store.turnActive } }) })

    const validationBodies = () => fetchMock.mock.calls
      .filter(([url, options]) => String(url) === '/api/v1/browser-validations' && options?.method === 'POST')
      .map(([, options]) => JSON.parse(options.body))
    await waitFor(() => expect(validationBodies().some((body) =>
      body.status === 'match' && body.expectedRows.some((row) => row.key === 'durable:agent-check-row')
    )).toBe(true))
    expect(container.querySelector('[data-testid="transcript-validator"]')).toBeNull()

    container.querySelector('[data-transcript-key="durable:agent-check-row"]').remove()
    await waitFor(() => expect(validationBodies().some((body) =>
      body.status === 'mismatch' && body.expectedRows.length === 1 && body.mountedRows.length === 0
    )).toBe(true))
    expect(JSON.stringify(validationBodies())).not.toContain('validate without a human toggle')
  })

  it('publishes content-free raw-key and semantic-ID duplicate counts from the mounted component', async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ accepted: true }))
    vi.stubGlobal('fetch', fetchMock)
    const { container } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const session = getSessionStore('session-1')
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [{
      id: 'duplicate-observation-row', type: 'userMessage', category: 'messages',
      text: 'content must stay out of duplicate evidence', done: true, item: { type: 'userMessage' },
    }], runtime: { sessionStatus: 'ready', turnActive: session.store.turnActive } }) })

    const validationBodies = () => fetchMock.mock.calls
      .filter(([url, options]) => String(url) === '/api/v1/browser-validations' && options?.method === 'POST')
      .map(([, options]) => JSON.parse(options.body))
    await waitFor(() => expect(validationBodies().some((body) => body.status === 'match')).toBe(true))

    const row = container.querySelector('[data-transcript-key="durable:duplicate-observation-row"]')
    const rawDuplicate = row.cloneNode(true)
    row.parentElement.append(rawDuplicate)
    await waitFor(() => expect(validationBodies().some((body) =>
      body.duplicateRenderKeyCount === 1 && body.duplicateSemanticIdCount === 1
    )).toBe(true))

    rawDuplicate.remove()
    row.dataset.transcriptSemanticId = 'canonical-message-1'
    const semanticDuplicate = row.cloneNode(true)
    semanticDuplicate.dataset.transcriptKey = 'live:semantic-alias'
    semanticDuplicate.dataset.transcriptId = 'semantic-alias'
    row.parentElement.append(semanticDuplicate)
    await waitFor(() => expect(validationBodies().some((body) =>
      body.duplicateRenderKeyCount === 0 && body.duplicateSemanticIdCount === 1
    )).toBe(true))

    expect(JSON.stringify(validationBodies())).not.toContain('content must stay out of duplicate evidence')
  })

  it('shows when the intended transcript and mounted browser rows diverge', async () => {
    localStorage.setItem('nano:transcript-validator', 'on')
    stubTurn(() => new Promise(() => {}))
    const { container } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const session = getSessionStore('session-1')
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [{
      id: 'visible-user-row', type: 'userMessage', category: 'messages',
      text: 'a visible prompt', done: true, item: { type: 'userMessage' },
    }], runtime: { sessionStatus: 'ready', turnActive: session.store.turnActive } }) })

    await waitFor(() => expect(container.querySelector('[data-testid="transcript-validator"]')?.textContent)
      .toContain('Render matches'))
    const row = container.querySelector('[data-transcript-key="durable:visible-user-row"]')
    expect(row?.textContent).toContain('a visible prompt')

    row.remove()
    await waitFor(() => expect(container.querySelector('[data-testid="transcript-validator"]')?.textContent)
      .toContain('Render mismatch'))
    expect(container.querySelector('[data-testid="transcript-validator"]')?.textContent).toContain('missing 1')
  })

  it('does not resurrect a session-keyed draft once stable thread identity is known', async () => {
    localStorage.setItem(FALLBACK_DRAFT_KEY, 'already submitted')
    stubTurn(() => new Promise(() => {}))

    const { container } = render(ThreadPaneView, { tab: liveTab })

    await waitFor(() => expect(container.querySelector('.composer textarea').value).toBe(''))
    expect(localStorage.getItem(FALLBACK_DRAFT_KEY)).toBeNull()
  })

  it('submits through healthy HTTP while the projection stream reconnects', async () => {
    const fetchMock = vi.fn(async (url) => {
      if (String(url).endsWith('/turn')) return jsonResponse({ accepted: true })
      return jsonResponse({})
    })
    vi.stubGlobal('fetch', fetchMock)
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const textarea = container.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'send during reconnect' } })

    await fireEvent.click(getByRole('button', { name: 'Send' }))

    await waitFor(() => expect(
      fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/turn'))
    ).toHaveLength(1))
    expect(textarea.value).toBe('')
    expect(textarea.classList.contains('send-blocked')).toBe(false)
  })

  it('keeps an ambiguous send out of the draft so a second Enter cannot duplicate it', async () => {
    const fetchMock = vi.fn(async (url) => {
      if (String(url).endsWith('/turn')) throw new Error('connection dropped')
      return jsonResponse({})
    })
    vi.stubGlobal('fetch', fetchMock)
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])

    const textarea = await typeAndSend(container, getByRole, 'submit this exactly once')
    await waitFor(() => expect(container.querySelector('.send-notice')).not.toBeNull())
    expect(textarea.value).toBe('')
    expect(localStorage.getItem(DRAFT_KEY)).toBeNull()

    await fireEvent.keyDown(textarea, { key: 'Enter' })

    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/turn'))).toHaveLength(1)
    expect(container.querySelectorAll('.unsent-send')).toHaveLength(1)
  })

  // An operator's Feedback note on a Codex thread mid-turn: "red dot is
  // showing as a pending prompt, but an agent message comes after that. not
  // possible". The steer was accepted by Codex and the agent kept answering
  // below it while its saved row lagged; the pane also counted two prompts.
  // A prompt the provider has with agent output below it carries no dot, a
  // saved one carries none whatever this browser still holds for it, and an
  // agent's addendum queued for the next turn says so, with a hollow dot and
  // Discard only. The count is the rows that need the person, as the
  // Feedback note reports it.
  it('shows no dot on a prompt being answered or saved, and a queued addendum as queued', async () => {
    const posts = []
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      if (options?.method === 'POST') posts.push({ url: String(url), body: JSON.parse(options.body) })
      if (String(url) === '/api/v1/browser-diagnostics') return jsonResponse({ bootId: 'boot-1' })
      if (String(url) === '/api/v1/feedback') return { ...jsonResponse({ id: 'fb_1', createdAt: '2026-10-14T12:52:53Z' }), status: 201 }
      return jsonResponse({ operations: [] })
    }))
    const codexSession = { ...liveSession, provider: 'codex', turnActive: true, runningTurnInput: true }
    app.sessions = [codexSession]
    messageDriver = createFileTranscriptDriver({ provider: 'codex' })
    // What this browser still held for the steer: its record from a send
    // whose acknowledgement was lost, although Nano had taken it.
    localStorage.setItem('nano-unsent:steer-67b2', JSON.stringify({
      clientActionId: 'steer-67b2', provider: 'codex', threadId: 'native-1', sessionId: 'session-1',
      text: 'Addendum to task 25: two small items', createdAt: Date.now(),
    }))
    const { container, getByRole, getByLabelText } = render(ThreadPaneView, { tab: { ...liveTab, provider: 'codex' } })
    syncSessionSummaries([codexSession])
    const turn = { sessionStatus: 'ready', turnActive: true }
    const saved = { id: 'saved-reply', type: 'agentMessage', text: 'The main read sources are now instrumented.', done: true, nativeItemID: 'msg_1' }
    const steer = { id: 'prompt:steer-67b2', type: 'userMessage', text: 'Addendum to task 25: two small items', clientActionId: 'steer-67b2', status: 'accepted', done: true }
    const reply = { id: 'reply:msg_2', type: 'agentMessage', text: 'That matches the change underway', done: false }
    const addendum = { id: 'prompt:agent-2a60', type: 'userMessage', text: 'Yes: one short signed private window', clientActionId: 'agent-2a60',
      status: 'queued', allowedActions: ['discard'], label: 'Queued for the next turn', done: true }
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'codex', items: [saved, steer, reply, addendum], runtime: turn }) })

    const row = (actionID) => container.querySelector(`[data-client-action-id="${actionID}"]`)
    const buttons = (element) => [...(element?.querySelectorAll('button') ?? [])].map((button) => button.textContent.trim())
    await waitFor(() => expect(row('agent-2a60')).not.toBeNull())
    expect(row('steer-67b2')).not.toBeNull()
    expect(row('steer-67b2').querySelector('.pending-user-dot')).toBeNull()
    expect(buttons(row('steer-67b2'))).toEqual([])
    expect(container.querySelector('.unsent-send')).toBeNull()
    expect(row('agent-2a60').textContent).toContain('Queued for the next turn')
    expect(row('agent-2a60').querySelector('.pending-user-dot.queued')).not.toBeNull()
    expect(buttons(row('agent-2a60'))).toEqual(['Discard'])

    // The steer's saved row lands; the stale record still names it.
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'codex', items: [
      saved, { ...steer, durableItemID: 'saved-steer' }, reply, addendum,
    ], runtime: turn }) })
    await waitFor(() => expect(container.querySelector('[data-transcript-key="durable:saved-steer"]')).not.toBeNull())
    expect([...container.querySelectorAll('.pending-user-dot')].map((dot) => dot.closest('[data-client-action-id]')?.dataset.clientActionId)).toEqual(['agent-2a60'])
    expect(container.querySelector('.unsent-send')).toBeNull()

    await fireEvent.click(getByRole('button', { name: 'Feedback' }))
    await fireEvent.input(getByLabelText('Feedback note'), { target: { value: 'count check' } })
    await fireEvent.click(getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(posts.some((post) => post.url === '/api/v1/feedback')).toBe(true))
    expect(posts.find((post) => post.url === '/api/v1/feedback').body.pane.pendingPrompts).toBe(1)
  })

  // The pane's count is the rows that need the person, as a Feedback note
  // reports it (an operator saw "count 5, rows 2"). It must equal what is
  // rendered after a reload and after Nano restarts and the stream sends a
  // fresh tail.
  it('counts exactly the rendered prompts that need the person, after a reload and a reconnect', async () => {
    const posts = []
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      if (options?.method === 'POST') posts.push({ url: String(url), body: JSON.parse(options.body) })
      if (String(url) === '/api/v1/browser-diagnostics') return jsonResponse({ bootId: 'boot-1' })
      if (String(url) === '/api/v1/feedback') return { ...jsonResponse({ id: 'fb_1', createdAt: '2026-10-14T13:00:00Z' }), status: 201 }
      return jsonResponse({ operations: [] })
    }))
    localStorage.setItem('nano-unsent:never-acked', JSON.stringify({
      clientActionId: 'never-acked', provider: 'claude', threadId: 'native-1', sessionId: 'session-1', text: 'lost on the wire', createdAt: Date.now(),
    }))
    const prompt = (id, status, extra = {}) => ({ id: `prompt:${id}`, type: 'userMessage', text: `prompt ${id}`, clientActionId: id, status, done: true, ...extra })
    const before = [
      prompt('sending', 'delivering'),
      prompt('taken', 'accepted'),
      prompt('next', 'queued', { allowedActions: ['discard'], label: 'Queued for the next turn' }),
      prompt('unsure', 'uncertain', { allowedActions: ['resend', 'discard'], label: 'Delivery not confirmed' }),
    ]
    const counted = async (view) => {
      posts.length = 0
      await fireEvent.click(view.getByRole('button', { name: 'Feedback' }))
      await fireEvent.input(view.getByLabelText('Feedback note'), { target: { value: 'count' } })
      await fireEvent.click(view.getByRole('button', { name: 'Save' }))
      await waitFor(() => expect(posts.some((post) => post.url === '/api/v1/feedback')).toBe(true))
      return posts.find((post) => post.url === '/api/v1/feedback').body.pane.pendingPrompts
    }
    const rendered = (view) => view.container.querySelectorAll('.pending-send, .unsent-send').length

    const first = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: before, runtime: { sessionStatus: 'ready' } }) })
    await waitFor(() => expect(first.container.querySelector('[data-client-action-id="unsure"]')).not.toBeNull())
    expect(rendered(first)).toBe(4)
    expect(await counted(first)).toBe(4)

    // Reload: a fresh store and component over the same browser storage.
    cleanup()
    dropSessionStore('session-1')
    const second = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: before, runtime: { sessionStatus: 'ready' } }) })
    await waitFor(() => expect(second.container.querySelector('[data-client-action-id="unsure"]')).not.toBeNull())
    expect(rendered(second)).toBe(4)
    expect(await counted(second)).toBe(4)

    // Nano restarts: the stream drops, reconnects and gets a fresh tail in
    // which the provider took the sending prompt and Discard ended another.
    MockEventSource.instances.filter((source) => source.url.includes('/live?')).forEach((source) => source.onerror?.())
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [
      prompt('sending', 'accepted'), prompt('taken', 'accepted'), before[2],
    ], runtime: { sessionStatus: 'ready' } }) })
    await waitFor(() => expect(second.container.querySelector('[data-client-action-id="unsure"]')).toBeNull())
    expect(rendered(second)).toBe(2)
    expect(await counted(second)).toBe(2)
  })

  // Resend and Discard are the server's to offer: a row shows exactly the
  // actions its stored state allows, under the server's label, and none on a
  // prompt Nano is still handing over or the provider has.
  it('offers Resend and Discard only where the server allows them', async () => {
    stubTurn(() => new Promise(() => {}))
    const { container } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const prompt = (id, status, extra = {}) => ({ id: `prompt:${id}`, type: 'userMessage', text: `prompt ${id}`, clientActionId: id, status, done: true, ...extra })
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [
      prompt('sending', 'delivering'),
      prompt('taken', 'accepted'),
      prompt('stopped', 'uncertain', { allowedActions: ['resend', 'discard'], label: 'Stopped before Claude took it' }),
      prompt('parked', 'parked', { allowedActions: ['resend', 'discard'], label: 'Not delivered' }),
    ], runtime: { sessionStatus: 'ready' } }) })
    const row = (id) => container.querySelector(`[data-client-action-id="${id}"]`)
    const buttons = (id) => [...row(id).querySelectorAll('button')].map((button) => button.textContent.trim())
    await waitFor(() => expect(row('parked')).not.toBeNull())
    expect(buttons('sending')).toEqual([])
    expect(row('sending').querySelector('.pending-user-dot')?.getAttribute('title')).toBe('On its way to Claude')
    expect(buttons('taken')).toEqual([])
    expect(buttons('stopped')).toEqual(['Resend', 'Discard'])
    expect(row('stopped').textContent).toContain('Stopped before Claude took it')
    expect(buttons('parked')).toEqual(['Resend', 'Discard'])
    expect(row('parked').textContent).toContain('Not delivered')
  })

  // The buttons are the server's allowedActions and nothing else (not a rule
  // of the browser's own by state), and pressing one asks Nano, which decides
  // from the prompt's stored state. Resend of a send whose delivery timed out
  // asks no question: the row's label is the warning, and Nano sends it once.
  // A refusal is shown in Nano's words, and hands no text back.
  it('acts only as the server allows, asks Nano once, and shows its refusal word for word', async () => {
    const requests = []
    const refusal = "Claude already has this prompt, so it can't be discarded."
    vi.stubGlobal('confirm', vi.fn(() => false))
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      const u = String(url)
      if (u.includes('/turn')) requests.push(`${options?.method ?? 'GET'} ${u}`)
      if (u.endsWith('/turn-delivery/slow/resend')) return jsonResponse({ clientActionId: 'resend-slow', state: 'queued' })
      if (u.endsWith('/turn-delivery/stale') && options?.method === 'DELETE') {
        return { ok: false, status: 409, headers: { get: () => null }, text: async () => JSON.stringify({
          error: { code: 'prompt_action_refused', message: refusal, details: { clientActionId: 'stale', state: 'accepted' } },
        }) }
      }
      return jsonResponse({})
    }))
    const { container } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const prompt = (id, status, extra = {}) => ({ id: `prompt:${id}`, type: 'userMessage', text: `prompt ${id}`, clientActionId: id, status, done: true, ...extra })
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [
      prompt('slow', 'uncertain', { allowedActions: ['resend', 'discard'], label: "Claude hasn't answered in 30 s; resending may deliver twice" }),
      prompt('discard-only', 'uncertain', { allowedActions: ['discard'], label: 'Delivery not confirmed' }),
      prompt('none', 'parked', { allowedActions: [], label: 'Not delivered' }),
      prompt('stale', 'accepted', { allowedActions: ['discard'] }),
    ], runtime: { sessionStatus: 'ready' } }) })
    const row = (id) => container.querySelector(`[data-client-action-id="${id}"]`)
    const button = (id, name) => [...row(id).querySelectorAll('button')].find((b) => b.textContent.trim() === name)
    const buttons = (id) => [...row(id).querySelectorAll('button')].map((b) => b.textContent.trim())
    await waitFor(() => expect(row('stale')).not.toBeNull())
    expect(buttons('slow')).toEqual(['Resend', 'Discard'])
    expect(row('slow').textContent).toContain("Claude hasn't answered in 30 s; resending may deliver twice")
    expect(buttons('discard-only')).toEqual(['Discard'])
    expect(buttons('none')).toEqual([])
    expect(buttons('stale')).toEqual(['Discard'])

    await fireEvent.click(button('slow', 'Resend'))
    await waitFor(() => expect(requests).toEqual(['POST /api/sessions/session-1/turn-delivery/slow/resend']))
    expect(window.confirm).not.toHaveBeenCalled()

    await fireEvent.click(button('stale', 'Discard'))
    await waitFor(() => expect(container.querySelector('.send-notice')?.textContent.trim()).toBe(refusal))
    expect(requests).toEqual(['POST /api/sessions/session-1/turn-delivery/slow/resend', 'DELETE /api/sessions/session-1/turn-delivery/stale'])
    expect(container.querySelector('.composer textarea').value).toBe('')
    expect(container.querySelector('.unsent-send')).toBeNull()
  })

  // The person sees their prompt the moment they press Enter, with its pending
  // dot, whatever the network does; the stream's row takes its place.
  it('shows a sent prompt with its dot at once, then the stream\'s row in its place', async () => {
    let actionID = ''
    stubTurn((options) => {
      actionID = JSON.parse(options.body).clientActionId
      return new Promise(() => {})
    })
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'show this prompt once')
    await waitFor(() => expect(actionID).not.toBe(''))
    const matchingRows = () => [...container.querySelectorAll('.convo .item')]
      .filter((row) => row.textContent.includes('show this prompt once'))
    await waitFor(() => expect(matchingRows()).toHaveLength(1))
    expect(container.querySelector(`[data-client-action-id="${actionID}"] .pending-user-dot`)).not.toBeNull()

    messageDriver.send({ op: 'item.upsert', item: {
      id: `row:claude:${actionID}`, type: 'userMessage', category: 'messages',
      text: 'show this prompt once', clientActionId: actionID, status: 'delivering',
      item: { type: 'userMessage' },
    } })

    await waitFor(() => expect(container.querySelector(`[data-client-action-id="${actionID}"]`)?.dataset.transcriptId)
      .toBe(`row:claude:${actionID}`))
    expect(matchingRows()).toHaveLength(1)
    expect(container.querySelector(`[data-client-action-id="${actionID}"] .pending-user-dot`)).not.toBeNull()
  })

  // Nano can answer the POST before its stream lists the prompt: the row
  // must not blink out between the two.
  it('keeps a sent prompt on screen when Nano answers before its stream lists it', async () => {
    let actionID = ''
    stubTurn((options) => {
      actionID = JSON.parse(options.body).clientActionId
      return Promise.resolve({ ok: true, status: 202, headers: { get: () => null }, json: async () => ({ clientActionId: actionID }), text: async () => '' })
    })
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'answered before listed')
    const session = getSessionStore('session-1')
    await waitFor(() => expect(session.store.sending).toBe(0))
    const matchingRows = () => [...container.querySelectorAll('.convo .item')]
      .filter((row) => row.textContent.includes('answered before listed'))
    expect(matchingRows()).toHaveLength(1)
    expect(container.querySelector(`[data-client-action-id="${actionID}"] .pending-user-dot`)).not.toBeNull()

    messageDriver.send({ op: 'item.upsert', item: {
      id: `row:claude:${actionID}`, type: 'userMessage', category: 'messages',
      text: 'answered before listed', clientActionId: actionID, status: 'accepted',
      item: { type: 'userMessage' },
    } })
    await waitFor(() => expect(container.querySelector(`[data-client-action-id="${actionID}"]`)?.dataset.transcriptId)
      .toBe(`row:claude:${actionID}`))
    expect(matchingRows()).toHaveLength(1)
  })

  // The stream's word ends an echo. A prompt Nano finds already saved when it
  // admits it is never listed live: the update that brings its saved row names
  // it saved, and that alone ends the browser's copy.
  const savedRow = (text) => ({ id: 'saved-user', type: 'user_message', category: 'messages', role: 'user',
    projectedSeq: 1, text, createdAt: new Date().toISOString() })
  const liveStream = () => MockEventSource.instances.find((source) => source.url.includes('/live?'))
  const answered202 = (actionID) => ({ ok: true, status: 202, headers: { get: () => null },
    json: async () => ({ clientActionId: actionID }), text: async () => '' })

  it('ends a sent prompt\'s row when an update names it saved without ever listing it', async () => {
    let actionID = ''
    stubTurn((options) => {
      actionID = JSON.parse(options.body).clientActionId
      return Promise.resolve(answered202(actionID))
    })
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'saved before listed')
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).not.toBeNull()

    liveStream().message({ type: 'update', rows: [savedRow('saved before listed')], live: [], saved: [`prompt:${actionID}`] })

    const matchingRows = () => [...container.querySelectorAll('.convo .item')]
      .filter((row) => row.textContent.includes('saved before listed'))
    await waitFor(() => expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).toBeNull())
    expect(matchingRows()).toHaveLength(1)
    expect(container.querySelector('.pending-user-dot')).toBeNull()
  })

  // A tail that names the prompt saved arrives while the POST is still out;
  // the POST answering afterwards must not bring the row back.
  it('ends a sent prompt\'s row on a tail that names it saved, before its POST answers', async () => {
    let actionID = ''
    let answer = null
    stubTurn((options) => {
      actionID = JSON.parse(options.body).clientActionId
      return new Promise((resolve) => { answer = resolve })
    })
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'tail before answer')
    await waitFor(() => expect(answer).toBeTypeOf('function'))

    liveStream().message({ type: 'tail', threadId: 'nth_1', key: 'k', rows: [savedRow('tail before answer')], live: [],
      saved: [`prompt:${actionID}`], turns: [], page: null, projection: { key: 'k', state: 'ready' } })
    await waitFor(() => expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).toBeNull())
    answer(answered202(actionID))
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))

    const matchingRows = [...container.querySelectorAll('.convo .item')]
      .filter((row) => row.textContent.includes('tail before answer'))
    expect(matchingRows).toHaveLength(1)
    expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).toBeNull()
  })

  // Only the stream's evidence ends an echo: a tail that does not mention a
  // send, and a later send changing what the pane shows, leave it in place.
  it('keeps an acknowledged prompt\'s row when a tail omits it and a later send follows', async () => {
    const answers = []
    const actions = []
    stubTurn((options) => {
      actions.push(JSON.parse(options.body).clientActionId)
      return new Promise((resolve) => { answers.push(resolve) })
    })
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'first prompt A')
    await waitFor(() => expect(answers).toHaveLength(1))
    liveStream().message({ type: 'tail', threadId: 'nth_1', key: 'k', rows: [], live: [], saved: [], turns: [], page: null,
      projection: { key: 'k', state: 'ready' } })
    answers[0](answered202(actions[0]))
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))

    void getSessionStore('session-1').sendTurn('second prompt B', { clientActionId: 'action-b' })
    await waitFor(() => expect(answers).toHaveLength(2))
    expect(container.querySelector(`[data-client-action-id="${actions[0]}"]`)).not.toBeNull()
    expect(container.querySelector(`[data-client-action-id="${actions[1]}"]`)).not.toBeNull()
  })

  // A stream away long enough for Nano to settle the prompt and forget it (the
  // live loop restarts when idle) reconnects to a tail that neither lists nor
  // names it. Nano's durable record decides: one read once Nano has the send.
  for (const [state, ends] of [['confirmed', true], ['accepted', false]]) {
    it(`${ends ? 'ends' : 'keeps'} an acknowledged prompt's row after a silent reconnect when its record says ${state}`, async () => {
      let actionID = ''
      const reads = []
      vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
        const u = String(url)
        if (u.endsWith('/turn')) {
          actionID = JSON.parse(options.body).clientActionId
          return answered202(actionID)
        }
        if (u.includes('/turn-delivery/') && !options.method) {
          reads.push(decodeURIComponent(u.split('/').at(-1)))
          return promptRead(actionID, state)
        }
        return jsonResponse({})
      }))
      const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
      syncSessionSummaries([liveSession])
      await typeAndSend(container, getByRole, 'settled while away')
      await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))

      liveStream().message({ type: 'tail', threadId: 'nth_1', key: 'k', rows: ends ? [savedRow('settled while away')] : [],
        live: [], saved: [], turns: [], page: null, projection: { key: 'k', state: 'ready' } })

      await waitFor(() => expect(reads).toEqual([actionID]))
      if (ends) {
        await waitFor(() => expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).toBeNull())
        expect([...container.querySelectorAll('.convo .item')].filter((row) => row.textContent.includes('settled while away'))).toHaveLength(1)
      } else {
        await new Promise((resolve) => setTimeout(resolve, 20))
        expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).not.toBeNull()
      }
    })
  }

  // The reconnect can come while the POST is still out: Nano's answer then
  // brings the same one read.
  it('ends a prompt\'s row when its POST answers after a silent tail and its record says it settled', async () => {
    let actionID = ''
    let answer = null
    const reads = []
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        actionID = JSON.parse(options.body).clientActionId
        return new Promise((resolve) => { answer = resolve })
      }
      if (u.includes('/turn-delivery/') && !options.method) {
        reads.push(decodeURIComponent(u.split('/').at(-1)))
        return promptRead(actionID, 'confirmed')
      }
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'answered after a silent tail')
    await waitFor(() => expect(answer).toBeTypeOf('function'))
    liveStream().message({ type: 'tail', threadId: 'nth_1', key: 'k', rows: [savedRow('answered after a silent tail')],
      live: [], saved: [], turns: [], page: null, projection: { key: 'k', state: 'ready' } })
    expect(reads).toEqual([])
    answer(answered202(actionID))

    await waitFor(() => expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).toBeNull())
    expect(reads).toEqual([actionID])
  })

  // Nano names every prompt whose saved row is in a tail's page, so the tail
  // that installs a send's saved row also ends the browser's copy: no frame
  // shows both, and no read is needed.
  it('shows one row the moment a reconnect tail installs a settled prompt and names it', async () => {
    let actionID = ''
    const reads = []
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        actionID = JSON.parse(options.body).clientActionId
        return answered202(actionID)
      }
      if (u.includes('/turn-delivery/') && !options.method) {
        reads.push(actionID)
        return new Promise(() => {})
      }
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'one row at once')
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).not.toBeNull()

    liveStream().message({ type: 'tail', threadId: 'nth_1', key: 'k', rows: [savedRow('one row at once')], live: [],
      saved: [`prompt:${actionID}`], turns: [], page: null, projection: { key: 'k', state: 'ready' } })
    await tick()

    expect([...container.querySelectorAll('.convo .item')].filter((row) => row.textContent.includes('one row at once'))).toHaveLength(1)
    expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).toBeNull()
    expect(reads).toEqual([])
  })

  // A read can be overtaken: the first silent tail's read is still out, its
  // stale 'accepted' answer delayed, when the stream drops, Nano settles the
  // prompt, and a fresh silent tail arrives. The newer tail must get a read of
  // its own; the stale answer must not end the matter.
  it('reads again for a newer silent tail while an older read is out, and ends the row on the newer answer', async () => {
    let actionID = ''
    let state = 'accepted'
    const reads = []
    const held = []
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        actionID = JSON.parse(options.body).clientActionId
        return answered202(actionID)
      }
      if (u.includes('/turn-delivery/') && !options.method) {
        const answer = promptRead(actionID, state)
        reads.push(state)
        if (reads.length === 1) return new Promise((resolve) => held.push(() => resolve(answer)))
        return answer
      }
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'overtaken read')
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    const silentTail = (rows) => liveStream().message({ type: 'tail', threadId: 'nth_1', key: 'k', rows, live: [], saved: [],
      turns: [], page: null, projection: { key: 'k', state: 'ready' } })

    silentTail([])
    await waitFor(() => expect(held).toHaveLength(1))
    state = 'confirmed'
    silentTail([savedRow('overtaken read')])
    held[0]()

    await waitFor(() => expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).toBeNull())
    expect(reads).toEqual(['accepted', 'confirmed'])
    expect([...container.querySelectorAll('.convo .item')].filter((row) => row.textContent.includes('overtaken read'))).toHaveLength(1)
  })

  // Plane Wi-Fi took about 9.5 s just to reach Nano, and a 15 s abort cancelled
  // sends that were still arriving. After 15 s the row says it is still
  // sending, and the request stays open until a much longer deadline.
  it('keeps a slow send\'s request open past 15 s and says it is still sending', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let signal = null
    let answer = null
    let posts = 0
    let actionID = ''
    stubTurn((options) => {
      posts += 1
      signal = options.signal
      actionID = JSON.parse(options.body).clientActionId
      return new Promise((resolve) => { answer = resolve })
    })
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'over plane wifi')
    await waitFor(() => expect(answer).toBeTypeOf('function'))
    await vi.advanceTimersByTimeAsync(16_000)
    expect(signal.aborted).toBe(false)
    expect(container.querySelector(`[data-client-action-id="${actionID}"] .prompt-label`)?.textContent).toBe('Still sending')
    await vi.advanceTimersByTimeAsync(40_000)
    expect(signal.aborted).toBe(false)
    expect(posts).toBe(1)

    answer(answered202(actionID))
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    expect(posts).toBe(1)
    expect(container.querySelector(`[data-client-action-id="${actionID}"] .prompt-label`)).toBeNull()
  })

  it('does not keep a restored sending row once its provider message is already present', async () => {
    localStorage.setItem('nano-pending:claude:native-1', JSON.stringify([{
      clientActionId: 'restored-row', provider: 'claude', threadId: 'native-1',
      sessionId: 'old-session', text: 'already in the transcript', normalizedText: 'already in the transcript',
      status: 'sending', createdAt: Date.now(), updatedAt: Date.now(),
    }]))
    stubTurn(() => new Promise(() => {}))
    const { container } = render(ThreadPaneView, { tab: liveTab })
    const session = getSessionStore('session-1')
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [{
      id: 'provider-user', type: 'userMessage', text: 'already in the transcript', createdAt: new Date().toISOString(),
    }], runtime: { sessionStatus: 'ready', turnActive: session.store.turnActive } }) })

    syncSessionSummaries([liveSession])

    await waitFor(() => expect(container.querySelector('.pending-send')).toBeNull())
  })

  it('does not paint operation state beside a saved user row', async () => {
    stubTurn(() => new Promise(() => {}))
    const { container } = render(ThreadPaneView, { tab: liveTab })
    const session = getSessionStore('session-1')
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [{
      id: 'provider-user', type: 'userMessage', text: 'try this again',
      createdAt: '2026-08-04T14:00:00Z', liveSeq: 1, done: true,
    }], runtime: { sessionStatus: 'ready', turnActive: session.store.turnActive } }) })

    const matchingRows = () => [...container.querySelectorAll('.convo .item')]
      .filter((row) => row.textContent.includes('try this again'))
    await waitFor(() => expect(matchingRows()).toHaveLength(1))
    expect(container.querySelector('[data-transcript-source="pending"]')).toBeNull()
  })

  it('renders a live prompt once and before its unsaved reply', async () => {
    stubTurn(() => new Promise(() => {}))
    const { container } = render(ThreadPaneView, { tab: liveTab })
    const session = getSessionStore('session-1')
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [
      {
        id: 'nano-user:action-echo', type: 'userMessage', text: 'one visible prompt', status: 'delivering',
        clientActionId: 'action-echo', createdAt: '2026-08-04T14:00:00Z', liveSeq: 1, done: true,
      },
      {
        id: 'agent-after-prompt', type: 'agentMessage', text: 'reply after the prompt',
        createdAt: '2026-08-04T14:00:01Z', liveSeq: 2, done: false,
      },
    ], runtime: { sessionStatus: 'ready', turnActive: session.store.turnActive } }) })

    const matchingRows = () => [...container.querySelectorAll('.convo .item')]
      .filter((row) => row.textContent.includes('one visible prompt'))
    await waitFor(() => expect(matchingRows()).toHaveLength(1))
    expect(container.querySelector('.pending-send')).not.toBeNull()
    const rows = [...container.querySelectorAll('.convo .item')]
    expect(rows.findIndex((row) => row.textContent.includes('one visible prompt')))
      .toBeLessThan(rows.findIndex((row) => row.textContent.includes('reply after the prompt')))
  })

  it('drops a refused prompt from the stream without leaving a dot or controls', async () => {
    stubTurn(() => new Promise(() => {}))
    const { container } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const store = getSessionStore('session-1').store
    messageDriver.send({ op: 'snapshot', snapshot: projectionSnapshot({ provider: 'claude', items: [{
      id: 'row:claude:interrupted-action', type: 'userMessage', text: 'ghost prompt',
      clientActionId: 'interrupted-action', status: 'delivering', done: true, item: { type: 'userMessage' },
    }], runtime: { sessionStatus: 'ready', turnActive: store.turnActive } }) })
    await waitFor(() => expect(container.querySelector('.pending-user-dot')).not.toBeNull())
    // Refusal drops the live entry; no saved row follows it.
    messageDriver.send({ op: 'item.remove', itemID: 'row:claude:interrupted-action' })
    await waitFor(() => {
      expect(container.querySelector('[data-client-action-id="interrupted-action"]')).toBeNull()
      expect(container.querySelector('.pending-user-dot')).toBeNull()
      expect([...container.querySelectorAll('button')].some((button) => button.textContent.trim() === 'Resend')).toBe(false)
      expect([...container.querySelectorAll('button')].some((button) => button.textContent.trim() === 'Discard')).toBe(false)
    })
  })

  it('brings a streamed prompt into view after the reader scrolled up', async () => {
    vi.stubGlobal('requestAnimationFrame', (callback) => setTimeout(() => callback(Date.now()), 0))
    vi.stubGlobal('cancelAnimationFrame', (handle) => clearTimeout(handle))
    stubTurn(() => new Promise(() => {}))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const scroller = container.querySelector('.convo')
    Object.defineProperty(scroller, 'scrollHeight', { configurable: true, get: () => 1000 })
    Object.defineProperty(scroller, 'clientHeight', { configurable: true, get: () => 200 })
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function () {
      if (this === scroller) return { top: 0, bottom: 200, height: 200 }
      if (this.dataset?.clientActionId) {
        const bottom = 1000 - scroller.scrollTop
        return { top: bottom - 80, bottom, height: 80 }
      }
      return { top: 0, bottom: 0, height: 0 }
    })
    await new Promise((resolve) => setTimeout(resolve, 20))

    scroller.scrollTop = 240
    await fireEvent.wheel(scroller)
    await fireEvent.scroll(scroller)
    await typeAndSend(container, getByRole, 'keep this prompt visible')

    const actionID = JSON.parse(fetch.mock.calls.find(([url]) => String(url).endsWith('/turn'))[1].body).clientActionId
    messageDriver.send({ op: 'item.upsert', item: {
      id: `row:claude:${actionID}`, type: 'userMessage', category: 'messages',
      text: 'keep this prompt visible', clientActionId: actionID, status: 'accepted',
      item: { type: 'userMessage' },
    } })
    await waitFor(() => expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).not.toBeNull())
    const pending = container.querySelector(`[data-client-action-id="${actionID}"]`)
    expect(pending.textContent).toContain('keep this prompt visible')
    await waitFor(() => expect(pending.getBoundingClientRect().bottom).toBeLessThanOrEqual(200))
    expect(scroller.scrollTop).toBe(1000)
  })

  it('restores the text only after a definitive server rejection', async () => {
    stubTurn(() => ({
      ok: false, status: 400, headers: { get: () => null },
      text: async () => 'invalid prompt',
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])

    const textarea = await typeAndSend(container, getByRole, 'the words that matter')

    // The failure is visible in the pane where it happened — not only on the
    // exited-session note — and the operator's words are back.
    await waitFor(() => expect(container.querySelector('.send-notice')).not.toBeNull())
    expect(container.querySelector('.send-notice').textContent).toContain('invalid prompt')
    await waitFor(() => expect(textarea.value).toBe('the words that matter'))
    expect(localStorage.getItem(DRAFT_KEY)).toBe('the words that matter')
    expect(container.querySelector('.pending-send')).toBeNull()

    expect(container.querySelector('.send-notice')).not.toBeNull()
  })

  it('deletes the persisted draft during the handoff to the pending operation', async () => {
    let respond
    stubTurn(() => new Promise((resolve) => { respond = resolve }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])

    const textarea = await typeAndSend(container, getByRole, 'half a thought')

    // The operation owns the words now; ordinary draft storage does not keep a
    // second editable copy while confirmation is pending.
    expect(textarea.value).toBe('')
    expect(localStorage.getItem(DRAFT_KEY)).toBeNull()

    respond({ ok: true, status: 200, headers: { get: () => null }, text: async () => '' })
    await waitFor(() => expect(localStorage.getItem(DRAFT_KEY)).toBeNull())
    expect(container.querySelector('.send-notice')).toBeNull()
  })

  // The 2026-09-30 loss: a POST that never reached Nano timed out after the
  // composer had handed its text to an in-memory operation, and with no owner
  // row there was nothing to Resend or Discard. The text must stay visible,
  // survive a reload, and a Resend must reuse the action id so a late first
  // attempt and the resend collapse into one prompt on the server. Discard
  // asks Nano to end it too, since a lost acknowledgement does not prove the
  // POST never landed, then returns the text to the composer.
  for (const action of ['Resend', 'Discard']) {
    it(`keeps a never-acknowledged send with Resend and Discard across a reload, then ${action}`, async () => {
      const posts = []
      const deletes = []
      vi.stubGlobal('fetch', vi.fn(async (url, options) => {
        const u = String(url)
        if (u.endsWith('/turn')) {
          posts.push(JSON.parse(options.body))
          throw Object.assign(new Error('The operation was aborted.'), { name: 'AbortError' })
        }
        if (u.includes('/turn-delivery/') && options?.method === 'DELETE') {
          const actionID = u.split('/').at(-1)
          deletes.push(actionID)
          return jsonResponse({ canceled: true, clientActionId: actionID, state: '' })
        }
        if (u.includes('/turn-delivery/')) return notFound
        return jsonResponse({})
      }))
      const first = render(ThreadPaneView, { tab: liveTab })
      syncSessionSummaries([liveSession])
      await typeAndSend(first.container, first.getByRole, 'the lost prompt')

      const unsent = () => first.container.querySelector('.unsent-send')
      await waitFor(() => expect(unsent()?.textContent).toContain('the lost prompt'))
      expect(first.container.querySelector('.send-notice').textContent).not.toContain('remains above')

      // Reload: a fresh store and component over the same browser storage.
      cleanup()
      dropSessionStore('session-1')
      const second = render(ThreadPaneView, { tab: liveTab })
      syncSessionSummaries([liveSession])
      await waitFor(() => expect(second.container.querySelector('.unsent-send')?.textContent).toContain('the lost prompt'))
      expect(second.getByRole('button', { name: 'Discard' })).toBeTruthy()

      await fireEvent.click(second.getByRole('button', { name: action }))
      if (action === 'Resend') {
        await waitFor(() => expect(posts).toHaveLength(2))
        expect(posts[1]).toEqual(posts[0])
        return
      }
      await waitFor(() => expect(deletes).toEqual([posts[0].clientActionId]))
      await waitFor(() => expect(second.container.querySelector('.unsent-send')).toBeNull())
      expect(second.container.querySelector('.composer textarea').value).toBe('the lost prompt')
      expect(localStorage.getItem(`nano-unsent:${posts[0].clientActionId}`)).toBeNull()
    })
  }

  // A send whose acknowledgement was lost may still have reached Nano. Once
  // the stream lists its action id, Nano owns it: the browser's copy goes,
  // and the row shows the server's state.
  it('hands a never-acknowledged send to the server once the stream lists it', async () => {
    stubTurn(() => { throw Object.assign(new Error('The operation was aborted.'), { name: 'AbortError' }) })
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'did this land?')
    await waitFor(() => expect(container.querySelector('.unsent-send')?.textContent).toContain('Not delivered yet'))
    const keys = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
    const actionID = keys.find((key) => key.startsWith('nano-unsent:')).slice('nano-unsent:'.length)

    messageDriver.send({ item: { id: `prompt:${actionID}`, type: 'userMessage', text: 'did this land?', clientActionId: actionID, status: 'accepted', done: true } })
    await waitFor(() => expect(container.querySelector('.unsent-send')).toBeNull())
    expect(container.querySelector(`[data-client-action-id="${actionID}"]`).textContent).toContain('did this land?')
    expect(localStorage.getItem(`nano-unsent:${actionID}`)).toBeNull()
  })

  // A POST whose answer is lost says nothing about what Nano did with the
  // send: Nano may have saved it, refused it (dropping its row from the
  // stream), or never seen it. The browser asks Nano once and keeps the text
  // reachable on every answer: a saved prompt makes no unacknowledged row, a
  // refused one returns to the composer, and with no answer the text stays
  // below with Resend and Discard.
  for (const [nano, outcome] of [['saved it', 'saved'], ['refused it', 'composer'], ['cannot be asked', 'unsent']]) {
    it(`keeps a stream-listed send reachable when its POST answer is lost and Nano ${nano}`, async () => {
      let rejectPost
      vi.stubGlobal('fetch', vi.fn(async (url, options) => {
        const u = String(url)
        if (u.endsWith('/turn')) return new Promise((_resolve, reject) => { rejectPost = reject })
        if (u.includes('/turn-delivery/') && !options?.method) {
          const clientActionId = decodeURIComponent(u.split('/').at(-1))
          if (outcome === 'saved') return promptRead(clientActionId, 'accepted')
          if (outcome === 'composer') return promptRead(clientActionId, 'refused', 'Not sent: provider refused')
          throw new TypeError('failed to fetch')
        }
        return jsonResponse({})
      }))
      const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
      syncSessionSummaries([liveSession])
      const textarea = await typeAndSend(container, getByRole, 'keep me reachable')
      await waitFor(() => expect(rejectPost).toBeTypeOf('function'))
      const keys = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
      const actionID = keys.find((key) => key.startsWith('nano-unsent:')).slice('nano-unsent:'.length)
      const prompt = { id: `prompt:${actionID}`, type: 'userMessage', text: 'keep me reachable', clientActionId: actionID, status: 'delivering', done: true }
      messageDriver.send({ item: prompt })
      await waitFor(() => expect(container.querySelector(`[data-client-action-id="${actionID}"]`)).not.toBeNull())
      if (outcome === 'saved') messageDriver.send({ item: { ...prompt, status: 'accepted', durableItemID: 'saved-already' } })
      else messageDriver.send({ op: 'item.remove', itemID: prompt.id })
      await waitFor(() => expect(container.querySelector(`[data-transcript-key="live:${prompt.id}"]`)).toBeNull())

      rejectPost(new Error('connection reset'))
      await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
      if (outcome === 'saved') {
        expect(container.querySelector('.unsent-send')).toBeNull()
        expect(container.querySelector('.send-notice')).toBeNull()
      } else if (outcome === 'composer') {
        await waitFor(() => expect(textarea.value).toBe('keep me reachable'))
        expect(container.querySelector('.unsent-send')).toBeNull()
      } else {
        expect(container.querySelector('.unsent-send')?.textContent).toContain('keep me reachable')
      }
    })
  }

  // An operator's Feedback note: "prompts submit, but nano says 'Not
  // acknowledged by Nano'". A send Nano answered is Nano's: its stored copy
  // goes at once, or the next session summary shows it again as unacknowledged.
  it('does not say Not acknowledged by Nano for a prompt that submitted, after the next summary', async () => {
    stubTurn(() => jsonResponse({ accepted: true }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'answered at once')
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))

    syncSessionSummaries([liveSession])
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(container.querySelector('.unsent-send')).toBeNull()
    const keys = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
    expect(keys.filter((key) => key.startsWith('nano-unsent:'))).toEqual([])
  })

  // Ending a prompt in Nano ends this browser's copy too: a Discard while the
  // send's own POST still waits must not leave its stored copy for a reload
  // to show again as unacknowledged.
  it('leaves no stored copy behind when a prompt is discarded while its POST waits', async () => {
    let actionID = ''
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        actionID = JSON.parse(options.body).clientActionId
        return new Promise(() => {})
      }
      if (options?.method === 'DELETE') return jsonResponse({ canceled: true, state: 'expired', clientActionId: decodeURIComponent(u.split('/').at(-1)) })
      return jsonResponse({})
    }))
    const first = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(first.container, first.getByRole, 'park then discard')
    await waitFor(() => expect(actionID).not.toBe(''))
    messageDriver.send({ item: { id: `prompt:${actionID}`, type: 'userMessage', text: 'park then discard', clientActionId: actionID,
      status: 'parked', allowedActions: ['resend', 'discard'], label: 'Not delivered', done: true } })
    await waitFor(() => expect(first.getByRole('button', { name: 'Discard' })).toBeTruthy())
    await fireEvent.click(first.getByRole('button', { name: 'Discard' }))
    await waitFor(() => expect(first.container.querySelector('.composer textarea').value).toBe('park then discard'))

    cleanup()
    dropSessionStore('session-1')
    const second = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(second.container.querySelector('.unsent-send')).toBeNull()
  })

  // A Discard Nano confirmed is final here: a late failure of the send's own
  // POST, with Nano's status out of reach, must not bring it back as
  // unacknowledged beside the text Discard already returned.
  it('does not revive a discarded prompt when its POST later fails and Nano cannot be asked', async () => {
    let rejectPost
    let actionID = ''
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        actionID = JSON.parse(options.body).clientActionId
        return new Promise((_resolve, reject) => { rejectPost = reject })
      }
      if (options?.method === 'DELETE') return jsonResponse({ canceled: true, state: 'expired', clientActionId: actionID })
      if (u.includes('/turn-delivery/')) throw new TypeError('failed to fetch')
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'discard stays discarded')
    await waitFor(() => expect(rejectPost).toBeTypeOf('function'))
    messageDriver.send({ item: { id: `prompt:${actionID}`, type: 'userMessage', text: 'discard stays discarded', clientActionId: actionID,
      status: 'parked', allowedActions: ['resend', 'discard'], label: 'Not delivered', done: true } })
    await waitFor(() => expect(getByRole('button', { name: 'Discard' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Discard' }))
    await waitFor(() => expect(container.querySelector('.composer textarea').value).toBe('discard stays discarded'))
    messageDriver.send({ op: 'item.remove', itemID: `prompt:${actionID}` })

    rejectPost(Object.assign(new Error('POST answer lost'), { name: 'AbortError' }))
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    expect(container.querySelector('.unsent-send')).toBeNull()
  })

  // A Resend Nano made retires this tab's copy of the original send for good:
  // the new prompt owns the text. Neither a late failure of the original POST
  // (with Nano out of reach) nor its late refusal may show it again as
  // unacknowledged or hand its text back to the composer.
  it('keeps the original send retired after Nano resends it, when its own POST later fails', async () => {
    let rejectPost, confirmResend
    const posts = []
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        posts.push(JSON.parse(options.body))
        return new Promise((_resolve, reject) => { rejectPost = reject })
      }
      if (u.endsWith('/resend')) return new Promise((resolve) => { confirmResend = () => resolve(jsonResponse({ clientActionId: 'replacement', state: 'queued' })) })
      if (u.includes('/turn-delivery/')) throw new TypeError('failed to fetch')
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'held then resent')
    await waitFor(() => expect(rejectPost).toBeTypeOf('function'))
    const actionID = posts[0].clientActionId
    messageDriver.send({ item: { id: `prompt:${actionID}`, type: 'userMessage', text: 'held then resent', clientActionId: actionID,
      status: 'parked', allowedActions: ['resend', 'discard'], label: 'Not delivered: provider refused', done: true } })
    await waitFor(() => expect(getByRole('button', { name: 'Resend' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Resend' }))
    await waitFor(() => expect(confirmResend).toBeTypeOf('function'))
    messageDriver.send({ op: 'item.remove', itemID: `prompt:${actionID}` })
    rejectPost(new Error('connection reset'))
    await waitFor(() => expect(container.querySelector('.unsent-send')).not.toBeNull())
    confirmResend()
    await waitFor(() => expect(container.querySelector('.unsent-send')).toBeNull())
    expect(localStorage.getItem(`nano-unsent:${actionID}`)).toBeNull()
    expect(container.querySelector('.composer textarea').value).toBe('')
  })

  it('keeps the original send retired after Nano resends it, when its own POST is later refused', async () => {
    let finishPost
    const posts = []
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        posts.push(JSON.parse(options.body))
        return new Promise((resolve) => { finishPost = () => resolve({ ok: false, status: 424, headers: { get: () => null }, text: async () => JSON.stringify({ error: { code: 'failed_dependency', message: 'provider refused' } }) }) })
      }
      if (u.endsWith('/resend')) return jsonResponse({ clientActionId: 'replacement', state: 'queued' })
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'resend owns this text')
    const actionID = posts[0].clientActionId
    messageDriver.send({ item: { id: `prompt:${actionID}`, type: 'userMessage', text: 'resend owns this text', clientActionId: actionID,
      status: 'parked', allowedActions: ['resend', 'discard'], label: 'Not delivered: provider refused', done: true } })
    await waitFor(() => expect(getByRole('button', { name: 'Resend' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Resend' }))
    await waitFor(() => expect(fetch.mock.calls.some(([u]) => String(u).endsWith('/resend'))).toBe(true))
    await new Promise((resolve) => setTimeout(resolve, 0))
    messageDriver.send({ op: 'item.remove', itemID: `prompt:${actionID}` })
    finishPost()
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    expect(container.querySelector('.composer textarea').value).toBe('')
    expect(container.querySelector('.unsent-send')).toBeNull()
  })

  // Text goes back to the composer at most once. A Discard of a queued send
  // whose own POST answer was lost can learn the end twice: Nano's status
  // read reports the cancel as a refusal, then the DELETE answer confirms it.
  it('hands a discarded queued prompt back once when Nano reports its end twice', async () => {
    let rejectPost
    let confirmDelete
    let actionID = ''
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        actionID = JSON.parse(options.body).clientActionId
        return new Promise((_resolve, reject) => { rejectPost = reject })
      }
      if (options?.method === 'DELETE') return new Promise((resolve) => { confirmDelete = () => resolve(jsonResponse({ canceled: true, state: 'refused', clientActionId: actionID })) })
      if (u.includes('/turn-delivery/')) return promptRead(actionID, 'refused', 'Not sent: canceled')
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'queued words once')
    await waitFor(() => expect(rejectPost).toBeTypeOf('function'))
    messageDriver.send({ item: { id: `prompt:${actionID}`, type: 'userMessage', text: 'queued words once', clientActionId: actionID,
      status: 'queued', allowedActions: ['discard'], label: 'Queued for the next turn', done: true } })
    await waitFor(() => expect(getByRole('button', { name: 'Discard' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Discard' }))
    await waitFor(() => expect(confirmDelete).toBeTypeOf('function'))
    messageDriver.send({ op: 'item.remove', itemID: `prompt:${actionID}` })
    rejectPost(Object.assign(new Error('POST answer lost'), { name: 'AbortError' }))
    await waitFor(() => expect(container.querySelector('.composer textarea').value).toBe('queued words once'))

    confirmDelete()
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(container.querySelector('.composer textarea').value).toBe('queued words once')
    expect(container.querySelector('.unsent-send')).toBeNull()
  })

  // Once Nano has said it has a send, that stays true: a Resend of a loaded
  // copy whose own answer is then lost must not show it again as
  // unacknowledged.
  it('keeps a loaded send cleared when Nano confirms it during Resend and the retry answer is lost', async () => {
    localStorage.setItem('nano-unsent:prior-send', JSON.stringify({
      clientActionId: 'prior-send', provider: 'claude', threadId: 'native-1', sessionId: 'session-1', text: 'already confirmed once', createdAt: Date.now(),
    }))
    let resolveLookup
    let rejectRetry
    let reads = 0
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      const u = String(url)
      if (u.endsWith('/turn')) return new Promise((_resolve, reject) => { rejectRetry = reject })
      if (u.includes('/turn-delivery/prior-send') && !options?.method) {
        reads += 1
        if (reads === 1) return new Promise((resolve) => { resolveLookup = () => resolve(promptRead('prior-send', 'confirmed')) })
        throw new TypeError('failed to fetch')
      }
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await waitFor(() => expect(resolveLookup).toBeTypeOf('function'))
    await waitFor(() => expect(getByRole('button', { name: 'Resend' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Resend' }))
    await waitFor(() => expect(rejectRetry).toBeTypeOf('function'))
    resolveLookup()
    await waitFor(() => expect(container.querySelector('.unsent-send')).toBeNull())

    rejectRetry(Object.assign(new Error('retry POST answer lost'), { name: 'AbortError' }))
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    expect(container.querySelector('.unsent-send')).toBeNull()
  })

  // A refusal Nano reports for a send this tab is still posting returns the
  // send's text even when Nano had already acknowledged it: here a queued
  // Discard is seen first as the retry's refusal, and the DELETE answer is
  // then lost.
  it('returns queued text once when an acknowledged retry observes cancellation and the DELETE answer is lost', async () => {
    localStorage.setItem('nano-unsent:prior-queued', JSON.stringify({
      clientActionId: 'prior-queued', provider: 'claude', threadId: 'native-1', sessionId: 'session-1', text: 'queue should come back', createdAt: Date.now(),
    }))
    let resolveLookup
    let rejectRetry
    let rejectDelete
    let reads = 0
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      const u = String(url)
      if (u.endsWith('/turn')) return new Promise((_resolve, reject) => { rejectRetry = reject })
      if (options?.method === 'DELETE') return new Promise((_resolve, reject) => { rejectDelete = reject })
      if (u.includes('/turn-delivery/prior-queued')) {
        reads += 1
        if (reads === 1) return new Promise((resolve) => { resolveLookup = () => resolve(promptRead('prior-queued', 'queued', 'Queued for the next turn')) })
        return promptRead('prior-queued', 'refused', 'Not sent: canceled')
      }
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await waitFor(() => expect(resolveLookup).toBeTypeOf('function'))
    await fireEvent.click(getByRole('button', { name: 'Resend' }))
    await waitFor(() => expect(rejectRetry).toBeTypeOf('function'))
    messageDriver.send({ item: { id: 'prompt:prior-queued', type: 'userMessage', text: 'queue should come back', clientActionId: 'prior-queued',
      status: 'queued', allowedActions: ['discard'], label: 'Queued for the next turn', done: true } })
    resolveLookup()
    await waitFor(() => expect(container.querySelector('.unsent-send')).toBeNull())
    await fireEvent.click(getByRole('button', { name: 'Discard' }))
    await waitFor(() => expect(rejectDelete).toBeTypeOf('function'))
    messageDriver.send({ op: 'item.remove', itemID: 'prompt:prior-queued' })
    rejectRetry(Object.assign(new Error('retry answer lost'), { name: 'AbortError' }))
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    rejectDelete(new TypeError('DELETE answer lost'))
    await waitFor(() => expect(container.textContent).toContain('could not cancel the message'))
    expect(container.querySelector('.composer textarea').value).toBe('queue should come back')
  })

  // A stored copy left behind by an earlier page (a build that did not clear
  // it) belongs to a prompt Nano may already have: a reload asks Nano once and
  // drops the copy of a prompt Nano has.
  it('drops a stored copy of a send Nano already has when the page loads', async () => {
    localStorage.setItem('nano-unsent:already-delivered', JSON.stringify({
      clientActionId: 'already-delivered', provider: 'claude', threadId: 'native-1', sessionId: 'session-1', text: 'delivered earlier', createdAt: Date.now(),
    }))
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      const u = String(url)
      if (u.includes('/turn-delivery/already-delivered')) return promptRead('already-delivered', 'confirmed')
      return jsonResponse({})
    }))
    const { container } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await waitFor(() => expect(localStorage.getItem('nano-unsent:already-delivered')).toBeNull())
    expect(container.querySelector('.unsent-send')).toBeNull()
  })

  // A row stays until the stream retires it, so a second click on its
  // Discard must not end it twice: that put its text back in the composer
  // twice. (A repeated Resend is answered once by Nano itself.)
  for (const action of ['Discard']) {
    it(`acts once when ${action} is clicked twice before the stream retires the row`, async () => {
      const posts = []
      const deletes = []
      vi.stubGlobal('fetch', vi.fn(async (url, options) => {
        if (options?.method === 'DELETE') {
          deletes.push(String(url))
          return jsonResponse({ canceled: true, state: 'expired', clientActionId: 'held' })
        }
        if (String(url).endsWith('/turn')) {
          posts.push(JSON.parse(options.body))
          return jsonResponse({ accepted: true })
        }
        return jsonResponse({})
      }))
      const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
      syncSessionSummaries([liveSession])
      messageDriver.send({ item: { id: 'prompt:held', type: 'userMessage', text: 'act once', clientActionId: 'held',
        status: 'parked', allowedActions: ['resend', 'discard'], label: 'Not delivered', done: true } })
      await waitFor(() => expect(getByRole('button', { name: action })).toBeTruthy())
      await fireEvent.click(getByRole('button', { name: action }))
      await waitFor(() => expect(deletes).toHaveLength(1))
      await fireEvent.click(getByRole('button', { name: action }))
      await new Promise((resolve) => setTimeout(resolve, 25))

      expect(deletes).toHaveLength(1)
      expect(posts).toHaveLength(0)
      expect(container.querySelector('.composer textarea').value).toBe('act once')
    })
  }

  // The same stall gave no sign the provider was slow. Nano's runtime status
  // counts its provider's recent delivery deadline expiries; the pane says so
  // while the count holds, and stops when it clears.
  it('says the provider is slow while its delivery deadlines are expiring', async () => {
    stubTurn(() => jsonResponse({ accepted: true }))
    const { container, queryByText } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const slow = 'Claude is slow right now: prompts may take minutes to be taken.'
    MockEventSource.instances.find((source) => source.url.includes('/live?'))?.open()
    messageDriver.send({ runtime: { sessionStatus: 'ready' } })
    await waitFor(() => expect(container.querySelector('.composer textarea')).not.toBeNull())
    expect(queryByText(slow)).toBeNull()

    messageDriver.send({ runtime: { deliveryExpiries: 2 } })
    await waitFor(() => expect(queryByText(slow)).not.toBeNull())

    messageDriver.send({ runtime: { deliveryExpiries: 0 } })
    await waitFor(() => expect(queryByText(slow)).toBeNull())
  })

  it('rejoins the failed text above what was typed while the send was in flight', async () => {
    let respond
    stubTurn(() => new Promise((resolve) => { respond = resolve }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])

    const textarea = await typeAndSend(container, getByRole, 'first message')
    await fireEvent.input(textarea, { target: { value: 'typed meanwhile' } })

    respond({
      ok: false, status: 400, headers: { get: () => null },
      text: async () => 'bad request',
    })
    await waitFor(() => expect(textarea.value).toBe('first message\n\ntyped meanwhile'))
  })

  // Two refusals in one moment each hand their text back; the second must not
  // overwrite the first before the composer takes it.
  it('returns both prompts when Nano refuses two sends at once', async () => {
    const pending = []
    stubTurn(() => new Promise((resolve) => { pending.push(resolve) }))
    const { container, getByRole, queryByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])

    const textarea = await typeAndSend(container, getByRole, 'first refused prompt')
    await fireEvent.input(textarea, { target: { value: 'second refused prompt' } })
    await fireEvent.click(queryByRole('button', { name: 'Queue' }) ?? getByRole('button', { name: 'Send' }))
    // The second waits until Nano has answered the first.
    await waitFor(() => expect(pending).toHaveLength(1))

    const refuse = (resolve) => resolve({ ok: false, status: 400, headers: { get: () => null }, text: async () => 'bad request' })
    refuse(pending[0])
    await waitFor(() => expect(pending).toHaveLength(2))
    refuse(pending[1])
    // Each comes back once; the later refusal lands above the earlier text.
    await waitFor(() => expect(textarea.value).toBe('second refused prompt\n\nfirst refused prompt'))
  })
})


// The operator, about to fly on in-flight wifi: "after the user submits, the
// browser keeps retrying until it goes through". A send Nano never
// acknowledged re-posts itself under its one action id, which Nano answers
// once however often it arrives, until Nano answers; it never re-posts a
// prompt Nano holds. Each test names the unsent-copy invariant it guards
// (lib/pending_sends.js).
describe('a send Nano never acknowledged retries until Nano answers', () => {
  let stopConnection = () => {}
  beforeEach(() => {
    // One page's connection view; each test starts from a page that has not checked yet.
    Object.assign(connection, { status: 'checking', lastSuccessAt: 0, offlineSinceAt: 0 })
  })
  afterEach(() => {
    stopConnection()
    stopConnection = () => {}
  })

  // stubNetwork fails every request the way a browser fails one offline until
  // net.up, then lets nano answer (undefined: an empty 200). It records each
  // POST /turn with whether the network was up for it.
  function stubNetwork(nano = () => undefined) {
    const net = { up: false, posts: [] }
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (u.endsWith('/turn')) net.posts.push({ ...JSON.parse(options.body), up: net.up })
      if (!net.up) throw new TypeError('Failed to fetch')
      return (await nano(u, options)) ?? jsonResponse({})
    }))
    vi.spyOn(navigator, 'onLine', 'get').mockImplementation(() => net.up)
    return net
  }

  const unsentRow = (container) => container.querySelector('.unsent-send')

  // The connection drops as the Send leaves, before the page knows it: the
  // POST gets no answer, and the send retries on its own until Nano answers.
  it('keeps a send that got no answer and delivers it once, under its one action id, when Nano is reachable again (text always held)', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const net = stubNetwork((u) => (u.endsWith('/turn') ? jsonResponse({ accepted: true }) : undefined))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])

    const textarea = await typeAndSend(container, getByRole, 'sent from the plane')
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('Not delivered yet: retrying in 2s (attempt 1)'))
    stopConnection = startConnectionChecks()
    expect(unsentRow(container).textContent).toContain('sent from the plane')
    expect(textarea.value).toBe('')
    expect(textarea.classList.contains('send-blocked')).toBe(false)
    const actionID = net.posts[0].clientActionId
    expect(JSON.parse(localStorage.getItem(`nano-unsent:${actionID}`)).text).toBe('sent from the plane')

    // Still offline: it tries again by itself, backing off to 30 s.
    await vi.advanceTimersByTimeAsync(2_000)
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('(attempt 2)'))
    await vi.advanceTimersByTimeAsync(4_000 + 8_000 + 16_000)
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('(attempt 5)'))

    // Back online: the next read finds Nano within 5 s and the send goes then,
    // not after its 30 s backoff.
    net.up = true
    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(5_100)
    await waitFor(() => expect(unsentRow(container)).toBeNull())
    expect(localStorage.getItem(`nano-unsent:${actionID}`)).toBeNull()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(net.posts.map((post) => [post.clientActionId, post.text])).toEqual(
      net.posts.map(() => [actionID, 'sent from the plane']))
    expect(net.posts.filter((post) => post.up)).toHaveLength(1)
  })

  // Two prompts typed while offline must reach the thread in the order typed:
  // the later one is not posted until Nano has answered the earlier one.
  it('posts a thread\'s not-delivered prompts one at a time, oldest first', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const answers = []
    const net = stubNetwork((u, options) => {
      if (!u.endsWith('/turn')) return undefined
      const { text } = JSON.parse(options.body)
      return new Promise((resolve) => answers.push({ text, answer: () => resolve(jsonResponse({ accepted: true })) }))
    })
    stopConnection = startConnectionChecks()
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'first, typed offline')
    await waitFor(() => expect(container.querySelectorAll('.unsent-send')).toHaveLength(1))
    // The failed first send left the optimistic turn on, so Enter sends this.
    const textarea = container.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'second, typed offline' } })
    await fireEvent.keyDown(textarea, { key: 'Enter' })
    await waitFor(() => expect(container.querySelector('.unsent-count')?.textContent).toContain('2 not delivered'))
    await fireEvent.click(container.querySelector('.unsent-count'))
    expect(container.querySelectorAll('.unsent-send')).toHaveLength(2)
    expect(container.querySelectorAll('.unsent-send')[1].textContent).toContain('waiting for the earlier message')

    net.up = true
    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(5_100)
    await waitFor(() => expect(answers.map((a) => a.text)).toContain('first, typed offline'))
    expect(answers.map((a) => a.text)).toEqual(['first, typed offline'])
    answers[0].answer()
    await waitFor(() => expect(answers.map((a) => a.text)).toEqual(['first, typed offline', 'second, typed offline']))
    answers[1].answer()
    await waitFor(() => expect(container.querySelectorAll('.unsent-send')).toHaveLength(0))
    expect(net.posts.filter((post) => post.up).map((post) => post.text)).toEqual(['first, typed offline', 'second, typed offline'])
  })

  // A Send made while Nano cannot be reached is kept here unposted, and the
  // only thing to do with it is Discard: a stuck prompt is not edited, only
  // discarded.
  it('holds a Send made offline unposted, offers only Discard, and posts it once Nano answers', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const net = stubNetwork((u) => (u.endsWith('/turn') ? jsonResponse({ accepted: true }) : undefined))
    stopConnection = startConnectionChecks()
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await waitFor(() => expect(connection.status).toBe('offline'))

    await typeAndSend(container, getByRole, 'held until Nano answers')
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('Not delivered yet: waiting for the connection'))
    expect(net.posts).toHaveLength(0)
    expect([...unsentRow(container).querySelectorAll('button')].map((button) => button.textContent)).toEqual(['Discard'])
    const keys = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
    const actionID = keys.find((key) => key.startsWith('nano-unsent:')).slice('nano-unsent:'.length)

    net.up = true
    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(5_100)
    await waitFor(() => expect(unsentRow(container)).toBeNull())
    expect(net.posts.map((post) => [post.clientActionId, post.text])).toEqual([[actionID, 'held until Nano answers']])
  })

  // Two tabs can hold the same stored copy. One tab's Discard stops it in the
  // shared record, and the other tab's automatic post reads that record
  // first: it stands down instead of posting the old id again.
  it('makes another tab holding the same send stand down after this tab discards it', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const posts = []
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        posts.push(JSON.parse(options.body).clientActionId)
        throw new TypeError('Failed to fetch')
      }
      if (u.includes('/turn-delivery/')) throw new TypeError('Failed to fetch')
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'one copy, two tabs')
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('(attempt 1)'))
    const other = getSessionStore('session-2')
    other.applySummary({ status: 'ready', provider: 'claude', threadId: 'native-1' })
    await waitFor(() => expect(other.store.unsent).toHaveLength(1))

    await fireEvent.click(within(unsentRow(container)).getByRole('button', { name: 'Discard' }))
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('Not acknowledged by Nano'))
    const before = posts.length
    await vi.advanceTimersByTimeAsync(120_000)
    expect(posts.length).toBe(before)
    expect(other.store.unsent).toHaveLength(0)
    dropSessionStore('session-2')
  })

  // Discard on Nano's row while this tab's own POST for it is still out: the
  // Discard is the person's word even when Nano could not confirm it and the
  // POST's answer is lost too. The send never posts again by itself, and a
  // later send does not wait behind it.
  it('stops a send discarded while its first POST was out, and lets later sends past it', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const posts = []
    let failFirst
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        const body = JSON.parse(options.body)
        posts.push([body.clientActionId, body.text])
        if (posts.length === 1) return new Promise((_resolve, reject) => { failFirst = reject })
        return jsonResponse({ accepted: true })
      }
      if (u.includes('/turn-delivery/')) throw new TypeError('Failed to fetch')
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    const textarea = await typeAndSend(container, getByRole, 'discard me while sending')
    await waitFor(() => expect(failFirst).toBeTypeOf('function'))
    const [actionID] = posts[0]
    messageDriver.send({ item: { id: `prompt:${actionID}`, type: 'userMessage', text: 'discard me while sending', clientActionId: actionID,
      status: 'uncertain', allowedActions: ['resend', 'discard'], label: 'Delivery not confirmed', done: true } })
    await waitFor(() => expect(getByRole('button', { name: 'Discard' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Discard' }))
    await waitFor(() => expect(container.querySelector('.send-notice')?.textContent).toContain('could not cancel'))
    expect(JSON.parse(localStorage.getItem(`nano-unsent:${actionID}`)).stopped).toBe(true)

    failFirst(new TypeError('Failed to fetch'))
    messageDriver.send({ op: 'item.remove', itemID: `prompt:${actionID}` })
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('Not acknowledged by Nano'))
    await vi.advanceTimersByTimeAsync(60_000)
    expect(posts.filter(([id]) => id === actionID)).toHaveLength(1)

    await fireEvent.input(textarea, { target: { value: 'a later send goes' } })
    await fireEvent.keyDown(textarea, { key: 'Enter' })
    await waitFor(() => expect(posts.map(([, text]) => text)).toContain('a later send goes'))
  })

  // Loading a page with two held sends posts the first once, not twice.
  it('posts the first of two stored held sends once when the page loads', async () => {
    const posts = []
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      if (String(url).endsWith('/turn')) {
        posts.push(JSON.parse(options.body).clientActionId)
        return new Promise(() => {})
      }
      return jsonResponse({})
    }))
    for (const [id, age] of [['held-1', 2_000], ['held-2', 1_000]]) {
      localStorage.setItem(`nano-unsent:${id}`, JSON.stringify({
        clientActionId: id, provider: 'claude', threadId: 'native-1', sessionId: 'session-1',
        text: id, createdAt: Date.now() - age, held: true,
      }))
    }
    render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await waitFor(() => expect(posts).toContain('held-1'))
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(posts).toEqual(['held-1'])
  })

  // A tab opened before another tab's send still keeps the thread's order:
  // it reads the shared stored sends before it posts its own.
  it('holds a send behind an older one another open tab stored after this tab loaded', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const posts = []
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        posts.push(JSON.parse(options.body).text)
        throw new TypeError('Failed to fetch')
      }
      if (u.includes('/turn-delivery/')) throw new TypeError('Failed to fetch')
      return jsonResponse({})
    }))
    const other = getSessionStore('session-2')
    other.applySummary({ status: 'ready', provider: 'claude', threadId: 'native-1' })
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'from this tab, first')
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('(attempt 1)'))

    await expect(other.sendTurn('from the other tab, later')).resolves.toBe('held')
    expect(posts).not.toContain('from the other tab, later')
    dropSessionStore('session-2')
  })

  // The row says when the next try is, second by second.
  it('counts down to the next automatic try', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    stubNetwork()
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'when is the next try?')
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('Not delivered yet: retrying in 2s (attempt 1)'))
    expect([...unsentRow(container).querySelectorAll('button')].map((button) => button.textContent)).toEqual(['Resend', 'Discard'])
    await vi.advanceTimersByTimeAsync(1_000)
    expect(unsentRow(container).textContent).toContain('retrying in 1s (attempt 1)')
  })

  // Once Nano holds the prompt, in any state, it is Nano's: an uncertain
  // prompt may still be taken late by the provider, so a re-post could
  // deliver twice.
  for (const how of ['its stream lists it', 'its read finds it']) {
    it(`never posts a send again once ${how}, in any state (a held prompt is never shown again)`, async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      let read = null
      const posts = []
      vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
        const u = String(url)
        if (u.endsWith('/turn')) {
          posts.push(JSON.parse(options.body).clientActionId)
          throw new TypeError('Failed to fetch')
        }
        if (u.includes('/turn-delivery/') && !options.method) {
          if (read) return read
          throw new TypeError('Failed to fetch')
        }
        return jsonResponse({})
      }))
      const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
      syncSessionSummaries([liveSession])
      await typeAndSend(container, getByRole, 'hold me once')
      await waitFor(() => expect(unsentRow(container)?.textContent).toContain('(attempt 1)'))
      const actionID = posts[0]

      const label = "Claude hasn't answered in 30 s; resending may deliver twice"
      if (how === 'its stream lists it') {
        messageDriver.send({ item: { id: `prompt:${actionID}`, type: 'userMessage', text: 'hold me once', clientActionId: actionID,
          status: 'uncertain', allowedActions: ['resend', 'discard'], label, done: true } })
      } else {
        read = promptRead(actionID, 'uncertain', label)
      }
      await vi.advanceTimersByTimeAsync(120_000)
      await waitFor(() => expect(unsentRow(container)).toBeNull())
      const settled = posts.length
      await vi.advanceTimersByTimeAsync(120_000)
      expect(posts.length).toBe(settled)
      expect(posts.length).toBeLessThanOrEqual(how === 'its stream lists it' ? 1 : 2)
      expect(localStorage.getItem(`nano-unsent:${actionID}`)).toBeNull()
    })
  }

  // Discard is the person's word that the send must not go: no automatic
  // re-post follows it, whether or not Nano could confirm it.
  for (const nano of ['confirms it', 'cannot be reached']) {
    it(`stops retrying once the person discards the send and Nano ${nano} (text handed back at most once)`, async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const posts = []
      vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
        const u = String(url)
        if (u.endsWith('/turn')) {
          posts.push(JSON.parse(options.body).clientActionId)
          throw new TypeError('Failed to fetch')
        }
        if (options.method === 'DELETE' && nano === 'confirms it') {
          return jsonResponse({ canceled: true, state: 'absent', clientActionId: decodeURIComponent(u.split('/').at(-1)) })
        }
        if (u.includes('/turn-delivery/')) throw new TypeError('Failed to fetch')
        return jsonResponse({})
      }))
      const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
      syncSessionSummaries([liveSession])
      const textarea = await typeAndSend(container, getByRole, 'never mind')
      await waitFor(() => expect(unsentRow(container)?.textContent).toContain('(attempt 1)'))

      await fireEvent.click(getByRole('button', { name: 'Discard' }))
      if (nano === 'confirms it') {
        await waitFor(() => expect(textarea.value).toBe('never mind'))
        expect(unsentRow(container)).toBeNull()
      } else {
        await waitFor(() => expect(unsentRow(container)?.textContent).toContain('Not acknowledged by Nano'))
        expect(unsentRow(container).textContent).not.toContain('retrying')
      }
      await vi.advanceTimersByTimeAsync(120_000)
      expect(posts).toHaveLength(1)
    })
  }

  // A Discard Nano could not confirm is still the person's word: an automatic
  // POST already on the wire that then gets no answer must not restart the
  // retry, nor may a reload of the page. Only their own Resend may.
  it('keeps a failed Discard a stop against a late lost POST and a reload (text handed back at most once)', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const posts = []
    let failRetry
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        posts.push(JSON.parse(options.body).clientActionId)
        if (posts.length === 2) return new Promise((_resolve, reject) => { failRetry = reject })
        throw new TypeError('Failed to fetch')
      }
      if (u.includes('/turn-delivery/')) throw new TypeError('Failed to fetch')
      return jsonResponse({})
    }))
    const first = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(first.container, first.getByRole, 'stop means stop')
    await waitFor(() => expect(unsentRow(first.container)?.textContent).toContain('(attempt 1)'))
    await vi.advanceTimersByTimeAsync(2_000)
    await waitFor(() => expect(failRetry).toBeTypeOf('function'))

    await fireEvent.click(first.getByRole('button', { name: 'Discard' }))
    await waitFor(() => expect(unsentRow(first.container)?.textContent).toContain('Not acknowledged by Nano'))
    failRetry(new TypeError('Failed to fetch'))
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    await vi.advanceTimersByTimeAsync(120_000)
    expect(posts).toHaveLength(2)
    expect(unsentRow(first.container).textContent).not.toContain('retrying')

    cleanup()
    dropSessionStore('session-1')
    const second = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await waitFor(() => expect(unsentRow(second.container)?.textContent).toContain('stop means stop'))
    await vi.advanceTimersByTimeAsync(120_000)
    expect(posts).toHaveLength(2)
    expect(unsentRow(second.container).textContent).toContain('Not acknowledged by Nano')
  })

  // The stop a Discard leaves must outlive the page. A browser that cannot
  // store it does not ask Nano to discard and says so, rather than leave a
  // record a reload would retry.
  it('does not discard a send whose stop it cannot store, and says so (text always held)', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const deletes = []
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (options.method === 'DELETE') deletes.push(u)
      if (u.endsWith('/turn') || u.includes('/turn-delivery/')) throw new TypeError('Failed to fetch')
      return jsonResponse({})
    }))
    const { container, getByRole } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await typeAndSend(container, getByRole, 'storage is full')
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('(attempt 1)'))
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    // jsdom Storage methods live on its prototype; spying on the instance
    // writes a storage entry instead of replacing the method. Node's shim
    // has own methods, so spy where this runtime actually defines setItem.
    const storageMethods = Object.hasOwn(localStorage, 'setItem') ? localStorage : Object.getPrototypeOf(localStorage)
    const setItem = localStorage.setItem
    vi.spyOn(storageMethods, 'setItem').mockImplementation(function (key, value) {
      if (String(key).startsWith('nano-unsent:')) throw new DOMException('quota', 'QuotaExceededError')
      return setItem.call(this, key, value)
    })

    await fireEvent.click(getByRole('button', { name: 'Discard' }))
    await waitFor(() => expect(container.querySelector('.send-notice')?.textContent).toContain('could not save the Discard'))
    expect(deletes).toEqual([])
    expect(unsentRow(container).textContent).toContain('storage is full')
    const keys = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
    const record = JSON.parse(localStorage.getItem(keys.find((key) => key.startsWith('nano-unsent:'))))
    expect(record.text).toBe('storage is full')
    expect(record.stopped).toBeUndefined()
  })

  // A record from an earlier page whose read says Nano refused it is ended:
  // an automatic POST that started before that answer and then gets none must
  // not show it again or post it again.
  it('keeps a page-load refusal final against an automatic POST that then gets no answer (text handed back at most once)', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    localStorage.setItem('nano-unsent:loaded-refused', JSON.stringify({
      clientActionId: 'loaded-refused', provider: 'claude', threadId: 'native-1', sessionId: 'session-1',
      text: 'refused while away', createdAt: Date.now() - 60_000,
    }))
    const posts = []
    let answerRead
    let failPost
    vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
      const u = String(url)
      if (u.endsWith('/turn')) {
        posts.push(JSON.parse(options.body).clientActionId)
        if (posts.length === 1) return new Promise((_resolve, reject) => { failPost = reject })
        throw new TypeError('Failed to fetch')
      }
      // The page-load read answers late; every later read gets no answer.
      if (u.endsWith('/turn-delivery/loaded-refused') && !options.method && !answerRead) return new Promise((resolve) => { answerRead = resolve })
      if (u.includes('/turn-delivery/')) throw new TypeError('Failed to fetch')
      return jsonResponse({})
    }))
    const { container } = render(ThreadPaneView, { tab: liveTab })
    syncSessionSummaries([liveSession])
    await waitFor(() => expect(unsentRow(container)?.textContent).toContain('refused while away'))
    await waitFor(() => expect(answerRead).toBeTypeOf('function'))
    await vi.advanceTimersByTimeAsync(2_000)
    await waitFor(() => expect(failPost).toBeTypeOf('function'))

    answerRead(promptRead('loaded-refused', 'refused', 'Not sent: provider refused'))
    const textarea = container.querySelector('.composer textarea')
    await waitFor(() => expect(textarea.value).toBe('refused while away'))
    failPost(new TypeError('Failed to fetch'))
    await waitFor(() => expect(getSessionStore('session-1').store.sending).toBe(0))
    await vi.advanceTimersByTimeAsync(120_000)
    expect(unsentRow(container)).toBeNull()
    expect(posts).toEqual(['loaded-refused'])
    expect(textarea.value).toBe('refused while away')
    expect(localStorage.getItem('nano-unsent:loaded-refused')).toBeNull()
  })
})

describe('connection-first composer', () => {
  for (const status of ['starting', 'reconnecting', 'reconnect_parked']) {
    it(`queues retained text while ${status} and shows no stale stop control`, async () => {
      const calls = []
      stubTurn((options) => { calls.push(JSON.parse(options.body)); return Promise.resolve(jsonResponse({ status: 'queued', queued: true })) })
      const summary = { ...liveSession, status, turnActive: true }
      app.sessions = [summary]
      const { container, getByRole, queryByRole } = render(ThreadPaneView, { tab: liveTab })
      syncSessionSummaries([summary])
      getSessionStore('session-1').store.connected = true
      const textarea = container.querySelector('.composer textarea')
      await fireEvent.input(textarea, { target: { value: 'keep my prompt' } })
      expect(textarea.disabled).toBe(false)
      expect(queryByRole('button', { name: 'Stop' })).toBeNull()
      await fireEvent.click(getByRole('button', { name: 'Queue' }))
      await waitFor(() => expect(calls).toHaveLength(1))
      expect(calls[0].text).toBe('keep my prompt')
      expect(container.querySelector('.thinking-elapsed')).toBeNull()
    })
  }
  for (const status of ['reconnect_failed', 'detached', 'exited']) {
    it(`retains the draft without submitting while ${status}`, async () => {
      const calls = []
      stubTurn((options) => { calls.push(options); return Promise.resolve(jsonResponse({})) })
      const summary = { ...liveSession, status, turnActive: true }
      app.sessions = [summary]
      const { container, queryByRole, getByRole } = render(ThreadPaneView, { tab: liveTab })
      syncSessionSummaries([summary])
      const textarea = container.querySelector('.composer textarea')
      await fireEvent.input(textarea, { target: { value: 'retained draft' } })
      expect(getByRole('button', { name: 'Send' }).disabled).toBe(true)
      await fireEvent.click(getByRole('button', { name: 'Send' }))
      expect(queryByRole('button', { name: 'Stop' })).toBeNull()
      expect(textarea.value).toBe('retained draft')
      expect(calls).toEqual([])
    })
  }
})
