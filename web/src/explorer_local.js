import { turnState } from './lib/shared/lifecycle_state.js'
// Local view pipeline over the in-memory thread window. The dataset (one
// fetched window of the catalog) is held whole in the browser, so sorting,
// facet selections, date ranges, and facet counts are all computed here —
// instantly, with no server round trip. Semantics deliberately mirror the
// backend list query: text sorts compare case-insensitively, values the
// provider never recorded sort last, ties break by id in the sort direction.

const TEXT_SORTS = {
  project: (t) => knownValue(t.metadata?.cwd),
  provider: (t) => t.provider,
  model: (t) => knownValue(t.metadata?.model),
  effort: (t) => knownValue(t.metadata?.effort),
}
const TIME_SORTS = {
  created: (t) => t.createdAt ?? null,
  updated: (t) => t.updatedAt ?? null,
}

function knownValue(field) {
  return field?.state === 'known' ? field.value : null
}

// Liveness rank — the sidebar's grouping, defined once so the compact rail and
// the expanded table cannot drift. Ordering uses only this thread's own work:
// descendant activity may make a supervisor look active, but it must not move
// the supervisor's whole tree. Detached follows what Nano currently holds,
// then plain history. Column-header sorts still replace this grouping.
export function livenessRank(item) {
  const ranked = item?.sidebar?.sort?.liveness
  if (Number.isFinite(ranked)) return ranked
  if (threadWorkIsActive(item)) return 0
  if (item.runtime?.attached) return 1
  if (item.runtime?.detached) return 2
  return 3
}

// A thread is working when the server's turn state for it says so. A
// supervisor's descendants count on their own rows, never on the supervisor.
export function threadWorkIsActive(item) {
  return turnState(item) === 'working'
}

// Agent disclosure answers whether a child still belongs to the current Nano
// workspace, not whether it owes work at this instant. An attached idle agent
// is current. A provider-native worker that is not a Nano session also remains
// current while it is working. Only neither-attached-nor-working rows are
// historical.
export function threadAgentIsCurrent(item) {
  if (item?.sidebar) {
    return Boolean(
      item.sidebar.active
      || item.sidebar.structural
      || (item.sidebar.descendantActivity?.active > 0),
    )
  }
  return Boolean(item.runtime?.attached || threadWorkIsActive(item) || item.subagentActivity?.active > 0)
}

// Current hosted assignment is the only Nano-owned role authority. Provider
// native subagents retain worker as compatibility evidence only when they are
// not hosted; invocation lineage itself never implies a role.
export function resolveThreadRole(item) {
  if (item?.assignmentUnavailable) return null
  const assignmentRole = item?.assignment?.assignmentRole
  if (item?.hostedThreadId && (assignmentRole === 'worker' || assignmentRole === 'auditor')) return assignmentRole
  if (!item?.hostedThreadId && (item?.threadKind ?? item?.kind) === 'subagent') return 'worker'
  return null
}

// The sort toggle's modes. Both keep the liveness grouping above — working,
// attached-idle, detached, then history. Updated orders working rows by their
// immutable start; Working dir keeps its stable project grouping and applies
// that start order inside each project. Column-header clicks still set a plain
// sort instead.
export const SORT_MODES = [
  { id: 'liveness-updated', label: 'Updated' },
  { id: 'liveness-project', label: 'Working dir' },
]
const LIVENESS = 'liveness-'

// Comparators that return 0 on a tie (unlike compareRows, which falls through
// to the id tiebreak) so the modes can chain: working dir, then recency.
function sortText(item, field, fallback) {
  const value = item?.sidebar?.sort?.[field]
  if (typeof value === 'string' && value) return value
  return fallback(item)
}

function sortTime(item, field, fallback) {
  const value = item?.sidebar?.sort?.[field]
  if (Number.isFinite(value) && value > 0) return value
  return epochMillis(fallback(item))
}

function epochMillis(value) {
  if (value == null || value === '') return null
  if (typeof value === 'number' && Number.isFinite(value)) return value
  const parsed = Date.parse(value)
  return Number.isFinite(parsed) ? parsed : null
}

function byProject(a, b) {
  const va = sortText(a, 'project', TEXT_SORTS.project)
  const vb = sortText(b, 'project', TEXT_SORTS.project)
  if (va == null || vb == null) return va == null && vb == null ? 0 : va == null ? 1 : -1
  const la = String(va).toLowerCase()
  const lb = String(vb).toLowerCase()
  return la < lb ? -1 : la > lb ? 1 : 0
}

