import { runtimeReady, turnState } from '../shared/lifecycle_state.js'

import { activityDot, cwdTail, displayModel, rowLabel } from './explorer_format.js'
import {
  resolveThreadParent, threadHeldByNano, threadIdentityKeys, threadWorkIsActive,
} from '../../explorer_local.js'

const UNKNOWN_SUBTITLE = '…'

// projectSidebar merges the two feeds the sidebar needs into one ordered list:
// the SSE-fresh catalog dataset, and `/api/sessions` summaries as a thin
// pending overlay. Rows are
// {key, kind, cls, provider, title, cwd, subtitle, indicator, click}, where
// `click` is the descriptor the row component routes on — the only place a row
// carries identity (session id, or provider + native id).
export function projectSidebar(dataset, sessions, now = new Date()) {
  const rows = []
  const projected = new Set() // session ids already represented by a dataset row
  const sessionsByThread = new Map() // provider:nativeId -> session summary
  for (const s of sessions ?? []) {
    if (s.threadId) sessionsByThread.set(`${s.provider}:${s.threadId}`, s)
  }

  // Order is the caller's. The dataset arrives already sorted by the shared
  // view (applyView) — the one sort both the toggle and the column headers
  // write to. Re-sorting here is what would let the rail and the expanded
  // table disagree.
  for (const item of dataset ?? []) {
    const runtime = item.runtime
    if (runtime?.attached) {
      if (projected.has(runtime.sessionId)) continue // one row per session, whatever the server sent
      projected.add(runtime.sessionId)
      rows.push(attachedRow(item, runtime))
      continue
    }
    // A session already holds this thread but the catalog's runtime overlay
    // hasn't caught up — the moments right after a resume. One thread, one
    // row: render the catalog row as attached to that session so the pending
    // overlay below skips it. Two rows would share a thread key and crash the
    // sidebar's keyed each block.
    const session = item.nativeId ? sessionsByThread.get(`${item.provider}:${item.nativeId}`) : null
    if (session && !projected.has(session.id)) {
      projected.add(session.id)
      const sessionRuntime = sessionRuntimeOverlay(session)
      rows.push(attachedRow({
        ...item,
        hostedThreadId: session.hostedThreadId ?? item.hostedThreadId,
        invokerHostedThreadId: session.invokerHostedThreadId ?? item.invokerHostedThreadId,
        invokerNativeThreadId: session.invokerNativeThreadId ?? item.invokerNativeThreadId,
        invokerProvider: session.invokerProvider ?? item.invokerProvider,
        sidebarPlacement: session.sidebarPlacement ?? item.sidebarPlacement,
        assignment: session.assignment ?? item.assignment,
        assignmentUnavailable: session.assignmentUnavailable ?? item.assignmentUnavailable,
        runtime: sessionRuntime,
      }, sessionRuntime))
    } else if (runtime?.detached) {
      rows.push(detachedRow(item))
    } else {
      rows.push(catalogRow(item, now))
    }
  }

  // Pending overlay: a session the dataset does not carry yet. It bridges the
  // sub-second gap between spawn and the catalog refetch, and it is the only
  // feed for an exited session — the server's memory-row overlay skips those,
  // but an exited session still owns its sidebar row until someone closes it.
  // It leads the list: it is live, and no sort can place a row the dataset has
  // never seen.
  const pending = []
  for (const s of sessions ?? []) {
    if (projected.has(s.id)) continue
    // A resume shell belongs to a durable catalog row even before its native
    // handshake finishes. During Nano restart, projecting those shells as new
    // pending rows duplicates the canonical list and makes every row move.
    // Genuine starts still use this overlay while their catalog row catches up.
    if (s.spawnMode === 'resume') continue
    projected.add(s.id)
    pending.push(pendingRow(s))
  }

  return [...pending, ...rows]
}

// The row's second line is one stable field across the compact rail. Liveness
// has its own visual channel (dot + chip), so a thread never swaps its model
// for a tmux window, a resume hint, or a timestamp as its runtime changes.
export function rowSubtitle(item, now = new Date()) {
  return modelSubtitle(
    knownValue(item.metadata?.model),
    knownValue(item.metadata?.effort),
    knownValue(item.metadata?.cwd)
  )
}

// Chip counts, derived from the projection so the chips can never disagree
// with the list they filter. Keys are the filter ids filterSidebar accepts.
export function sidebarCounts(rows) {
  const counts = { all: rows.length, working: 0, nano: 0 }
  for (const row of rows) {
    if (row.working) counts.working += 1
    if (row.cls !== 'none') counts[row.cls] += 1
  }
  return counts
}

