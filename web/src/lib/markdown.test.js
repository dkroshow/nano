// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { marked } from 'marked'
import { esc, boxTableToHtml, md } from './markdown.js'

afterEach(() => {
  vi.restoreAllMocks()
  document.body.replaceChildren()
})

describe('markdown structure renders', () => {
  it('bold', () => {
    expect(md('this is **important** stuff')).toContain('<strong>important</strong>')
  })

  it('lists', () => {
    const html = md('- one\n- two')
    expect(html).toContain('<ul>')
    expect(html).toContain('<li>one</li>')
    expect(html).toContain('<li>two</li>')
  })

  it('headings', () => {
    expect(md('## Section')).toContain('<h2>Section</h2>')
  })

  it('inline code becomes a code element', () => {
    expect(md('run `npm test` now')).toContain('<code>npm test</code>')
  })
})

describe('box-drawing handling', () => {
  it('auto-fences runs of box-drawing characters as code', () => {
    const html = md('before\n╭───╮\n│ x │\n╰───╯\nafter')
    expect(html).toMatch(/<pre><code>[^]*╭───╮/)
    expect(html).toContain('<p>before</p>')
    expect(html).toContain('<p>after</p>')
  })

  it('keeps an already-fenced tree in one code block, including the root label', () => {
    const src = [
      '```text',
      'Project',
      '└── Orchestration',
      '    ├── Understand the portfolio',
      '    │   └── Maintain the Intent Tree',
      '    └── Feed results back',
      '```',
    ].join('\n')
    const html = md(src)
    expect(html).toContain('<pre><code class="language-text">')
    expect(html).toContain('Project\n└── Orchestration\n    ├── Understand the portfolio\n    │   └── Maintain the Intent Tree')
    expect(html).not.toMatch(/<p>[^<]*└──/)
    expect(html.match(/<pre><code/g)).toHaveLength(1)
  })

  it('does not break a tilde-fenced box-drawing block', () => {
    const html = md('~~~\nProject\n│   └── child\n~~~')
    expect(html).toContain('Project\n│   └── child')
    expect(html).not.toMatch(/<p>[^<]*│/)
  })

  it('leaves a fenced box-drawing table as code instead of converting it', () => {
    const html = md('```\n┌────┬────┐\n│ a  │ b  │\n├────┼────┤\n│ 1  │ 2  │\n└────┴────┘\n```')
    expect(html).toContain('<pre><code>')
    expect(html).toContain('┌────┬────┐')
    expect(html).not.toContain('box-table')
  })

  it('converts box-drawing tables via boxTableToHtml', () => {
    const html = md('┌────┬────┐\n│ a  │ b  │\n├────┼────┤\n│ 1  │ 2  │\n└────┴────┘')
    expect(html).toContain('<div class="box-table-wrap"><table class="box-table">')
    expect(html).toContain('<thead><tr><th>a</th><th>b</th></tr></thead>')
    expect(html).toContain('<tr><td>1</td><td>2</td></tr>')
  })

  it('boxTableToHtml escapes cell content', () => {
    const html = boxTableToHtml(['┌────┐', '│ <b> │', '└────┘'])
    expect(html).toContain('<th>&lt;b&gt;</th>')
  })
})

