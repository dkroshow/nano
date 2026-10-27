import { beforeEach, describe, expect, it } from 'vitest'
import {
  __resetPanesForTest,
  activeTab,
  addPane,
  adoptSessionTabs,
  closePaneDetail,
  closeTab,
  closeThreadTabs,
  detachSessionTabs,
  focusTab,
  moveTabToPane,
  openDetailForActiveTab,
  openFileTab,
  openThreadTab,
  panes,
  reorderTab,
  reconcileFileTab,
  restoreActiveThreadView,
  saveActiveThreadView,
  splitWithTab,
  tabTarget,
} from './panes.svelte.js'

const known = (value) => ({ state: 'known', value })

function catalog(overrides = {}) {
  return {
    id: 'nth_1',
    provider: 'codex',
    nativeId: 'native-1',
    title: 'Pane mechanics',
    metadata: {
      cwd: known('/workspace/nano'),
      model: known('gpt-5.6-sol'),
      effort: known('high'),
    },
    ...overrides,
  }
}

function session(overrides = {}) {
  return {
    id: 'session-1',
    provider: 'codex',
    threadId: 'native-1',
    cwd: '/workspace/nano',
    model: 'gpt-5.6-sol',
    status: 'ready',
    ...overrides,
  }
}

beforeEach(() => {
  localStorage.removeItem('nano:active-thread-view')
  __resetPanesForTest()
})