function byUpdatedDesc(a, b) {
  const va = sortTime(a, 'updatedAt', TIME_SORTS.updated)
  const vb = sortTime(b, 'updatedAt', TIME_SORTS.updated)
  if (!va || !vb) return !va && !vb ? 0 : !va ? 1 : -1
  return va < vb ? 1 : va > vb ? -1 : 0
}

function byWorkingStart(a, b) {
  const read = (item) => {
    const value = Number(item.runtime?.turnStartedAtMs || 0)
    return Number.isFinite(value) && value > 0 ? value : Number.MAX_SAFE_INTEGER
  }
  return read(a) - read(b)
}

// Facet field -> the selection key in the view state and the row value reader.
export const FACET_FIELDS = {
  project: { selectionKey: 'projects', value: TEXT_SORTS.project },
  provider: { selectionKey: 'providers', value: TEXT_SORTS.provider },
  model: { selectionKey: 'models', value: TEXT_SORTS.model },
  effort: { selectionKey: 'efforts', value: TEXT_SORTS.effort },
}

function selectionAdmits(selection, value) {
  if (selection == null) return true
  if (value == null) return selection.unknown
  return selection.values.includes(value)
}

// Inclusive local-date range check against an ISO timestamp.
function rangeAdmits(range, iso) {
  if (range == null) return true
  if (!iso) return false
  const at = new Date(iso).getTime()
  if (range.from) {
    const [y, m, d] = range.from.split('-').map(Number)
    if (at < new Date(y, m - 1, d).getTime()) return false
  }
  if (range.to) {
    const [y, m, d] = range.to.split('-').map(Number)
    if (at > new Date(y, m - 1, d, 23, 59, 59, 999).getTime()) return false
  }
  return true
}

function rowPasses(row, view, skipField) {
  for (const [field, spec] of Object.entries(FACET_FIELDS)) {
    if (field === skipField) continue
    if (!selectionAdmits(view[spec.selectionKey], spec.value(row))) return false
  }
  if (skipField !== 'ranges') {
    if (!rangeAdmits(view.createdRange, row.createdAt)) return false
    if (!rangeAdmits(view.updatedRange, row.updatedAt)) return false
  }
  return true
}

function compareRows(a, b, sort, dir) {
  const asc = dir === 'asc'
  if (sort.startsWith(LIVENESS)) {
    const rank = livenessRank(a) - livenessRank(b)
    if (rank !== 0) return rank
    if (sort.slice(LIVENESS.length) === 'project') {
      const byDir = byProject(a, b)
      if (byDir !== 0) return byDir
    }
    if (livenessRank(a) === 0) {
      const working = byWorkingStart(a, b)
      return working !== 0 ? working : tieBreak(a, b, true)
    }
    const recent = byUpdatedDesc(a, b) // non-working bands break ties on recency
    return recent !== 0 ? recent : tieBreak(a, b, false)
  }
  const textField = sort === 'project' ? 'project' : sort === 'title' ? 'title' : null
  const timeField = sort === 'created' ? 'createdAt' : sort === 'updated' ? 'updatedAt' : null
  const read = TEXT_SORTS[sort] ?? TIME_SORTS[sort] ?? TIME_SORTS.updated
  let va = textField ? sortText(a, textField, read) : timeField ? sortTime(a, timeField, read) : read(a)
  let vb = textField ? sortText(b, textField, read) : timeField ? sortTime(b, timeField, read) : read(b)
  if (timeField) {
    va = epochMillis(va)
    vb = epochMillis(vb)
  }
  // Nulls last regardless of direction (matching the backend's IS NULL ASC).
  if (va == null || vb == null) {
    if (va == null && vb == null) return tieBreak(a, b, asc)
    return va == null ? 1 : -1
  }
  if (TEXT_SORTS[sort] || textField) {
    va = String(va).toLowerCase()
    vb = String(vb).toLowerCase()
  }
  if (va !== vb) return (va < vb ? -1 : 1) * (asc ? 1 : -1)
  return tieBreak(a, b, asc)
}

function tieBreak(a, b, asc) {
  if (a.id === b.id) return 0
  return (a.id < b.id ? -1 : 1) * (asc ? 1 : -1)
}

