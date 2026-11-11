// @vitest-environment jsdom
//
// Detail-view layout contract. `standalone` is what makes Details fill its
// pane; without it the panel is a narrow companion pinned beside a transcript.
// A commit once dropped the prop and quietly turned the full-pane view into a
// 360px inspector, which no logic test could have caught — the difference is
// entirely in what gets rendered.
import { describe, it, expect, afterEach, vi } from 'vitest'
import { render, cleanup, fireEvent } from '@testing-library/svelte'
import ThreadDetailPanel from './thread_detail_panel.svelte'
import { app } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { resetThreadDetailCacheForTest } from './thread_detail_cache.js'
import { __resetDescendantCloseForTest, cancelDescendantClose, descendantClose } from './descendant_close.svelte.js'

// A transient tab (no catalog id) is deliberately never fetched, so these
// render without touching the network.
function transientTab(overrides = {}) {
  return {
    id: 1,
    catalogId: '',
    sessionId: '',
    provider: 'codex',
    nativeId: 'native-1',
    title: 'Pane mechanics',
    cwd: '/workspace/nano',
    model: 'gpt-5.6-sol',
    effort: 'high',
    ...overrides,
  }
}

afterEach(() => {
  cleanup()
  app.sessions = []
  explorer.dataset = []
  cancelDescendantClose()
  __resetDescendantCloseForTest()
  vi.unstubAllGlobals()
  vi.useRealTimers()
  resetThreadDetailCacheForTest()
})

describe('thread detail panel layout', () => {
  it('fills its pane when standalone', () => {
    const { container } = render(ThreadDetailPanel, { tab: transientTab(), standalone: true })

    const panel = container.querySelector('.thread-detail-panel')
    expect(panel).not.toBeNull()
    expect(panel.classList.contains('standalone')).toBe(true)
  })

  it('stays a narrow companion when docked beside a transcript', () => {
    const { container } = render(ThreadDetailPanel, { tab: transientTab(), standalone: false })

    const panel = container.querySelector('.thread-detail-panel')
    expect(panel.classList.contains('standalone')).toBe(false)
  })

  it('defaults to the docked width rather than silently filling the pane', () => {
    const { container } = render(ThreadDetailPanel, { tab: transientTab() })

    expect(container.querySelector('.thread-detail-panel').classList.contains('standalone')).toBe(false)
  })

  it('renders the thread identity without a detail fetch', () => {
    const { container, getByText } = render(ThreadDetailPanel, { tab: transientTab(), standalone: true })

    expect(getByText('Pane mechanics')).toBeTruthy()
    expect(container.querySelector('.thread-detail-heading')).not.toBeNull()
    expect(vi.isMockFunction(globalThis.fetch)).toBe(false)
  })

  it('routes detail-panel detach through descendant selection', async () => {
    app.sessions = [{
      id: 'session-1', provider: 'codex', threadId: 'native-1', hostedThreadId: 'nht_parent',
      status: 'ready', turnActive: false,
    }, { id: 'child-session', provider: 'codex', threadId: 'child-native', hostedThreadId: 'nht_child', status: 'ready' }]
    explorer.dataset = [{
      id: 'thread-1', provider: 'codex', nativeId: 'native-1', hostedThreadId: 'nht_parent',
      title: 'Parent', metadata: {},
      runtime: { attached: true, sessionId: 'session-1' },
    }, { id: 'child', provider: 'codex', nativeId: 'child-native', hostedThreadId: 'nht_child', invokerHostedThreadId: 'nht_parent', runtime: { attached: true, sessionId: 'child-session' } }]
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true, status: 200, text: async () => '', json: async () => explorer.dataset[0],
    })))
    const view = render(ThreadDetailPanel, {
      tab: transientTab({ catalogId: 'thread-1', sessionId: 'session-1', title: 'Parent' }),
      standalone: true,
    })

    await fireEvent.click(view.getByRole('button', { name: 'Detach' }))

    expect(descendantClose.request).toMatchObject({
      parentSessionId: 'session-1',
    })
    expect(fetch.mock.calls.some(([, options]) => options?.method === 'DELETE')).toBe(false)
  })

  // The live catalog replaces explorer.dataset and bumps catalogVersion several
  // times a second for other threads. Each tick once refetched every open
  // panel's detail, about 30 reads a second from two tabs. Only this thread's
  // own row changing may revalidate it.
  it('refetches only when its own thread changes, not on other threads\' catalog ticks', async () => {
    vi.useFakeTimers()
    const detailRead = vi.fn(async () => ({
      ok: true, status: 200, json: async () => ({ id: 'thread-1', provider: 'codex', nativeId: 'native-1', metadata: {}, currentResolution: { state: 'current' } }),
    }))
    vi.stubGlobal('fetch', detailRead)
    const own = { id: 'thread-1', provider: 'codex', nativeId: 'native-1', metadata: {} }
    explorer.dataset = [own, { id: 'other', provider: 'codex', nativeId: 'other-native', metadata: {} }]
    explorer.catalogVersion = 1
    render(ThreadDetailPanel, { tab: transientTab({ catalogId: 'thread-1' }), standalone: true })
    await vi.advanceTimersByTimeAsync(1000)
    const opened = detailRead.mock.calls.length

    for (let tick = 2; tick <= 11; tick += 1) {
      explorer.dataset = [own, { id: 'other', provider: 'codex', nativeId: 'other-native', metadata: {}, updatedAt: String(tick) }]
      explorer.catalogVersion = tick
      await vi.advanceTimersByTimeAsync(100)
    }
    await vi.advanceTimersByTimeAsync(1000)
    expect(detailRead).toHaveBeenCalledTimes(opened)

    explorer.dataset = [{ ...own, updatedAt: 'now' }, explorer.dataset[1]]
    await vi.advanceTimersByTimeAsync(1000)
    expect(detailRead).toHaveBeenCalledTimes(opened + 1)
  })
})
