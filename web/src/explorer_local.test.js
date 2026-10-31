import { describe, it, expect } from 'vitest'
import {
  applyView, arrangeThreadHierarchy, collapseWorkerGroups, computeFacets,
  decorateSubagentActivity, livenessRank, resolveThreadRole, threadIdentity,
  threadParentIdentityKeys, threadWorkIsActive, viewIsNarrowed,
} from './explorer_local.js'

const known = (value) => ({ state: 'known', value, evidence: 'native_durable' })
const unknown = () => ({ state: 'unknown', value: null })
const row = (id, { provider = 'codex', cwd, model, effort, created, updated } = {}) => ({
  id,
  provider,
  createdAt: created ?? null,
  updatedAt: updated ?? null,
  metadata: {
    cwd: cwd ? known(cwd) : unknown(),
    model: model ? known(model) : unknown(),
    effort: effort ? known(effort) : unknown(),
  },
})

const DATASET = [
  row('a', { provider: 'claude', cwd: '/w/beta', model: 'claude-fable-5', created: '2026-07-01T10:00:00Z', updated: '2026-07-18T10:00:00Z' }),
  row('b', { provider: 'codex', cwd: '/w/Alpha', model: 'gpt-5.6-sol', effort: 'high', created: '2026-07-02T10:00:00Z', updated: '2026-07-19T10:00:00Z' }),
  row('c', { provider: 'codex', cwd: '/w/beta', model: 'gpt-5.5', effort: 'low', created: '2026-07-03T10:00:00Z', updated: '2026-07-17T10:00:00Z' }),
  row('d', { provider: 'codex' }), // nothing known, no timestamps
]
const VIEW = { sort: 'updated', dir: 'desc', projects: null, providers: null, models: null, efforts: null, createdRange: null, updatedRange: null }

describe('applyView', () => {
  it('default view sorts by updated desc with nulls last', () => {
    expect(applyView(DATASET, VIEW).map((r) => r.id)).toEqual(['b', 'a', 'c', 'd'])
  })

  it('text sort is case-insensitive ascending with unknowns last, flipping with dir', () => {
    expect(applyView(DATASET, { ...VIEW, sort: 'project', dir: 'asc' }).map((r) => r.id)).toEqual(['b', 'a', 'c', 'd'])
    expect(applyView(DATASET, { ...VIEW, sort: 'project', dir: 'desc' }).map((r) => r.id)).toEqual(['c', 'a', 'b', 'd'])
  })

  it('facet selections and unknown buckets filter rows', () => {
    const view = { ...VIEW, projects: { values: ['/w/beta'], unknown: true } }
    expect(applyView(DATASET, view).map((r) => r.id)).toEqual(['a', 'c', 'd'])
    const none = { ...VIEW, models: { values: [], unknown: false } }
    expect(applyView(DATASET, none)).toEqual([])
  })

  it('date ranges are inclusive local days and exclude missing timestamps', () => {
    const view = { ...VIEW, createdRange: { from: '2026-07-02', to: '2026-07-03' } }
    expect(applyView(DATASET, view).map((r) => r.id)).toEqual(['b', 'c'])
  })
})

describe('computeFacets', () => {
  it('counts distinct values under other filters, never its own', () => {
    const view = { ...VIEW, projects: { values: ['/w/beta'], unknown: false }, providers: { values: ['codex'], unknown: false } }
    expect(computeFacets(DATASET, view, 'project')).toEqual({
      values: [
        { value: '/w/Alpha', count: 1 },
        { value: '/w/beta', count: 1 },
      ],
      unknownCount: 1,
    })
    expect(computeFacets(DATASET, view, 'model')).toEqual({
      values: [{ value: 'gpt-5.5', count: 1 }],
      unknownCount: 0,
    })
  })
})

