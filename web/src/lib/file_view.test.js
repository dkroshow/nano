// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import FileView from './FileView.svelte'
import InstructionFiles from './InstructionFiles.svelte'
import ThreadPane from './ThreadPane.svelte'
import { __resetPanesForTest, openFileTab, panes } from '../panes.svelte.js'

function response(body, { status = 200 } = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
    text: async () => typeof body === 'string' ? body : JSON.stringify(body),
  }
}

function readBody({
  content = '# Instructions\n',
  editable = true,
  readOnlyReason = '',
  mtime = 10,
} = {}) {
  return {
    file: {
      path: '/workspace/AGENTS.md',
      basename: 'AGENTS.md',
      editable,
      readOnlyReason,
      provenLoaded: true,
      evidence: 'wire-authoritative',
      mtime,
    },
    encoding: 'utf-8',
    content,
    lineCount: content.split('\n').length,
  }
}

function props() {
  return {
    session: { id: 'session-1' },
    file: { path: '/workspace/AGENTS.md', basename: 'AGENTS.md', dirty: false },
    visible: true,
  }
}

afterEach(() => {
  cleanup()
  localStorage.removeItem('nano:file-word-wrap')
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function generalProps(overrides = {}) {
  __resetPanesForTest()
  const tab = {
    id: 1,
    kind: 'file',
    requestedPath: 'docs/report.md',
    path: '',
    basename: 'report.md',
    title: 'report.md',
    cwd: '/workspace',
    catalogThreadId: 'thread-1',
    sessionId: 'session-1',
    line: 2,
    navigationNonce: 1,
    fileDirty: false,
    ...overrides,
  }
  panes.tabs[1] = tab
  panes.items[0].tabIds = [1]
  panes.items[0].activeTabId = 1
  panes.nextTabId = 2
  return { tab: panes.tabs[1], paneId: 1, visible: true }
}

function generalRead(overrides = {}) {
  return {
    requestedPath: 'docs/report.md',
    path: '/workspace/docs/report.md',
    basename: 'report.md',
    content: '# First\nsecond\nthird\n',
    encoding: 'utf-8',
    sizeBytes: 21,
    lineCount: 3,
    mtime: 10,
    editable: true,
    resolvedBy: 'cwd',
    ...overrides,
  }
}

describe('instruction file view', () => {
  it('renders markdown and keeps provider-home instruction files view-only', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(readBody({
      editable: false,
      readOnlyReason: 'view-only: nano is read-only toward /Users/test/.codex',
    }))))

    const { container, getByRole, queryByRole } = render(FileView, props())

    await waitFor(() => expect(getByRole('heading', { name: 'Instructions' })).toBeTruthy())
    expect(queryByRole('button', { name: 'Edit' })).toBeNull()
    expect(container.querySelector('.file-readonly-note')?.textContent).toContain('read-only toward')

    await fireEvent.click(getByRole('button', { name: 'Raw' }))
    expect(container.querySelector('.md-raw')?.textContent).toContain('# Instructions')
  })

  it('tracks edits and saves with the mtime returned by the read', async () => {
    const requests = []
    vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
      if (String(url).endsWith('/read?path=%2Fworkspace%2FAGENTS.md')) return response(readBody())
      if (String(url).endsWith('/write')) {
        requests.push(JSON.parse(init.body))
        return response({ file: readBody({ content: '# Updated\n', mtime: 11 }).file })
      }
      throw new Error(`unexpected request: ${url}`)
    }))

    const viewProps = props()
    const { container, getByRole } = render(FileView, viewProps)
    await waitFor(() => expect(getByRole('button', { name: 'Edit' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Edit' }))

    const editor = getByRole('textbox')
    await fireEvent.input(editor, { target: { value: '# Updated\n' } })
    expect(viewProps.file.dirty).toBe(true)
    expect(container.querySelector('.file-dirty-dot')).toBeTruthy()

    await fireEvent.keyDown(editor, { key: 's', ctrlKey: true })
    await waitFor(() => expect(requests).toHaveLength(1))
    expect(requests[0]).toEqual({
      path: '/workspace/AGENTS.md',
      content: '# Updated\n',
      mtime: 10,
    })
    await waitFor(() => expect(viewProps.file.dirty).toBe(false))
    expect(container.querySelector('.file-editor')).toBeNull()
  })

  it('forces a conflicted save only after confirmation', async () => {
    const requests = []
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
      if (String(url).includes('/read?')) return response(readBody())
      if (String(url).endsWith('/write')) {
        requests.push(JSON.parse(init.body))
        if (requests.length === 1) return response({ error: 'modified', mtime: 20 }, { status: 409 })
        return response({ file: readBody({ content: '# Mine\n', mtime: 21 }).file })
      }
      throw new Error(`unexpected request: ${url}`)
    }))

    const { getByRole } = render(FileView, props())
    await waitFor(() => expect(getByRole('button', { name: 'Edit' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Edit' }))
    await fireEvent.input(getByRole('textbox'), { target: { value: '# Mine\n' } })
    await fireEvent.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(requests).toHaveLength(2))
    expect(requests[0].mtime).toBe(10)
    expect(requests[1]).toEqual({ path: '/workspace/AGENTS.md', content: '# Mine\n' })
    expect(window.confirm).toHaveBeenCalledWith('File was modified on disk. Overwrite anyway?')
  })

  it('inserts two spaces for Tab and confirms before discarding edits', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(readBody({ content: 'hello' }))))
    vi.spyOn(window, 'confirm').mockReturnValueOnce(false).mockReturnValueOnce(true)

    const viewProps = props()
    const { container, getByRole } = render(FileView, viewProps)
    await waitFor(() => expect(getByRole('button', { name: 'Edit' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Edit' }))

    const editor = getByRole('textbox')
    editor.selectionStart = editor.selectionEnd = 2
    await fireEvent.keyDown(editor, { key: 'Tab' })
    await tick()
    expect(editor.value).toBe('he  llo')
    expect(viewProps.file.dirty).toBe(true)

    await fireEvent.click(getByRole('button', { name: 'View' }))
    expect(container.querySelector('.file-editor')).toBeTruthy()
    await fireEvent.click(getByRole('button', { name: 'View' }))
    expect(container.querySelector('.file-editor')).toBeNull()
    expect(viewProps.file.dirty).toBe(false)
  })

  it('silently reloads a clean external change', async () => {
    let poll
    let diskContent = '# First\n'
    let diskMtime = 10
    vi.spyOn(window, 'setInterval').mockImplementation((callback) => {
      poll = callback
      return 1
    })
    vi.spyOn(window, 'clearInterval').mockImplementation(() => {})
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      if (String(url).includes('/read?')) return response(readBody({ content: diskContent, mtime: diskMtime }))
      if (String(url).includes('/mtime?')) return response({ path: '/workspace/AGENTS.md', mtime: diskMtime })
      throw new Error(`unexpected request: ${url}`)
    }))

    const { container, getByRole } = render(FileView, props())
    await waitFor(() => expect(getByRole('heading', { name: 'First' })).toBeTruthy())
    expect(poll).toBeTypeOf('function')

    diskContent = '# Second\n'
    diskMtime = 20
    await poll()
    await waitFor(() => expect(getByRole('heading', { name: 'Second' })).toBeTruthy())
    expect(container.querySelector('.file-external-change')).toBeNull()
  })

  it('warns about an external change while the buffer is dirty', async () => {
    let poll
    let diskMtime = 10
    vi.spyOn(window, 'setInterval').mockImplementation((callback) => {
      poll = callback
      return 1
    })
    vi.spyOn(window, 'clearInterval').mockImplementation(() => {})
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      if (String(url).includes('/read?')) return response(readBody({ mtime: 10 }))
      if (String(url).includes('/mtime?')) return response({ path: '/workspace/AGENTS.md', mtime: diskMtime })
      throw new Error(`unexpected request: ${url}`)
    }))

    const { container, getByRole } = render(FileView, props())
    await waitFor(() => expect(getByRole('button', { name: 'Edit' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Edit' }))
    await fireEvent.input(getByRole('textbox'), { target: { value: '# Unsaved\n' } })
    await tick()
    expect(container.querySelector('.file-dirty-dot')).toBeTruthy()
    diskMtime = 20
    await poll()
    await waitFor(() => expect(container.querySelector('.file-external-change')).toBeTruthy())
    expect(container.querySelector('.file-external-change').textContent).toContain('File changed on disk')
    expect(getByRole('textbox').value).toBe('# Unsaved\n')
  })
})

