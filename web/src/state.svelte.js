import { promptAllowed } from './lib/shared/lifecycle_state.js'
// Per-session client state. The server owns provider parsing and publishes a
// file transcript; this store keeps status, delivery controls and the raw
// Event Feed separately.

import { untrack } from 'svelte'
import { SvelteMap } from 'svelte/reactivity'
import { recordBrowserDiagnostic } from './browser_diagnostics.js'
import { readAPIError } from './api_error.js'
import { loadQueue, queueEntry, queueKey, saveQueue } from './lib/composer/turn_queue.js'
import {
  forgetUnsent,
  loadUnsent,
  newClientActionId,
  persistUnsent,
  purgeLegacyPendingStorage,
  readUnsent,
  retryDelayMs,
  unsentStep,
} from './lib/composer/pending_sends.js'
import { applyRuntimeStatus } from './live_projection.js'
import { connection, noteNanoContact, onReachable } from './connection.svelte.js'

const MAX_FEED_EVENTS = 5000
const MAX_BROWSE_EVENTS = 7500
// A request on a slow link (plane Wi-Fi took about 9.5 s just to reach Nano)
// is not cancelled while it may still arrive: after 15 s its row says it is
// still sending, and only a network error or 60 s ends the wait. A retry
// keeps the action id, which Nano answers once however often it arrives.
const SEND_SLOW_MS = 15_000
const SEND_REQUEST_TIMEOUT_MS = 60_000
// A prompt's record in these states is no longer live text: Nano settled it
// (its saved row holds it) or ended it.
const ECHO_SETTLED_STATES = new Set(['confirmed', 'refused', 'expired'])
// The states a cancel can leave a prompt in, as Nano names them.
const ENDED_STATES = new Set(['refused', 'expired', 'absent'])
const NATIVE_ID_RETRY_DELAYS_MS = [250, 500, 1_000, 2_000]
const NATIVE_ID_PROVISIONAL_POLL_MS = 15_000

// Reactive, so a check over every store (an unsettled send, say) sees one
// created after it ran.
const stores = new SvelteMap()
let missingSessionRecovery = null
let legacyPendingPurged = false

const GATEWAY_AMBIGUOUS_STATUSES = new Set([502, 503, 504])

// App state owns the authoritative session registry, while this module owns
// delivery. Registering one narrow resolver avoids a circular import and lets
// a definite stale-id 404 rebind by stable thread identity before retrying.
export function setMissingSessionRecovery(handler) {
  missingSessionRecovery = typeof handler === 'function' ? handler : null
}
// Hosted metadata reaches a store from its own reads: /state and its metadata
// read. The session list carries only each field's displayed value: it paints
// posture until the store's own read lands, and its revision says when to read
// again. Each read takes a sequence number when it starts, and a store accepts
// metadata (or its absence) only from a read that started after the one it
// last accepted.
let metadataReadSeq = 0
export function beginMetadataRead() {
  metadataReadSeq += 1
  return metadataReadSeq
}

// Runtime events carry each thread's newest metadata revision on every event,
// changed or not, so an open store whose read failed asks again next time.
export function noteMetadataRevisions(hints) {
  for (const { id, revision } of hints) stores.get(id)?.noteMetadataRevision(revision)
}

export function syncSessionSummaries(summaries, metadataSeq) {
  for (const summary of summaries ?? []) {
    stores.get(summary.id)?.applySummary(summary, metadataSeq)
  }
}

export function getSessionStore(id, initialSummary = null) {
  let s = stores.get(id)
  if (!s) {
    s = createSessionStore(id, initialSummary)
    stores.set(id, s)
    // The list summary is already on screen before this lazy store exists.
    // Seed its stable identity immediately instead of waiting for /state;
    // that request may be the very one interrupted by a Nano restart.
    if (initialSummary) s.applySummary(initialSummary)
  }
  return s
}

// instructionFileUnsaved says a session's instruction file has edits not yet
// saved.
export function instructionFileUnsaved() {
  for (const { store } of stores.values()) {
    if (store.openFiles?.some((file) => file.dirty)) return true
  }
  return false
}

// sendUnsettled says this browser holds a prompt Nano does not: one being
// sent, one Nano never acknowledged, or a refused prompt's text on its way
// back to the composer. Nano keeps every other prompt across a reload.
export function sendUnsettled() {
  for (const { store } of stores.values()) {
    if (store.restoreDraft || store.sending > 0 || store.unsent.length > 0) return true
  }
  return false
}

export function dropSessionStore(id) {
  stores.get(id)?.disconnect()
  stores.delete(id)
}

// A Nano restart rotates process-local session ids while the provider thread
// remains the same. The old store can still own browser-only state that the
// server cannot reconstruct (a send Nano never acknowledged, a refused
// prompt's text). Move that state into the successor before disposing the
// predecessor so a successful 404 recovery cannot clear the composer.
export function handoffSessionStore(previousId, successorSummary) {
  const previous = stores.get(previousId)
  if (!previous || !successorSummary?.id || successorSummary.id === previousId) {
    dropSessionStore(previousId)
    return null
  }
  const successor = getSessionStore(successorSummary.id, successorSummary)
  successor.adoptSessionHandoff(previous)
  previous.disconnect()
  stores.delete(previousId)
  return successor
}

