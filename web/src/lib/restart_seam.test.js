// @vitest-environment jsdom
//
// Prompt-loss hardening, Unit C — the restart seam. A nano deploy restart
// mints new session ids for every re-adopted thread, and the poll that
// noticed the old id died can land seconds before the one carrying the
// successor. The pane used to flip to read-only history in that window —
// unmounting the composer and eating whatever was typed in it — then revive
// by itself. These pin the new behavior: the live view holds through the
// window under a "reconnecting" hint, adopts the successor in place, and goes
// historical only when the grace expires (or instantly on a deliberate
// detach, covered in thread_pane_view.test.js).
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup } from '@testing-library/svelte'
import { fireEvent } from '@testing-library/dom'
import { tick } from 'svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import {
  dropSessionStore,
} from '../state.svelte.js'
import { projectionSnapshot, sendFileSnapshot } from '../test_projection.js'
import { __resetPanesForTest, adoptSessionTabs, openThreadTab, panes, tabTarget } from '../panes.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() {}
  open() { this.onopen?.() }
  message(update) { this.onmessage?.({ data: JSON.stringify(update) }) }
}
MockEventSource.instances = []

const emptyTranscript = {
  items: [],
  turns: [],
  page: { hasMoreBefore: false, beforeCursor: null },
  projection: { state: 'ready', epoch: 'tpe_1' },
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

const catalogRow = { id: 'nth_1', provider: 'codex', nativeId: 'native-1', title: 'Seam', metadata: {} }

function session(overrides = {}) {
  return {
    id: 'session-1', hostedThreadId: 'nht-thread-1', provider: 'codex', threadId: 'native-1',
    cwd: '/w', status: 'ready', turnActive: false, ...overrides,
  }
}

// The tab comes out of panes.tabs, as it does in the app: adoptSessionTabs
// mutates it there, and the pane must react to that mutation.
function openLiveTab() {
  __resetPanesForTest()
  const tabId = openThreadTab(tabTarget({ item: catalogRow, session: session() }))
  return panes.tabs[tabId]
}

beforeEach(() => {
  MockEventSource.instances = []

  vi.stubGlobal('EventSource', MockEventSource)
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url) => {
      if (String(url).includes('/transcript')) return jsonResponse(emptyTranscript)
      return jsonResponse({})
    }),
  )
  localStorage.clear()
  localStorage.setItem('nano-view', 'clean')
  app.sessions = [session()]
  explorer.dataset = [catalogRow]
})

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')
  dropSessionStore('session-2')

  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('restart seam', () => {

  it('keeps the composer mounted, with its text, across a session-id swap', async () => {
    const tab = openLiveTab()
    const { container } = render(ThreadPaneView, { tab })
    MockEventSource.instances.find((source) => source.url.includes('/live?'))?.open()
    const textarea = container.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'mid-thought' } })

    // The poll that noticed the restart: the old id is gone, nothing yet in
    // its place. The pane holds the live view and says why.
    app.sessions = []
    adoptSessionTabs(app.sessions)
    await tick()
    expect(container.querySelector('.composer textarea')).toBe(textarea)
    expect(container.querySelector('.pane-reconnecting')).not.toBeNull()
    expect(container.querySelector('.pane-readonly')).toBeNull()

    // The next poll carries the re-adopted successor; the tab adopts its id in
    // place, and the composer — same DOM node — still holds the words.
    app.sessions = [session({ id: 'session-2' })]
    adoptSessionTabs(app.sessions)
    await tick()
    expect(tab.sessionId).toBe('session-2')
    expect(container.querySelector('.composer textarea')).toBe(textarea)
    expect(textarea.value).toBe('mid-thought')
    expect(container.querySelector('.pane-reconnecting')).toBeNull()
  })

  it('submits through the successor when the old id rejects during restart', async () => {
    const successor = session({ id: 'session-2' })
    const turnURLs = []
    let actionID = ''
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url, init) => {
        const target = String(url)
        if (target === '/api/sessions/session-1/state') {
          // The summary already carries enough stable identity to recover; a
          // slow sidebar snapshot must not make the first prompt miss the seam.
          return await new Promise(() => {})
        }
        if (target.endsWith('/turn')) {
          turnURLs.push(target)
          actionID = JSON.parse(init.body).clientActionId
          if (target.includes('session-1')) {
            return { ok: false, status: 404, text: async () => JSON.stringify({ error: { code: 'not_found', message: 'no such session' } }) }
          }
          return jsonResponse({ accepted: true })
        }
        if (target === '/api/sessions') {
          return jsonResponse({ sessions: [successor], detached: [], defaults: {} })
        }
        if (target.includes('/transcript')) return jsonResponse(emptyTranscript)
        return jsonResponse({})
      }),
    )
    app.activeId = 'session-1'
    const tab = openLiveTab()
    const { container } = render(ThreadPaneView, { tab })
    MockEventSource.instances.find((source) => source.url.includes('/live?'))?.open()
    const textarea = container.querySelector('.composer textarea')
    await fireEvent.input(textarea, { target: { value: 'cross the restart' } })
    await fireEvent.click(container.querySelector('[aria-label="Send"]'))

    await vi.waitFor(() => expect(turnURLs).toEqual([
      '/api/sessions/session-1/turn',
      '/api/sessions/session-2/turn',
    ]))
    expect(tab.sessionId).toBe('session-2')
    expect(app.activeId).toBe('session-2')
    expect(container.querySelector('.send-notice')).toBeNull()
    expect(textarea.value).toBe('')
    const snapshot = projectionSnapshot({ items: [{
      id: `row:codex:${actionID}`, nativeItemID: '', type: 'userMessage',
      text: 'cross the restart', clientActionId: actionID, status: 'accepted', done: true,
      item: { type: 'userMessage' },
    }] })
    snapshot.epoch = 'successor-owner'
    snapshot.revision = 1
    sendFileSnapshot(MockEventSource.instances.filter((source) => source.url.includes('/live?')).at(-1), snapshot)
    await tick()
    expect([...container.querySelectorAll('.item.userMessage .bubble')].map((node) => node.textContent.trim()))
      .toContain('cross the restart')
  })

  it('goes historical only after the grace expires on a genuine kill', async () => {
    vi.useFakeTimers()
    const tab = openLiveTab()
    const { container } = render(ThreadPaneView, { tab })
    expect(container.querySelector('.composer textarea')).not.toBeNull()

    app.sessions = []
    adoptSessionTabs(app.sessions)
    await tick()
    expect(container.querySelector('.pane-reconnecting')).not.toBeNull()
    expect(container.querySelector('.composer textarea')).not.toBeNull()

    // Half the grace: still holding.
    await vi.advanceTimersByTimeAsync(2500)
    expect(container.querySelector('.composer textarea')).not.toBeNull()

    // Grace over, no successor: this really was a kill.
    await vi.advanceTimersByTimeAsync(3000)
    expect(container.querySelector('.composer textarea')).toBeNull()
    expect(container.querySelector('.pane-reconnecting')).toBeNull()
    expect(container.querySelector('.pane-readonly')).not.toBeNull()
  })
})
