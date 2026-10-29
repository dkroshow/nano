// App-level state: the open-threads list and which one is on screen.
// Session-scoped state lives in per-session stores (state.svelte.js).

import { tick } from 'svelte'
import { recordBrowserDiagnostic } from './browser_diagnostics.js'
import { readAPIError } from './api_error.js'
import {
  dropSessionStore,
  handoffSessionStore,
  setMissingSessionRecovery,
  beginMetadataRead,
  noteMetadataRevisions,
  syncSessionSummaries,
} from './state.svelte.js'
import {
  explorer,
  closeDetail,
  addRuntimeListener,
  catalogStreamOpen,
  setRuntimePosition,
} from './explorer.svelte.js'
import { buildWindowQuery } from './explorer_query.js'
import { setThreadReferenceNavigator } from './lib/transcript/thread_references.js'
import {
  activeTab,
  adoptSessionTabs,
  closeThreadTabs,
  detachSessionTabs,
  isThreadTab,
  openThreadTab,
  panes,
  tabTarget,
} from './panes.svelte.js'

// Fallback poll cadence — used ONLY while the SSE stream is broken or a needed
// registry read has failed. A healthy stream keeps app.sessions current from
// runtime events (see applyRuntimeEvent below).
const SESSIONS_FALLBACK_POLL_MS = 30000
let lastRegistryDiagnostic = ''
let lastRegistrySessions = new Map()

function registrySessionDiagnostic(session) {
  return JSON.stringify([
    session.id, session.provider, session.threadId, session.hostedThreadId,
    session.status, Boolean(session.turnActive),
  ])
}

function recordRegistryDiagnostics(sessions) {
  const ordered = [...sessions].sort((a, b) => String(a.id).localeCompare(String(b.id)))
  const current = new Map(ordered.map((session) => [session.id, registrySessionDiagnostic(session)]))
  const signature = JSON.stringify([...current])
  if (signature === lastRegistryDiagnostic) return

  recordBrowserDiagnostic('registry.refresh', { itemCount: ordered.length })
  for (const session of ordered) {
    if (lastRegistrySessions.get(session.id) === current.get(session.id)) continue
    recordBrowserDiagnostic('registry.session', {
      provider: session.provider,
      threadId: session.threadId,
      sessionId: session.id,
      hostedThreadId: session.hostedThreadId,
      status: session.status,
      turnActive: Boolean(session.turnActive),
    })
  }
  lastRegistryDiagnostic = signature
  lastRegistrySessions = current
}

// Row density, shared by the compact rail and the expanded table so a row is
// the same height in both. Persisted: it is a standing preference about how
// much of the catalog fits on screen, not per-visit UI state.
const DENSITY_KEY = 'nano.density'
const DENSITIES = ['full', 'compact']

function storedDensity() {
  try {
    const saved = localStorage.getItem(DENSITY_KEY)
    return DENSITIES.includes(saved) ? saved : 'full'
  } catch {
    return 'full' // private mode / storage disabled
  }
}

export function toggleDensity() {
  app.density = app.density === 'full' ? 'compact' : 'full'
  try {
    localStorage.setItem(DENSITY_KEY, app.density)
  } catch {
    // preference just won't survive the reload
  }
}

export const app = $state({
  density: storedDensity(),
  sessions: [], // summaries from GET /api/sessions
  detached: [], // attachments (any provider) whose session a restart killed (re-attachable)
  defaults: { cwd: '', model: '', sandbox: '', approval: '', allowedTools: '' },
  activeId: null,
  spawning: false,
  // The catalog is a layer over the live workspace, never a replacement for
  // it. The empty workspace remains useful when no session is attached.
  explorerOpen: false,
  // Mobile only (viewport.svelte.js): under 700px the rail is a drawer over
  // the workspace instead of a column beside it. Desktop never reads this —
  // the rail is always on screen there, so nothing can be "closed".
  sidebarOpen: false,
  // The usage-detail dialog. Its trigger lives in the rail strip but the
  // dialog itself is rendered at the top of App, outside the sidebar: on
  // mobile the sidebar is transformed, and a transformed ancestor becomes the
  // containing block for position:fixed descendants, which trapped the
  // full-screen overlay inside the drawer. Shared state is what lets the
  // trigger and the dialog live in different subtrees.
  usageDetailOpen: false,
  // A ?thread= deep link owns first navigation until the server has resolved
  // it. This stops the normal "newest session" bootstrap from briefly
  // claiming the pane (and browser history) before an explicit link lands.
  threadReferenceResolving: false,
  threadReferenceError: '',
})

let threadReferenceNavigationStarted = false
let threadReferenceNavigationToken = 0
let threadReferencePopstateListener = null

