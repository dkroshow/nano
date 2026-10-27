// Latest-request-wins regression tests for the explorer dataset store. The
// fetch stubs deliberately IGNORE the abort signal so a superseded request
// still resolves — proving the sequence guard alone keeps a stale response
// from clobbering a newer window fetch.
import { describe, it, expect, vi, afterEach } from 'vitest'
import { explorer, loadDataset, setFetchParam } from './explorer.svelte.js'
import { DEFAULT_FETCH, DEFAULT_VIEW } from './explorer_query.js'

function deferred() {
  let resolve, reject
  const promise = new Promise((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

const okJson = (data) => ({ ok: true, json: async () => data })
const page = (ids, catalogVersion, nextCursor = null) => ({
  items: ids.map((id) => ({ id })),
  nextCursor,
  catalogVersion,
})
const flush = () => new Promise((r) => setTimeout(r))

afterEach(() => {
  vi.unstubAllGlobals()
  explorer.fetchParams = { ...DEFAULT_FETCH }
  explorer.view = { ...DEFAULT_VIEW }
  explorer.dataset = []
  explorer.datasetComplete = false
  explorer.listState = 'idle'
  explorer.listError = null
})

// Returns the request URLs plus per-call deferreds: request N gets deferreds[N-1].
function stubFetchQueue(deferreds) {
  const urls = []
  vi.stubGlobal('fetch', vi.fn((url) => {
    urls.push(String(url))
    return deferreds[urls.length - 1].promise
  }))
  return urls
}

describe('window fetch races', () => {
  it('a fetch-param change while an earlier fetch is pending wins; the older response is discarded', async () => {
    const slow = deferred()
    const fast = deferred()
    const urls = stubFetchQueue([slow, fast])

    const oldRequest = loadDataset() // boot/prior-params request, resolves late
    setFetchParam({ archived: 'only' }) // supersedes it and issues request 2

    expect(urls).toHaveLength(2)
    expect(urls[1]).toContain('archived=only')

    fast.resolve(okJson(page(['new'], 2)))
    await flush()
    expect(explorer.dataset.map((i) => i.id)).toEqual(['new'])
    expect(explorer.datasetComplete).toBe(true)

    slow.resolve(okJson(page(['stale'], 1)))
    await oldRequest
    expect(explorer.dataset.map((i) => i.id)).toEqual(['new'])
  })

  it('a stale failure cannot flip a newer successful dataset into an error state', async () => {
    const slow = deferred()
    const fast = deferred()
    stubFetchQueue([slow, fast])

    const oldRequest = loadDataset()
    setFetchParam({ includeSubagents: true })

    fast.resolve(okJson(page(['kept'], 3)))
    await flush()

    slow.reject(new Error('boom'))
    await oldRequest
    expect(explorer.listState).toBe('idle')
    expect(explorer.listError).toBeNull()
    expect(explorer.dataset.map((i) => i.id)).toEqual(['kept'])
  })

  it('the window fetch follows cursors to completion and a mid-loop param change aborts the rest', async () => {
    const page1 = deferred()
    const page2 = deferred()
    const fresh = deferred()
    const urls = stubFetchQueue([page1, page2, fresh])

    const oldRequest = loadDataset()
    page1.resolve(okJson(page(['a'], 5, 'cursor-1')))
    await flush()
    expect(explorer.dataset.map((i) => i.id)).toEqual([])
    expect(explorer.datasetComplete).toBe(false)
    expect(urls[1]).toContain('cursor=cursor-1')

    setFetchParam({ archived: 'exclude' }) // supersedes the in-flight page 2
    page2.resolve(okJson(page(['b'], 5)))
    await oldRequest
    // The superseded loop must not have appended page 2 or marked complete.
    expect(explorer.dataset.map((i) => i.id)).toEqual([])

    fresh.resolve(okJson(page(['c'], 6)))
    await flush()
    expect(explorer.dataset.map((i) => i.id)).toEqual(['c'])
    expect(explorer.datasetComplete).toBe(true)
  })

  it('keeps the last complete window visible until every page of a refresh has arrived', async () => {
    explorer.dataset = [{ id: 'old-a' }, { id: 'old-b' }]
    explorer.datasetComplete = true
    const page1 = deferred()
    const page2 = deferred()
    stubFetchQueue([page1, page2])

    const refresh = loadDataset()
    page1.resolve(okJson(page(['new-a'], 8, 'cursor-1')))
    await flush()

    // Counts and filters derive directly from dataset. Publishing page one
    // here would make every sidebar count collapse while the refresh runs.
    expect(explorer.dataset.map((i) => i.id)).toEqual(['old-a', 'old-b'])
    expect(explorer.datasetComplete).toBe(false)

    page2.resolve(okJson(page(['new-b'], 8)))
    await refresh
    expect(explorer.dataset.map((i) => i.id)).toEqual(['new-a', 'new-b'])
    expect(explorer.datasetComplete).toBe(true)
  })
})
