// The sidebar's whole contract lives in projectSidebar: how every catalog row
// is classified by liveness, how the /api/sessions pending overlay merges in,
// and how each row is titled, subtitled, ordered and clicked. sidebarCounts and
// filterSidebar are the chip half of the same contract.
import { describe, it, expect } from 'vitest'
import {
  filterSidebar,
  orderSidebarRows,
  projectSidebar,
  sectionSidebar,
  sidebarCounts,
  suppressBackgroundRows,
} from './sidebar_projection.js'

const known = (value) => ({ state: 'known', value, evidence: 'native_durable' })
const unknown = () => ({ state: 'unknown' })

function catalogRow(over = {}) {
  return {
    id: 'nth_1',
    provider: 'codex',
    nativeId: 'nat-1',
    kind: 'thread',
    name: null,
    title: null,
    metadata: { cwd: known('/workspace/nano'), model: known('gpt-5.6-sol') },
    createdAt: '2026-07-27T10:00:00Z',
    updatedAt: '2026-07-27T10:00:00Z',
    ...over,
  }
}

const attachedRuntime = (over = {}) => ({ attached: true, sessionId: 's_1', status: 'ready', turnActive: false, threadStatus: 'idle', ...over })

function summary(over = {}) {
  return {
    id: 's_1',
    provider: 'codex',
    threadId: 'nat-1',
    model: 'gpt-5.6-sol',
    cwd: '/workspace/nano',
    status: 'ready',
    turnActive: false, threadStatus: 'idle',
    createdAt: '2026-07-27T10:00:00Z',
    ...over,
  }
}

describe('projectSidebar liveness classification', () => {
  it('attached catalog rows (durable and memory alike) are nano rows', () => {
    const rows = projectSidebar(
      [
        catalogRow({ runtime: attachedRuntime() }),
        catalogRow({
          id: 'mem_s_2', nativeId: '', transient: true, updatedAt: '2026-07-27T11:00:00Z',
          runtime: attachedRuntime({ sessionId: 's_2', status: 'starting' }),
        }),
      ],
      []
    )
    // Dataset order, not recency: sorting is applyView's job now.
    expect(rows.map((r) => [r.cls, r.kind, r.click.sessionId])).toEqual([
      ['nano', 'attached', 's_1'],
      ['nano', 'attached', 's_2'],
    ])
  })

  it('detached catalog rows open their durable history without auto-resuming', () => {
    const rows = projectSidebar([catalogRow({ runtime: { attached: false, detached: true, status: 'resting' } })], [])
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({
      kind: 'detached',
      cls: 'nano',
      provider: 'codex',
      subtitle: 'gpt-5.6-sol',
      click: { action: 'history', catalogId: 'nth_1', provider: 'codex', nativeId: 'nat-1' },
    })
  })

  // A full catalog window brought detached history back into the Nano view.
  it('leaves a detached thread out of the Nano view unless it is resting', () => {
    const rows = projectSidebar([catalogRow({ runtime: { attached: false, detached: true } })], [])
    expect(rows[0]).toMatchObject({ kind: 'detached', cls: 'none' })
    expect(rows[0].indicator.state).toBe('detached')
    expect(filterSidebar(rows, 'nano')).toEqual([])
  })

  it('a row live nowhere is a "none" row with no indicator', () => {
    const rows = projectSidebar([catalogRow(), catalogRow({ id: 'nth_2', runtime: { attached: false, detached: false } })], [])
    expect(rows.map((r) => [r.cls, r.kind])).toEqual([
      ['none', 'catalog'],
      ['none', 'catalog'],
    ])
    expect(rows[0].indicator).toBeNull()
  })

})

describe('projectSidebar click descriptors', () => {
  const clickOf = (over) => projectSidebar([catalogRow(over)], [])[0].click

  it('a nano session opens live while a detached thread opens as history', () => {
    expect(clickOf({ runtime: attachedRuntime() })).toEqual({ action: 'open', sessionId: 's_1', catalogId: 'nth_1' })
    expect(clickOf({ runtime: { detached: true } })).toEqual({
      action: 'history', catalogId: 'nth_1', provider: 'codex', nativeId: 'nat-1',
    })
  })

  it('a historical row opens its durable-history pane', () => {
    expect(clickOf({})).toEqual({ action: 'history', catalogId: 'nth_1', nativeId: 'nat-1' })
  })

  it('catalog identity is sufficient even when the native id is not available', () => {
    expect(clickOf({ nativeId: '' })).toEqual({ action: 'history', catalogId: 'nth_1', nativeId: null })
  })

  it('a pending session opens on stage', () => {
    expect(projectSidebar([], [summary({ id: 's_new' })])[0].click).toEqual({ action: 'open', sessionId: 's_new' })
  })
})

