import { describe, it, expect } from 'vitest'
import { compactPath, historicalBarFields, sessionBarFields, tierLabel, tookField } from './session_bar.js'

describe('compactPath', () => {
  it('reads a home path as a place, tilde and all', () => {
    expect(compactPath('/home/user/Code/nano')).toBe('~/Code/nano')
    expect(compactPath('/home/user')).toBe('~')
  })

  it('keeps the identifying tail of a deep path instead of overflowing the bar', () => {
    expect(compactPath('/home/user/Code/nano/web/src')).toBe('…/web/src')
    expect(compactPath('/a/b/c/d')).toBe('…/c/d')
  })

  it('leaves a short absolute path alone and tolerates a trailing slash', () => {
    expect(compactPath('/workspace/nano')).toBe('/workspace/nano')
    expect(compactPath('/home/user/Code/nano/')).toBe('~/Code/nano')
  })

  it('has nothing to say about nothing', () => {
    expect(compactPath('')).toBe('')
    expect(compactPath(null)).toBe('')
    expect(compactPath(undefined)).toBe('')
  })
})

describe('tierLabel', () => {
  it('uses fast/default in the compact bottom bar', () => {
    expect(tierLabel('priority')).toBe('fast')
    expect(tierLabel('fast')).toBe('fast')
    expect(tierLabel('standard')).toBe('default')
    expect(tierLabel('default')).toBe('default')
    expect(tierLabel('flex')).toBe('flex')
    expect(tierLabel('')).toBe('')
    expect(tierLabel(null)).toBe('')
  })
})

describe('sessionBarFields', () => {
  it('states all six fields in order, identity last', () => {
    const fields = sessionBarFields({
      provider: 'codex',
      cwd: '/home/user/Code/nano',
      gitBranch: 'main',
      model: '',
      threadModel: 'gpt-5.6-sol',
      effort: '',
      threadEffort: 'high',
      serviceTier: 'priority',
      threadId: 'th_abc123',
    })
    expect(fields.map((f) => f.key)).toEqual(['cwd', 'branch', 'model', 'effort', 'tier', 'thread'])
    expect(fields.map((f) => f.text)).toEqual([
      '~/Code/nano',
      'main',
      'gpt-5.6-sol',
      'high',
      'fast',
      'th_abc123',
    ])
    // The compacted path carries the full one, so a bar that had to shorten it
    // can still answer "which nano?" on hover.
    expect(fields[0].title).toBe('/home/user/Code/nano')
  })

  it('shows the override in force rather than the thread default it replaces', () => {
    const fields = sessionBarFields({
      model: 'gpt-5.6-luna',
      threadModel: 'gpt-5.6-sol',
      effort: 'low',
      threadEffort: 'high',
    })
    expect(fields.map((f) => f.text)).toEqual(['gpt-5.6-luna', 'low'])
  })

  it("falls back to the thread's own effort when no override is set", () => {
    // The value the next turn will actually use: a resumed thread with no
    // override still runs at its persisted effort, and the bar must say so.
    const fields = sessionBarFields({ threadModel: 'gpt-5.6-sol', threadEffort: 'high' })
    expect(fields.map((f) => f.text)).toEqual(['gpt-5.6-sol', 'high'])
  })

  it('renders the tier for codex only — claude has nothing behind the word', () => {
    const claude = sessionBarFields({ provider: 'claude', threadModel: 'opus', serviceTier: 'default' })
    expect(claude.map((f) => f.key)).toEqual(['model'])
    const codex = sessionBarFields({ provider: 'codex', threadModel: 'gpt-5.6-sol', serviceTier: 'default' })
    expect(codex.map((f) => f.key)).toEqual(['model', 'tier'])
    expect(codex[1].text).toBe('default')
  })

  it('says a protected folder was not read instead of dropping the branch', () => {
    // Nano never reads inside a folder macOS guards with a privacy prompt;
    // an empty field would read as "not a repository".
    const notice = 'Not read: macOS asks permission before any app reads this folder'
    const fields = sessionBarFields({ provider: 'codex', cwd: '/Users/u/Documents/x', protectedFolder: notice })
    expect(fields.find((f) => f.key === 'branch')).toEqual({ key: 'branch', text: 'not read: protected folder', title: notice })
  })

  it('drops what it does not know instead of inventing a placeholder', () => {
    // A codex resume before its handshake lands: a cwd and nothing else. The
    // failure this guards is a bar reading "~/Code/nano · · · " — separators
    // around holes, which is worse than a short bar.
    const fields = sessionBarFields({
      provider: 'codex',
      cwd: '/home/user/Code/nano',
      gitBranch: '',
      model: '',
      effort: '',
      serviceTier: '',
    })
    expect(fields.map((f) => f.key)).toEqual(['cwd'])
  })

  it('states that a live native id is pending, then replaces it with the UUID', () => {
    const pending = sessionBarFields({ provider: 'claude', threadPending: true })
    expect(pending).toEqual([{
      key: 'thread',
      text: 'native id pending…',
      title: 'Waiting for the provider-native resumable thread id',
    }])

    const resolved = sessionBarFields({
      provider: 'claude',
      threadPending: true,
      threadId: '22222222-aaaa-4bbb-8ccc-dddddddddddd',
    })
    expect(resolved).toEqual([{ key: 'thread', text: '22222222-aaaa-4bbb-8ccc-dddddddddddd' }])
  })

  it('answers empty for a session it was told nothing about', () => {
    expect(sessionBarFields()).toEqual([])
  })
})