// keepHistory is off for a narrowed view: its groups stay open, so the history
// rows a parent's disclosure needs would show as rows of their own.
export function filterSidebar(rows, filter, keepHistory = true) {
  if (filter === 'all') return rows
  const matches = rows.filter((row) => filter === 'working' ? row.working : row.cls === filter)
  // Nano keeps its host boundary: a thread Nano does not hold never crosses it.
  if (filter === 'nano') return keepHistory ? retainNanoTrees(rows, matches) : matches
  const visibleKeys = new Set(matches.map((row) => row.key))
  const byIdentity = new Map()
  for (const row of rows) {
    for (const key of threadIdentityKeys(row)) byIdentity.set(key, row)
  }
  // Chip filters retain supervisor context just like Explorer facets do. A
  // detached worker in the nano band must not become a top-level orphan merely
  // because its historical parent is outside that liveness band.
  for (const match of matches) {
    let cursor = match
    const visited = new Set()
    let parent = resolveThreadParent(cursor, byIdentity)
    while (parent && !visited.has(cursor.key)) {
      visited.add(cursor.key)
      visibleKeys.add(parent.key)
      cursor = parent
      parent = resolveThreadParent(cursor, byIdentity)
    }
  }
  return rows.filter((row) => visibleKeys.has(row.key))
}

function retainNanoTrees(rows, matches) {
  const visibleKeys = new Set(matches.map((row) => row.key))
  const byIdentity = new Map()
  for (const row of rows) {
    for (const key of threadIdentityKeys(row)) byIdentity.set(key, row)
  }
  // A visible Nano parent owns its whole descendant tree, including provider
  // subagents that are no longer attached. Keep those rows in the pipeline so
  // the parent's active/historical disclosures operate on the same population
  // as its catalog-wide agent count. This does not change the Nano chip count.
  const children = new Map()
  for (const row of rows) {
    const parent = resolveThreadParent(row, byIdentity)
    if (!parent || parent.key === row.key) continue
    const group = children.get(parent.key) ?? []
    group.push(row)
    children.set(parent.key, group)
  }
  const queue = rows.filter((row) => visibleKeys.has(row.key))
  for (let index = 0; index < queue.length; index += 1) {
    for (const child of children.get(queue[index].key) ?? []) {
      if (visibleKeys.has(child.key)) continue
      visibleKeys.add(child.key)
      queue.push(child)
    }
  }

  return visibleKeys.size === matches.length
    ? matches
    : arrangeVisibleHierarchy(rows.filter((row) => visibleKeys.has(row.key)))
}

// Render a selected subtree depth-first so worker disclosure can operate on
// adjacent parent/child rows.
function arrangeVisibleHierarchy(rows) {
  const list = rows ?? []
  const byIdentity = new Map()
  for (const row of list) {
    for (const key of threadIdentityKeys(row)) byIdentity.set(key, row)
  }
  const children = new Map()
  const nested = new Set()
  for (const row of list) {
    const parent = resolveThreadParent(row, byIdentity)
    if (!parent || parent.key === row.key) continue
    const group = children.get(parent.key) ?? []
    group.push(row)
    children.set(parent.key, group)
    nested.add(row.key)
  }
  const arranged = []
  const visited = new Set()
  const append = (row, depth = 0, last = false, treePath = []) => {
    if (visited.has(row.key)) return
    visited.add(row.key)
    arranged.push({ ...row, treeDepth: depth, treeLast: last, treePath })
    const group = children.get(row.key) ?? []
    const childPath = depth > 0 ? [...treePath, last] : []
    group.forEach((child, index) => append(child, depth + 1, index === group.length - 1, childPath))
  }
  for (const row of list) if (!nested.has(row.key)) append(row)
  for (const row of list) append(row)
  return arranged
}