// The visible list: dataset -> facet selections + date ranges -> sort.
export function applyView(dataset, view) {
  const decorated = decorateSubagentActivity(dataset)
  const admitted = decorated.filter((row) => rowPasses(row, view, null))
  const visibleIDs = new Set(admitted.map((row) => row.id))
  const byIdentity = indexThreadIdentities(decorated)
  // A matching worker pulls its ancestor chain in as structural context. This
  // keeps every visible worker attached even when a local facet/date filter
  // would otherwise hide its supervisor.
  for (const row of admitted) {
    let cursor = row
    const visited = new Set()
    let parent = resolveThreadParent(cursor, byIdentity)
    while (parent && !visited.has(cursor.id)) {
      visited.add(cursor.id)
      visibleIDs.add(parent.id)
      cursor = parent
      parent = resolveThreadParent(cursor, byIdentity)
    }
  }
  const sorted = decorated
    .filter((row) => visibleIDs.has(row.id))
    .sort((a, b) => compareRows(a, b, view.sort, view.dir))
  return arrangeThreadHierarchy(sorted)
}

// Hosted identity is the provider-neutral hierarchy key. Native identity stays
// as an alias because provider-owned subagents still point at parentNativeId.
// This lets a Codex parent own a Claude or Grok worker without pretending their
// provider-native ids share one namespace.
export function threadIdentity(row) {
  const hostedThreadId = row?.hostedThreadId ?? row?.runtime?.hostedThreadId
  if (hostedThreadId) return `hosted:${hostedThreadId}`
  return row?.nativeId ? `${row.provider ?? ''}:${row.nativeId}` : null
}

export function threadIdentityKeys(row) {
  const keys = []
  const preferred = threadIdentity(row)
  if (preferred) keys.push(preferred)
  const native = row?.nativeId ? `${row.provider ?? ''}:${row.nativeId}` : null
  if (native && native !== preferred) keys.push(native)
  if (row?.id && !keys.includes(row.id)) keys.push(row.id)
  return keys
}

export function threadParentIdentity(row) {
  return threadParentIdentityKeys(row)[0] ?? null
}

// Invocation snapshots retain both the durable hosted id and the provider's
// native id. A manually reopened thread can acquire a new hosted record while
// its resumed conversation still names the older hosted identity. Try every
// retained spelling so that child stays attached to the one visible catalog
// row for that provider conversation. Mutable relationships remain first.
export function threadParentIdentityKeys(row) {
  const keys = []
  const push = (key) => { if (key && !keys.includes(key)) keys.push(key) }
  const relatedHostedThreadId = !row?.assignmentUnavailable
    ? row?.assignment?.relatedHostedThreadId
    : null
  const sidebarPlacement = row?.sidebarPlacement ?? row?.runtime?.sidebarPlacement ?? 'invoker'
  if (relatedHostedThreadId || sidebarPlacement !== 'standalone') push(row?.sidebar?.parentId ?? null)
  push(relatedHostedThreadId ? `hosted:${relatedHostedThreadId}` : null)
  const invokerHostedThreadId = row?.invokerHostedThreadId ?? row?.runtime?.invokerHostedThreadId
  const invokerNativeThreadId = row?.invokerNativeThreadId ?? row?.runtime?.invokerNativeThreadId
  const invokerProvider = row?.invokerProvider ?? row?.runtime?.invokerProvider ?? row?.provider ?? ''
  if (relatedHostedThreadId) {
    // A resumed parent can replace the hosted id while retaining the same
    // provider conversation. Its native invocation snapshot is a valid alias
    // only when the assignment still names that original invoker.
    if (relatedHostedThreadId === invokerHostedThreadId) {
      push(invokerNativeThreadId ? `${invokerProvider}:${invokerNativeThreadId}` : null)
    }
    return keys
  }
  if (sidebarPlacement === 'standalone') return keys
  push(invokerHostedThreadId ? `hosted:${invokerHostedThreadId}` : null)
  push(invokerNativeThreadId ? `${invokerProvider}:${invokerNativeThreadId}` : null)
  push(row?.parentNativeId ? `${row.provider ?? ''}:${row.parentNativeId}` : null)
  return keys
}

export function resolveThreadParent(row, byIdentity) {
  for (const key of threadParentIdentityKeys(row)) {
    const parent = byIdentity?.get(key)
    if (parent) return parent
  }
  return null
}

function indexThreadIdentities(rows) {
  const byIdentity = new Map()
  for (const row of rows ?? []) {
    for (const key of threadIdentityKeys(row)) byIdentity.set(key, row)
  }
  return byIdentity
}

// Whether the view is narrowed to a subset rather than browsing everything.
// Sort/direction reorder the same rows and so do not count.
const NARROWING_VIEW_KEYS = ['projects', 'providers', 'models', 'efforts', 'createdRange', 'updatedRange']

export function viewIsNarrowed(view) {
  return NARROWING_VIEW_KEYS.some((key) => view?.[key] != null)
}

