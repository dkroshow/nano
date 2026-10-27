import { mergeTranscriptItems, transcriptQuery, transcriptURL } from '../transcript_query.js'
import { recordBrowserDiagnostic } from '../browser_diagnostics.js'
import { noteNanoContact } from '../connection.svelte.js'
import { reopenIfClosed } from './stream_reopen.js'

// openFileTranscript streams one pane of a file-only thread: its saved rows
// and the live text the server holds (the reply being streamed and prompts
// not saved yet). The server matches saved rows to live text and sends a row
// and the removal of its live text in one message, so this applies messages
// in order and matches nothing. The thread's status (Working, turn timing,
// approval cards) arrives on this stream too, as its first message and then
// on every change; onStatus hands it to the session store, onDisconnect
// marks the stream closed until the next status, and onDrop reports a failed
// connection or a server error, not a deliberate close. onSaved hands on the
// action ids of prompts the server names saved, including prompts it never
// showed live, so the pane can end its own copy of a send; onTail hands on the
// prompts a tail lists, so the pane can check a copy no message names.
//
// Every connect, including a reconnect after an outage, starts with a fresh
// tail: the newest page plus live text, which replaces what the pane shows in
// one step. Until it arrives the pane keeps what it showed. A stream the
// browser has given up on is opened again (stream_reopen.js). A failed read
// ends the stream with an error the pane shows.
//
// A prompt's live entry carries its stored state, the actions the person may
// take and a label, all decided by Nano. seed, a warm entry of the same thread
// and profile (warm_transcripts.js), gives rows to show until the first tail
// replaces them.
export function openFileTranscript(ref, view, { onStatus, onDisconnect, onDrop, onSaved, onTail, seed = null } = {}) {
  const state = $state({
    history: seed
      ? { state: historyState(seed.projection, seed.items), items: [...seed.items], turns: [...seed.turns], page: null, projection: seed.projection, error: null, loadingEarlier: false, earlierError: null }
      : { state: 'loading', items: [], turns: [], page: null, projection: null, error: null, loadingEarlier: false, earlierError: null },
    live: [],
    // tailed: a tail has landed, so this stream may replace the one shown.
    tailed: false,

  })
  let threadId = ''
  let closed = false
  // A tail starts a new generation; an older page requested before it is
  // ignored when it answers.
  let generation = 0
  // Lag measures, recorded for live text the server names as saved: L1 from
  // a prompt's first live text to its saved row, L2 from a reply's last text
  // to its row.
  const seen = new Map()
  function observeLive() {
    const now = Date.now()
    const ids = new Set()
    for (const entry of state.live) {
      ids.add(entry.id)
      const times = seen.get(entry.id)
      if (!times) seen.set(entry.id, { kind: entry.kind, actionId: entry.actionId, length: entry.text.length, textAt: now, shownAt: now })
      else if (times.length !== entry.text.length) Object.assign(times, { length: entry.text.length, textAt: now })
    }
    for (const id of seen.keys()) if (!ids.has(id)) seen.delete(id)
  }
  function recordSaved(ids) {
    const prompts = (ids ?? []).filter((id) => id.startsWith('prompt:')).map((id) => id.slice('prompt:'.length))
    if (prompts.length) onSaved?.(prompts)
    const now = Date.now()
    // Each lag is recorded once: a later tail names a settled prompt again.
    for (const id of ids ?? []) {
      const times = seen.get(id)
      seen.delete(id)
      if (times?.kind === 'reply') recordBrowserDiagnostic('file.lag.reply', { hostedThreadId: ref, durationMs: now - times.textAt })
      else if (times?.kind === 'prompt') recordBrowserDiagnostic('file.lag.prompt', { hostedThreadId: ref, actionId: times.actionId, durationMs: now - times.shownAt })
    }
  }
  let source = null
  let reopenTimer = null
  // A connection's first status may replace a status from another projection
  // (after a restart); later ones only advance it.
  let fresh = true

  function onMessage(event) {
    if (closed) return
    noteNanoContact()
    let message
    try {
      message = JSON.parse(event.data)
    } catch {
      return
    }
    switch (message.type) {
      case 'tail':
        generation += 1
        threadId = message.threadId ?? ''
        recordSaved(message.saved)
        state.history = {
          state: historyState(message.projection, message.rows ?? []),
          items: message.rows ?? [],
          turns: message.turns ?? [],
          page: message.page ?? null,
          projection: message.projection ?? null,
          error: null,
          loadingEarlier: false,
          earlierError: null,
        }
        state.live = message.live ?? []
        state.tailed = true
        onTail?.(new Set(state.live.filter((entry) => entry.kind === 'prompt').map((entry) => entry.actionId)))
        seen.clear()
        observeLive()
        break
      case 'update': {
        const removed = new Set(message.removed ?? [])
        const items = mergeTranscriptItems(state.history.items, message.rows).filter((row) => !removed.has(row.id))
        state.history = {
          ...state.history,
          state: historyState(state.history.projection, items),
          items,
          turns: mergeTranscriptItems(state.history.turns, message.turns),
        }
        if (message.live) {
          recordSaved(message.saved)
          state.live = message.live
          observeLive()
        }
        break
      }
      case 'live':
        state.live = message.live ?? []
        observeLive()
        break
      case 'status':
        onStatus?.(message.status, fresh)
        fresh = false
        break
      case 'append': {
        const entry = state.live.find((candidate) => candidate.id === message.id)
        if (!entry) break
        entry.text += message.append
        observeLive()
        break
      }
      case 'error':
        onDrop?.()
        close()
        state.history = { ...state.history, state: 'error', error: message.error }
        break
    }
  }

  function onError() {
    fresh = true
    onDrop?.()
    onDisconnect?.()
    if (closed) return
    reopenTimer ??= reopenIfClosed(source, () => {
      reopenTimer = null
      if (closed) return
      source.close()
      connect()
    })
  }

  function connect() {
    source = new EventSource(liveURL(ref, view))
    source.onmessage = onMessage
    source.onerror = onError
  }
  connect()

  function close() {
    closed = true
    clearTimeout(reopenTimer)
    source.close()
    onDisconnect?.()
  }

  async function loadEarlier() {
    const history = state.history
    const cursor = history.page?.nextBeforeCursor
    if (!cursor || !threadId || history.loadingEarlier) return
    const asked = generation
    state.history = { ...history, loadingEarlier: true, earlierError: null }
    try {
      const response = await fetch(transcriptURL(threadId, cursor, profile(view)))
      if (!response.ok) throw new Error(`earlier transcript failed: ${response.status}`)
      const body = await response.json()
      if (asked !== generation) return
      state.history = {
        ...state.history,
        items: mergeTranscriptItems(body.items, state.history.items),
        turns: mergeTranscriptItems(body.turns, state.history.turns),
        page: { ...state.history.page, hasMoreBefore: body.page?.hasMoreBefore, nextBeforeCursor: body.page?.nextBeforeCursor },
        loadingEarlier: false,
      }
    } catch (error) {
      if (asked === generation) state.history = { ...state.history, loadingEarlier: false, earlierError: error?.message ?? String(error) }
    }
  }

  return { state, loadEarlier, close, seeded: Boolean(seed) }
}

// liveItems is the live text in the shape the pane renders live rows in.
export function liveItems(live) {
  return live.map((entry) => entry.kind === 'prompt'
    ? { id: entry.id, type: 'userMessage', text: entry.text, clientActionId: entry.actionId, status: entry.state,
      allowedActions: entry.allowedActions ?? [], label: entry.label ?? '',
      senderSubmittedAt: entry.senderSubmittedAt, done: true, item: { type: 'userMessage' } }
    : { id: entry.id, type: 'agentMessage', text: entry.text, done: entry.state === 'done', item: { type: 'agentMessage' } })
}

function profile(view) {
  return view === 'all' ? { view: 'dev', hiddenCategories: [] } : { view: 'enhanced' }
}

function liveURL(ref, view) {
  return `/api/v1/hosted-threads/${encodeURIComponent(ref)}/live?${transcriptQuery(null, profile(view))}`
}

function historyState(projection, rows) {
  if (projection?.state && projection.state !== 'ready') return projection.state
  return rows.length ? 'ready' : 'empty'
}
