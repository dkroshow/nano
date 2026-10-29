// The viewport module decides which subtree mounts on a phone, so its failure
// mode is a desktop layout on a 390px screen (or, worse, a mobile layout on a
// desktop). Both directions are covered here: what it reads at import, what it
// does when the query flips, and what it answers when there is no matchMedia to
// ask — the node/jsdom case every other test in this suite runs under.
import { describe, it, expect, vi, afterEach } from 'vitest'

const MOBILE = '(max-width: 700px)'
const COARSE = '(pointer: coarse)'

// A matchMedia stand-in that remembers its change listeners, so a test can
// resize the "window" the same way a browser would.
function fakeMatchMedia(initial = {}) {
  const listeners = new Map()
  const fn = (query) => ({
    matches: Boolean(initial[query]),
    addEventListener: (type, handler) => {
      if (type === 'change') listeners.set(query, handler)
    },
  })
  return { fn, fire: (query, matches) => listeners.get(query)?.({ matches }) }
}

// The module reads matchMedia once, at import, so each case needs a fresh one.
async function loadViewport(media) {
  vi.resetModules()
  if (media) vi.stubGlobal('matchMedia', media.fn)
  else vi.stubGlobal('matchMedia', undefined)
  return (await import('./viewport.svelte.js')).viewport
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('viewport', () => {
  it('answers desktop when there is no matchMedia to ask', async () => {
    const viewport = await loadViewport(null)

    // Safe defaults matter: this is the answer every logic test and every
    // server-side render gets, and it must be the layout that already works.
    expect(viewport).toEqual({ mobile: false, coarse: false })
  })

  it('reads both queries at import', async () => {
    const viewport = await loadViewport(fakeMatchMedia({ [MOBILE]: true, [COARSE]: true }))

    expect(viewport.mobile).toBe(true)
    expect(viewport.coarse).toBe(true)
  })

  it('keeps the two queries independent — a touch laptop is coarse but not mobile', async () => {
    const viewport = await loadViewport(fakeMatchMedia({ [COARSE]: true }))

    expect(viewport.mobile).toBe(false)
    expect(viewport.coarse).toBe(true)
  })

  it('follows the query after import, so a rotation or resize re-lays out', async () => {
    const media = fakeMatchMedia({ [MOBILE]: true })
    const viewport = await loadViewport(media)
    expect(viewport.mobile).toBe(true)

    media.fire(MOBILE, false)

    expect(viewport.mobile).toBe(false)
  })
})