describe('tables and links', () => {
  it('wraps pipe tables in .md-table-wrap', () => {
    const html = md('| a | b |\n|---|---|\n| 1 | 2 |')
    expect(html).toContain('<div class="md-table-wrap"><table>')
    expect(html).toContain('</table></div>')
  })

  it('links open in a new tab with noopener', () => {
    const html = md('see [docs](https://example.com "the docs")')
    expect(html).toContain('href="https://example.com"')
    expect(html).toContain('target="_blank"')
    expect(html).toContain('rel="noopener noreferrer"')
    expect(html).toContain('title="the docs"')
    expect(html).toContain('>docs</a>')
  })

  // An <img> would make the operator's browser fetch an agent-written URL;
  // the page makes no such request, so an image renders as a link.
  it('renders an image as a link and never an <img>', () => {
    const root = rendered('![diagram](https://example.com/d.png "the diagram")')
    expect(root.querySelector('img')).toBeNull()
    const link = root.querySelector('a')
    expect(link.getAttribute('href')).toBe('https://example.com/d.png')
    expect(link.getAttribute('target')).toBe('_blank')
    expect(link.getAttribute('rel')).toBe('noopener noreferrer')
    expect(link.getAttribute('title')).toBe('the diagram')
    expect(link.textContent).toBe('diagram')
  })

  it('an image without alt text links with its URL as the text; other URL forms follow the link rule', () => {
    expect(rendered('![](https://example.com/d.png)').querySelector('a').textContent).toBe('https://example.com/d.png')
    const data = rendered('![a](data:image/png;base64,AAAA)')
    expect(data.querySelector('img, a')).toBeNull()
    expect(data.textContent.trim()).toBe('a')
    const relative = rendered('![a](./local.png)')
    expect(relative.querySelector('img')).toBeNull()
    expect(relative.querySelector('a.nano-file-link').getAttribute('href')).toBe('#')
  })
})

describe('link safety (nano hardening over standard — protocol filter kept from the retired transcript renderer)', () => {
  it('refuses unsafe link protocols — javascript: renders as text', () => {
    const html = md('[bad](javascript:alert(1))')
    expect(html).not.toContain('href="javascript:')
    expect(html).toContain('bad')
  })

  it('turns local filesystem paths into inert Nano file links', () => {
    const html = md('[architecture](/home/user/Code/nano/docs/lifecycle.md)')
    expect(html).toContain('class="nano-file-link"')
    expect(html).toContain('data-nano-file-path="/home/user/Code/nano/docs/lifecycle.md"')
    expect(html).not.toContain('target="_blank"')
    expect(html).not.toContain('file://')
  })

  it('keeps https and mailto links, attribute-escaped, with target/noopener', () => {
    const html = md('[ok](https://example.com/a?b=1)')
    expect(html).toContain('href="https://example.com/a?b=1"')
    expect(html).toContain('rel="noopener noreferrer"')
    expect(md('[m](mailto:kd@example.com)')).toContain('href="mailto:kd@example.com"')
  })

  it('makes explicitly authored bare and documented thread references safe same-origin links', () => {
    const native = '11111111-2222-4333-8444-555555555555'
    const html = md(`[hosted](nht_abc-123) [documented](?thread=${native})`)
    expect(html).toContain('href="?thread=nht_abc-123"')
    expect(html).toContain(`href="?thread=${native}"`)
    expect(html).toContain(`data-nano-thread-reference="${native}"`)
    expect(html).toContain('class="nano-thread-link"')
    expect(html).not.toContain('target="_blank"')
  })

  it('treats a same-origin absolute documented thread URL as a Nano link', () => {
    const hosted = 'nht_abc-123'
    const href = `${window.location.origin}/threads/view?thread=${hosted}`
    const html = md(`[open](${href})`)
    expect(html).toContain(`href="?thread=${hosted}"`)
    expect(html).toContain('class="nano-thread-link"')
    expect(html).not.toContain('target="_blank"')
  })
})

describe('fallback path', () => {
  it('falls back to .md-plain escaped text when marked.parse throws', () => {
    vi.spyOn(marked, 'parse').mockImplementation(() => {
      throw new Error('boom')
    })
    expect(md('hello <world> & "friends"')).toBe(
      '<div class="md-plain">hello &lt;world&gt; &amp; &quot;friends&quot;</div>'
    )
  })
})

function rendered(source) {
  const root = document.createElement('div')
  root.innerHTML = md(source)
  document.body.append(root)
  return root
}

// What the operator gets from selecting a rendered element and copying it.
function copied(element) {
  const selection = window.getSelection()
  selection.selectAllChildren(element)
  const text = selection.toString()
  selection.removeAllRanges()
  return text
}