function createSessionStore(id, initialSummary = null) {
  const store = $state({
    connected: false,
    provider: null,
    hostedThreadId: initialSummary?.hostedThreadId ?? '',
    // A file-only pane's status streams: whether one is open, and the
    // projection and revision of the status last applied.
    streamConnected: false,
    statusEpoch: '',
    statusRevision: -1,
    turnStartedAtMs: 0,
    compactingSinceMs: 0,
    // The provider's delivery deadlines that expired in the last few minutes,
    // across all its threads: above zero, the provider is slow right now.
    deliveryExpiries: 0,
    threadId: '', // stable provider thread identity; survives Nano session-id rotation
    // The bottom bar reads only the shared identity resolver. Runtime/session
    // thread ids remain useful transport hints, but never supply this value.
    resolvedNativeThreadId: '',
    nativeIdentityStatus: initialSummary?.hostedThreadId ? 'pending' : 'idle',
    meta: null, // /state: { provider, threadId, cwd, init, thread, transcriptPath }
    events: [], // raw Event Feed: { seq, ts, dir, msg, label, kind, preview }
    threadStatus: 'unknown',
    turnActive: false,
    // Send joins a working turn on this provider; Queue holds for the next.
    runningTurnInput: initialSummary?.runningTurnInput === true,
    lastTurnEndedAt: null,
    // Codex per-turn overrides the NEXT turn/start will carry, server-owned:
    // { model, effort, summary, threadModel }. Null until /state lands.
    turnOptions: null,
    // The server's unified posture projection. Unlike /state's provider
    // snapshot, a creation decision exists before an asynchronous handshake
    // confirms it, so summaries must be allowed to update this immediately.
    posture: initialSummary?.posture ?? null,
		// Full canonical hosted metadata, from /state or the metadata read. It
		// is kept separately from provider protocol snapshots so a reconnect
		// cannot accidentally revive transcript-derived facts.
		hostedMetadata: null,
		metadataUnavailable: Boolean(initialSummary?.metadataUnavailable),
		metadataDegraded: Boolean(initialSummary?.metadataDegraded),
    interrupting: false,
    // Sends Nano never acknowledged (lib/composer/pending_sends.js), shown with Resend
    // and Discard. Every other prompt, and its state, comes on the pane's
    // stream from Nano.
    unsent: [],
    // This tab's sends the stream has not listed yet, shown in the transcript
    // as delivering from the moment they are sent (lib/composer/pending_sends.js echo).
    echoes: [],
    // POSTs in flight; their text is in storage until Nano answers.
    sending: 0,
    // Server-owned pending interactions (dialogs the agent is waiting on).
    // Seeded from /state — the wire buffer is bounded, so a tab opened late
    // cannot rediscover a parked request by replay — and kept current by the
    // pane status snapshots, which arrive on every file-stream connect.
    pendingInteractions: [],
    tokenUsage: null,
    lastError: null,
    // The one failure the operator must never miss: their own message not
    // getting through. Kept apart from lastError (which any background fetch
    // can overwrite) and rendered as a dismissible banner above the composer.
    // Cleared by the next successful send. { reason, fromQueue }
    sendError: null,
    restoreDraft: null,
    // Session-summary runtime status (starting | ready | exited), synced from
    // the list poll and /state. Gates the composer and the attaching chip.
    sessionStatus: null,
    // MCP tool registry, fetched lazily from /mcp when the Tool Registry
    // section is first expanded — it costs the app-server seconds, so it is
    // deliberately off the /state path that gates first paint.
    mcp: null, // { servers: [{ name, tools }] }
    mcpLoading: false,
    mcpError: null,
    // Codex skills list, fetched from /skills when its section first opens:
    // most of what /state once sent, and only that section shows it.
    skills: null, // { data: [...] }
    skillsError: null,
    eventsHasOlder: false,
    eventsLoadingOlder: false,
    // Set when a page request fails. The feed pages by scrolling, so a failure
    // has to stop the loop — otherwise a dead endpoint re-fires on every scroll
    // event. Cleared by the retry the error note carries.
    eventsOlderError: '',
    // Right-pane tabs. Static tabs are 'responses' | 'feed'; each open
    // instruction file adds a closable tab keyed 'file:<path>'. Kept on the
    // store so file tabs survive thread switches (SessionView is re-keyed).
    rightTab: 'responses',
    openFiles: [], // [{ path, basename, dirty }] — deduped by path
  })

  const sentMethodByID = new Map()

	let feedES = null
	let mcpRefreshTimer = null
	let nativeIdentityTimer = null
	let nativeIdentityAbort = null
	let nativeIdentityAttempt = 0
	let nativeIdentityRequest = 0
	let disposed = false
  // True once the projection has decided turn state; from then on /state
  // snapshots stop seeding store.turnActive (refreshMeta).
  let wireTurnSeen = false
  let projectionRuntimeSeen = false
  let metadataRead = null // the one hosted-metadata read in flight
  let metadataFollowUp = null // one shared read after it, for hints that arrived meanwhile
  let appliedMetadataSeq = 0 // start sequence of the read whose metadata the store holds
  let listedMetadataRevision // the session list's revision, before any full read
  let listedPostureSeq = 0 // start sequence of the list read whose posture the store shows
  // The localStorage key the queue currently persists under. Resolves late,
  // like the composer draft's: session-id fallback until /state or a summary
  // supplies provider + thread id, then the thread key takes over.
  let queueStorageKey = ''
	const legacyQueueMigrations = new Set()
  // Actions whose POST is still waiting: a second Resend click must not race it.
  const postsInFlight = new Set()
  // This tab's copy of each send it touched, kept by unsentStep
  // (lib/composer/pending_sends.js); store.unsent is the shown ones.
  const copies = new Map()
  // The front copy's next automatic post, and when it is due (the row's
  // countdown reads the same time the timer was set from).
  const retryTimers = new Map()
  const retryDueAt = new Map()
  let adopting = false
  // Nano answering again after it could not be reached: the front copy posts
  // at once instead of waiting out its backoff.
  const stopRetryWake = onReachable(() => drain(true))
	const currentThreadId = () => store.threadId || store.meta?.threadId || ''

	function diagnosticContext(extra = {}) {
		return untrack(() => ({
			provider: store.provider,
			threadId: currentThreadId(),
			sessionId: id,
			hostedThreadId: store.hostedThreadId,
			epoch: store.statusEpoch,
			revision: store.statusRevision,
			connected: Boolean(store.streamConnected),
			turnActive: Boolean(store.turnActive),
			...extra,
		}))
	}

	function clearNativeIdentityRequest() {
		clearTimeout(nativeIdentityTimer)
		nativeIdentityTimer = null
		nativeIdentityAbort?.abort()
		nativeIdentityAbort = null
		nativeIdentityRequest += 1
	}

	function scheduleNativeIdentityResolve(delay) {
		clearTimeout(nativeIdentityTimer)
		if (disposed || !store.hostedThreadId) return
		nativeIdentityTimer = setTimeout(() => {
			nativeIdentityTimer = null
			void resolveNativeIdentity()
		}, delay)
	}

	function resetNativeIdentity() {
		clearNativeIdentityRequest()
		nativeIdentityAttempt = 0
		store.resolvedNativeThreadId = ''
		store.nativeIdentityStatus = store.hostedThreadId ? 'pending' : 'idle'
		if (store.hostedThreadId && !disposed) void resolveNativeIdentity()
	}

	// Resolve by immutable hosted id. Session summaries, /state and wire
	// metadata are deliberately only wake-up hints: their thread-id fields do
	// not cross this function's display boundary.
	async function resolveNativeIdentity({ force = false, clear = false } = {}) {
		const ref = store.hostedThreadId
		if (!ref || disposed) return
		if (nativeIdentityAbort && !force) return
		if (force) clearNativeIdentityRequest()
		else clearTimeout(nativeIdentityTimer)
		nativeIdentityTimer = null
		if (clear) {
			store.resolvedNativeThreadId = ''
			store.nativeIdentityStatus = 'pending'
		}
		const request = ++nativeIdentityRequest
		const controller = new AbortController()
		nativeIdentityAbort = controller
		try {
			const response = await fetch(`/api/v1/identity/resolve?ref=${encodeURIComponent(ref)}`, {
				signal: controller.signal,
			})
			if (disposed || request !== nativeIdentityRequest || ref !== store.hostedThreadId) return
			if (!response.ok) throw new Error(`identity resolve status ${response.status}`)
			const identity = await response.json()
			if (disposed || request !== nativeIdentityRequest || ref !== store.hostedThreadId) return
			if (identity.hostedThreadId && identity.hostedThreadId !== ref) {
				throw new Error('identity resolver returned a different hosted thread')
			}
			if (store.provider && identity.provider && identity.provider !== store.provider) {
				throw new Error('identity resolver returned a different provider')
			}
			if (identity.nativeThreadId && !identity.nativeThreadIdProvisional) {
				store.resolvedNativeThreadId = identity.nativeThreadId
				store.nativeIdentityStatus = 'ready'
				nativeIdentityAttempt = 0
				return
			}
			store.resolvedNativeThreadId = ''
			store.nativeIdentityStatus = 'pending'
			nativeIdentityAttempt = 0
			// Claude keeps its cs_ transport stand-in until the first prompt. A
			// slow fallback covers a missed runtime hint without polling hard for
			// the potentially long pre-prompt lifetime.
			scheduleNativeIdentityResolve(
				identity.nativeThreadIdProvisional ? NATIVE_ID_PROVISIONAL_POLL_MS : NATIVE_ID_RETRY_DELAYS_MS[0],
			)
		} catch (error) {
			if (error?.name === 'AbortError' || disposed || request !== nativeIdentityRequest || ref !== store.hostedThreadId) return
			if (!store.resolvedNativeThreadId) store.nativeIdentityStatus = 'pending'
			const delay = NATIVE_ID_RETRY_DELAYS_MS[Math.min(nativeIdentityAttempt, NATIVE_ID_RETRY_DELAYS_MS.length - 1)]
			nativeIdentityAttempt += 1
			scheduleNativeIdentityResolve(delay)
		} finally {
			if (request === nativeIdentityRequest) nativeIdentityAbort = null
		}
	}

	// Codex represents JSON-RPC ids in their raw JSON form (for example `5` or
	// `"request-5"`), while Claude uses an ordinary token. Unwrap the JSON form
	// so the content-free diagnostics schema can retain the request identity.
	function diagnosticInteractionID(requestID) {
		const raw = String(requestID ?? '')
		try {
			const parsed = JSON.parse(raw)
			if (typeof parsed === 'string' || typeof parsed === 'number') return String(parsed)
		} catch {
			// Claude ids are already plain tokens.
		}
		return raw
	}

  // A file-only pane's status comes only from its own stream (one writer, in
  // revision order), never the page feed or a page snapshot. Several panes of
  // one thread share this store, so a status applies only when it is newer: a
  // later revision of the same projection, or a fresh connection's first
  // status from a new projection (after a restart). While a stream is open it
  // owns the status; the registry and /state stop writing it.
  const statusStreams = new Set()
  // Projections a newer one replaced; a delayed message from one, even a
  // connection's first, is never newer.
  const retiredEpochs = new Set()
  function streamOwnsStatus() {
    return statusStreams.size > 0
  }

  function applyStreamStatus(stream, status, fresh) {
    if (disposed || !status) return
    statusStreams.add(stream)
    store.streamConnected = true
    if (retiredEpochs.has(status.epoch)) return
    if (status.epoch === store.statusEpoch ? status.revision <= store.statusRevision : !fresh) return
    if (store.statusEpoch && status.epoch !== store.statusEpoch) retiredEpochs.add(store.statusEpoch)
    applyRuntimeStatus(store, status.runtime, status.interactions)
    projectionRuntimeSeen = true
    store.statusEpoch = status.epoch
    store.statusRevision = status.revision
  }

  function streamDisconnected(stream) {
    statusStreams.delete(stream)
    store.streamConnected = statusStreams.size > 0
  }

  function connectFeed() {
    if (feedES || disposed) return
    feedES = new EventSource(`/api/sessions/${id}/events`)
    feedES.onmessage = (event) => {
      noteNanoContact()
      try {
        ingest(JSON.parse(event.data))
      } catch {
        // A malformed raw feed line is inspectable server-side; it must not
        // take down the normalized conversation channel.
      }
    }
  }

  function disconnectFeed() {
    feedES?.close()
    feedES = null
  }

  // One-way migration for queues created by older bundles. Post each legacy
  // action to Nano with its original id; multiple tabs are harmless because
  // the server journal deduplicates them. New queues never use localStorage.
  function adoptPersistedQueue() {
    const key = queueKey({ provider: store.provider, threadId: currentThreadId(), sessionId: id })
	if (!key) return
    const previousKey = key === queueStorageKey ? '' : queueStorageKey
	queueStorageKey = key
    const persisted = [...loadQueue(key), ...(previousKey ? loadQueue(previousKey) : [])]
      .map(queueEntry).filter(Boolean)
    if (store.sessionStatus !== 'ready') return
    for (const entry of persisted) {
      if (legacyQueueMigrations.has(entry.clientActionId)) continue
      legacyQueueMigrations.add(entry.clientActionId)
      void sendTurn(entry.text, { fromQueue: true, clientActionId: entry.clientActionId }).then((outcome) => {
        if (outcome === 'failed') {
          legacyQueueMigrations.delete(entry.clientActionId)
          return
        }
        saveQueue(key, [])
        if (previousKey) saveQueue(previousKey, [])
      })
    }
  }

  // Shows this thread's sends Nano never acknowledged, from storage, so a
  // reload or a rotated session id keeps them. Old pending-operation keys are
  // removed once per page.
  function adoptUnsent() {
    if (!legacyPendingPurged) {
      legacyPendingPurged = true
      purgeLegacyPendingStorage()
    }
    const threadId = currentThreadId()
    // A send made before the native thread id landed is rebound to it.
    if (threadId) {
      for (const send of loadUnsent({ sessionId: id })) {
        persistUnsent({ ...send, provider: send.provider || store.provider, threadId })
      }
    }
    const found = loadUnsent({ provider: store.provider, threadId, sessionId: id })
      .filter((send) => !copies.has(send.clientActionId) && !postsInFlight.has(send.clientActionId))
    // Adoption posts nothing itself: the queue moves once, after every found
    // send is in place.
    adopting = true
    for (const send of found) {
      stepUnsent(send.clientActionId, { type: 'loaded', send })
      // A copy from an earlier page may belong to a prompt Nano took or
      // refused after that page lost its answer: ask Nano once. A prompt Nano
      // has a record of, in any state, is Nano's from then on.
      void deliveryAnswer(send.clientActionId).then((answer) => {
        if (answer.taken) stepUnsent(send.clientActionId, { type: 'answered' })
        else if (answer.refused) stepUnsent(send.clientActionId, { type: 'refused', text: send.text })
      })
    }
    adopting = false
    if (found.length) {
      drain()
      showUnsent()
    }
  }

  // stepUnsent applies one event to this tab's copy of a send and carries out
  // what unsentStep decides: forget the stored record, hand text back, and
  // show exactly the copies that remain unacknowledged.
  function stepUnsent(actionID, event) {
    const before = copies.get(actionID)
    const { entry, forget, handBack } = unsentStep(before, event)
    copies.set(actionID, entry)
    if (forget) forgetUnsent(actionID)
    if (handBack) handBackText(actionID, handBack)
    if (event.type === 'lost') disarm(actionID) // its next try waits a longer backoff
    if (entry.check === 'reading' && before?.check !== 'reading') void readEcho(actionID)
    drain()
    showUnsent()
    if (!entry.shown && store.sendError?.unsent && store.sendError.clientActionId === actionID) store.sendError = null
    return copies.get(actionID)
  }

  // showUnsent renders the shown copies oldest first, with what each row says
  // about itself: its state, its countdown, and whether it waits behind an
  // older one.
  function showUnsent() {
    const first = frontCopy()
    const shown = [...copies.values()].filter((copy) => copy.shown)
      .sort((a, b) => (a.send.createdAt || 0) - (b.send.createdAt || 0))
      .map(({ send, retry, attempts, posted, inFlight }) => ({
        ...send, retry, attempts, posted, inFlight,
        retryAt: retryDueAt.get(send.clientActionId) ?? 0,
        behind: Boolean(first) && first.send.clientActionId !== send.clientActionId && !send.stopped,
      }))
    const key = (rows) => JSON.stringify(rows)
    if (key(shown) !== key(store.unsent)) store.unsent = shown
    const echoes = [...copies.values()].filter((copy) => copy.echo)
      .sort((a, b) => (a.send.createdAt || 0) - (b.send.createdAt || 0))
      .map(({ send, slow }) => ({ clientActionId: send.clientActionId, text: send.text, slow }))
    if (key(echoes) !== key(store.echoes)) store.echoes = echoes
  }

  // A thread's copies reach Nano one at a time, oldest first by their stored
  // createdAt: a copy waits while any older one is not yet Nano's, even one
  // whose POST is out. A copy the person discarded (stopped) waits for no one
  // and blocks no one.
  function waiting(copy) {
    return !copy.acked && !copy.ended && (copy.shown || copy.inFlight) && !copy.send?.stopped
  }

  function frontCopy() {
    let first = null
    for (const copy of copies.values()) {
      if (waiting(copy) && (!first || (copy.send.createdAt || 0) < (first.send.createdAt || 0))) first = copy
    }
    return first
  }

  function disarm(actionID) {
    clearTimeout(retryTimers.get(actionID))
    retryTimers.delete(actionID)
    retryDueAt.delete(actionID)
  }

  // drain moves the thread's queue: only the front copy may post. One never
  // posted goes as soon as Nano can be reached; a posted one on its backoff,
  // or at once (now) when Nano answers again. Nothing posts while the front
  // is out.
  function drain(now = false) {
    if (disposed || adopting) return
    const first = frontCopy()
    for (const actionID of retryTimers.keys()) if (actionID !== first?.send.clientActionId) disarm(actionID)
    if (!first) return
    const actionID = first.send.clientActionId
    if (first.inFlight) {
      disarm(actionID)
      return
    }
    if (!first.posted) {
      if (connection.status !== 'offline') postStored(actionID)
      return
    }
    if (!first.retry) return
    if (now) postStored(actionID)
    else if (!retryTimers.has(actionID)) {
      const dueAt = Date.now() + retryDelayMs(first.attempts)
      retryDueAt.set(actionID, dueAt)
      retryTimers.set(actionID, setTimeout(() => drain(true), dueAt - Date.now()))
    }
  }

  // postStored sends a copy as its stored record has it, never as this tab
  // remembers it: another tab may have discarded or stopped it, and
  // then this tab stands down.
  function postStored(actionID) {
    disarm(actionID)
    if (postsInFlight.has(actionID)) return
    // Another tab may have stored an older send to this thread since this
    // tab last looked: it goes first. Adopting it may already have posted
    // this one, too.
    adoptUnsent()
    const copy = copies.get(actionID)
    if (!copy || copy.inFlight || postsInFlight.has(actionID) || frontCopy() !== copy) return
    const stored = readUnsent(actionID)
    if (!stored || stored.stopped) {
      stepUnsent(actionID, { type: 'gone' })
      return
    }
    recordBrowserDiagnostic('send.retry', diagnosticContext({ actionId: actionID }))
    void sendTurn(stored.text, { clientActionId: actionID, nextTurn: stored.nextTurn, createdAt: stored.createdAt })
  }

  // The stream lists this action: Nano has it.
  function acknowledgeSend(actionID) {
    const copy = copies.get(actionID)
    if (copy?.shown || copy?.echo) stepUnsent(actionID, { type: 'listed' })
  }

  // The stream names these actions saved: their saved rows replace the echoes.
  function settleEchoes(actionIDs) {
    for (const actionID of actionIDs) {
      if (copies.get(actionID)?.echo) stepUnsent(actionID, { type: 'saved' })
    }
  }

  // A tail lists listedIDs as its live prompts. An echo it neither lists nor
  // names saved (settleEchoes ran first) may have settled while the stream
  // was away, which no message will name now: once Nano has the send, a read
  // of its record decides (unsentStep: check).
  function echoTail(listedIDs) {
    for (const [actionID, copy] of copies) {
      if (copy.echo && !listedIDs.has(actionID)) stepUnsent(actionID, { type: 'missed' })
    }
  }

  // readEcho is the one read a copy's check asks for (unsentStep: check).
  async function readEcho(actionID) {
    const answer = await deliveryAnswer(actionID)
    stepUnsent(actionID, { type: 'checked', settled: ECHO_SETTLED_STATES.has(answer.state) })
  }

  // handBackText queues a refused prompt's text for the composer after any text
  // still waiting there, so two refusals in one moment lose neither.
  function handBackText(clientActionId, text) {
    const waiting = store.restoreDraft
    const texts = waiting ? (Array.isArray(waiting.texts) ? waiting.texts : [waiting.text]) : []
    store.restoreDraft = { clientActionId, text, texts: [...texts, text], nonce: Date.now() }
  }


  function canonicalPosture(metadata) {
    const fields = metadata?.fields ?? {}
    const value = (field) => {
      const current = fields[field]
      if (!current) return ''
      // The server is the sole authority for display precedence. In
      // particular, an imported historical request yields to its observed
      // legacy fold while remaining available separately for resume/audit.
      const raw = current.displayValue
      return typeof raw === 'string' ? raw : ''
    }
    return {
      model: value('model'),
      effort: value('effort'),
      permissionMode: value('permissionMode'),
      serviceTier: value('serviceTier'),
    }
  }

  // Refetches precisely the hosted metadata view after a lossy revision hint.
  // It deliberately does not consult transcript or tab-cache metadata if the
  // canonical control plane cannot answer.
  // One read at a time; calls during it share one follow-up read, which
  // starts after them and so sees what they asked for.
  function refreshHostedMetadata() {
    if (!metadataRead) {
      metadataRead = readHostedMetadata().finally(() => { metadataRead = null })
      return metadataRead
    }
    metadataFollowUp ??= metadataRead.then(() => {
      metadataFollowUp = null
      return refreshHostedMetadata()
    })
    return metadataFollowUp
  }

  // The one acceptance rule for every metadata source: a read that started
  // before the accepted one never replaces it, whether it carries metadata or
  // reports it unavailable; and since start order is not server order, an
  // older canonical revision never replaces a newer one either. The list's
  // displayed values are not a full view, so they keep a watermark of their
  // own (applySummary).
  function acceptMetadata(seq, metadata) {
    if (seq < appliedMetadataSeq) return false
    // A failure that started before the list read now shown cannot undo it; a
    // full view from that read is still welcome.
    if (!metadata && seq < listedPostureSeq) return false
    if (metadata && metadata.revision < (store.hostedMetadata?.revision ?? 0)) return false
    appliedMetadataSeq = seq
    store.hostedMetadata = metadata
    store.posture = metadata ? canonicalPosture(metadata) : {}
    store.metadataUnavailable = !metadata
    return true
  }

  // A runtime event names the newest canonical revision; re-read this
  // thread's metadata while the store holds an older one.
  function noteMetadataRevision(revision) {
    if (revision > (store.hostedMetadata?.revision ?? 0)) void refreshHostedMetadata()
  }

  async function readHostedMetadata() {
    const hostedThreadId = store.hostedThreadId
    if (!hostedThreadId) return refreshMeta()
    const previousRevision = store.hostedMetadata?.revision
    const seq = beginMetadataRead()
    try {
      const res = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(hostedThreadId)}/metadata`)
      if (!res.ok) throw new Error(`metadata status ${res.status}`)
      const metadata = await res.json()
      if (disposed || hostedThreadId !== store.hostedThreadId) return
      if (!acceptMetadata(seq, metadata)) return
      if (metadata?.revision !== previousRevision) void resolveNativeIdentity({ force: true })
    } catch {
      acceptMetadata(seq, null)
    }
  }

  // A session snapshot carries runtime state as well as an embedded canonical
  // view. It is used for initial/reconnect hydration; revision hints use the
  // narrower canonical metadata endpoint above.
  async function refreshMeta() {
    const previousThreadHint = store.threadId
    const previousMetadataRevision = store.hostedMetadata?.revision
    const seq = beginMetadataRead()
    try {
      const res = await fetch(`/api/sessions/${id}/state`)
		if (!res.ok) {
			acceptMetadata(seq, null)
			return
		}
      const meta = await res.json()
      if (disposed) return
      store.meta = meta
		if (meta?.metadataUnavailable) acceptMetadata(seq, null)
		else if (meta?.metadata) acceptMetadata(seq, meta.metadata)
		else if (seq >= appliedMetadataSeq) store.metadataUnavailable = false
		store.metadataDegraded = Boolean(meta?.metadataDegraded)
      store.provider = meta?.provider ?? store.provider
      store.threadId = meta?.threadId ?? store.threadId
      const threadHintChanged = Boolean(previousThreadHint && store.threadId && previousThreadHint !== store.threadId)
      const metadataChanged = meta?.metadata?.revision !== undefined && meta.metadata.revision !== previousMetadataRevision
      if (threadHintChanged || metadataChanged) {
        void resolveNativeIdentity({ force: true, clear: threadHintChanged })
      }
      adoptPersistedQueue() // thread identity may have just landed
      adoptUnsent()
      // Seed turn state from the snapshot ONLY until the wire has spoken: a
      // pane opened mid-turn has no events yet (a codex re-attach replays
      // none), while a pane with live traffic must never let a stale snapshot
      // overwrite what it watched happen.
      if (!projectionRuntimeSeen && !wireTurnSeen && typeof meta?.turnActive === 'boolean') {
        store.turnActive = meta.turnActive
      }
      if (meta?.turnOptions) store.turnOptions = meta.turnOptions
      if (Array.isArray(meta?.pendingInteractions) && !streamOwnsStatus()) {
        // The snapshot is authoritative for what is parked right now; feed
        // events that already landed describe the same set, so replacing is
        // convergence rather than a lost update.
        store.pendingInteractions = meta.pendingInteractions
      }
      if (meta?.status) applySummary(meta, seq)
	} catch {
		// A hosted metadata read has no safe local fallback. Keep transcript and
		// runtime state, but blank standing provider facts until a later refetch.
		acceptMetadata(seq, null)
    }
  }

  // loadMcpServers fetches the tool registry on demand (first Tool Registry
  // expand, then on invalidation). The server caches it, so a re-expand is
  // free; force=true is the wire-triggered refresh of an already-shown panel.
  async function loadMcpServers(force = false) {
    if (store.provider === 'claude' || store.provider === 'grok') return
    if (store.mcpLoading || (store.mcp && !force)) return
    store.mcpLoading = true
    store.mcpError = null
    try {
      const res = await fetch(`/api/sessions/${id}/mcp`)
      if (!res.ok) throw new Error((await readAPIError(res, `tool registry failed: ${res.status}`)).message)
      const inventory = await res.json()
      if (disposed) return
      store.mcp = inventory
    } catch (err) {
      if (!store.mcp) store.mcpError = String(err.message ?? err)
    } finally {
      store.mcpLoading = false
    }
  }

  // loadSkills rereads the server's cached list on every panel open: a cold
  // cache answers an empty list at once while it fetches the real one.
  async function loadSkills() {
    if (store.provider !== 'codex') return
    store.skillsError = null
    try {
      const res = await fetch(`/api/sessions/${id}/skills`)
      if (!res.ok) throw new Error((await readAPIError(res, `skills list failed: ${res.status}`)).message)
      const skills = await res.json()
      if (!disposed) store.skills = skills
    } catch (err) {
      store.skillsError = String(err.message ?? err)
    }
  }

  // A server startup transition invalidated the server-side cache; only
  // re-fetch if the panel is actually showing the registry. Debounced because
  // several servers become ready in one burst.
  function scheduleMcpRefresh() {
    if (!store.mcp) return
    clearTimeout(mcpRefreshTimer)
    mcpRefreshTimer = setTimeout(() => loadMcpServers(true), 800)
  }

  // applySummary ingests the runtime status from a session summary or /state.
  // The starting→ready flip fires the channel's single tail revalidation (the
  // R3 freshness edge); a failed resume surfaces its error on the existing
  // lastError chip.
  // A summary with no read sequence is cached data (a store's first summary),
  // so it counts as the oldest read.
  function applySummary(summary, metadataSeq = 0) {
    const previousHostedThreadId = store.hostedThreadId
    const previousThreadHint = store.threadId
    store.provider = summary.provider ?? store.provider
    store.hostedThreadId = summary.hostedThreadId ?? store.hostedThreadId
    store.runningTurnInput = summary.runningTurnInput ?? store.runningTurnInput
    store.threadId = summary.threadId ?? store.threadId
		const previousMetadataRevision = store.hostedMetadata?.revision ?? listedMetadataRevision
		// A readable canonical view is direct evidence that metadata is
		// available; acceptMetadata keeps a list read that started earlier from
		// undoing a newer read either way.
		if (summary.metadataUnavailable) acceptMetadata(metadataSeq, null)
		else if (summary.metadata) {
			// Displayed values only: they paint posture until the store's own
			// read lands, and never replace that read.
			listedMetadataRevision = summary.metadata.revision
			if (!store.hostedMetadata && metadataSeq >= appliedMetadataSeq && metadataSeq >= listedPostureSeq) {
				listedPostureSeq = metadataSeq
				store.posture = canonicalPosture(summary.metadata)
				store.metadataUnavailable = false
			}
		} else {
			if (summary.metadataUnavailable === false && metadataSeq >= appliedMetadataSeq) store.metadataUnavailable = false
			// Discovered non-hosted threads still use their transcript projection.
			// Once a hosted id exists there is no local precedence fallback.
			if (summary.posture && !store.hostedThreadId) store.posture = summary.posture
		}
		if (summary.metadataDegraded !== undefined) store.metadataDegraded = Boolean(summary.metadataDegraded)
		// Runtime notifications are lossy hints. A changed hosted revision in a
		// list summary immediately re-fetches the complete canonical view, and
		// that read resolves the native identity again when the revision moved.
		if (summary.metadata?.revision !== undefined && previousMetadataRevision !== undefined && summary.metadata.revision > previousMetadataRevision) {
			void refreshHostedMetadata()
		}
		const hostedThreadChanged = previousHostedThreadId !== store.hostedThreadId
		const threadHintChanged = Boolean(previousThreadHint && store.threadId && previousThreadHint !== store.threadId)
		if (hostedThreadChanged) resetNativeIdentity()
		else if (threadHintChanged) void resolveNativeIdentity({ force: true, clear: true })
    if (!streamOwnsStatus()) store.sessionStatus = summary.status
    if (summary.lastTurnEndedAt !== undefined) store.lastTurnEndedAt = summary.lastTurnEndedAt
    adoptPersistedQueue()
    adoptUnsent()
    // Same seed rule as refreshMeta: the summary poll knows turn state the
    // wire hasn't shown this tab yet (re-attach replays nothing on codex).
    if (!projectionRuntimeSeen && !wireTurnSeen && typeof summary.turnActive === 'boolean') {
      store.turnActive = summary.turnActive
    }
    if (summary.status === 'exited' && summary.error && !streamOwnsStatus()) store.lastError = summary.error
  }

  function adoptSessionHandoff(previousSession) {
    const previous = previousSession?.store
    if (!previous) return false
    if (store.provider && previous.provider && store.provider !== previous.provider) return false
    if (currentThreadId() && (previous.threadId || previous.meta?.threadId) &&
      currentThreadId() !== (previous.threadId || previous.meta?.threadId)) return false

    for (const send of previous.unsent ?? []) {
      if (!copies.has(send.clientActionId)) stepUnsent(send.clientActionId, { type: 'loaded', send })
    }
    if (!store.restoreDraft && previous.restoreDraft) store.restoreDraft = previous.restoreDraft
    if (!store.sendError && previous.sendError) store.sendError = previous.sendError
	recordBrowserDiagnostic('store.handoff', diagnosticContext({ previousSessionId: previousSession.id }))
    // The predecessor's 404 recovery may still be posting to this session.
    if (previous.sending > 0) {
      wireTurnSeen = true
      store.turnActive = true
    }
    return true
  }

  function connect() {
    // Panes open file streams independently of /state. The raw Event Feed
    // opens only when its right-pane tab is mounted.
    void refreshMeta()
    if (store.hostedThreadId) void resolveNativeIdentity()
  }

  function disconnect() {
    disposed = true
    stopRetryWake()
    for (const timer of retryTimers.values()) clearTimeout(timer)
    retryTimers.clear()
    retryDueAt.clear()
    disconnectFeed()
    clearTimeout(mcpRefreshTimer)
    clearNativeIdentityRequest()
    store.connected = false
  }

  async function loadOlderEvents() {
    if (store.eventsLoadingOlder || store.events.length === 0) return
    const beforeSeq = store.events[0].seq
    if (beforeSeq <= 1) {
      store.eventsHasOlder = false
      return
    }
    store.eventsLoadingOlder = true
    store.eventsOlderError = ''
    try {
      const res = await fetch(`/api/sessions/${id}/events/page?beforeSeq=${beforeSeq}&limit=500`)
      if (!res.ok) throw new Error((await readAPIError(res, `event history failed: ${res.status}`)).message)
      const page = await res.json()
      const older = page.events ?? []
      for (const evt of older) annotate(evt)
      const bySeq = new Map()
      for (const evt of [...older, ...store.events]) bySeq.set(evt.seq, evt)
      store.events = [...bySeq.values()].sort((a, b) => a.seq - b.seq)
      if (store.events.length > MAX_BROWSE_EVENTS) store.events.splice(MAX_BROWSE_EVENTS)
      store.eventsHasOlder = Boolean(page.hasMore) || (store.events[0]?.seq ?? 1) > 1
    } catch (err) {
      store.eventsOlderError = String(err.message ?? err)
      store.lastError = `load older events failed: ${store.eventsOlderError}`
    } finally {
      store.eventsLoadingOlder = false
    }
  }

  // Composer submissions cross the ownership boundary only while the session
  // accepts prompts. The network is not asked first: the send is its own
  // probe, and its text is stored before it posts, so a send that gets no
  // answer stays below the composer and retries until Nano answers.
  function canSubmitTurn() {
    if (promptAllowed(store.sessionStatus)) return true
    recordBrowserDiagnostic('send.blocked', diagnosticContext({ code: 'session_not_ready' }))
    return false
  }

  // A committed prompt goes to Nano at once unless an older send to this
  // thread has no answer yet or Nano is known to be unreachable: then it is
  // held, stored, until its turn. Nano decides atomically whether to start it
  // or persist it behind the active turn. Returns 'sent' | 'failed' |
  // 'uncertain' | 'held'. The composer has already handed its text over;
  // only a definitive failure transfers it back.
  // nextTurn holds the message for the next turn instead of the running one.
  // clientActionId names the send, so the pane can follow its row.
  async function submitTurn(text, { nextTurn = false, clientActionId = '' } = {}) {
    // Another open tab may have stored an older send to this thread.
    adoptUnsent()
    if (!frontCopy() && connection.status !== 'offline') return sendTurn(text, { nextTurn, clientActionId })
    // An older send to this thread has no answer yet, or Nano cannot be
    // reached: this one is stored and waits its turn, unposted.
    const send = newSend(text, { clientActionId, nextTurn, held: true })
    if (!persistUnsent(send)) {
      stepUnsent(send.clientActionId, { type: 'held', send })
      stepUnsent(send.clientActionId, { type: 'refused', text: send.text })
      store.sendError = { reason: 'this browser could not save the message, so it was not sent', fromQueue: false }
      return 'failed'
    }
    stepUnsent(send.clientActionId, { type: 'held', send })
    return 'held'
  }

  function clearSendError() {
    store.sendError = null
  }

  // Discard ends the prompt in Nano for good, so no reload or other tab brings
  // it back: even a send Nano never acknowledged may have reached it. Its text
  // returns to the composer (unsentStep decides whether). Nano may refuse,
  // and says why.
  async function discardPendingSend(actionID, text = '') {
    const copy = copies.get(actionID)
    if (copy?.ending || copy?.ended) return
    // A send never posted cannot have reached Nano: it ends here, and its
    // text goes back to the composer.
    if (copy?.shown && !copy.posted && readUnsent(actionID)?.held) {
      stepUnsent(actionID, { type: 'ending' })
      stepUnsent(actionID, { type: 'ended' })
      return
    }
    // This browser's stored copy says stopped before Nano is asked, so no
    // reload retries a send the person discarded: any copy this tab may still
    // post, shown or with its POST out (its record exists until it settles).
    // Unable to store that, it does not discard, as a Send it cannot store is
    // not sent. A settled copy's record is never written again.
    const live = copy && !copy.acked && !copy.ended && copy.send?.text !== undefined
    if (live && !persistUnsent({ ...copy.send, stopped: true })) {
      store.sendError = { reason: 'This browser could not save the Discard, so it was not done. The message is still below.', refusal: true }
      return
    }
    stepUnsent(actionID, { type: 'ending' })
    const { ended, httpStatus } = await cancelServerOperation(actionID)
    if (!ended) stepUnsent(actionID, { type: 'endFailed' })
    else stepUnsent(actionID, { type: 'ended', text })
    recordBrowserDiagnostic('send.discard', diagnosticContext({
      actionId: actionID, outcome: ended ? `ended:${ended}` : 'failed', httpStatus,
    }))
    if (ended && store.sendError?.clientActionId === actionID) store.sendError = null
  }

  // Asks Nano to end the prompt. ended is the state Nano reports it ended in
  // ('absent' when Nano does not have it), or '' when it did not, with the
  // reason shown.
  async function cancelServerOperation(actionID) {
	let res
	try {
	  res = await fetch(`/api/sessions/${id}/turn-delivery/${encodeURIComponent(actionID)}`, { method: 'DELETE' })
	} catch (err) {
	  store.sendError = { reason: `could not cancel the message (${err.message ?? err})`, fromQueue: true }
	  return { ended: '', httpStatus: 0 }
	}
	if (!res.ok) {
	  await showActionFailure(res, actionID, `could not cancel the message (${res.status})`)
	  return { ended: '', httpStatus: res.status }
	}
	let body = {}
	try { body = await res.json() } catch { body = {} }
	const state = body?.state === '' ? 'absent' : body?.state
	// Only Nano's own answer for this action ends it here.
	if (body?.canceled !== true || body.clientActionId !== actionID || !ENDED_STATES.has(state)) {
	  store.sendError = { reason: 'Nano did not confirm the cancel', fromQueue: true }
	  return { ended: '', httpStatus: res.status }
	}
	return { ended: state, httpStatus: res.status }
  }

  // showActionFailure shows why Nano did not do a Resend or Discard. A refusal
  // is Nano's decision from the prompt's state, in words shown as they are;
  // it also proves Nano has the prompt, so this tab's copy of it is Nano's.
  async function showActionFailure(res, actionID, fallback) {
    const failure = await readAPIError(res, fallback)
    if (failure.code === 'prompt_action_refused' && failure.details?.clientActionId === actionID) {
      stepUnsent(actionID, { type: 'answered' })
      store.sendError = { reason: failure.message, refusal: true }
      return
    }
    store.sendError = { reason: failure.message, fromQueue: true }
  }

  // deliveryAnswer asks Nano once what became of an action whose POST answer
  // was lost: taken (Nano has or had it; any later refusal parks it on the
  // thread), refused (with Nano's label), or neither when Nano does not know
  // it or cannot be asked within the send deadline.
  async function deliveryAnswer(actionID) {
    const controller = new AbortController()
    const timeout = setTimeout(() => controller.abort(), SEND_REQUEST_TIMEOUT_MS)
    let res
    let body = {}
    try {
      res = await fetch(`/api/sessions/${id}/turn-delivery/${encodeURIComponent(actionID)}`, { signal: controller.signal })
      if (!res.ok) return {}
      body = await res.json()
    } catch {
      return {}
    } finally {
      clearTimeout(timeout)
    }
    // Only Nano's own answer for this action counts.
    if (body?.clientActionId !== actionID || !body.state) return {}
    if (body.state === 'refused') return { refused: true, reason: body.label || 'Nano did not send it', state: body.state }
    return { taken: true, state: body.state }
  }

  // A send Nano never acknowledged resends under its own action id, which
  // Nano answers once however often it arrives. Any other prompt is Nano's,
  // and Nano resends it: it decides from the prompt's state whether it may,
  // and sends the same words once as a new prompt.
  async function resendPendingSend(actionID) {
    const copy = copies.get(actionID)
    const unsent = copy?.shown ? copy.send : null
    let outcome
    if (unsent) {
      // A second click while this action is posting would race its outcome
      // and could return the same text to the composer twice.
      if (postsInFlight.has(actionID)) return 'uncertain'
      if (!unsent.stopped) {
        // The front of the queue tries now; the rest keep their turn.
        if (frontCopy() === copy) postStored(actionID)
        return 'uncertain'
      }
      // The person's own Resend of a send they had discarded.
      outcome = await sendTurn(unsent.text, { clientActionId: actionID, nextTurn: unsent.nextTurn })
    } else {
      outcome = await resendServerPrompt(actionID)
    }
    recordBrowserDiagnostic('send.resend', diagnosticContext({ actionId: actionID, outcome }))
    return outcome
  }

  async function resendServerPrompt(actionID) {
    let res
    try {
      res = await fetch(`/api/sessions/${id}/turn-delivery/${encodeURIComponent(actionID)}/resend`, { method: 'POST' })
    } catch (err) {
      store.sendError = { reason: `could not resend the message (${err.message ?? err})`, fromQueue: true }
      return 'failed'
    }
    if (!res.ok) {
      await showActionFailure(res, actionID, `could not resend the message (${res.status})`)
      return 'failed'
    }
    // The new prompt owns the text: no late answer to this tab's original
    // send may show it again or hand its text back.
    stepUnsent(actionID, { type: 'resent' })
    store.sendError = null
    return 'sent'
  }

  function consumeRestoreDraft() {
    const draft = store.restoreDraft
    store.restoreDraft = null
    return draft
  }

  // newSend is one send's record. createdAt is its place in the thread's
  // order, kept by every retry; a new send's
  // comes strictly after every send this tab knows, so two never tie.
  function newSend(text, { clientActionId = '', nextTurn = false, createdAt = 0, held = false } = {}) {
    let newest = 0
    for (const copy of copies.values()) newest = Math.max(newest, copy.send?.createdAt || 0)
    return {
      clientActionId: clientActionId || newClientActionId(),
      provider: store.provider ?? '',
      threadId: currentThreadId(),
      sessionId: id,
      text,
      createdAt: createdAt || copies.get(clientActionId)?.send?.createdAt || Math.max(Date.now(), newest + 1),
      // Part of the request: a retry of this action must ask the same.
      ...(nextTurn ? { nextTurn: true } : {}),
      ...(held ? { held: true } : {}),
    }
  }

  async function sendTurn(text, { fromQueue = false, clientActionId = '', nextTurn = false, createdAt = 0 } = {}) {
    // Stored without the held mark before the POST leaves: a reload must not
    // take a send Nano may have for one it never saw.
    const send = newSend(text, { clientActionId, nextTurn, createdAt })
	recordBrowserDiagnostic('send.echo', diagnosticContext({ actionId: send.clientActionId }))
    // The composer has already dropped its saved draft. If this browser cannot
    // save the send either, a reload would lose the text, so the send is
    // refused before it starts and the text returns to the composer.
    if (!persistUnsent(send)) {
      stepUnsent(send.clientActionId, { type: 'posting', send })
      stepUnsent(send.clientActionId, { type: 'refused', text: send.text, quiet: fromQueue })
      store.sendError = { reason: 'this browser could not save the message, so it was not sent', fromQueue }
      return 'failed'
    }
    stepUnsent(send.clientActionId, { type: 'posting', send })
    postsInFlight.add(send.clientActionId)
    store.sending += 1
    try {
      return await postTurnOperation(send, fromQueue)
    } finally {
      postsInFlight.delete(send.clientActionId)
      store.sending -= 1
    }
  }

  async function postTurnOperation(send, fromQueue) {
    const isCodex = store.provider === 'codex'
    const controller = new AbortController()
    const requestBody = JSON.stringify({ text: send.text, clientActionId: send.clientActionId, ...(send.nextTurn ? { nextTurn: true } : {}) })
    const postTurn = (sessionId) => {
	  recordBrowserDiagnostic('send.post.start', diagnosticContext({
		actionId: send.clientActionId,
		sessionId,
		status: fromQueue ? 'queued' : 'direct',
	  }))
	  return fetch(`/api/sessions/${sessionId}/turn`, {
		method: 'POST',
		headers: { 'content-type': 'application/json' },
		body: requestBody,
		signal: controller.signal,
	  }).then((response) => {
		recordBrowserDiagnostic('send.post.response', diagnosticContext({
		  actionId: send.clientActionId,
		  sessionId,
		  httpStatus: response.status,
		  outcome: response.ok ? 'ok' : 'failed',
		}))
		return response
	  })
	}
    const wireTurnSeenBeforeSend = wireTurnSeen
    wireTurnSeen = true
    if (isCodex) {
      store.turnActive = true
    } else if (store.provider === 'claude' || store.provider === 'grok') {
      // Claude has no turn/started notification, and Grok's ACP prompt
      // acceptance is not a synchronous ack either; mark active optimistically
      // and clear on the result/terminal update.
      store.turnActive = true
    }
    let res
    let failure = null // a failed answer's body, once read
    const slow = setTimeout(() => stepUnsent(send.clientActionId, { type: 'slow' }), SEND_SLOW_MS)
    const timeout = setTimeout(() => controller.abort(), SEND_REQUEST_TIMEOUT_MS)
    const recoverSession = async () => {
      try {
        return await missingSessionRecovery({
          sessionId: id,
          provider: store.provider ?? send.provider,
          threadId: currentThreadId() || send.threadId,
        })
      } catch {
        // Registry refresh failed; the caller goes on with the answer it has.
        return null
      }
    }
    const refused = (reason) => {
      recordBrowserDiagnostic('send.post.refused', diagnosticContext({ actionId: send.clientActionId, httpStatus: res?.status }))
      store.sendError = { reason, fromQueue }
      stepUnsent(send.clientActionId, { type: 'refused', text: send.text, quiet: fromQueue })
      wireTurnSeen = wireTurnSeenBeforeSend
      store.turnActive = false
      return 'failed'
    }
    // noAnswer is a POST that got no answer from Nano: the connection failed,
    // the browser's deadline passed, or a front answered for Nano. Nano may
    // have taken the request just before, so the browser keeps the send (and
    // its stored text), retrying it under its action id until Nano answers;
    // the stream hands it to Nano if Nano lists it. Turn state goes back to
    // the server: with wireTurnSeen false again, the next /state or summary
    // snapshot seeds the truth.
    const noAnswer = async (reason) => {
      wireTurnSeen = false
      // The answer was lost, not necessarily the send: Nano may have taken or
      // refused it, and its stream may already have shown and dropped its row.
      // One read asks Nano what it did; no answer keeps the text here.
      const answer = await deliveryAnswer(send.clientActionId)
      if (answer.refused) return refused(answer.reason)
      if (answer.taken) {
        stepUnsent(send.clientActionId, { type: 'answered' })
        store.sendError = null
        return 'sent'
      }
      const copy = stepUnsent(send.clientActionId, { type: 'lost' })
      if (!copy.shown) return copy.acked ? 'sent' : 'failed'
      store.sendError = { reason, fromQueue, unsent: true, clientActionId: send.clientActionId }
      return 'uncertain'
    }
    try {
      res = await postTurn(id)
      if (res.status === 404) {
        failure = await readAPIError(res, `server rejected the turn (${res.status})`)
        if (failure.code !== 'not_found' || failure.message !== 'no such session' || !missingSessionRecovery) return refused(failure.message)
        const successor = await recoverSession()
        if (!successor?.id || successor.id === id) return refused(failure.message)
        res = await postTurn(successor.id)
        failure = null
      } else if (GATEWAY_AMBIGUOUS_STATUSES.has(res.status)) {
        failure = await readAPIError(res, `server rejected the turn (${res.status})`)
        if (!failure.envelope && missingSessionRecovery) {
          // A front answered for a Nano that may be mid-restart, and the
          // session registry may have rotated under this id. Refresh and post
          // once more at once, against whichever session now carries the
          // thread: the same id if it survived, its successor if not.
          const target = await recoverSession()
          if (target?.id) {
            res = await postTurn(target.id)
            failure = null
          }
        }
      }
    } catch (err) {
	  recordBrowserDiagnostic('send.post.error', diagnosticContext({
		actionId: send.clientActionId,
		code: err?.name || 'error',
	  }))
      return await noAnswer(err?.name === 'AbortError'
        ? `Nano did not answer within ${SEND_REQUEST_TIMEOUT_MS / 1000} seconds`
        : `the connection failed (${err.message ?? err})`)
    } finally {
      clearTimeout(slow)
      clearTimeout(timeout)
    }
    if (!res.ok && res.status !== 202) {
      failure ??= await readAPIError(res, `server rejected the turn (${res.status})`)
      if (GATEWAY_AMBIGUOUS_STATUSES.has(res.status) && !failure.envelope) {
        recordBrowserDiagnostic('send.post.gateway', diagnosticContext({ actionId: send.clientActionId, httpStatus: res.status }))
        return noAnswer(`Nano could not be reached (${res.status})`)
      }
      return refused(failure.message)
    }
    // Nano has the action; its stream shows the prompt and its state from here.
    stepUnsent(send.clientActionId, { type: 'answered' })
    store.sendError = null
    // A 202 holds the prompt for later; the turn state comes from the server.
    if (res.status === 202) wireTurnSeen = false
    return 'sent'
  }

  // interruptTurn stops the turn in flight. The completion race is expected,
  // not exceptional: the server answers 200 with interrupted:false when the
  // turn already ended, and that must stay silent — the operator asked for the
  // turn to stop and it did. Only a real transport/server failure is reported.
  async function interruptTurn() {
    if (store.interrupting) return
    store.interrupting = true
    try {
      const res = await fetch(`/api/sessions/${id}/interrupt`, { method: 'POST' })
      if (!res.ok) store.lastError = `stop failed: ${(await readAPIError(res, `request failed (${res.status})`)).message}`
    } catch (err) {
      store.lastError = `stop failed: ${err.message ?? err}`
    } finally {
      store.interrupting = false
    }
  }

  async function setTurnOptions(patch) {
    try {
      const res = await fetch(`/api/sessions/${id}/turn-options`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(patch),
      })
      if (!res.ok) {
        store.lastError = `turn options failed: ${(await readAPIError(res, `request failed (${res.status})`)).message}`
        return null
      }
      store.turnOptions = await res.json()
      return store.turnOptions
    } catch (err) {
      store.lastError = `turn options failed: ${err.message ?? err}`
      return null
    }
  }

  // answerInteraction responds to a parked server request. Nano's HTTP reply
  // is the answering tab's acknowledgement, so success removes the card at
  // once; the projection event remains the idempotent cross-tab confirmation.
  // 409 means another tab or an auto-resolution timer won the race and also
  // collapses silently. A real delivery failure leaves the card answerable.
  async function answerInteraction(requestID, body) {
    const interactionId = diagnosticInteractionID(requestID)
    const removeLocally = (outcome) => {
      store.pendingInteractions = store.pendingInteractions.filter((i) => i.id !== requestID)
      recordBrowserDiagnostic('interaction.resolve.local', diagnosticContext({ interactionId, outcome }))
    }
    recordBrowserDiagnostic('interaction.post.start', diagnosticContext({
      interactionId,
      status: body?.decision,
    }))
    try {
      const res = await fetch(`/api/sessions/${id}/interactions/${encodeURIComponent(requestID)}`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      })
      recordBrowserDiagnostic('interaction.post.response', diagnosticContext({
        interactionId,
        httpStatus: res.status,
        outcome: res.ok ? 'ok' : res.status === 409 ? 'already_resolved' : 'failed',
      }))
      if (res.status === 409) {
        removeLocally('already_resolved')
        return null
      }
      if (!res.ok) return (await readAPIError(res, `answer failed: ${res.status}`)).message
      removeLocally('acknowledged')
      return null
    } catch (err) {
      recordBrowserDiagnostic('interaction.post.error', diagnosticContext({
        interactionId,
        code: err?.name || 'error',
      }))
      return `answer failed: ${err.message ?? err}`
    }
  }

  function ingest(evt) {
    annotate(evt)
    store.events.push(evt)
    if (store.events.length > MAX_FEED_EVENTS) store.events.splice(0, store.events.length - MAX_FEED_EVENTS)
    store.eventsHasOlder = (store.events[0]?.seq ?? 1) > 1

    // A thread/resume reply on the wire IS the ready signal: the background
    // attach just landed. Unlock the composer off the wire rather than waiting
    // for the 5s list poll, and pull the thread snapshot the right sidebar
    // needs immediately — undebounced, because this fires exactly once and it
    // is what the sidebar is blocked on.
    if (evt.kind === 'reply' && evt.label === 'thread/resume') {
      applySummary({ status: 'ready' })
      void refreshMeta()
    }

    const { msg } = evt
    if (evt.dir === 'send' && msg.id != null && msg.method) sentMethodByID.set(msg.id, msg.method)
    while (sentMethodByID.size > MAX_FEED_EVENTS) sentMethodByID.delete(sentMethodByID.keys().next().value)
    if (msg.method === 'mcpServer/startupStatus/updated') scheduleMcpRefresh()
  }

  // Label + one-line preview for the event feed.
  function annotate(evt) {
    const { msg } = evt
    if (msg.method) {
      evt.kind = msg.id != null ? 'request' : 'notification'
      evt.label = msg.method
      evt.preview = previewOf(msg.params)
    } else if (msg.type) {
      // Claude stream-json line: typed, no JSON-RPC envelope.
      evt.kind = 'notification'
      evt.label =
        msg.type +
        (msg.subtype ? `.${msg.subtype}` : msg.type === 'stream_event' ? `.${msg.event?.type ?? ''}` : '')
      evt.preview = previewOf(msg.message ?? msg.event ?? msg.result ?? msg)
    } else {
      // Pairing works for pre-connect requests too: the server replays its
      // full wire buffer (including its own sends) on SSE connect.
      const method = sentMethodByID.get(msg.id) ?? `#${msg.id}`
      evt.kind = msg.error ? 'error' : 'reply'
      evt.label = `${method}`
      evt.preview = msg.error ? msg.error.message : previewOf(msg.result)
    }
  }

  // openInstructionFile opens (or focuses) a right-pane tab for one
  // instruction file. Deduped by path so re-clicking a row just re-focuses it.
  function openInstructionFile(meta) {
    if (!store.openFiles.some((f) => f.path === meta.path)) {
      store.openFiles.push({ path: meta.path, basename: meta.basename, dirty: false })
    }
    store.rightTab = fileTabKey(meta.path)
  }

  function closeInstructionFile(path) {
    if (store.rightTab === fileTabKey(path)) store.rightTab = 'responses'
    store.openFiles = store.openFiles.filter((f) => f.path !== path)
  }

  adoptPersistedQueue() // session-key fallback until identity resolves
  adoptUnsent()
  connect()
  return {
    id,
    store,
    canSubmitTurn,
    sendTurn: submitTurn,
    clearSendError,
    discardPendingSend,
    resendPendingSend,
    acknowledgeSend,
    settleEchoes,
    echoTail,
    applyStreamStatus,
    streamDisconnected,
    consumeRestoreDraft,
    interruptTurn,
    setTurnOptions,
    answerInteraction,
    loadOlderEvents,
    connectFeed,
    disconnectFeed,
    loadMcpServers,
    loadSkills,
    disconnect,
    openInstructionFile,
    closeInstructionFile,
    applySummary,
    noteMetadataRevision,
    adoptSessionHandoff,
  }
}

export function fileTabKey(path) {
  return 'file:' + path
}

// claudeTurnAborted reports that a failing result line is the operator's own
// interrupt landing. The CLI names both flavors — mid-stream and mid-tool-call —
// with an `aborted_` prefix on terminal_reason, and a normal turn reads
// `completed`.
export function claudeTurnAborted(result) {
  return String(result?.terminal_reason ?? '').startsWith('aborted')
}

function previewOf(v) {
  if (v == null) return ''
  const s = JSON.stringify(v)
  return s.length > 160 ? s.slice(0, 160) + '…' : s
}
