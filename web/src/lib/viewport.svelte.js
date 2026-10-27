// One reactive answer to "is this a phone-sized, touch-driven browser?".
//
// Almost everything that changes with the viewport is a media query in
// nano.css. This module exists for the handful of places CSS cannot reach,
// because the decision is *which subtree to mount*, not how to paint it: the
// workspace renders a single pane, the sidebar becomes a drawer, and Enter
// stops sending. One listener per query, shared by every component, mirroring
// how lib/text_size.js centralizes the other cross-cutting display concern.
//
// 700px is the single mobile tier, below the existing 760/900/1100 breakpoints.
// `coarse` is a capability, not a width: a touch laptop wants the bigger hit
// areas and the newline-on-Enter keyboard even at desktop width.
//
// matchMedia is absent on the node test environment, so it is asked for rather
// than assumed. With no way to ask, both answers stay false — the desktop
// layout, which is what every existing test and every desktop browser gets.

const MOBILE_QUERY = '(max-width: 700px)'
const COARSE_QUERY = '(pointer: coarse)'

export const viewport = $state({ mobile: false, coarse: false })

function track(key, query) {
  const list = globalThis.matchMedia?.(query)
  if (!list) return
  viewport[key] = list.matches
  list.addEventListener?.('change', (event) => {
    viewport[key] = event.matches
  })
}

track('mobile', MOBILE_QUERY)
track('coarse', COARSE_QUERY)
