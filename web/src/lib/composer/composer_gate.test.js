import { describe, it, expect } from 'vitest'
import { composerLocked, composerPlaceholder, enterSubmits, startingPhase } from './composer_gate.js'

describe('composerLocked', () => {
  it('allows queueing while the resume attach is starting', () => {
    expect(composerLocked('starting')).toBe(false)
  })

  it('unlocks on ready', () => {
    expect(composerLocked('ready')).toBe(false)
  })

  it('does not lock during a turn — the message queues instead', () => {
    expect(composerLocked('ready', true)).toBe(false)
    expect(composerLocked(null, true)).toBe(true)
  })

  it('exited and unknown statuses retain drafts without submitting', () => {
    expect(composerLocked('exited')).toBe(true)
    expect(composerLocked(null)).toBe(true)
  })
})

// Both spawn kinds now answer their POST before the provider handshake lands,
// so "starting" no longer means "attaching": a fresh start spends that time
// creating a session that does not exist yet.
describe('startingPhase', () => {
  it('separates creating from attaching', () => {
    expect(startingPhase('starting', 'start')).toBe('creating')
    expect(startingPhase('starting', 'resume')).toBe('attaching')
  })

  it('is empty for every settled status', () => {
    expect(startingPhase('ready', 'start')).toBe('')
    expect(startingPhase('exited', 'resume')).toBe('')
    expect(startingPhase(null, 'resume')).toBe('')
  })

  it('reads an unknown spawn mode as a start', () => {
    expect(startingPhase('starting', undefined)).toBe('creating')
  })
})

describe('composerPlaceholder', () => {
  it('explains each state', () => {
    expect(composerPlaceholder('starting', false, 'resume')).toBe('attaching to thread…')
    expect(composerPlaceholder('starting', false, 'start')).toBe('starting session…')
    expect(composerPlaceholder('ready', true)).toBe('Queue the next message')
    expect(composerPlaceholder('ready', false)).toBe('Message the agent')
  })
})

describe('enterSubmits', () => {
  it('sends on a bare Enter from a real keyboard', () => {
    expect(enterSubmits('Enter')).toBe(true)
  })

  it('leaves Shift+Enter as the newline it has always been', () => {
    expect(enterSubmits('Enter', { shiftKey: true })).toBe(false)
  })

  it('never sends on a touch keyboard — Return is its only line break', () => {
    expect(enterSubmits('Enter', { coarse: true })).toBe(false)
    expect(enterSubmits('Enter', { shiftKey: true, coarse: true })).toBe(false)
  })

  it('ignores every other key', () => {
    expect(enterSubmits('a')).toBe(false)
    expect(enterSubmits('Escape')).toBe(false)
  })
})
