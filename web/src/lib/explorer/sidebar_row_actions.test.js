// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte'
import CompactThreadRow from './CompactThreadRow.svelte'
import ThreadRowItem from './ThreadRowItem.svelte'
import ThreadQuickDetail from './ThreadQuickDetail.svelte'
import { projectSidebar } from './sidebar_projection.js'
import { arrangeThreadHierarchy } from '../../explorer_local.js'
import { app } from '../../app.svelte.js'
import { __resetExplorerPageCacheForTest, explorer } from '../../explorer.svelte.js'
import { closeQuickDetail, openQuickDetail } from './quick_detail.svelte.js'
import { __resetDescendantCloseForTest, cancelDescendantClose, descendantClose } from './descendant_close.svelte.js'

function catalogRow() {
  return {
    id: 'nth_1',
    provider: 'codex',
    nativeId: 'native-1',
    title: 'Timer stays put',
    metadata: {},
    runtime: { status: 'ready', attached: true, sessionId: 's1', turnActive: true, threadStatus: 'working', turnStartedAtMs: Date.now() - 3_000 },
  }
}

beforeEach(() => {
  app.sessions = [{ id: 's1', provider: 'codex', threadId: 'native-1', cwd: '/workspace', status: 'ready', turnActive: true, threadStatus: 'working' }]
  explorer.dataset = [catalogRow()]
  __resetExplorerPageCacheForTest()
  __resetDescendantCloseForTest()
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => ({}) })))
})

afterEach(() => {
  cleanup()
  closeQuickDetail()
  cancelDescendantClose()
  __resetDescendantCloseForTest()
  vi.unstubAllGlobals()
})

it('keeps the working timer before ⋮ and moves Close session into its popup', async () => {
  const [row] = projectSidebar([catalogRow()], app.sessions)
  const rail = render(CompactThreadRow, { props: { row } })
  const timer = rail.container.querySelector('.t-age.t-working')
  const star = rail.getByRole('button', { name: 'Star thread' })
  const menu = rail.getByRole('button', { name: 'Thread details and actions' })

  expect(timer).not.toBeNull()
  expect(timer.compareDocumentPosition(star) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  expect(star.compareDocumentPosition(menu) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  expect(rail.container.querySelector('.t-close')).toBeNull()

  render(ThreadQuickDetail)
  await fireEvent.click(menu)
  await waitFor(() => expect(document.querySelector('.quick-detail')).not.toBeNull())
  expect(document.querySelector('.quick-detail').textContent).toContain('Close session')
})

it('opens the same descendant selection before Close session in quick details', async () => {
  const parent = { ...catalogRow(), hostedThreadId: 'nht-parent' }
  const child = {
    id: 'nth_child', provider: 'claude', nativeId: 'child-native', hostedThreadId: 'nht-child',
    invokerHostedThreadId: 'nht-parent', title: 'Audit child', metadata: {},
    assignment: { assignmentRole: 'auditor' },
  }
  explorer.dataset = [parent, child]
  app.sessions = [
    { id: 's1', provider: 'codex', threadId: 'native-1', hostedThreadId: 'nht-parent', cwd: '/workspace', status: 'ready' },
    { id: 's2', provider: 'claude', threadId: 'child-native', hostedThreadId: 'nht-child', cwd: '/workspace', status: 'ready' },
  ]
  const [row] = projectSidebar(explorer.dataset, app.sessions)
  const rail = render(CompactThreadRow, { props: { row } })
  render(ThreadQuickDetail)

  await fireEvent.click(rail.getByRole('button', { name: 'Thread details and actions' }))
  await waitFor(() => expect(document.querySelector('.quick-detail')).not.toBeNull())
  await fireEvent.click(document.querySelector('.quick-detail-danger'))

  await waitFor(() => expect(descendantClose.request?.nodes.map((node) => node.label)).toEqual(['Audit child']))
  expect(fetch).not.toHaveBeenCalledWith('/api/sessions/s1', { method: 'DELETE' })
  expect(document.querySelector('.quick-detail')).not.toBeNull()
})

it('keeps Bulk close tree for a historical row whose descendant count exists only in the projection', async () => {
  const parent = {
    ...catalogRow(),
    runtime: undefined,
    hostedThreadId: 'nht-parent',
    title: 'Exited historical tree',
  }
  explorer.dataset = [parent]
  app.sessions = []
  const [historicalRow] = projectSidebar(explorer.dataset, app.sessions)
  const projectedRow = {
    ...historicalRow,
    subagentActivity: { total: 23, active: 0, workers: 0 },
  }
  expect(explorer.dataset[0].subagentActivity).toBeUndefined()

  const rail = render(CompactThreadRow, { props: { row: projectedRow } })
  render(ThreadQuickDetail)
  await fireEvent.click(rail.getByRole('button', { name: 'Thread details and actions' }))

  await waitFor(() => expect(document.querySelector('.quick-detail')).not.toBeNull())
  expect(document.querySelector('.quick-detail').textContent).toContain('Bulk close tree')
})

it('sets a role and reparents the thread under the related hosted thread', async () => {
  const original = { ...catalogRow(), id: 'nth_original', hostedThreadId: 'nht-original', title: 'Original parent' }
  const next = {
    id: 'nth_next', provider: 'claude', nativeId: 'native-next', hostedThreadId: 'nht-next',
    title: 'New parent', metadata: {},
  }
  const child = {
    id: 'nth_child', provider: 'codex', nativeId: 'native-child', hostedThreadId: 'nht-child',
    invokerHostedThreadId: 'nht-original', title: 'Child', metadata: {},
  }
  const moved = {
    ...child,
    assignment: {
      eventId: 9,
      assignmentRole: 'auditor',
      focus: 'Review the parent work',
      relatedHostedThreadId: 'nht-next',
      auditTargets: [{ hostedThreadId: 'nht-next' }],
    },
  }
  explorer.dataset = [original, next, child]
  app.sessions = []
  let updateBody = null
  vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
    if (init.method === 'PUT') {
      updateBody = JSON.parse(init.body)
      return { ok: true, json: async () => ({ assignment: moved.assignment }), text: async () => '', headers: { get: () => null } }
    }
    if (String(url).startsWith('/api/v1/threads?')) {
      return { ok: true, json: async () => ({ items: [original, next, moved], nextCursor: null }), headers: { get: () => null } }
    }
    return { ok: true, json: async () => ({}), headers: { get: () => null } }
  }))

  render(ThreadQuickDetail)
  openQuickDetail({ catalogId: child.id, provider: child.provider, nativeId: child.nativeId })
  const roleButton = () => [...document.querySelectorAll('.quick-detail-actions button')]
    .find((button) => button.textContent === 'Set role')
  await waitFor(() => expect(roleButton()).not.toBeUndefined())
  await fireEvent.click(roleButton())
  await fireEvent.change(document.querySelector('[aria-label="Set as"]'), { target: { value: 'auditor' } })
  await fireEvent.input(document.querySelector('[aria-label="Related thread"]'), { target: { value: 'nht-next' } })
  await fireEvent.input(document.querySelector('[aria-label="Role focus"]'), { target: { value: 'Review the parent work' } })
  await fireEvent.click(document.querySelector('.quick-detail-role-actions .xname-save-btn'))

  await waitFor(() => expect(updateBody?.assignment).toMatchObject({
    assignmentRole: 'auditor',
    relatedHostedThreadId: 'nht-next',
    focus: 'Review the parent work',
  }))
  expect(updateBody.assignment.auditTargets).toEqual([{ hostedThreadId: 'nht-next' }])
  await waitFor(() => expect(arrangeThreadHierarchy(explorer.dataset).map((row) => row.id)).toEqual([
    'nth_original', 'nth_next', 'nth_child',
  ]))
  expect(arrangeThreadHierarchy(explorer.dataset)[2]).toMatchObject({ treeDepth: 1 })
})