describe('projectSidebar pending overlay', () => {
	it('keeps boot-resumed sessions out of the new-session overlay', () => {
		const durable = [
			catalogRow({
				id: 'nth_older', nativeId: 'native-older', title: 'Older canonical',
				runtime: { attached: false, detached: true },
			}),
			catalogRow({
				id: 'nth_newer', nativeId: 'native-newer', title: 'Newer canonical',
				runtime: { attached: false, detached: true },
			}),
		]
		const restoring = [
			summary({ id: 'session-older', threadId: '', name: 'Restoring older', status: 'starting', spawnMode: 'resume' }),
			summary({ id: 'session-newer', threadId: '', name: 'Restoring newer', status: 'starting', spawnMode: 'resume' }),
		]

		expect(projectSidebar(durable, restoring).map((row) => [row.kind, row.title])).toEqual([
			['detached', 'Older canonical'],
			['detached', 'Newer canonical'],
		])
		expect(projectSidebar(durable, [
			summary({ id: 'session-new', threadId: '', name: 'Actually new', status: 'starting', spawnMode: 'start' }),
		]).map((row) => [row.kind, row.title])).toEqual([
			['pending', 'Actually new'],
			['detached', 'Older canonical'],
			['detached', 'Newer canonical'],
		])
	})

	it('keeps one DOM identity from hosted creation through native attachment', () => {
		const hostedThreadId = 'nht_stable'
		const starting = projectSidebar([], [summary({
			id: 's_new', threadId: '', hostedThreadId, status: 'starting',
		})])[0]
		const native = projectSidebar([], [summary({
			id: 's_new', threadId: 'native-stable', hostedThreadId, status: 'ready',
		})])[0]
		const attached = projectSidebar([catalogRow({
			nativeId: 'native-stable', hostedThreadId,
			runtime: attachedRuntime({ sessionId: 's_new' }),
		})], [])[0]

		expect([starting.key, native.key, attached.key]).toEqual([
			'hosted:nht_stable',
			'hosted:nht_stable',
			'hosted:nht_stable',
		])
	})

	it('preserves standalone placement through compact rows and Nano filtering', () => {
		const parent = catalogRow({
			id: 'nth_parent', nativeId: 'native-parent', hostedThreadId: 'nht_parent',
		})
		const child = catalogRow({
			id: 'nth_child', provider: 'grok', nativeId: 'native-child', hostedThreadId: 'nht_child',
			invokerHostedThreadId: 'nht_parent', invokerNativeThreadId: 'native-parent', invokerProvider: 'codex',
			sidebarPlacement: 'standalone', runtime: attachedRuntime({ sessionId: 's_child' }),
		})
		const rows = projectSidebar([parent, child], [])
		expect(rows[1].sidebarPlacement).toBe('standalone')
		expect(filterSidebar(rows, 'nano').map((row) => row.key)).toEqual(['hosted:nht_child'])
	})

	it('preserves standalone placement on a pending session row', () => {
		const [row] = projectSidebar([], [summary({ id: 's_new', sidebarPlacement: 'standalone' })])
		expect(row.sidebarPlacement).toBe('standalone')
	})

  it('a session the dataset does not carry yet becomes a pending nano row', () => {
    const rows = projectSidebar([], [summary({ id: 's_new', threadId: '', status: 'starting', model: '' })])
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({
      kind: 'pending',
      cls: 'nano',
      title: '–', // no name or title yet; the cwd has its own line
      titleKind: 'none',
      subtitle: 'nano',
    })
  })

  it('shows the dialog name before the durable catalog row exists', () => {
    const [row] = projectSidebar([], [summary({ threadId: '', name: 'newly named thread' })])
    expect(row).toMatchObject({ title: 'newly named thread', titleKind: 'name' })
  })

  it('the dataset wins: a session already attached in the dataset gets no second row', () => {
    const rows = projectSidebar([catalogRow({ runtime: attachedRuntime() })], [summary()])
    expect(rows).toHaveLength(1)
    expect(rows[0].kind).toBe('attached')
  })

  it('one row when a session holds a thread the catalog has not stamped attached yet', () => {
    // The post-resume race: /api/sessions already carries the session, but the
    // catalog row's runtime overlay hasn't refreshed. Two rows would share the
    // provider:nativeId key and crash the sidebar's keyed each block.
    const rows = projectSidebar([catalogRow()], [summary()])
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({
      kind: 'attached',
      cls: 'nano',
      title: '–',
      click: { action: 'open', sessionId: 's_1', catalogId: 'nth_1' },
    })
    expect(rows[0].indicator.state).toBe('idle')
  })

  it('the session overlay does not swallow a detached row for a different session', () => {
    const rows = projectSidebar(
      [catalogRow({ runtime: { attached: false, detached: true } })],
      [summary({ id: 's_other', threadId: 'nat-other' })]
    )
    expect(rows.map((r) => r.kind)).toEqual(['pending', 'detached'])
    expect(new Set(rows.map((r) => r.key)).size).toBe(rows.length)
  })

  it('an exited session keeps its row until it is closed, retaining its model subtitle', () => {
    // The server's memory-row overlay skips exited sessions (their child is
    // gone), so the pending overlay is the only feed that can still show one.
    const rows = projectSidebar([], [summary({ status: 'exited' })])
    expect(rows[0]).toMatchObject({ kind: 'pending', subtitle: 'gpt-5.6-sol' })
    expect(rows[0].indicator).toBeNull() // live nowhere: no dot state
  })

  it('a pending row mid-turn shows the working indicator', () => {
    const rows = projectSidebar([], [summary({ turnActive: true, threadStatus: 'working' })])
    expect(rows[0].indicator.state).toBe('working')
  })

  it('preserves catalog assignment data but never fabricates it for a pending handoff', () => {
    const assigned = projectSidebar([catalogRow({
      hostedThreadId: 'nht_1', assignment: { assignmentRole: 'auditor', focus: 'Review', auditTargets: [{ hostedThreadId: 'nht_2' }] },
    })], [])[0]
    expect(assigned.assignment).toMatchObject({ assignmentRole: 'auditor' })
    expect(assigned.assignmentUnavailable).toBe(false)

    const pending = projectSidebar([], [summary({ id: 's_pending', assignmentUnavailable: true })])[0]
    expect(pending.assignment).toBeNull()
    expect(pending.assignmentUnavailable).toBe(true)
  })
})

