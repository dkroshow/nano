// @vitest-environment jsdom
//
// The quick-detail presentation toggle: it PUTs the explicit next mode to the
// hosted-thread endpoint and patches both local stores so the sidebar reacts
// before the runtime SSE confirmation.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import ThreadQuickDetail from './ThreadQuickDetail.svelte'
import { app } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { closeQuickDetail, openQuickDetail } from './quick_detail.svelte.js'
import { __resetBulkCloseForTest, bulkClose, cancelBulkClose } from './bulk_close.svelte.js'

const item = {
  id: 'nth_a', provider: 'codex', nativeId: 'nat-a', kind: 'thread', title: 'Thread A',
  hostedThreadId: 'nht_a',
  metadata: { cwd: { state: 'known', value: '/workspace/nano', evidence: 'session_meta' } },
  createdAt: '2026-07-31T08:00:00Z', updatedAt: '2026-07-31T12:00:00Z',
  runtime: { attached: true, sessionId: 's_a', status: 'ready', turnActive: false, presentationMode: 'shared' },
}

let fetchMock

beforeEach(() => {
  fetchMock = vi.fn(async (url, init) => {
    if (String(url).includes('/family/close') && init?.method !== 'POST') {
      const body = { rootHostedThreadId: 'nht_a', memberCount: 2, members: [
        { hostedThreadId: 'nht_a', provider: 'codex', nativeThreadId: 'nat-a', depth: 0, status: 'exited', working: false, starred: false },
        { hostedThreadId: 'nht_b', provider: 'codex', nativeThreadId: 'nat-b', depth: 1, status: 'ready', working: true, starred: false },
      ] }
      return { ok: true, status: 200, text: async () => JSON.stringify(body), json: async () => body }
    }
    if (String(url).includes('/presentation')) {
      return { ok: true, status: 200, text: async () => '', json: async () => ({ presentationMode: JSON.parse(init.body).presentationMode }) }
    }
    if (String(url).includes('/sidebar-placement')) {
      return { ok: true, status: 200, text: async () => '', json: async () => ({ sidebarPlacement: JSON.parse(init.body).sidebarPlacement }) }
    }
    if (String(url).includes('/identity/resolve')) {
      return { ok: true, status: 200, json: async () => ({ displayName: 'work-intake agent (standing)', provider: 'codex', nativeThreadId: 'nat-parent', hostedThreadId: 'nht_parent' }) }
    }
    return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}), text: async () => '' }
  })
  vi.stubGlobal('fetch', fetchMock)
  app.sessions = [{ id: 's_a', provider: 'codex', threadId: 'nat-a', hostedThreadId: 'nht_a', presentationMode: 'shared', status: 'ready', turnActive: false }]
  explorer.dataset = [item]
})

afterEach(() => {
  cancelBulkClose()
  __resetBulkCloseForTest()
  closeQuickDetail()
  cleanup()
  vi.unstubAllGlobals()
  explorer.activeItems = []
  explorer.windowItems = []
  explorer.historyScopes = {}
})

describe('quick-detail bulk close', () => {
  it('offers the tree action for an already-closed historical parent', async () => {
    app.sessions = []
    explorer.dataset = [{
      ...item,
      runtime: undefined,
      hostedStatus: 'exited',
      starred: true,
      sidebar: {
        active: true,
        descendantActivity: { total: 2, active: 0, workers: 1 },
      },
    }]
    openQuickDetail({ catalogId: 'nth_a', provider: 'codex', nativeId: 'nat-a', closable: false })
    const { getByRole, queryByRole } = render(ThreadQuickDetail)
    expect(queryByRole('button', { name: 'Close session' })).toBeNull()

    await fireEvent.click(getByRole('button', { name: 'Bulk close tree' }))

    await waitFor(() => expect(bulkClose.request?.rootHostedThreadId).toBe('nht_a'))
  })
})

// Catches a stored thread that Nano is reattaching after a restart being
// labelled detached, and a genuinely detached thread losing that label.
// Boundary: the rendered quick-detail status for each runtime overlay.
describe('quick-detail status after a restart', () => {
  for (const [runtime, label] of [
    [{ attached: false, detached: true, status: 'starting' }, 'Starting after restart · durable history'],
    [{ attached: false, detached: true }, 'Detached · durable history'],
  ]) {
    it(`shows ${label}`, () => {
      app.sessions = []
      explorer.dataset = [{ ...item, runtime }]
      openQuickDetail({ catalogId: 'nth_a', provider: 'codex', nativeId: 'nat-a', detached: true })
      const { getByText } = render(ThreadQuickDetail)
      expect(getByText(label)).toBeTruthy()
    })
  }
})