// Background-only trees stay behind the global disclosure. Once any row in a
// tree is already visible, keep the whole tree together regardless of each
// child's presentation mode; its parent's agent-count control owns expansion.
// Waiting, failed, and exited work also promote their whole tree so lineage is
// never lost when something needs a human.
export function suppressBackgroundRows(rows, expanded = false) {
  const list = rows ?? []
  const byIdentity = new Map()
  for (const row of list) {
    for (const key of threadIdentityKeys(row)) byIdentity.set(key, row)
  }

  const neighbors = new Map(list.map((row) => [row.key, []]))
  for (const row of list) {
    const parent = resolveThreadParent(row, byIdentity)
    if (!parent || parent.key === row.key) continue
    neighbors.get(row.key)?.push(parent)
    neighbors.get(parent.key)?.push(row)
  }

  const visibleKeys = new Set()
  const queue = list.filter((row) => (
    row.runtime?.presentationMode !== 'background' || backgroundNeedsAttention(row)
  ))
  for (const row of queue) visibleKeys.add(row.key)
  for (let index = 0; index < queue.length; index += 1) {
    for (const neighbor of neighbors.get(queue[index].key) ?? []) {
      if (visibleKeys.has(neighbor.key)) continue
      visibleKeys.add(neighbor.key)
      queue.push(neighbor)
    }
  }

  const visible = list.filter((row) => visibleKeys.has(row.key))
  const globallyFoldedCount = list.length - visible.length
  return {
    rows: expanded ? list : visible,
    backgroundCount: globallyFoldedCount,
    hiddenCount: expanded ? 0 : globallyFoldedCount,
  }
}

// The compact rail has one host boundary the expanded catalog does not: Nano
// sessions form the first band. Rank whole relationship trees by the root's
// own state, never by a descendant's state. The already-arranged children keep
// their own sibling order inside that root's tree.
export function orderSidebarRows(rows) {
  const rank = (row) => {
    if (row.cls === 'nano') {
      if (row.working) return 0
      return row.kind === 'detached' ? 2 : 1
    }
    return row.working ? 3 : 4
  }
  const arranged = arrangeVisibleHierarchy(rows ?? [])
  const groups = []
  for (const row of arranged) {
    if ((row.treeDepth ?? 0) === 0 || groups.length === 0) {
      groups.push({ rows: [row], index: groups.length })
    } else {
      groups[groups.length - 1].rows.push(row)
    }
  }
  return groups
    .map((group) => ({ ...group, rank: rank(group.rows[0]) }))
    .sort((a, b) => a.rank - b.rank || a.index - b.index)
    .flatMap((group) => group.rows)
}

// Starred rows bring the useful part of their family into the section: their
// ancestor path supplies lineage context, and their descendants travel with
// them as one disclosure-owned subtree. Context rows keep their own unstarred
// state, so only an explicit star renders filled. A Set makes overlapping
// starred families appear once.
export function sectionSidebar(rows, reorder = true) {
  const list = rows ?? []
  const byIdentity = new Map()
  const children = new Map()
  for (const row of list) {
    for (const key of threadIdentityKeys(row)) byIdentity.set(key, row)
  }
  for (const row of list) {
    const parent = resolveThreadParent(row, byIdentity)
    if (!parent || parent.key === row.key) continue
    const group = children.get(parent.key) ?? []
    group.push(row)
    children.set(parent.key, group)
  }

  const starredKeys = new Set()
  const descendantKeys = new Set()
  const ancestorKeys = new Set()
  for (const row of list) {
    // A star never promotes a thread Nano does not hold, or its parent.
    if (!row.starred || row.cls === 'none') continue

    const descendants = [row]
    const visitedDescendants = new Set()
    for (let index = 0; index < descendants.length; index += 1) {
      const descendant = descendants[index]
      if (visitedDescendants.has(descendant.key)) continue
      visitedDescendants.add(descendant.key)
      starredKeys.add(descendant.key)
      descendantKeys.add(descendant.key)
      descendants.push(...(children.get(descendant.key) ?? []))
    }

    let cursor = row
    const visited = new Set([row.key])
    let parent = resolveThreadParent(cursor, byIdentity)
    while (parent && !visited.has(parent.key)) {
      visited.add(parent.key)
      starredKeys.add(parent.key)
      ancestorKeys.add(parent.key)
      cursor = parent
      parent = resolveThreadParent(cursor, byIdentity)
    }
  }

  const arrange = reorder ? orderSidebarRows : arrangeVisibleHierarchy
  const starred = list
    .filter((row) => starredKeys.has(row.key))
    .map((row) => {
      if (!ancestorKeys.has(row.key) || descendantKeys.has(row.key)) return row
      // A context-only ancestor may have siblings left in the ordinary
      // section. Recompute its disclosure count from this selected slice so
      // its caret never promises rows that cannot appear here.
      return {
        ...row,
        subagentActivity: null,
        sidebar: row.sidebar ? { ...row.sidebar, historicalDescendantCount: null } : row.sidebar,
      }
    })
  const rest = list.filter((row) => !starredKeys.has(row.key))
  return {
    starred: arrange(starred),
    rest: arrange(rest),
  }
}