// After a restart Nano leaves every thread without open work resting, and a
// browser brings back what it shows: a shown row, an expanded child included,
// asks for its thread once per resting spell, however often it re-renders,
// and asks again after a failed request.
it('asks once for a resting thread its sidebar row shows', async () => {
  app.sessions = []
  const resting = { ...catalogRow(), hostedThreadId: 'nht-resting', runtime: { detached: true, status: 'resting' } }
  const [row] = projectSidebar([resting], [])
  const rail = render(CompactThreadRow, { props: { row } })
  const prioritized = () => fetch.mock.calls.filter(([url]) => String(url) === '/api/v1/hosted-threads/nht-resting/prioritize')
  await waitFor(() => expect(prioritized()).toHaveLength(1))
  await rail.rerender({ row: { ...row } })
  expect(prioritized()).toHaveLength(1)

  // Attached, then resting again after a later restart: it asks again.
  await rail.rerender({ row: { ...row, runtime: { attached: true, status: 'ready' } } })
  await rail.rerender({ row: { ...row } })
  await waitFor(() => expect(prioritized()).toHaveLength(2))

  // A failed request re-arms: the next render asks again.
  await rail.rerender({ row: { ...row, runtime: { attached: true, status: 'ready' } } })
  fetch.mockImplementationOnce(async () => ({ ok: false, json: async () => ({}) }))
  await rail.rerender({ row: { ...row } })
  await waitFor(() => expect(prioritized()).toHaveLength(3))
  await new Promise((resolve) => setTimeout(resolve))
  await rail.rerender({ row: { ...row, title: 'rerendered' } })
  await waitFor(() => expect(prioritized()).toHaveLength(4))
})
