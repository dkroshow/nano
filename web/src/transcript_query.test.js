import { describe, expect, it } from 'vitest'
import {
  canonicalHiddenCategories,
  defaultHiddenCategories,
  mergeTranscriptItems,
	transcriptBodyURL,
  transcriptEvidenceURL,
  transcriptProfileKey,
  transcriptURL,
} from './transcript_query.js'

describe('transcript query', () => {
  it('requests a bounded tail and encodes backward cursors', () => {
    expect(transcriptURL('nth/a')).toBe('/api/v1/threads/nth%2Fa/transcript?limit=40&view=enhanced&hide=reasoning%2CtoolCall%2CtoolResult%2Csubagent%2Csystem%2Cerror%2Cunclassified')
    expect(transcriptURL('nth/a', 'cursor +/')).toContain('before=cursor+%2B%2F')
  })

  it('requests explicit Dev and category profiles server-side', () => {
    expect(transcriptURL('nth/a', null, { view: 'dev', hiddenCategories: [] }))
      .toBe('/api/v1/threads/nth%2Fa/transcript?limit=40&view=dev&hide=none')
    expect(transcriptURL('nth/a', null, { view: 'enhanced', hiddenCategories: ['error', 'messages'] }))
      .toContain('hide=messages%2Cerror')
  })

  it('uses stable category ordering in profile cache keys', () => {
    expect(canonicalHiddenCategories(['system', 'reasoning', 'system', 'unknown']))
      .toEqual(['reasoning', 'system'])
    expect(transcriptProfileKey('dev', ['system', 'reasoning']))
      .toBe('dev:reasoning,system')
    expect(defaultHiddenCategories('enhanced')).toEqual(['reasoning', 'toolCall', 'toolResult', 'subagent', 'system', 'error', 'unclassified'])
  })

  it('builds an item-scoped lazy evidence endpoint', () => {
    expect(transcriptEvidenceURL('nth/a', 'item / 7'))
      .toBe('/api/v1/threads/nth%2Fa/transcript/evidence/item%20%2F%207')
  })

	it('builds an item-scoped retained body endpoint', () => {
		expect(transcriptBodyURL('nth/a', 'item / 7'))
			.toBe('/api/v1/threads/nth%2Fa/transcript/body/item%20%2F%207')
	})

  it('deduplicates durable item ids and keeps projected order', () => {
    const items = mergeTranscriptItems(
      [{ id: 'b', projectedSeq: 2 }, { id: 'a', projectedSeq: 1 }],
      [{ id: 'b', projectedSeq: 2 }, { id: 'c', projectedSeq: 3 }],
    )
    expect(items.map((item) => item.id)).toEqual(['a', 'b', 'c'])
  })

  it('lets a revalidated durable row replace its cached copy', () => {
    const items = mergeTranscriptItems(
      [{ id: 'a', projectedSeq: 1, text: 'old' }],
      [{ id: 'a', projectedSeq: 1, text: 'new' }],
    )
    expect(items).toEqual([{ id: 'a', projectedSeq: 1, text: 'new' }])
  })
})
