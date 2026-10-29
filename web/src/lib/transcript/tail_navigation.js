const SCROLL_SETTLE_MAX_FRAMES = 12
const SCROLL_BOTTOM_EPS = 2
const MANUAL_SCROLL_HOLD_MS = 10 * 60 * 1000
const RECENT_SCROLL_INPUT_MS = 1500

export function createTailNavigation(getScroller) {
  let settleHandle = 0
  let followHandle = 0
  let lastScrollInputAt = 0
  let manualScrollAwayAt = 0

  const requestFrame = (callback) => {
    if (typeof requestAnimationFrame === 'function') return requestAnimationFrame(callback)
    return setTimeout(callback, 0)
  }

  const cancelFrame = (handle) => {
    if (!handle) return
    if (typeof cancelAnimationFrame === 'function') cancelAnimationFrame(handle)
    else clearTimeout(handle)
  }

  function isNearBottom(threshold = 120) {
    const scroller = getScroller()
    if (!scroller) return true
    return scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight <= threshold
  }

  function markScrollInput(now = Date.now()) {
    // A real input wins over a settle loop already in flight. This matters on
    // a just-revealed pane where the operator can grab the wheel before all of
    // the bounded layout-repair frames have completed.
    cancelFrame(settleHandle)
    settleHandle = 0
    cancelFrame(followHandle)
    followHandle = 0
    lastScrollInputAt = now
  }

  function observeScroll(now = Date.now()) {
    const atBottom = isNearBottom()
    if (atBottom) manualScrollAwayAt = 0
    else if (lastScrollInputAt > 0 && now >= lastScrollInputAt && now - lastScrollInputAt <= RECENT_SCROLL_INPUT_MS) {
      manualScrollAwayAt = now
    }
    return atBottom
  }

  function hasManualScrollHold(now = Date.now()) {
    return manualScrollAwayAt > 0 && now - manualScrollAwayAt <= MANUAL_SCROLL_HOLD_MS
  }

  function manualScrollHoldAt() {
    return manualScrollAwayAt
  }

  function restoreManualScrollHold(value) {
    const restored = Number(value || 0)
    if (Number.isFinite(restored) && restored > manualScrollAwayAt) manualScrollAwayAt = restored
  }

  function clearManualScrollHold() {
    manualScrollAwayAt = 0
  }

  function pinInstant(top = null) {
    const scroller = getScroller()
    if (!scroller) return
    const previous = scroller.style.scrollBehavior
    scroller.style.scrollBehavior = 'auto'
    scroller.scrollTop = top == null ? scroller.scrollHeight : top
    scroller.style.scrollBehavior = previous
  }

  function settleToBottom(onSettled = null) {
    cancelFrame(settleHandle)
    settleHandle = 0
    let lastHeight = -1
    let stableFrames = 0
    let frames = 0
    const step = () => {
      settleHandle = 0
      const scroller = getScroller()
      if (!scroller) return
      pinInstant()
      stableFrames = scroller.scrollHeight === lastHeight ? stableFrames + 1 : 0
      lastHeight = scroller.scrollHeight
      frames += 1
      if ((stableFrames >= 2 && isNearBottom(SCROLL_BOTTOM_EPS)) || frames >= SCROLL_SETTLE_MAX_FRAMES) {
        onSettled?.()
        return
      }
      settleHandle = requestFrame(step)
    }
    // Pin synchronously so making a hidden tab visible cannot paint its stale
    // viewport for one frame; the loop then repairs any later layout growth.
    pinInstant()
    settleHandle = requestFrame(step)
  }

  // Smoothed text can change height 20–30 times per second. Coalesce those
  // small growth notifications into one pin per animation frame instead of
  // restarting the heavier bounded settle loop for every visible patch.
  function followGrowth() {
    if (followHandle || hasManualScrollHold()) return
    followHandle = requestFrame(() => {
      followHandle = 0
      if (!hasManualScrollHold()) pinInstant()
    })
  }

  // A submitted prompt is a stronger anchor than the transcript's estimated
  // bottom. Session rebinds and optimistic→provider row replacement can move
  // that row while the surrounding scroll height also changes, so keep its
  // bottom edge inside the viewport until layout settles.
  function settleElementIntoView(getElement, onSettled = null) {
    cancelFrame(settleHandle)
    settleHandle = 0
    let lastBottom = null
    let stableFrames = 0
    let frames = 0

    const pin = () => {
      const scroller = getScroller()
      const element = getElement?.()
      if (!scroller || !element) return { found: false, visible: false, bottom: null }
      const scrollerRect = scroller.getBoundingClientRect()
      const elementRect = element.getBoundingClientRect()
      const delta = elementRect.bottom - scrollerRect.bottom
      if (Math.abs(delta) > SCROLL_BOTTOM_EPS) {
        const previous = scroller.style.scrollBehavior
        scroller.style.scrollBehavior = 'auto'
        scroller.scrollTop += delta
        scroller.style.scrollBehavior = previous
      }
      const settledRect = element.getBoundingClientRect()
      return {
        found: true,
        visible: settledRect.bottom >= scrollerRect.top - SCROLL_BOTTOM_EPS &&
          settledRect.bottom <= scrollerRect.bottom + SCROLL_BOTTOM_EPS,
        bottom: settledRect.bottom,
      }
    }

    const step = () => {
      settleHandle = 0
      const result = pin()
      frames += 1
      if (!result.found) {
        onSettled?.(result)
        return
      }
      stableFrames = result.bottom === lastBottom ? stableFrames + 1 : 0
      lastBottom = result.bottom
      if ((stableFrames >= 2 && result.visible) || frames >= SCROLL_SETTLE_MAX_FRAMES) {
        onSettled?.(result)
        return
      }
      settleHandle = requestFrame(step)
    }

    const initial = pin()
    if (!initial.found) {
      onSettled?.(initial)
      return
    }
    settleHandle = requestFrame(step)
  }

  function destroy() {
    cancelFrame(settleHandle)
    cancelFrame(followHandle)
    settleHandle = 0
    followHandle = 0
  }

  return {
    clearManualScrollHold,
    destroy,
    followGrowth,
    hasManualScrollHold,
    manualScrollHoldAt,
    markScrollInput,
    observeScroll,
    pinInstant,
    restoreManualScrollHold,
    settleElementIntoView,
    settleToBottom,
  }
}
