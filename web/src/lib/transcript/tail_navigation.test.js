import { afterEach, describe, expect, it, vi } from 'vitest'
import { createTailNavigation } from './tail_navigation.js'

describe('tail navigation', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('holds only a recent deliberate scroll away from the tail', () => {
    const scroller = { scrollHeight: 1000, scrollTop: 200, clientHeight: 200, style: {} }
    const navigation = createTailNavigation(() => scroller)

    // A programmatic/non-input scroll is not a reading-position preference.
    expect(navigation.observeScroll(1000)).toBe(false)
    expect(navigation.hasManualScrollHold(1000)).toBe(false)

    navigation.markScrollInput(2000)
    expect(navigation.observeScroll(2100)).toBe(false)
    expect(navigation.hasManualScrollHold(2100 + 10 * 60 * 1000)).toBe(true)
    expect(navigation.hasManualScrollHold(2101 + 10 * 60 * 1000)).toBe(false)

    // Returning to the live edge clears the preference immediately.
    scroller.scrollTop = 800
    expect(navigation.observeScroll(2200)).toBe(true)
    expect(navigation.hasManualScrollHold(2200)).toBe(false)
  })

  it('coalesces presentation growth without fighting a manual scroll', () => {
    vi.useFakeTimers()
    const scroller = { scrollHeight: 1000, scrollTop: 700, clientHeight: 200, style: {} }
    const navigation = createTailNavigation(() => scroller)

    navigation.followGrowth()
    navigation.followGrowth()
    expect(scroller.scrollTop).toBe(700)
    vi.runOnlyPendingTimers()
    expect(scroller.scrollTop).toBe(1000)

    scroller.scrollTop = 500
    const now = Date.now()
    navigation.markScrollInput(now)
    navigation.observeScroll(now + 1)
    navigation.followGrowth()
    vi.runOnlyPendingTimers()
    expect(scroller.scrollTop).toBe(500)
  })
})