describe('code shows and copies its literal characters', () => {
  // Catches code rendered as &lt;/&gt; (escaped twice), which breaks copied commands.
  it('inline code', () => {
    const code = rendered('run `ls a<b >out.txt` now').querySelector('code')
    expect(code.textContent).toBe('ls a<b >out.txt')
    expect(copied(code)).toBe('ls a<b >out.txt')
  })

  it('fenced code, where a typed entity also stays literal', () => {
    const code = rendered('```sh\ngrep "<div>" in.html > out.txt\necho &lt;\n```').querySelector('pre code')
    expect(code.textContent).toBe('grep "<div>" in.html > out.txt\necho &lt;\n')
    expect(copied(code)).toBe('grep "<div>" in.html > out.txt\necho &lt;\n')
    expect(code.children).toHaveLength(0)
  })
})

describe('agent HTML never becomes live in the rendered DOM', () => {
  const hostile = [
    ['script tag', '<script>alert(1)</script>'],
    ['img onerror', '<img src=x onerror=alert(1)>'],
    ['iframe', '<iframe src="https://example.com"></iframe>'],
    ['HTML comment', 'a <!-- <img src=x onerror=alert(1)> --> b'],
    ['line-start HTML block', '<div onclick="alert(1)">\n\n**x**\n\n</div>'],
    ['raw pre block', '<pre>\n<img/src=x onerror=alert(1)>\n</pre>'],
    ['HTML inside emphasis', '**<img src=x onerror=alert(1)>**'],
    ['HTML inside link text', '[<img src=x onerror=alert(1)>](https://example.com)'],
    ['javascript: link', '[x](javascript:alert(1))'],
    ['data: link', '[x](data:text/html,<script>alert(1)</script>)'],
    ['vbscript: link', '[x](vbscript:msgbox(1))'],
    ['javascript: autolink', '<javascript:alert(1)>'],
    ['data: autolink', '<data:text/html,alert(1)>'],
    ['vbscript: autolink', '<vbscript:msgbox(1)>'],
    ['quotes in a link title', '[x](https://example.com "a\\" onmouseover=\\"alert(1)")'],
    ['quotes in an image', '![a" onerror="alert(1)](x" onerror="alert(1))'],
    ['HTML inside a code fence', '```\n<script>alert(1)</script>\n```'],
  ]

  it.each(hostile)('%s', (_, source) => {
    const root = rendered(source)
    expect(root.querySelector('script, iframe, style, object, embed, div')).toBeNull()
    for (const element of root.querySelectorAll('*')) {
      for (const { name, value } of element.attributes) {
        expect(name).not.toMatch(/^on/i)
        if (name === 'href' || name === 'src') expect(value).not.toMatch(/^\s*(javascript|data|vbscript):/i)
      }
    }
  })

  it('shows the HTML as text', () => {
    expect(rendered('hi <script>alert(1)</script>').textContent).toBe('hi <script>alert(1)</script>\n')
  })

  it('decodes a literal entity outside code, as Markdown does', () => {
    expect(rendered('a &lt; b &amp; c').textContent).toBe('a < b & c\n')
  })
})

describe('Markdown that parsing unescaped input enables', () => {
  it('renders blockquotes', () => {
    expect(rendered('> quoted').querySelector('blockquote').textContent.trim()).toBe('quoted')
  })

  it('renders web autolinks through the hardened link renderer', () => {
    const link = rendered('<https://example.com>').querySelector('a')
    expect(link.getAttribute('href')).toBe('https://example.com')
    expect(link.getAttribute('rel')).toBe('noopener noreferrer')
  })
})

describe('esc', () => {
  it('escapes the five HTML metacharacters', () => {
    expect(esc(`<a href="x" title='y'>&</a>`)).toBe(
      '&lt;a href=&quot;x&quot; title=&#39;y&#39;&gt;&amp;&lt;/a&gt;'
    )
  })
})