describe('projectSidebar ordering', () => {
  // Ordering moved to the shared view (applyView) so the rail and the expanded
  // table cannot disagree; what this function still owns is preserving it.
  it('preserves the order of the dataset it is handed', () => {
    const rows = projectSidebar(
      [
        catalogRow({ id: 'c', nativeId: 'nat-c', updatedAt: '2026-07-20T09:00:00Z' }),
        catalogRow({ id: 'a', nativeId: 'nat-a', updatedAt: '2026-07-27T12:00:00Z', runtime: attachedRuntime({ sessionId: 's_a' }) }),
        catalogRow({ id: 'b', nativeId: 'nat-b', updatedAt: '2026-07-26T09:00:00Z', runtime: { detached: true } }),
      ],
      []
    )
    expect(rows.map((r) => r.key)).toEqual(['codex:nat-c', 'codex:nat-a', 'codex:nat-b'])
  })

  it('leads with pending sessions: no sort can place a row the dataset has never seen', () => {
    const rows = projectSidebar(
      [catalogRow({ id: 'nth', nativeId: 'nat-x', updatedAt: '2026-07-27T13:00:00Z' })],
      [summary({ id: 's_mid', threadId: '', createdAt: '2026-07-27T10:00:00Z' })]
    )
    expect(rows.map((r) => r.key)).toEqual(['session:s_mid', 'codex:nat-x'])
  })

  it('keeps every nano row ahead of working external rows, within their own activity bands', () => {
    const rows = [
      { key: 'external-idle', cls: 'none', kind: 'catalog', working: false },
      { key: 'nano-idle', cls: 'nano', kind: 'attached', working: false },
      { key: 'external-working', cls: 'none', kind: 'catalog', working: true },
      { key: 'nano-working', cls: 'nano', kind: 'attached', working: true },
      { key: 'nano-detached', cls: 'nano', kind: 'detached', working: false },
    ]

    expect(orderSidebarRows(rows).map((row) => row.key)).toEqual([
      'nano-working',
      'nano-idle',
      'nano-detached',
      'external-working',
      'external-idle',
    ])
  })

  it('ranks a parent tree by the parent, never by an active child', () => {
    const rows = [
      {
        key: 'tree-a', provider: 'codex', nativeId: 'tree-a', hostedThreadId: 'nht_a',
        cls: 'nano', kind: 'attached', working: false,
      },
      {
        key: 'tree-a-child', provider: 'claude', nativeId: 'tree-a-child', hostedThreadId: 'nht_a_child',
        invokerHostedThreadId: 'nht_a', cls: 'nano', kind: 'attached', working: true,
      },
      {
        key: 'tree-b', provider: 'codex', nativeId: 'tree-b', hostedThreadId: 'nht_b',
        cls: 'nano', kind: 'attached', working: true,
      },
      {
        key: 'tree-b-child', provider: 'codex', nativeId: 'tree-b-child', hostedThreadId: 'nht_b_child',
        invokerHostedThreadId: 'nht_b', cls: 'nano', kind: 'attached', working: false,
      },
    ]

    expect(orderSidebarRows(rows).map((row) => [row.key, row.treeDepth])).toEqual([
      ['tree-b', 0],
      ['tree-b-child', 1],
      ['tree-a', 0],
      ['tree-a-child', 1],
    ])
  })
})

