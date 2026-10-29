import { describe, it, expect, beforeEach } from 'vitest'
import { dequeue, enqueue, loadQueue, queueKey, queuedLabel, saveQueue } from './turn_queue.js'

describe('turn queue', () => {
  it('ignores blank input rather than queueing an empty turn', () => {
    expect(enqueue([], '   ')).toEqual([])
    expect(enqueue(['a'], '')).toEqual(['a'])
  })

  it('trims and appends in order', () => {
    expect(enqueue(enqueue([], ' first '), 'second').map((entry) => entry.text)).toEqual(['first', 'second'])
  })

  it('keeps repeated text as separate action-bearing operations', () => {
    const queue = enqueue(enqueue([], 'ship it'), 'ship it')
    expect(queue.map((entry) => entry.text)).toEqual(['ship it', 'ship it'])
    expect(queue[0].clientActionId).not.toBe(queue[1].clientActionId)
  })

  it('dequeues one at a time, oldest first', () => {
    const [next, rest] = dequeue(['a', 'b'])
    expect(next.text).toBe('a')
    expect(rest[0].text).toBe('b')
    expect(dequeue([])).toEqual([null, []])
    expect(dequeue(undefined)).toEqual([null, []])
  })

  it('collapses a multi-line message onto one chip line', () => {
    expect(queuedLabel('one\n\ntwo   three')).toBe('one two three')
    expect(queuedLabel('x'.repeat(100))).toHaveLength(72)
  })
})

describe('turn queue persistence', () => {
  beforeEach(() => localStorage.clear())

  it('keys by thread with a session fallback, same rules as composer drafts', () => {
    expect(queueKey({ provider: 'claude', threadId: 'nat-1', sessionId: 'aaaa' })).toBe(
      queueKey({ provider: 'claude', threadId: 'nat-1', sessionId: 'bbbb' }),
    )
    expect(queueKey({ sessionId: 'aaaa' })).toBe('nano-queue:session:aaaa')
    expect(queueKey({})).toBe('')
  })

  it('round-trips messages and treats an empty queue as a deletion', () => {
    const key = queueKey({ provider: 'codex', threadId: 'nat-1' })
    saveQueue(key, ['first', 'second'])
    expect(loadQueue(key).map((entry) => entry.text)).toEqual(['first', 'second'])
    saveQueue(key, [])
    expect(localStorage.getItem(key)).toBeNull()
  })

  it('survives a corrupt or foreign stored value by returning an empty queue', () => {
    const key = queueKey({ provider: 'codex', threadId: 'nat-2' })
    localStorage.setItem(key, 'not json')
    expect(loadQueue(key)).toEqual([])
    localStorage.setItem(key, JSON.stringify({ nope: true }))
    expect(loadQueue(key)).toEqual([])
    localStorage.setItem(key, JSON.stringify(['ok', 42, '  ']))
    expect(loadQueue(key).map((entry) => entry.text)).toEqual(['ok'])
  })

  it('is a no-op without a key rather than writing a shared bucket', () => {
    saveQueue('', ['orphan'])
    expect(localStorage.length).toBe(0)
    expect(loadQueue('')).toEqual([])
  })
})
