import { marked } from 'marked'
import { messageBodyHtml } from './message_body.js'

function fencedCode(token) {
  if (token?.type !== 'code' || !/^ {0,3}(?:`{3,}|~{3,})/.test(token.raw || '')) return null
  return {
    language: String(token.lang || '').trim().split(/\s+/, 1)[0],
    text: String(token.text || ''),
  }
}

export function streamingMarkdownBlocks(source) {
  const text = String(source || '')
  try {
    let offset = 0
    return marked.lexer(text).map((token, index) => {
      const raw = String(token.raw || '')
      const code = fencedCode(token)
      const block = {
        key: `${offset}:${token.type || index}`,
        kind: code ? 'code' : 'markdown',
        source: raw,
        code,
      }
      offset += raw.length
      return block
    })
  } catch {
    return [{ key: '0:fallback', kind: 'markdown', source: text, code: null }]
  }
}

function removeBetween(block) {
  let current = block.start.nextSibling
  while (current && current !== block.end) {
    const next = current.nextSibling
    current.remove()
    current = next
  }
}

function removeBlock(block) {
  let current = block.start
  while (current) {
    const next = current.nextSibling
    current.remove()
    if (current === block.end) break
    current = next
  }
}

function insertHTML(block, html) {
  const template = block.end.ownerDocument.createElement('template')
  template.innerHTML = html
  block.end.parentNode.insertBefore(template.content, block.end)
}

function renderMarkdownBlock(block, descriptor) {
  removeBetween(block)
  insertHTML(block, messageBodyHtml('agent', descriptor.source))
}

function codeClass(language) {
  return language ? `language-${language}` : ''
}

function renderCodeBlock(block, descriptor) {
  const document = block.end.ownerDocument
  const pre = document.createElement('pre')
  const code = document.createElement('code')
  code.className = codeClass(descriptor.code.language)
  block.codeText = document.createTextNode(descriptor.code.text)
  code.append(block.codeText, document.createTextNode('\n'))
  pre.append(code)
  block.end.parentNode.insertBefore(pre, block.end)
}

function updateBlock(block, descriptor) {
  if (descriptor.kind === 'code' && block.descriptor.kind === 'code') {
    const previous = block.descriptor.code.text
    const next = descriptor.code.text
    const code = block.codeText?.parentElement
    if (code && next.startsWith(previous)) block.codeText.appendData(next.slice(previous.length))
    else {
      removeBetween(block)
      renderCodeBlock(block, descriptor)
    }
    if (code) code.className = codeClass(descriptor.code.language)
  } else if (descriptor.source !== block.descriptor.source) {
    renderMarkdownBlock(block, descriptor)
  }
  block.descriptor = descriptor
}

function mountBlock(root, descriptor) {
  const document = root.ownerDocument
  const block = {
    descriptor,
    start: document.createComment('nano-stream-block'),
    end: document.createComment('/nano-stream-block'),
    codeText: null,
  }
  root.append(block.start, block.end)
  if (descriptor.kind === 'code') renderCodeBlock(block, descriptor)
  else renderMarkdownBlock(block, descriptor)
  return block
}

// A presentation-only Markdown surface. Canonical replacements still use the
// shared renderer. Prefix growth keeps completed block DOM mounted and patches
// only the live trailing block; fenced code appends to one text node so a long
// block is never reparsed and replaced on every visual tick.
export function createStreamingMarkdownSurface(root) {
  let source = ''
  let blocks = []
  let incremental = false

  function renderIncremental(next) {
    const descriptors = streamingMarkdownBlocks(next)
    let shared = 0
    while (
      shared < blocks.length &&
      shared < descriptors.length &&
      blocks[shared].descriptor.key === descriptors[shared].key &&
      blocks[shared].descriptor.kind === descriptors[shared].kind
    ) {
      updateBlock(blocks[shared], descriptors[shared])
      shared += 1
    }
    while (blocks.length > shared) removeBlock(blocks.pop())
    for (let index = shared; index < descriptors.length; index += 1) {
      blocks.push(mountBlock(root, descriptors[index]))
    }
    source = next
    incremental = true
  }

  function replace(next, options = {}) {
    const text = String(next || '')
    root.replaceChildren()
    blocks = []
    source = text
    incremental = Boolean(options.incremental)
    if (incremental) renderIncremental(text)
    else root.innerHTML = messageBodyHtml('agent', text)
  }

  function append(delta) {
    const addition = String(delta || '')
    if (!addition) return
    const next = source + addition
    if (!incremental) {
      root.replaceChildren()
      blocks = []
    }
    renderIncremental(next)
  }

  function finish(next = source) {
    replace(next)
  }

  return {
    append,
    finish,
    replace,
    text: () => source,
  }
}