describe('pane workspace', () => {
  it('restores the focused thread by durable identity after a page reload', () => {
    const first = openThreadTab(tabTarget({ item: catalog(), session: session() }))
    openThreadTab(tabTarget({
      item: catalog({ id: 'nth_2', nativeId: 'native-2', title: 'Second' }),
      session: session({ id: 'session-2', threadId: 'native-2' }),
    }))
    focusTab(1, first)

    expect(saveActiveThreadView()).toBe(true)
    __resetPanesForTest()
    expect(restoreActiveThreadView()).toBe(true)

    const restored = panes.tabs[panes.items[0].activeTabId]
    expect(restored).toMatchObject({
      catalogId: 'nth_1',
      sessionId: 'session-1',
      provider: 'codex',
      nativeId: 'native-1',
      title: 'Pane mechanics',
    })

    // The session id is only a hint. A Nano restart can rotate it while the
    // native thread identity remains stable.
    adoptSessionTabs([session({ id: 'session-9' })])
    expect(restored.sessionId).toBe('session-9')
  })

  it('keeps a legacy one-thread snapshot readable during the workspace upgrade', () => {
    localStorage.setItem('nano:active-thread-view', JSON.stringify({
      version: 1,
      thread: {
        catalogId: 'nth_1', sessionId: 'session-1', provider: 'codex', nativeId: 'native-1',
        title: 'Pane mechanics', cwd: '/workspace/nano', model: 'gpt-5.6-sol', effort: 'high', status: 'ready',
      },
    }))

    expect(restoreActiveThreadView()).toBe(true)
    expect(panes.tabs[panes.items[0].activeTabId]).toMatchObject({ nativeId: 'native-1', title: 'Pane mechanics' })
  })

  it('restores the complete pane layout, every thread tab, and the focused pane', () => {
    const first = openThreadTab(tabTarget({ item: catalog(), session: session() }))
    const secondPane = addPane()
    const second = openThreadTab(tabTarget({
      item: catalog({ id: 'nth_2', nativeId: 'native-2', title: 'Second' }),
      session: session({ id: 'session-2', threadId: 'native-2' }),
    }))
    const third = openThreadTab(tabTarget({
      item: catalog({ id: 'nth_3', nativeId: 'native-3', title: 'Third' }),
      session: session({ id: 'session-3', threadId: 'native-3' }),
    }))
    focusTab(1, first)

    expect(saveActiveThreadView()).toBe(true)
    __resetPanesForTest()
    expect(restoreActiveThreadView()).toBe(true)

    expect(panes.layout).toEqual([[1], [2]])
    expect(panes.activePaneId).toBe(1)
    expect(panes.items).toHaveLength(2)
    expect(panes.items[0].tabIds.map((id) => panes.tabs[id].nativeId)).toEqual(['native-1'])
    expect(panes.items[1].tabIds.map((id) => panes.tabs[id].nativeId)).toEqual(['native-2', 'native-3'])
    expect(panes.tabs[panes.items[0].activeTabId].nativeId).toBe('native-1')
    expect(panes.tabs[panes.items[1].activeTabId].nativeId).toBe('native-3')
    expect(second).not.toBe(third)
    expect(secondPane).toBe(2)
  })

  it('restores file tabs and a pane detail view with their thread workspace', () => {
    const thread = openThreadTab(tabTarget({ item: catalog(), session: session() }))
    const file = openFileTab({ path: '/workspace/nano/docs/lifecycle.md', line: 8 }, { cwd: '/workspace/nano' })
    reconcileFileTab(file, { path: '/workspace/nano/docs/lifecycle.md', basename: 'lifecycle.md', editable: true })
    focusTab(1, thread)
    const detailPane = openDetailForActiveTab(1)

    expect(saveActiveThreadView()).toBe(true)
    __resetPanesForTest()
    expect(restoreActiveThreadView()).toBe(true)

    const restoredFile = Object.values(panes.tabs).find((tab) => tab.kind === 'file')
    expect(restoredFile).toMatchObject({ path: '/workspace/nano/docs/lifecycle.md', line: 8 })
    const restoredDetail = panes.items.find((pane) => pane.id === detailPane).detail
    expect(restoredDetail).not.toBeNull()
    expect(panes.tabs[restoredDetail.tabId].nativeId).toBe('native-1')
  })

  it('marks sidebar opens as forced tail navigation and tab focus as preservable navigation', () => {
    const tabId = openThreadTab(tabTarget({ item: catalog() }))
    const initial = panes.tabs[tabId].tailResetNonce
    expect(initial).toBeGreaterThan(0)

    focusTab(1, tabId)
    const focused = panes.tabs[tabId].tailResetNonce
    expect(focused).toBeGreaterThan(initial)
    expect(panes.tabs[tabId].forceTailResetNonce).toBe(0)

    expect(openThreadTab(tabTarget({ item: catalog() }), { forceTail: true })).toBe(tabId)
    expect(panes.tabs[tabId].tailResetNonce).toBeGreaterThan(focused)
    expect(panes.tabs[tabId].forceTailResetNonce).toBe(panes.tabs[tabId].tailResetNonce)
  })

  it('merges historical and live identities into one tab', () => {
    const tabId = openThreadTab(tabTarget({ item: catalog({ runtime: { detached: true } }) }))
    expect(panes.tabs[tabId]).toMatchObject({ catalogId: 'nth_1', sessionId: '', detached: true })

    const sameTabId = openThreadTab(tabTarget({ item: catalog(), session: session() }))
    expect(sameTabId).toBe(tabId)
    expect(panes.tabs[tabId]).toMatchObject({
      catalogId: 'nth_1',
      sessionId: 'session-1',
      nativeId: 'native-1',
      detached: false,
    })
    expect(panes.items[0].tabIds).toEqual([tabId])
  })

  it('turns an open historical tab live when its thread appears in the session list', () => {
    const tabId = openThreadTab(tabTarget({ item: catalog({ runtime: { detached: true } }) }))
    expect(panes.tabs[tabId]).toMatchObject({ sessionId: '', detached: true })

    adoptSessionTabs([session()])
    expect(panes.tabs[tabId]).toMatchObject({ sessionId: 'session-1', status: 'ready', detached: false })
  })

  it('adopts sessions only onto the tab whose thread and provider match', () => {
    const codexTab = openThreadTab(tabTarget({ item: catalog() }))
    const otherTab = openThreadTab(tabTarget({ item: catalog({ id: 'nth_2', nativeId: 'native-2' }) }))
    const claudeTab = openThreadTab(
      tabTarget({ item: catalog({ id: 'nth_3', nativeId: 'native-3', provider: 'claude' }) }),
    )

    adoptSessionTabs([session(), session({ id: 'session-3', provider: 'codex', threadId: 'native-3' })])

    expect(panes.tabs[codexTab].sessionId).toBe('session-1')
    expect(panes.tabs[otherTab].sessionId).toBe('')
    // Same native id, different provider: not the same thread.
    expect(panes.tabs[claudeTab].sessionId).toBe('')
  })

  it('leaves a live tab bound while its session is still in the list', () => {
    const tabId = openThreadTab(tabTarget({ item: catalog(), session: session() }))

    adoptSessionTabs([session(), session({ id: 'session-9', threadId: 'native-9' })])
    expect(panes.tabs[tabId].sessionId).toBe('session-1')
  })

  it('re-binds a tab whose session id went stale onto the same thread’s new session', () => {
    // A nano deploy restart mints new session ids for every re-adopted thread.
    // The tab's identity is the THREAD: left bound to the dead id, the pane
    // flipped to read-only history for a poll cycle and ate in-flight prompts.
    const tabId = openThreadTab(tabTarget({ item: catalog(), session: session() }))

    adoptSessionTabs([session({ id: 'session-9' })])
    expect(panes.tabs[tabId]).toMatchObject({ sessionId: 'session-9', detached: false })
  })

  it('reports a rotated predecessor only after no tab still uses it', () => {
    const tabId = openThreadTab(tabTarget({ item: catalog(), session: session() }))
    const secondTabId = openThreadTab(tabTarget({ item: catalog({ id: 'nth_2', nativeId: 'native-2' }), session: session({ threadId: 'native-2' }) }))
    panes.tabs[secondTabId].sessionId = 'session-1'
    const rotated = []

    adoptSessionTabs([session({ id: 'session-9' }), session({ id: 'session-10', threadId: 'native-2' })], (id) => rotated.push(id))

    expect(panes.tabs[tabId].sessionId).toBe('session-9')
    expect(panes.tabs[secondTabId].sessionId).toBe('session-10')
    expect(rotated).toEqual(['session-1'])
  })

  it('does not re-bind a stale tab onto another provider’s session with the same native id', () => {
    const tabId = openThreadTab(tabTarget({ item: catalog(), session: session() }))

    adoptSessionTabs([session({ id: 'session-9', provider: 'claude' })])
    expect(panes.tabs[tabId].sessionId).toBe('session-1')
  })

  it('a live tab follows its session onto a new native id, and the successor row lands on it', () => {
    // `claude -p --resume` cannot continue a process: it spawns one that mints
    // a fresh native id. A tab still naming the predecessor would resume the
    // wrong thread and, when the successor gets indexed, open a second tab for
    // the session already on screen.
    const tabId = openThreadTab(
      tabTarget({ item: catalog({ provider: 'claude' }), session: session({ provider: 'claude' }) }),
    )

    adoptSessionTabs([session({ provider: 'claude', threadId: 'native-2' })])
    expect(panes.tabs[tabId].nativeId).toBe('native-2')

    const successorRow = catalog({ id: 'nth_2', nativeId: 'native-2', provider: 'claude' })
    expect(openThreadTab(tabTarget({ item: successorRow }))).toBe(tabId)
    expect(Object.keys(panes.tabs)).toHaveLength(1)
  })

  it('keeps the open detail view when a resumed session binds to its same tab', () => {
    const tabId = openThreadTab(tabTarget({ item: catalog({ runtime: { detached: true } }) }))
    const detailPane = openDetailForActiveTab(1)

    expect(openThreadTab(tabTarget({ item: catalog(), session: session() }))).toBe(tabId)
    expect(panes.items.find((pane) => pane.id === detailPane).detail).toEqual({ tabId })
  })

  it('opens new threads as tabs in the focused pane and preserves tab order', () => {
    const first = openThreadTab(tabTarget({ item: catalog() }))
    const second = openThreadTab(tabTarget({
      item: catalog({ id: 'nth_2', nativeId: 'native-2', title: 'Second' }),
    }))
    expect(panes.items[0].tabIds).toEqual([first, second])
    reorderTab(1, second, first, true)
    expect(panes.items[0].tabIds).toEqual([second, first])
    const beforeReturn = panes.tabs[first].tailResetNonce
    closeTab(1, second)
    expect(panes.items[0].tabIds).toEqual([first])
    expect(panes.tabs[first].tailResetNonce).toBeGreaterThan(beforeReturn)
  })

  it('moves tabs across columns and creates vertical stacks', () => {
    const first = openThreadTab(tabTarget({ item: catalog() }))
    const second = openThreadTab(tabTarget({
      item: catalog({ id: 'nth_2', nativeId: 'native-2', title: 'Second' }),
    }))
    const secondPane = addPane()
    moveTabToPane(second, secondPane)
    expect(panes.layout).toEqual([[1], [2]])
    expect(panes.items.find((pane) => pane.id === secondPane).tabIds).toEqual([second])

    const stackedPane = splitWithTab(1, first, 'after')
    expect(stackedPane).not.toBeNull()
    expect(panes.layout.some((column) => column.length === 2)).toBe(true)
  })

  it('opens details in a new right pane and keeps the source tab intact', () => {
    const sourceTab = openThreadTab(tabTarget({ item: catalog() }))
    const detailPane = openDetailForActiveTab(1)
    expect(detailPane).toBe(2)
    expect(panes.layout).toEqual([[1], [2]])
    expect(panes.items.find((pane) => pane.id === detailPane).detail).toEqual({ tabId: sourceTab })
    expect(panes.items[0].tabIds).toEqual([sourceTab])
    // Session polling asks activeTab() whether the workspace is empty. A
    // detail-only companion pane still represents its source thread and must
    // not be replaced by the newest globally attached session.
    expect(activeTab()).toBe(panes.tabs[sourceTab])

    closePaneDetail(detailPane)
    expect(panes.items.find((pane) => pane.id === detailPane).detail).toBeNull()
    expect(panes.items[0].tabIds).toEqual([sourceTab])
    expect(panes.activePaneId).toBe(1)
  })

  it('uses the nearest existing pane for details without deleting its tabs', () => {
    const sourceTab = openThreadTab(tabTarget({ item: catalog() }))
    const secondPane = addPane()
    const preservedTab = openThreadTab(tabTarget({ item: catalog({ id: 'nth_2', nativeId: 'native-2' }) }))
    focusTab(1, sourceTab)

    expect(openDetailForActiveTab(1)).toBe(secondPane)
    const target = panes.items.find((pane) => pane.id === secondPane)
    expect(target.detail).toEqual({ tabId: sourceTab })
    expect(target.tabIds).toEqual([preservedTab])
    const beforeReturn = panes.tabs[preservedTab].tailResetNonce
    closePaneDetail(secondPane)
    expect(panes.tabs[preservedTab].tailResetNonce).toBeGreaterThan(beforeReturn)
    expect(panes.activePaneId).toBe(secondPane)
  })

  it('opens a file in place with one pane and in the nearest other pane with two', () => {
    const thread = openThreadTab(tabTarget({ item: catalog(), session: session() }))
    const firstFile = openFileTab({ path: '/tmp/one.md', line: 3 }, { cwd: '/workspace/nano' }, 1)
    expect(panes.items[0].tabIds).toEqual([thread, firstFile])
    expect(panes.tabs[firstFile]).toMatchObject({ kind: 'file', requestedPath: '/tmp/one.md', line: 3 })

    const secondPane = addPane()
    focusTab(1, thread)
    const secondFile = openFileTab({ path: '/tmp/two.md' }, {}, 1)
    expect(panes.items.find((pane) => pane.id === secondPane).tabIds).toContain(secondFile)
    expect(panes.activePaneId).toBe(secondPane)
  })

  it('deduplicates pending requests by authored path and resolution context', () => {
    const first = openFileTab('src/main.go', { cwd: '/workspace/one' })
    const beforeNavigation = panes.tabs[first].navigationNonce
    const same = openFileTab({ path: 'src/main.go', line: 9 }, { cwd: '/workspace/one' })
    const otherContext = openFileTab('src/main.go', { cwd: '/workspace/two' })

    expect(same).toBe(first)
    expect(panes.tabs[first].line).toBe(9)
    expect(panes.tabs[first].navigationNonce).toBeGreaterThan(beforeNavigation)
    expect(otherContext).not.toBe(first)
    expect(Object.values(panes.tabs).filter((tab) => tab.kind === 'file')).toHaveLength(2)
  })

  it('coalesces aliases after the backend returns one canonical path', () => {
    const first = openFileTab('./docs/lifecycle.md', { cwd: '/workspace/nano' })
    reconcileFileTab(first, { path: '/workspace/nano/docs/lifecycle.md', basename: 'lifecycle.md', editable: true })
    const alias = openFileTab('/workspace/nano/docs/lifecycle.md')
    const kept = reconcileFileTab(alias, { path: '/workspace/nano/docs/lifecycle.md', basename: 'lifecycle.md', editable: true })

    expect(kept).toBe(first)
    expect(Object.values(panes.tabs).filter((tab) => tab.kind === 'file')).toHaveLength(1)
    expect(panes.tabs[first]).toMatchObject({ path: '/workspace/nano/docs/lifecycle.md', title: 'lifecycle.md' })
  })

  it('does not open thread details for a file tab', () => {
    openFileTab('/tmp/report.md')
    expect(openDetailForActiveTab(1)).toBeNull()
    expect(panes.items).toHaveLength(1)
  })

})