describe('supervisor/worker hierarchy', () => {
  it('moves workers beneath their visible supervisor while preserving sibling sort order', () => {
    const rows = [
      { ...row('worker-new', { provider: 'claude', updated: '2026-07-20T12:00:00Z' }), nativeId: 'agent-new', kind: 'subagent', parentNativeId: 'parent' },
      { ...row('other', { provider: 'claude', updated: '2026-07-19T12:00:00Z' }), nativeId: 'other' },
      { ...row('parent', { provider: 'claude', updated: '2026-07-18T12:00:00Z' }), nativeId: 'parent' },
      { ...row('worker-old', { provider: 'claude', updated: '2026-07-17T12:00:00Z' }), nativeId: 'agent-old', kind: 'subagent', parentNativeId: 'parent' },
    ]
    expect(applyView(rows, VIEW).map((item) => item.id)).toEqual([
      'other', 'parent', 'worker-new', 'worker-old',
    ])
    const arranged = applyView(rows, VIEW)
    expect(arranged.find((item) => item.id === 'worker-new')).toMatchObject({ treeDepth: 1, treeLast: false })
    expect(arranged.find((item) => item.id === 'worker-old')).toMatchObject({ treeDepth: 1, treeLast: true })
  })

  it('nests an API worker beneath its invoker across provider boundaries', () => {
    const parent = {
      ...row('parent', { provider: 'codex', model: 'parent-model', updated: '2026-07-18T12:00:00Z' }),
      nativeId: 'codex-native', hostedThreadId: 'nht_parent', kind: 'thread',
    }
    const worker = {
      ...row('worker', { provider: 'claude', model: 'worker-model', updated: '2026-07-20T12:00:00Z' }),
      nativeId: 'claude-native', hostedThreadId: 'nht_worker', kind: 'thread',
      invokerHostedThreadId: 'nht_parent', invokerNativeThreadId: 'codex-native', invokerProvider: 'codex',
      runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'working' },
    }
    const arranged = applyView([worker, parent], VIEW)
    expect(arranged.map((item) => item.id)).toEqual(['parent', 'worker'])
    expect(arranged[1]).toMatchObject({ treeDepth: 1, treeLast: true })
    expect(arranged[0].subagentActivity).toEqual({ total: 1, active: 1, workers: 0 })

    const models = { values: ['worker-model'], unknown: false }
    expect(applyView([parent, worker], { ...VIEW, models }).map((item) => item.id)).toEqual(['parent', 'worker'])
  })

  it('records ancestor endings so deep tree connectors continue only when needed', () => {
    const parent = { ...row('parent'), nativeId: 'parent' }
    const first = { ...row('first'), nativeId: 'first', parentNativeId: 'parent' }
    const grandchild = { ...row('grandchild'), nativeId: 'grandchild', parentNativeId: 'first' }
    const last = { ...row('last'), nativeId: 'last', parentNativeId: 'parent' }

    const arranged = arrangeThreadHierarchy([parent, first, grandchild, last])
    expect(arranged.find((item) => item.id === 'first')).toMatchObject({ treeDepth: 1, treeLast: false, treePath: [] })
    expect(arranged.find((item) => item.id === 'grandchild')).toMatchObject({ treeDepth: 2, treeLast: true, treePath: [false] })
    expect(arranged.find((item) => item.id === 'last')).toMatchObject({ treeDepth: 1, treeLast: true, treePath: [] })
  })

  it('keeps a filtered worker connected by retaining its supervisor as context', () => {
    const parent = { ...row('parent', { provider: 'claude', model: 'parent-model' }), nativeId: 'parent' }
    const worker = {
      ...row('worker', { provider: 'claude', model: 'worker-model' }),
      nativeId: 'agent-worker', kind: 'subagent', parentNativeId: 'parent',
    }
    const models = { values: ['worker-model'], unknown: false }
    expect(applyView([parent, worker], { ...VIEW, models }).map((item) => item.id)).toEqual(['parent', 'worker'])
  })

  it('leaves an orphan worker visible in its ordinary sorted position', () => {
    const orphan = { ...row('orphan'), nativeId: 'agent-orphan', kind: 'subagent', parentNativeId: 'missing' }
    expect(arrangeThreadHierarchy([orphan])).toEqual([{ ...orphan, treeDepth: 0 }])
  })

  it('rolls a running worker up for display without making the supervisor working', () => {
    const parent = { ...row('parent'), nativeId: 'parent' }
    const worker = {
      ...row('worker'), nativeId: 'agent-worker', kind: 'subagent', parentNativeId: 'parent',
      fleet: { label: 'working' },
    }
    const decorated = decorateSubagentActivity([parent, worker])
    expect(decorated[0].subagentActivity).toEqual({ total: 1, active: 1, workers: 1 })
    expect(threadWorkIsActive(decorated[0])).toBe(false)
    expect(livenessRank(decorated[0])).toBe(3)
    expect(threadWorkIsActive(decorated[1])).toBe(true)
    expect(livenessRank(decorated[1])).toBe(0)
  })

  it('keeps completed workers connected without making the supervisor active', () => {
    const parent = { ...row('parent'), nativeId: 'parent' }
    const worker = {
      ...row('worker'), nativeId: 'agent-worker', kind: 'subagent', parentNativeId: 'parent',
      fleet: { label: 'idle' },
    }
    const decorated = decorateSubagentActivity([parent, worker])
    expect(decorated[0].subagentActivity).toEqual({ total: 1, active: 0, workers: 1 })
    expect(threadWorkIsActive(decorated[0])).toBe(false)
  })
})

