// A deploy can change the bundle Nano serves, and a tab still running the old
// bundle must not stay on it. A tab reloads onto the new bundle by itself,
// once, when the operator is not looking at it (the page is hidden, or it
// shows no thread) and it holds nothing a reload could lose: no composer text,
// no prompt without a saved row, and no unsaved edit. Any other tab goes on
// working and shows a note whose button reloads. The button waits while a
// prompt is unsettled and asks first before it discards an unsaved edit.
//
// The tab compares the bundle it loaded with the one Nano serves each time
// the stream reopens. It remembers no served build, so a read that fails or
// is answered by the new server during a deploy cannot hide the change.

import { SvelteSet } from 'svelte/reactivity'
import { app } from './app.svelte.js'
import { panes } from './panes.svelte.js'
import { instructionFileUnsaved, sendUnsettled } from './state.svelte.js'

export const DISCARD_AND_RELOAD = 'Discard unsaved file changes and reload?'

// Mounted composers that hold text. Drafts survive a reload, but a tab does
// not reload under someone part way through a message.
const composing = new SvelteSet()

export function noteComposerText(composer, hasText) {
  if (hasText) composing.add(composer)
  else composing.delete(composer)
}

// loadedBundle names the entry script this page loaded, as /api/v1/build
// names it. The dev server's page loads source, not a built entry: null, so
// that page never reloads.
function loadedBundle() {
  const src = document.querySelector('script[type="module"][src*="/assets/index-"]')?.getAttribute('src')
  return src ? src.slice(src.lastIndexOf('/') + 1) : null
}

// readServedBundle names the bundle Nano serves; null when it cannot.
async function readServedBundle() {
  try {
    const response = await fetch('/api/v1/build')
    if (!response.ok) return null
    return (await response.json())?.bundle ?? null
  } catch {
    return null
  }
}

function unsavedEdit() {
  return Object.values(panes.tabs).some((tab) => tab?.kind === 'file' && tab.fileDirty) || instructionFileUnsaved()
}

function showsThread() {
  return app.explorerOpen || Object.values(panes.tabs).some((tab) => tab?.kind === 'thread')
}

export function createBuildReload({ reload, confirm = (text) => window.confirm(text) }) {
  const state = $state({ pending: false })
  const page = $state({ hidden: document.visibilityState === 'hidden' })
  const loaded = loadedBundle()
  let reloaded = false

  function reloadOnce() {
    if (reloaded) return
    reloaded = true
    reload()
  }

  return {
    state,
    // sending holds the note's button while a prompt has no saved row.
    get sending() {
      return sendUnsettled()
    },
    // start reloads by itself from now on, as soon as a new bundle is pending,
    // nobody is looking, and nothing would be lost.
    start() {
      document.addEventListener('visibilitychange', () => {
        page.hidden = document.visibilityState === 'hidden'
      })
      $effect.root(() => {
        $effect(() => {
          if (!state.pending || (!page.hidden && showsThread())) return
          if (composing.size === 0 && !sendUnsettled() && !unsavedEdit()) reloadOnce()
        })
      })
    },
    // opened reads the served bundle again once the page reaches Nano.
    async opened() {
      if (loaded === null) return
      const served = await readServedBundle()
      if (served !== null && served !== loaded) state.pending = true
    },
    // reloadNow is the note's button.
    reloadNow() {
      if (sendUnsettled()) return
      if (unsavedEdit() && !confirm(DISCARD_AND_RELOAD)) return
      reloadOnce()
    },
  }
}
