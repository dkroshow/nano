// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render } from '@testing-library/svelte'
import ThreadQuickDetail from './ThreadQuickDetail.svelte'
import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import { openQuickDetail, closeQuickDetail, quickDetail } from './quick_detail.svelte.js'
import { __resetPanesForTest, closeTab, openThreadTab, tabTarget } from '../panes.svelte.js'

afterEach(() => {
  cleanup()
  closeQuickDetail()
  app.detached = []
  app.sessions = []
  explorer.dataset = []
  __resetPanesForTest()
  vi.unstubAllGlobals()
})

it('shows a detached Claude stop error and retains the actual popup for retry', async () => {
  app.detached = [{ provider: 'claude', nativeThreadId: 'native-1' }]
  openQuickDetail({ provider: 'claude', nativeId: 'native-1', detached: true, closable: true })
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 502, text: async () => 'Claude stop unconfirmed' })))
  const view = render(ThreadQuickDetail)
  await fireEvent.click(view.getByRole('button', { name: 'End session' }))
  expect(await view.findByText('Claude stop unconfirmed')).toBeTruthy()
  expect(view.getByRole('button', { name: 'End session' })).toBeTruthy()
  expect(app.detached).toHaveLength(1)
  expect(quickDetail.target).not.toBeNull()
  expect(fetch).toHaveBeenCalledWith('/api/attachments/native-1?provider=claude', { method: 'DELETE' })
  const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
  await fireEvent.click(view.getByRole('button', { name: 'Forget attachment anyway' }))
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining('may still be running'))
  expect(fetch).toHaveBeenCalledTimes(1)
  confirm.mockReturnValue(true)
  fetch.mockResolvedValueOnce({ ok: true })
  await fireEvent.click(view.getByRole('button', { name: 'Forget attachment anyway' }))
  expect(fetch).toHaveBeenLastCalledWith('/api/attachments/native-1?provider=claude&forget=unconfirmed&expectedShimSessionId=&expectedAttachedAt=', { method: 'DELETE' })
  expect(app.detached).toHaveLength(0)
  confirm.mockRestore()
})

it('plain tab closure does not request provider termination', () => {
  vi.stubGlobal('fetch', vi.fn())
  const tab = openThreadTab(tabTarget({ item: { id: 'thread-1', nativeId: 'native-1', provider: 'claude', metadata: {} } }))
  closeTab(tab)
  expect(fetch).not.toHaveBeenCalled()
})
