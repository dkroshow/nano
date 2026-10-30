// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte'
import BulkCloseDialog from './BulkCloseDialog.svelte'
import { __resetBulkCloseForTest, bulkClose } from './bulk_close.svelte.js'
import { app } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'

const nodes = [
  { key: 'hosted:root', depth: 0, label: 'Closed parent', activity: 'last active 2h', working: false, starred: true, dot: null },
  { key: 'hosted:worker', depth: 1, label: 'Active worker', activity: 'working', working: true, starred: false, dot: { state: 'working' } },
]

function openDialog() {
  bulkClose.request = {
    root: { hostedThreadId: 'root' }, rootHostedThreadId: 'root', nodes,
    busy: false, results: [], response: null, resolve: vi.fn(),
  }
}

function response({ status = 200, body = {} } = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    text: async () => JSON.stringify(body),
    json: async () => body,
  }
}

beforeEach(() => {
  app.sessions = []
  explorer.dataset = []
  explorer.windowItems = []
  explorer.activeItems = []
  vi.spyOn(HTMLElement.prototype, 'offsetParent', 'get').mockReturnValue(document.body)
  vi.stubGlobal('fetch', vi.fn(async (url) => {
    if (String(url).includes('/family/close')) {
      return response({ body: { complete: true, members: [], memberCount: 2 } })
    }
    if (url === '/api/sessions') return response({ body: { sessions: [], detached: [], defaults: {} } })
    return response({ body: { items: [], nextCursor: null } })
  }))
  __resetBulkCloseForTest()
})

afterEach(() => {
  cleanup()
  __resetBulkCloseForTest()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('BulkCloseDialog', () => {
  it('warns about working and starred members, then closes in one request', async () => {
    openDialog()
    const view = render(BulkCloseDialog)
    expect(view.getByRole('dialog', { name: 'Bulk close thread tree' }).textContent).toContain('closes 2 threads')
    expect(view.container.textContent).toContain('1 thread is working')
    expect(view.container.textContent).toContain('1 thread is starred. Closing removes the star.')
    expect(view.container.querySelector('.descendant-close-row[style*="padding-left: 26px"]')).not.toBeNull()

    await fireEvent.click(view.getByRole('button', { name: 'Close tree' }))

    await waitFor(() => {
      const calls = fetch.mock.calls.filter(([url]) => String(url).includes('/family/close'))
      expect(calls).toHaveLength(1)
      expect(calls[0][0]).toBe('/api/v1/hosted-threads/root/family/close')
      expect(bulkClose.request).toBeNull()
    })
  })

  it('keeps partial member failures visible', async () => {
    vi.stubGlobal('fetch', vi.fn(async (url) => {
      if (String(url).includes('/family/close')) {
        return response({
          status: 207,
          body: { complete: false, members: [{ closeAction: 'failed', error: 'worker could not close' }] },
        })
      }
      if (url === '/api/sessions') return response({ body: { sessions: [], detached: [], defaults: {} } })
      return response({ body: { items: [], nextCursor: null } })
    }))
    openDialog()
    const view = render(BulkCloseDialog)
    await fireEvent.click(view.getByRole('button', { name: 'Close tree' }))
    await waitFor(() => expect(view.getByRole('alert').textContent).toContain('worker could not close'))
    expect(bulkClose.request).not.toBeNull()
  })

  it('cancels without calling the API', async () => {
    openDialog()
    const view = render(BulkCloseDialog)
    await fireEvent.click(view.getByRole('button', { name: 'Cancel' }))
    expect(fetch).not.toHaveBeenCalled()
    expect(bulkClose.request).toBeNull()
  })
})
