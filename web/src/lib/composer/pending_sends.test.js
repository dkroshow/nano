import { describe, expect, it } from 'vitest'
import { newClientActionId, purgeLegacyPendingStorage } from './pending_sends.js'

describe('pending send identity', () => {
  it('mints stable non-empty client action ids', () => {
    const first = newClientActionId()
    const second = newClientActionId()
    expect(first).toBeTruthy()
    expect(second).toBeTruthy()
    expect(second).not.toBe(first)
  })
})

describe('pending send storage', () => {
  it('purges only legacy pending keys', () => {
    localStorage.setItem('nano-pending:codex:thread', '[]')
    localStorage.setItem('nano-draft:codex:thread', 'keep')
    purgeLegacyPendingStorage()
    expect(localStorage.getItem('nano-pending:codex:thread')).toBeNull()
    expect(localStorage.getItem('nano-draft:codex:thread')).toBe('keep')
  })
})
