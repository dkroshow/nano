// Thread references are intentionally stricter than file references. A native
// UUID is linkable only when an author deliberately puts it in Markdown; prose
// auto-linking is reserved for Nano's unmistakable nht_ namespace.
export const NATIVE_THREAD_REFERENCE_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
export const HOSTED_THREAD_REFERENCE_RE = /^nht_[A-Za-z0-9_-]+$/
const HOSTED_THREAD_REFERENCE_IN_PROSE_RE = /(?<![A-Za-z0-9_-])(nht_[A-Za-z0-9_-]+)(?![A-Za-z0-9_-])/g
let threadReferenceNavigator = null

export function isThreadReference(value) {
  const reference = String(value ?? '').trim()
  return NATIVE_THREAD_REFERENCE_RE.test(reference) || HOSTED_THREAD_REFERENCE_RE.test(reference)
}

export function threadReferenceHref(reference) {
  return `?thread=${encodeURIComponent(reference)}`
}

// Markdown's documented form is ?thread=<reference>. Absolute links are
// accepted only when they point back to this Nano origin; every other URL
// remains an ordinary web link with its normal safe renderer treatment.
export function threadReferenceFromHref(value) {
  const target = String(value ?? '').trim()
  if (isThreadReference(target)) return target // retain the concise bare form
  if (!target.startsWith('?') && !/^https?:/i.test(target)) return null
  try {
    const base = typeof window === 'undefined' ? 'http://localhost/' : window.location.href
    const parsed = new URL(target, base)
    if (/^https?:/i.test(target) && typeof window !== 'undefined' && parsed.origin !== window.location.origin) return null
    const reference = parsed.searchParams.get('thread')?.trim() ?? ''
    return isThreadReference(reference) ? reference : null
  } catch {
    return null
  }
}

// App installs the shared resolver once. Keeping this small bridge here lets
// transcript and FileView actions use that path without importing app state.
export function setThreadReferenceNavigator(navigate) {
  threadReferenceNavigator = typeof navigate === 'function' ? navigate : null
}

export function navigateThreadReference(reference) {
  threadReferenceNavigator?.(reference)
}

const identityCache = new Map()

export function unresolvedHostedLabel(hostedId) {
  return `${hostedId} (unresolved hosted id)`
}

export function resolveHostedIdentity(hostedId, fetchImpl = globalThis.fetch) {
  if (identityCache.has(hostedId)) return identityCache.get(hostedId)
  const pending = (async () => {
    try {
      const response = await fetchImpl(`/api/v1/identity/resolve?ref=${encodeURIComponent(hostedId)}`)
      if (!response?.ok) return null
      const body = await response.json()
      return body?.displayName || body?.nativeThreadId ? body : null
    } catch {
      return null
    }
  })()
  identityCache.set(hostedId, pending)
  return pending
}

export function resetHostedIdentityCache() {
  identityCache.clear()
}

export function hostedIdentityLabel(identity, hostedId) {
  if (!identity) return unresolvedHostedLabel(hostedId)
  const name = String(identity.displayName ?? '').trim()
  const native = identity.nativeThreadIdProvisional ? '' : String(identity.nativeThreadId ?? '').trim()
  if (name && native) return `${name} (${native})`
  if (name) return name
  if (native) return native
  return unresolvedHostedLabel(hostedId)
}

function relabelHostedAnchor(anchor, hostedId) {
  void resolveHostedIdentity(hostedId).then((identity) => {
    if (!anchor.isConnected && !anchor.parentNode) return
    anchor.textContent = hostedIdentityLabel(identity, hostedId)
    const native = identity && !identity.nativeThreadIdProvisional ? String(identity.nativeThreadId ?? '').trim() : ''
    if (native && NATIVE_THREAD_REFERENCE_RE.test(native)) anchor.href = threadReferenceHref(native)
  })
}

function threadReferenceAnchor(reference, label = reference) {
  const anchor = document.createElement('a')
  anchor.className = 'nano-thread-link'
  // A real same-origin query href keeps native browser copy/open semantics;
  // App's boot/popstate resolver owns the eventual route decision.
  anchor.href = threadReferenceHref(reference)
  anchor.dataset.nanoThreadReference = reference
  anchor.textContent = label
  return anchor
}

// Auto-link hosted ids only in ordinary prose. Do not inspect code/pre blocks:
// command examples and opaque ids should remain copyable verbatim there.
export function decorateHostedThreadReferences(root) {
  if (!root) return
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT)
  const nodes = []
  let node
  while ((node = walker.nextNode())) {
    const parent = node.parentElement
    if (!parent || parent.closest('a,textarea,input,pre,code')) continue
    nodes.push(node)
  }
  for (const textNode of nodes) {
    const value = textNode.textContent || ''
    HOSTED_THREAD_REFERENCE_IN_PROSE_RE.lastIndex = 0
    if (!HOSTED_THREAD_REFERENCE_IN_PROSE_RE.test(value)) continue
    HOSTED_THREAD_REFERENCE_IN_PROSE_RE.lastIndex = 0
    const fragment = document.createDocumentFragment()
    let cursor = 0
    let match
    while ((match = HOSTED_THREAD_REFERENCE_IN_PROSE_RE.exec(value))) {
      if (match.index > cursor) fragment.append(document.createTextNode(value.slice(cursor, match.index)))
      const anchor = threadReferenceAnchor(match[1])
      relabelHostedAnchor(anchor, match[1])
      fragment.append(anchor)
      cursor = match.index + match[1].length
    }
    if (cursor < value.length) fragment.append(document.createTextNode(value.slice(cursor)))
    textNode.replaceWith(fragment)
  }
}
