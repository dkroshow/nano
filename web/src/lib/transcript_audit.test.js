// @vitest-environment jsdom

import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { compareTranscriptRows } from './transcript_validator.js'
import { messageBodyHtml } from './message_body.js'
import {
  captureTranscriptAuditRows,
  compareTranscriptAuditRows,
  normalizeTranscriptPresentation,
} from './transcript_audit.js'

const duplicateTailFixture = JSON.parse(readFileSync(
  resolve(process.cwd(), '../testdata/transcript_audit/duplicate-tail.json'),
  'utf8',
))
const differentProjectionFixture = JSON.parse(readFileSync(
  resolve(process.cwd(), '../testdata/transcript_audit/different-projection-tail.json'),
  'utf8',
))

function mountedRow({ id, rawId = id, seq, category = 'messages', role = 'assistant', text, type = 'agent_message', source = 'durable', projectionKey = '', extra = '' }) {
  return `<div data-transcript-row data-transcript-source="${source}" data-transcript-id="${rawId}" data-transcript-semantic-id="${id}" data-transcript-projected-seq="${seq}" data-transcript-projection-key="${projectionKey}" data-transcript-type="${type}" data-transcript-category="${category}" data-transcript-role="${role}" ${extra}><div data-transcript-content>${text}</div></div>`
}

function referenceRow(id, seq, text, overrides = {}) {
  return { id, projectedSeq: seq, type: 'agent_message', category: 'messages', role: 'assistant', text, contentTruncated: false, ...overrides }
}

async function compare(referenceRows, html, scopes = ['messages'], options = {}) {
  document.body.innerHTML = `<div id="root">${html}</div>`
  const capture = captureTranscriptAuditRows(document.querySelector('#root'), scopes, options.boundaries ?? {
    startBoundary: 'start', endBoundary: 'tail',
  })
  return compareTranscriptAuditRows({ rows: referenceRows, snapshotHeadSeq: referenceRows.at(-1)?.projectedSeq ?? 0 }, capture, {
    scopes,
    viewMode: 'all',
    ...options,
  })
}

