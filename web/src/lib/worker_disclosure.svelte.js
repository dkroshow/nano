// Which worker groups are open. One store, because the compact rail and the
// expanded Explorer render one catalog (the operator, 2026-07-29: the Explorer is an
// expansion of the compact view, so the two surfaces must never disagree) —
// a group opened in the rail is already open when the table appears.
//
// Keyed by thread identity, not by row key or catalog id: the rail builds its
// key from the session/row it is standing for and the table keys on the durable
// catalog id, so only the provider-scoped native id means the same supervisor
// on both sides. Groups restore from a browser-local view and start closed when
// none exists. `*` is reserved for the explicit global expand action;
// `!<key>` is an individual closed exception after that action.

import { threadIdentity } from '../explorer_local.js'

export const WORKER_DISCLOSURE_STORAGE_KEY = 'nano:subagent-disclosure-view'
const WORKER_DISCLOSURE_STORAGE_VERSION = 1
const MAX_SAVED_GROUPS = 256
const MAX_SAVED_KEY_LENGTH = 512

function browserStorage() {
  try {
    return globalThis.localStorage ?? null
  } catch {
    return null
  }
}

function normalizedExpanded(value) {
  if (!Array.isArray(value)) return []
  const hasExpandAll = value.includes('*')
  const seen = new Set()
  const recent = []
  for (let index = value.length - 1; index >= 0; index -= 1) {
    const token = value[index]
    if (typeof token !== 'string' || token === '*' || !token || token.length > MAX_SAVED_KEY_LENGTH || seen.has(token)) continue
    seen.add(token)
    recent.push(token)
    if (recent.length >= MAX_SAVED_GROUPS - (hasExpandAll ? 1 : 0)) break
  }
  recent.reverse()
  return hasExpandAll ? ['*', ...recent] : recent
}

function readSavedDisclosure(storage = browserStorage()) {
  if (!storage?.getItem) return []
  try {
    const saved = JSON.parse(storage.getItem(WORKER_DISCLOSURE_STORAGE_KEY) ?? 'null')
    if (saved?.version !== WORKER_DISCLOSURE_STORAGE_VERSION) return []
    return normalizedExpanded(saved.expanded)
  } catch {
    return []
  }
}

const restoredExpanded = readSavedDisclosure()
export const workerDisclosure = $state({ expanded: restoredExpanded, historicalExpanded: [] })

function saveWorkerDisclosure(storage = browserStorage()) {
  if (!storage?.setItem) return false
  try {
    workerDisclosure.expanded = normalizedExpanded(workerDisclosure.expanded)
    storage.setItem(WORKER_DISCLOSURE_STORAGE_KEY, JSON.stringify({
      version: WORKER_DISCLOSURE_STORAGE_VERSION,
      expanded: workerDisclosure.expanded,
    }))
    return true
  } catch {
    return false
  }
}

export function restoreWorkerDisclosure(storage = browserStorage()) {
  workerDisclosure.expanded = readSavedDisclosure(storage)
  // Historical rows are lazy-loaded. Never restore a disclosure that would
  // claim history is visible before those rows have been fetched.
  workerDisclosure.historicalExpanded = []
  return workerDisclosure.expanded.length > 0
}

function collapsedToken(key) {
  return `!${key}`
}

export function workerGroupIsExpanded(row, expanded = workerDisclosure.expanded) {
  const key = threadIdentity(row)
  if (!key) return false
  return expanded.includes('*')
    ? !expanded.includes(collapsedToken(key))
    : expanded.includes(key)
}

export function toggleWorkerGroup(row) {
  const key = threadIdentity(row)
  if (!key) return
  if (workerDisclosure.expanded.includes('*')) {
    const token = collapsedToken(key)
    const closing = !workerDisclosure.expanded.includes(token)
    workerDisclosure.expanded = closing
      ? [...workerDisclosure.expanded, token]
      : workerDisclosure.expanded.filter((entry) => entry !== token)
    if (closing) workerDisclosure.historicalExpanded = workerDisclosure.historicalExpanded.filter((entry) => entry !== key)
    saveWorkerDisclosure()
    return
  }
  const closing = workerDisclosure.expanded.includes(key)
  workerDisclosure.expanded = closing
    ? workerDisclosure.expanded.filter((entry) => entry !== key)
    : [...workerDisclosure.expanded, key]
  if (closing) workerDisclosure.historicalExpanded = workerDisclosure.historicalExpanded.filter((entry) => entry !== key)
  saveWorkerDisclosure()
}

export function toggleHistoricalWorkerGroup(row) {
  const key = threadIdentity(row)
  if (!key) return
  workerDisclosure.historicalExpanded = workerDisclosure.historicalExpanded.includes(key)
    ? workerDisclosure.historicalExpanded.filter((entry) => entry !== key)
    : [...workerDisclosure.historicalExpanded, key]
}

export function setAllWorkerGroups(expanded) {
  workerDisclosure.expanded = expanded ? ['*'] : []
  workerDisclosure.historicalExpanded = []
  saveWorkerDisclosure()
}

export function workerGroupKeys(rows) {
  const keys = []
  for (let index = 0; index < (rows?.length ?? 0) - 1; index += 1) {
    if ((rows[index + 1].treeDepth ?? 0) <= (rows[index].treeDepth ?? 0)) continue
    const key = threadIdentity(rows[index])
    if (key) keys.push(key)
  }
  return keys
}

export function allWorkerGroupsExpanded(rows, expanded = workerDisclosure.expanded) {
  const keys = workerGroupKeys(rows)
  if (keys.length === 0) return false
  return keys.every((key) => expanded.includes('*')
    ? !expanded.includes(collapsedToken(key))
    : expanded.includes(key))
}

export function resetWorkerDisclosure(storage = browserStorage()) {
  workerDisclosure.expanded = []
  workerDisclosure.historicalExpanded = []
  try {
    storage?.removeItem?.(WORKER_DISCLOSURE_STORAGE_KEY)
  } catch {
    // Storage is an optional view preference; reset still succeeds in memory.
  }
}
