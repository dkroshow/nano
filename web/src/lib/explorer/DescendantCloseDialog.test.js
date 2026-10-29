// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import DescendantCloseDialog from './DescendantCloseDialog.svelte'
import { __resetDescendantCloseForTest, descendantClose } from './descendant_close.svelte.js'
import { app } from '../../app.svelte.js'

const nodes = [
  { key: 'review', depth: 0, label: 'Audit coordinator', closable: true, sessionId: 'review-session', badges: [{ role: 'auditor', text: 'auditor' }], activity: 'last active 1m', working: false, dot: null },
  { key: 'worker', depth: 1, label: 'Implementation', closable: true, sessionId: 'worker-session', badges: [{ role: 'worker', text: 'worker' }], activity: 'working', working: true, dot: { state: 'working' } },
  { key: 'history', depth: 0, label: 'Earlier review', closable: false, sessionId: '', badges: [], activity: 'last active 1h', working: false, dot: null },
]

function openDialog() {
  descendantClose.request = {
    parent: { provider: 'codex', nativeId: 'parent' }, parentSessionId: 'parent-session', provider: 'codex',
    nodes, selected: [], busy: false, results: [], resolve: vi.fn(),
  }
}

function response(ok = true, text = '') {
  return { ok, status: ok ? 200 : 500, text: async () => text, json: async () => ({ sessions: [], detached: [], defaults: {} }) }
}

beforeEach(() => {
  app.sessions = []
  vi.spyOn(HTMLElement.prototype, 'offsetParent', 'get').mockReturnValue(document.body)
  vi.stubGlobal('fetch', vi.fn(async (_url, options) => options?.method === 'DELETE' ? response() : response()))
  __resetDescendantCloseForTest()
})

afterEach(() => {
  cleanup()
  document.querySelectorAll('[data-descendant-dialog-opener]').forEach((element) => element.remove())
  __resetDescendantCloseForTest()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('DescendantCloseDialog', () => {
  it('renders a recursive, default-unchecked tree with disabled historical context', () => {
    openDialog()
    const view = render(DescendantCloseDialog)
    expect(view.getByRole('button', { name: 'Detach parent' })).not.toBeNull()
    expect(view.getByLabelText('Close Audit coordinator').checked).toBe(false)
    expect(view.getByLabelText('Close Implementation').checked).toBe(false)
    expect(view.getByLabelText('Close Earlier review').disabled).toBe(true)
    expect(view.container.querySelector('.descendant-close-row[style*="padding-left: 26px"]')).not.toBeNull()
    expect(view.container.querySelector('.role-badge')).toBeNull()
    expect(view.container.textContent).not.toContain('auditor')
    expect(view.container.textContent).not.toContain('supervisor')
    expect(view.container.textContent).toContain('last active 1h')
  })

  it('selects all closable agents and maintains the indeterminate state', async () => {
    openDialog()
    const view = render(DescendantCloseDialog)
    const all = view.getByLabelText('Select all closable agents')
    await fireEvent.click(view.getByLabelText('Close Audit coordinator'))
    expect(all.checked).toBe(false)
    expect(all.indeterminate).toBe(true)
    await fireEvent.click(all)
    expect(view.getByLabelText('Close Audit coordinator').checked).toBe(true)
    expect(view.getByLabelText('Close Implementation').checked).toBe(true)
    expect(all.checked).toBe(true)
    expect(all.indeterminate).toBe(false)
  })

  it('moves focus into the dialog and traps Tab inside it', async () => {
    const opener = document.body.appendChild(document.createElement('button'))
    opener.dataset.descendantDialogOpener = 'true'
    opener.textContent = 'Detach'
    opener.focus()
    openDialog()
    const view = render(DescendantCloseDialog)
    const all = view.getByLabelText('Select all closable agents')
    const primary = view.getByRole('button', { name: 'Detach parent' })

    await waitFor(() => expect(document.activeElement).toBe(all))
    primary.focus()
    await fireEvent.keyDown(window, { key: 'Tab' })
    expect(document.activeElement).toBe(all)
    await fireEvent.keyDown(window, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(primary)
  })

  it('cancels with Escape without DELETE and restores focus to the opener', async () => {
    const opener = document.body.appendChild(document.createElement('button'))
    opener.dataset.descendantDialogOpener = 'true'
    opener.textContent = 'Close session'
    opener.focus()
    openDialog()
    render(DescendantCloseDialog)
    await tick()

    await fireEvent.keyDown(window, { key: 'Escape' })

    expect(fetch).not.toHaveBeenCalled()
    expect(descendantClose.request).toBeNull()
    await waitFor(() => expect(document.activeElement).toBe(opener))
  })

  it('warns for a selected working agent and closes only selected agents plus the parent', async () => {
    openDialog()
    const view = render(DescendantCloseDialog)
    await fireEvent.click(view.getByLabelText('Close Implementation'))
    expect(view.container.textContent).toContain('1 selected agent is working.')
    expect(view.getByRole('button', { name: 'Detach parent and close 1 agent' })).not.toBeNull()
    await fireEvent.click(view.getByRole('button', { name: 'Detach parent and close 1 agent' }))
    await waitFor(() => {
      expect(fetch).toHaveBeenNthCalledWith(1, '/api/sessions/worker-session', { method: 'DELETE' })
      expect(fetch).toHaveBeenNthCalledWith(3, '/api/sessions/parent-session', { method: 'DELETE' })
      expect(descendantClose.request).toBeNull()
    })
    expect(fetch).not.toHaveBeenCalledWith('/api/sessions/review-session', { method: 'DELETE' })
  })

  it('cancels without changing runtime state', async () => {
    openDialog()
    const view = render(DescendantCloseDialog)
    await fireEvent.click(view.getByRole('button', { name: 'Cancel' }))
    expect(fetch).not.toHaveBeenCalled()
    expect(descendantClose.request).toBeNull()
  })

  it('keeps partial close failures visible', async () => {
    openDialog()
    vi.stubGlobal('fetch', vi.fn(async (url, options) => {
      if (options?.method === 'DELETE' && String(url).includes('worker-session')) return response(false, 'worker is unavailable')
      return response()
    }))
    const view = render(DescendantCloseDialog)
    await fireEvent.click(view.getByLabelText('Close Implementation'))
    await fireEvent.click(view.getByRole('button', { name: 'Detach parent and close 1 agent' }))
    await waitFor(() => {
      expect(view.getByRole('alert').textContent).toContain('worker is unavailable')
      expect(descendantClose.request).not.toBeNull()
    })
    await fireEvent.keyDown(window, { key: 'Escape' })
    expect(view.getByRole('alert').textContent).toContain('worker is unavailable')
    expect(descendantClose.request).not.toBeNull()
  })
})