describe('projectSidebar row content', () => {
  it('title chain: user-assigned name > derived title > muted dash — the cwd is never repeated as a title', () => {
    const titleOf = (over) => projectSidebar([catalogRow({ runtime: attachedRuntime(), ...over })], [])[0].title
    expect(titleOf({ name: { value: 'influence graph' }, title: 'Derived title' })).toBe('influence graph')
    expect(titleOf({ title: 'Derived title' })).toBe('Derived title')
    expect(titleOf({})).toBe('–')
    expect(titleOf({ nativeId: '55555555aaaa', metadata: { cwd: unknown() } })).toBe('–')
  })

  it('carries the full working directory as the stable middle row', () => {
    const row = projectSidebar([catalogRow({ runtime: attachedRuntime() })], [])[0]
    expect(row.cwd).toBe('/workspace/nano')
    expect(row.nativeId).toBe('nat-1')

    const pending = projectSidebar([], [summary({ cwd: '/workspace/pending' })])[0]
    expect(pending.cwd).toBe('/workspace/pending')
    expect(pending.nativeId).toBe('nat-1')
  })

  it('uses the shortened model as the subtitle regardless of lifecycle state', () => {
    const subtitleOf = (over) => projectSidebar([catalogRow({ runtime: attachedRuntime(), ...over })], [])[0].subtitle
    expect(subtitleOf({ metadata: { model: known('claude-fable-5') } })).toBe('fable-5')
    expect(subtitleOf({ runtime: attachedRuntime({ status: 'starting' }) })).toBe('gpt-5.6-sol')
    expect(subtitleOf({ metadata: { model: unknown() } })).toBe('…')
  })

  it('adds effort to the model line when it is known', () => {
    const subtitle = projectSidebar([
      catalogRow({ runtime: attachedRuntime(), metadata: { cwd: known('/workspace/nano'), model: known('gpt-5.6-sol'), effort: known('high') } }),
    ], [])[0].subtitle
    expect(subtitle).toBe('gpt-5.6-sol · high')
  })

  it('carries the update time separately for the compact age treatment', () => {
    const row = projectSidebar([catalogRow({ updatedAt: '2026-07-27T10:32:00Z' })], [])[0]
    expect(row.updatedAt).toBe('2026-07-27T10:32:00Z')
  })

  it('falls back from model to project when the model is unknown', () => {
    const subtitleOf = (over) => projectSidebar([catalogRow({ createdAt: null, updatedAt: null, ...over })], [])[0].subtitle
    expect(subtitleOf({})).toBe('gpt-5.6-sol')
    expect(subtitleOf({ metadata: { cwd: known('/workspace/nano'), model: unknown() } })).toBe('nano')
    expect(subtitleOf({ metadata: {} })).toBe('…')
  })

  it('keys on thread identity so the memory → durable handoff keeps one row', () => {
    const memory = projectSidebar(
      [catalogRow({ id: 'mem_s_1', transient: true, runtime: attachedRuntime() })],
      []
    )
    const durable = projectSidebar([catalogRow({ id: 'nth_1', runtime: attachedRuntime() })], [])
    expect(memory[0].key).toBe(durable[0].key)
  })

  it('a row with no native id still gets a distinct key from its own id', () => {
    const rows = projectSidebar([catalogRow({ id: 'nth_a', nativeId: '' }), catalogRow({ id: 'nth_b', nativeId: '' })], [])
    expect(rows.map((r) => r.key)).toEqual(['row:nth_a', 'row:nth_b'])
  })

  it('tolerates missing feeds', () => {
    expect(projectSidebar(undefined, undefined)).toEqual([])
  })
})

