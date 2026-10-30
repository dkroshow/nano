// Resume-from-Explorer bridge (Slice R1). Gating is a pure policy function so
// the enabled/disabled/absent matrix is unit-testable without Svelte; the
// action wraps the shared spawnSession helper and resolves the server's
// single-attachment conflict (HTTP 409 thread_attached) by jumping to the
// existing session instead of erroring.
import { app, spawnSession, openSession, closeSession } from '../../app.svelte.js'
import { requestParentClose } from './descendant_close.svelte.js'

// Settled decision 3: archived and subagent codex threads show Resume
// disabled with a visible note — intentionally off, not silently hidden.
const ARCHIVED_NOTE = 'Resume is intentionally off for archived threads for now (not yet validated).'
const SUBAGENT_NOTE = "Resume is intentionally off for subagent threads — they belong to the parent's orchestration."
// Claude-only gates. A claude session IS its `claude -p` process, so resume
// means respawning with `--resume` from the native transcript, in the project
// the thread ran in. Both of those are preconditions nano can check from
// catalog evidence alone — no per-row filesystem probing.
const NO_CWD_NOTE = 'No recorded working directory for this thread — resume needs the project it ran in.'
const SOURCE_MISSING_NOTE = 'The native session file was purged — resume unavailable.'

// One canonical metadata read: the catalog reports every field as
// known/unknown/unsupported, and only a known value may be acted on. Anything
// else is absence, never a default to guess from.
function recordedValue(item, field) {
  const cell = item?.metadata?.[field]
  if (cell?.state !== 'known' || typeof cell.value !== 'string') return ''
  return cell.value.trim()
}

// prioritizeStartingThread asks Nano to re-attach a thread that still waits
// after a restart now, ahead of the boot queue or out of rest, and reports
// whether Nano accepted the request. The tab goes live through the session
// list like any attach; a resting thread has no boot queue to fall back on,
// so callers ask again after a failure.
export function prioritizeStartingThread(hostedThreadId) {
  return fetch(`/api/v1/hosted-threads/${encodeURIComponent(hostedThreadId)}/prioritize`, { method: 'POST' })
    .then((response) => response.ok, () => false)
}

// After a restart Nano leaves every thread without open work "resting", and a
// sidebar row that shows one asks for it once while it rests: rows re-render
// often. A request that fails re-arms, so the next render asks again.
const restingAsked = new Set()
export function attachShownRestingThread(hostedThreadId, resting) {
  if (!resting) {
    restingAsked.delete(hostedThreadId)
    return
  }
  if (restingAsked.has(hostedThreadId)) return
  restingAsked.add(hostedThreadId)
  void prioritizeStartingThread(hostedThreadId).then((ok) => {
    if (!ok) restingAsked.delete(hostedThreadId)
  })
}

// null → no resume affordance at all. Anything else renders the button, enabled
// or disabled-with-a-note.
export function resumeGate(item) {
	if (!item) return null
	if (item.provider === 'codex') return codexGate(item)
	if (item.provider === 'claude') return claudeGate(item)
	if (item.provider === 'grok') return grokGate(item)
	return null
}

function codexGate(item) {
  if (item.archived) return { enabled: false, note: ARCHIVED_NOTE }
  if (item.kind === 'subagent') return { enabled: false, note: SUBAGENT_NOTE }
  return { enabled: true, note: null }
}

// Settled decision 5: a claude subagent row shows nothing at all rather than a
// disabled button — its transcript is a sidechain of the parent's session, not
// a session anything could resume. (Codex keeps its grayed-out note; that
// behavior is shipped and untouched.)
function claudeGate(item) {
  if (item.kind === 'subagent') return null
  if (item.archived) return { enabled: false, note: ARCHIVED_NOTE }
  // Only a row that *has* recorded metadata can testify that the cwd is
  // unknown. A thread outside the Explorer's current window — or one whose
  // catalog row has not landed yet — carries no metadata object at all, and
  // reading that absence as "no recorded working directory" disables Resume on
  // perfectly resumable threads with a claim nano cannot support. Offer it and
  // let the server answer from evidence it can actually see: it either finds the
  // thread's attachment record, or refuses in a sentence naming what is missing.
  if (item.metadata && !recordedValue(item, 'cwd')) return { enabled: false, note: NO_CWD_NOTE }
  if (item.source?.status === 'source_missing') return { enabled: false, note: SOURCE_MISSING_NOTE }
  return { enabled: true, note: null }
}

function grokGate(item) {
	if (item.kind === 'subagent') return null
	if (item.archived) return { enabled: false, note: ARCHIVED_NOTE }
	if (item.metadata && !recordedValue(item, 'cwd')) return { enabled: false, note: NO_CWD_NOTE }
	if (item.source?.status === 'source_missing') return { enabled: false, note: SOURCE_MISSING_NOTE }
	return { enabled: true, note: null }
}