function threadReferenceFromLocation() {
  if (typeof window === 'undefined') return ''
  return new URL(window.location.href).searchParams.get('thread')?.trim() ?? ''
}

function writeThreadReference(reference, { replace = false } = {}) {
  if (typeof window === 'undefined' || !reference) return
  const next = new URL(window.location.href)
  if (next.searchParams.get('thread') === reference) return
  next.searchParams.set('thread', reference)
  window.history[replace ? 'replaceState' : 'pushState']({}, '', next)
}

// A rendered reference can already be represented in this browser's live
// registry or catalog window. Resolve those exact identities before asking the
// shared server resolver, which remains the authority for unknown, ambiguous,
// and fresh-tab references.
function uniqueHostedContinuation(matches, hostedKey) {
  const hosted = [...new Set(matches.map((candidate) => candidate[hostedKey]).filter(Boolean))]
  if (hosted.length === 1) {
    return matches.find((candidate) => candidate[hostedKey] === hosted[0]) ?? matches[0]
  }
  return null
}

function localThreadReferenceTarget(reference) {
  const sessionMatches = app.sessions.filter(
    (candidate) => candidate.hostedThreadId === reference || candidate.threadId === reference,
  )
  if (sessionMatches.length === 1) return { session: sessionMatches[0] }
  if (sessionMatches.length > 1) {
    const current = uniqueHostedContinuation(sessionMatches, 'hostedThreadId')
    return current ? { session: current } : null
  }

  const catalogMatches = explorer.dataset.filter(
    (candidate) =>
      candidate.nativeId === reference ||
      candidate.hostedThreadId === reference ||
      (reference.startsWith('nth_') && candidate.id === reference),
  )
  if (catalogMatches.length === 1) return { item: catalogMatches[0] }
  if (catalogMatches.length > 1) {
    const current = uniqueHostedContinuation(catalogMatches, 'hostedThreadId')
    return current ? { item: current } : null
  }
  return null
}

async function stageLocalThreadReference(reference, target, token) {
  app.threadReferenceError = ''
  try {
    if (target.session) {
      const opened = await openSession(target.session.id, { requireFresh: true, writeReference: false })
      if (!opened) throw new Error('The local thread is no longer available.')
      if (token !== threadReferenceNavigationToken) return
      // A live continuation has one canonical Nano URL, even when the link
      // named its native provider id.
      writeThreadReference(referenceForSession(target.session))
      return
    }
    stageCatalogItem(target.item, { writeReference: false })
    // Historical hosted and catalog links retain the exact reference that was
    // clicked, just as the server resolver does for a fresh browser.
    writeThreadReference(reference)
  } catch (error) {
    if (token !== threadReferenceNavigationToken) return
    app.threadReferenceError = error?.message || 'Could not open this thread reference.'
  }
}

// Called by delegated transcript/FileView links. Stage a local target first;
// an unknown reference falls back to the shared resolver. In either case URL
// state is a record of a successful open, never the command that initiates it.
export function openThreadReference(reference) {
  const targetReference = String(reference ?? '').trim()
  if (typeof window === 'undefined' || !targetReference) return
  const localTarget = localThreadReferenceTarget(targetReference)
  if (localTarget) {
    // Do not let a slow boot/popstate resolver overwrite the explicit local
    // selection while it is staging.
    const token = ++threadReferenceNavigationToken
    app.threadReferenceResolving = false
    void stageLocalThreadReference(targetReference, localTarget, token)
    return
  }
  void resolveThreadReference(targetReference, { writeReference: true })
}

setThreadReferenceNavigator(openThreadReference)

function referenceForSession(session) {
  return session?.hostedThreadId || session?.threadId || ''
}

function referenceForCatalogItem(item) {
  if (item?.runtime?.attached && item.hostedThreadId) return item.hostedThreadId
  return item?.nativeId || ''
}

// Install once at boot. Copy/pasted links and browser back/forward use the
// same resolver as initial load; a resolver result is the only authority that
// may translate a native UUID into an active hosted continuation.
export function startThreadReferenceNavigation() {
  if (threadReferenceNavigationStarted || typeof window === 'undefined') return
  threadReferenceNavigationStarted = true
  threadReferencePopstateListener = () => void resolveThreadReferenceFromLocation()
  window.addEventListener('popstate', threadReferencePopstateListener)
  void resolveThreadReferenceFromLocation({ initial: true })
}

