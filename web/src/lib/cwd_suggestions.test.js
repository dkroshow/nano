import { describe, it, expect } from 'vitest'
import { cwdSuggestions } from './cwd_suggestions.js'

const row = (cwd, updatedAt) => ({ metadata: { cwd: { state: 'known', value: cwd } }, updatedAt })

describe('cwdSuggestions', () => {
  it('ranks by how often a directory is worked in, then by recency', () => {
    // Two real repos across many threads, one throwaway probe dir from today:
    // the probe dir is the most RECENT but must sink below the daily drivers.
    const dataset = [
      row('/home/user/Code/sample-project', '2026-07-30T10:00:00Z'),
      row('/home/user/Code/sample-project', '2026-07-29T10:00:00Z'),
      row('/home/user/Code/nano', '2026-07-31T09:00:00Z'),
      row('/home/user/Code/sample-project', '2026-07-28T10:00:00Z'),
      row('/home/user/Code/nano', '2026-07-30T09:00:00Z'),
      row('/private/tmp/nano-verify-x', '2026-07-31T12:00:00Z'),
    ]
    const got = cwdSuggestions({ dataset })
    expect(got.map((s) => s.path)).toEqual([
      '/home/user/Code/sample-project',
      '/home/user/Code/nano',
      '/private/tmp/nano-verify-x',
    ])
    expect(got[0].label).toBe('~/Code/sample-project')
  })

  it('filters by substring of the full path as the operator types', () => {
    const dataset = [row('/home/user/Code/sample-project', '1'), row('/home/user/Code/nano', '2')]
    expect(cwdSuggestions({ dataset, query: 'sample' }).map((s) => s.path)).toEqual(['/home/user/Code/sample-project'])
    expect(cwdSuggestions({ dataset, query: 'SAMPLE' }).map((s) => s.path)).toEqual(['/home/user/Code/sample-project'])
    expect(cwdSuggestions({ dataset, query: 'zzz' })).toEqual([])
  })

  it('counts live sessions and ignores unknown metadata cells', () => {
    const dataset = [
      { metadata: { cwd: { state: 'unknown', value: '/should/not/appear' } }, updatedAt: '9' },
    ]
    const sessions = [{ cwd: '/home/user/live-now', createdAt: '2026-07-31T12:00:00Z' }]
    expect(cwdSuggestions({ dataset, sessions }).map((s) => s.path)).toEqual(['/home/user/live-now'])
  })

  it('caps the list and survives empty inputs', () => {
    const dataset = Array.from({ length: 20 }, (_, i) => row(`/dir/${i}`, String(i)))
    expect(cwdSuggestions({ dataset }).length).toBe(8)
    expect(cwdSuggestions({})).toEqual([])
  })
})
