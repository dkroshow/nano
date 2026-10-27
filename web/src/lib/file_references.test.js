// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { decorateFileReferences, fileReferenceLinks, parseFileReference } from './file_references.js'
import { resetHostedIdentityCache } from './thread_references.js'
import { messageBodyHtml } from './message_body.js'

afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  resetHostedIdentityCache()
})

describe('file reference recognition', () => {
  it.each([
    ['/tmp/report.md:12', '/tmp/report.md', 12],
    ['~/notes/plan.txt', '~/notes/plan.txt', 0],
    ['./src/main.go', './src/main.go', 0],
    ['../shared/types.ts:7', '../shared/types.ts', 7],
    ['src/lib/App.svelte', 'src/lib/App.svelte', 0],
    ['evidence/threads.jsonl', 'evidence/threads.jsonl', 0],
    ['src/Makefile', 'src/Makefile', 0],
    ['~/Library/Application Support/project/fleet-monitor.json', '~/Library/Application Support/project/fleet-monitor.json', 0],
  ])('recognizes %s', (authored, path, line) => {
    expect(parseFileReference(authored)).toEqual({ path, line })
  })

  it('decodes an explicit file URI', () => {
    expect(parseFileReference('file:///tmp/space%20name.md:3', { explicit: true })).toEqual({
      path: '/tmp/space name.md', line: 3,
    })
  })

  it('recognizes extensionless paths without turning slash commands into files', () => {
    expect(parseFileReference('/tmp/Makefile')).toEqual({ path: '/tmp/Makefile', line: 0 })
    expect(parseFileReference('/reconcile')).toBeNull()
    expect(parseFileReference('not-a-path')).toBeNull()
  })

  it.each(['https://example.com/a.md', 'mailto:a@example.com', '#section', 'javascript:alert(1)', 'data:text/plain,x'])('rejects %s', (value) => {
    expect(parseFileReference(value, { explicit: true })).toBeNull()
  })
})

