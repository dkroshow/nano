import { describe, it, expect } from 'vitest'
import { buildWindowQuery, mergePages, DEFAULT_FETCH } from './explorer_query.js'

const NOW = new Date(2026, 6, 20, 12, 0) // Jul 20 2026 local

describe('buildWindowQuery', () => {
  it('bounds the default window to 30 days of last-message time', () => {
    const q = buildWindowQuery(DEFAULT_FETCH, null, NOW)
    expect(q).toContain('updatedFrom=' + encodeURIComponent(new Date(NOW.getTime() - 30 * 86_400_000).toISOString()))
    expect(q).toContain('includeSubagents=true')
    expect(q).toContain('includeProgrammatic=false')
    expect(q).toContain('limit=250')
  })

  it('the all window sends no time bound', () => {
    expect(buildWindowQuery({ ...DEFAULT_FETCH, window: 'all' }, null, NOW)).not.toContain('updatedFrom')
  })

  it('an exact threadId search drops the window bound so old threads stay findable', () => {
    const q = buildWindowQuery({ ...DEFAULT_FETCH, threadId: ' native-123 ' }, null, NOW)
    expect(q).toContain('threadId=native-123')
    expect(q).not.toContain('updatedFrom')
  })

  it('sends dataset toggles and cursor; archived only when non-default', () => {
    const q = buildWindowQuery(
      { ...DEFAULT_FETCH, archived: 'only', includeSubagents: true },
      'cursor-2',
      NOW
    )
    expect(q).toContain('archived=only')
    expect(q).toContain('includeSubagents=true')
    expect(q).toContain('cursor=cursor-2')
    expect(buildWindowQuery(DEFAULT_FETCH, null, NOW)).not.toContain('archived')
  })

  it('never sends view params — sort, facets, and ranges are local', () => {
    const q = buildWindowQuery(DEFAULT_FETCH, null, NOW)
    for (const param of ['sort', 'dir', 'project', 'model', 'effort', 'createdFrom']) {
      expect(q).not.toContain(param + '=')
    }
  })
})

describe('mergePages', () => {
  const row = (id, v = 1) => ({ id, v })

  it('appends unseen rows and keeps first-seen positions', () => {
    const merged = mergePages([row('a'), row('b')], [row('b', 2), row('c')])
    expect(merged.map((r) => r.id)).toEqual(['a', 'b', 'c'])
    expect(merged.find((r) => r.id === 'b').v).toBe(2) // last write wins
  })

  it('tolerates a null incoming page', () => {
    expect(mergePages([row('a')], null).map((r) => r.id)).toEqual(['a'])
  })
})

// Memory rows (live sessions the scanner hasn't indexed yet) carry
// `transient: true` and a `mem_` id, so they never collide by id with the
// durable row for the same thread. Dedup is by (provider, nativeId), durable
// wins — the scanner can index a thread between the server's existence check
// and the page we are merging.
describe('mergePages durable-vs-transient dedup', () => {
  const durable = (id, provider, nativeId) => ({ id, provider, nativeId })
  const memory = (sessionId, provider, nativeId) => ({
    id: `mem_${sessionId}`, provider, nativeId, transient: true,
  })

  it('a durable row supersedes the memory row for the same thread', () => {
    const merged = mergePages(
      [durable('nth_1', 'codex', 'nat-1'), memory('s2', 'codex', 'nat-2')],
      [durable('nth_2', 'codex', 'nat-2')]
    )
    expect(merged.map((r) => r.id)).toEqual(['nth_1', 'nth_2'])
  })

  it('a memory row for a thread already held durably is skipped', () => {
    const merged = mergePages(
      [durable('nth_2', 'codex', 'nat-2')],
      [memory('s2', 'codex', 'nat-2'), durable('nth_3', 'claude', 'nat-3')]
    )
    expect(merged.map((r) => r.id)).toEqual(['nth_2', 'nth_3'])
  })

  it('dedups within one response — the first page carries both classes', () => {
    const merged = mergePages([], [durable('nth_2', 'codex', 'nat-2'), memory('s2', 'codex', 'nat-2')])
    expect(merged.map((r) => r.id)).toEqual(['nth_2'])
  })

  it('matches on provider too — the same native id under another provider is a different thread', () => {
    const merged = mergePages(
      [memory('s2', 'claude', 'nat-2')],
      [durable('nth_2', 'codex', 'nat-2')]
    )
    expect(merged.map((r) => r.id)).toEqual(['mem_s2', 'nth_2'])
  })

  it('an empty nativeId (a starting session) matches nothing and is kept', () => {
    const merged = mergePages(
      [memory('s9', 'codex', ''), durable('nth_e', 'codex', '')],
      [durable('nth_2', 'codex', 'nat-2')]
    )
    expect(merged.map((r) => r.id)).toEqual(['mem_s9', 'nth_e', 'nth_2'])
  })

  // The rule only ever drops transients: durable rows sharing a native id
  // (subagent/parent shapes the catalog does emit) all survive the merge.
  it('leaves an all-durable merge untouched', () => {
    const rows = [durable('nth_1', 'codex', 'nat-1'), durable('nth_2', 'codex', 'nat-1')]
    expect(mergePages(rows.slice(0, 1), rows.slice(1)).map((r) => r.id)).toEqual(['nth_1', 'nth_2'])
  })
})
