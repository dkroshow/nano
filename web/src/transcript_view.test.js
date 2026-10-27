import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  senderSubmissionTimestampFromFooter,
  consumeTranscriptFilterEscape,
  evidenceDisplay,
  evidenceProvenance,
	formatSubmissionTimestamp,
  formatTranscriptTimestamp,
  liveItemCategory,
  normalizedItemMetadata,
  shouldAutoLoadEarlier,
  transcriptCategoryLabel,
  transcriptItemCategory,
  transcriptItemKind,
  turnDurationsByFinalItem,
} from './transcript_view.js'
import senderFooterFixture from './lib/fixtures/sender-footer.json'
import { ENHANCED_HIDDEN_CATEGORIES } from './transcript_query.js'

afterEach(() => vi.restoreAllMocks())

describe('live wire item categories', () => {
  // The live block renders raw wire types; the enhanced hide-set is expressed
  // in durable category keys. Every type either dialect can produce has to land
  // on a key, or the clean view silently leaks it.
  it('maps every live type both dialects produce onto a durable category', () => {
    const mapped = Object.fromEntries(
      [
        'userMessage',
        'agentMessage',
        'reasoning',
        'commandExecution',
        'fileChange',
        'mcpToolCall',
        'dynamicToolCall',
        'webSearch',
        'plan',
        'toolUse',
        'subAgentActivity',
        'contextCompaction',
      ].map((type) => [type, liveItemCategory({ type })]),
    )

    expect(mapped).toEqual({
      userMessage: 'messages',
      agentMessage: 'messages',
      reasoning: 'reasoning',
      commandExecution: 'toolCall',
      fileChange: 'toolCall',
      mcpToolCall: 'toolCall',
      dynamicToolCall: 'toolCall',
      webSearch: 'toolCall',
      plan: 'toolCall',
      toolUse: 'toolCall',
      subAgentActivity: 'subagent',
      contextCompaction: 'system',
    })
  })

  it('leaves unknown types unclassified, which Enhanced hides without deleting them', () => {
    expect(liveItemCategory({ type: 'somethingCodexAddedLater' })).toBe('unclassified')
    expect(liveItemCategory(undefined)).toBe('unclassified')
    expect(ENHANCED_HIDDEN_CATEGORIES).toContain('unclassified')
  })

  it('hides everything but user and agent messages, subagent work included', () => {
    const hidden = (type) => ENHANCED_HIDDEN_CATEGORIES.includes(liveItemCategory({ type }))

    expect(['reasoning', 'commandExecution', 'toolUse', 'contextCompaction'].map(hidden)).toEqual([
      true, true, true, true,
    ])
    expect(['agentMessage', 'userMessage', 'subAgentActivity'].map(hidden)).toEqual([false, false, true])
  })
})