describe('assignment roles and supervisor propagation', () => {
  const hosted = (id, over = {}) => ({ ...row(id), nativeId: `native-${id}`, hostedThreadId: `nht-${id}`, kind: 'thread', ...over })

  it('keeps auditor-only branches out of the supervisor role', () => {
    const main = hosted('main')
    const review = hosted('review', { invokerHostedThreadId: main.hostedThreadId, assignment: { assignmentRole: 'auditor' } })
    const decorated = decorateSubagentActivity([main, review])
    expect(decorated[0].subagentActivity).toEqual({ total: 1, active: 0, workers: 0 })
    expect(resolveThreadRole(decorated[1])).toBe('auditor')
  })

  it('propagates worker supervision through an auditor coordinator', () => {
    const main = hosted('main')
    const coordinator = hosted('coordinator', {
      invokerHostedThreadId: main.hostedThreadId,
      assignment: { assignmentRole: 'auditor' },
    })
    const investigation = hosted('investigation', {
      invokerHostedThreadId: coordinator.hostedThreadId,
      assignment: { assignmentRole: 'worker' },
    })
    const decorated = decorateSubagentActivity([main, coordinator, investigation])
    expect(decorated.map((item) => item.subagentActivity?.workers ?? 0)).toEqual([1, 1, 0])
  })

  it('uses the mutable assignment relationship as the current visual parent', () => {
    const original = hosted('original')
    const next = hosted('next')
    const child = hosted('child', {
      invokerHostedThreadId: original.hostedThreadId,
      assignment: {
        assignmentRole: 'auditor',
        relatedHostedThreadId: next.hostedThreadId,
      },
    })
    const arranged = arrangeThreadHierarchy([original, next, child])
    expect(arranged.map((item) => item.id)).toEqual(['original', 'next', 'child'])
    expect(arranged[2]).toMatchObject({ treeDepth: 1, treeLast: true })
  })

  it('keeps standalone threads top-level while allowing assignments to place them', () => {
    const parent = hosted('parent')
    const standalone = hosted('standalone', {
      sidebarPlacement: 'standalone',
      invokerHostedThreadId: parent.hostedThreadId,
      invokerNativeThreadId: parent.nativeId,
      invokerProvider: parent.provider,
      parentNativeId: parent.nativeId,
      sidebar: { parentId: parent.id },
    })
    expect(threadParentIdentityKeys(standalone)).toEqual([])
    expect(arrangeThreadHierarchy([parent, standalone]).find((item) => item.id === 'standalone').treeDepth).toBe(0)

    const assigned = {
      ...standalone,
      assignment: { assignmentRole: 'worker', relatedHostedThreadId: parent.hostedThreadId },
    }
    expect(threadParentIdentityKeys(assigned)).toEqual([parent.id, `hosted:${parent.hostedThreadId}`, `${parent.provider}:${parent.nativeId}`])
    expect(arrangeThreadHierarchy([parent, assigned]).find((item) => item.id === 'standalone').treeDepth).toBe(1)
  })

  it('falls back to the invoker native snapshot when a reopened parent has a new hosted id', () => {
    const reopenedParent = hosted('current-parent', {
      nativeId: '4621f48e-cabf-56a5-8bcf-6e61bfc0d9e3',
    })
    const child = hosted('child', {
      invokerHostedThreadId: 'nht-retired-parent',
      invokerNativeThreadId: reopenedParent.nativeId,
      invokerProvider: 'codex',
      assignment: {
        assignmentRole: 'worker',
        relatedHostedThreadId: 'nht-retired-parent',
      },
    })
    const arranged = arrangeThreadHierarchy([child, reopenedParent])
    expect(arranged.map((item) => item.id)).toEqual(['current-parent', 'child'])
    expect(arranged[1]).toMatchObject({ treeDepth: 1, treeLast: true })
  })

  it('falls back to immutable invocation lineage when assignment resolution is unavailable', () => {
    const original = hosted('original')
    const next = hosted('next')
    const child = hosted('child', {
      invokerHostedThreadId: original.hostedThreadId,
      assignmentUnavailable: true,
      assignment: {
        assignmentRole: 'worker',
        relatedHostedThreadId: next.hostedThreadId,
      },
    })
    const arranged = arrangeThreadHierarchy([next, original, child])
    expect(arranged.map((item) => item.id)).toEqual(['next', 'original', 'child'])
    expect(arranged[2]).toMatchObject({ treeDepth: 1, treeLast: true })
  })

  it('does not infer worker from hosted invocation lineage, but keeps native subagent evidence', () => {
    expect(resolveThreadRole(hosted('child', { invokerHostedThreadId: 'nht-parent' }))).toBeNull()
    expect(resolveThreadRole(hosted('unknown', { assignmentUnavailable: true, assignment: { assignmentRole: 'worker' } }))).toBeNull()
    expect(resolveThreadRole({ ...row('native'), kind: 'subagent' })).toBe('worker')
  })
})

