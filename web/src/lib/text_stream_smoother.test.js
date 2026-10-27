import { beforeEach, describe, expect, it } from 'vitest'
import {
  __resetStreamRatesForTest,
  createTextStreamSmoother,
  graphemes,
  providerStreamRate,
} from './text_stream_smoother.js'

function harness(options = {}) {
  let now = 0
  let nextID = 1
  const frames = new Map()
  const replacements = []
  const appends = []
  const settled = []
  const smoother = createTextStreamSmoother({
    now: () => now,
    requestFrame: (callback) => {
      const id = nextID++
      frames.set(id, callback)
      return id
    },
    cancelFrame: (id) => frames.delete(id),
    onReplace: (text, detail) => replacements.push({ text, detail }),
    onAppend: (delta, text, detail) => appends.push({ delta, text, detail }),
    onSettled: (text, metrics) => settled.push({ text, metrics }),
    ...options,
  })
  return {
    appends,
    frames,
    replacements,
    settled,
    smoother,
    advance(ms = 16) {
      now += ms
      const pending = [...frames.values()]
      frames.clear()
      for (const callback of pending) callback(now)
    },
    drain(step = 40) {
      for (let index = 0; index < 300 && smoother.state().backlog; index += 1) this.advance(step)
      if (smoother.state().backlog) throw new Error('smoother did not drain')
    },
  }
}

beforeEach(() => {
  __resetStreamRatesForTest()
})

describe('text stream smoother', () => {
  it('shows a cached snapshot immediately and paces later prefix growth', () => {
    const test = harness({ provider: 'codex' })
    test.smoother.sync({ text: 'Already here', animate: true })
    test.smoother.sync({ text: `Already here ${'x'.repeat(200)}`, animate: true })

    expect(test.replacements.at(-1).text).toBe('Already here')
    expect(test.appends).toHaveLength(0)
    test.advance(16)
    expect(test.appends[0].text.startsWith('Already here ')).toBe(true)
    expect(test.appends[0].text.length).toBeLessThan(213)
    test.drain()
    expect(test.smoother.state().displayed).toBe(`Already here ${'x'.repeat(200)}`)
  })

  it('paces the first batch of a genuinely new live message', () => {
    const test = harness({ provider: 'claude' })
    const full = 'A newly arrived Claude answer'
    test.smoother.sync({ text: full, animate: true, animateInitial: true })

    expect(test.replacements.at(-1)).toMatchObject({ text: '', detail: { reason: 'initial-live' } })
    test.advance(40)
    expect(test.smoother.state().displayed.length).toBeGreaterThan(0)
    expect(test.smoother.state().displayed.length).toBeLessThan(full.length)
  })

  it('keeps roughly one measured batch in reserve instead of pulsing to empty', () => {
    const test = harness({ provider: 'codex', initialRate: 250 })
    test.smoother.sync({ text: '', animate: true })
    test.smoother.sync({ text: 'x'.repeat(30), animate: true })
    test.advance(40)
    test.advance(40)
    test.advance(40)

    expect(test.smoother.state().backlog).toBeGreaterThan(0)
    test.smoother.sync({ text: 'x'.repeat(60), animate: true })
    expect(test.smoother.metrics()).toMatchObject({ batches: 2, batchIntervalMs: 120 })
    expect(test.smoother.metrics().arrivalRateCps).toBe(250)
  })

  it('accelerates above the learned rate only when backlog requires it', () => {
    const small = harness({ initialRate: 220 })
    const large = harness({ initialRate: 220 })
    small.smoother.sync({ text: '', animate: true })
    large.smoother.sync({ text: '', animate: true })
    small.smoother.sync({ text: 'x'.repeat(10), animate: true })
    large.smoother.sync({ text: 'x'.repeat(200), animate: true })
    small.advance(40)
    large.advance(40)

    expect(large.appends[0].delta.length).toBeGreaterThan(small.appends[0].delta.length)
  })

  it('reveals whole Unicode grapheme clusters', () => {
    const test = harness({ initialRate: 10, maxRate: 10, publishIntervalMs: 1 })
    test.smoother.sync({ text: 'A', animate: true })
    test.smoother.sync({ text: 'A👨‍👩‍👧‍👦B', animate: true })
    test.advance(100)

    expect(test.appends[0].delta).toBe('👨‍👩‍👧‍👦')
    expect(graphemes('A👨‍👩‍👧‍👦B')).toEqual(['A', '👨‍👩‍👧‍👦', 'B'])
  })

  it('re-segments an undisplayed grapheme split across provider snapshots', () => {
    const test = harness({ initialRate: 10, maxRate: 10, publishIntervalMs: 1 })
    test.smoother.sync({ text: 'A', animate: true })
    test.smoother.sync({ text: 'A👨‍', animate: true })
    test.smoother.sync({ text: 'A👨‍👩', animate: true })
    test.advance(100)

    expect(test.appends[0].delta).toBe('👨‍👩')
  })

  it('snaps corrections and hidden panes instead of replaying stale text', () => {
    const test = harness()
    test.smoother.sync({ text: 'prefix', animate: true })
    test.smoother.sync({ text: 'prefix plus backlog', animate: true })
    test.smoother.sync({ text: 'replacement', animate: true })
    expect(test.replacements.at(-1).detail.reason).toBe('correction')

    test.smoother.sync({ text: 'replacement plus hidden backlog', animate: true })
    test.smoother.sync({ text: 'replacement plus hidden backlog', animate: false })
    expect(test.replacements.at(-1).detail.reason).toBe('inactive')
    expect(test.smoother.state().backlog).toBe(0)
  })

  it('drains completion smoothly and reports the measured cadence', () => {
    const test = harness({ provider: 'claude', initialRate: 300 })
    const full = `answer ${'tail '.repeat(30)}`
    test.smoother.sync({ text: 'answer', animate: true })
    test.smoother.sync({ text: full, animate: true })
    test.smoother.sync({ text: full, animate: true, complete: true })

    expect(test.smoother.state().backlog).toBeGreaterThan(0)
    expect(test.replacements.at(-1).detail.reason).not.toBe('complete')
    test.drain()
    expect(test.settled).toHaveLength(1)
    expect(test.settled[0].text).toBe(full)
    expect(test.settled[0].metrics).toMatchObject({ provider: 'claude', batches: 1 })
  })

  it('settles unchanged text after a drained row leaves the live edge', () => {
    const test = harness()
    test.smoother.sync({ text: '## Complete', animate: true })
    test.smoother.sync({ text: '## Complete', animate: false, complete: true })

    expect(test.smoother.state()).toMatchObject({ displayed: '## Complete', backlog: 0, completing: false })
    expect(test.settled).toHaveLength(1)
    expect(test.settled[0].text).toBe('## Complete')
  })

  it('uses and learns separate provider rate profiles', () => {
    expect(providerStreamRate('claude')).toBe(300)
    expect(providerStreamRate('codex')).toBe(220)
    const test = harness({ provider: 'codex' })
    test.smoother.sync({ text: '', animate: true })
    test.smoother.sync({ text: 'x'.repeat(24), animate: true })
    test.advance(120)
    test.smoother.sync({ text: 'x'.repeat(84), animate: true })

    expect(providerStreamRate('codex')).toBeGreaterThan(220)
    expect(providerStreamRate('claude')).toBe(300)
  })

  it('cancels its scheduled frame on destroy', () => {
    const test = harness()
    test.smoother.sync({ text: '', animate: true })
    test.smoother.sync({ text: 'pending', animate: true })
    expect(test.frames.size).toBe(1)

    test.smoother.destroy()

    expect(test.frames.size).toBe(0)
  })
})
