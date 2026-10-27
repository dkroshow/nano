import { describe, expect, it, vi } from 'vitest'
import { bulkClosePreviewRows, requestBulkClose } from './bulk_close.svelte.js'

describe('bulk close server preview', () => {
  it('keeps every canonical member even when its catalog row is not loaded', () => {
    const root = { id: 'a', provider: 'codex', nativeId: 'a', hostedThreadId: 'a', title: 'A' }
    const members = [
      { hostedThreadId: 'a', provider: 'codex', nativeThreadId: 'a', depth: 0, status: 'exited', starred: true },
      { hostedThreadId: 'b', provider: 'claude', nativeThreadId: 'native-b', depth: 1, status: 'ready', working: true, },
    ]
    const rows = bulkClosePreviewRows(root, members, [root], Date.UTC(2026, 7, 23))
    expect(rows.map((row) => [row.label, row.depth, row.activity])).toEqual([
      ['A', 0, 'closed'],
      ['claude · native-b', 1, 'working'],
    ])
    expect(rows[0].starred).toBe(true)
  })

  it('reports the common error message without exposing its JSON', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: false,
      status: 409,
      text: async () => JSON.stringify({
        error: { code: 'target_not_idle', message: 'A descendant is still working.' },
      }),
    })))
    try {
      await expect(requestBulkClose({ hostedThreadId: 'nht_root' }))
        .rejects.toMatchObject({ message: 'A descendant is still working.' })
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