describe('closeThreadTabs', () => {
  // The explicit-action counterpart to detachSessionTabs: an operator who
  // deliberately detaches or closes a thread wants the tab gone, not flipped
  // to read-only history (which is what a session merely vanishing from the
  // control plane — e.g. a nano restart — still does, via detachSessionTabs).
  it('closes an open tab bound to the session being closed', () => {
    const tabId = openThreadTab(tabTarget({ item: catalog(), session: session() }))
    expect(panes.tabs[tabId]).toBeDefined()

    closeThreadTabs({ sessionId: 'session-1' })

    expect(panes.tabs[tabId]).toBeUndefined()
    expect(panes.items[0].tabIds).not.toContain(tabId)
  })

  it('closes an already-detached tab by provider + nativeId, since its sessionId is already cleared', () => {
    const tabId = openThreadTab(tabTarget({ item: catalog(), session: session() }))
    detachSessionTabs('session-1')
    expect(panes.tabs[tabId].sessionId).toBe('')

    closeThreadTabs({ provider: 'codex', nativeId: 'native-1' })

    expect(panes.tabs[tabId]).toBeUndefined()
  })

  it('leaves other tabs untouched', () => {
    const target = openThreadTab(tabTarget({ item: catalog(), session: session() }))
    const other = openThreadTab(tabTarget({
      item: catalog({ id: 'nth_2', nativeId: 'native-2', title: 'Second' }),
      session: session({ id: 'session-2', threadId: 'native-2' }),
    }))

    closeThreadTabs({ sessionId: 'session-1' })

    expect(panes.tabs[target]).toBeUndefined()
    expect(panes.tabs[other]).toBeDefined()
  })
})
