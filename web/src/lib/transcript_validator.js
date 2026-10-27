// The validator compares the transcript rows Nano intends to render with the
// rows the active pane actually painted. It deliberately compares identities,
// order, type, and empty/non-empty presentation only — never message text.

function clean(value) {
  return String(value ?? '').trim()
}

export function transcriptValidationKey(source, id) {
  const normalizedSource = clean(source)
  const normalizedID = clean(id)
  return normalizedSource && normalizedID ? `${normalizedSource}:${normalizedID}` : ''
}

function semanticTranscriptID(source, item = {}) {
  if (source === 'durable') return clean(item.id)
  if (source !== 'live') return ''
  if (clean(item.durableItemID)) return clean(item.durableItemID)
  const id = clean(item.id)
  return id.startsWith('durable:') ? id.slice('durable:'.length) : ''
}

function expectedRow(source, id, type, text = '', semanticID = '') {
  const key = transcriptValidationKey(source, id)
  if (!key) return null
  return {
    key,
    source: clean(source),
    id: clean(id),
    type: clean(type) || 'unknown',
    semanticID: clean(semanticID),
    expectsContent: clean(text).length > 0,
  }
}

export function expectedTranscriptRows({ durableItems = [], liveItems = [] } = {}) {
  return [
    ...durableItems.map((item) => expectedRow('durable', item?.id, item?.type, item?.text, semanticTranscriptID('durable', item))),
    ...liveItems.map((item) => expectedRow('live', item?.id, item?.type, item?.text, semanticTranscriptID('live', item))),
  ].filter(Boolean)
}

function elementPainted(element, root) {
  if (typeof getComputedStyle !== 'function') return true
  let current = element
  while (current?.nodeType === 1) {
    const style = getComputedStyle(current)
    if (style.display === 'none' || style.visibility === 'hidden' || style.visibility === 'collapse' || Number(style.opacity) === 0) {
      return false
    }
    if (current === root) break
    current = current.parentElement
  }
  return true
}

export function actualTranscriptRows(root) {
  if (!root?.querySelectorAll) return []
  return [...root.querySelectorAll('[data-transcript-row]')].map((row) => ({
    key: clean(row.dataset.transcriptKey),
    source: clean(row.dataset.transcriptSource),
    id: clean(row.dataset.transcriptId),
    semanticID: clean(row.dataset.transcriptSemanticId),
    type: clean(row.dataset.transcriptType) || 'unknown',
    hasContent: elementPainted(row, root) &&
      clean(row.querySelector('[data-transcript-content]')?.textContent).length > 0,
  })).filter((row) => row.key)
}

export function compareTranscriptRows(expected = [], actual = []) {
  const expectedByKey = new Map(expected.map((row) => [row.key, row]))
  const actualByKey = new Map()
  const duplicates = []
  const semanticRows = new Map()
  const semanticDuplicates = []
  for (const row of actual) {
    if (actualByKey.has(row.key)) duplicates.push(row)
    else actualByKey.set(row.key, row)
    if (row.semanticID) {
      if (semanticRows.has(row.semanticID)) semanticDuplicates.push(row)
      else semanticRows.set(row.semanticID, row)
    }
  }
  const missing = expected.filter((row) => !actualByKey.has(row.key))
  const extra = actual.filter((row) => !expectedByKey.has(row.key))
  const blank = expected.filter((row) => row.expectsContent && actualByKey.has(row.key) && !actualByKey.get(row.key).hasContent)
  const typeMismatch = expected.filter((row) => {
    const mounted = actualByKey.get(row.key)
    return mounted && mounted.type !== row.type
  })
  const expectedCommon = expected.filter((row) => actualByKey.has(row.key)).map((row) => row.key)
  const actualCommon = actual.filter((row) => expectedByKey.has(row.key)).map((row) => row.key)
  let orderMismatchCount = 0
  for (let index = 0; index < Math.max(expectedCommon.length, actualCommon.length); index += 1) {
    if (expectedCommon[index] !== actualCommon[index]) orderMismatchCount += 1
  }
  return {
    ok: missing.length === 0 && extra.length === 0 && duplicates.length === 0 && semanticDuplicates.length === 0 &&
      blank.length === 0 && typeMismatch.length === 0 && orderMismatchCount === 0,
    expected,
    actual,
    missing,
    extra,
    duplicates,
    semanticDuplicates,
    blank,
    typeMismatch,
    orderMismatchCount,
  }
}

export function compactTranscriptValidationID(id, length = 18) {
  const value = clean(id)
  if (value.length <= length) return value
  const side = Math.max(4, Math.floor((length - 1) / 2))
  return `${value.slice(0, side)}…${value.slice(-side)}`
}
