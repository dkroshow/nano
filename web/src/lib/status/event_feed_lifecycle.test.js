// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte'
import EventFeed from './EventFeed.svelte'
import { dropSessionStore, getSessionStore } from '../../state.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    this.closed = false
    MockEventSource.instances.push(this)
  }

  close() {
    this.closed = true
  }
}
MockEventSource.instances = []

beforeEach(() => {
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200, json: async () => ({}), text: async () => '' })))
})

afterEach(() => {
  cleanup()
  dropSessionStore('feed-lifecycle')
  vi.unstubAllGlobals()
})

describe('raw event feed lifecycle', () => {
  it('closes its session stream when the feed tab unmounts', () => {
    const session = getSessionStore('feed-lifecycle', { provider: 'codex', threadId: 'native-feed' })
    const view = render(EventFeed, { session })
    const feed = MockEventSource.instances.find((source) => source.url.endsWith('/api/sessions/feed-lifecycle/events'))

    expect(feed).toBeDefined()
    view.unmount()
    expect(feed.closed).toBe(true)
  })
})

describe('raw event feed paging', () => {
  // jsdom lays nothing out, so scrollTop/scrollHeight are stubbed to place the
  // reader deliberately. Writable: the feed pins scrollTop after a page lands.
  function scrollTo(element, top) {
    for (const [key, value] of [['scrollTop', top], ['scrollHeight', 4000], ['clientHeight', 600]]) {
      Object.defineProperty(element, key, { value, configurable: true, writable: true })
    }
    return fireEvent.scroll(element)
  }

  function feedSession(events) {
    const session = getSessionStore('feed-lifecycle', { provider: 'codex', threadId: 'native-feed' })
    session.store.events = events
    session.store.eventsHasOlder = true
    return session
  }

  const laterEvents = [
    { seq: 40, ts: '2026-08-07T00:00:00.000Z', dir: 'send', kind: 'call', label: 'turn', preview: '', msg: {} },
  ]

  it('pages in older events when the reader scrolls to the top', async () => {
    const pages = []
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      pages.push(String(url))
      return { ok: true, status: 200, json: async () => ({ events: [], hasMore: false }), text: async () => '' }
    }))
    const session = feedSession(laterEvents)
    const { container } = render(EventFeed, { session })
    const scroller = container.querySelector('.feed')

    await scrollTo(scroller, 4000 - 600)
    expect(pages.some((url) => url.includes('/events/page'))).toBe(false)

    await scrollTo(scroller, 10)
    await waitFor(() => expect(pages.some((url) => url.includes('beforeSeq=40'))).toBe(true))
    expect(container.querySelector('.load-older')).toBeNull()
  })

  it('stops paging after a failure and offers a retry instead', async () => {
    let attempts = 0
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      if (!String(url).includes('/events/page')) return { ok: true, status: 200, json: async () => ({}), text: async () => '' }
      attempts += 1
      return { ok: false, status: 500, json: async () => ({}), text: async () => 'spool unreadable' }
    }))
    const session = feedSession(laterEvents)
    const { container, getByRole } = render(EventFeed, { session })
    const scroller = container.querySelector('.feed')

    await scrollTo(scroller, 0)
    await waitFor(() => expect(attempts).toBe(1))

    // A second scroll must not re-fire against a failing endpoint.
    await scrollTo(scroller, 0)
    expect(attempts).toBe(1)

    await fireEvent.click(getByRole('button', { name: 'Retry' }))
    await waitFor(() => expect(attempts).toBe(2))
  })
})