describe('historicalBarFields', () => {
  it('states the recorded facts from a catalog row, identity last', () => {
    const fields = historicalBarFields({
      provider: 'codex',
      nativeId: 'th_hist1',
      metadata: {
        cwd: { state: 'known', value: '/home/user/Code/sample-project', evidence: 'native_durable' },
        historicalBranch: { state: 'known', value: 'main', evidence: 'native_durable' },
        model: { state: 'known', value: 'gpt-5.6-sol', evidence: 'native_durable' },
        effort: { state: 'known', value: 'high', evidence: 'native_durable' },
        priority: { state: 'known', value: 'priority', evidence: 'native_durable' },
      },
    })
    expect(fields.map((f) => f.key)).toEqual(['cwd', 'branch', 'model', 'effort', 'tier', 'thread'])
    expect(fields.map((f) => f.text)).toEqual(['~/Code/sample-project', 'main', 'gpt-5.6-sol', 'high', 'fast', 'th_hist1'])
  })

  it('lets only known cells speak — unknown and unsupported stay silent', () => {
    // The three-way metadata contract: an unknown model or an unsupported
    // priority (claude) must not leak a null or the word "unknown" into the bar.
    // The non-known cells deliberately carry values: the bar must trust the
    // state, not the value — a cell that is not known has nothing to say even
    // when a stale or placeholder value is sitting next to it.
    const fields = historicalBarFields({
      provider: 'claude',
      nativeId: 'sess-uuid',
      metadata: {
        cwd: { state: 'known', value: '/home/user/Code/sample-project', evidence: 'native_durable' },
        model: { state: 'unknown', value: 'stale-guess', evidence: null },
        priority: { state: 'unsupported', value: 'default', evidence: null },
      },
    })
    expect(fields.map((f) => f.key)).toEqual(['cwd', 'thread'])
  })

  it('never invents the live pending sentinel for a historical row', () => {
    expect(historicalBarFields({ provider: 'claude', nativeId: '', metadata: {} })).toEqual([])
  })

  it('has nothing to say about a thread with no row', () => {
    expect(historicalBarFields(null)).toEqual([])
    expect(historicalBarFields(undefined)).toEqual([])
  })
})

// The read-only bar's "took": a SUM of native turn durations with the count of
// turns that sum covers, never a wall-clock span. The count is what keeps a
// partial sum honest — partial coverage is the common case on both providers.
describe('historicalBarFields took', () => {
  const row = { provider: 'codex', nativeId: 'native-1', metadata: {} }

  it('states the summed duration and the turns it covers', () => {
    const fields = historicalBarFields(row, { durationMs: 760_000, turns: 9 })
    const took = fields.find((f) => f.key === 'took')
    expect(took.text).toBe('12m 40s · 9 turns')
    expect(took.title).toContain('not wall-clock')
  })

  it('singularises a one-turn thread', () => {
    expect(tookField({ durationMs: 5_000, turns: 1 }).text).toBe('5s · 1 turn')
  })

  // Zero coverage is unknown, not zero: the field disappears rather than
  // claiming a thread took no time (D2/D8).
  it('drops the field when nothing recorded a duration', () => {
    expect(tookField(null)).toBeNull()
    expect(tookField({ durationMs: 0, turns: 0 })).toBeNull()
    expect(tookField({ durationMs: 500, turns: 1 })).toBeNull()
    expect(historicalBarFields(row, { durationMs: 0, turns: 0 }).some((f) => f.key === 'took')).toBe(false)
  })

  // The thread id closes the bar as its identity register; took belongs with
  // the facts about the work, just before it.
  it('sits immediately before the thread id', () => {
    const keys = historicalBarFields(row, { durationMs: 760_000, turns: 9 }).map((f) => f.key)
    expect(keys.slice(-2)).toEqual(['took', 'thread'])
  })
})
