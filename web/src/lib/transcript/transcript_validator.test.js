// @vitest-environment jsdom

import { describe, expect, it } from 'vitest'
import {
  actualTranscriptRows,
  compareTranscriptRows,
  expectedTranscriptRows,
  transcriptValidationKey,
} from './transcript_validator.js'

describe('transcript render validator', () => {
  it('compares the intended durable/live order with mounted rows', () => {
    const expected = expectedTranscriptRows({
      durableItems: [{ id: 'history-1', type: 'agentMessage', text: 'old answer' }],
      liveItems: [{ id: 'live-1', type: 'userMessage', text: 'new prompt' }],
    })
    document.body.innerHTML = `
      <div id="root">
        <div data-transcript-row data-transcript-key="durable:history-1" data-transcript-source="durable" data-transcript-id="history-1" data-transcript-type="agentMessage"><span data-transcript-content>old answer</span></div>
        <div data-transcript-row data-transcript-key="live:live-1" data-transcript-source="live" data-transcript-id="live-1" data-transcript-type="userMessage"><span data-transcript-content>new prompt</span></div>
      </div>`

    const report = compareTranscriptRows(expected, actualTranscriptRows(document.querySelector('#root')))
    expect(report.ok).toBe(true)
    expect(report.expected.map((row) => row.key)).toEqual([
      'durable:history-1', 'live:live-1',
    ])
  })

  it('reports missing, extra, duplicate, blank, type, and order mismatches without message content', () => {
    const expected = expectedTranscriptRows({ liveItems: [
      { id: 'one', type: 'userMessage', text: 'secret prompt' },
      { id: 'two', type: 'agentMessage', text: 'secret answer' },
      { id: 'missing', type: 'agentMessage', text: 'never mounted' },
    ] })
    const actual = [
      { key: 'live:two', source: 'live', id: 'two', type: 'userMessage', hasContent: false },
      { key: 'live:one', source: 'live', id: 'one', type: 'userMessage', hasContent: true },
      { key: 'live:one', source: 'live', id: 'one', type: 'userMessage', hasContent: true },
      { key: 'live:extra', source: 'live', id: 'extra', type: 'agentMessage', hasContent: true },
    ]

    const report = compareTranscriptRows(expected, actual)
    expect(report.ok).toBe(false)
    expect(report.missing.map((row) => row.key)).toEqual(['live:missing'])
    expect(report.extra.map((row) => row.key)).toEqual(['live:extra'])
    expect(report.duplicates.map((row) => row.key)).toEqual(['live:one'])
    expect(report.blank.map((row) => row.key)).toEqual(['live:two'])
    expect(report.typeMismatch.map((row) => row.key)).toEqual(['live:two'])
    expect(report.orderMismatchCount).toBeGreaterThan(0)
    expect(JSON.stringify(report)).not.toContain('secret prompt')
  })

  it('rejects incomplete row identity', () => {
    expect(transcriptValidationKey('live', '')).toBe('')
    expect(transcriptValidationKey('', 'row')).toBe('')
  })

  it('fails validation when a mounted owner row has no raw render key', () => {
    const expected = expectedTranscriptRows({
      liveItems: [{ id: 'submission:action-1', type: 'userMessage', text: 'accepted prompt' }],
    })
    document.body.innerHTML = `
      <div id="root">
        <div data-transcript-row data-transcript-source="live" data-transcript-id="submission:action-1" data-transcript-type="userMessage"><span data-transcript-content>accepted prompt</span></div>
      </div>`

    const report = compareTranscriptRows(expected, actualTranscriptRows(document.querySelector('#root')))

    expect(report.ok).toBe(false)
    expect(report.missing.map((row) => row.key)).toEqual(['live:submission:action-1'])
  })

  it('reports two DOM rows that claim one canonical semantic message', () => {
    const expected = expectedTranscriptRows({
      durableItems: [{ id: 'nit_canonical', type: 'agentMessage', text: 'one message' }],
      liveItems: [{ id: 'durable:nit_canonical', durableItemID: 'nit_canonical', type: 'agentMessage', text: 'one message' }],
    })
    const actual = [
      { key: 'durable:nit_canonical', source: 'durable', id: 'nit_canonical', semanticID: 'nit_canonical', type: 'agentMessage', hasContent: true },
      { key: 'live:durable:nit_canonical', source: 'live', id: 'durable:nit_canonical', semanticID: 'nit_canonical', type: 'agentMessage', hasContent: true },
    ]
    const report = compareTranscriptRows(expected, actual)
    expect(report.ok).toBe(false)
    expect(report.semanticDuplicates.map((row) => row.key)).toEqual(['live:durable:nit_canonical'])
  })

  it('does not call a mounted row visible when a transcript ancestor hides it', () => {
    const expected = expectedTranscriptRows({
      liveItems: [{ id: 'accepted', type: 'userMessage', text: 'accepted prompt' }],
    })
    document.body.innerHTML = `
      <div id="root" style="visibility: hidden">
        <div data-transcript-row data-transcript-key="live:accepted" data-transcript-source="live" data-transcript-id="accepted" data-transcript-type="userMessage"><span data-transcript-content>accepted prompt</span></div>
      </div>`

    const actual = actualTranscriptRows(document.querySelector('#root'))
    const report = compareTranscriptRows(expected, actual)

    expect(actual[0].hasContent).toBe(false)
    expect(report.ok).toBe(false)
    expect(report.blank.map((row) => row.key)).toEqual(['live:accepted'])
  })
})
