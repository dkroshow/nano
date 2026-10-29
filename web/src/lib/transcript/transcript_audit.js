import { messageBodyHtml } from './message_body.js'

export const TRANSCRIPT_AUDIT_SCOPES = ['messages']
export const MAX_TRANSCRIPT_AUDIT_ROWS = 200
export const MAX_TRANSCRIPT_AUDIT_DIFFERENCES = 20

const BLOCK_TAGS = new Set([
  'ADDRESS', 'ARTICLE', 'ASIDE', 'BLOCKQUOTE', 'DIV', 'DL', 'FIELDSET', 'FIGCAPTION',
  'FIGURE', 'FOOTER', 'FORM', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'HEADER', 'HR',
  'LI', 'MAIN', 'NAV', 'OL', 'P', 'PRE', 'SECTION', 'TABLE', 'TBODY', 'TD', 'TFOOT',
  'TH', 'THEAD', 'TR', 'UL',
])

function clean(value) {
  return String(value ?? '').trim()
}

export function normalizeTranscriptPresentation(value) {
  return String(value ?? '')
    .replace(/\r\n?/g, '\n')
    .replaceAll('\u00a0', ' ')
    .split('\n')
    .map((line) => line.replace(/[\t ]+$/g, ''))
    .join('\n')
    .replace(/^\n+|\n+$/g, '')
}

function painted(element, root) {
  if (!element || typeof getComputedStyle !== 'function') return true
  let current = element
  while (current?.nodeType === 1) {
    const style = getComputedStyle(current)
    if (style.display === 'none' || style.visibility === 'hidden' || style.visibility === 'collapse' || Number(style.opacity) === 0) {
      return false
    }
    if (current.tagName === 'DETAILS' && !current.open && !element.closest('summary')) return false
    if (current === root) break
    current = current.parentElement
  }
  return true
}

function presentationText(node) {
  if (!node) return ''
  const chunks = []
  const walk = (current) => {
    if (current.nodeType === 3) {
      const value = current.nodeValue ?? ''
      // Template indentation between block elements is not painted text. Keep
      // deliberate inline spaces and all whitespace inside PRE elements.
      const besideBlock = BLOCK_TAGS.has(current.previousSibling?.tagName) || BLOCK_TAGS.has(current.nextSibling?.tagName)
      if (/^\s+$/.test(value) && (value.includes('\n') || besideBlock) && current.parentElement?.tagName !== 'PRE') return
      chunks.push(value)
      return
    }
    if (current.nodeType !== 1) return
    if (current.tagName === 'BR') {
      chunks.push('\n')
      return
    }
    const block = BLOCK_TAGS.has(current.tagName)
    if (block && chunks.length && !chunks.at(-1).endsWith('\n')) chunks.push('\n')
    for (const child of current.childNodes) walk(child)
    if (block && chunks.length && !chunks.at(-1).endsWith('\n')) chunks.push('\n')
  }
  walk(node)
  return normalizeTranscriptPresentation(chunks.join(''))
}

function scopeForCategory(category) {
  if (category === 'messages') return 'messages'
  return ''
}

function normalizedScopes(scopes) {
  const requested = new Set(Array.isArray(scopes) ? scopes : ['messages'])
  return TRANSCRIPT_AUDIT_SCOPES.filter((scope) => requested.has(scope))
}

