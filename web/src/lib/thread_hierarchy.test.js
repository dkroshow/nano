// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render } from '@testing-library/svelte'
import CompactThreadRow from './CompactThreadRow.svelte'
import ThreadRowItem from './ThreadRowItem.svelte'
import { projectSidebar } from './sidebar_projection.js'
import { applyView, collapseWorkerGroups, threadIdentity } from '../explorer_local.js'
import { explorer } from '../explorer.svelte.js'
import {
  resetWorkerDisclosure, restoreWorkerDisclosure, setAllWorkerGroups, toggleHistoricalWorkerGroup,
  toggleWorkerGroup, workerDisclosure, workerGroupIsExpanded, WORKER_DISCLOSURE_STORAGE_KEY,
} from './worker_disclosure.svelte.js'

const worker = {
  id: 'nth_worker',
  provider: 'claude',
  nativeId: 'agent-worker',
  parentNativeId: 'parent-session',
  kind: 'subagent',
  preview: 'Inspect parser',
  fleet: { label: 'working' },
  metadata: {},
  source: { status: 'present' },
}

const parent = {
  id: 'nth_parent',
  provider: 'claude',
  nativeId: 'parent-session',
  kind: 'thread',
  title: 'Supervisor',
  metadata: {},
  source: { status: 'present' },
}

const view = {
  projects: null,
  providers: null,
  models: null,
  efforts: null,
  createdRange: null,
  updatedRange: null,
  sort: 'liveness-updated',
  dir: 'desc',
}

afterEach(() => {
  cleanup()
  resetWorkerDisclosure() // module-level state and localStorage outlive a render
  explorer.historyScopes = {}
  vi.unstubAllGlobals()
})

