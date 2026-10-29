// @vitest-environment jsdom

import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { app } from '../../app.svelte.js'
import { dropSessionStore } from '../../state.svelte.js'
import { projectionItem, projectionSnapshot, sendProjection, sendProjectionSnapshot } from '../../test_projection.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    this.closed = false
    MockEventSource.instances.push(this)
  }
  close() { this.closed = true }
}
MockEventSource.instances = []

const emptyTranscript = {
  items: [],
  turns: [],
  page: { hasMoreBefore: false, beforeCursor: null },
  projection: { state: 'ready', epoch: 'tpe_1' },
}

const liveSession = {
  id: 'session-1',
  hostedThreadId: 'nht-session-1',
  provider: 'codex',
  threadId: 'native-1',
  cwd: '/workspace/nano',
  model: 'gpt-5.6-sol',
  status: 'ready',
  turnActive: false,
}

const tab = {
  id: 1,
  catalogId: 'nth_1',
  sessionId: 'session-1',
  provider: 'codex',
  nativeId: 'native-1',
  title: 'Projection',
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

function conversationText(container) {
  return container.querySelector('.convo')?.textContent ?? ''
}

beforeEach(() => {

  vi.stubGlobal('EventSource', MockEventSource)
  MockEventSource.instances = []
  app.sessions = [liveSession]
  // Every visible pane reads its own file stream.
  localStorage.setItem('nano-view', 'all')
  vi.stubGlobal('fetch', vi.fn(async (url) => {
    const target = String(url)
    if (target.includes('/transcript')) return jsonResponse(emptyTranscript)
    if (target.endsWith('/state')) return jsonResponse({ ...liveSession, sessionId: liveSession.id })
    if (target.includes('/conversation')) return jsonResponse(projectionSnapshot())
    return jsonResponse({})
  }))
})

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')

  vi.unstubAllGlobals()
})

