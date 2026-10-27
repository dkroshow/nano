import {
  decorateHostedThreadReferences,
  isThreadReference,
  navigateThreadReference,
  threadReferenceFromHref,
} from './thread_references.js'
// Recognition is intentionally broad. Agents routinely mention repo-specific
// extensions (.jsonl is the example that exposed the old allowlist), generated
// files, and extensionless names. Resolution remains the authority when a
// path-shaped token turns out not to exist.
const PATH_PART = '[\\w@.+-]*[\\w@+-]'
const PREFIXED_PATH = '(?:~\\/|\\/|\\.{1,2}\\/)(?:' + PATH_PART + '\\/)*' + PATH_PART
const RELATIVE_PATH = '(?:' + PATH_PART + '\\/)+' + PATH_PART
const DOTTED_FILE = PATH_PART + '\\.' + PATH_PART
const PLAIN_FILE_RE = new RegExp('(' + PREFIXED_PATH + '|' + RELATIVE_PATH + '|' + DOTTED_FILE + ')(?::(\\d+))?', 'gi')
const KNOWN_EXTENSIONLESS_FILES = new Set([
  'dockerfile', 'gemfile', 'license', 'makefile', 'procfile', 'readme',
])

function likelyLocalFilePath(value) {
  if (!value || /[\n\r\0<>|`]/.test(value) || value.endsWith('/')) return false
  const normalized = value.replace(/^(?:~\/|\.\.\/|\.\/|\/)/, '')
  const parts = normalized.split('/').filter(Boolean)
  if (!parts.length) return false
  const basename = parts.at(-1)
  const fileNamed = /\.[A-Za-z0-9][\w+-]*$/.test(basename) || KNOWN_EXTENSIONLESS_FILES.has(basename.toLowerCase())
  if (!value.includes('/')) return fileNamed
  // Avoid turning slash commands such as /reconcile into files. Longer
  // absolute paths and every authored relative/home path are path-shaped
  // enough to offer resolution even when the final name is extensionless.
  if (value.startsWith('/') && parts.length < 2 && !fileNamed) return false
  return true
}

export function parseFileReference(value, { explicit = false } = {}) {
  let text = String(value || '').trim()
  if (!text || text.startsWith('#') || /^(?:mailto|tel|javascript|data):/i.test(text)) return null
  if ((text.startsWith('"') && text.endsWith('"')) || (text.startsWith("'") && text.endsWith("'"))) {
    text = text.slice(1, -1).trim()
  }
  if (/^https?:/i.test(text)) return null
  if (/^file:\/\//i.test(text)) {
    try {
      text = decodeURIComponent(new URL(text).pathname)
    } catch {
      return null
    }
  } else {
    try {
      text = decodeURIComponent(text)
    } catch {
      // Keep a literal authored path when it is not valid URI encoding.
    }
  }
  text = text.split(/[?#]/, 1)[0].trim()
  if (!text || /^[a-z][a-z0-9+.-]*:\/\//i.test(text)) return null
  let line = 0
  const lineMatch = text.match(/^(.*):(\d+)$/)
  if (lineMatch) {
    text = lineMatch[1]
    line = Number(lineMatch[2])
  }
  if (!text || text.includes('\0')) return null
  if (!explicit && !likelyLocalFilePath(text)) return null
  return { path: text, line }
}

function anchorFor(reference, label) {
  const anchor = document.createElement('a')
  anchor.className = 'nano-file-link'
  anchor.href = '#'
  anchor.textContent = label
  anchor.dataset.filePath = reference.path
  if (reference.line) anchor.dataset.fileLine = String(reference.line)
  anchor.title = reference.path + (reference.line ? `:${reference.line}` : '')
  return anchor
}

function decorateExistingAnchors(root) {
  for (const anchor of root.querySelectorAll('a[data-nano-file-path], a:not(.nano-file-link)')) {
    if (anchor.classList.contains('nano-file-link') && anchor.dataset.filePath) continue
    const authored = anchor.dataset.nanoFilePath || anchor.getAttribute('href') || ''
    const reference = parseFileReference(authored, { explicit: true })
    if (!reference) continue
    anchor.classList.add('nano-file-link')
    anchor.href = '#'
    anchor.dataset.filePath = reference.path
    if (reference.line) anchor.dataset.fileLine = String(reference.line)
    anchor.removeAttribute('target')
    anchor.removeAttribute('rel')
  }
}

function replaceExactCodePath(code, reference, value) {
  const leading = value.match(/^\s*/)?.[0] ?? ''
  const trailing = value.match(/\s*$/)?.[0] ?? ''
  code.replaceChildren()
  if (leading) code.append(document.createTextNode(leading))
  code.append(anchorFor(reference, value.trim()))
  if (trailing) code.append(document.createTextNode(trailing))
}

function decorateCode(root) {
  for (const code of root.querySelectorAll('code')) {
    if (code.closest('a') || code.querySelector('a')) continue
    const value = code.textContent || ''
    const reference = parseFileReference(value.trim())
    if (reference) {
      replaceExactCodePath(code, reference, value)
      continue
    }
    decorateText(code, true)
  }
}

function decorateText(root, insideCode = false) {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT)
  const nodes = []
  let node
  while ((node = walker.nextNode())) {
    const parent = node.parentElement
    if (!parent || parent.closest('a,textarea,input')) continue
    if (!insideCode && parent.closest('pre,code')) continue
    nodes.push(node)
  }
  for (const textNode of nodes) {
    const value = textNode.textContent || ''
    PLAIN_FILE_RE.lastIndex = 0
    if (!PLAIN_FILE_RE.test(value)) continue
    PLAIN_FILE_RE.lastIndex = 0
    const fragment = document.createDocumentFragment()
    let cursor = 0
    let match
    while ((match = PLAIN_FILE_RE.exec(value))) {
      const before = value.slice(0, match.index)
      // The path-like suffix of a plain URL or email address is not a local
      // file reference. Authored Markdown anchors are handled separately.
      if (/(?:https?|file):\/$/i.test(before) || (!match[0].includes('/') && match[0].includes('@'))) continue
      const reference = parseFileReference(match[0])
      if (!reference) continue
      if (match.index > cursor) fragment.append(document.createTextNode(value.slice(cursor, match.index)))
      fragment.append(anchorFor(reference, match[0]))
      cursor = match.index + match[0].length
    }
    if (cursor === 0) continue
    if (cursor < value.length) fragment.append(document.createTextNode(value.slice(cursor)))
    textNode.replaceWith(fragment)
  }
}

export function decorateFileReferences(root) {
  if (!root) return
  decorateExistingAnchors(root)
  decorateCode(root)
  decorateText(root)
  decorateHostedThreadReferences(root)
}

// Svelte action shared by live/durable transcript messages and the Markdown
// file reader. A MutationObserver catches {@html} replacements; decoration is
// idempotent, and click handling stays delegated to the stable root.
export function fileReferenceLinks(node, options = {}) {
  let current = options
  let scheduled = false
  const decorate = () => {
    scheduled = false
    decorateFileReferences(node)
  }
  const schedule = () => {
    if (scheduled) return
    scheduled = true
    queueMicrotask(decorate)
  }
  const click = (event) => {
    const threadAnchor = event.target?.closest?.('a.nano-thread-link')
    if (threadAnchor && node.contains(threadAnchor)) {
      // Let the browser own a modified or middle click so new tabs, copied
      // links, and platform gestures retain their normal behavior.
      if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return
      const reference = threadAnchor.dataset.nanoThreadReference || threadReferenceFromHref(threadAnchor.getAttribute('href'))
      if (!isThreadReference(reference)) return
      event.preventDefault()
      if (typeof current?.onOpenThread === 'function') current.onOpenThread(reference)
      else navigateThreadReference(reference)
      return
    }
    const anchor = event.target?.closest?.('a.nano-file-link')
    if (!anchor || !node.contains(anchor)) return
    event.preventDefault()
    const reference = {
      path: anchor.dataset.filePath || '',
      line: Number(anchor.dataset.fileLine || 0),
    }
    current?.onOpen?.(reference, current.context || {})
  }
  node.addEventListener('click', click)
  const observer = new MutationObserver(schedule)
  observer.observe(node, { childList: true, subtree: true })
  schedule()
  return {
    update(next) {
      current = next
      schedule()
    },
    destroy() {
      observer.disconnect()
      node.removeEventListener('click', click)
    },
  }
}