function attachedRow(item, runtime) {
  const label = catalogTitle(item)
  return {
    id: item.id,
    key: threadKey(item.provider, item.nativeId, item.hostedThreadId, `session:${runtime.sessionId}`),
    kind: 'attached',
    runtime,
    cls: 'nano',
    provider: item.provider,
    nativeId: item.nativeId,
    hostedThreadId: item.hostedThreadId ?? null,
    invokerHostedThreadId: item.invokerHostedThreadId ?? null,
    invokerNativeThreadId: item.invokerNativeThreadId ?? null,
    invokerProvider: item.invokerProvider ?? null,
    sidebarPlacement: item.sidebarPlacement ?? 'invoker',
    parentNativeId: item.parentNativeId ?? null,
    assignment: item.assignment ?? null,
    assignmentUnavailable: Boolean(item.assignmentUnavailable),
    threadKind: item.kind,
    treeDepth: item.treeDepth ?? 0,
    treeLast: item.treeLast ?? false,
    treePath: item.treePath ?? [],
    subagentActivity: item.subagentActivity ?? null,
    fleet: item.fleet ?? null,
    working: threadWorkIsActive(item),
    starred: Boolean(item.starred),
    mail: item.mail ?? null,
    sidebar: item.sidebar ?? null,
    title: label.text,
    titleKind: label.kind,
    cwd: knownValue(item.metadata?.cwd),
    subtitle: rowSubtitle(item),
    updatedAt: item.updatedAt ?? item.createdAt ?? null,
    indicator: activityDot(item),
    click: { action: 'open', sessionId: runtime.sessionId, catalogId: item.transient ? null : item.id },
  }
}

function detachedRow(item) {
  const label = catalogTitle(item)
  return {
    id: item.id,
    key: threadKey(item.provider, item.nativeId, item.hostedThreadId, `row:${item.id}`),
    kind: 'detached',
    runtime: item.runtime,
    cls: threadHeldByNano(item) ? 'nano' : 'none',
    provider: item.provider,
    nativeId: item.nativeId,
    hostedThreadId: item.hostedThreadId ?? null,
    invokerHostedThreadId: item.invokerHostedThreadId ?? null,
    invokerNativeThreadId: item.invokerNativeThreadId ?? null,
    invokerProvider: item.invokerProvider ?? null,
    sidebarPlacement: item.sidebarPlacement ?? 'invoker',
    parentNativeId: item.parentNativeId ?? null,
    assignment: item.assignment ?? null,
    assignmentUnavailable: Boolean(item.assignmentUnavailable),
    threadKind: item.kind,
    treeDepth: item.treeDepth ?? 0,
    treeLast: item.treeLast ?? false,
    treePath: item.treePath ?? [],
    subagentActivity: item.subagentActivity ?? null,
    fleet: item.fleet ?? null,
    working: threadWorkIsActive(item),
    starred: Boolean(item.starred),
    mail: item.mail ?? null,
    sidebar: item.sidebar ?? null,
    title: label.text,
    titleKind: label.kind,
    cwd: knownValue(item.metadata?.cwd),
    subtitle: rowSubtitle(item),
    updatedAt: item.updatedAt ?? item.createdAt ?? null,
    indicator: activityDot(item),
    click: { action: 'history', catalogId: item.id, provider: item.provider, nativeId: item.nativeId },
  }
}

function pendingRow(session) {
  // A just-created session has no durable title yet, but its dialog name is
  // already server-owned and should not wait for the first prompt to paint.
  const label = rowLabel({ name: session.name })
  const runtime = sessionRuntimeOverlay(session)
  return {
    key: threadKey(session.provider, session.threadId, session.hostedThreadId, `session:${session.id}`),
    kind: 'pending',
    runtime,
    cls: 'nano',
    provider: session.provider,
    nativeId: session.threadId,
    hostedThreadId: session.hostedThreadId ?? null,
    invokerHostedThreadId: session.invokerHostedThreadId ?? null,
    invokerNativeThreadId: session.invokerNativeThreadId ?? null,
    invokerProvider: session.invokerProvider ?? null,
    sidebarPlacement: session.sidebarPlacement ?? 'invoker',
    assignment: session.assignment ?? null,
    assignmentUnavailable: Boolean(session.assignmentUnavailable),
    threadKind: 'thread',
    treeDepth: 0,
    treeLast: false,
    treePath: [],
    subagentActivity: null,
    working: threadWorkIsActive({ runtime }),
    starred: Boolean(session.starred),
    title: label.text,
    titleKind: label.kind,
    cwd: session.cwd || null,
    subtitle: modelSubtitle(session.model, session.effort, session.cwd),
    updatedAt: session.updatedAt ?? session.createdAt ?? null,
    // An exited session is live nowhere: no dot state; its close affordance
    // remains visible on hover without changing the row's metadata line.
    indicator: activityDot({ runtime }),
    click: { action: 'open', sessionId: session.id },
  }
}

