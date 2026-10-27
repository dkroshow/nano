import { runtimeLabel, runtimeReady, turnState } from './lifecycle_state.js'
// Pure projection for the parent-close confirmation. It uses the same catalog
// lineage and role helpers as the rail, but deliberately does not alter them:
// selecting an intermediate child never selects its descendants by implication.
import { activityDot, rowLabel, sidebarAge } from './explorer_format.js'
import {
  decorateSubagentActivity,
  resolveThreadParent,
  threadIdentity,
  threadIdentityKeys,
} from '../explorer_local.js'

function sessionKeys(session) {
  const keys = []
  if (session?.hostedThreadId) keys.push(`hosted:${session.hostedThreadId}`)
  if (session?.provider && session?.threadId) keys.push(`${session.provider}:${session.threadId}`)
  return keys
}

function sessionIndex(sessions) {
  const byIdentity = new Map()
  for (const session of sessions ?? []) {
    for (const key of sessionKeys(session)) byIdentity.set(key, session)
  }
  return byIdentity
}

function descendantsByParent(rows) {
  const byIdentity = new Map()
  for (const row of rows ?? []) {
    for (const key of threadIdentityKeys(row)) byIdentity.set(key, row)
  }
  const children = new Map()
  for (const row of rows ?? []) {
    const parentRow = resolveThreadParent(row, byIdentity)
    const parent = parentRow ? threadIdentity(parentRow) : null
    const identity = threadIdentity(row)
    if (!parent || !identity || parent === identity) continue
    const group = children.get(parent) ?? []
    group.push(row)
    children.set(parent, group)
  }
  return children
}

function activityFor(row, session, now) {
  const runtime = session ?? row?.runtime
  if (runtime && !runtimeReady(runtime)) return { text: runtimeLabel(runtime), working: false }
  // A live session summary is fresher than the row's overlay.
  const state = session ? turnState({ runtime: session }) : turnState(row)
  if (runtime && state === 'needs_you') return { text: 'waiting', working: false }
  if (state === 'working') return { text: 'working', working: true }
  const at = row.updatedAt ?? row.createdAt ?? session?.updatedAt ?? session?.createdAt
  const age = sidebarAge(at, now)
  return { text: age ? `last active ${age}` : 'inactive', working: false }
}

// Returns a flat depth-first tree below `parent`. A node is closable only when
// Nano currently has a session id for it; durable history remains visible as
// disabled context, never as an invented close target.
export function closeDescendantRows(parent, rows, sessions, now = Date.now()) {
  const parentKeys = threadIdentityKeys(parent)
  if (parentKeys.length === 0) return []
  const children = descendantsByParent(decorateSubagentActivity(rows ?? []))
  const bySession = sessionIndex(sessions)
  const nodes = []
  const seen = new Set()

  const append = (parentKey, depth) => {
    for (const row of children.get(parentKey) ?? []) {
      const identity = threadIdentity(row)
      if (!identity || seen.has(identity)) continue
      seen.add(identity)
      const session = threadIdentityKeys(row).map((key) => bySession.get(key)).find(Boolean) ?? null
      const label = rowLabel({ name: row.name?.value, title: row.title, preview: row.preview, kind: row.kind }).text
      const activity = activityFor(row, session, now)
      nodes.push({
        key: identity,
        row,
        depth,
        label,
        dot: activityDot(row),
        activity: activity.text,
        working: activity.working,
        closable: Boolean(session?.id),
        sessionId: session?.id ?? '',
      })
      append(identity, depth + 1)
    }
  }
  for (const key of parentKeys) append(key, 0)
  return nodes
}

export function closeSelection(nodes, selected = []) {
  const eligible = (nodes ?? []).filter((node) => node.closable)
  const selectedSet = new Set(selected)
  const selectedNodes = eligible.filter((node) => selectedSet.has(node.sessionId))
  return {
    eligible,
    selectedNodes,
    selectedIds: selectedNodes.map((node) => node.sessionId),
    checked: eligible.length > 0 && selectedNodes.length === eligible.length,
    indeterminate: selectedNodes.length > 0 && selectedNodes.length < eligible.length,
    workingCount: selectedNodes.filter((node) => node.working).length,
  }
}

export function parentCloseLabel(provider, selectedCount) {
  const parent = provider === 'claude' ? 'End parent session' : 'Detach parent'
  if (!selectedCount) return parent
  return `${parent} and close ${selectedCount} agent${selectedCount === 1 ? '' : 's'}`
}