export function __resetThreadReferenceNavigationForTest() {
  if (typeof window !== 'undefined' && threadReferencePopstateListener) {
    window.removeEventListener('popstate', threadReferencePopstateListener)
  }
  threadReferenceNavigationStarted = false
  threadReferencePopstateListener = null
  threadReferenceNavigationToken += 1
  app.threadReferenceResolving = false
  app.threadReferenceError = ''
}

export function recoverThreadReference() {
  app.threadReferenceError = ''
  openThreadExplorer()
}

function resolveThreadReferenceFromLocation({ initial = false } = {}) {
  const reference = threadReferenceFromLocation()
  if (!reference) {
    threadReferenceNavigationToken += 1
    app.threadReferenceResolving = false
    app.threadReferenceError = ''
    void sessionsRead().then(stageDefaultSession)
    return
  }
  return resolveThreadReference(reference, { replaceLiveReference: true, timing: { startedAt: initial ? 0 : performance.now(), trigger: initial ? 'load' : 'tab' } })
}

async function resolveThreadReference(reference, { writeReference = false, replaceLiveReference = false, timing = { startedAt: performance.now(), trigger: 'sidebar' } } = {}) {
  const token = ++threadReferenceNavigationToken
  app.threadReferenceResolving = true
  app.threadReferenceError = ''
  try {
    const res = await fetch(`/api/v1/thread-references/${encodeURIComponent(reference)}`)
    const body = await res.json().catch(() => null)
    if (token !== threadReferenceNavigationToken) return
    if (!res.ok) throw new Error(body?.error?.message || `Could not open thread reference (${res.status})`)
    if (body?.state !== 'resolved' || !body?.target) {
      const reason = body?.reason === 'not_indexed'
        ? 'This thread is not indexed yet. Nano will keep loading the normal workspace.'
        : body?.reason === 'ambiguous'
          ? 'This thread reference is ambiguous, so Nano did not choose a thread.'
          : 'Nano does not know this exact thread reference.'
      app.threadReferenceError = reason
      return
    }
    const target = body.target
    await stageThreadReferenceTarget(target, timing)
    if (token !== threadReferenceNavigationToken) return
    if (target.route === 'live' && target.hostedThreadId && (writeReference || replaceLiveReference)) {
      // A live UUID continuation has one canonical, durable Nano address.
      // Boot/popstate normalization replaces; an in-app click records one
      // ordinary entry after the stage has succeeded.
      writeThreadReference(target.hostedThreadId, { replace: replaceLiveReference })
    } else if (writeReference) {
      writeThreadReference(reference)
    }
  } catch (error) {
    if (token === threadReferenceNavigationToken) {
      app.threadReferenceError = error?.message || 'Could not resolve this thread reference.'
    }
  } finally {
    if (token === threadReferenceNavigationToken) {
      app.threadReferenceResolving = false
      // A failed link releases the ordinary workspace once. A resolved target
      // already staged a tab, so staging has nothing left to claim.
      void sessionsRead().then(stageDefaultSession)
    }
  }
}

async function stageThreadReferenceTarget(target, timing) {
  if (target.route === 'live' && target.sessionId) {
    // Initial deep links can resolve before the ordinary registry fetch. Fill
    // the registry while its default staging remains gated, then make the
    // resolver's explicit selection. A session newer than that read needs one
    // of its own.
    await sessionsRead()
    if (!app.sessions.some((session) => session.id === target.sessionId)) await refreshSessions()
    const opened = await openSession(target.sessionId, { requireFresh: true, writeReference: false, timing })
    if (opened) return
  }
  if (target.route !== 'historical' || !target.catalogThreadId) {
    throw new Error('The resolved thread is no longer available.')
  }
  openThreadTab({
    catalogId: target.catalogThreadId,
    provider: target.provider || '',
    nativeId: target.nativeThreadId || '',
    title: target.nativeThreadId || 'Thread',
  }, { forceTail: true, timing })
  app.activeId = null
  dismissCatalogOverlays()
}

export function toggleSidebar() {
  app.sidebarOpen = !app.sidebarOpen
}

export function closeSidebar() {
  app.sidebarOpen = false
}

// Staging a thread dismisses every catalog surface layered over the workspace:
// the quick-detail card, the expanded Explorer, and the mobile drawer the
// operator just picked from. All three entry points want the same dismissal,
// so they share one — a drawer that stayed open over the thread it just opened
// was the whole failure this covers.
function dismissCatalogOverlays() {
  closeDetail()
  app.explorerOpen = false
  app.sidebarOpen = false
}

// The registry is ~1.5 MB and every runtime nudge asks for it. Overlapping
// downloads queued other requests, prompts included, behind the browser's
// per-host connection limit. One fetch runs at a time; every call made during
// it shares one follow-up fetch, which starts after the call and so reflects it.
// A lone call returns loadSessions' own promise: wrapping it would add promise
// ticks before callers resume, and pane teardown is sensitive to that order.
let registryFetch = null
let registryFollowUp = null

