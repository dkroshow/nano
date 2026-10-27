// Small test-side transport fixture for the normalized conversation channel.
// Production never receives provider-shaped rows from the browser stream.

import { liveItemCategory } from './transcript_view.js'

export const TEST_PROJECTION_EPOCH = 'test-projection-epoch'

export function projectionSnapshot({
  provider = 'codex',
  nativeThreadID = 'native-1',
  epoch = TEST_PROJECTION_EPOCH,
  revision = 0,
  items = [],
  turns = [],
  runtime = {},
  interactions = [],
} = {}) {
  return {
    version: 1,
    provider,
    nativeThreadID,
    epoch,
    revision,
    items,
    turns,
    runtime: {
      threadStatus: 'idle',
      turnActive: false,
      ...runtime,
    },
    interactions,
  }
}

export function projectionItem({
  id,
  type = 'agentMessage',
  text = '',
  done = true,
  createdAt = '2026-07-29T10:00:00Z',
  item = {},
  ...metadata
} = {}) {
  return {
    id,
    type,
    text,
    done,
    createdAt,
    item: { type, ...item },
    ...metadata,
  }
}

function upsertByID(items, next) {
  if (!next?.id) return items
  const index = items.findIndex((item) => item.id === next.id)
  if (index < 0) return [...items, next]
  return items.map((item, candidate) => candidate === index
    ? { ...item, ...next, item: { ...(item.item ?? {}), ...(next.item ?? {}) } }
    : item)
}

function freshFixtureState(provider = 'codex', nativeThreadID = 'native-1') {
  return {
    provider,
    threadId: nativeThreadID,
    items: [],
    projectionEpoch: TEST_PROJECTION_EPOCH,
    projectionRevision: 0,
    projectionTurns: [],
    pendingInteractions: [],
    sessionStatus: 'ready',
    threadStatus: 'idle',
    turnActive: false,
    turnID: '',
    turnStartedAtMs: 0,
    compactingSinceMs: 0,
    tokenUsage: null,
    lastError: '',
    deliveryExpiries: 0,
  }
}

function adoptFixtureSnapshot(state, snapshot) {
  state.provider = snapshot.provider ?? state.provider
  state.threadId = snapshot.nativeThreadID ?? snapshot.nativeThreadId ?? state.threadId
  state.items = [...(snapshot.items ?? [])]
  state.projectionEpoch = snapshot.epoch
  state.projectionRevision = snapshot.revision
  state.projectionTurns = [...(snapshot.turns ?? [])]
  state.pendingInteractions = [...(snapshot.interactions ?? [])]
  Object.assign(state, snapshot.runtime ?? {})
}

// Tests describe owner state changes tersely, but every browser delivery is a
// complete replacement snapshot. This keeps fixtures from exercising the
// retired browser-side item reducer.
function applyFixtureChange(state, update = {}) {
  if (update.op === 'snapshot' && update.snapshot) {
    adoptFixtureSnapshot(state, update.snapshot)
    return
  }
  state.projectionRevision += 1
  if (update.runtime) Object.assign(state, update.runtime)
  switch (update.op ?? 'item.upsert') {
    case 'item.upsert':
    case 'item.complete':
      state.items = upsertByID(state.items, update.item)
      break
    case 'item.remove':
      state.items = state.items.filter((item) => item.id !== update.itemID)
      break
    case 'turn.upsert':
      state.projectionTurns = upsertByID(state.projectionTurns, update.turn)
      break
    case 'interaction.upsert':
      state.pendingInteractions = upsertByID(state.pendingInteractions, update.interaction)
      break
    case 'interaction.remove':
      state.pendingInteractions = state.pendingInteractions.filter((item) => item.id !== update.requestID)
      break
    default:
      break
  }
}

function snapshotFromFixture(state) {
  return projectionSnapshot({
    provider: state.provider,
    nativeThreadID: state.threadId,
    epoch: state.projectionEpoch,
    revision: state.projectionRevision,
    items: state.items,
    turns: state.projectionTurns,
    interactions: state.pendingInteractions,
    runtime: {
      sessionStatus: state.sessionStatus,
      threadStatus: state.threadStatus,
      turnActive: state.turnActive,
      turnID: state.turnID,
      turnStartedAtMs: state.turnStartedAtMs,
      compactingSinceMs: state.compactingSinceMs,
      tokenUsage: state.tokenUsage,
      lastError: state.lastError,
      deliveryExpiries: state.deliveryExpiries,
    },
  })
}

function publishSnapshot(source, snapshot) {
  if (source.url?.includes('/live?')) {
    sendFileSnapshot(source, snapshot)
    return
  }
  source.onmessage?.({ data: JSON.stringify({
    version: 1,
    epoch: snapshot.epoch,
    revision: snapshot.revision,
    op: 'snapshot',
    snapshot,
  }) })
}

export function sendProjection(source, update = {}) {
  source.__projectionState ??= freshFixtureState()
  applyFixtureChange(source.__projectionState, update)
  publishSnapshot(source, snapshotFromFixture(source.__projectionState))
}

export function sendProjectionSnapshot(source, options = {}) {
  const snapshot = projectionSnapshot(options)
  source.__projectionState = freshFixtureState(snapshot.provider, snapshot.nativeThreadID)
  adoptFixtureSnapshot(source.__projectionState, snapshot)
  publishSnapshot(source, snapshot)
}