// Roll worker counts and activity up to every visible ancestor. The source is
// the whole fetched window, before local facets/date ranges, so filtering a
// worker out of the table cannot make its still-running supervisor look idle.
export function decorateSubagentActivity(rows) {
  const identity = threadIdentity
  const byIdentity = indexThreadIdentities(rows)
  const children = new Map()
  for (const row of rows) {
    const parent = resolveThreadParent(row, byIdentity)
    if (!parent || parent.id === row.id) continue
    const key = identity(parent)
    const group = children.get(key) ?? []
    group.push(row)
    children.set(key, group)
  }

  const memo = new Map()
  const summarize = (row, path = new Set()) => {
    if (memo.has(row.id)) return memo.get(row.id)
    if (path.has(row.id)) return { total: 0, active: 0, workers: 0 }
    const nextPath = new Set(path).add(row.id)
    let total = 0
    let active = 0
    let workers = 0
    for (const child of children.get(identity(row)) ?? []) {
      const descendants = summarize(child, nextPath)
      total += 1 + descendants.total
      active += (threadWorkIsActive(child) ? 1 : 0) + descendants.active
      workers += (resolveThreadRole(child) === 'worker' ? 1 : 0) + descendants.workers
    }
    const summary = { total, active, workers }
    memo.set(row.id, summary)
    return summary
  }

  return rows.map((row) => {
    const server = row.sidebar?.descendantActivity
    if (server && Number.isFinite(server.total)) {
      return server.total > 0 ? { ...row, subagentActivity: server } : row
    }
    const summary = summarize(row)
    return summary.total > 0 ? { ...row, subagentActivity: summary } : row
  })
}

// Keep a worker directly below its supervisor when both survive the active
// filters. The ordinary sort still determines the supervisor order and the
// order of siblings. Missing parents stay in their sorted position instead of
// disappearing, which matters when a time/facet filter selects only a worker.
export function arrangeThreadHierarchy(rows) {
  const identity = threadIdentity
  const byIdentity = indexThreadIdentities(rows)

  const children = new Map()
  const nestedIDs = new Set()
  for (const row of rows) {
    const parent = resolveThreadParent(row, byIdentity)
    if (!parent || parent.id === row.id) continue
    const key = identity(parent)
    const group = children.get(key) ?? []
    group.push(row)
    children.set(key, group)
    nestedIDs.add(row.id)
  }

  const arranged = []
  const visited = new Set()
  const append = (row, depth = 0, last = false, treePath = []) => {
    if (visited.has(row.id)) return
    visited.add(row.id)
    arranged.push(depth > 0 ? { ...row, treeDepth: depth, treeLast: last, treePath } : { ...row, treeDepth: 0 })
    const group = children.get(identity(row)) ?? []
    const childPath = depth > 0 ? [...treePath, last] : []
    group.forEach((child, index) => append(child, depth + 1, index === group.length - 1, childPath))
  }
  for (const row of rows) if (!nestedIDs.has(row.id)) append(row)
  // A malformed provider cycle must not make evidence vanish.
  for (const row of rows) append(row)
  return arranged
}