describe('visible worker lineage', () => {
  it('defaults every group closed without a saved view while retaining global expansion', () => {
    resetWorkerDisclosure()
    expect(workerDisclosure.expanded).toEqual([])
    expect(workerDisclosure.historicalExpanded).toEqual([])
    expect(workerGroupIsExpanded(parent)).toBe(false)

    toggleWorkerGroup(parent)
    expect(workerDisclosure.expanded).toEqual([threadIdentity(parent)])
    expect(workerGroupIsExpanded(parent)).toBe(true)

    setAllWorkerGroups(false)
    expect(workerDisclosure.expanded).toEqual([])
    setAllWorkerGroups(true)
    expect(workerDisclosure.expanded).toEqual(['*'])
    expect(workerDisclosure.historicalExpanded).toEqual([])
  })

  it('restores current subagent disclosure after a reload but keeps historical closed', () => {
    resetWorkerDisclosure()
    toggleWorkerGroup(parent)
    toggleHistoricalWorkerGroup(parent)

    expect(JSON.parse(localStorage.getItem(WORKER_DISCLOSURE_STORAGE_KEY))).toEqual({
      version: 1,
      expanded: [threadIdentity(parent)],
    })

    workerDisclosure.expanded = []
    workerDisclosure.historicalExpanded = []
    restoreWorkerDisclosure()
    expect(workerGroupIsExpanded(parent)).toBe(true)
    expect(workerDisclosure.historicalExpanded).toEqual([])
  })

  it('restores global expansion and its individual closed exceptions', () => {
    resetWorkerDisclosure()
    setAllWorkerGroups(true)
    toggleWorkerGroup(parent)

    workerDisclosure.expanded = []
    restoreWorkerDisclosure()
    expect(workerDisclosure.expanded).toEqual(['*', `!${threadIdentity(parent)}`])
    expect(workerGroupIsExpanded(parent)).toBe(false)
    expect(workerGroupIsExpanded({ ...parent, nativeId: 'another-parent' })).toBe(true)
  })

  it('fails closed for invalid saved views and bounds restored group keys', () => {
    localStorage.setItem(WORKER_DISCLOSURE_STORAGE_KEY, '{not-json')
    restoreWorkerDisclosure()
    expect(workerDisclosure.expanded).toEqual([])

    localStorage.setItem(WORKER_DISCLOSURE_STORAGE_KEY, JSON.stringify({ version: 2, expanded: ['*'] }))
    restoreWorkerDisclosure()
    expect(workerDisclosure.expanded).toEqual([])

    localStorage.setItem(WORKER_DISCLOSURE_STORAGE_KEY, JSON.stringify({
      version: 1,
      expanded: ['*', 'x'.repeat(513), ...Array.from({ length: 300 }, (_, index) => `claude:parent-${index}`)],
    }))
    restoreWorkerDisclosure()
    expect(workerDisclosure.expanded).toHaveLength(256)
    expect(workerDisclosure.expanded[0]).toBe('*')
    expect(workerDisclosure.expanded).not.toContain('x'.repeat(513))
  })

  it('collapses workers by default and uses the lower-line count to reveal them', async () => {
    const arranged = applyView([worker, parent], view)
    expect(arranged.map((row) => row.nativeId)).toEqual(['parent-session', 'agent-worker'])

    const sidebarRows = projectSidebar(arranged, [])
    const sidebarWorker = sidebarRows[1]
    const collapsed = collapseWorkerGroups(sidebarRows)
    expect(collapsed.map((row) => row.nativeId)).toEqual(['parent-session'])
    expect(collapseWorkerGroups(sidebarRows, [threadIdentity(parent)]).map((row) => row.nativeId)).toEqual([
      'parent-session', 'agent-worker',
    ])

    const supervisor = render(CompactThreadRow, { props: { row: collapsed[0] } })
    const toggle = supervisor.getByRole('button', { name: 'Show subagents of Supervisor' })
    // The roll-up sits on the working-directory line; on the title line it
    // pushed the thread's name out of a rail this narrow.
    expect(toggle.closest('.t-cwd-line')).not.toBeNull()
    expect(supervisor.container.querySelector('.t-title .worker-summary')).toBeNull()
    expect(toggle.textContent.trim()).toContain('subagents: 1 (0) · 1 working')
    expect(toggle.getAttribute('aria-expanded')).toBe('false')
    expect(supervisor.container.querySelector('.t-dot.working')).toBeNull()
    await fireEvent.click(toggle)
    expect(workerDisclosure.expanded).toEqual([threadIdentity(parent)])
    supervisor.unmount()

    const compact = render(CompactThreadRow, { props: { row: sidebarWorker } })
    expect(compact.container.querySelector('.tree-branch-lines')?.getAttribute('title')).toBe('Child of parent-session')
    expect(compact.container.querySelectorAll('.tree-branch-slot')).toHaveLength(1)
    expect(compact.container.querySelector('.tree-branch-slot.fork.last')).not.toBeNull()
    expect(compact.container.querySelector('.role-badge')).toBeNull()
    expect(compact.container.querySelector('.t-title-text')?.textContent).toBe('Inspect parser')
    expect(compact.container.querySelector('.t-dot.working')).not.toBeNull()
    compact.unmount()

    const expanded = render(ThreadRowItem, { props: { item: arranged[1] } })
    expect(expanded.container.querySelector('.tree-branch-lines')?.getAttribute('title')).toBe('Child of parent-session')
  })

  it('keeps hierarchy and assignments without rendering legacy role pills', () => {
    const apiParent = {
      ...parent, provider: 'codex', nativeId: 'codex-native', hostedThreadId: 'nht_parent',
    }
    const apiWorker = {
      ...worker,
      provider: 'claude', nativeId: 'claude-native', hostedThreadId: 'nht_worker', kind: 'thread',
      parentNativeId: null, invokerHostedThreadId: 'nht_parent',
      invokerNativeThreadId: 'codex-native', invokerProvider: 'codex',
      runtime: { attached: true, sessionId: 'session-worker', status: 'ready' },
    }
    const unassigned = applyView([apiWorker, apiParent], view)
    const sidebarWorker = projectSidebar(unassigned, [])[1]

    const compact = render(CompactThreadRow, { props: { row: sidebarWorker } })
    expect(compact.container.querySelector('.tree-branch-lines')?.getAttribute('title')).toBe('Child of codex-native')
    expect(compact.container.querySelector('.role-badge')).toBeNull()
    compact.unmount()

    const expanded = render(ThreadRowItem, { props: { item: unassigned[1] } })
    expect(expanded.container.querySelector('.tree-branch-lines')?.getAttribute('title')).toBe('Child of codex-native')
    expect(expanded.container.querySelector('.role-badge')).toBeNull()
    expanded.unmount()

    const unavailable = applyView([{
      ...apiWorker, assignmentUnavailable: true, assignment: { assignmentRole: 'worker' },
    }, apiParent], view)
    const unavailableCompact = render(CompactThreadRow, { props: { row: projectSidebar(unavailable, [])[1] } })
    expect(unavailableCompact.container.querySelector('.role-badge')).toBeNull()
    unavailableCompact.unmount()

    const assigned = applyView([{
      ...apiWorker,
      assignment: { assignmentRole: 'worker', focus: 'Implement exactly this', auditTargets: [{ hostedThreadId: 'nht_parent' }] },
    }, apiParent], view)
    const assignedRail = render(CompactThreadRow, { props: { row: projectSidebar(assigned, [])[1] } })
    const assignedExplorer = render(ThreadRowItem, { props: { item: assigned[1] } })
    expect(assignedRail.container.querySelector('.role-badge')).toBeNull()
    expect(assignedExplorer.container.querySelector('.role-badge')).toBeNull()
    expect(assignedRail.container.textContent).not.toContain('Implement exactly this')
    expect(assignedExplorer.container.textContent).not.toContain('nht_parent')
  })

  // subagentActivity rolls up the whole fetched window on purpose, so a count
  // is not proof the rail is carrying the rows. A caret there would promise an
  // expansion with nothing behind it.
  it('states the worker count without a caret when the rail has no rows to disclose', () => {
    const [supervisorOnly] = collapseWorkerGroups(projectSidebar(applyView([parent], view), []))
    expect(supervisorOnly.workersCollapsible).toBeUndefined()

    const row = {
      ...supervisorOnly,
      currentSubagentCount: 4,
      historicalAgentCount: 0,
      workingSubagentCount: 0,
    }
    const rendered = render(CompactThreadRow, { props: { row } })
    expect(rendered.queryByRole('button', { name: /subagents of/ })).toBeNull()
    expect(rendered.container.querySelector('.t-cwd-line .worker-summary')?.textContent).toBe('subagents: 4 (0) · 0 working')
  })

  it('offers historical subagents separately and keeps the choice shared across both surfaces', async () => {
    const historicalWorker = {
      ...worker,
      id: 'nth_historical_worker',
      nativeId: 'historical-worker',
      fleet: { label: 'idle' },
    }
    const arranged = applyView([historicalWorker, worker, parent], view)
    const railRows = projectSidebar(arranged, [])
    resetWorkerDisclosure()
    toggleWorkerGroup(parent)

    const currentRail = collapseWorkerGroups(
      railRows,
      workerDisclosure.expanded,
      workerDisclosure.historicalExpanded,
    )
    expect(currentRail.map((row) => row.nativeId)).toEqual(['parent-session', 'agent-worker'])
    expect(currentRail[0].currentSubagentCount).toBe(1)
    expect(currentRail[0].historicalAgentCount).toBe(1)
    expect(currentRail[0].workingSubagentCount).toBe(1)

    const compactParent = render(CompactThreadRow, { props: { row: currentRail[0] } })
    expect(compactParent.getByRole('button', { name: 'Hide subagents of Supervisor' }).textContent)
      .toContain('subagents: 1 (1) · 1 working')
    expect(compactParent.queryByRole('button', { name: 'Show historical subagents of Supervisor' })).toBeNull()
    compactParent.unmount()

    const compactTail = render(CompactThreadRow, { props: { row: currentRail[1] } })
    const historyToggle = compactTail.getByRole('button', { name: 'Show historical subagents of Supervisor' })
    expect(historyToggle.classList.contains('worker-history-row')).toBe(true)
    expect(historyToggle.textContent).toBe('Show historical (1)')
    await fireEvent.click(historyToggle)
    expect(workerDisclosure.historicalExpanded).toEqual([threadIdentity(parent)])
    compactTail.unmount()

    for (const rows of [railRows, arranged]) {
      expect(collapseWorkerGroups(
        rows,
        workerDisclosure.expanded,
        workerDisclosure.historicalExpanded,
      ).map((row) => row.nativeId)).toEqual(['parent-session', 'agent-worker', 'historical-worker'])
    }

    const explorerRows = collapseWorkerGroups(
      arranged,
      workerDisclosure.expanded,
      workerDisclosure.historicalExpanded,
    )
    const explorer = render(ThreadRowItem, { props: { item: explorerRows.at(-1) } })
    expect(explorer.getByRole('button', { name: 'Hide historical subagents of Supervisor' })).not.toBeNull()
  })

  it('retries a failed lazy-history request without closing the disclosure', async () => {
    const [supervisor] = collapseWorkerGroups(projectSidebar(applyView([{
      ...parent,
      sidebar: { active: true, historicalDescendantCount: 1, descendantActivity: { total: 1, active: 0, workers: 0 } },
    }], view), []))
    workerDisclosure.expanded = ['*']
    workerDisclosure.historicalExpanded = [threadIdentity(supervisor)]
    const [expanded] = collapseWorkerGroups([supervisor], workerDisclosure.expanded, workerDisclosure.historicalExpanded)
    explorer.historyScopes = { [expanded.id]: { state: 'loading', items: [], error: '' } }
    const loading = render(CompactThreadRow, { props: { row: expanded } })
    const loadingButton = loading.getByRole('button', { name: 'Hide historical subagents of Supervisor' })
    expect(loadingButton.textContent).toBe('Loading historical (1)')
    expect(loadingButton.getAttribute('aria-busy')).toBe('true')
    loading.unmount()

    explorer.historyScopes = { [expanded.id]: { state: 'error', items: [], error: 'temporary' } }
    const fetchMock = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ items: [], nextCursor: null }),
    }))
    vi.stubGlobal('fetch', fetchMock)

    const compact = render(CompactThreadRow, { props: { row: expanded } })
    await fireEvent.click(compact.getByRole('button', { name: 'Retry historical subagents of Supervisor' }))

    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(workerDisclosure.historicalExpanded).toEqual([threadIdentity(supervisor)])
    expect(explorer.historyScopes[expanded.id].state).toBe('ready')
  })

  // The Explorer is an expansion of the compact view (the operator, 2026-07-29), so it
  // collapses the same groups from the same state: a group closed in the rail
  // must not spring open just because the rail was widened.
  it('collapses the same groups in the expanded Explorer, over shared state', async () => {
    const arranged = applyView([worker, parent], view)
    const supervisorKey = threadIdentity(parent)
    expect(collapseWorkerGroups(arranged).map((row) => row.nativeId)).toEqual(['parent-session'])

    const table = render(ThreadRowItem, { props: { item: collapseWorkerGroups(arranged)[0] } })
    const toggle = table.getByRole('button', { name: 'Show subagents of Supervisor' })
    expect(toggle.closest('.xc-title')).not.toBeNull()
    await fireEvent.click(toggle)
    expect(workerDisclosure.expanded).toEqual([supervisorKey])
    table.unmount()

    // The rail's row key and the table's catalog id differ for this supervisor;
    // only the shared identity key can open the group on both surfaces.
    const railRows = projectSidebar(arranged, [])
    expect(railRows[0].key).not.toBe(arranged[0].id)
    expect(collapseWorkerGroups(railRows, workerDisclosure.expanded).map((row) => row.nativeId)).toEqual([
      'parent-session', 'agent-worker',
    ])
    expect(collapseWorkerGroups(arranged, workerDisclosure.expanded).map((row) => row.nativeId)).toEqual([
      'parent-session', 'agent-worker',
    ])

    toggleWorkerGroup(arranged[0])
    expect(workerDisclosure.expanded).toEqual([])
  })

  // The Explorer's name cell stacks title / working directory / model exactly
  // as .t-main does, and both stacks are centred in one --row-h — so a badge on
  // a different line in one surface moves every line of the row on expand.
  it('carries the worker roll-up on the same line of the stack in both surfaces', () => {
    const [supervisor] = collapseWorkerGroups(applyView([worker, parent], view))
    const railRow = collapseWorkerGroups(projectSidebar(applyView([worker, parent], view), []))[0]

    const rail = render(CompactThreadRow, { props: { row: railRow } })
    const table = render(ThreadRowItem, { props: { item: supervisor } })

    for (const [surface, titleSel, cwdSel] of [
      [rail, '.t-title', '.t-cwd-line'],
      [table, '.xc-line', '.xc-cwd-line'],
    ]) {
      expect(surface.container.querySelector(`${titleSel} .worker-summary`)).toBeNull()
      expect(surface.container.querySelector(`${cwdSel} .worker-summary`)).not.toBeNull()
    }
  })

  it('keeps identical assignment hierarchy without legacy role pills', () => {
    const main = {
      ...parent, id: 'nth_main', nativeId: 'main-native', provider: 'codex', hostedThreadId: 'nht_main',
    }
    const coordinator = {
      ...parent, id: 'nth_coordinator', nativeId: 'coordinator-native', hostedThreadId: 'nht_coordinator',
      invokerHostedThreadId: 'nht_main', assignment: { assignmentRole: 'auditor' }, title: 'Audit coordinator',
    }
    const investigation = {
      ...worker, id: 'nth_investigation', nativeId: 'investigation-native', hostedThreadId: 'nht_investigation',
      kind: 'thread', invokerHostedThreadId: 'nht_coordinator', assignment: { assignmentRole: 'worker' },
    }
    const arranged = applyView([investigation, coordinator, main], view)
    const compactRow = projectSidebar(arranged, []).find((row) => row.nativeId === 'coordinator-native')
    const explorerItem = arranged.find((item) => item.nativeId === 'coordinator-native')
    const compactInvestigation = projectSidebar(arranged, []).find((row) => row.nativeId === 'investigation-native')
    const explorerInvestigation = arranged.find((item) => item.nativeId === 'investigation-native')
    const compact = render(CompactThreadRow, { props: { row: compactRow } })
    const expanded = render(ThreadRowItem, { props: { item: explorerItem } })
    const nestedCompact = render(CompactThreadRow, { props: { row: compactInvestigation } })
    const nestedExpanded = render(ThreadRowItem, { props: { item: explorerInvestigation } })

    for (const surface of [compact, expanded]) expect(surface.container.querySelector('.role-badge')).toBeNull()
    expect(compactRow.treeDepth).toBe(explorerItem.treeDepth)
    expect(compact.container.querySelector('.thread-row')?.classList.contains('worker-child')).toBe(true)
    expect(expanded.container.querySelector('.xrow')?.classList.contains('worker-child')).toBe(true)
    for (const surface of [nestedCompact, nestedExpanded]) {
      expect(surface.container.querySelectorAll('.tree-branch-slot')).toHaveLength(2)
      expect(surface.container.querySelector('.tree-branch-slot.ancestor')).not.toBeNull()
      expect(surface.container.querySelector('.tree-branch-slot.fork.last')).not.toBeNull()
    }
  })
})