describe('quick-detail presentation toggle', () => {
  it('moves a shared thread to background and patches local state', async () => {
    openQuickDetail({ catalogId: 'nth_a', provider: 'codex', nativeId: 'nat-a', sessionId: 's_a' })
    const { getByRole } = render(ThreadQuickDetail)
    await fireEvent.click(getByRole('button', { name: 'Move to background' }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([url]) => String(url).includes('/presentation'))
      expect(call).toBeTruthy()
      expect(call[0]).toBe('/api/v1/hosted-threads/nht_a/presentation')
      expect(call[1].method).toBe('PUT')
      expect(JSON.parse(call[1].body)).toEqual({ presentationMode: 'background' })
    })
    await waitFor(() => {
      expect(app.sessions[0].presentationMode).toBe('background')
      expect(explorer.dataset[0].runtime.presentationMode).toBe('background')
    })
    // The label follows the new mode, offering the way back.
    await waitFor(() => expect(getByRole('button', { name: 'Bring to sidebar' })).toBeTruthy())
  })

  it('surfaces the server envelope message and changes nothing on failure', async () => {
    fetchMock.mockImplementation(async (url) => String(url).includes('/presentation')
      ? {
          ok: false,
          status: 400,
          text: async () => JSON.stringify({
            error: {
              code: 'invalid_request',
              message: 'body must be {"presentationMode":"shared"} or {"presentationMode":"background"}',
            },
          }),
        }
      : { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}), text: async () => '' })
    openQuickDetail({ catalogId: 'nth_a', provider: 'codex', nativeId: 'nat-a', sessionId: 's_a' })
    const { getByRole, getByText } = render(ThreadQuickDetail)
    await fireEvent.click(getByRole('button', { name: 'Move to background' }))
    await waitFor(() => expect(getByText('body must be {"presentationMode":"shared"} or {"presentationMode":"background"}')).toBeTruthy())
    expect(app.sessions[0].presentationMode).toBe('shared')
  })
})

describe('quick-detail sidebar placement', () => {
  it('shows clickable invocation lineage and makes an unassigned thread standalone', async () => {
    const nested = {
      ...item,
      invokerHostedThreadId: 'nht_parent', invokerNativeThreadId: 'nat-parent', invokerProvider: 'codex',
      sidebarPlacement: 'invoker',
    }
    explorer.dataset = [nested]
    explorer.activeItems = [nested]
    explorer.windowItems = [nested]
    explorer.historyScopes = { parent: { state: 'ready', items: [nested] } }
    openQuickDetail({ catalogId: 'nth_a', provider: 'codex', nativeId: 'nat-a', sessionId: 's_a' })
    const { getByRole, getByText } = render(ThreadQuickDetail)

    await waitFor(() => expect(getByRole('button', { name: /work-intake agent \(standing\)/ })).toBeTruthy())
    expect(getByText('Under invoker')).toBeTruthy()
    await fireEvent.click(getByRole('button', { name: 'Make standalone' }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([url]) => String(url).includes('/sidebar-placement'))
      expect(call[0]).toBe('/api/v1/hosted-threads/nht_a/sidebar-placement')
      expect(call[1].method).toBe('PUT')
      expect(JSON.parse(call[1].body)).toEqual({ sidebarPlacement: 'standalone' })
    })
    await waitFor(() => {
      expect(explorer.dataset[0].sidebarPlacement).toBe('standalone')
      expect(app.sessions[0].sidebarPlacement).toBe('standalone')
      expect(explorer.activeItems[0].sidebarPlacement).toBe('standalone')
      expect(explorer.windowItems[0].sidebarPlacement).toBe('standalone')
      expect(explorer.historyScopes.parent.items[0].sidebarPlacement).toBe('standalone')
      expect(getByRole('button', { name: 'Nest under invoker' })).toBeTruthy()
    })
  })

  it('lets a current assignment control placement', () => {
    explorer.dataset = [{
      ...item,
      sidebarPlacement: 'standalone',
      assignment: { assignmentRole: 'worker', relatedHostedThreadId: 'nht_parent' },
    }]
    openQuickDetail({ catalogId: 'nth_a', provider: 'codex', nativeId: 'nat-a', sessionId: 's_a' })
    const { getByText, queryByRole } = render(ThreadQuickDetail)
    expect(getByText('Controlled by role assignment')).toBeTruthy()
    expect(queryByRole('button', { name: 'Nest under invoker' })).toBeNull()
  })
})