// stageDefaultSession puts the newest shared session in the active pane when
// nothing is open, unless a thread link is still resolving. It runs after
// every list read, and when a link ends, since the read may have landed while
// the link held staging back.
function stageDefaultSession() {
  if (app.threadReferenceResolving) return
  const newestShared = app.sessions.slice().reverse().find(
    (session) => session.presentationMode !== 'background',
  ) ?? null
  if (app.activeId && !app.sessions.some((s) => s.id === app.activeId)) {
    app.activeId = newestShared?.id ?? null
  } else if (!app.activeId && newestShared) {
    // The live workspace is the app's primary surface. On a fresh load,
    // stage the newest shared session. A background worker never steals the
    // browser just because it was created most recently.
    app.activeId = newestShared.id
  }
  if (!activeTab() && newestShared) stageSession(newestShared, '', {
    writeReference: true, replaceReference: true,
    timing: panes.nextTabId === 1 ? { trigger: 'load', startedAt: 0 } : null,
  })
}

// sessionsRead is for callers that need the list, not a newer one: it waits
// for the read in flight (or the one queued after it), and reads only if the
// list has never been read. A page load then reads the list once; its stream
// replays what changed after that read (runtimePosition).
function sessionsRead() {
  return registryFollowUp ?? registryFetch ?? (registryRead ? Promise.resolve() : refreshSessions())
}

export function refreshSessions() {
  if (!registryFetch) {
    registryFetch = loadSessions()
    return registryFetch
  }
  registryFollowUp ??= registryFetch.then(() => {
    registryFollowUp = null
    return refreshSessions()
  })
  return registryFollowUp
}

async function loadSessions() {
  const runtimeSeqAtStart = runtimeSeq
  runtimeRowsSinceRead = new Map()
  sessionChangesSinceRead = []
  const metadataSeq = beginMetadataRead()
  try {
    const response = await fetch('/api/sessions')
    if (response.ok === false) throw new Error(`session registry failed (${response.status})`)
    const data = await response.json()
    const previousIds = new Set(app.sessions.map((session) => session.id))
    app.sessions = data.sessions ?? []
    // The list was read before the runtime events that arrived meanwhile;
    // their rows, newest per thread, are the state to keep.
    for (const change of sessionChangesSinceRead) app.sessions = mergeSessionChanges(app.sessions, change)
    if (runtimeSeq !== runtimeSeqAtStart) {
      app.sessions = patchRuntimeFields(app.sessions, [...runtimeRowsSinceRead.values()]).sessions
    }
    registryStale = false
    registryRead = true
    runtimePosition = data.runtimeStream ?? null
    app.detached = sessionChangesSinceRead.reduce(mergeDetachedChanges, data.detached ?? [])
    app.defaults = data.defaults ?? app.defaults
    syncSessionSummaries(app.sessions, metadataSeq) // status/error flow into open session stores
    adoptSessionList(previousIds)
    return { ok: true, sessions: app.sessions }
  } catch {
    // server unreachable; keep the last known list, and let the fallback poll
    // retry until a read lands
    registryStale = true
    return { ok: false, sessions: app.sessions }
  } finally {
    registryFetch = null
    sessionChangesSinceRead = []
  }
}

// adoptSessionList follows a new app.sessions with the tabs: an open
// historical tab whose thread just became attached turns live (so a resume
// never depends on a staging call landing), a rotated session hands its store
// to its successor, and a session that left the list leaves its tab as
// durable history when a catalog id is known.
function adoptSessionList(previousIds) {
  recordRegistryDiagnostics(app.sessions)
  adoptSessionTabs(app.sessions, (rotatedId, successor) => {
    recordBrowserDiagnostic('registry.rotate', {
      previousSessionId: rotatedId,
      sessionId: successor?.id,
      provider: successor?.provider,
      threadId: successor?.threadId,
      hostedThreadId: successor?.hostedThreadId,
      status: successor?.status,
      turnActive: Boolean(successor?.turnActive),
    })
    handoffSessionStore(rotatedId, successor)
  })
  stageDefaultSession()
  for (const id of previousIds) {
    if (!app.sessions.some((session) => session.id === id)) detachSessionTabs(id)
  }
}

// applySessionChanges applies the sessions a runtime event names: the list
// rows of sessions that joined or changed and the ids of sessions that left,
// and likewise the detached rows and the keys ("provider:nativeThreadId") of
// those that left. A full read is then needed only for what no event carries.
// A list read answers from before the changes that arrived while it was in
// flight, so it applies them over its answer, as it does runtime rows. Kept
// only while a read is in flight.
let sessionChangesSinceRead = []