// The sort toggle's two modes. Both are shared by the compact rail and the
// expanded table (App.svelte orders the rail through applyView), so these
// assertions cover the ordering of both surfaces at once.
describe('sort modes', () => {
  const live = (id, over) => ({ ...row(id, over), runtime: { status: 'ready', attached: true, sessionId: 's_' + id } })
  const working = (id, startedAtMs, over) => ({
    ...row(id, over),
    runtime: { status: 'ready', attached: true, sessionId: 's_' + id, turnActive: true, threadStatus: 'working', turnStartedAtMs: startedAtMs },
  })
  const gone = (id, over) => ({ ...row(id, over), runtime: { detached: true } })

  const MIXED = [
    row('hist_new', { cwd: '/w/aaa', updated: '2026-07-27T13:00:00Z' }),
    gone('det', { cwd: '/w/zzz', updated: '2026-07-26T09:00:00Z' }),
    live('att_old', { cwd: '/w/mmm', updated: '2026-07-20T08:00:00Z' }),
    row('hist_old', { cwd: '/w/aaa', updated: '2026-07-21T13:00:00Z' }),
  ]

  it('liveness-updated: what nano holds first, then detached, then history by recency', () => {
    const out = applyView(MIXED, { ...VIEW, sort: 'liveness-updated', dir: 'desc' })
    expect(out.map((r) => r.id)).toEqual(['att_old', 'det', 'hist_new', 'hist_old'])
  })

  it('liveness-updated: working threads stay longest-running first as updatedAt churns', () => {
    const first = working('first', 1000, { updated: '2026-07-20T08:00:00Z' })
    const second = working('second', 2000, { updated: '2026-07-27T13:00:00Z' })
    const attached = live('idle', { updated: '2026-07-28T13:00:00Z' })
    const view = { ...VIEW, sort: 'liveness-updated', dir: 'desc' }

    expect(applyView([second, attached, first], view).map((r) => r.id)).toEqual([
      'first', 'second', 'idle',
    ])
    expect(applyView([
      { ...first, updatedAt: '2026-07-29T13:00:00Z' },
      { ...second, updatedAt: '2026-07-30T13:00:00Z' },
      attached,
    ], view).map((r) => r.id)).toEqual(['first', 'second', 'idle'])
  })

  it('puts unanchored work after known runs and keeps its tie order stable', () => {
    const anchored = working('anchored', 1000, { updated: '2026-07-20T08:00:00Z' })
    const workerZ = { ...row('worker-z', { updated: '2026-07-30T13:00:00Z' }), kind: 'subagent', fleet: { label: 'working' } }
    const workerA = { ...row('worker-a', { updated: '2026-07-19T13:00:00Z' }), kind: 'subagent', fleet: { label: 'working' } }
    const view = { ...VIEW, sort: 'liveness-updated', dir: 'desc' }

    expect(applyView([workerZ, anchored, workerA], view).map((r) => r.id)).toEqual([
      'anchored', 'worker-a', 'worker-z',
    ])
  })

  it('liveness-project: same grouping, ordered by working directory inside it', () => {
    const out = applyView(
      [...MIXED, row('hist_mid', { cwd: '/w/bbb', updated: '2026-07-22T13:00:00Z' })],
      { ...VIEW, sort: 'liveness-project', dir: 'desc' }
    )
    expect(out.map((r) => r.id)).toEqual(['att_old', 'det', 'hist_new', 'hist_old', 'hist_mid'])
  })

  it('liveness-project keeps project groups stable and uses longest run inside each project', () => {
    const out = applyView(
      [
        working('beta-newer', 3000, { cwd: '/w/beta', updated: '2026-07-30T13:00:00Z' }),
        working('alpha', 1000, { cwd: '/w/alpha', updated: '2026-07-29T13:00:00Z' }),
        working('beta-older', 2000, { cwd: '/w/beta', updated: '2026-07-20T13:00:00Z' }),
      ],
      { ...VIEW, sort: 'liveness-project', dir: 'desc' }
    )
    expect(out.map((r) => r.id)).toEqual(['alpha', 'beta-older', 'beta-newer'])
  })

  it('liveness-project breaks ties on recency, and sorts an unknown cwd last', () => {
    const out = applyView(
      [
        row('no_cwd', { updated: '2026-07-27T13:00:00Z' }),
        row('same_old', { cwd: '/w/aaa', updated: '2026-07-20T13:00:00Z' }),
        row('same_new', { cwd: '/w/aaa', updated: '2026-07-26T13:00:00Z' }),
      ],
      { ...VIEW, sort: 'liveness-project', dir: 'desc' }
    )
    expect(out.map((r) => r.id)).toEqual(['same_new', 'same_old', 'no_cwd'])
  })

  it('a column-header sort still applies plainly, with no liveness grouping', () => {
    const out = applyView(MIXED, { ...VIEW, sort: 'updated', dir: 'desc' })
    expect(out.map((r) => r.id)).toEqual(['hist_new', 'det', 'hist_old', 'att_old'])
  })
})

