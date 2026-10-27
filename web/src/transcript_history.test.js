// Durable-history channel (Slice R3, gate R3-2): the per-session instance of
// the transcript machinery that renders a resumed session's pre-attach
// history. Covers: tail load from the transcript API (no /state involved),
// zero-fetch seed adoption, cursor paging with order/cursor preservation,
// the exactly-once ETag tail revalidation (including the queued-before-load
// case and paged-back retention), and repair on projection reset. fetch is
// mocked per the existing transcript test patterns.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createDurableHistory, __resetTranscriptForTest } from './transcript_store.svelte.js'

const headers = (values = {}) => ({ get: (name) => values[name.toLowerCase()] ?? null })
const ok = (body, values) => ({ ok: true, status: 200, headers: headers(values), json: async () => body })
const notModified = () => ({ ok: false, status: 304, headers: headers(), json: async () => ({}) })
const changed = () => ({
  ok: false,
  status: 409,
  headers: headers(),
  json: async () => ({ error: { code: 'transcript_changed', message: 'seam turn not indexed' } }),
})

function page(ids, { beforeCursor = null, epoch = 'tpe_1' } = {}) {
  return {
    items: ids.map((id) => ({ id, projectedSeq: Number(id.slice(1)), type: 'agent_message', text: id })),
    turns: [],
    page: { hasMoreBefore: Boolean(beforeCursor), beforeCursor },
    projection: { state: 'ready', epoch },
  }
}

function seedOf(ids, { beforeCursor = null, etag = null } = {}) {
  const body = page(ids, { beforeCursor })
  return { ...body, state: 'ready', view: 'enhanced', hiddenCategories: undefined, etag }
}

afterEach(() => {
  vi.unstubAllGlobals()
  __resetTranscriptForTest()
})

