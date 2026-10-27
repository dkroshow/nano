import { rowRenderModel } from './explorer_format.js'

function clean(value) {
  return String(value ?? '').trim()
}

function boolValue(value) {
  return value === true || value === 'true'
}

function depthValue(value) {
  const number = Number(value)
  return Number.isFinite(number) && number >= 0 ? Math.trunc(number) : 0
}

function parentIdentity(row) {
  return clean(row?.assignment?.relatedHostedThreadId ?? row?.invokerNativeThreadId ??
    row?.invokerHostedThreadId ?? row?.parentNativeId)
}

function expectedRow(id, type, status, active, group, depth) {
  const normalizedID = clean(id)
  if (!normalizedID) return null
  return {
    key: `sidebar:${normalizedID}`,
    source: 'sidebar',
    id: normalizedID,
    type: clean(type) || 'thread',
    expectsContent: true,
    active: Boolean(active),
    status: clean(status) || 'idle',
    group: clean(group),
    depth: depthValue(depth),
  }
}

export function expectedCompactSidebarRows(rows = [], isActive = () => false) {
  return rows.map((row) => expectedRow(
    row?.key,
    row?.kind,
    row?.indicator?.state ?? row?.cls,
    isActive(row),
    parentIdentity(row),
    row?.treeDepth,
  )).filter(Boolean)
}

export function expectedExplorerSidebarRows(rows = [], isActive = () => false) {
  return rows.map((row) => {
    const model = rowRenderModel(row)
    return expectedRow(
      row?.id,
      row?.transient ? 'transient' : 'catalog',
      model?.dot?.state ?? 'idle',
      isActive(row),
      parentIdentity(row),
      row?.treeDepth,
    )
  }).filter(Boolean)
}

export function actualSidebarRows(root) {
  if (!root?.querySelectorAll) return []
  return [...root.querySelectorAll('[data-sidebar-row]')].map((row) => ({
    key: clean(row.dataset.sidebarKey),
    source: 'sidebar',
    id: clean(row.dataset.sidebarId),
    type: clean(row.dataset.sidebarType) || 'thread',
    hasContent: clean(row.querySelector('[data-sidebar-content]')?.textContent).length > 0,
    active: boolValue(row.dataset.sidebarActive),
    status: clean(row.dataset.sidebarStatus) || 'idle',
    group: clean(row.dataset.sidebarGroup),
    depth: depthValue(row.dataset.sidebarDepth),
  })).filter((row) => row.key)
}

export function compareSidebarRows(expected = [], actual = []) {
  const expectedByKey = new Map(expected.map((row) => [row.key, row]))
  const actualByKey = new Map()
  const duplicates = []
  for (const row of actual) {
    if (actualByKey.has(row.key)) duplicates.push(row)
    else actualByKey.set(row.key, row)
  }
  const missing = expected.filter((row) => !actualByKey.has(row.key))
  const extra = actual.filter((row) => !expectedByKey.has(row.key))
  const blank = expected.filter((row) => actualByKey.has(row.key) && !actualByKey.get(row.key).hasContent)
  const stateMismatch = expected.filter((row) => {
    const mounted = actualByKey.get(row.key)
    return mounted && (mounted.type !== row.type || mounted.active !== row.active ||
      mounted.status !== row.status || mounted.group !== row.group || mounted.depth !== row.depth)
  })
  const expectedCommon = expected.filter((row) => actualByKey.has(row.key)).map((row) => row.key)
  const actualCommon = actual.filter((row) => expectedByKey.has(row.key)).map((row) => row.key)
  let orderMismatchCount = 0
  for (let index = 0; index < Math.max(expectedCommon.length, actualCommon.length); index += 1) {
    if (expectedCommon[index] !== actualCommon[index]) orderMismatchCount += 1
  }
  return {
    ok: missing.length === 0 && extra.length === 0 && duplicates.length === 0 &&
      blank.length === 0 && stateMismatch.length === 0 && orderMismatchCount === 0,
    expected, actual, missing, extra, duplicates, blank, stateMismatch, orderMismatchCount,
  }
}