describe('quick-detail role editor names threads for people', () => {
  it('prefills the supervisor by name and submits its hosted id', async () => {
    const supervisor = {
      id: 'nth_b', provider: 'claude', nativeId: 'nat-b', kind: 'thread', title: 'Supervisor B',
      hostedThreadId: 'nht_b', name: { value: 'mini supervisor' },
      metadata: { cwd: { state: 'known', value: '/workspace/nano', evidence: 'session_meta' } },
      createdAt: '2026-07-31T08:00:00Z', updatedAt: '2026-07-31T12:00:00Z',
      runtime: { attached: true, sessionId: 's_b', status: 'ready', turnActive: false, presentationMode: 'shared' },
    }
    explorer.dataset = [{ ...item, invokerHostedThreadId: 'nht_b' }, supervisor]
    openQuickDetail({ catalogId: 'nth_a', provider: 'codex', nativeId: 'nat-a', sessionId: 's_a' })
    const { getByRole, getByLabelText, container } = render(ThreadQuickDetail)
    await fireEvent.click(getByRole('button', { name: 'Set role' }))

    const related = getByLabelText('Related thread')
    expect(related.value).toBe('mini supervisor (claude, nat-b)')
    expect(related.value).not.toContain('nht_')
    expect([...container.querySelectorAll('datalist option')].map((option) => option.value)).toEqual(['mini supervisor (claude, nat-b)'])

    await fireEvent.input(getByLabelText('Role focus'), { target: { value: 'Build the thing' } })
    await fireEvent.click(getByRole('button', { name: 'Save role' }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([url]) => String(url).includes('/assignment'))
      expect(call).toBeTruthy()
      expect(JSON.parse(call[1].body).assignment.relatedHostedThreadId).toBe('nht_b')
    })
  })
})

// Rendered controls own the operator-visible saved choices and failure state.
it('switches completion channels independently and preserves choices on a failed save', async () => {
  let choices = { inbox: 'send', prompt: 'send' }
  const originalFetch = fetchMock.getMockImplementation()
  fetchMock.mockImplementation(async (url, init) => {
    if (String(url) === '/api/v1/hosted-threads/nht_a') {
      return { ok: true, status: 200, json: async () => ({ subscriptions: choices }) }
    }
    if (String(url).endsWith('/subscriptions')) {
      choices = JSON.parse(init.body)
      return { ok: true, status: 200, json: async () => ({ subscriptions: choices }) }
    }
    return originalFetch(url, init)
  })
  explorer.dataset = [{ ...item, invokerHostedThreadId: 'nht_parent' }]
  openQuickDetail({ catalogId: item.id, provider: 'codex', nativeId: 'nat-a', sessionId: 's_a' })
  const { getByRole } = render(ThreadQuickDetail)
  await waitFor(() => expect(getByRole('button', { name: 'Prompt notices: On' }).disabled).toBe(false))
  await fireEvent.click(getByRole('button', { name: 'Prompt notices: On' }))
  await waitFor(() => expect(getByRole('button', { name: 'Prompt notices: Off' }).getAttribute('aria-pressed')).toBe('false'))
  expect(choices).toEqual({ inbox: 'send', prompt: 'none' })
  expect(getByRole('button', { name: 'Inbox notices: On' }).getAttribute('aria-pressed')).toBe('true')
  await fireEvent.click(getByRole('button', { name: 'Inbox notices: On' }))
  await waitFor(() => expect(getByRole('button', { name: 'Inbox notices: Off' }).disabled).toBe(false))
  expect(choices).toEqual({ inbox: 'none', prompt: 'none' })
  await fireEvent.click(getByRole('button', { name: 'Prompt notices: Off' }))
  await waitFor(() => expect(getByRole('button', { name: 'Prompt notices: On' }).disabled).toBe(false))
  expect(choices).toEqual({ inbox: 'none', prompt: 'send' })
  fetchMock.mockImplementation(async (url, init) => String(url).endsWith('/subscriptions')
    ? { ok: false, status: 503, text: async () => JSON.stringify({ error: 'Notifications unavailable' }) }
    : originalFetch(url, init))
  await fireEvent.click(getByRole('button', { name: 'Prompt notices: On' }))
  await waitFor(() => expect(getByRole('alert').textContent).toContain('Notifications unavailable'))
  expect(getByRole('button', { name: 'Prompt notices: On' }).getAttribute('aria-pressed')).toBe('true')
  app.sessions = []
  explorer.dataset = [{ ...item, hostedThreadId: '' }]
  await waitFor(() => expect(document.querySelector('[aria-label="Completion notifications"]')).toBeNull())
})

it('shows a failed notification read without inventing enabled settings', async () => {
  const originalFetch = fetchMock.getMockImplementation()
  fetchMock.mockImplementation(async (url, init) => String(url) === '/api/v1/hosted-threads/nht_a'
    ? { ok: false, status: 503, text: async () => JSON.stringify({ error: 'Settings unavailable' }) }
    : originalFetch(url, init))
  explorer.dataset = [{ ...item, invokerHostedThreadId: 'nht_parent' }]
  openQuickDetail({ catalogId: item.id, provider: 'codex', nativeId: 'nat-a', sessionId: 's_a' })
  const { getByRole } = render(ThreadQuickDetail)
  await waitFor(() => expect(getByRole('alert').textContent).toContain('Settings unavailable'))
  expect(getByRole('button', { name: 'Prompt notices: Loading…' }).disabled).toBe(true)
})
