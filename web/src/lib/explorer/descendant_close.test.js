import { describe, expect, it } from 'vitest'
import { closeDescendantRows, closeSelection, parentCloseLabel } from './descendant_close.js'

const NOW = Date.UTC(2026, 7, 11, 12, 0, 0)

const parent = { id: 'parent', provider: 'codex', nativeId: 'main', hostedThreadId: 'nht_parent' }
const coordinator = {
  id: 'coordinator', provider: 'claude', nativeId: 'review', hostedThreadId: 'nht_review',
  invokerHostedThreadId: 'nht_parent', title: 'Audit coordinator',
  assignment: { assignmentRole: 'auditor' },
  updatedAt: new Date(NOW - 90_000).toISOString(),
}
const worker = {
  id: 'worker', provider: 'codex', nativeId: 'implementation', hostedThreadId: 'nht_worker',
  invokerHostedThreadId: 'nht_review', title: 'Implementation', assignment: { assignmentRole: 'worker' },
  runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'working' }, updatedAt: new Date(NOW - 2_000).toISOString(),
}
const historical = {
  id: 'history', provider: 'claude', nativeId: 'old-review', hostedThreadId: 'nht_history',
  invokerHostedThreadId: 'nht_parent', title: 'Earlier review', updatedAt: new Date(NOW - 3_600_000).toISOString(),
}
const sessions = [
  { status: 'ready', id: 'session-review', provider: 'claude', threadId: 'review', hostedThreadId: 'nht_review', turnActive: false, threadStatus: 'idle' },
  { status: 'ready', id: 'session-worker', provider: 'codex', threadId: 'implementation', hostedThreadId: 'nht_worker', turnActive: true, threadStatus: 'working' },
]

describe('close descendant tree', () => {
  it('decorates raw recursive descendants without legacy role pills', () => {
    const nodes = closeDescendantRows(parent, [parent, coordinator, worker, historical], sessions, NOW)
    expect(nodes.map((node) => [node.label, node.depth, node.closable, node.activity])).toEqual([
      // Its worker's turn is the worker's own; the idle coordinator is not working.
      ['Audit coordinator', 0, true, 'last active 1m'],
      ['Implementation', 1, true, 'working'],
      ['Earlier review', 0, false, 'last active 1h'],
    ])
    expect(nodes[0].row.subagentActivity).toMatchObject({ total: 1, active: 1, workers: 1 })
    expect(nodes[0]).not.toHaveProperty('badges')
    expect(nodes[1]).not.toHaveProperty('badges')
  })

  it('selects only explicit closable agents and reports all/indeterminate state', () => {
    const nodes = closeDescendantRows(parent, [parent, coordinator, worker, historical], sessions, NOW)
    expect(closeSelection(nodes, [])).toMatchObject({ selectedIds: [], checked: false, indeterminate: false, workingCount: 0 })
    expect(closeSelection(nodes, ['session-review'])).toMatchObject({
      selectedIds: ['session-review'], checked: false, indeterminate: true, workingCount: 0,
    })
    expect(closeSelection(nodes, ['session-review', 'session-worker'])).toMatchObject({
      checked: true, indeterminate: false, workingCount: 1,
    })
    // Selecting an intermediate coordinator leaves its unselected worker out.
    expect(closeSelection(nodes, ['session-review']).selectedIds).toEqual(['session-review'])
  })

  it("keeps a waiting descendant's current activity distinct from last-active time", () => {
    const waiting = {
      ...worker,
      id: 'waiting-worker', nativeId: 'waiting-implementation', hostedThreadId: 'nht_waiting',
      invokerHostedThreadId: 'nht_parent', runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'needs_you', waiting: true },
    }
    const nodes = closeDescendantRows(parent, [parent, waiting], [], NOW)
    expect(nodes[0]).toMatchObject({ activity: 'waiting', working: false, closable: false })
  })

  it('uses the provider-aware parent action label', () => {
    expect(parentCloseLabel('codex', 0)).toBe('Detach parent')
    expect(parentCloseLabel('codex', 2)).toBe('Detach parent and close 2 agents')
    expect(parentCloseLabel('claude', 1)).toBe('End parent session and close 1 agent')
  })
})