describe('durable history channel', () => {
  it('loads the tail window from the transcript API — no other endpoint involved', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(ok(page(['i1', 'i2'])))
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1')
    await channel.load()

    expect(fetchMock).toHaveBeenCalledTimes(1)
    const url = String(fetchMock.mock.calls[0][0])
    expect(url).toContain('/api/v1/threads/th_1/transcript')
    expect(url).toContain('view=enhanced')
    expect(channel.state.items.map((item) => item.id)).toEqual(['i1', 'i2'])
    expect(channel.state.state).toBe('ready')
  })


  it('still reports a missing thread as an error outside a live session', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce({
      ok: false,
      status: 404,
      headers: headers(),
      json: async () => ({ error: { code: 'thread_not_found', message: 'Thread not found' } }),
    }))
    const channel = createDurableHistory('th_1')
    await channel.load()
    expect(channel.state.state).toBe('error')
    expect(channel.state.error).toBe('Thread not found')
  })

  it('adopts a seed with zero fetches', async () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1', seedOf(['i1', 'i2'], { etag: 'W/"a"' }))
    await channel.load()

    expect(fetchMock).not.toHaveBeenCalled()
    expect(channel.state.items.map((item) => item.id)).toEqual(['i1', 'i2'])
    expect(channel.state.state).toBe('ready')
  })

  it('pages earlier through the cursor, prepending in order and keeping the older cursor', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(ok(page(['i3', 'i4'], { beforeCursor: 'c3' })))
      .mockResolvedValueOnce(ok(page(['i1', 'i2'], { beforeCursor: 'c1' })))
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1')
    await channel.load()
    await channel.loadEarlier()

    expect(String(fetchMock.mock.calls[1][0])).toContain('before=c3')
    expect(channel.state.items.map((item) => item.id)).toEqual(['i1', 'i2', 'i3', 'i4'])
    expect(channel.state.page.beforeCursor).toBe('c1')
    expect(channel.state.page.hasMoreBefore).toBe(true)
  })

  it('surfaces a paging failure without touching the rendered items', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(ok(page(['i3'], { beforeCursor: 'c3' })))
      .mockResolvedValueOnce({ ok: false, status: 500, headers: headers(), json: async () => ({ error: { message: 'boom' } }) })
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1')
    await channel.load()
    await channel.loadEarlier()

    expect(channel.state.items.map((item) => item.id)).toEqual(['i3'])
    expect(channel.state.earlierError).toBe('boom')
    expect(channel.state.loadingEarlier).toBe(false)
  })

  it('repairs by reloading the tail when paging hits a projection reset', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(ok(page(['i3'], { beforeCursor: 'c3', epoch: 'tpe_1' })))
      .mockResolvedValueOnce(ok(page(['i1'], { beforeCursor: 'c1', epoch: 'tpe_2' })))
      .mockResolvedValueOnce(ok(page(['i8', 'i9'], { epoch: 'tpe_2' })))
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1')
    await channel.load()
    await channel.loadEarlier()

    expect(fetchMock).toHaveBeenCalledTimes(3)
    expect(channel.state.items.map((item) => item.id)).toEqual(['i8', 'i9'])
  })

  it('revalidates the tail exactly once, with If-None-Match from the seed ETag', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(notModified())
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1', seedOf(['i1', 'i2'], { etag: 'W/"a"' }))
    await channel.load()
    await channel.revalidateTail()
    await channel.revalidateTail() // second observation of ready → no-op

    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls[0][1].headers).toEqual({ 'If-None-Match': 'W/"a"' })
    expect(channel.state.items.map((item) => item.id)).toEqual(['i1', 'i2']) // 304 → untouched
  })

  it('merges a changed revalidation tail while retaining paged-back items and their cursor', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(ok(page(['i2', 'i3'])))
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1', seedOf(['i1', 'i2'], { beforeCursor: 'c1', etag: 'W/"a"' }))
    await channel.load()
    await channel.revalidateTail()

    expect(channel.state.items.map((item) => item.id)).toEqual(['i1', 'i2', 'i3'])
    // The oldest loaded page still owns the backward cursor.
    expect(channel.state.page.beforeCursor).toBe('c1')
    expect(channel.state.page.hasMoreBefore).toBe(true)
  })

  it.each([
    { name: 'an exhausted cached prefix', cached: ['i1', 'i2', 'i3', 'i4', 'i5', 'i6', 'i7', 'i8', 'i9'], cursor: null },
    { name: 'a still-pageable cached prefix', cached: ['i5', 'i6', 'i7', 'i8', 'i9'], cursor: 'before-5' },
  ])('uses the refreshed cursor across a non-overlapping reconnect gap with $name', async ({ cached, cursor }) => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(ok(page(['i20', 'i21', 'i22', 'i23', 'i24'], { beforeCursor: 'before-20' })))
      .mockResolvedValueOnce(ok(page(['i10', 'i11', 'i12', 'i13', 'i14', 'i15', 'i16', 'i17', 'i18', 'i19'], { beforeCursor: 'before-10' })))
      .mockResolvedValueOnce(ok(page(['i1', 'i2', 'i3', 'i4', 'i5', 'i6', 'i7', 'i8', 'i9'])))
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1', seedOf(cached, { beforeCursor: cursor, etag: 'W/"old"' }))
    await channel.load()
    await channel.revalidateTail()

    // No shared durable row means the old and refreshed windows cannot be
    // presented as one range. The refreshed page owns the paging cursor.
    expect(channel.state.items.map((item) => item.id)).toEqual(['i20', 'i21', 'i22', 'i23', 'i24'])
    expect(channel.state.page.beforeCursor).toBe('before-20')
    expect(channel.state.page.hasMoreBefore).toBe(true)

    await channel.loadEarlier()
    expect(String(fetchMock.mock.calls[1][0])).toContain('before=before-20')
    expect(channel.state.items.map((item) => item.id)).toEqual([
      'i10', 'i11', 'i12', 'i13', 'i14', 'i15', 'i16', 'i17', 'i18', 'i19',
      'i20', 'i21', 'i22', 'i23', 'i24',
    ])

    await channel.loadEarlier()
    expect(String(fetchMock.mock.calls[2][0])).toContain('before=before-10')
    expect(channel.state.items.map((item) => item.id)).toEqual([
      'i1', 'i2', 'i3', 'i4', 'i5', 'i6', 'i7', 'i8', 'i9',
      'i10', 'i11', 'i12', 'i13', 'i14', 'i15', 'i16', 'i17', 'i18', 'i19',
      'i20', 'i21', 'i22', 'i23', 'i24',
    ])
  })

  it('rearm allows exactly one more revalidation — the shared channel outlives one attach cycle', async () => {
    const fetchMock = vi.fn().mockResolvedValue(notModified())
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1', seedOf(['i1'], { etag: 'W/"a"' }))
    await channel.load()
    await channel.revalidateTail()
    await channel.revalidateTail() // guarded
    expect(fetchMock).toHaveBeenCalledTimes(1)

    channel.rearm() // next attach (or the detach back to read-only)
    await channel.revalidateTail()
    await channel.revalidateTail() // guarded again
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('queues a revalidation requested before the initial window lands — still exactly one', async () => {
    let resolveLoad
    const held = new Promise((resolve) => { resolveLoad = resolve })
    const fetchMock = vi.fn()
      .mockReturnValueOnce(held) // initial tail fetch, held open
      .mockResolvedValueOnce(notModified()) // the queued revalidation
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1')
    const loading = channel.load()
    let queuedSettled = false
    const queued = channel.revalidateTail().then((result) => {
      queuedSettled = true
      return result
    })
    await Promise.resolve()
    expect(queuedSettled).toBe(false) // the caller needs the post-load fetch, not queue admission
    expect(fetchMock).toHaveBeenCalledTimes(1)

    resolveLoad(ok(page(['i1']), { etag: 'W/"b"' }))
    await loading
    expect(await queued).toBe(true)

    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(fetchMock.mock.calls[1][1].headers).toEqual({ 'If-None-Match': 'W/"b"' })
  })

  it('treats an already-running tail refresh as deferred instead of failed', async () => {
    let resolveRefresh
    const held = new Promise((resolve) => { resolveRefresh = resolve })
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(ok(page(['i1']), { etag: 'W/"a"' }))
      .mockReturnValueOnce(held)
      .mockResolvedValueOnce(notModified())
    vi.stubGlobal('fetch', fetchMock)

    const channel = createDurableHistory('th_1')
    await channel.load()
    channel.rearm()
    const refreshing = channel.revalidateTail()
    let queuedSettled = false
    const queued = channel.revalidateTail().then((result) => {
      queuedSettled = true
      return result
    })
    await Promise.resolve()
    expect(queuedSettled).toBe(false)

    resolveRefresh(notModified())
    expect(await refreshing).toBe(true)
    expect(await queued).toBe(true)
    expect(fetchMock).toHaveBeenCalledTimes(3)
  })
})
