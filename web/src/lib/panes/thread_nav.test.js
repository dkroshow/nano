// The contracts behind keyboard navigation: ⌥Tab cycles finished threads
// newest-first regardless of sidebar position, while ⌥↓ / ⌥↑ step through the
// visible order without wrapping.
import { describe, it, expect } from 'vitest'
import { activeRowKey, cycleRowKey, neighborKey, newestUpdatedRow, recencyOrderedRows } from './thread_nav.js'

function row(key, over = {}) {
  return {
    key,
    runtime: { attached: true, sessionId: `s_${key}` },
    click: { action: 'open', sessionId: `s_${key}`, catalogId: `nth_${key}` },
    ...over,
  }
}

describe('the recency walk order', () => {
  it('orders by updatedAt newest first and drops working rows', () => {
    const rows = [
      row('old', { updatedAt: '2026-07-31T10:00:00Z' }),
      row('busy', { updatedAt: '2026-07-31T13:00:00Z', working: true }),
      row('new', { updatedAt: '2026-07-31T12:00:00Z' }),
    ]
    expect(recencyOrderedRows(rows).map((r) => r.key)).toEqual(['new', 'old'])
  })

  it('keeps the projected order for equal or missing stamps', () => {
    const rows = [row('a'), row('b', { updatedAt: '2026-07-31T12:00:00Z' }), row('c')]
    expect(recencyOrderedRows(rows).map((r) => r.key)).toEqual(['b', 'a', 'c'])
    expect(recencyOrderedRows([]).length).toBe(0)
  })
})

describe('visibility is the one membership rule', () => {
  // Operator, 2026-08-25: the walk covers exactly what the sidebar renders.
  // These functions receive the rendered rows; a background row that is on
  // screen (an expanded subagent, the disclosure open, tree retention) is a
  // legitimate target. The mode-based skip this replaces hid visible finished
  // subagents and made ⌥Tab drop past fresh threads to hour-old ones.
  const backgroundChild = row('worker', {
    updatedAt: '2026-07-31T14:00:00Z',
    runtime: { attached: true, sessionId: 's_worker', presentationMode: 'background' },
  })
  const parent = row('parent', { updatedAt: '2026-07-31T12:00:00Z' })

  it('walks and jumps to a rendered background row like any other', () => {
    expect(recencyOrderedRows([backgroundChild, parent]).map((r) => r.key)).toEqual(['worker', 'parent'])
    expect(newestUpdatedRow([backgroundChild, parent])?.key).toBe('worker')
  })
})

describe('the newest-updated jump target', () => {
  it('picks the newest updatedAt, working rows included', () => {
    const rows = [
      row('done', { updatedAt: '2026-07-31T12:00:00Z' }),
      row('busy', { updatedAt: '2026-07-31T13:00:00Z', working: true }),
    ]
    expect(newestUpdatedRow(rows)?.key).toBe('busy')
  })

  it('keeps the first projected row on ties and survives empty lists', () => {
    const stamp = '2026-07-31T12:00:00Z'
    expect(newestUpdatedRow([row('a', { updatedAt: stamp }), row('b', { updatedAt: stamp })])?.key).toBe('a')
    expect(newestUpdatedRow([])).toBeNull()
  })
})

describe('sidebar tab cycle', () => {
  const rows = [row('a'), row('b'), row('c')]

  it('moves forward through the exact sidebar order and wraps', () => {
    expect(cycleRowKey(rows, 'a', 1)).toBe('b')
    expect(cycleRowKey(rows, 'b', 1)).toBe('c')
    expect(cycleRowKey(rows, 'c', 1)).toBe('a')
  })

  it('moves backward through the exact sidebar order and wraps', () => {
    expect(cycleRowKey(rows, 'a', -1)).toBe('c')
    expect(cycleRowKey(rows, 'c', -1)).toBe('b')
    expect(cycleRowKey(rows, 'b', -1)).toBe('a')
  })

  it('starts at the leading or trailing visible row when no tab matches', () => {
    expect(cycleRowKey(rows, null, 1)).toBe('a')
    expect(cycleRowKey(rows, 'not-visible', -1)).toBe('c')
    expect(cycleRowKey([], null, 1)).toBeNull()
  })
})

describe('row stepping', () => {
  const rows = [row('a'), row('b'), row('c')]

  it('moves down and up the projected order without wrapping', () => {
    expect(neighborKey(rows, 'b', 1)).toBe('c')
    expect(neighborKey(rows, 'b', -1)).toBe('a')
    expect(neighborKey(rows, 'c', 1)).toBeNull()
    expect(neighborKey(rows, 'a', -1)).toBeNull()
  })
})

describe('the active row', () => {
  const rows = [row('a'), row('b', { click: { action: 'history', catalogId: 'nth_b' } })]

  it('matches the active tab by session, and by catalog thread', () => {
    expect(activeRowKey(rows, { sessionId: 's_a', catalogId: '' })).toBe('a')
    expect(activeRowKey(rows, { sessionId: '', catalogId: 'nth_b' })).toBe('b')
  })
})
