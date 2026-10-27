import { describe, it, expect } from 'vitest'
import { mailBadge, mailIsEditable, mailSenderLabel, mailStatusLabel } from './mail_format.js'
import { projectSidebar } from './sidebar_projection.js'

describe('mailBadge', () => {
  it('is absent without mail', () => {
    expect(mailBadge(null)).toBeNull()
    expect(mailBadge({ total: 0, unread: 0, seen: 0 })).toBeNull()
  })

  it('always shows unread/total', () => {
    expect(mailBadge({ total: 12, unread: 3, seen: 0 }).text).toBe('3/12')
    expect(mailBadge({ total: 5, unread: 0, seen: 2 }).text).toBe('0/5')
    expect(mailBadge({ total: 7, unread: 0, seen: 0 }).text).toBe('0/7')
  })

  it('is yellow with unread mail or a dropped ball, quiet otherwise', () => {
    expect(mailBadge({ total: 12, unread: 3, seen: 0 }).state).toBe('attention')
    expect(mailBadge({ total: 5, unread: 0, seen: 2 }).state).toBe('attention')
    expect(mailBadge({ total: 5, unread: 1, seen: 2 }).state).toBe('attention')
    expect(mailBadge({ total: 7, unread: 0, seen: 0 }).state).toBe('quiet')
  })
})

describe('mail labels', () => {
  it('marks seen as undispositioned for the operator', () => {
    expect(mailStatusLabel('seen')).toBe('seen · undispositioned')
    expect(mailStatusLabel('acted')).toBe('acted')
  })

  it('labels senders by kind', () => {
    expect(mailSenderLabel({ kind: 'operator', name: 'Operator' })).toBe('Operator')
    expect(mailSenderLabel({ kind: 'system' })).toBe('nano')
    expect(mailSenderLabel({ kind: 'thread', name: 'sample-task worker' })).toBe('sample-task worker')
  })

  it('permits edits only on unread operator mail', () => {
    expect(mailIsEditable({ sender: { kind: 'operator' }, status: 'unread' })).toBe(true)
    expect(mailIsEditable({ sender: { kind: 'operator' }, status: 'seen' })).toBe(false)
    expect(mailIsEditable({ sender: { kind: 'thread' }, status: 'unread' })).toBe(false)
  })
})

describe('sidebar mail carry', () => {
  it('carries catalog mail counts onto projected rows', () => {
    const item = {
      id: 'nth_1', provider: 'codex', nativeId: 'nat-1', kind: 'thread',
      hostedThreadId: 'nht_1', metadata: {}, mail: { total: 4, unread: 2, seen: 1 },
    }
    const rows = projectSidebar([item], [])
    expect(rows[0].mail).toEqual({ total: 4, unread: 2, seen: 1 })
    expect(rows[0].hostedThreadId).toBe('nht_1')
  })
})
