import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { app, closeThreadExplorer, openThread, openThreadExplorer, toggleThreadExplorer } from './app.svelte.js'
import { closeDetail, explorer } from './explorer.svelte.js'
import { __resetPanesForTest, activeTab } from './panes.svelte.js'

describe('Thread Explorer rail state', () => {
  beforeEach(() => {
    app.explorerOpen = false
    app.activeId = null
    explorer.dataset = []
    __resetPanesForTest()
    closeDetail()
  })

  afterEach(() => {
    closeDetail()
    vi.unstubAllGlobals()
  })

  it('opens and collapses without changing the active session', () => {
    app.activeId = 'session_1'

    openThreadExplorer()
    expect(app.explorerOpen).toBe(true)
    expect(app.activeId).toBe('session_1')

    toggleThreadExplorer()
    expect(app.explorerOpen).toBe(false)
    expect(app.activeId).toBe('session_1')
  })

  it('toggle opens a collapsed Explorer', () => {
    toggleThreadExplorer()
    expect(app.explorerOpen).toBe(true)
    closeThreadExplorer()
  })

  it('stages selected history in the workspace and collapses the rail', () => {
    explorer.dataset = [{
      id: 'thread_1',
      provider: 'codex',
      nativeId: 'native_1',
      title: 'History',
      metadata: {},
    }]
    app.activeId = 'session_1'
    app.explorerOpen = true

    openThread('thread_1')

    expect(activeTab()).toMatchObject({ catalogId: 'thread_1', nativeId: 'native_1' })
    expect(app.explorerOpen).toBe(false)
    expect(app.activeId).toBeNull()
  })
})