describe('general file view', () => {
  it('soft-wraps by default, allows opting out, and never changes the file text', async () => {
    const longLine = 'This is one deliberately long line that should visually reflow with the pane without acquiring newline characters.'
    vi.stubGlobal('fetch', vi.fn(async () => response(generalRead({ content: longLine, lineCount: 1 }))))
    const { getByRole } = render(FileView, generalProps({ line: 0 }))
    await waitFor(() => expect(getByRole('button', { name: 'Edit' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Edit' }))

    const editor = getByRole('textbox')
    const wrap = getByRole('button', { name: 'Wrap' })
    expect(editor.getAttribute('wrap')).toBe('soft')
    expect(editor.classList.contains('no-wrap')).toBe(false)
    expect(editor.value).toBe(longLine)

    await fireEvent.click(wrap)
    expect(editor.getAttribute('wrap')).toBe('off')
    expect(editor.classList.contains('no-wrap')).toBe(true)
    expect(editor.value).toBe(longLine)
    expect(localStorage.getItem('nano:file-word-wrap')).toBe('off')

    await fireEvent.click(wrap)
    expect(editor.getAttribute('wrap')).toBe('soft')
    expect(editor.value).toBe(longLine)
  })

  it('mounts as a real pane tab without a thread composer', async () => {
    __resetPanesForTest()
    vi.stubGlobal('fetch', vi.fn(async () => response(generalRead())))
    openFileTab('/workspace/docs/report.md')

    const { container } = render(ThreadPane, { pane: panes.items[0] })

    await waitFor(() => expect(container.querySelector('.file-view')).toBeTruthy())
    expect(container.querySelector('.composer')).toBeNull()
    expect(container.querySelector('.thread-pane-tab-name')?.textContent).toBe('report.md')
  })

  it('loads a contextual path, reconciles its canonical identity, and targets a line', async () => {
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      expect(String(url)).toContain('/api/v1/files/read?path=docs%2Freport.md')
      expect(String(url)).toContain('cwd=%2Fworkspace')
      expect(String(url)).toContain('threadId=thread-1')
      return response(generalRead())
    }))

    const viewProps = generalProps()
    const { container } = render(FileView, viewProps)

    await waitFor(() => expect(container.querySelector('[data-line="2"]')).toBeTruthy())
    expect(viewProps.tab.path).toBe('/workspace/docs/report.md')
    expect(container.querySelector('.md-raw')).toBeTruthy()
    expect(container.querySelector('[data-line="2"]')?.textContent).toBe('second')
  })

  it('shows a clear notice when a requested line is outside the file', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(generalRead({ content: 'only one line', lineCount: 1 }))))
    const { container } = render(FileView, generalProps({ line: 40 }))

    await waitFor(() => expect(container.querySelector('.file-line-notice')).toBeTruthy())
    expect(container.querySelector('.file-line-notice').textContent).toContain('outside this 1-line file')
  })

  it('saves through the general API and tracks dirty state on the file tab', async () => {
    const writes = []
    vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
      if (String(url).includes('/read?')) return response(generalRead())
      if (String(url).endsWith('/write')) {
        writes.push(JSON.parse(init.body))
        return response(generalRead({ content: '# Updated\n', mtime: 11 }))
      }
      throw new Error(`unexpected request: ${url}`)
    }))
    const viewProps = generalProps({ line: 0 })
    const { getByRole } = render(FileView, viewProps)
    await waitFor(() => expect(getByRole('button', { name: 'Edit' })).toBeTruthy())
    await fireEvent.click(getByRole('button', { name: 'Edit' }))
    await fireEvent.input(getByRole('textbox'), { target: { value: '# Updated\n' } })
    expect(viewProps.tab.fileDirty).toBe(true)
    await fireEvent.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toEqual({ path: '/workspace/docs/report.md', content: '# Updated\n', mtime: 10 })
    expect(viewProps.tab.fileDirty).toBe(false)
  })

  it('opens a nested Markdown file link in a pane tab', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(generalRead({
      content: '[notes](./notes.md)',
      lineCount: 1,
    }))))
    const viewProps = generalProps({ line: 0 })
    const { container } = render(FileView, viewProps)
    const link = await waitFor(() => {
      const candidate = container.querySelector('a.nano-file-link')
      expect(candidate).toBeTruthy()
      return candidate
    })
    await fireEvent.click(link)

    const nested = Object.values(panes.tabs).find((candidate) => candidate.id !== 1)
    expect(nested).toMatchObject({ kind: 'file', requestedPath: './notes.md', cwd: '/workspace/docs' })
  })

  it('lets the operator pick which file an ambiguous link meant', async () => {
    const requests = []
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      requests.push(String(url))
      if (requests.length === 1) {
        return response({
          error: { code: 'ambiguous_path', message: 'More than one file matches this reference' },
          candidates: ['/workspace/docs/report.md', '/workspace/packages/worker/docs/report.md'],
        }, { status: 409 })
      }
      return response(generalRead({ path: '/workspace/packages/worker/docs/report.md' }))
    }))

    const viewProps = generalProps({ line: 0 })
    const { container, getByRole } = render(FileView, viewProps)

    await waitFor(() => expect(container.querySelectorAll('.file-candidate')).toHaveLength(2))
    expect(container.querySelector('.file-candidates-label').textContent).toContain('2 files match')

    await fireEvent.click(getByRole('button', { name: '/workspace/packages/worker/docs/report.md' }))

    await waitFor(() => expect(viewProps.tab.path).toBe('/workspace/packages/worker/docs/report.md'))
    expect(container.querySelector('.file-candidates')).toBeFalsy()
    expect(requests[1]).toContain('path=%2Fworkspace%2Fpackages%2Fworker%2Fdocs%2Freport.md')
    expect(requests[1]).not.toContain('cwd=')
  })

  it('keeps embedded HTML inert in formatted Markdown files', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(generalRead({
      content: '<img src=x onerror=alert(1)>', lineCount: 1,
    }))))
    const { container } = render(FileView, generalProps({ line: 0 }))

    await waitFor(() => expect(container.querySelector('.fb-reader-body')).toBeTruthy())
    expect(container.querySelector('.fb-reader-body img')).toBeNull()
    expect(container.querySelector('.fb-reader-body').textContent).toContain('<img src=x onerror=alert(1)>')
  })
})

describe('instruction file rows', () => {
  it('opens existing sources and disables missing ones', async () => {
    const openInstructionFile = vi.fn()
    const existing = {
      path: '/workspace/AGENTS.md', basename: 'AGENTS.md', exists: true, editable: true,
      evidence: 'wire-authoritative', sourceKind: 'codex-thread-instruction-source',
    }
    const missing = {
      path: '/workspace/MISSING.md', basename: 'MISSING.md', exists: false, editable: false,
      evidence: 'wire-authoritative', sourceKind: 'codex-thread-instruction-source',
    }
    vi.stubGlobal('fetch', vi.fn(async () => response({ files: [existing, missing] })))

    const { getByRole } = render(InstructionFiles, {
      session: { id: 'session-1', openInstructionFile },
    })
    const existingRow = await waitFor(() => getByRole('button', { name: /AGENTS\.md/ }))
    const missingRow = getByRole('button', { name: /MISSING\.md/ })

    expect(existingRow.disabled).toBe(false)
    expect(missingRow.disabled).toBe(true)
    await fireEvent.click(existingRow)
    expect(openInstructionFile).toHaveBeenCalledWith(existing)
  })
})