// resolveThreadIdentity answers "which thread is this surface showing?" for a
// tab whose two ids can disagree. A claude `--resume` successor mints a new
// native session id and announces it after the tab is already bound, so the tab
// (or its live session) holds the current identity while tab.catalogId still
// names the *predecessor's* row until the successor is indexed. Resolving from
// the native id first is what keeps Resume from targeting the predecessor —
// which would fork the conversation a second time and leave the successor's
// work behind — and keeps the transcript on screen describing the same thread
// the button would resume.
export function resolveThreadIdentity({ nativeId = '', catalogId = '', provider = '', dataset = [] } = {}) {
  const providerMatches = (row) => !provider || !row.provider || row.provider === provider
  const byNative = nativeId ? dataset.find((row) => row.nativeId === nativeId && providerMatches(row)) ?? null : null
  const byCatalog = catalogId ? dataset.find((row) => row.id === catalogId) ?? null : null
  const row = byNative ?? byCatalog
  const resolvedNativeId = nativeId || row?.nativeId || ''
  // The nano row id the resume request and the read-only transcript travel on.
  // A row known to describe some other native id must not lend its id: that is
  // the predecessor's transcript, not this thread's. Unresolvable is honest —
  // the file stream resolves its own thread instead of guessing.
  const staleRow = Boolean(byCatalog?.nativeId && byCatalog.nativeId !== resolvedNativeId)
  const rowId = byNative && !byNative.transient ? byNative.id : staleRow ? '' : catalogId
  return { row, provider: row?.provider || provider || '', nativeId: resolvedNativeId, rowId }
}

// Request field ← catalog metadata field, in the order they go on the wire.
const CLAUDE_POSTURE_FIELDS = [
  ['cwd', 'cwd'],
  ['model', 'model'],
  ['permissionMode', 'permission'],
  ['effort', 'effort'],
]

// The POST body for one resume. Codex sends the bare frozen triple: the
// app-server still holds the thread, so anything else nano sent could only
// override posture it already knows. A claude resume respawns a process from
// nothing, so it carries the thread's own recorded posture (settled decision
// 2) — continue as it ran, including bypassPermissions — omitting whatever the
// catalog does not know so the CLI's defaults apply instead of a guess.
function resumeRequest(item) {
	const request = { provider: item.provider, mode: 'resume', threadId: item.nativeId }
	if (item.provider === 'grok') {
		const cwd = recordedValue(item, 'cwd')
		if (cwd) request.cwd = cwd
		return request
	}
	if (item.provider !== 'claude') return request
  for (const [field, metadataField] of CLAUDE_POSTURE_FIELDS) {
    const value = recordedValue(item, metadataField)
    if (value) request[field] = value
  }
  return request
}

// Detach is the inverse bridge: nano lets go of the thread without touching
// its history. It is the same close as the sidebar's Close session, so it also
// removes the thread's star. The semantics differ per provider — a codex thread keeps
// running in the app-server daemon, while ending a hosted claude session stops
// its child process (a later resume respawns a new one from the native
// transcript, which cannot carry a turn that was in flight) — so the
// affordance is labeled per provider rather than pretending one word covers
// both.
export function detachLabels(provider) {
  if (provider === 'claude') {
    return {
      label: 'End session',
      busy: 'Ending…',
      title: 'Stop the hosted claude process and remove its star. History stays in the catalog.',
    }
  }
  return {
    label: 'Detach',
    busy: 'Detaching…',
    title: 'Close nano’s attachment and remove its star. The thread keeps its history and can be resumed any time.',
  }
}

export function createDetachAction() {
  const state = $state({ busy: false, error: null })

  async function detach(sessionId, parent = null) {
    if (state.busy || !sessionId) return false
    state.busy = true
    state.error = null
    try {
      const result = parent
        ? await requestParentClose({ parent, sessionId, provider: parent.provider })
        : (await closeSession(sessionId), { parentClosed: true })
      return result.parentClosed
    } catch (err) {
      state.error = String(err?.message ?? err)
      return false
    } finally {
      state.busy = false
    }
  }

  return { state, detach }
}

export function createResumeAction() {
  const state = $state({
    busy: false, // POST in flight → button disabled
    error: null, // inline error next to the button; conflicts never land here
  })

  // Resolves true when the caller should close the history view: a successful
  // resume (spawnSession already switched to the live view) or a conflict
  // jump. False leaves the modal open with state.error set.
  //
  // The catalog record supplies the native resume target and, for Claude,
  // the recorded metadata that owns its posture.
  async function resume(item) {
    const nativeId = item?.nativeId
    if (state.busy || !nativeId) return false
    state.busy = true
    state.error = null
    const err = await spawnSession(resumeRequest(item), nativeId)
    state.busy = false
    if (err == null) return true
    const existingSessionId = err.code === 'thread_attached' && err.details?.existingSessionId
    if (existingSessionId) {
      await openSession(existingSessionId)
      return true
    }
    state.error = err.message ?? err
    return false
  }

  return { state, resume }
}