// One collapse serves both renderers of the hierarchy, so it keys on thread
// identity rather than on the row key the rail builds or the catalog id the
// table uses — those differ between the surfaces for the same supervisor.
describe('worker group disclosure', () => {
  const node = (nativeId, treeDepth, subagentActivity) => ({
    id: `nth_${nativeId}`, provider: 'claude', nativeId, treeDepth, subagentActivity,
  })
  const held = (row) => ({ ...row, runtime: { attached: true } })
  const parent = held(node('parent', 0, { total: 3, active: 1 }))
  const child = held(node('child', 1, { total: 1, active: 1 }))
  const grandchild = held(node('grandchild', 2))
  const sibling = node('sibling', 1)
  // Counted by the window-wide roll-up, but not carried in these rows.
  const other = node('other', 0, { total: 2, active: 1 })
  const rows = [parent, child, grandchild, sibling, other]
  const ids = (list) => list.map((r) => r.nativeId)

  it('defaults worker groups closed without hiding the next supervisor', () => {
    expect(ids(collapseWorkerGroups(rows))).toEqual(['parent', 'other'])
  })

  it('opens only active branches at the requested levels', () => {
    expect(ids(collapseWorkerGroups(rows, [threadIdentity(parent)]))).toEqual([
      'parent', 'child', 'other',
    ])
    expect(ids(collapseWorkerGroups(rows, [threadIdentity(parent), threadIdentity(child)]))).toEqual([
      'parent', 'child', 'grandchild', 'other',
    ])
  })

  it('keeps history hidden under expand-all until that parent reveals it', () => {
    const current = collapseWorkerGroups(rows, ['*'])
    expect(ids(current)).toEqual([
      'parent', 'child', 'grandchild', 'other',
    ])
    expect(current[0].subagentHistoryControlsAfter).toBeUndefined()
    expect(current[2].subagentHistoryControlsAfter?.map((control) => control.parent.nativeId)).toEqual(['parent'])

    const withHistory = collapseWorkerGroups(rows, ['*'], [threadIdentity(parent)])
    expect(ids(withHistory)).toEqual([
      'parent', 'child', 'grandchild', 'sibling', 'other',
    ])
    expect(withHistory[3].subagentHistoryControlsAfter?.map((control) => control.parent.nativeId)).toEqual(['parent'])
    expect(ids(collapseWorkerGroups(rows, ['*', `!${threadIdentity(child)}`]))).toEqual([
      'parent', 'child', 'other',
    ])
  })

  // A busy transcript made an unheld worker look current once the Explorer had
  // loaded the whole catalog window.
  it('keeps a working thread Nano does not hold behind the history control', () => {
    const unheld = { ...node('unheld', 1), fleet: { label: 'working' }, kind: 'subagent' }
    const list = [parent, unheld]
    expect(ids(collapseWorkerGroups(list, [threadIdentity(parent)]))).toEqual(['parent'])
  })

  it('marks only the rows that head a group present in the list', () => {
    const [head, tail] = collapseWorkerGroups(rows)
    expect(head.workersCollapsible).toBe(true)
    expect(head.workersExpanded).toBe(false)
    expect(tail.subagentActivity.total).toBe(2) // counted, but no rows here to open
    expect(tail.workersCollapsible).toBeUndefined()

    const opened = collapseWorkerGroups(rows, [threadIdentity(parent)])
    expect(opened[0].workersExpanded).toBe(true)
    expect(opened[0].historicalAgentCount).toBe(1)
    expect(opened[0].historicalAgentsExpanded).toBe(false)
    expect(opened[1].workersCollapsible).toBe(true)
    expect(opened[2].workersCollapsible).toBeUndefined() // 'other' leads nothing
  })

  // Both narrowing paths keep matches plus their supervisor chain, so callers
  // must not collapse a narrowed view — the match would be the row hidden.
  it('reports a narrowed view for any facet or range, but not for a re-sort', () => {
    const browsing = {
      sort: 'liveness-updated', dir: 'desc', projects: null, providers: null,
      models: null, efforts: null, createdRange: null, updatedRange: null,
    }
    expect(viewIsNarrowed(browsing)).toBe(false)
    expect(viewIsNarrowed({ ...browsing, sort: 'updated', dir: 'asc' })).toBe(false)
    expect(viewIsNarrowed({ ...browsing, providers: { values: ['claude'], unknown: false } })).toBe(true)
    expect(viewIsNarrowed({ ...browsing, updatedRange: { from: '2026-07-01', to: '' } })).toBe(true)
  })
})
