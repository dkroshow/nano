// @vitest-environment jsdom
//
// A deploy changes the bundle Nano serves. A tab nobody is looking at (hidden,
// or showing no thread) that holds nothing a reload could lose reloads onto it
// by itself, once; any other tab keeps every pane as it is and waits for the
// user. Boundary: the reload decision over the page's own entry script, Nano's
// build read, page visibility, and the app's real tab, composer, overlay and
// store state.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { tick } from 'svelte'
import { render, cleanup, fireEvent } from '@testing-library/svelte'
import ThreadPaneView from './lib/panes/ThreadPaneView.svelte'
import NewVersionNote from './lib/status/NewVersionNote.svelte'
import { createBuildReload, DISCARD_AND_RELOAD } from './build_reload.svelte.js'
import { app } from './app.svelte.js'
import {
  __resetPanesForTest,
  closeTab,
  openFileTab,
  openThreadTab,
  panes,
  setFileTabDirty,
  tabTarget,
} from './panes.svelte.js'
import { dropSessionStore, getSessionStore } from './state.svelte.js'
import { projectionSnapshot, sendFileSnapshot } from './test_projection.js'

class MockEventSource {
  constructor(url) { this.url = url; MockEventSource.instances.push(this) }
  close() {}
}
MockEventSource.instances = []

const catalogRow = { id: 'nth_1', provider: 'claude', nativeId: 'native-1', title: 'Reload', metadata: {} }
const session = { id: 'session-1', hostedThreadId: 'nht-thread-1', provider: 'claude', threadId: 'native-1', cwd: '/w', status: 'ready' }

// The page's entry script and GET /api/v1/build as a served Nano gives them.
const LOADED_ENTRY = '<script type="module" crossorigin src="/assets/index-gu4KM9Nx.js"></script>'
const LOADED = { bundle: 'index-gu4KM9Nx.js', stamp: '20260929T132107Z-beabd589-94585', storeSchema: 22 }
const NEXT = { bundle: 'index-C94yu3j4.js', stamp: '20260930T140000Z-c948273a-12345', storeSchema: 22 }

// nano is a fake Nano behind fetch. A build read answers with the build
// serving when it is answered. While down, reads fail as they do while Nano
// restarts; while holding, reads wait for release, as a read sent just before
// a restart can be answered by the new server.
let nano

// The page's visibility as the browser reports it.
let visibility

function setVisibility(next) {
  visibility = next
  document.dispatchEvent(new Event('visibilitychange'))
}

function release() {
  for (const answer of nano.held.splice(0)) answer()
}

async function settled() {
  for (let i = 0; i < 5; i++) {
    await tick()
    await Promise.resolve()
  }
}

// seesNewBuild starts a reload watch on the loaded build, deploys the next
// build, and lets the watch see it when the stream reopens.
async function seesNewBuild(confirm = vi.fn(() => true)) {
  const reload = vi.fn()
  const watch = createBuildReload({ reload, confirm })
  watch.start()
  await settled()
  nano.build = NEXT
  watch.opened()
  await settled()
  return { watch, reload }
}

// threadOnScreen renders a thread tab's pane and lets the watch see a new
// build. visible: false renders it as an inactive tab.
async function threadOnScreen({ visible = true } = {}) {
  app.sessions = [session]
  const tabId = openThreadTab(tabTarget({ item: catalogRow, session }))
  const view = render(ThreadPaneView, { tab: panes.tabs[tabId], visible })
  await settled()
  const seen = await seesNewBuild()
  return { ...view, ...seen }
}

const unsentSend = {
  clientActionId: 'action-unsent', provider: 'claude', threadId: 'native-1', sessionId: 'session-1',
  text: 'a prompt', createdAt: Date.now(),
}

beforeEach(() => {
  visibility = 'visible'
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
  __resetPanesForTest()
  app.explorerOpen = false
  document.head.innerHTML = LOADED_ENTRY
  nano = { build: LOADED, down: false, holding: false, held: [] }
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal('fetch', vi.fn((url) => {
    if (url !== '/api/v1/build') return Promise.resolve({ ok: true, status: 200, headers: { get: () => null }, json: async () => ({}), text: async () => '' })
    const answer = () => nano.down
      ? Promise.reject(new TypeError('Failed to fetch'))
      : Promise.resolve({ ok: true, status: 200, json: async () => ({ ...nano.build }) })
    if (!nano.holding) return answer()
    return new Promise((resolve) => nano.held.push(() => resolve(answer())))
  }))
})

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')
  localStorage.clear()
  app.explorerOpen = false
  document.head.innerHTML = ''
  vi.unstubAllGlobals()
})