describe('sidebar chips', () => {
  const dataset = [
    catalogRow({ id: 'nth_att', nativeId: 'nat-att', runtime: attachedRuntime() }),
    catalogRow({ id: 'nth_det', nativeId: 'nat-det', runtime: { detached: true } }),
    catalogRow({ id: 'nth_hist', nativeId: 'nat-hist' }),
  ]

  it('counts every band, with pending sessions counting as nano', () => {
    const rows = projectSidebar(dataset, [summary({ id: 's_pending', threadId: '' })])
    expect(sidebarCounts(rows)).toEqual({ all: 4, working: 0, nano: 2 })
  })

  it('counts zero for a band with no rows', () => {
    expect(sidebarCounts(projectSidebar([], []))).toEqual({ all: 0, working: 0, nano: 0 })
  })

  it('filters to one liveness band, or passes everything through for "all"', () => {
    const rows = projectSidebar(dataset, [])
    expect(filterSidebar(rows, 'all')).toBe(rows)
    expect(filterSidebar(rows, 'nano').map((r) => r.key)).toEqual(['codex:nat-att'])
  })

  it('filters active runtime turns and provider workers into one working band', () => {
    const rows = projectSidebar([
      catalogRow({ id: 'runtime', nativeId: 'runtime', runtime: attachedRuntime({ turnActive: true, threadStatus: 'working' }) }),
      catalogRow({
        id: 'worker', nativeId: 'worker', provider: 'claude', kind: 'subagent',
        fleet: { label: 'working' }, preview: 'Inspect parser',
      }),
      catalogRow({ id: 'idle', nativeId: 'idle' }),
    ], [])
    expect(sidebarCounts(rows).working).toBe(2)
    expect(filterSidebar(rows, 'working').map((r) => r.key)).toEqual(['codex:runtime', 'claude:worker'])
  })

  it('filtering preserves the projection order', () => {
    const rows = projectSidebar(dataset, [])
    expect(filterSidebar(rows, 'nano')).toEqual(rows.filter((r) => r.cls === 'nano'))
  })

  it('keeps the nano chip strict even when a nano worker has an external supervisor', () => {
    const rows = [
      { key: 'claude:parent', provider: 'claude', nativeId: 'parent', cls: 'none' },
      { key: 'claude:worker', provider: 'claude', nativeId: 'worker', parentNativeId: 'parent', cls: 'nano' },
    ]
    expect(filterSidebar(rows, 'nano').map((row) => row.key)).toEqual(['claude:worker'])
  })

  // A gray row used to appear here: a closed hosted parent of a detached row
  // once the Explorer had loaded the whole catalog window.
  it('keeps a closed hosted parent of a Nano row out of the Nano view', () => {
    const rows = [
      { key: 'codex:child', provider: 'codex', nativeId: 'child', hostedThreadId: 'nht_child', invokerHostedThreadId: 'nht_closed', cls: 'nano' },
      { key: 'codex:closed', provider: 'codex', nativeId: 'closed', hostedThreadId: 'nht_closed', cls: 'none' },
    ]
    expect(filterSidebar(rows, 'nano').map((row) => row.key)).toEqual(['codex:child'])
  })

  it('keeps every descendant of a Nano parent available to its agent disclosure', () => {
    const parent = {
      key: 'codex:parent', provider: 'codex', nativeId: 'parent', hostedThreadId: 'nht_parent', cls: 'nano',
    }
    const children = Array.from({ length: 53 }, (_, index) => ({
      key: `codex:child-${index}`,
      provider: 'codex',
      nativeId: `child-${index}`,
      parentNativeId: 'parent',
      cls: 'none',
      working: index === 0,
    }))
    const rows = [parent, ...children]

    const nano = filterSidebar(rows, 'nano')
    expect(nano).toHaveLength(54)
    expect(nano.slice(1).map((row) => row.nativeId)).toEqual(children.map((row) => row.nativeId))
    expect(sidebarCounts(rows).nano).toBe(1)
  })

  it('retains a cross-provider invoker as context for a working worker', () => {
    const rows = [
      { key: 'codex:parent', provider: 'codex', nativeId: 'parent', hostedThreadId: 'nht_parent', cls: 'none' },
      {
        key: 'claude:worker', provider: 'claude', nativeId: 'worker', hostedThreadId: 'nht_worker',
        invokerHostedThreadId: 'nht_parent', cls: 'nano', working: true,
      },
    ]
    expect(filterSidebar(rows, 'working').map((row) => row.key)).toEqual(['codex:parent', 'claude:worker'])
  })
})

