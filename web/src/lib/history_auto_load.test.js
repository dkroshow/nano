// @vitest-environment jsdom
//
// Earlier history arrives by scrolling, not by clicking. The threshold itself
// is unit-tested in transcript_view.test.js; these tests pin the wiring — that
// a scroll event near the top of the real rendered pane reaches the pager, and
// that the old manual button is gone from the surface.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte'
import Conversation from './Conversation.svelte'
import HistoricalThreadView from './HistoricalThreadView.svelte'
import { tick } from 'svelte'

function historyChannel({ hasMoreBefore = true, loadEarlier = vi.fn(async () => {}) } = {}) {
  return {
    state: {
      threadId: 'thread-1',
      state: 'ready',
      items: [
        { id: 'a', type: 'user_message', role: 'user', text: 'first', createdAt: '2026-08-07T00:00:00Z' },
        { id: 'b', type: 'agent_message', text: 'second', createdAt: '2026-08-07T00:00:01Z' },
      ],
      turns: [],
      page: { hasMoreBefore, beforeCursor: hasMoreBefore ? 'cursor-1' : null },
      loadingEarlier: false,
      earlierError: null,
    },
    loadEarlier,
    revalidateTail: vi.fn(),
  }
}

// jsdom lays nothing out, so scrollTop stays 0 and every scroll event reads as
// "at the top". Height is stubbed only where a test needs the reader to be
// somewhere else.
function scrollTo(element, top) {
  // Writable on purpose: the pane pins scrollTop itself after a page lands.
  Object.defineProperty(element, 'scrollTop', { value: top, configurable: true, writable: true })
  Object.defineProperty(element, 'scrollHeight', { value: 4000, configurable: true, writable: true })
  Object.defineProperty(element, 'clientHeight', { value: 600, configurable: true, writable: true })
  return fireEvent.scroll(element)
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('historical thread pane', () => {
  it('pages in earlier history when the reader scrolls to the top', async () => {
    const history = historyChannel()
    const { container } = render(HistoricalThreadView, { history, visible: true })
    const scroller = container.querySelector('.pane-transcript')

    await scrollTo(scroller, 4000 - 600)
    expect(history.loadEarlier).not.toHaveBeenCalled()

    await scrollTo(scroller, 10)
    await waitFor(() => expect(history.loadEarlier).toHaveBeenCalledTimes(1))
  })

  it('offers no manual load button, and stops paging once the server has no earlier page', async () => {
    const history = historyChannel({ hasMoreBefore: false })
    const { container, queryByRole } = render(HistoricalThreadView, { history, visible: true })

    expect(queryByRole('button', { name: 'Show earlier history' })).toBeNull()
    await scrollTo(container.querySelector('.pane-transcript'), 0)
    expect(history.loadEarlier).not.toHaveBeenCalled()
  })

  it('stops paging after a failure and offers a retry instead', async () => {
    const history = historyChannel()
    history.state.earlierError = 'cursor rejected'
    const { container, getByRole } = render(HistoricalThreadView, { history, visible: true })

    await scrollTo(container.querySelector('.pane-transcript'), 0)
    expect(history.loadEarlier).not.toHaveBeenCalled()

    await fireEvent.click(getByRole('button', { name: 'Retry' }))
    expect(history.loadEarlier).toHaveBeenCalledTimes(1)
  })
})

describe('live thread pane', () => {
  async function livePane({ hasMoreBefore = true } = {}) {
    const sources = []
    vi.stubGlobal('EventSource', class {
      constructor(url) { this.url = url; sources.push(this) }
      close() {}
    })
    const fetchMock = vi.fn(async () => ({ ok: true, json: async () => ({ items: [], turns: [], page: { hasMoreBefore: false } }) }))
    vi.stubGlobal('fetch', fetchMock)
    const session = {
      id: 'session-1',
      store: {
        sessionStatus: 'ready', provider: 'claude', threadId: 'native-1',
        pendingInteractions: [], pendingSends: [], turnOptions: {}, meta: {},
      },
    }
    const view = render(Conversation, { session, info: { status: 'ready' }, visible: true })
    const source = sources.find((entry) => entry.url.includes('/live?'))
    source.onmessage({ data: JSON.stringify({
      type: 'tail', threadId: 'thread-1', rows: [
        { id: 'a', type: 'user_message', role: 'user', text: 'first', projectedSeq: 1 },
      ], live: [], turns: [], projection: { state: 'ready' },
      page: { hasMoreBefore, nextBeforeCursor: hasMoreBefore ? 'cursor-1' : null },
    }) })
    await tick()
    return { ...view, pageCalls: () => fetchMock.mock.calls.filter(([url]) => String(url).includes('/transcript?')) }
  }

  it('pages ordinary saved history when the reader scrolls to the top', async () => {
    const { container, pageCalls } = await livePane()
    const scroller = container.querySelector('.convo')
    await scrollTo(scroller, 4000 - 600)
    expect(pageCalls()).toHaveLength(0)
    await scrollTo(scroller, 10)
    await waitFor(() => expect(pageCalls()).toHaveLength(1))
    expect(String(pageCalls()[0][0])).toContain('/threads/thread-1/transcript?')
    expect(String(pageCalls()[0][0])).toContain('before=cursor-1')
    expect(container.querySelector('.load-older')).toBeNull()
  })

  it('leaves the pager alone once the server has no earlier page', async () => {
    const { container, pageCalls } = await livePane({ hasMoreBefore: false })
    await scrollTo(container.querySelector('.convo'), 0)
    expect(pageCalls()).toHaveLength(0)
  })
})
