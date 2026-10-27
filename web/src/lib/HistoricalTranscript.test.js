// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render } from '@testing-library/svelte'
import HistoricalTranscript from './HistoricalTranscript.svelte'
import { __resetTranscriptForTest, transcript } from '../transcript_store.svelte.js'

let observer
let resizeObservers
let originalGetRect
class FakeIntersectionObserver {
  constructor(callback, options) { this.callback = callback; this.options = options; observer = this }
  observe(node) { this.node = node }
  unobserve() {}
  disconnect() {}
}

beforeEach(() => { observer = null; resizeObservers = []; originalGetRect = HTMLElement.prototype.getBoundingClientRect })
afterEach(() => { HTMLElement.prototype.getBoundingClientRect = originalGetRect; cleanup(); __resetTranscriptForTest(); vi.unstubAllGlobals(); observer = null; resizeObservers = [] })

describe('HistoricalTranscript retained message hydration', () => {
  it('paints a preview, then hydrates an intersecting message in the same bubble', async () => {
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
    vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, json: async () => ({ itemId: 'i1', projectionKey: 'p1', contentRef: 'r1', text: 'full retained text' }) }))
    transcript.threadId = 'nth_1'; transcript.state = 'ready'; transcript.projection = { key: 'p1' }
    transcript.items = [{ id: 'i1', projectedSeq: 1, type: 'user_message', category: 'messages', text: 'preview text', contentRef: 'r1', originalBytes: 9000 }]
    const view = render(HistoricalTranscript)
    expect(view.getByText('preview text')).toBeTruthy()
    await vi.waitFor(() => expect(observer?.node?.dataset.itemId).toBe('i1'))
    expect(observer.options.root).toBe(view.getByTestId('historical-transcript-scroll'))
    expect(observer.options.rootMargin).toBe('100% 0px')
    observer.callback([{ isIntersecting: true, target: observer.node }])
    await vi.waitFor(() => expect(view.getByText('full retained text')).toBeTruthy())
    expect(fetch).toHaveBeenCalledTimes(1)
		const bubble = view.getByTestId('transcript-item').querySelector('.history-bubble')
		expect(bubble.textContent).toContain('full retained text')
		expect(bubble.textContent).not.toContain('preview text')
		expect(view.queryByText('Message truncated')).toBeNull()
  })

  it('does not auto-fetch tools, keeps failure previews retryable, and labels retained truncation', async () => {
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
		class HydratableResizeObserver {
			constructor() { resizeObservers.push(this) }
			observe() {}
			disconnect() {}
		}
    vi.stubGlobal('ResizeObserver', HydratableResizeObserver)
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 503, json: async () => ({ error: { message: 'body unavailable' } }) })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ itemId: 'u', projectionKey: 'p1', contentRef: 'ru', text: 'prefix', truncated: true, storedBytes: 1048576, originalBytes: 2000000 }) }))
    transcript.threadId = 'nth_1'; transcript.state = 'ready'; transcript.projection = { key: 'p1' }
    transcript.items = [
      { id: 'u', projectedSeq: 1, type: 'user_message', category: 'messages', text: 'preview', contentRef: 'ru' },
      { id: 'tool', projectedSeq: 2, type: 'tool_result', category: 'toolResult', text: 'tool preview', contentRef: 'rt' },
    ]
    const view = render(HistoricalTranscript)
    await vi.waitFor(() => expect(observer?.node?.dataset.itemId).toBe('u'))
		await vi.waitFor(() => expect(resizeObservers).toHaveLength(1))
    observer.callback([{ isIntersecting: true, target: observer.node }])
    await vi.waitFor(() => expect(view.getByText('body unavailable')).toBeTruthy())
    expect(fetch).toHaveBeenCalledTimes(1)
		expect(view.getByText('Preview remains because the full message failed to load.')).toBeTruthy()
		expect(view.queryByText(/retained message content is loading automatically/)).toBeNull()
    await view.getByText('Retry full message').click()
    await vi.waitFor(() => expect(view.getByText(/Nano retained only the first/)).toBeTruthy())
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(view.getByText('tool preview')).toBeTruthy()
		expect(view.getByText(/Output truncated/)).toBeTruthy()
  })

	it('re-evaluates automatic hydration when a tail update adds a body reference', async () => {
		vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
		vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, json: async () => ({ itemId: 'late', projectionKey: 'p1', contentRef: 'r-late', text: 'late full body' }) }))
		transcript.threadId = 'nth_1'; transcript.state = 'ready'; transcript.projection = { key: 'p1' }
		transcript.items = [{ id: 'late', projectedSeq: 1, type: 'agent_message', category: 'messages', text: 'late preview' }]
		const view = render(HistoricalTranscript)
		await Promise.resolve()
		expect(observer).toBeNull()
		transcript.items = [{ id: 'late', projectedSeq: 1, type: 'agent_message', category: 'messages', text: 'late preview', contentRef: 'r-late' }]
		await vi.waitFor(() => expect(observer?.node?.dataset.itemId).toBe('late'))
		observer.callback([{ isIntersecting: true, target: observer.node }])
		await vi.waitFor(() => expect(view.getByText('late full body')).toBeTruthy())
	})

  it('anchors only a hydrated article that was fully above the viewport', async () => {
    const rects = new WeakMap()
    HTMLElement.prototype.getBoundingClientRect = function () {
      return rects.get(this) ?? { top: 0, bottom: 0, height: 0 }
    }
    class FakeResizeObserver {
      constructor(callback) { this.callback = callback; resizeObservers.push(this) }
      observe(node) { this.node = node }
      disconnect() {}
      fire() { this.callback([{ target: this.node }]) }
    }
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
    vi.stubGlobal('ResizeObserver', FakeResizeObserver)
    transcript.threadId = 'nth_1'; transcript.state = 'ready'; transcript.projection = { key: 'p1' }
    transcript.items = [{ id: 'i1', projectedSeq: 1, type: 'user_message', category: 'messages', text: 'preview', contentRef: 'r1' }]
    const view = render(HistoricalTranscript)
    const scroll = view.getByTestId('historical-transcript-scroll')
    const article = view.getByTestId('transcript-item')
    rects.set(scroll, { top: 0, bottom: 300, height: 300 })
    rects.set(article, { top: -50, bottom: -10, height: 40 })
    // Re-render action state with its initial above-viewport geometry.
    cleanup()
    const second = render(HistoricalTranscript)
    const scroll2 = second.getByTestId('historical-transcript-scroll')
    const article2 = second.getByTestId('transcript-item')
    rects.set(scroll2, { top: 0, bottom: 300, height: 300 })
    rects.set(article2, { top: -50, bottom: -10, height: 40 })
    await vi.waitFor(() => expect(resizeObservers.length).toBeGreaterThan(0))
    const resize = resizeObservers.at(-1)
    scroll2.scrollTop = 200
    rects.set(article2, { top: -50, bottom: 50, height: 100 }); resize.fire()
    expect(scroll2.scrollTop).toBe(260)
    // First establish an overlapping baseline without growth, then grow: no shift.
    rects.set(article2, { top: -10, bottom: 90, height: 100 }); resize.fire()
    rects.set(article2, { top: -10, bottom: 150, height: 160 }); resize.fire()
    expect(scroll2.scrollTop).toBe(260)
  })
})
