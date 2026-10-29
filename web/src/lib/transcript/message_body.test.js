import { describe, it, expect } from 'vitest'
import { messageBodyHtml } from './message_body.js'

describe('messageBodyHtml dispatch — agent gets markdown, everyone else stays plain', () => {
  it('agent messages render markdown structure', () => {
    const html = messageBodyHtml('agent', '**bold** and `code`\n\n- item')
    expect(html).toContain('<strong>bold</strong>')
    expect(html).toContain('<code>code</code>')
    expect(html).toContain('<li>item</li>')
  })

  it('agent messages keep embedded HTML inert', () => {
    const html = messageBodyHtml('agent', 'hi <script>alert(1)</script>')
    expect(html).not.toContain('<script')
    expect(html).toContain('&lt;script&gt;alert(1)&lt;/script&gt;')
  })

  it('user messages stay escaped plain text — markdown syntax is preserved literally', () => {
    const html = messageBodyHtml('user', '**not bold** `not code`')
    expect(html).not.toContain('<strong>')
    expect(html).not.toContain('<code>')
    expect(html).toContain('**not bold** `not code`')
  })

  it('user messages escape HTML metacharacters', () => {
    expect(messageBodyHtml('user', '<img src=x onerror=alert(1)>')).toBe(
      '&lt;img src=x onerror=alert(1)&gt;'
    )
  })

  it('reasoning stays escaped plain text', () => {
    const html = messageBodyHtml('reasoning', '## thinking <b>hard</b>')
    expect(html).not.toContain('<h2>')
    expect(html).not.toContain('<b>')
    expect(html).toContain('## thinking &lt;b&gt;hard&lt;/b&gt;')
  })

  it('tolerates empty and missing text for both branches', () => {
    expect(messageBodyHtml('agent', '')).toBe('')
    expect(messageBodyHtml('agent', undefined)).toBe('')
    expect(messageBodyHtml('user', undefined)).toBe('')
    expect(messageBodyHtml('user', null)).toBe('')
  })
})
