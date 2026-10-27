import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  __resetTranscriptForTest,
  __transcriptCacheBytesForTest,
	__bodyCacheBytesForTest,
	__setBodyCacheLimitForTest,
  loadEarlier,
  loadTranscript,
	loadTranscriptBody,
  loadTranscriptEvidence,
  setTranscriptCategoryVisible,
  setTranscriptView,
  transcript,
} from './transcript_store.svelte.js'
import { TRANSCRIPT_CACHE_BYTES } from './transcript_query.js'

const headers = (values = {}) => ({ get: (name) => values[name.toLowerCase()] ?? null })
const ok = (body, values) => ({ ok: true, status: 200, headers: headers(values), json: async () => body })
const flush = () => new Promise((resolve) => setTimeout(resolve))

function page(ids, { beforeCursor = null, epoch = 'tpe_1' } = {}) {
  return {
    items: ids.map((id) => ({ id, projectedSeq: Number(id.slice(1)), type: 'agent_message', text: id })),
    turns: [],
    page: { hasMoreBefore: Boolean(beforeCursor), beforeCursor },
    projection: { state: 'ready', epoch },
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
  __resetTranscriptForTest()
})

describe('transcript store', () => {
  it('loads Enhanced by default without eagerly requesting native evidence', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(ok(page(['i1']))))

    await loadTranscript('nth_enhanced')

    expect(fetch).toHaveBeenCalledTimes(1)
    expect(fetch.mock.calls[0][0]).toContain('view=enhanced')
    expect(fetch.mock.calls[0][0]).toContain('hide=reasoning%2CtoolCall%2CtoolResult%2Csubagent%2Csystem%2Cerror%2Cunclassified')
    expect(transcript.view).toBe('enhanced')
  })

  it('keeps Dev and filter profiles in separate cache/request identities', async () => {
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i1'])))
      .mockResolvedValueOnce(ok(page(['i2'])))
      .mockResolvedValueOnce(ok(page(['i3']))))

    await loadTranscript('nth_profiles')
    await setTranscriptView('dev')
    await setTranscriptCategoryVisible('system', false)

    expect(fetch.mock.calls[1][0]).toContain('view=dev&hide=none')
    expect(fetch.mock.calls[2][0]).toContain('view=dev&hide=system')
    expect(transcript.items.map((item) => item.id)).toEqual(['i3'])
  })

  it('fetches exact native evidence only when explicitly expanded', async () => {
    const projected = page(['i1'])
    projected.projection.evidenceAvailable = true
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(projected))
      .mockResolvedValueOnce(ok({
        provider: 'codex',
        path: '/tmp/rollout.jsonl',
        line: 7,
        rawRow: '{"type":"response_item"}',
      })))

    await loadTranscript('nth_evidence')
    expect(fetch).toHaveBeenCalledTimes(1)

    await loadTranscriptEvidence('i1')
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(fetch.mock.calls[1][0]).toBe('/api/v1/threads/nth_evidence/transcript/evidence/i1')
    expect(fetch.mock.calls[1][1].cache).toBe('no-store')
    expect(transcript.evidence.i1.data.line).toBe(7)
  })

  it('hydrates a retained normalized body independently of native evidence', async () => {
    const projected = page(['i1'])
    projected.items[0].contentRef = 'ncb_1'
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(projected))
			.mockResolvedValueOnce(ok({
				itemId: 'i1', projectionKey: 'tpe_1', contentRef: 'ncb_1', text: 'complete retained content',
			})))

		await loadTranscript('nth_body')
		await loadTranscriptBody('i1')

		expect(fetch.mock.calls[1][0]).toBe('/api/v1/threads/nth_body/transcript/body/i1')
		expect(transcript.body.i1.data.text).toBe('complete retained content')
	})

  it('keeps only one potentially large native evidence row resident', async () => {
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i1', 'i2'])))
      .mockResolvedValueOnce(ok({ raw: '{"item":1}' }))
      .mockResolvedValueOnce(ok({ raw: '{"item":2}' })))

    await loadTranscript('nth_bounded_evidence')
    await loadTranscriptEvidence('i1')
    expect(Object.keys(transcript.evidence)).toEqual(['i1'])

    await loadTranscriptEvidence('i2')
    expect(Object.keys(transcript.evidence)).toEqual(['i2'])
    expect(transcript.evidence.i2.data.raw).toBe('{"item":2}')
  })

  it('deduplicates a body request and keeps its retained body in the byte cache', async () => {
    const projected = page(['i1'])
    projected.items[0].contentRef = 'ncb_1'
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(projected))
      .mockResolvedValueOnce(ok({ itemId: 'i1', projectionKey: 'tpe_1', contentRef: 'ncb_1', text: 'retained body' })))
    await loadTranscript('nth_body_dedupe')
    await Promise.all([loadTranscriptBody('i1'), loadTranscriptBody('i1')])
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(__bodyCacheBytesForTest()).toBeGreaterThan(0)
  })

	it('replaces a ready retained body when a tail update changes its content reference', async () => {
		const projected = page(['i1']); projected.items[0].contentRef = 'r1'
		vi.stubGlobal('fetch', vi.fn()
			.mockResolvedValueOnce(ok(projected))
			.mockResolvedValueOnce(ok({ itemId: 'i1', projectionKey: 'tpe_1', contentRef: 'r1', text: 'first body' }))
			.mockResolvedValueOnce(ok({ itemId: 'i1', projectionKey: 'tpe_1', contentRef: 'r2', text: 'replacement body' })))
		await loadTranscript('nth_content_ref'); await loadTranscriptBody('i1')
		expect(transcript.body.i1.data.text).toBe('first body')
		transcript.items = [{ ...transcript.items[0], contentRef: 'r2' }]
		await loadTranscriptBody('i1')
		expect(fetch).toHaveBeenCalledTimes(3)
		expect(transcript.body.i1.data.text).toBe('replacement body')
		expect(transcript.body.i1.key).toContain('\u0000r2')
	})

	it('aborts and rejects an old content-reference body completion without caching it', async () => {
		const projected = page(['i1']); projected.items[0].contentRef = 'r1'
		let resolveOld; let resolveNew; let oldSignal
		vi.stubGlobal('fetch', vi.fn((_, options = {}) => {
			if (fetch.mock.calls.length === 1) return Promise.resolve(ok(projected))
			if (fetch.mock.calls.length === 2) {
				oldSignal = options.signal
				return new Promise((resolve) => { resolveOld = resolve })
			}
			if (fetch.mock.calls.length === 3) return new Promise((resolve) => { resolveNew = resolve })
			return new Promise(() => {})
		}))
		await loadTranscript('nth_content_ref_race')
		void loadTranscriptBody('i1'); await flush()
		transcript.items = [{ ...transcript.items[0], contentRef: 'r2' }]
		void loadTranscriptBody('i1'); await flush()
		expect(fetch).toHaveBeenCalledTimes(3)
		expect(oldSignal.aborted).toBe(true)
		resolveOld(ok({ itemId: 'i1', projectionKey: 'tpe_1', contentRef: 'r1', text: 'stale body' }))
		await flush()
		expect(transcript.body.i1?.data?.text).not.toBe('stale body')
		resolveNew(ok({ itemId: 'i1', projectionKey: 'tpe_1', contentRef: 'r2', text: 'current body' }))
		await flush()
		expect(transcript.body.i1.data.text).toBe('current body')
		// Switching back to r1 must fetch: the abort-insensitive old response
		// could not have populated the r1 cache entry.
		transcript.items = [{ ...transcript.items[0], contentRef: 'r1' }]
		void loadTranscriptBody('i1'); await flush()
		expect(fetch).toHaveBeenCalledTimes(4)
	})

  it('runs no more than four retained-body loads at once', async () => {
    const projected = page(['i1', 'i2', 'i3', 'i4', 'i5'])
    for (const row of projected.items) row.contentRef = `ref-${row.id}`
    const pending = []
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(projected))
      .mockImplementation(() => new Promise((resolve) => pending.push(resolve))))
    await loadTranscript('nth_body_limit')
    for (const row of projected.items) void loadTranscriptBody(row.id)
    await flush()
    expect(fetch).toHaveBeenCalledTimes(5) // transcript plus four bodies
    pending.shift()(ok({ itemId: 'i1', projectionKey: 'tpe_1', contentRef: 'ref-i1', text: 'one' }))
    await flush()
    expect(fetch).toHaveBeenCalledTimes(6)
  })

  it('clears hydrated bodies when a replacement projection arrives', async () => {
    const first = page(['i1'], { epoch: 'p1' }); first.items[0].contentRef = 'r1'
    const replacement = page(['i2'], { epoch: 'p2' }); replacement.items[0].contentRef = 'r2'
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(ok(first)).mockResolvedValueOnce(ok({ itemId: 'i1', projectionKey: 'p1', contentRef: 'r1', text: 'old' })).mockResolvedValueOnce(ok(replacement)))
    await loadTranscript('nth_projection'); await loadTranscriptBody('i1'); await loadTranscript('nth_projection', { force: true })
    expect(transcript.projection.epoch).toBe('p2')
    expect(transcript.body).toEqual({})
  })

  it('does not let an abort-insensitive old body completion consume current-generation slots', async () => {
    const oldPage = page(['o1', 'o2', 'o3', 'o4'], { epoch: 'p1' }); oldPage.items.forEach((row) => { row.contentRef = `r-${row.id}` })
    const newPage = page(['n1', 'n2', 'n3', 'n4', 'n5'], { epoch: 'p2' }); newPage.items.forEach((row) => { row.contentRef = `r-${row.id}` })
    const oldResolvers = []; const newResolvers = []; const signals = []
    let call = 0
    vi.stubGlobal('fetch', vi.fn((url, options = {}) => {
      const current = call++
      if (current === 0) return Promise.resolve(ok(oldPage))
      if (current >= 1 && current <= 4) return new Promise((resolve) => { oldResolvers.push(resolve); signals.push(options.signal) })
      if (current === 5) return Promise.resolve(ok(newPage))
      return new Promise((resolve) => { newResolvers.push(resolve); signals.push(options.signal) })
    }))
    await loadTranscript('nth_old')
    for (const row of oldPage.items) void loadTranscriptBody(row.id)
    await flush(); expect(fetch).toHaveBeenCalledTimes(5)
    await loadTranscript('nth_new')
    for (const row of newPage.items) void loadTranscriptBody(row.id)
    await flush(); expect(fetch).toHaveBeenCalledTimes(10)
    expect(signals.slice(0, 4).every((signal) => signal.aborted)).toBe(true)
    oldResolvers[0](ok({ itemId: 'o1', projectionKey: 'p1', contentRef: 'r-o1', text: 'stale' }))
    await flush(); expect(fetch).toHaveBeenCalledTimes(10); expect(transcript.body.o1).toBeUndefined()
    newResolvers[0](ok({ itemId: 'n1', projectionKey: 'p2', contentRef: 'r-n1', text: 'current' }))
    await flush(); expect(fetch).toHaveBeenCalledTimes(11)
  })

  it('keys retained bodies by projection and evicts least-recently-used bytes', async () => {
    __setBodyCacheLimitForTest(500)
    const first = page(['a', 'b', 'c'], { epoch: 'p1' }); first.items.forEach((row) => { row.contentRef = `r-${row.id}` })
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(first))
      .mockResolvedValueOnce(ok({ itemId: 'a', projectionKey: 'p1', contentRef: 'r-a', text: 'a'.repeat(120) }))
      .mockResolvedValueOnce(ok({ itemId: 'b', projectionKey: 'p1', contentRef: 'r-b', text: 'b'.repeat(120) }))
      .mockResolvedValueOnce(ok({ itemId: 'c', projectionKey: 'p1', contentRef: 'r-c', text: 'c'.repeat(120) }))
      .mockResolvedValueOnce(ok({ itemId: 'b', projectionKey: 'p1', contentRef: 'r-b', text: 'b'.repeat(120) }))
      .mockResolvedValueOnce(ok(page(['a'], { epoch: 'p2' })))
      .mockResolvedValueOnce(ok({ itemId: 'a', projectionKey: 'p2', contentRef: 'r-a', text: 'new projection' })))
    await loadTranscript('nth_cache'); await loadTranscriptBody('a'); await loadTranscriptBody('b')
    // Remove only A's visible state so a cache hit touches A's recency while B
    // remains rendered. C must then evict B from both stores, not merely from
    // an already-empty visible-body map.
    transcript.body = { b: transcript.body.b }; await loadTranscriptBody('a'); expect(fetch).toHaveBeenCalledTimes(3)
    await loadTranscriptBody('c'); expect(fetch).toHaveBeenCalledTimes(4)
		// A was touched most recently, so C evicts B from both the byte cache and
		// the rendered retained-body state. A and C remain resident.
		expect(transcript.body.a?.state).toBe('ready'); expect(transcript.body.c?.state).toBe('ready')
		expect(transcript.body.b).toBeUndefined(); expect(__bodyCacheBytesForTest()).toBeLessThanOrEqual(500)
    transcript.body = {}; await loadTranscriptBody('a'); expect(fetch).toHaveBeenCalledTimes(4)
    await loadTranscriptBody('b'); expect(fetch).toHaveBeenCalledTimes(5)
    await loadTranscript('nth_cache', { force: true }); transcript.items[0].contentRef = 'r-a'; await loadTranscriptBody('a')
    expect(fetch).toHaveBeenCalledTimes(7)
  })

	it('does not retain an individual body that exceeds the byte-cache limit', async () => {
		__setBodyCacheLimitForTest(80)
		const projected = page(['large']); projected.items[0].contentRef = 'r-large'
		vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(ok(projected)).mockResolvedValueOnce(ok({
			itemId: 'large', projectionKey: 'tpe_1', contentRef: 'r-large', text: 'x'.repeat(200),
		})))
		await loadTranscript('nth_body_too_large'); await loadTranscriptBody('large')
		expect(transcript.body.large).toBeUndefined()
		expect(__bodyCacheBytesForTest()).toBe(0)
	})

  it('ignores a late abort-insensitive evidence response after a newer item wins', async () => {
    let resolveFirstEvidence
    const firstEvidence = new Promise((resolve) => { resolveFirstEvidence = resolve })
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i1', 'i2'])))
      .mockReturnValueOnce(firstEvidence)
      .mockResolvedValueOnce(ok({ raw: '{"item":2}' })))

    await loadTranscript('nth_evidence_race')
    const stale = loadTranscriptEvidence('i1')
    await flush()
    await loadTranscriptEvidence('i2')
    resolveFirstEvidence(ok({ raw: '{"item":1,"late":true}' }))
    await stale

    expect(Object.keys(transcript.evidence)).toEqual(['i2'])
    expect(transcript.evidence.i2.data.raw).toBe('{"item":2}')
  })

  it('loads only the selected thread and discards a superseded response', async () => {
    let resolveFirst
    const first = new Promise((resolve) => { resolveFirst = resolve })
    vi.stubGlobal('fetch', vi.fn()
      .mockReturnValueOnce(first)
      .mockResolvedValueOnce(ok(page(['i2']))))

    const stale = loadTranscript('nth_old')
    await loadTranscript('nth_new')
    resolveFirst(ok(page(['i1'])))
    await stale

    expect(transcript.threadId).toBe('nth_new')
    expect(transcript.items.map((item) => item.id)).toEqual(['i2'])
  })

  it('prepends an older page without durable duplicates', async () => {
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i3', 'i4'], { beforeCursor: 'older' })))
      .mockResolvedValueOnce(ok(page(['i1', 'i2', 'i3']))))

    await loadTranscript('nth_1')
    await loadEarlier()

    expect(transcript.items.map((item) => item.id)).toEqual(['i1', 'i2', 'i3', 'i4'])
    expect(fetch.mock.calls[1][0]).toContain('before=older')
  })

  it('renders a cached thread immediately while revalidating it', async () => {
    let resolveRevalidate
    const pending = new Promise((resolve) => { resolveRevalidate = resolve })
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i7']), { etag: '"one"' }))
      .mockReturnValueOnce(pending))

    await loadTranscript('nth_cached')
    loadTranscript('nth_cached')
    await flush()

    expect(transcript.state).toBe('ready')
    expect(transcript.items[0].id).toBe('i7')
    expect(fetch.mock.calls[1][1].headers['If-None-Match']).toBe('"one"')
    resolveRevalidate({ ok: false, status: 304, headers: headers() })
  })

  it('keeps explicitly loaded older pages when the tail revalidates', async () => {
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i3', 'i4'], { beforeCursor: 'older' })))
      .mockResolvedValueOnce(ok(page(['i1', 'i2'])))
      .mockResolvedValueOnce(ok(page(['i4', 'i5'], { beforeCursor: 'newer-boundary' }))))

    await loadTranscript('nth_paged')
    await loadEarlier()
    await loadTranscript('nth_paged')

    expect(transcript.items.map((item) => item.id)).toEqual(['i1', 'i2', 'i3', 'i4', 'i5'])
    expect(transcript.page.hasMoreBefore).toBe(false)
  })

  it('never admits an oversized single response into the memory cache', async () => {
    const oversized = page(['i1'])
    oversized.items[0].text = 'x'.repeat(TRANSCRIPT_CACHE_BYTES + 1)
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(oversized))
      .mockResolvedValueOnce(ok(page(['i2']))))

    await loadTranscript('nth_large')
    expect(__transcriptCacheBytesForTest()).toBe(0)
    await loadTranscript('nth_large')
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(transcript.items[0].id).toBe('i2')
  })

  it('weighs a merged revalidation by retained history and evicts it at the cap', async () => {
    const nearCap = page(['i1', 'i2'])
    nearCap.items[0].text = 'x'.repeat(TRANSCRIPT_CACHE_BYTES - (64 * 1024))
    const newTail = page(['i2', 'i3'])
    newTail.items[1].text = 'y'.repeat(128 * 1024)
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(nearCap))
      .mockResolvedValueOnce(ok(newTail)))

    await loadTranscript('nth_near_cap')
    expect(__transcriptCacheBytesForTest()).toBeGreaterThan(15 * 1024 * 1024)
    await loadTranscript('nth_near_cap')

    expect(transcript.items.map((item) => item.id)).toEqual(['i1', 'i2', 'i3'])
    expect(__transcriptCacheBytesForTest()).toBe(0)
  })
})