export function captureTranscriptAuditRows(root, scopes = ['messages'], boundaries = {}) {
  const allowed = new Set(normalizedScopes(scopes))
  const all = root?.querySelectorAll ? [...root.querySelectorAll('[data-transcript-row][data-transcript-category="messages"]')] : []
  const capped = all.length > MAX_TRANSCRIPT_AUDIT_ROWS
  const rows = all.slice(-MAX_TRANSCRIPT_AUDIT_ROWS).map((row, index) => {
    const content = row.querySelector('[data-transcript-content]')
    const collapsed = Boolean(content?.closest('details:not([open])'))
    const visible = Boolean(content) && painted(content, root)
    const category = clean(row.dataset.transcriptCategory) || 'unclassified'
    const scope = scopeForCategory(category)
    return {
      index,
      source: clean(row.dataset.transcriptSource),
      id: clean(row.dataset.transcriptId),
      semanticId: clean(row.dataset.transcriptSemanticId),
      projectedSeq: Math.max(0, Number(row.dataset.transcriptProjectedSeq) || 0),
      projectionKey: clean(row.dataset.transcriptProjectionKey),
      awaitingDurable: row.dataset.transcriptAwaitingDurable === 'true',
      category,
      role: clean(row.dataset.transcriptRole),
      contentState: row.dataset.transcriptTruncated === 'true'
        ? 'truncated'
        : collapsed
          ? 'collapsed'
          : visible
            ? 'visible'
            : content
              ? 'hidden'
              : 'not_mounted',
      presentation: visible && row.dataset.transcriptEmpty !== 'true' ? presentationText(content) : '',
      inScope: Boolean(scope && allowed.has(scope)),
    }
  })
  const scoped = rows.filter((row) => row.inScope)
  const projected = scoped.map((row) => row.projectedSeq).filter(Boolean)
  return {
    rows: scoped,
    capped,
    mountedRowCount: all.length,
    minProjectedSeq: projected.length ? Math.min(...projected) : 0,
    maxProjectedSeq: projected.length ? Math.max(...projected) : 0,
    startBoundary: ['start', 'earlier_unmounted'].includes(boundaries.startBoundary) ? boundaries.startBoundary : 'unknown',
    endBoundary: ['tail', 'later_unmounted'].includes(boundaries.endBoundary) ? boundaries.endBoundary : 'unknown',
    unmountedRows: (boundaries.unmountedRows ?? []).map((row) => ({
      semanticId: clean(row?.semanticId),
      projectedSeq: Math.max(0, Number(row?.projectedSeq) || 0),
    })).filter((row) => row.semanticId && row.projectedSeq > 0),
    unmountedRanges: (boundaries.unmountedRanges ?? []).map((range) => ({
      afterSeq: Math.max(0, Number(range?.afterSeq) || 0),
      beforeSeq: Math.max(0, Number(range?.beforeSeq) || 0),
      evidence: clean(range?.evidence),
    })).filter((range) => range.afterSeq > 0 && range.beforeSeq > range.afterSeq + 1 && range.evidence),
  }
}

function referencePresentation(row) {
  if (row.category !== 'messages' || row.role !== 'assistant') {
    return normalizeTranscriptPresentation(row.text)
  }
  const surface = document.createElement('div')
  surface.innerHTML = messageBodyHtml('agent', row.text || '')
  return presentationText(surface)
}

async function digest(value) {
  const bytes = new TextEncoder().encode(value)
  if (globalThis.crypto?.subtle) {
    const result = await globalThis.crypto.subtle.digest('SHA-256', bytes)
    return [...new Uint8Array(result)].map((part) => part.toString(16).padStart(2, '0')).join('')
  }
  // Test and stripped-browser fallback. This is only an equality token; the
  // server never treats it as source evidence or persists message bodies.
  let hash = 2166136261
  for (const byte of bytes) hash = Math.imul(hash ^ byte, 16777619)
  return `fnv1a-${(hash >>> 0).toString(16).padStart(8, '0')}`
}

function difference(kind, id, fields = {}) {
  return { kind, id: clean(id), ...fields }
}

function addBounded(list, item, state) {
  if (list.length < MAX_TRANSCRIPT_AUDIT_DIFFERENCES) list.push(item)
  else state.truncated = true
}

