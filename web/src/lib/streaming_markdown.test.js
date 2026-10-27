// @vitest-environment jsdom

import { describe, expect, it } from 'vitest'
import { messageBodyHtml } from './message_body.js'
import { createStreamingMarkdownSurface, streamingMarkdownBlocks } from './streaming_markdown.js'

describe('streaming Markdown surface', () => {
  it('keeps completed blocks mounted while the trailing block grows', () => {
    const root = document.createElement('div')
    const surface = createStreamingMarkdownSurface(root)
    surface.replace('First paragraph.\n\nSecond', { incremental: true })
    const firstParagraph = root.querySelector('p')

    surface.append(' paragraph.')

    expect(root.querySelector('p')).toBe(firstParagraph)
    expect([...root.querySelectorAll('p')].map((node) => node.textContent)).toEqual([
      'First paragraph.',
      'Second paragraph.',
    ])
  })

  it('appends fenced code to the existing text node', () => {
    const root = document.createElement('div')
    const surface = createStreamingMarkdownSurface(root)
    surface.replace('```js\nconst first = 1', { incremental: true })
    const code = root.querySelector('code')
    const textNode = code.firstChild

    surface.append('\nconst second = 2')

    expect(root.querySelector('code')).toBe(code)
    expect(code.firstChild).toBe(textNode)
    expect(code.textContent).toBe('const first = 1\nconst second = 2\n')
  })

  it('converges to the canonical renderer on completion', () => {
    const root = document.createElement('div')
    const surface = createStreamingMarkdownSurface(root)
    const text = '## Result\n\n```js\nconst safe = true\n```'
    surface.replace(text.slice(0, -3), { incremental: true })
    surface.append('```')
    surface.finish(text)

    expect(root.innerHTML).toBe(messageBodyHtml('agent', text))
  })

  it('never treats indented code as a fenced streaming block', () => {
    expect(streamingMarkdownBlocks('    const value = 1')[0].kind).toBe('markdown')
  })

  it('keeps raw HTML inert during incremental rendering', () => {
    const root = document.createElement('div')
    const surface = createStreamingMarkdownSurface(root)
    surface.replace('<img src=x onerror=alert(1)>', { incremental: true })

    expect(root.querySelector('img')).toBeNull()
    expect(root.textContent).toContain('<img src=x onerror=alert(1)>')
  })
})