// A thread Nano is not holding. Its catalog id opens durable history.
function catalogRow(item, now) {
  const label = catalogTitle(item)
  return {
    id: item.id,
    key: threadKey(item.provider, item.nativeId, item.hostedThreadId, `row:${item.id}`),
    kind: 'catalog',
    runtime: item.runtime,
    cls: 'none',
    provider: item.provider,
    nativeId: item.nativeId,
    hostedThreadId: item.hostedThreadId ?? null,
    invokerHostedThreadId: item.invokerHostedThreadId ?? null,
    invokerNativeThreadId: item.invokerNativeThreadId ?? null,
    invokerProvider: item.invokerProvider ?? null,
    sidebarPlacement: item.sidebarPlacement ?? 'invoker',
    parentNativeId: item.parentNativeId ?? null,
    assignment: item.assignment ?? null,
    assignmentUnavailable: Boolean(item.assignmentUnavailable),
    threadKind: item.kind,
    treeDepth: item.treeDepth ?? 0,
    treeLast: item.treeLast ?? false,
    treePath: item.treePath ?? [],
    subagentActivity: item.subagentActivity ?? null,
    fleet: item.fleet ?? null,
    working: threadWorkIsActive(item),
    starred: Boolean(item.starred),
    mail: item.mail ?? null,
    sidebar: item.sidebar ?? null,
    title: label.text,
    titleKind: label.kind,
    cwd: knownValue(item.metadata?.cwd),
    subtitle: rowSubtitle(item, now),
    updatedAt: item.updatedAt ?? item.createdAt ?? null,
    indicator: activityDot(item),
    click: { action: 'history', catalogId: item.id, nativeId: item.nativeId || null },
  }
}

// The runtime overlay a session summary implies, for the two places the
// sidebar has to build one itself: the pending row (a session the catalog has
// never seen) and the catalog row a session already holds while the server's
// own overlay catches up. It carries the turn state and the live-turn anchor
// verbatim, so a row bridged through either path keeps its ticking timer
// instead of losing it for the second or two before the catalog agrees.
function sessionRuntimeOverlay(session) {
  return {
    attached: session.status !== 'exited',
    sessionId: session.id,
    status: session.status,
    threadStatus: session.threadStatus,
    turnStartedAtMs: session.turnStartedAtMs,
    compactingSinceMs: session.compactingSinceMs,
    lastTurnEndedAt: session.lastTurnEndedAt,
    error: session.error,
    needsAttention: session.needsAttention,
    presentationMode: session.presentationMode ?? 'shared',
  }
}

function backgroundNeedsAttention(row) {
  const runtime = row.runtime ?? {}
  return Boolean((runtimeReady(runtime) && turnState(row) === 'needs_you') || runtime.needsAttention || runtime.error || runtime.status === 'exited')
}

// Keyed on the most durable identity available, not row id. Hosted identity
// exists before a provider native id and survives attachment changes, so the
// spawn → native-id → durable-row handoff keeps one DOM node. Non-hosted rows
// retain provider/native identity, then fall back to their feed id.
function threadKey(provider, nativeId, hostedThreadId, fallback) {
  if (hostedThreadId) return `hosted:${hostedThreadId}`
  return nativeId ? `${provider}:${nativeId}` : fallback
}

// Title chain: rowLabel (explorer_format), the ONE chain both the compact
// rail and the expanded Explorer render — the rule (the operator, 2026-07-29) is that
// the Explorer is an expansion of the compact view, so the two surfaces must
// never disagree. An unnamed, untitled row reads as the muted dash; the
// working directory has its own line and is never repeated as a title.
function catalogTitle(item) {
  return rowLabel({ name: item.name?.value, title: item.title, preview: item.preview, kind: item.kind })
}

function modelSubtitle(model, effort, cwd) {
  const display = displayModel(model)
  if (display) return effort ? `${display} · ${effort}` : display
  return effort || cwdTail(cwd) || UNKNOWN_SUBTITLE
}

function knownValue(field) {
  return field?.state === 'known' ? field.value : null
}
