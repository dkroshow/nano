import { describe, expect, it } from 'vitest'
import { activityDot, workingIndicator, tickingAnchor } from '../explorer/explorer_format.js'
import { threadWorkIsActive } from '../../explorer_local.js'
import { composerLocked, composerPlaceholder } from '../composer/composer_gate.js'

describe('connection precedes stored work', () => {
  for (const status of ['starting', 'reconnecting', 'reconnect_parked', 'reconnect_failed', 'detached', 'exited']) {
    it(`${status} ignores stale work, waiting, and timer anchors`, () => {
      const item = { runtime: { attached: true, status, turnActive: true, threadStatus: 'needs_you', waiting: true, turnStartedAtMs: 1000 } }
      expect(activityDot(item).state).toBe(status === 'reconnect_parked' ? 'parked' : status === 'reconnect_failed' ? 'unreachable' : status)
      expect(workingIndicator(item, 2000)).toBeNull()
      expect(tickingAnchor(item)).toBe(0)
    })
  }
  it('uses ready provider idle ahead of a historical worker court', () => {
    expect(threadWorkIsActive({ kind: 'subagent', fleet: { label: 'working' }, runtime: { attached: true, status: 'ready', turnActive: false, threadStatus: 'idle' } })).toBe(false)
  })
  it('queues during attachment and reconnect, refuses unavailable attachments', () => {
    for (const status of ['starting', 'reconnecting', 'reconnect_parked', 'ready']) expect(composerLocked(status)).toBe(false)
    for (const status of ['reconnect_failed', 'detached', 'exited', null]) expect(composerLocked(status)).toBe(true)
    expect(composerPlaceholder('reconnect_failed', true)).toBe(composerPlaceholder('reconnect_failed', false))
    expect(composerPlaceholder('exited', true)).toBe(composerPlaceholder('exited', false))
  })
})
