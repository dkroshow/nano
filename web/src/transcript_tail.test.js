// Live tail follow (Slice G, gate G1). The SSE catalog nudge drives a tail
// fetch while a transcript is open; these cover the store contract: strict
// append-by-seq growth, no-growth no-op (referential stability), repair on
// mismatch, one-in-flight coalescing, no fetch without an open modal, and
// view/hide carried into the tail request. fetch is mocked; nudgeTranscriptTail
// is the mocked nudge entry point.
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  __resetTranscriptForTest,
  closeTranscript,
  loadTranscript,
  nudgeTranscriptTail,
  transcript,
} from './transcript_store.svelte.js'

const headers = (values = {}) => ({ get: (name) => values[name.toLowerCase()] ?? null })
const ok = (body, values) => ({ ok: true, status: 200, headers: headers(values), json: async () => body })
const flush = () => new Promise((resolve) => setTimeout(resolve))

function page(ids, { epoch = 'tpe_1' } = {}) {
  return {
    items: ids.map((id) => ({ id, projectedSeq: Number(id.slice(1)), type: 'agent_message', text: id })),
    turns: [],
    page: { hasMoreBefore: false, beforeCursor: null, endSeq: Number(ids.at(-1)?.slice(1) ?? 0) },
    projection: { state: 'ready', epoch },
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
  __resetTranscriptForTest()
})

describe('live tail follow', () => {
  it('appends only items newer than the last rendered seq, reusing existing item objects', async () => {
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i1', 'i2'])))
      .mockResolvedValueOnce(ok(page(['i2', 'i3', 'i4']))))

    await loadTranscript('nth_grow')
    const [i1, i2] = transcript.items
    await nudgeTranscriptTail()

    expect(transcript.items.map((item) => item.id)).toEqual(['i1', 'i2', 'i3', 'i4'])
    expect(transcript.items[0]).toBe(i1) // already-rendered items never re-created
    expect(transcript.items[1]).toBe(i2)
  })

  it('treats a no-growth tail as a no-op — same array, same item references', async () => {
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i1', 'i2'])))
      .mockResolvedValueOnce(ok(page(['i1', 'i2']))))

    await loadTranscript('nth_flat')
    const before = transcript.items
    await nudgeTranscriptTail()

    expect(transcript.items).toBe(before) // no state churn at all
  })

  it('repairs by reloading the tail window when the projection epoch resets', async () => {
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i1', 'i2'], { epoch: 'tpe_1' })))
      .mockResolvedValueOnce(ok(page(['i5', 'i6'], { epoch: 'tpe_2' }))))

    await loadTranscript('nth_reset')
    await nudgeTranscriptTail()

    expect(transcript.items.map((item) => item.id)).toEqual(['i5', 'i6'])
  })

  it('repairs when the tail window has slid entirely past the rendered tail', async () => {
    // Same projection, but every tail item is newer than everything rendered:
    // a plain append would drop the items in between, so reload instead.
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(ok(page(['i1', 'i2'])))
      .mockResolvedValueOnce(ok(page(['i5', 'i6']))))

    await loadTranscript('nth_slid')
    await nudgeTranscriptTail()

    expect(transcript.items.map((item) => item.id)).toEqual(['i5', 'i6'])
  })

  it('coalesces N nudges during one in-flight fetch into exactly one follow-up', async () => {
    let resolveFirstTail
    const firstTail = new Promise((resolve) => { resolveFirstTail = resolve })
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(ok(page(['i1', 'i2']))) // initial load
      .mockReturnValueOnce(firstTail) // tail fetch #1 (held open)
      .mockResolvedValueOnce(ok(page(['i2', 'i3']))) // the single follow-up
    vi.stubGlobal('fetch', fetchMock)

    await loadTranscript('nth_coalesce')
    expect(fetchMock).toHaveBeenCalledTimes(1)

    nudgeTranscriptTail() // starts tail fetch #1
    await flush()
    expect(fetchMock).toHaveBeenCalledTimes(2)

    nudgeTranscriptTail()
    nudgeTranscriptTail()
    nudgeTranscriptTail() // three more while #1 is in flight → one dirty follow-up
    await flush()
    expect(fetchMock).toHaveBeenCalledTimes(2) // still just #1 in flight

    resolveFirstTail(ok(page(['i2', 'i3'])))
    await flush()
    await flush()
    expect(fetchMock).toHaveBeenCalledTimes(3) // exactly one follow-up, no more
  })

  it('does not fetch when no modal is open', async () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    closeTranscript() // no thread selected

    await nudgeTranscriptTail()

    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('carries the current view and hide settings into the tail fetch', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(ok(page(['i1'])))
      .mockResolvedValueOnce(ok(page(['i1'])))
    vi.stubGlobal('fetch', fetchMock)

    await loadTranscript('nth_params', { view: 'dev', hiddenCategories: ['system'] })
    await nudgeTranscriptTail()

    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(String(fetchMock.mock.calls[1][0])).toContain('view=dev')
    expect(String(fetchMock.mock.calls[1][0])).toContain('hide=system')
  })
})