// This test-side driver also supplies retained page-feed fixtures. File pane
// fixtures use createFileTranscriptDriver below to deliver through their stream.
export function createMessageProjectionDriver(onSnapshot, {
  provider = 'codex',
  nativeThreadID = 'native-1',
} = {}) {
  const state = {
    ...freshFixtureState(provider, nativeThreadID),
    messageItems: [],
  }
  return {
    state,
    send(update) {
      applyFixtureChange(state, update)
      onSnapshot(snapshotFromFixture(state))
    },
  }
}

// Existing component fixtures describe normalized items. Deliver them through
// the pane's file HTTP messages; no provider frames or production hooks enter
// the browser tests. Unfinished replies remain live text through completion.
export function sendFileSnapshot(source, snapshot) {
  const previousLive = source.__fileLiveIDs ?? new Set()
  const rows = []
  const live = []
  const currentLive = new Set()
  for (const [index, item] of (snapshot.items ?? []).entries()) {
    const prompt = item.type === 'userMessage'
    const reply = item.type === 'agentMessage'
    if (!item.durableItemID && ((reply && (!item.done || previousLive.has(item.id))) || (prompt && item.clientActionId))) {
      currentLive.add(item.id)
      live.push({ id: item.id, kind: prompt ? 'prompt' : 'reply', text: item.text ?? '',
        ...(prompt ? { actionId: item.clientActionId, state: item.status ?? 'accepted',
          ...(item.allowedActions ? { allowedActions: item.allowedActions } : {}), ...(item.label ? { label: item.label } : {}) }
          : item.done ? { state: 'done' } : {}) })
    } else {
      // Only transcriptItem wire fields belong in saved rows. Owner DTO keys
      // such as done, item, clientActionId and nativeItemID must not leak in.
      rows.push({ id: item.durableItemID || item.id, type: prompt ? 'user_message' : reply ? 'agent_message' : item.type,
        category: item.category ?? liveItemCategory(item),
        role: prompt ? 'user' : 'assistant', projectedSeq: item.projectedSeq || index + 1,
        text: item.text || (!prompt && !reply ? JSON.stringify(item.item) : ''),
        createdAt: item.createdAt ?? null, filterTags: item.filterTags ?? null,
        synthetic: Boolean(item.synthetic), sidechain: Boolean(item.sidechain),
        isFinalAnswer: Boolean(item.isFinalAnswer), contentTruncated: Boolean(item.contentTruncated),
        ...(item.item ? { detail: item.item } : {}),
        ...((item.nativeItemID || item.nativeItemId) ? { nativeItemId: item.nativeItemID || item.nativeItemId } : {}),
        ...(item.status ? { status: item.status } : {}),
        ...(item.contentRef ? { contentRef: item.contentRef } : {}),
        ...(item.originalBytes ? { originalBytes: item.originalBytes } : {}),
        ...(item.phase ? { phase: item.phase } : {}) })
    }
  }
  const hidden = new Set(new URL(source.url, 'http://localhost').searchParams.get('hide')?.split(',') ?? [])
  const shownRows = rows.filter((row) => !hidden.has(row.category))
  source.__fileLiveIDs = currentLive
  source.onmessage?.({ data: JSON.stringify({ type: 'status', status: {
    epoch: snapshot.epoch, revision: snapshot.revision, runtime: snapshot.runtime, interactions: snapshot.interactions,
  } }) })
  source.onmessage?.({ data: JSON.stringify({ type: 'tail', threadId: 'nth_1', key: 'test-file-key', rows: shownRows, live,
    turns: (snapshot.turns ?? []).map((turn, index) => ({ id: turn.id, ordinal: turn.ordinal ?? index + 1,
      startSeq: turn.startSeq ?? 0, endSeq: turn.endSeq ?? 0, status: turn.status,
      startedAt: turn.startedAt ?? null, completedAt: turn.completedAt ?? null,
      ...((turn.finalItemId || turn.finalItemID) ? { finalItemId: turn.finalItemId || turn.finalItemID } : {}),
      ...(turn.durationMs ? { durationMs: turn.durationMs } : {}) })),
    page: { startSeq: shownRows[0]?.projectedSeq ?? 0, endSeq: shownRows.at(-1)?.projectedSeq ?? 0,
      latestSeq: shownRows.at(-1)?.projectedSeq ?? 0, snapshotHeadSeq: shownRows.at(-1)?.projectedSeq ?? 0,
      hasMoreBefore: false, startsMidTurn: false, nextBeforeCursor: null },
    projection: source.__fileProjection ?? { key: 'test-file-key', schemaVersion: 0, state: 'ready', sourceStatus: 'present',
      lastIndexedAt: null, evidenceAvailable: false, itemCount: shownRows.length },
  }) })
}

export function publishFileSnapshot(snapshot) {
  for (const source of EventSource.instances ?? []) {
    if (!source.closed && source.url?.includes('/live?')) sendFileSnapshot(source, snapshot)
  }
}

export function createFileTranscriptDriver({ provider = 'codex', nativeThreadID = 'native-1' } = {}) {
  return createMessageProjectionDriver(publishFileSnapshot, { provider, nativeThreadID })
}