function applySessionChanges(payload) {
  const change = {
    sessions: payload.sessions ?? [],
    removed: new Set(payload.removedSessions ?? []),
    detachedRows: payload.detachedRows ?? [],
    removedDetached: new Set(payload.removedDetached ?? []),
  }
  if (!change.sessions.length && !change.removed.size && !change.detachedRows.length && !change.removedDetached.size) return
  if (registryFetch) sessionChangesSinceRead.push(change)
  const previousIds = new Set(app.sessions.map((session) => session.id))
  app.sessions = mergeSessionChanges(app.sessions, change)
  app.detached = mergeDetachedChanges(app.detached, change)
  syncSessionSummaries(change.sessions, beginMetadataRead())
  adoptSessionList(previousIds)
}

const detachedKey = (row) => `${row.provider}:${row.nativeThreadId}`

function mergeDetachedChanges(detached, change) {
  if (!change.detachedRows.length && !change.removedDetached.size) return detached
  const byKey = new Map(change.detachedRows.map((row) => [detachedKey(row), row]))
  const next = detached.filter((row) => !change.removedDetached.has(detachedKey(row))).map((row) => byKey.get(detachedKey(row)) ?? row)
  const present = new Set(next.map(detachedKey))
  for (const row of change.detachedRows) if (!present.has(detachedKey(row))) next.push(row)
  return next
}

function mergeSessionChanges(sessions, change) {
  const byId = new Map(change.sessions.map((session) => [session.id, session]))
  const next = sessions.filter((session) => !change.removed.has(session.id)).map((session) => byId.get(session.id) ?? session)
  const present = new Set(next.map((session) => session.id))
  for (const session of change.sessions) if (!present.has(session.id)) next.push(session)
  return next
}

// Runtime events carry the runtime state of each thread whose state changed,
// and null for a thread Nano no longer holds, and the list rows of sessions
// that joined, changed or left (applySessionChanges). They patch app.sessions
// in place; the whole list is read again only when the event says so
// (refresh) or when a row's identity still does not match the list.
// The rows of every runtime event since the current list read began, newest
// per thread: a list read answers from before them, so they are applied over
// it. Each event names only the threads that changed, so one event's rows
// cannot stand for the others'.
let runtimeRowsSinceRead = new Map()
let runtimeSeq = 0
// Where the list stands in the server's runtime stream: the last read's
// position, advanced by each event. The catalog stream connects from it and
// first replays what the list has not seen, so neither a page load nor a
// short drop reads the list again.
let runtimePosition = null
let registryStale = false
let registryRead = false // a list read has succeeded since the page loaded

const RUNTIME_FIELDS = ['status', 'turnActive', 'threadStatus', 'turnStartedAtMs', 'compactingSinceMs', 'waiting', 'needsAttention',
  'presentationMode', 'error', 'metadataDegraded']

function runtimeSummary(session, runtime) {
  const summary = { id: session.id }
  for (const field of RUNTIME_FIELDS) summary[field] = runtime[field]
  // Rows omit empty fields; a summary states them, so a store sees a turn end.
  summary.turnActive = Boolean(runtime.turnActive)
  summary.waiting = Boolean(runtime.waiting)
  summary.needsAttention = Boolean(runtime.needsAttention)
  summary.metadataDegraded = Boolean(runtime.metadataDegraded)
  summary.error = runtime.error ?? ''
  if (runtime.lastTurnEndedAt !== undefined) summary.lastTurnEndedAt = runtime.lastTurnEndedAt
  return summary
}

function runtimeByThread(rows) {
  const byThread = new Map()
  for (const row of rows) {
    if (row?.provider && row.nativeId && row.runtime) byThread.set(`${row.provider}:${row.nativeId}`, row.runtime)
  }
  return byThread
}

// Returns the list with each attached session's runtime fields replaced by its
// row, and the runtime-only summaries of the sessions that changed.
function patchRuntimeFields(sessions, rows) {
  const byThread = runtimeByThread(rows)
  const changed = []
  const next = sessions.map((session) => {
    const runtime = session.threadId ? byThread.get(`${session.provider}:${session.threadId}`) : null
    if (!runtime?.attached || runtime.sessionId !== session.id) return session
    const summary = runtimeSummary(session, runtime)
    // The delta is current on the subscription it arrived on.
    if (Object.keys(summary).every((field) => session[field] === summary[field])) return session
    changed.push(summary)
    return { ...session, ...summary }
  })
  return { sessions: changed.length ? next : sessions, changed }
}

