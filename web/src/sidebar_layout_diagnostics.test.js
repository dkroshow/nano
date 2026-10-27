// @vitest-environment jsdom

import { beforeEach, describe, expect, it } from 'vitest'
import { __resetBrowserDiagnosticsForTest } from './browser_diagnostics.js'
import {
  __resetSidebarLayoutDiagnosticsForTest,
  recordSidebarLayout,
} from './sidebar_layout_diagnostics.js'

function diagnosticEvents() {
  return JSON.parse(sessionStorage.getItem('nano.browser-diagnostics.v1') || '{"events":[]}').events
}

function mountedSidebar() {
  document.body.innerHTML = `
    <div class="side-compact">
      <div class="thread-list">
        <div data-sidebar-row data-sidebar-key="sidebar:one" data-sidebar-id="one"></div>
        <div data-sidebar-row data-sidebar-key="sidebar:two" data-sidebar-id="two"></div>
      </div>
    </div>`
  const root = document.querySelector('.side-compact')
  const scroller = root.querySelector('.thread-list')
  const tops = new Map([['one', 0], ['two', 40]])
  scroller.getBoundingClientRect = () => ({ top: 100, bottom: 300, height: 200 })
  Object.defineProperties(scroller, {
    scrollTop: { value: 0, writable: true },
    scrollHeight: { value: 400, configurable: true },
    clientHeight: { value: 200, configurable: true },
  })
  for (const row of root.querySelectorAll('[data-sidebar-row]')) {
    row.getBoundingClientRect = () => {
      const top = 100 + tops.get(row.dataset.sidebarId)
      return { top, bottom: top + 32, height: 32 }
    }
  }
  return { root, scroller, tops }
}

describe('sidebar layout diagnostics', () => {
  beforeEach(() => {
    __resetBrowserDiagnosticsForTest()
    __resetSidebarLayoutDiagnosticsForTest()
    sessionStorage.clear()
  })

  it('records actual row movement and the ordering facts that caused it', async () => {
    const { root, tops } = mountedSidebar()
    const rows = [
      { key: 'one', cls: 'nano', kind: 'attached', working: false },
      { key: 'two', cls: 'nano', kind: 'attached', working: true },
    ]
    recordSidebarLayout(root, rows)

    tops.set('one', 40)
    tops.set('two', 0)
    root.querySelector('.thread-list').prepend(root.querySelector('[data-sidebar-id="two"]'))
    recordSidebarLayout(root, rows)
    await new Promise((resolve) => setTimeout(resolve, 0))

    const events = diagnosticEvents()
    const layouts = events.filter((event) => event.kind === 'sidebar.layout')
    expect(layouts).toHaveLength(2)
    expect(layouts[1]).toMatchObject({
      itemCount: 2,
      movementCount: 2,
      reorderCount: 2,
      maxMovementPx: 40,
      scrollTop: 0,
      scrollHeight: 400,
      viewportHeight: 200,
    })
    expect(events.filter((event) => event.kind === 'sidebar.row.move')).toEqual(expect.arrayContaining([
      expect.objectContaining({
        threadId: 'one', rowIndex: 1, previousRowIndex: 0,
        topPx: 40, previousTopPx: 0, movementPx: 40,
        code: 'nano-idle', turnActive: false,
      }),
      expect.objectContaining({
        threadId: 'two', rowIndex: 0, previousRowIndex: 1,
        topPx: 0, previousTopPx: 40, movementPx: 40,
        code: 'nano-working', turnActive: true,
      }),
    ]))
  })
})