describe('background presentation', () => {
  function backgroundRow(over = {}) {
    return {
      key: 'background',
      runtime: { attached: true, status: 'ready', presentationMode: 'background', ...over },
    }
  }

  it('suppresses background sessions by default and restores them on disclosure', () => {
    const rows = [{ key: 'shared', runtime: { presentationMode: 'shared' } }, backgroundRow()]
    expect(suppressBackgroundRows(rows)).toEqual({
      rows: [rows[0]], backgroundCount: 1, hiddenCount: 1,
    })
    expect(suppressBackgroundRows(rows, true)).toEqual({
      rows, backgroundCount: 1, hiddenCount: 0,
    })
  })

  it('keeps a background child in its visible parent tree', () => {
    const parent = {
      key: 'parent', provider: 'codex', nativeId: 'parent', hostedThreadId: 'nht_parent',
      runtime: { presentationMode: 'shared' },
    }
    const child = {
      ...backgroundRow(), key: 'child', provider: 'claude', nativeId: 'child',
      hostedThreadId: 'nht_child', invokerHostedThreadId: 'nht_parent',
    }
    expect(suppressBackgroundRows([parent, child])).toEqual({
      rows: [parent, child], backgroundCount: 0, hiddenCount: 0,
    })
  })

  it('folds an entire background-only tree behind the global disclosure', () => {
    const parent = {
      ...backgroundRow(), key: 'parent', provider: 'codex', nativeId: 'parent',
      hostedThreadId: 'nht_parent',
    }
    const child = {
      ...backgroundRow(), key: 'child', provider: 'claude', nativeId: 'child',
      hostedThreadId: 'nht_child', invokerHostedThreadId: 'nht_parent',
    }
    expect(suppressBackgroundRows([parent, child])).toEqual({
      rows: [], backgroundCount: 2, hiddenCount: 2,
    })
    expect(suppressBackgroundRows([parent, child], true)).toEqual({
      rows: [parent, child], backgroundCount: 2, hiddenCount: 0,
    })
  })

  it.each([
    ['waiting', { threadStatus: 'needs_you' }],
    ['failed', { needsAttention: true }],
    ['spawn error', { error: 'provider unavailable' }],
    ['exited', { status: 'exited' }],
  ])('promotes a %s background session while collapsed', (_label, state) => {
    const row = backgroundRow(state)
    expect(suppressBackgroundRows([row]).rows).toEqual([row])
  })

  it('carries presentation and attention state through the pending overlay', () => {
    const row = projectSidebar([], [summary({
      presentationMode: 'background', needsAttention: true, error: 'turn failed',
    })])[0]
    expect(row.runtime).toMatchObject({
      presentationMode: 'background', needsAttention: true, error: 'turn failed', status: 'ready',
    })
  })
})