// Only the rows an event names can disagree with the list: a row attached to
// a session the list lacks, or a row no longer attached while the list still
// holds a live session for its thread. A shell without a native id has no
// row; the server flags its changes.
function runtimeIdentityChanged(sessions, rows) {
  const byId = new Map(sessions.map((session) => [session.id, session]))
  const liveByThread = new Map(sessions
    .filter((session) => session.threadId && session.status !== 'exited')
    .map((session) => [`${session.provider}:${session.threadId}`, session]))
  return rows.some((row) => {
    if (!row?.provider || !row.nativeId) return false
    const runtime = row.runtime
    if (runtime?.attached && runtime.sessionId) {
      const session = byId.get(runtime.sessionId)
      return !session || session.provider !== row.provider || session.threadId !== row.nativeId
    }
    return liveByThread.has(`${row.provider}:${row.nativeId}`)
  })
}

function applyRuntimeEvent(payload) {
  const rows = Array.isArray(payload?.rows) ? payload.rows : null
  if (!rows) {
    void refreshSessions() // an event without rows says nothing; read the list
    return
  }
  for (const row of rows) {
    if (row?.provider && row.nativeId) runtimeRowsSinceRead.set(`${row.provider}:${row.nativeId}`, row)
  }
  runtimeSeq += 1
  if (runtimePosition && payload.seq > runtimePosition.seq) runtimePosition = { ...runtimePosition, seq: payload.seq }
  applySessionChanges(payload)
  if (payload.refresh || runtimeIdentityChanged(app.sessions, rows)) void refreshSessions()
  const { sessions, changed } = patchRuntimeFields(app.sessions, rows)
  if (changed.length) {
    app.sessions = sessions
    // Runtime fields only: an open store's metadata comes from its own read.
    syncSessionSummaries(changed)
  }
  noteMetadataRevisions(rows
    .filter((row) => row?.runtime?.attached && row.runtime.sessionId && row.runtime.metadataRevision)
    .map((row) => ({ id: row.runtime.sessionId, revision: row.runtime.metadataRevision })))
}

// A turn can race a Nano restart after the old server id has died but before
// this page has adopted the boot-reconnected successor. Session stores cannot
// import app state without creating a module cycle, so this narrow callback
// refreshes the registry and resolves the session to retry against: the same
// id if the registry still holds it (a gateway 5xx proves nothing about the
// session), else the same stable provider/thread under its rotated id. The
// 404 caller filters out the same-id result itself — Nano answered "no such
// session" authoritatively there.
setMissingSessionRecovery(async ({ sessionId, provider, threadId }) => {
  recordBrowserDiagnostic('send.recovery.start', { sessionId, provider, threadId })
  const wasActive = app.activeId === sessionId
  await refreshSessions()
  const original = app.sessions.find((session) => session.id === sessionId) ?? null
  const successor = !provider || !threadId ? null : app.sessions.find(
    (session) =>
      session.id !== sessionId &&
      session.provider === provider &&
      session.threadId === threadId,
  ) ?? null
  if (!original && successor && wasActive) app.activeId = successor.id
  recordBrowserDiagnostic('send.recovery.result', {
    previousSessionId: sessionId,
    sessionId: (original ?? successor)?.id,
    provider,
    threadId,
    hostedThreadId: (original ?? successor)?.hostedThreadId,
    outcome: original ? 'same-session' : successor ? 'successor' : 'missing',
  })
  return original ?? successor
})

// reattachThread resumes a detached thread through the normal resume path —
// 409 thread_attached guard and R3 instant-history included — so a
// restart-killed session comes back exactly as an Explorer resume would. On a
// 409 (already re-attached elsewhere) it jumps to that session instead.
export async function reattachThread(provider, nativeThreadId) {
  const failure = await spawnSession({ provider, mode: 'resume', threadId: nativeThreadId }, nativeThreadId)
  if (failure?.code === 'thread_attached' && failure.details?.existingSessionId) {
    await openSession(failure.details.existingSessionId)
    return null
  }
  return failure?.message ?? failure
}

// dismissDetached closes a detached attachment without re-attaching it.
// Keep the row and pane until the server confirms success. Matches on
// provider + nativeThreadId since the same native id can exist under two
// providers (unlikely, but the composite is the actual identity).
export async function dismissDetached(provider, nativeThreadId, unconfirmedAttachment = null) {
  const query = new URLSearchParams({ provider })
  if (unconfirmedAttachment) {
    query.set('forget', 'unconfirmed')
    query.set('expectedShimSessionId', unconfirmedAttachment.shimSessionId ?? '')
    query.set('expectedAttachedAt', unconfirmedAttachment.attachedAt ?? '')
  }
  const res = await fetch(`/api/attachments/${encodeURIComponent(nativeThreadId)}?${query}`, { method: 'DELETE' })
  if (!res.ok) throw new Error((await readAPIError(res, `Could not end session (${res.status})`)).message)
  app.detached = app.detached.filter((d) => !(d.provider === provider && d.nativeThreadId === nativeThreadId))
  closeThreadTabs({ provider, nativeId: nativeThreadId })
}