describe('on-demand transcript audit', () => {
  it('does not let out-of-scope rows consume the message capture cap', async () => {
    const root = document.createElement('div')
    root.innerHTML = mountedRow({ id: 'message', seq: 1, text: 'answer' }) +
      Array.from({ length: 250 }, (_, index) => mountedRow({
        id: `tool-${index}`, seq: index + 2, category: 'toolCall', text: 'output',
      })).join('')
    const capture = captureTranscriptAuditRows(root, ['messages'], { startBoundary: 'start', endBoundary: 'tail' })
    expect(capture.capped).toBe(false)
    expect(capture.rows.map((row) => row.semanticId)).toEqual(['message'])
  })

  it('detects the wrong message role even when the text and identity agree', async () => {
    const report = await compare(
      [referenceRow('message', 1, 'answer')],
      mountedRow({ id: 'message', seq: 1, text: 'answer', role: 'user' }),
    )
    expect(report.status).toBe('mismatch')
    expect(report.differences).toContainEqual(expect.objectContaining({ kind: 'structured_changed', field: 'role' }))
  })

  it('finds a missing middle message even when the old intended-vs-mounted validator says match', async () => {
    const html = mountedRow({ id: 'a', seq: 1, text: 'one' }) + mountedRow({ id: 'c', seq: 3, text: 'three' })
    document.body.innerHTML = `<div id="root">${html}</div>`
    const root = document.querySelector('#root')
    const oldActual = [...root.querySelectorAll('[data-transcript-row]')].map((row) => ({
      key: `durable:${row.dataset.transcriptId}`, source: 'durable', id: row.dataset.transcriptId,
      semanticID: row.dataset.transcriptSemanticId, type: row.dataset.transcriptType, hasContent: true,
    }))
    expect(compareTranscriptRows(oldActual.map((row) => ({ ...row, expectsContent: true })), oldActual).ok).toBe(true)

    const report = await compare([
      referenceRow('a', 1, 'one'), referenceRow('b', 2, 'two'), referenceRow('c', 3, 'three'),
    ], html)
    expect(report.status).toBe('mismatch')
    expect(report.differences).toContainEqual(expect.objectContaining({ kind: 'missing', id: 'b' }))
  })

  it.each([
    ['wrong order', mountedRow({ id: 'a', seq: 1, text: 'one' }) + mountedRow({ id: 'c', seq: 3, text: 'three' }) + mountedRow({ id: 'b', seq: 2, text: 'two' }), 'reordered'],
    ['same-id truncation', mountedRow({ id: 'a', seq: 1, text: 'short' }), 'changed'],
  ])('detects %s', async (_name, html, kind) => {
    const report = await compare([
      referenceRow('a', 1, 'a complete answer'), referenceRow('b', 2, 'two'), referenceRow('c', 3, 'three'),
    ], html)
    expect(report.status).toBe('mismatch')
    expect(report.differences.some((item) => item.kind === kind)).toBe(true)
  })

  it('detects saved/live copies with distinct render ids but one semantic id', async () => {
    const html = mountedRow({ id: 'a', rawId: 'saved-a', seq: 1, text: 'same' }) +
      mountedRow({ id: 'a', rawId: 'live-a', seq: 1, text: 'same' })
    const report = await compare([referenceRow('a', 1, 'same')], html)
    expect(report.status).toBe('mismatch')
    expect(report.differences).toContainEqual(expect.objectContaining({ kind: 'duplicate', id: 'a', browserCount: 2 }))
  })

  it('reproduces the sanitized duplicate saved/live incident while keeping its uncertain tail explicit', async () => {
    const report = await compareTranscriptAuditRows(
      { rows: duplicateTailFixture.reference, snapshotHeadSeq: 2 },
      { rows: duplicateTailFixture.browser, minProjectedSeq: 1, maxProjectedSeq: 2, capped: false },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(report.status).toBe('mismatch')
    expect(report.complete).toBe(false)
    expect(report.differences.filter((item) => item.kind === 'duplicate').map((item) => item.id)).toEqual([
      'saved-user-1', 'saved-agent-1',
    ])
    expect(report.exclusions).toContainEqual(expect.objectContaining({ reason: 'ambiguous_identity' }))
  })

  it('reproduces a stale rendered projection whose row ids all differ from the pinned projection', async () => {
    const report = await compareTranscriptAuditRows(
      { rows: differentProjectionFixture.reference, snapshotHeadSeq: differentProjectionFixture.snapshotHeadSeq },
      {
        rows: differentProjectionFixture.browser,
        minProjectedSeq: 1,
        maxProjectedSeq: 3,
        startBoundary: 'start',
        endBoundary: 'tail',
        capped: false,
      },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(report.status).toBe('mismatch')
    expect(report.counts).toEqual(expect.objectContaining({ missing: 2, extra: 2 }))
    expect(report.exclusions).toContainEqual({ scope: 'messages', reason: 'after_pinned_snapshot', count: 1 })
  })

  it('accepts repeated legitimate same-text prompts when stable identities differ', async () => {
    const refs = [
      referenceRow('prompt-1', 1, 'repeat', { type: 'user_message', role: 'user' }),
      referenceRow('prompt-2', 2, 'repeat', { type: 'user_message', role: 'user' }),
    ]
    const html = mountedRow({ id: 'prompt-1', seq: 1, text: 'repeat', type: 'user_message', role: 'user' }) +
      mountedRow({ id: 'prompt-2', seq: 2, text: 'repeat', type: 'user_message', role: 'user' })
    const report = await compare(refs, html)
    expect(report.status).toBe('match')
    expect(report.counts.duplicate).toBe(0)
  })

  it('matches only the mounted sequence window and labels it partial', async () => {
    const refs = [
      referenceRow('a', 1, 'one'), referenceRow('b', 2, 'two'), referenceRow('c', 3, 'three'),
      referenceRow('d', 4, 'four'), referenceRow('e', 5, 'five'),
    ]
    const html = mountedRow({ id: 'b', seq: 2, text: 'two' }) +
      mountedRow({ id: 'c', seq: 3, text: 'three' }) +
      mountedRow({ id: 'd', seq: 4, text: 'four' })
    const report = await compare(refs, html, ['messages'], {
      boundaries: { startBoundary: 'earlier_unmounted', endBoundary: 'later_unmounted' },
    })
    expect(report.status).toBe('match')
    expect(report.counts.reference).toBe(3)
    expect(report.range).toEqual(expect.objectContaining({ startSeq: 2, endSeq: 4, partial: true }))
  })

  it.each([
    ['newest', mountedRow({ id: 'a', seq: 1, text: 'one' }) + mountedRow({ id: 'b', seq: 2, text: 'two' }), 'c'],
    ['oldest', mountedRow({ id: 'b', seq: 2, text: 'two' }) + mountedRow({ id: 'c', seq: 3, text: 'three' }), 'a'],
  ])('detects a dropped %s row when the component declares real transcript boundaries', async (_edge, html, missingID) => {
    const report = await compare([
      referenceRow('a', 1, 'one'), referenceRow('b', 2, 'two'), referenceRow('c', 3, 'three'),
    ], html)
    expect(report.status).toBe('mismatch')
    expect(report.differences).toContainEqual(expect.objectContaining({ kind: 'missing', id: missingID }))
  })

  it('does not claim a match when the component cannot establish its window boundaries', async () => {
    const report = await compare([referenceRow('a', 1, 'one')], mountedRow({ id: 'a', seq: 1, text: 'one' }), ['messages'], {
      boundaries: { startBoundary: 'unknown', endBoundary: 'unknown' },
    })
    expect(report.status).toBe('inconclusive')
    expect(report.reasons).toEqual(expect.arrayContaining(['start_boundary_unknown', 'end_boundary_unknown']))
  })

  it('excludes durable rows created after the pinned snapshot instead of calling them extra', async () => {
    const report = await compareTranscriptAuditRows(
      { rows: [referenceRow('a', 1, 'one'), referenceRow('b', 2, 'two')], snapshotHeadSeq: 2 },
      { rows: [
        { source: 'durable', semanticId: 'a', projectedSeq: 1, category: 'messages', contentState: 'visible', presentation: 'one', index: 0 },
        { source: 'durable', semanticId: 'b', projectedSeq: 2, category: 'messages', contentState: 'visible', presentation: 'two', index: 1 },
        { source: 'durable', semanticId: 'after', projectedSeq: 3, category: 'messages', contentState: 'visible', presentation: 'after pin', index: 2 },
      ], minProjectedSeq: 1, maxProjectedSeq: 3, startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(report.status).toBe('inconclusive')
    expect(report.counts.extra).toBe(0)
    expect(report.exclusions).toContainEqual({ scope: 'messages', reason: 'after_pinned_snapshot', count: 1 })
  })

  it('counts every difference even when the reported detail list is capped', async () => {
    const refs = Array.from({ length: 40 }, (_, index) => referenceRow(`r${index}`, index + 1, `text ${index}`))
    const html = mountedRow({ id: 'r0', seq: 1, text: 'text 0' }) + mountedRow({ id: 'r39', seq: 40, text: 'text 39' })
    const report = await compare(refs, html)
    expect(report.counts.missing).toBe(38)
    expect(report.differences).toHaveLength(20)
    expect(report.differencesTruncated).toBe(true)
  })

  it('aggregates exclusions by scope and reason before returning them', async () => {
    const rows = Array.from({ length: 200 }, (_, index) => ({
      source: 'live', id: `raw-${index}`, semanticId: '', projectedSeq: 0, category: 'messages',
      contentState: 'visible', presentation: 'ambiguous', index,
    }))
    const report = await compareTranscriptAuditRows(
      { rows: [], snapshotHeadSeq: 0 },
      { rows, startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'clean' },
    )
    expect(report.exclusions).toEqual([
      { scope: 'messages', reason: 'ambiguous_identity', count: 200 },
    ])
    expect(report.counts.notChecked).toBe(200)
  })

  it('excludes only rows named by actual unmounted-window evidence', async () => {
    const refs = ['a', 'b', 'c', 'd', 'e'].map((id, index) => referenceRow(id, index + 1, id))
    const browser = [
      { source: 'durable', semanticId: 'a', projectedSeq: 1, category: 'messages', role: 'assistant', contentState: 'visible', presentation: 'a', index: 0 },
      { source: 'live', semanticId: 'e', projectedSeq: 5, category: 'messages', role: 'assistant', contentState: 'visible', presentation: 'e', index: 1 },
    ]
    const healthy = await compareTranscriptAuditRows(
      { rows: refs, snapshotHeadSeq: 5 },
      { rows: browser, startBoundary: 'start', endBoundary: 'tail', unmountedRows: [
        { semanticId: 'b', projectedSeq: 2 }, { semanticId: 'c', projectedSeq: 3 }, { semanticId: 'd', projectedSeq: 4 },
      ] },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(healthy.status).toBe('inconclusive')
    expect(healthy.counts.missing).toBe(0)
    expect(healthy.exclusions).toContainEqual({ scope: 'messages', reason: 'unmounted_gap', count: 3 })

    const droppedMiddle = await compareTranscriptAuditRows(
      { rows: refs, snapshotHeadSeq: 5 },
      { rows: browser, startBoundary: 'start', endBoundary: 'tail', unmountedRows: [
        { semanticId: 'b', projectedSeq: 2 }, { semanticId: 'd', projectedSeq: 4 },
      ] },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(droppedMiddle.status).toBe('mismatch')
    expect(droppedMiddle.differences).toContainEqual(expect.objectContaining({ kind: 'missing', id: 'c' }))
  })

  it('uses same-projection sequence evidence for rows older than a capped reference', async () => {
    const refs = Array.from({ length: 100 }, (_, index) => referenceRow(`r${index + 51}`, index + 51, `text ${index + 51}`))
    const browser = Array.from({ length: 150 }, (_, index) => ({
      source: 'durable', semanticId: `r${index + 1}`, projectedSeq: index + 1, projectionKey: 'projection-cap',
      category: 'messages', role: 'assistant', contentState: 'visible', presentation: `text ${index + 1}`, index,
    }))
    const report = await compareTranscriptAuditRows(
      { rows: refs, projectionKey: 'projection-cap', snapshotHeadSeq: 150, startSeq: 51, capReached: true },
      { rows: browser, startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(report.status).toBe('inconclusive')
    expect(report.counts.extra).toBe(0)
    expect(report.exclusions).toContainEqual({ scope: 'messages', reason: 'before_reference_cap', count: 50 })

    browser[0] = { ...browser[0], projectionKey: 'projection-old' }
    const differentGeneration = await compareTranscriptAuditRows(
      { rows: refs, projectionKey: 'projection-cap', snapshotHeadSeq: 150, startSeq: 51, capReached: true },
      { rows: browser, startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(differentGeneration.status).toBe('mismatch')
    expect(differentGeneration.differences).toContainEqual(expect.objectContaining({ kind: 'extra', id: 'r1' }))

    const confirmedLive = browser.map((row) => ({ ...row, source: 'live', projectionKey: '' }))
    const liveReport = await compareTranscriptAuditRows(
      { rows: refs, projectionKey: 'projection-cap', snapshotHeadSeq: 150, startSeq: 51, capReached: true, capReason: 'rows' },
      { rows: confirmedLive, startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(liveReport.status).toBe('inconclusive')
    expect(liveReport.counts.extra).toBe(0)
    expect(liveReport.exclusions).toContainEqual({ scope: 'messages', reason: 'before_reference_cap', count: 50 })

    confirmedLive[0] = { ...confirmedLive[0], projectionKey: 'known-other-projection' }
    const knownContradiction = await compareTranscriptAuditRows(
      { rows: refs, projectionKey: 'projection-cap', snapshotHeadSeq: 150, startSeq: 51, capReached: true, capReason: 'bytes' },
      { rows: confirmedLive, startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(knownContradiction.status).toBe('mismatch')
    expect(knownContradiction.reasons).toContain('reference_byte_cap_reached')
    expect(knownContradiction.differences).toContainEqual(expect.objectContaining({ kind: 'extra', id: 'r1' }))

    const withinReferenceDefect = confirmedLive.map((row) => ({ ...row, projectionKey: '' }))
    withinReferenceDefect[100] = { ...withinReferenceDefect[100], semanticId: 'foreign-r101' }
    const withinReport = await compareTranscriptAuditRows(
      { rows: refs, projectionKey: 'projection-cap', snapshotHeadSeq: 150, startSeq: 51, capReached: true, capReason: 'rows' },
      { rows: withinReferenceDefect, startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(withinReport.status).toBe('mismatch')
    expect(withinReport.differences).toEqual(expect.arrayContaining([
      expect.objectContaining({ kind: 'missing', id: 'r101' }),
      expect.objectContaining({ kind: 'extra', id: 'foreign-r101' }),
    ]))
  })

  it('bounds unconfirmed-tail uncertainty without hiding stable omissions', async () => {
    const refs = [referenceRow('a', 1, 'one'), referenceRow('b', 2, 'two')]
    const stable = { source: 'live', semanticId: 'a', projectedSeq: 1, category: 'messages', role: 'assistant', contentState: 'visible', presentation: 'one', index: 0 }
    const awaiting = { source: 'live', semanticId: '', projectedSeq: 0, liveSeq: 2, awaitingDurable: true, category: 'messages', role: 'assistant', contentState: 'visible', presentation: 'two', index: 1 }
    const uncertain = await compareTranscriptAuditRows(
      { rows: refs, snapshotHeadSeq: 2 },
      { rows: [stable, awaiting], startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(uncertain.status).toBe('inconclusive')
    expect(uncertain.counts.missing).toBe(0)
    expect(uncertain.exclusions).toEqual(expect.arrayContaining([
      { scope: 'messages', reason: 'unconfirmed_tail', count: 1 },
      { scope: 'messages', reason: 'ambiguous_identity', count: 1 },
    ]))

    const droppedMiddle = await compareTranscriptAuditRows(
      { rows: [referenceRow('a', 1, 'one'), referenceRow('b', 2, 'two'), referenceRow('c', 3, 'three')], snapshotHeadSeq: 3 },
      { rows: [stable, { ...stable, semanticId: 'c', projectedSeq: 3, presentation: 'three', index: 1 }, { ...awaiting, presentation: 'tail', index: 2 }], startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(droppedMiddle.status).toBe('mismatch')
    expect(droppedMiddle.differences).toContainEqual(expect.objectContaining({ kind: 'missing', id: 'b' }))

    const changedTail = await compareTranscriptAuditRows(
      { rows: refs, snapshotHeadSeq: 2 },
      { rows: [stable, { ...awaiting, presentation: 'different' }], startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(changedTail.status).toBe('mismatch')
    expect(changedTail.differences).toContainEqual(expect.objectContaining({ kind: 'missing', id: 'b' }))

    const oldEchoes = await compareTranscriptAuditRows(
      { rows: refs, snapshotHeadSeq: 2 },
      { rows: [stable, { ...awaiting, awaitingDurable: false }, { ...awaiting, awaitingDurable: false, index: 2 }], startBoundary: 'start', endBoundary: 'tail' },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(oldEchoes.status).toBe('mismatch')
    expect(oldEchoes.differences).toContainEqual(expect.objectContaining({ kind: 'missing', id: 'b' }))
  })

  it.each([
    ['saved reference', { capReached: true }, { capped: false }, 'reference_row_cap_reached'],
    ['browser DOM', { capReached: false }, { capped: true }, 'browser_row_cap_reached'],
  ])('cannot pass when the %s cap is reached', async (_name, referenceExtra, captureExtra, reason) => {
    const rows = [referenceRow('a', 1, 'same')]
    const browser = [{
      source: 'durable', id: 'a', semanticId: 'a', projectedSeq: 1, type: 'agent_message',
      category: 'messages', role: 'assistant', contentState: 'visible', presentation: 'same', inScope: true,
    }]
    const report = await compareTranscriptAuditRows(
      { rows, snapshotHeadSeq: 1, ...referenceExtra },
      { rows: browser, minProjectedSeq: 1, maxProjectedSeq: 1, ...captureExtra },
      { scopes: ['messages'], viewMode: 'all' },
    )
    expect(report.status).toBe('inconclusive')
    expect(report.reasons).toContain(reason)
  })

  it('reports ambiguous identity instead of matching by equal text', async () => {
    const html = mountedRow({ id: 'saved', seq: 1, text: 'same' }) +
      '<div data-transcript-row data-transcript-source="live" data-transcript-id="local" data-transcript-type="agentMessage" data-transcript-category="messages" data-transcript-role="assistant"><div data-transcript-content>same</div></div>'
    const report = await compare([referenceRow('saved', 1, 'same')], html)
    expect(report.status).toBe('inconclusive')
    expect(report.exclusions).toContainEqual(expect.objectContaining({ reason: 'ambiguous_identity' }))
  })

  it('uses rendered DOM presentation for Markdown without hiding substantive code changes', async () => {
    const markdown = '**Answer**\n\n```js\nconst n = 1\n```'
    const matching = mountedRow({ id: 'a', seq: 1, text: messageBodyHtml('agent', markdown) })
    const pass = await compare([referenceRow('a', 1, markdown)], matching)
    expect(pass.status, JSON.stringify(pass)).toBe('match')

    const changed = await compare([referenceRow('a', 1, markdown)], matching.replace('n = 1', 'n = 2'))
    expect(changed.status).toBe('mismatch')
    expect(changed.differences).toContainEqual(expect.objectContaining({ kind: 'changed', id: 'a' }))
    expect(normalizeTranscriptPresentation('x  \r\n')).toBe('x')
  })

  it('preserves blank lines and intra-line whitespace as substantive code presentation', async () => {
    const blankLine = await compare(
      [referenceRow('code', 1, 'line 1\n\nline 2', { type: 'user_message', role: 'user' })],
      mountedRow({ id: 'code', seq: 1, text: 'line 1\nline 2', type: 'user_message', role: 'user' }),
    )
    const intraLine = await compare(
      [referenceRow('space', 1, 'const  value = 1', { type: 'user_message', role: 'user' })],
      mountedRow({ id: 'space', seq: 1, text: 'const value = 1', type: 'user_message', role: 'user' }),
    )
    expect(blankLine.differences).toContainEqual(expect.objectContaining({ kind: 'changed', id: 'code' }))
    expect(intraLine.differences).toContainEqual(expect.objectContaining({ kind: 'changed', id: 'space' }))
  })

})