describe('transcript presentation', () => {
  it('assigns stable role and category classes from normalized items', () => {
    const user = { type: 'user_message', role: 'user' }
    const agent = { type: 'agent_message', role: 'assistant' }
    const tool = { type: 'command' }
    const error = { type: 'notice', status: 'error' }
		const question = { type: 'interaction', role: 'assistant', status: 'completed' }

    expect([transcriptItemKind(user), transcriptItemCategory(user)]).toEqual(['user', 'messages'])
    expect([transcriptItemKind(agent), transcriptItemCategory(agent)]).toEqual(['agent', 'messages'])
    expect([transcriptItemKind(tool), transcriptItemCategory(tool)]).toEqual(['aux', 'toolCall'])
    expect([transcriptItemKind(error), transcriptItemCategory(error)]).toEqual(['error', 'error'])
		expect([transcriptItemKind(question), transcriptItemCategory(question)]).toEqual(['agent', 'messages'])
    expect(transcriptCategoryLabel('unclassified')).toBe('Unclassified Provider Events')
  })

  it('keeps a compact human timestamp while retaining invalid-value safety', () => {
    const DateTimeFormat = Intl.DateTimeFormat
    vi.spyOn(Intl, 'DateTimeFormat').mockImplementation((locale, options) =>
      new DateTimeFormat(locale, { ...options, timeZone: 'UTC' }))
    expect(formatTranscriptTimestamp('2026-07-14T12:34:56Z')).toMatch(/Jul.*14.*12:34:56/i)
    expect(formatTranscriptTimestamp('not-a-date')).toBe('')
  })

  it('formats durable agent submission time in the browser zone across daylight saving', () => {
    const DateTimeFormat = Intl.DateTimeFormat
    let timeZone = 'America/Los_Angeles'
    vi.spyOn(Intl, 'DateTimeFormat').mockImplementation((locale, options) =>
      new DateTimeFormat(locale, { ...options, timeZone }))
    expect(formatSubmissionTimestamp('2026-08-27T15:22:14.843Z')).toBe('Aug 27, 8:22:14 AM PDT')
    expect(formatSubmissionTimestamp('2026-01-15T16:22:14Z')).toBe('Jan 15, 8:22:14 AM PST')
    timeZone = 'UTC'
    expect(formatSubmissionTimestamp('2026-08-27T15:22:14.843Z')).toBe('Aug 27, 3:22:14 PM UTC')
    expect(formatSubmissionTimestamp('not-a-date')).toBe('')
  })

  it('auto-pages only near the oldest loaded transcript edge', () => {
    const ready = { hasOlder: true, loadingEarlier: false, earlierError: null }

    expect(shouldAutoLoadEarlier({ ...ready, scrollTop: 0 })).toBe(true)
    expect(shouldAutoLoadEarlier({ ...ready, scrollTop: 119 })).toBe(true)
    expect(shouldAutoLoadEarlier({ ...ready, scrollTop: 120 })).toBe(false)
    expect(shouldAutoLoadEarlier({ ...ready, scrollTop: Number.NaN })).toBe(false)
    expect(shouldAutoLoadEarlier({ ...ready, scrollTop: 40, hasOlder: false })).toBe(false)
    expect(shouldAutoLoadEarlier({ ...ready, scrollTop: 40, loadingEarlier: true })).toBe(false)
    expect(shouldAutoLoadEarlier({ ...ready, scrollTop: 40, earlierError: 'offline' })).toBe(false)
  })

  it('exposes normalized metadata and backend provenance names', () => {
    const item = { id: 'i1', projectedSeq: 4, type: 'tool_result', category: 'toolResult', callId: 'c1' }
    expect(normalizedItemMetadata(item)).toContainEqual(['category', 'toolResult'])
    expect(evidenceProvenance({
      provider: 'codex',
      sourcePath: '/tmp/rollout.jsonl',
      sourceLine: 9,
      sourceRowType: 'response_item',
      sourcePayloadType: 'function_call_output',
      rawBytes: 128,
      validJson: true,
      validUtf8: true,
      sha256: 'abc123',
    })).toEqual([
      ['provider', 'codex'],
      ['path', '/tmp/rollout.jsonl'],
      ['line', 9],
      ['row type', 'response_item'],
      ['payload', 'function_call_output'],
      ['bytes', 128],
      ['valid JSON', 'true'],
      ['valid UTF-8', 'true'],
      ['SHA-256', 'abc123'],
    ])
    expect(evidenceDisplay({ raw: '{"exact": true}', validUtf8: true })).toEqual({
      encoding: 'UTF-8 native row',
      text: '{"exact": true}',
    })
  })

  it('labels invalid UTF-8 evidence as untouched base64 bytes', () => {
    expect(evidenceDisplay({ rawBase64: '/wA=', validUtf8: false })).toEqual({
      encoding: 'Base64 · exact native bytes (not decoded)',
      text: '/wA=',
    })
  })

  it('consumes filter Escape before the modal-level window handler', () => {
    const event = {
      key: 'Escape',
      preventDefault: vi.fn(),
      stopPropagation: vi.fn(),
      stopImmediatePropagation: vi.fn(),
    }
    const close = vi.fn()

    expect(consumeTranscriptFilterEscape(event, true, close)).toBe(true)
    expect(event.preventDefault).toHaveBeenCalledOnce()
    expect(event.stopPropagation).toHaveBeenCalledOnce()
    expect(event.stopImmediatePropagation).toHaveBeenCalledOnce()
    expect(close).toHaveBeenCalledOnce()
  })
})

describe('turnDurationsByFinalItem', () => {
  it('keys the duration by the item that ended the turn', () => {
    const map = turnDurationsByFinalItem([
      { id: 't1', finalItemId: 'item-9', durationMs: 760_000 },
      { id: 't2', finalItemId: 'item-20', durationMs: 42_000 },
    ])
    expect(map.get('item-9')).toBe('12m 40s')
    expect(map.get('item-20')).toBe('42s')
  })

  it('omits turns with no recorded duration', () => {
    const map = turnDurationsByFinalItem([
      { id: 't1', finalItemId: 'item-9' },
      { id: 't2', finalItemId: 'item-10', durationMs: null },
      { id: 't3', finalItemId: 'item-11', durationMs: 0 },
    ])
    expect(map.size).toBe(0)
  })

  // Sub-second turns render nothing: "(0s)" beside a message is noise.
  it('omits a turn shorter than a second', () => {
    expect(turnDurationsByFinalItem([{ finalItemId: 'i', durationMs: 400 }]).size).toBe(0)
  })

  // An in-progress turn has no final item, so there is nothing to hang a
  // duration on and nothing to guess.
  it('ignores turns with no final item', () => {
    expect(turnDurationsByFinalItem([{ id: 't1', durationMs: 5_000 }]).size).toBe(0)
    expect(turnDurationsByFinalItem(null).size).toBe(0)
  })
})

// The presentation boundary accepts the production sender footer and mail
// trailer only; a pasted matching footer intentionally has the same display.
describe('saved sender footer submission time', () => {
  const { text, submittedAt } = senderFooterFixture
  const notice = '\n\n[nano: 2 unread — GET /api/v1/hosted-threads/nht_00000000000000000000000000000000/inbox]'
  it.each([
    ['captured footer', text, submittedAt],
    ['production unread notice after footer', text + notice, submittedAt],
    ['human prompt', 'Inspect the parser.', ''],
    ['legacy footer', text.replace(' · submitted ' + submittedAt, ''), ''],
    ['system footer', 'Inspect the parser.\n\n[from nano]', ''],
    ['fractional time not emitted by senderFooter', text.replace(submittedAt, '2026-10-08T01:29:25.123Z'), ''],
    ['offset not emitted by senderFooter', text.replace(submittedAt, '2026-10-08T01:29:25+00:00'), ''],
    ['text after footer', text + '\nAnother sentence.', ''],
  ])('%s', (_name, prompt, expected) => {
    expect(senderSubmissionTimestampFromFooter(prompt)).toBe(expected)
  })
})