// Live sessions can be named before their provider has produced a durable
// transcript. The server keeps that pre-bind name pending when necessary, then
// returns the updated summary so the rail and any open tab repaint immediately.
export async function renameSession(id, name) {
  const res = await fetch(`/api/sessions/${encodeURIComponent(id)}/name`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  })
  if (!res.ok) throw new Error((await readAPIError(res, `Could not rename session (${res.status})`)).message)
  const updated = await res.json()
  app.sessions = app.sessions.map((session) => session.id === id ? updated : session)
  adoptSessionTabs(app.sessions)
  return updated
}

// spawnSession POSTs the New Window dialog's payload. Returns a structured
// API failure, or null on success (the new session becomes active).
//
// continuesNativeId names the thread the new session continues, for the one
// case its summary cannot: a claude `--resume` child announces its (brand new)
// native id asynchronously, so the summary here carries no thread identity at
// all and the session would otherwise land in a second tab beside the one the
// operator resumed from.
export async function spawnSession(payload, continuesNativeId = '') {
  app.spawning = true
  try {
    const res = await fetch('/api/sessions', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(payload),
    })
    if (!res.ok) {
      const failure = await readAPIError(res, `Could not start session (${res.status})`)
      return failure
    }
    const sess = await res.json()
    await refreshSessions()
    app.activeId = sess.id
    stageSession(app.sessions.find((session) => session.id === sess.id) ?? sess, continuesNativeId, { writeReference: true })
    dismissCatalogOverlays()
    return null
  } catch (e) {
    return { code: '', message: String(e), details: null }
  } finally {
    app.spawning = false
  }
}

// openSession brings an already-running session on stage (runtime-badge jump,
// 409 thread_attached conflict resolution). The page-level message feed has
// already warmed its normal transcript, so this is a local visibility change.
let navigationIntent = 0

export async function openSession(id, { requireFresh = false, writeReference = true, timing = null } = {}) {
  const intent = ++navigationIntent
  const restoreComposerFocus = typeof document !== 'undefined' && document.activeElement?.matches('.composer textarea')
  const session = app.sessions.find((candidate) => candidate.id === id)
  recordBrowserDiagnostic('navigation.request', {
    sessionId: id,
    provider: session?.provider,
    threadId: session?.threadId,
    hostedThreadId: session?.hostedThreadId,
    status: session?.status,
  })
  if (!session) {
    // Conflict resolution can identify a live session before this browser's
    // registry refresh has delivered its summary. Preserve the old immediate
    // jump for that programmatic path; adoptSessionTabs binds the tab when the
    // refresh lands. Sidebar rows always carry a summary and use requireFresh.
    if (!requireFresh) {
      app.activeId = id
      dismissCatalogOverlays()
      void refreshSessions()
      return true
    }
    void refreshSessions()
    return false
  }
  if (intent !== navigationIntent) return false
  app.activeId = id
  dismissCatalogOverlays()
  stageSession(session, '', { forceTail: true, timing })
  if (writeReference) writeThreadReference(referenceForSession(session))
  recordBrowserDiagnostic('navigation.commit', {
    sessionId: id,
    provider: session.provider,
    threadId: session.threadId,
    hostedThreadId: session.hostedThreadId,
    status: session.status,
  })
  if (restoreComposerFocus) {
    await tick()
    if (intent === navigationIntent) {
      document.querySelector('.thread-pane.focused .thread-pane-view:not(.hidden) .composer textarea')?.focus()
    }
  }
  return true
}

// openSidebarRow stages one projected sidebar row, routing on the descriptor
// the projection built (lib/explorer/sidebar_projection.js). The row's own click handler
// and the keyboard shortcuts share it, so a thread opens identically however it
// was picked.
export function openSidebarRow(click) {
  if (click.action === 'open') return openSession(click.sessionId, { requireFresh: true })
  navigationIntent += 1 // cancel a slower live-thread click that preceded this one
  if (click.catalogId) openThread(click.catalogId)
  else openThreadExplorer() // no identity to look up yet — the list itself is the fallback
  return true
}

// openThreadExplorer opens the catalog over the current workspace. It has a
// separate state from the active session so the conversation and right detail
// pane stay mounted behind it.
export function openThreadExplorer() {
  app.explorerOpen = true
}

