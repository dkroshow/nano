// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render } from '@testing-library/svelte'
import StreamingAgentMessage from './StreamingAgentMessage.svelte'
import { messageBodyHtml } from './message_body.js'

let frameNow
let frameID
let frames

function advance(ms = 16) {
  frameNow += ms
  const pending = [...frames.values()]
  frames.clear()
  for (const callback of pending) callback(frameNow)
}

function renderedText(view) {
  return view.container.querySelector('.md-body').textContent.replace(/\n$/, '')
}

beforeEach(() => {
  frameNow = performance.now()
  frameID = 1
  frames = new Map()
  vi.stubGlobal('requestAnimationFrame', (callback) => {
    const id = frameID++
    frames.set(id, callback)
    return id
  })
  vi.stubGlobal('cancelAnimationFrame', (id) => frames.delete(id))
  vi.stubGlobal('matchMedia', () => ({
    matches: false,
    addEventListener() {},
    removeEventListener() {},
  }))
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
})

afterEach(() => {
  cleanup()
  delete document.visibilityState
  vi.unstubAllGlobals()
})

describe('streaming agent message', () => {
  it('shows cached text immediately, then smooths later growth', async () => {
    const view = render(StreamingAgentMessage, { text: 'Cached answer', smooth: true })
    const full = `Cached answer ${'x'.repeat(200)}`
    await view.rerender({ text: full, smooth: true })

    expect(renderedText(view)).toBe('Cached answer')
    frameNow = performance.now()
    advance(16)
    expect(renderedText(view).startsWith('Cached answer ')).toBe(true)
    expect(renderedText(view).length).toBeLessThan(full.length)

    while (frames.size) advance(40)
    expect(renderedText(view)).toBe(full)
  })

  it('paces the initial batch when the row was created live', () => {
    const full = 'First live batch should not flash in whole'
    const view = render(StreamingAgentMessage, {
      text: full,
      smooth: true,
      animateInitial: true,
      provider: 'codex',
    })

    expect(renderedText(view)).toBe('')
    frameNow = performance.now()
    advance(40)
    expect(renderedText(view).length).toBeGreaterThan(0)
    expect(renderedText(view).length).toBeLessThan(full.length)
  })

  it('snaps the current truth when the pane stops being the live tail', async () => {
    const view = render(StreamingAgentMessage, { text: 'Start', smooth: true })
    const full = `Start ${'backlog '.repeat(30)}`
    await view.rerender({ text: full, smooth: true })
    expect(renderedText(view)).not.toBe(full)

    await view.rerender({ text: full, smooth: false })

    expect(renderedText(view)).toBe(full)
    expect(frames.size).toBe(0)
  })

  it('converges to the existing Markdown renderer on completion', async () => {
    const partial = '## Result\n\n```js\nconst value ='
    const full = `${partial} true\n\`\`\``
    const view = render(StreamingAgentMessage, { text: partial, smooth: true })
    await view.rerender({ text: full, smooth: true })
    await view.rerender({ text: full, smooth: true, complete: true })
    frameNow = performance.now()
    while (frames.size) advance(40)

    expect(view.container.querySelector('.md-body').innerHTML).toBe(messageBodyHtml('agent', full))
  })

  it('canonicalizes unchanged text when completion arrives after displacement', async () => {
    const text = '## Complete'
    const view = render(StreamingAgentMessage, { text, smooth: true })
    expect(view.container.querySelector('.md-body').innerHTML).not.toBe(messageBodyHtml('agent', text))

    await view.rerender({ text, smooth: false, complete: true })

    expect(view.container.querySelector('.md-body').innerHTML).toBe(messageBodyHtml('agent', text))
  })

  it('shows a new live batch immediately when reduced motion is enabled', () => {
    vi.stubGlobal('matchMedia', () => ({
      matches: true,
      addEventListener() {},
      removeEventListener() {},
    }))
    const text = 'No animated replay'
    const view = render(StreamingAgentMessage, {
      text,
      smooth: true,
      animateInitial: true,
    })

    expect(renderedText(view)).toBe(text)
    expect(frames.size).toBe(0)
  })

  it('snaps pending text when the page becomes hidden', async () => {
    const view = render(StreamingAgentMessage, { text: 'Start', smooth: true })
    const full = `Start ${'pending '.repeat(30)}`
    await view.rerender({ text: full, smooth: true })
    expect(renderedText(view)).not.toBe(full)

    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
    document.dispatchEvent(new Event('visibilitychange'))

    expect(renderedText(view)).toBe(full)
    expect(frames.size).toBe(0)
  })
})