describe('file transcript conversation transport', () => {
	it('renders a durable-backfilled prompt from a fresh restart snapshot', async () => {
		const snapshot = projectionSnapshot({
			items: [
				projectionItem({ id: 'durable:prompt-row', type: 'userMessage', text: 'recovered agent prompt', durableItemID: 'prompt-row', liveSeq: 1 }),
				projectionItem({ id: 'agent-row', type: 'agentMessage', text: 'continuing work', durableItemID: 'agent-row', liveSeq: 2 }),
			],
		})
		fetch.mockImplementation(async (url) => {
			const target = String(url)
			if (target.endsWith('/state')) return jsonResponse({ ...liveSession, sessionId: liveSession.id })
			if (target.includes('/transcript')) return jsonResponse(emptyTranscript)
			if (target.includes('/conversation')) return jsonResponse(snapshot)
			return jsonResponse({})
		})

		const { container } = render(ThreadPaneView, { tab })
		sendProjectionSnapshot(MockEventSource.instances.find((source) => source.url.includes('/live?')), snapshot)
		await waitFor(() => expect(conversationText(container)).toContain('recovered agent prompt'))
		expect(conversationText(container).match(/recovered agent prompt/g)).toHaveLength(1)
	})

  it('opens the file stream in the normal Enhanced view', async () => {
    localStorage.setItem('nano-view', 'clean')
    render(ThreadPaneView, { tab })
    await new Promise((resolve) => setTimeout(resolve, 0))

    expect(MockEventSource.instances.some((source) => source.url.includes('/live?'))).toBe(true)
  })

  it('does not open a projection stream for a hidden pane', async () => {
    render(ThreadPaneView, { tab, visible: false })
    await new Promise((resolve) => setTimeout(resolve, 0))

    expect(MockEventSource.instances.some((source) => source.url.includes('/live?'))).toBe(false)
  })

  it('renders saved rows and updates without raw provider events', async () => {
    const snapshot = projectionSnapshot({
      items: [projectionItem({ id: 'snapshot-answer', type: 'agentMessage', text: 'snapshot answer' })],
    })
    fetch.mockImplementation(async (url) => {
      const target = String(url)
      if (target.includes('/transcript')) return jsonResponse(emptyTranscript)
      if (target.endsWith('/state')) return jsonResponse({ ...liveSession, sessionId: liveSession.id })
      if (target.includes('/conversation')) return jsonResponse(snapshot)
      return jsonResponse({})
    })

    const { container } = render(ThreadPaneView, { tab })
    sendProjectionSnapshot(MockEventSource.instances.find((source) => source.url.includes('/live?')), snapshot)
    await waitFor(() => expect(conversationText(container)).toContain('snapshot answer'))

    const projection = MockEventSource.instances.find((source) => source.url.includes('/live?'))
    sendProjection(projection, {
      item: projectionItem({ id: 'live-answer', type: 'agentMessage', text: 'typed update' }),
    })
    await waitFor(() => expect(conversationText(container)).toContain('typed update'))

    expect(MockEventSource.instances.some((source) => source.url.endsWith('/api/sessions/session-1/events'))).toBe(false)
  })

  it('paces growth of the visible live assistant row', async () => {
    let frameID = 0
    let frameAt = performance.now()
    const frames = new Map()
    vi.stubGlobal('requestAnimationFrame', (callback) => {
      const id = ++frameID
      frames.set(id, callback)
      return id
    })
    vi.stubGlobal('cancelAnimationFrame', (id) => frames.delete(id))
    const { container } = render(ThreadPaneView, { tab })
    const projection = MockEventSource.instances.find((source) => source.url.includes('/live?'))
    await tick()
    const start = 'Visible start'
    const full = `${start} ${'stream '.repeat(35)}`

    sendProjection(projection, {
      item: projectionItem({ id: 'smooth-answer', text: start, done: false }),
    })
    await tick()
    expect(conversationText(container)).toContain(start)
    frameAt = performance.now()
    for (let index = 0; index < 100 && !conversationText(container).includes(start); index += 1) {
      frameAt += 40
      const pending = [...frames.values()]
      frames.clear()
      for (const callback of pending) callback(frameAt)
    }
    expect(conversationText(container)).toContain(start)

    sendProjection(projection, {
      item: projectionItem({ id: 'smooth-answer', text: full, done: false }),
    })
    await tick()
    expect(conversationText(container)).not.toContain(full)

    frameAt = performance.now()
    for (let index = 0; index < 100 && !conversationText(container).includes(full); index += 1) {
      frameAt += 40
      const pending = [...frames.values()]
      frames.clear()
      for (const callback of pending) callback(frameAt)
    }
    expect(conversationText(container)).toContain(full)

    const completed = `${full}finished.`
    sendProjection(projection, {
      op: 'item.complete',
      item: projectionItem({ id: 'smooth-answer', text: completed, done: true }),
    })
    await tick()
    expect(conversationText(container)).not.toContain(completed)
    for (let index = 0; index < 100 && !conversationText(container).includes(completed); index += 1) {
      frameAt += 40
      const pending = [...frames.values()]
      frames.clear()
      for (const callback of pending) callback(frameAt)
    }
    expect(conversationText(container)).toContain(completed)
  })

  it('does not replay a live partial when a second pane mounts the same thread', async () => {
    let frameID = 0
    const frames = new Map()
    vi.stubGlobal('requestAnimationFrame', (callback) => {
      const id = ++frameID
      frames.set(id, callback)
      return id
    })
    vi.stubGlobal('cancelAnimationFrame', (id) => frames.delete(id))
    const first = render(ThreadPaneView, { tab })
    const projection = MockEventSource.instances.find((source) => source.url.includes('/live?'))
    await tick()
    const partial = 'This partial belongs to the already-mounted pane.'

    sendProjection(projection, {
      item: projectionItem({ id: 'shared-live-answer', text: partial, done: false }),
    })
    await tick()
    expect(conversationText(first.container)).toContain(partial)

    const second = render(ThreadPaneView, { tab: { ...tab, id: 2 } })
    const secondStream = MockEventSource.instances.filter((source) => source.url.includes('/live?')).at(-1)
    sendProjectionSnapshot(secondStream, { items: [projectionItem({ id: 'shared-live-answer', text: partial, done: false })] })
    await tick()

    expect(conversationText(second.container)).toContain(partial)
  })

  it('snaps a lagging row before rendering a later transcript item', async () => {
    let frameID = 0
    let frameAt = performance.now()
    const frames = new Map()
    vi.stubGlobal('requestAnimationFrame', (callback) => {
      const id = ++frameID
      frames.set(id, callback)
      return id
    })
    vi.stubGlobal('cancelAnimationFrame', (id) => frames.delete(id))
    const { container } = render(ThreadPaneView, { tab })
    const projection = MockEventSource.instances.find((source) => source.url.includes('/live?'))
    const start = 'Opening sentence.'
    const full = `${start} ${'lagging '.repeat(35)}`

    sendProjection(projection, {
      item: projectionItem({ id: 'first-live-answer', text: start, done: false }),
    })
    await tick()
    for (let index = 0; index < 100 && !conversationText(container).includes(start); index += 1) {
      frameAt += 40
      const pending = [...frames.values()]
      frames.clear()
      for (const callback of pending) callback(frameAt)
    }
    sendProjection(projection, {
      item: projectionItem({ id: 'first-live-answer', text: full, done: false }),
    })
    await tick()
    expect(conversationText(container)).not.toContain(full)

    sendProjection(projection, {
      item: projectionItem({ id: 'later-answer', text: 'Later row', done: false }),
    })
    await tick()

    const rows = [...container.querySelectorAll('.item.agentMessage')]
    expect(rows).toHaveLength(2)
    expect(rows[0].textContent).toContain(full)
  })

})