export function closeThreadExplorer() {
  app.explorerOpen = false
}

export function toggleThreadExplorer() {
  if (app.explorerOpen) closeThreadExplorer()
  else openThreadExplorer()
}

// openThread stages one catalog thread as a tab in the focused pane and
// collapses the expanded rail. Attached rows become live tabs; every other
// indexed row becomes durable read-only history.
export function openThread(id) {
  const item = explorer.dataset.find((candidate) => candidate.id === id)
  if (!item) {
    app.explorerOpen = true
    return
  }
  stageCatalogItem(item, { writeReference: true })
}

function stageCatalogItem(item, { writeReference = false } = {}) {
  const session = item.runtime?.sessionId
    ? app.sessions.find((candidate) => candidate.id === item.runtime.sessionId) ?? null
    : null
  // A rail click is an explicit request for the newest message. Tab-to-tab
  // navigation is softer and may preserve a recent deliberate history view;
  // panes.svelte.js carries that distinction on the tab reset nonce.
  openThreadTab(tabTarget({ item, session }), { forceTail: true })
  app.activeId = session?.id ?? null
  dismissCatalogOverlays()
  if (writeReference) writeThreadReference(referenceForCatalogItem(item))
}

// openThreadExplorerAt is the session view's full-history escape hatch: find
// and stage one thread by native id. The lookup is an exact-threadId catalog
// query with every filter widened, so
// old/archived/subagent threads stay findable regardless of the operator's
// window settings. Not indexed yet (or catalog unreachable) → the Explorer
// opens as the fallback; the scanner lands the row within seconds.
export async function openThreadExplorerAt(nativeId) {
  try {
    const query = buildWindowQuery(
      { window: 'all', archived: 'include', includeSubagents: true, includeProgrammatic: true, threadId: nativeId },
      null
    )
    const res = await fetch(`/api/v1/threads?${query}`)
    if (!res.ok) {
      openThreadExplorer()
      return
    }
    const row = (await res.json()).items?.[0]
    if (row) stageCatalogItem(row, { writeReference: true })
    else openThreadExplorer()
  } catch {
    // no error surface — the open Explorer view is the fallback
    openThreadExplorer()
  }
}

export async function closeSession(id) {
  const response = await fetch(`/api/sessions/${id}`, { method: 'DELETE' })
  if (!response.ok) throw new Error((await readAPIError(response, `Could not close session (${response.status})`)).message)
  closeThreadTabs({ sessionId: id })
  dropSessionStore(id)
  await refreshSessions()
}

// continuesNativeId stands in while a session has not announced its own thread
// id yet (see spawnSession). It only ever names the thread the tab is already
// showing, so binding to it cannot land the session on the wrong tab.
function stageSession(session, continuesNativeId = '', { forceTail = false, writeReference = false, replaceReference = false, timing = null } = {}) {
  if (!session) return
  const nativeId = session.threadId || continuesNativeId
  const item = explorer.dataset.find(
    (candidate) =>
      candidate.provider === session.provider &&
      candidate.nativeId &&
      candidate.nativeId === nativeId,
  )
  openThreadTab({ ...tabTarget({ item, session }), nativeId }, { forceTail, timing })
  if (writeReference) writeThreadReference(referenceForSession(session), { replace: replaceReference })
}

let polling = false
let firstRegistryPromise = null

// hiddenThreadRefs names this page's open thread tabs no pane shows, newest
// tab first: the hints the warm stream carries ahead of the server's choice.
export function hiddenThreadRefs() {
  const shown = new Set(panes.items.map((pane) => pane.activeTabId))
  const hosted = new Map(app.sessions.map((session) => [session.id, session.hostedThreadId]))
  return Object.values(panes.tabs)
    .filter((tab) => isThreadTab(tab) && !shown.has(tab.id))
    .sort((a, b) => b.id - a.id)
    .map((tab) => hosted.get(tab.sessionId))
    .filter(Boolean)
}

export function startPolling() {
  if (polling) return firstRegistryPromise
  polling = true
  firstRegistryPromise = sessionsRead() // joins a thread link's read at startup
  addRuntimeListener(applyRuntimeEvent) // the same event that patches the explorer catalog window
  // With no position (no read has landed) the server answers a refresh.
  setRuntimePosition(() => runtimePosition ?? { generation: '', seq: 0 })
  setInterval(() => {
    if (!catalogStreamOpen() || registryStale) void refreshSessions()
  }, SESSIONS_FALLBACK_POLL_MS) // covers a broken stream or a failed read
  return firstRegistryPromise
}