describe('file reference decoration', () => {
  it('decorates prose, inline code, and fenced code', () => {
    const root = document.createElement('div')
    root.innerHTML = '<p>See /tmp/report.md:9 and <code>src/main.go</code>.</p><pre><code>/tmp/nope.md</code></pre>'

    decorateFileReferences(root)

    const links = [...root.querySelectorAll('a.nano-file-link')]
    expect(links.map((link) => [link.dataset.filePath, link.dataset.fileLine || ''])).toEqual([
      ['/tmp/report.md', '9'],
      ['src/main.go', ''],
      ['/tmp/nope.md', ''],
    ])
  })

  it('links every file path in the reported Project message', () => {
    const root = document.createElement('div')
    root.innerHTML = messageBodyHtml('agent', [
      'First, it loads:',
      '',
      '```text',
      '~/Library/Application Support/project/fleet-monitor.json',
      '```',
      '',
      'Second:',
      '',
      '```text',
      'commentary/agent-escalations.jsonl',
      '```',
      '',
      '- `state.json`;',
      '- `evidence/threads.jsonl`.',
    ].join('\n'))

    decorateFileReferences(root)

    expect([...root.querySelectorAll('a.nano-file-link')].map((link) => link.dataset.filePath)).toEqual([
      '~/Library/Application Support/project/fleet-monitor.json',
      'commentary/agent-escalations.jsonl',
      'state.json',
      'evidence/threads.jsonl',
    ])
  })

  it('does not decorate path-shaped pieces of URLs or email addresses', () => {
    const root = document.createElement('div')
    root.textContent = 'See https://example.com/report.md or mail agent@example.com.'

    decorateFileReferences(root)

    expect(root.querySelector('a.nano-file-link')).toBeNull()
  })

  it('auto-links hosted ids in prose but never native UUIDs or code', async () => {
    const native = '11111111-2222-4333-8444-555555555555'
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false })))
    const root = document.createElement('div')
    root.innerHTML = `<p>Follow nht_worker-1 and ${native}.</p><pre><code>nht_not-a-link</code></pre>`

    decorateFileReferences(root)

    const links = [...root.querySelectorAll('a.nano-thread-link')]
    expect(links.map((link) => [link.textContent, link.getAttribute('href')])).toEqual([
      ['nht_worker-1', '?thread=nht_worker-1'],
    ])
    expect(root.querySelector('pre a')).toBeNull()
    expect(root.textContent).toContain(native)
    await vi.waitFor(() => expect(links[0].textContent).toBe('nht_worker-1 (unresolved hosted id)'))
    expect(links[0].getAttribute('href')).toBe('?thread=nht_worker-1')
  })

  it('relabels hosted-id links as display name (native UUID) and points the href at the UUID', async () => {
    const native = '11111111-2222-4333-8444-555555555555'
    vi.stubGlobal('fetch', vi.fn(async (url) => ({
      ok: true,
      json: async () => (String(url).includes('nht_worker-1')
        ? { displayName: 'research worker', nativeThreadId: native, hostedThreadId: 'nht_worker-1', provider: 'codex' }
        : { displayName: 'new thread', nativeThreadId: 'cs_abc', nativeThreadIdProvisional: true, hostedThreadId: 'nht_fresh', provider: 'claude' }),
    })))
    const root = document.createElement('div')
    root.innerHTML = '<p>Ask nht_worker-1 or nht_fresh.</p>'

    decorateFileReferences(root)

    const [resolved, provisional] = [...root.querySelectorAll('a.nano-thread-link')]
    await vi.waitFor(() => expect(resolved.textContent).toBe(`research worker (${native})`))
    expect(resolved.getAttribute('href')).toBe(`?thread=${native}`)
    expect(resolved.dataset.nanoThreadReference).toBe('nht_worker-1')
    await vi.waitFor(() => expect(provisional.textContent).toBe('new thread'))
    expect(provisional.getAttribute('href')).toBe('?thread=nht_fresh')
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('delegates a decorated click without navigating the browser', async () => {
    const root = document.createElement('div')
    root.innerHTML = '<p>/tmp/report.md:4</p>'
    document.body.append(root)
    const onOpen = vi.fn()
    const action = fileReferenceLinks(root, { onOpen, context: { cwd: '/tmp' } })
    await new Promise((resolve) => queueMicrotask(resolve))

    const event = new MouseEvent('click', { bubbles: true, cancelable: true })
    root.querySelector('a').dispatchEvent(event)

    expect(event.defaultPrevented).toBe(true)
    expect(onOpen).toHaveBeenCalledWith({ path: '/tmp/report.md', line: 4 }, { cwd: '/tmp' })
    action.destroy()
    root.remove()
  })

  it('routes an unmodified Nano thread click in-app and leaves modified clicks to the browser', async () => {
    const hosted = 'nht_worker-1'
    const root = document.createElement('div')
    root.innerHTML = `<a class="nano-thread-link" href="?thread=${hosted}" data-nano-thread-reference="${hosted}">open</a>`
    document.body.append(root)
    const onOpenThread = vi.fn()
    const action = fileReferenceLinks(root, { onOpenThread })
    await new Promise((resolve) => queueMicrotask(resolve))

    const link = root.querySelector('a')
    const normal = new MouseEvent('click', { bubbles: true, cancelable: true })
    link.dispatchEvent(normal)
    expect(normal.defaultPrevented).toBe(true)
    expect(onOpenThread).toHaveBeenCalledWith(hosted)

    const modified = new MouseEvent('click', { bubbles: true, cancelable: true, ctrlKey: true })
    link.dispatchEvent(modified)
    expect(modified.defaultPrevented).toBe(false)
    expect(onOpenThread).toHaveBeenCalledTimes(1)
    action.destroy()
    root.remove()
  })

})