describe('reload on a new served build', () => {
  for (const [name, open] of [
    ['no tab', () => {}],
    ['only a clean file tab', () => openFileTab({ path: '/w/notes.md' }, { cwd: '/w' })],
  ]) {
    it(`reloads by itself once with ${name}`, async () => {
      open()
      const { watch, reload } = await seesNewBuild()
      expect(reload).toHaveBeenCalledTimes(1)
      watch.opened()
      await settled()
      expect(reload).toHaveBeenCalledTimes(1)
    })
  }

  for (const [name, next] of [
    ['the same build', () => {}],
    ['a failed build read', () => { nano.build = NEXT; nano.down = true }],
  ]) {
    it(`reloads nothing when the stream reopens on ${name}`, async () => {
      const reload = vi.fn()
      const watch = createBuildReload({ reload })
      watch.start()
      await settled()
      next()
      watch.opened()
      await settled()
      expect(reload).not.toHaveBeenCalled()
      expect(watch.state.pending).toBe(false)
    })
  }

  // A deploy can land while the tab makes its first build read. Either way
  // the tab still runs the old bundle, and it must see the new one when the
  // stream reopens on the new server.
  for (const [name, deploy] of [
    ['fails while Nano restarts', async (watch) => {
      nano.down = true
      watch.start()
      await settled()
      nano.build = NEXT
      nano.down = false
    }],
    ['is answered late by the new build', async (watch) => {
      nano.holding = true
      watch.start()
      await settled()
      nano.holding = false
      // The stream opens on the old build before the deploy.
      watch.opened()
      await settled()
      nano.build = NEXT
      release()
      await settled()
    }],
  ]) {
    it(`reloads once when the first build read ${name}`, async () => {
      const reload = vi.fn()
      const watch = createBuildReload({ reload })
      await deploy(watch)
      watch.opened()
      await settled()
      expect(reload).toHaveBeenCalledTimes(1)
    })
  }

  // The dev server's page loads source, not a built entry, so it cannot name
  // the bundle it runs; were it to count as old, it would reload on every
  // stream open, forever.
  it('never reloads a page with no built entry script (the dev server)', async () => {
    document.head.innerHTML = '<script type="module" src="/@vite/client"></script><script type="module" src="/src/main.js"></script>'
    const { watch, reload } = await seesNewBuild()
    expect(reload).not.toHaveBeenCalled()
    expect(watch.state.pending).toBe(false)
  })

  it('waits while a thread tab is open, visible or hidden, and reloads once when the last one closes', async () => {
    const thread = openThreadTab(tabTarget({ item: catalogRow, session }))
    const { reload } = await seesNewBuild()
    expect(reload).not.toHaveBeenCalled()
    // A file tab in front hides the thread tab; it still counts.
    openFileTab({ path: '/w/notes.md' }, { cwd: '/w' })
    await settled()
    expect(panes.items[0].activeTabId).not.toBe(thread)
    expect(reload).not.toHaveBeenCalled()
    closeTab(panes.items[0].id, thread)
    await settled()
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('waits while the Explorer overlay is open and reloads once when it closes', async () => {
    app.explorerOpen = true
    const { reload } = await seesNewBuild()
    expect(reload).not.toHaveBeenCalled()
    app.explorerOpen = false
    await settled()
    expect(reload).toHaveBeenCalledTimes(1)
  })

  for (const [name, leaveUnsaved, save] of [
    ['a file tab', () => {
      const tab = openFileTab({ path: '/w/notes.md' }, { cwd: '/w' })
      setFileTabDirty(tab, true)
      return () => setFileTabDirty(tab, false)
    }],
    ['an instruction file', () => {
      const { store } = getSessionStore('session-1')
      store.openFiles.push({ path: '/w/CLAUDE.md', basename: 'CLAUDE.md', dirty: true })
      return () => { store.openFiles[0].dirty = false }
    }],
  ]) {
    it(`waits while ${name} has unsaved changes, through a failed save, and reloads once it is saved`, async () => {
      const saved = leaveUnsaved()
      const { reload } = await seesNewBuild()
      expect(reload).not.toHaveBeenCalled()
      // A failed save leaves the edit unsaved.
      await settled()
      expect(reload).not.toHaveBeenCalled()
      saved()
      await settled()
      expect(reload).toHaveBeenCalledTimes(1)
    })
  }

  it('keeps file-stream rows reaching the rendered pane while reload waits', async () => {
    app.sessions = [session]
    const tabId = openThreadTab(tabTarget({ item: catalogRow, session }))
    const { container } = render(ThreadPaneView, { tab: panes.tabs[tabId] })
    const { reload } = await seesNewBuild()
    const source = MockEventSource.instances.find((source) => source.url.includes('/live?'))
    sendFileSnapshot(source, projectionSnapshot({ provider: 'claude', items: [{
      id: 'row:p1', type: 'userMessage', text: 'prompt 1', done: true, item: { type: 'userMessage' },
    }] }))
    await settled()
    expect(container.querySelector('.convo').textContent).toContain('prompt 1')
    expect(reload).not.toHaveBeenCalled()
  })

})

// A hidden tab is one the operator is not looking at. It reloads by itself
// only when nothing on it could be lost, and otherwise keeps waiting, so the
// note is there when the operator comes back. Boundary: the reload decision
// over the rendered composer and the real session store.
describe('a hidden tab on a new served build', () => {
  it('reloads once when it is hidden and idle', async () => {
    setVisibility('hidden')
    const { watch, reload } = await threadOnScreen()
    expect(reload).toHaveBeenCalledTimes(1)
    watch.opened()
    await settled()
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('shows the note while visible and reloads once when it goes hidden idle', async () => {
    const { reload, watch } = await threadOnScreen()
    expect(watch.state.pending).toBe(true)
    expect(reload).not.toHaveBeenCalled()
    setVisibility('hidden')
    await settled()
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('waits while an inactive tab\'s composer holds a draft restored from storage, and reloads once it is cleared', async () => {
    localStorage.setItem('nano-draft:claude:native-1', 'saved before the deploy')
    setVisibility('hidden')
    const { container, reload } = await threadOnScreen({ visible: false })
    const textarea = container.querySelector('.composer textarea')
    expect(textarea.value).toBe('saved before the deploy')
    expect(reload).not.toHaveBeenCalled()
    setVisibility('visible')
    setVisibility('hidden')
    await settled()
    expect(reload).not.toHaveBeenCalled()
    await fireEvent.input(textarea, { target: { value: '' } })
    await settled()
    expect(reload).toHaveBeenCalledTimes(1)
  })

  // Only a prompt this page holds and Nano does not holds a reload back: one
  // still being sent, or one Nano never acknowledged. Nano republishes every
  // other prompt to the reloaded page.
  for (const [name, hold, release] of [
    ['being sent', (store) => { store.sending = 1 }, (store) => { store.sending = 0 }],
    ['unacknowledged', (store) => { store.unsent = [unsentSend] }, (store) => { store.unsent = [] }],
  ]) {
    it(`waits while a prompt is ${name}, and reloads once Nano has it`, async () => {
      app.sessions = [session]
      openThreadTab(tabTarget({ item: catalogRow, session }))
      const { store } = getSessionStore('session-1')
      hold(store)
      setVisibility('hidden')
      const { reload } = await seesNewBuild()
      expect(reload).not.toHaveBeenCalled()
      release(store)
      await settled()
      expect(reload).toHaveBeenCalledTimes(1)
    })
  }

  // A refused POST ends the send and hands its text back to the composer. A
  // reload between those two steps would find the text in neither place.
  it('keeps a hidden tab whose send Nano refuses, with the text back in the composer', async () => {
    let refuse
    const build = globalThis.fetch
    vi.stubGlobal('fetch', vi.fn((url, options) => String(url).endsWith('/turn')
      ? new Promise((resolve) => { refuse = () => resolve({ ok: false, status: 400, headers: { get: () => null }, json: async () => ({}), text: async () => 'bad request' }) })
      : build(url, options)))
    const { container, getByRole, reload } = await threadOnScreen()
    MockEventSource.instances.find((source) => source.url.includes('/live?'))?.onopen?.()
    const textarea = container.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'the refused prompt' } })
    await fireEvent.click(getByRole('button', { name: 'Send' }))
    await settled()
    expect(textarea.value).toBe('')
    setVisibility('hidden')
    await settled()
    expect(reload).not.toHaveBeenCalled()

    refuse()
    await settled()
    expect(textarea.value).toBe('the refused prompt')
    expect(reload).not.toHaveBeenCalled()
  })
})

// The note is what a tab with a thread shows instead of reloading: it stays
// visible over the Explorer overlay, leaves the composer working, and its
// button waits for an unsettled prompt and asks before it discards an unsaved
// edit. Boundary: the rendered note beside the rendered composer.
describe('the new-version note', () => {
  it('shows over the Explorer overlay and reloads once from its button', async () => {
    openThreadTab(tabTarget({ item: catalogRow, session }))
    app.explorerOpen = true
    const { watch, reload } = await seesNewBuild()
    const { container, getByRole } = render(NewVersionNote, { buildReload: watch })
    expect(container.textContent).toContain('Nano was updated —')
    expect(reload).not.toHaveBeenCalled()
    await fireEvent.click(getByRole('button', { name: 'Reload' }))
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('appears on a visible thread tab without taking the composer, and the draft survives the reload', async () => {
    const { container, reload, watch } = await threadOnScreen()
    const note = render(NewVersionNote, { buildReload: watch })
    expect(note.container.textContent).toContain('Nano was updated —')
    const textarea = container.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'half a thought' } })
    expect(textarea.disabled).toBe(false)
    expect(textarea.value).toBe('half a thought')
    await settled()
    expect(reload).not.toHaveBeenCalled()

    await fireEvent.click(note.getByRole('button', { name: 'Reload' }))
    expect(reload).toHaveBeenCalledTimes(1)
    // The reload: a fresh store and composer over the same browser storage.
    cleanup()
    dropSessionStore('session-1')
    const after = render(ThreadPaneView, { tab: panes.tabs[panes.items[0].activeTabId] })
    await settled()
    expect(after.container.querySelector('.composer textarea').value).toBe('half a thought')
  })

  it('holds its button while a prompt is unacknowledged, then reloads once from it', async () => {
    const { watch, reload } = await threadOnScreen()
    const { store } = getSessionStore('session-1')
    store.unsent = [unsentSend]
    const note = render(NewVersionNote, { buildReload: watch })
    await settled()
    const button = note.getByRole('button', { name: 'Reload' })
    expect(button.disabled).toBe(true)
    expect(note.container.textContent).toContain('Waiting for your message')
    watch.reloadNow()
    expect(reload).not.toHaveBeenCalled()

    store.unsent = []
    await settled()
    expect(button.disabled).toBe(false)
    expect(note.container.textContent).not.toContain('Waiting for your message')
    await fireEvent.click(button)
    expect(reload).toHaveBeenCalledTimes(1)
  })

  // The note can appear before a thread's session store exists; a send from
  // that store must still hold the button.
  it('holds its button for a send from a thread opened after the note appeared', async () => {
    const reload = vi.fn()
    const watch = createBuildReload({ reload })
    watch.start()
    app.explorerOpen = true
    nano.build = NEXT
    await watch.opened()
    await settled()
    const note = render(NewVersionNote, { buildReload: watch })
    expect(note.getByRole('button', { name: 'Reload' }).disabled).toBe(false)

    const build = globalThis.fetch
    vi.stubGlobal('fetch', vi.fn((url, options) => String(url).endsWith('/turn') ? new Promise(() => {}) : build(url, options)))
    app.sessions = [session]
    const tabId = openThreadTab(tabTarget({ item: catalogRow, session }))
    const view = render(ThreadPaneView, { tab: panes.tabs[tabId] })
    await settled()
    MockEventSource.instances.find((source) => source.url.includes('/live?'))?.onopen?.()
    await fireEvent.input(view.container.querySelector('.composer textarea'), { target: { value: 'a held prompt' } })
    await fireEvent.click(view.getByRole('button', { name: 'Send' }))
    await settled()
    expect(note.getByRole('button', { name: 'Reload' }).disabled).toBe(true)
    expect(note.container.textContent).toContain('Waiting for your message')
  })

  it('asks before it discards an unsaved instruction file: Cancel keeps everything, OK reloads once', async () => {
    const thread = openThreadTab(tabTarget({ item: catalogRow, session }))
    const { store } = getSessionStore('session-1')
    store.openFiles.push({ path: '/w/CLAUDE.md', basename: 'CLAUDE.md', dirty: true })
    const confirm = vi.fn(() => false)
    const { watch, reload } = await seesNewBuild(confirm)
    const { container, getByRole } = render(NewVersionNote, { buildReload: watch })

    await fireEvent.click(getByRole('button', { name: 'Reload' }))
    expect(confirm).toHaveBeenCalledWith(DISCARD_AND_RELOAD)
    expect(reload).not.toHaveBeenCalled()
    expect(store.openFiles[0].dirty).toBe(true)
    expect(panes.tabs[thread]).toBeDefined()
    expect(container.textContent).toContain('Nano was updated —')

    confirm.mockReturnValue(true)
    await fireEvent.click(getByRole('button', { name: 'Reload' }))
    expect(reload).toHaveBeenCalledTimes(1)
    await fireEvent.click(getByRole('button', { name: 'Reload' }))
    expect(reload).toHaveBeenCalledTimes(1)
  })
})