describe('starred sectioning', () => {
  it('projects the catalog starred flag onto compact rows', () => {
    const rows = projectSidebar([
      catalogRow({ starred: true }),
      catalogRow({ id: 'nth_2', nativeId: 'nat-2', starred: false }),
    ], [])
    expect(rows.map((row) => row.starred)).toEqual([true, false])
  })

  // In a full catalog window a star on a history row pulled its live parent up.
  it('ignores a star on a thread Nano does not hold', () => {
    const rows = [
      { key: 'parent', provider: 'codex', nativeId: 'parent', cls: 'nano', kind: 'attached', starred: false },
      { key: 'history', provider: 'codex', nativeId: 'history', parentNativeId: 'parent', cls: 'none', kind: 'catalog', starred: true },
    ]
    expect(sectionSidebar(rows, false).starred).toEqual([])
  })

  it('puts every starred row above every unstarred row and hides an empty Starred section', () => {
    const rows = [
      { key: 'idle', cls: 'nano', kind: 'attached', working: false, starred: false },
      { key: 'star-idle', cls: 'nano', kind: 'attached', working: false, starred: true },
      { key: 'working', cls: 'nano', kind: 'attached', working: true, starred: false },
      { key: 'star-working', cls: 'nano', kind: 'attached', working: true, starred: true },
    ]
    const sectioned = sectionSidebar(rows)
    expect(sectioned.starred.map((row) => row.key)).toEqual(['star-working', 'star-idle'])
    expect(sectioned.rest.map((row) => row.key)).toEqual(['working', 'idle'])
    expect(sectionSidebar([]).starred).toEqual([])
  })

  it('moves a starred child with its unstarred ancestor context', () => {
    const parent = {
      key: 'parent', provider: 'codex', nativeId: 'parent', hostedThreadId: 'nht_parent',
      cls: 'nano', kind: 'attached', working: false, starred: false,
    }
    const child = {
      key: 'child', provider: 'claude', nativeId: 'child', hostedThreadId: 'nht_child',
      invokerHostedThreadId: 'nht_parent', cls: 'nano', kind: 'attached', working: false, starred: true,
    }
    const sectioned = sectionSidebar([parent, child])
    expect(sectioned.starred.map((row) => [row.key, row.treeDepth, row.starred])).toEqual([
      ['parent', 0, false],
      ['child', 1, true],
    ])
    expect(sectioned.rest).toEqual([])
  })

  it('moves a starred parent with its whole subtree and never duplicates overlapping stars', () => {
    const parent = {
      key: 'parent', provider: 'codex', nativeId: 'parent', hostedThreadId: 'nht_parent',
      cls: 'nano', kind: 'attached', working: false, starred: true,
    }
    const child = {
      key: 'child', provider: 'claude', nativeId: 'child', hostedThreadId: 'nht_child',
      invokerHostedThreadId: 'nht_parent', cls: 'nano', kind: 'attached', working: false, starred: true,
    }
    const grandchild = {
      key: 'grandchild', provider: 'grok', nativeId: 'grandchild', hostedThreadId: 'nht_grandchild',
      invokerHostedThreadId: 'nht_child', cls: 'nano', kind: 'attached', working: false, starred: false,
    }
    const sectioned = sectionSidebar([parent, child, grandchild])
    expect(sectioned.starred.map((row) => [row.key, row.treeDepth])).toEqual([
      ['parent', 0],
      ['child', 1],
      ['grandchild', 2],
    ])
    expect(sectioned.rest).toEqual([])
  })

  it('leaves unselected sibling branches in the ordinary section as roots', () => {
    const parent = {
      key: 'parent', provider: 'codex', nativeId: 'parent', hostedThreadId: 'nht_parent',
      cls: 'nano', kind: 'attached', working: false, starred: false,
    }
    const starredChild = {
      key: 'starred-child', provider: 'claude', nativeId: 'starred-child', hostedThreadId: 'nht_starred',
      invokerHostedThreadId: 'nht_parent', cls: 'nano', kind: 'attached', working: false, starred: true,
    }
    const sibling = {
      key: 'sibling', provider: 'grok', nativeId: 'sibling', hostedThreadId: 'nht_sibling',
      invokerHostedThreadId: 'nht_parent', cls: 'nano', kind: 'attached', working: false, starred: false,
    }
    const sectioned = sectionSidebar([parent, starredChild, sibling], false)
    expect(sectioned.starred.map((row) => [row.key, row.treeDepth])).toEqual([
      ['parent', 0],
      ['starred-child', 1],
    ])
    expect(sectioned.rest.map((row) => [row.key, row.treeDepth])).toEqual([['sibling', 0]])
  })
})
