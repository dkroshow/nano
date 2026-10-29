// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { app } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { __resetDescendantCloseForTest, cancelDescendantClose, descendantClose, requestParentClose } from './descendant_close.svelte.js'

function response() {
  return { ok: true, status: 200, text: async () => '', json: async () => ({ sessions: [], detached: [], defaults: {} }) }
}

beforeEach(() => {
  app.sessions = [{ id: 'parent-session', provider: 'codex', threadId: 'parent' }]
  explorer.dataset = []
  __resetDescendantCloseForTest()
  vi.stubGlobal('fetch', vi.fn(async () => response()))
})

afterEach(() => {
  __resetDescendantCloseForTest()
  vi.unstubAllGlobals()
})

it('uses the existing direct close path when the parent has no descendants', async () => {
  await expect(requestParentClose({ parent: { provider: 'codex', nativeId: 'parent' }, sessionId: 'parent-session', provider: 'codex' }))
    .resolves.toMatchObject({ parentClosed: true })
  expect(descendantClose.request).toBeNull()
  expect(fetch).toHaveBeenCalledWith('/api/sessions/parent-session', { method: 'DELETE' })
})
