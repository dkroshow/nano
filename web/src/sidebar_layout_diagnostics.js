import { recordBrowserDiagnostic } from './browser_diagnostics.js'

const MAX_ROW_EVENTS = 24

let previousLayout = null
let layoutSequence = 0
let lastSignature = ''

function whole(value) {
  const number = Number(value)
  return Number.isFinite(number) ? Math.max(0, Math.round(number)) : 0
}

function orderingBand(row) {
  const working = Boolean(row?.working)
  if (row?.cls === 'nano') {
    if (working) return 'nano-working'
    return row?.kind === 'detached' ? 'nano-detached' : 'nano-idle'
  }
  return working ? 'external-working' : 'external-idle'
}

function layoutSnapshot(root, projectedRows) {
  if (!root?.querySelectorAll) return null
  const scroller = root.querySelector('.thread-list') ?? root
  const scrollerRect = scroller.getBoundingClientRect?.() ?? { top: 0, bottom: 0, height: 0 }
  const scrollTop = whole(scroller.scrollTop)
  const factsByKey = new Map((projectedRows ?? []).map((row) => [`sidebar:${row.key}`, row]))
  const rows = [...root.querySelectorAll('[data-sidebar-row]')].map((node, index) => {
    const rect = node.getBoundingClientRect?.() ?? { top: 0, bottom: 0, height: 0 }
    const key = String(node.dataset.sidebarKey ?? '')
    const facts = factsByKey.get(key) ?? null
    return {
      key,
      id: String(node.dataset.sidebarId ?? ''),
      hostedThreadId: String(facts?.hostedThreadId ?? ''),
      index,
      topPx: whole(Number(rect.top) - Number(scrollerRect.top) + scrollTop),
      heightPx: whole(rect.height),
      visible: Number(rect.bottom) > Number(scrollerRect.top) && Number(rect.top) < Number(scrollerRect.bottom),
      type: String(node.dataset.sidebarType ?? facts?.kind ?? 'thread'),
      status: String(node.dataset.sidebarStatus ?? facts?.indicator?.state ?? facts?.cls ?? 'idle'),
      band: orderingBand(facts),
      working: Boolean(facts?.working),
      starred: Boolean(facts?.starred),
    }
  }).filter((row) => row.key && row.id)
  return {
    scrollTop,
    scrollHeight: whole(scroller.scrollHeight),
    viewportHeight: whole(scroller.clientHeight ?? scrollerRect.height),
    rows,
  }
}

function movement(previous, current) {
  const previousByKey = new Map((previous?.rows ?? []).map((row) => [row.key, row]))
  const currentByKey = new Map(current.rows.map((row) => [row.key, row]))
  const changes = []
  let reorderCount = 0
  let maxMovementPx = 0
  for (const row of current.rows) {
    const before = previousByKey.get(row.key)
    if (!before) {
      changes.push({ op: 'added', row, before: null, movementPx: 0 })
      continue
    }
    const movementPx = Math.abs(row.topPx - before.topPx)
    if (row.index !== before.index) reorderCount += 1
    if (movementPx > maxMovementPx) maxMovementPx = movementPx
    if (row.index !== before.index || movementPx > 0) changes.push({ op: 'moved', row, before, movementPx })
  }
  for (const row of previous?.rows ?? []) {
    if (!currentByKey.has(row.key)) changes.push({ op: 'removed', row, before: row, movementPx: 0 })
  }
  return {
    changes,
    movementCount: changes.filter((change) => change.op === 'moved').length,
    reorderCount,
    addedCount: changes.filter((change) => change.op === 'added').length,
    removedCount: changes.filter((change) => change.op === 'removed').length,
    maxMovementPx,
  }
}

export function recordSidebarLayout(root, projectedRows = []) {
  const current = layoutSnapshot(root, projectedRows)
  if (!current) return null
  const signature = JSON.stringify(current)
  if (signature === lastSignature) return null
  lastSignature = signature
  const delta = movement(previousLayout, current)
  const sequence = ++layoutSequence
  recordBrowserDiagnostic('sidebar.layout', {
    layoutSequence: sequence,
    itemCount: current.rows.length,
    pendingCount: current.rows.filter((row) => row.type === 'pending').length,
    workingCount: current.rows.filter((row) => row.working).length,
    starredCount: current.rows.filter((row) => row.starred).length,
    detachedCount: current.rows.filter((row) => row.type === 'detached').length,
    movementCount: delta.movementCount,
    maxMovementPx: delta.maxMovementPx,
    reorderCount: delta.reorderCount,
    addedCount: delta.addedCount,
    removedCount: delta.removedCount,
    scrollTop: current.scrollTop,
    scrollHeight: current.scrollHeight,
    viewportHeight: current.viewportHeight,
  })
  if (previousLayout) {
    const ranked = [...delta.changes].sort((left, right) =>
      right.movementPx - left.movementPx || (left.row.index ?? 0) - (right.row.index ?? 0))
    for (const change of ranked.slice(0, MAX_ROW_EVENTS)) {
      const row = change.row
      const before = change.before
      recordBrowserDiagnostic('sidebar.row.move', {
        layoutSequence: sequence,
        threadId: row.id,
        hostedThreadId: row.hostedThreadId,
        op: change.op,
        code: row.band,
        status: row.status,
        rowIndex: row.index,
        previousRowIndex: before?.index,
        topPx: row.topPx,
        previousTopPx: before?.topPx,
        movementPx: change.movementPx,
        turnActive: row.working,
        visible: row.visible,
      })
    }
  }
  previousLayout = current
  return { sequence, ...delta, ...current }
}

export function __resetSidebarLayoutDiagnosticsForTest() {
  previousLayout = null
  layoutSequence = 0
  lastSignature = ''
}