export async function compareTranscriptAuditRows(reference, capture, options = {}) {
  const scopes = normalizedScopes(options.scopes)
  const scopeSet = new Set(scopes)
  const refs = (reference?.rows ?? []).filter((row) => scopeSet.has(scopeForCategory(row.category)))
  const browser = (capture?.rows ?? []).filter((row) => scopeSet.has(scopeForCategory(row.category)))
  const differences = []
  const bounded = { truncated: false }
  const differenceCounts = { missing: 0, extra: 0, duplicate: 0, reordered: 0, changed: 0 }
  const reasons = []
  const exclusionCounts = new Map()
  const exclude = (scope, reason, count = 1) => {
    const key = `${scope}\u0000${reason}`
    const existing = exclusionCounts.get(key) ?? { scope, reason, count: 0 }
    existing.count += count
    exclusionCounts.set(key, existing)
  }
  const recordDifference = (item) => {
    const bucket = item.kind === 'structured_changed' ? 'changed' : item.kind
    differenceCounts[bucket] += 1
    addBounded(differences, item, bounded)
  }

  if (capture?.capped) reasons.push('browser_row_cap_reached')
  if (reference?.capReached) reasons.push(reference?.capReason === 'bytes' ? 'reference_byte_cap_reached' : 'reference_row_cap_reached')

  const browserByID = new Map()
  for (const row of browser) {
    if (!row.semanticId) continue
    const group = browserByID.get(row.semanticId) ?? []
    group.push(row)
    browserByID.set(row.semanticId, group)
  }
  for (const [id, group] of browserByID) {
    if (group.length > 1) recordDifference(difference('duplicate', id, { browserCount: group.length }))
  }

  const refIndex = new Map(refs.map((row, index) => [row.id, index]))
  const common = browser.filter((row) => row.semanticId && refIndex.has(row.semanticId) && browserByID.get(row.semanticId)?.length === 1)
  let windowRefs = refs
  let anchorsReliable = false
  const startBoundary = capture?.startBoundary ?? 'unknown'
  const endBoundary = capture?.endBoundary ?? 'unknown'
  const startKnown = startBoundary === 'start' || startBoundary === 'earlier_unmounted'
  const endKnown = endBoundary === 'tail' || endBoundary === 'later_unmounted'
  if (startBoundary === 'earlier_unmounted' && capture?.minProjectedSeq > 0) {
    windowRefs = windowRefs.filter((row) => row.projectedSeq >= capture.minProjectedSeq)
  }
  if (endBoundary === 'later_unmounted' && capture?.maxProjectedSeq > 0) {
    windowRefs = windowRefs.filter((row) => row.projectedSeq <= capture.maxProjectedSeq)
  }
  if (!startKnown) reasons.push('start_boundary_unknown')
  if (!endKnown) reasons.push('end_boundary_unknown')
  if (startKnown && endKnown) {
    anchorsReliable = true
  } else if (common.length >= 2) {
    const positions = common.map((row) => refIndex.get(row.semanticId))
    const first = Math.min(...positions)
    const last = Math.max(...positions)
    windowRefs = refs.slice(first, last + 1)
    anchorsReliable = true
  } else if (common.length === 1 && refs.length === 1 && browser.length === 1) {
    windowRefs = refs
    anchorsReliable = true
  }
  if (!anchorsReliable) reasons.push('no_reliable_anchors')

  const windowIDs = new Set(windowRefs.map((row) => row.id))
  const uniqueBrowser = browser.filter((row) => row.semanticId && browserByID.get(row.semanticId)?.length === 1)
  const explicitlyUnmounted = new Set((capture?.unmountedRows ?? []).map((row) => `${row.semanticId}\u0000${row.projectedSeq}`))
  const unmountedRanges = capture?.unmountedRanges ?? []
  const hasUnmountedProof = (row) => explicitlyUnmounted.has(`${row.id}\u0000${row.projectedSeq}`) ||
    unmountedRanges.some((range) => row.projectedSeq > range.afterSeq && row.projectedSeq < range.beforeSeq)

  // A durable write can precede the live row learning its saved id. Only a
  // contiguous, explicitly marked live suffix may account for an equally
  // bounded saved suffix, and only when their visible presentation agrees.
  // This never assigns an identity; both sides remain excluded/inconclusive.
  const ambiguousTail = []
  for (let index = browser.length - 1; index >= 0; index -= 1) {
    const row = browser[index]
    if (row.semanticId || row.source !== 'live' || !row.awaitingDurable) break
    ambiguousTail.unshift(row)
  }
  const lastStableReferenceIndex = uniqueBrowser.reduce((last, row) => {
    const index = refIndex.get(row.semanticId)
    return index == null ? last : Math.max(last, index)
  }, -1)
  const missingTail = windowRefs.slice(lastStableReferenceIndex + 1).filter((row) => !browserByID.has(row.id))
  const tailStartsAfterStable = missingTail.length > 0 &&
    windowRefs.slice(lastStableReferenceIndex + 1).every((row) => !browserByID.has(row.id))
  const unconfirmedMissingIDs = new Set()
  if (tailStartsAfterStable && ambiguousTail.length) {
    const pairCount = Math.min(missingTail.length, ambiguousTail.length)
    const savedCandidates = missingTail.slice(-pairCount)
    const liveCandidates = ambiguousTail.slice(-pairCount)
    for (let index = 0; index < pairCount; index += 1) {
      const expected = savedCandidates[index]
      const actual = liveCandidates[index]
      if (expected.contentTruncated || actual.contentState !== 'visible') continue
      if (scopeForCategory(expected.category) !== scopeForCategory(actual.category)) continue
      if (clean(expected.role) && clean(actual.role) && clean(expected.role) !== clean(actual.role)) continue
      if (referencePresentation(expected) !== normalizeTranscriptPresentation(actual.presentation)) continue
      unconfirmedMissingIDs.add(expected.id)
    }
  }
  if (anchorsReliable) {
    for (const row of windowRefs) {
      if (browserByID.has(row.id)) continue
      const scope = scopeForCategory(row.category)
      if (hasUnmountedProof(row)) exclude(scope, 'unmounted_gap')
      else if (unconfirmedMissingIDs.has(row.id)) exclude(scope, 'unconfirmed_tail')
      else recordDifference(difference('missing', row.id, { expectedIndex: refIndex.get(row.id) }))
    }
  }
  for (const row of browser) {
    if (!row.semanticId) {
      exclude(scopeForCategory(row.category), 'ambiguous_identity')
      continue
    }
    if (refIndex.has(row.semanticId)) continue
    if (row.projectedSeq > (reference?.snapshotHeadSeq ?? 0)) {
      exclude(scopeForCategory(row.category), 'after_pinned_snapshot')
    } else if (reference?.capReached && (reference?.startSeq ?? 0) > 0 &&
        row.projectedSeq > 0 && row.projectedSeq < reference.startSeq &&
        (row.source === 'durable' && row.projectionKey && row.projectionKey === clean(reference?.projectionKey) ||
          row.source === 'live' && (!row.projectionKey || row.projectionKey === clean(reference?.projectionKey)))) {
      exclude(scopeForCategory(row.category), 'before_reference_cap')
    } else if (row.source === 'durable' || (row.projectedSeq > 0 && row.projectedSeq <= (reference?.snapshotHeadSeq ?? 0))) {
      recordDifference(difference('extra', row.semanticId, { browserIndex: row.index }))
    } else {
      exclude(scopeForCategory(row.category), 'live_tail_not_in_snapshot')
    }
  }

  if (anchorsReliable) {
    const expectedOrder = windowRefs.map((row) => row.id).filter((id) => browserByID.has(id))
    const actualOrder = uniqueBrowser.map((row) => row.semanticId).filter((id) => windowIDs.has(id))
    const orderLength = Math.max(expectedOrder.length, actualOrder.length)
    for (let index = 0; index < orderLength; index += 1) {
      if (expectedOrder[index] !== actualOrder[index]) {
        recordDifference(difference('reordered', actualOrder[index] || expectedOrder[index], {
          expectedIndex: expectedOrder.indexOf(actualOrder[index]), browserIndex: index,
        }))
      }
    }
  }

  let checkedContentCount = 0
  for (const row of uniqueBrowser) {
    const expected = refs[refIndex.get(row.semanticId)]
    if (!expected) continue
    if (clean(expected.role) && clean(row.role) && expected.role !== row.role) {
      recordDifference(difference('structured_changed', row.semanticId, { field: 'role' }))
    }
    if (expected.contentTruncated) {
      exclude(scopeForCategory(row.category), 'reference_content_truncated')
      continue
    }
    if (row.contentState !== 'visible') {
      exclude(scopeForCategory(row.category), `browser_content_${row.contentState}`)
      continue
    }
    checkedContentCount += 1
    const expectedText = referencePresentation(expected)
    const actualText = normalizeTranscriptPresentation(row.presentation)
    if (expectedText !== actualText) {
      recordDifference(difference('changed', row.semanticId, {
        expectedHash: await digest(expectedText), browserHash: await digest(actualText),
        expectedLength: expectedText.length, browserLength: actualText.length,
      }))
    }
  }

  const exclusions = [...exclusionCounts.values()]
  const mismatch = Object.values(differenceCounts).some((count) => count > 0)
  const incomplete = reasons.length > 0 || exclusions.length > 0
  return {
    status: mismatch ? 'mismatch' : incomplete ? 'inconclusive' : 'match',
    complete: !incomplete,
    scopes,
    counts: {
      reference: windowRefs.length,
      browser: browser.length,
      contentChecked: checkedContentCount,
      ...differenceCounts,
      notChecked: exclusions.reduce((total, item) => total + item.count, 0),
    },
    differences,
    differencesTruncated: bounded.truncated,
    exclusions,
    reasons,
    range: {
      startSeq: windowRefs[0]?.projectedSeq ?? 0,
      endSeq: windowRefs.at(-1)?.projectedSeq ?? 0,
      snapshotHeadSeq: reference?.snapshotHeadSeq ?? 0,
      partial: Boolean(reference?.hasMoreBefore || reference?.snapshotHeadSeq > (windowRefs.at(-1)?.projectedSeq ?? 0)),
    },
  }
}