// Worker groups share one disclosure state across both renderers. Expanding an
// agent badge reveals descendants that currently owe work. Finished descendants
// stay behind a separate historical disclosure, so a long-lived parent does
// not fan dozens of completed agents across the sidebar after every refresh.
// A group is the rows actually here, not what
// `subagentActivity` counts: that roll-up spans the whole fetched window on
// purpose, so a supervisor whose workers are outside it has nothing to disclose
// and heads no group. Rows arrive from arrangeThreadHierarchy in depth-first
// order, so tracking one depth hides whole branches in a single pass.
//
// Callers must not collapse a narrowed view. Both narrowing paths keep matches
// plus their supervisor chain as context, so every group left standing holds a
// match and closing it would hide the rows the filter was set to find.
export function collapseWorkerGroups(rows, expandedKeys = [], historicalExpandedKeys = []) {
  const expanded = new Set(expandedKeys)
  const historicalExpanded = new Set(historicalExpandedKeys)
  const expandAll = expanded.has('*')
  const list = rows ?? []
  const visible = []
  const historyControls = []

  // The roll-up is normally present, but calculating from this arranged slice
  // keeps the historical control exact when a renderer carries fewer rows than
  // the catalog-wide activity summary counted.
  const inactivePrefix = [0]
  for (const row of list) inactivePrefix.push(inactivePrefix.at(-1) + (threadAgentIsCurrent(row) ? 0 : 1))
  const workingPrefix = [0]
  for (const row of list) workingPrefix.push(workingPrefix.at(-1) + (threadWorkIsActive(row) ? 1 : 0))
  const subtreeEnds = list.map(() => list.length)
  const openSubtrees = []
  for (let index = 0; index < list.length; index += 1) {
    const depth = list[index].treeDepth ?? 0
    while (openSubtrees.length > 0 && openSubtrees.at(-1).depth >= depth) {
      subtreeEnds[openSubtrees.pop().index] = index
    }
    openSubtrees.push({ index, depth })
  }
  const historicalCounts = list.map((_, index) => inactivePrefix[subtreeEnds[index]] - inactivePrefix[index + 1])
  const workingCounts = list.map((_, index) => workingPrefix[subtreeEnds[index]] - workingPrefix[index + 1])

  const ancestors = []
  for (const [index, row] of list.entries()) {
    const depth = row.treeDepth ?? 0
    ancestors.length = depth
    const parentState = depth > 0 ? ancestors[depth - 1] : null
    const visibleUnderParent = !parentState || (
      parentState.visible &&
      parentState.workersExpanded &&
      (parentState.historicalAllowed || threadAgentIsCurrent(row))
    )
    const hasChildren = (list[index + 1]?.treeDepth ?? 0) > depth
    const historicalCount = Number.isFinite(row.sidebar?.historicalDescendantCount)
      ? row.sidebar.historicalDescendantCount
      : historicalCounts[index]
    const totalCount = Number.isFinite(row.subagentActivity?.total)
      ? row.subagentActivity.total
      : subtreeEnds[index] - index - 1
    const currentCount = Math.max(0, totalCount - historicalCount)
    const countFields = {
      currentSubagentCount: currentCount,
      historicalAgentCount: historicalCount,
      workingSubagentCount: workingCounts[index],
    }
    const collapsible = hasChildren || historicalCount > 0
    const key = threadIdentity(row)
    const workersExpanded = collapsible && (expandAll ? !expanded.has(`!${key}`) : expanded.has(key))
    const historicalInherited = Boolean(parentState?.historicalAllowed)
    const historicalExplicit = Boolean(key && historicalExpanded.has(key))
    const historicalAllowed = historicalInherited || historicalExplicit
    const state = { visible: visibleUnderParent, workersExpanded, historicalAllowed }
    ancestors[depth] = state
    if (!visibleUnderParent) continue

    if (!collapsible) {
      visible.push(totalCount > 0 ? { ...row, ...countFields } : row)
      continue
    }
    const visibleRow = {
      ...row,
      ...countFields,
      workersCollapsible: true,
      workersExpanded,
      historicalAgentsExpanded: historicalAllowed,
      historicalAgentsInherited: historicalInherited,
    }
    visible.push(visibleRow)
    if (workersExpanded && historicalCount > 0 && !historicalInherited) {
      historyControls.push({
        parent: visibleRow,
        parentDepth: depth,
        depth: depth + 1,
        count: historicalCount,
        expanded: historicalAllowed,
        startIndex: visible.length - 1,
      })
    }
  }

  // A history action belongs after the subtree it changes, not beside the
  // parent's compact count. Attach it to the last row that survives collapse
  // so both renderers can emit the control immediately after that row without
  // introducing a fake thread into navigation, selection, or diagnostics.
  const controlsByTarget = new Map()
  for (const control of historyControls) {
    let target = control.startIndex
    while (target + 1 < visible.length && (visible[target + 1].treeDepth ?? 0) > control.parentDepth) target += 1
    const controls = controlsByTarget.get(target) ?? []
    controls.push(control)
    controlsByTarget.set(target, controls)
  }
  for (const [target, controls] of controlsByTarget) {
    // When nested groups end on the same leaf, the inner action comes first.
    controls.sort((a, b) => b.depth - a.depth)
    visible[target] = { ...visible[target], subagentHistoryControlsAfter: controls }
  }
  return visible
}

// Facet counts for one column's picker: distinct values with counts under
// every OTHER active selection/range (never the column's own), plus the
// count of rows where the field is unknown. Sorted by value, matching the
// server facet endpoints' shape.
export function computeFacets(dataset, view, field) {
  const spec = FACET_FIELDS[field]
  const counts = new Map()
  let unknownCount = 0
  for (const row of dataset) {
    if (!rowPasses(row, view, field)) continue
    const value = spec.value(row)
    if (value == null) unknownCount += 1
    else counts.set(value, (counts.get(value) ?? 0) + 1)
  }
  const values = [...counts.entries()]
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([value, count]) => ({ value, count }))
  return { values, unknownCount }
}
