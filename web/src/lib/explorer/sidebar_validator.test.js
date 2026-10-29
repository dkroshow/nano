// @vitest-environment jsdom

import { describe, expect, it } from 'vitest'
import { actualSidebarRows, compareSidebarRows, expectedCompactSidebarRows } from './sidebar_validator.js'

describe('sidebar render validator', () => {
  it('compares order, active selection, status, grouping, depth, and mounted title content', () => {
    const rows = [{
      key: 'catalog:one', kind: 'catalog', cls: 'nano', treeDepth: 1,
      indicator: { state: 'working' }, assignment: { relatedHostedThreadId: 'parent-1' },
    }]
    const expected = expectedCompactSidebarRows(rows, () => true)
    document.body.innerHTML = `<div id="root"><div data-sidebar-row data-sidebar-key="sidebar:catalog:one" data-sidebar-id="catalog:one" data-sidebar-type="catalog" data-sidebar-status="working" data-sidebar-active="true" data-sidebar-group="parent-1" data-sidebar-depth="1"><span data-sidebar-content>Thread</span></div></div>`

    expect(compareSidebarRows(expected, actualSidebarRows(document.querySelector('#root'))).ok).toBe(true)
  })

  it('reports a mounted state mismatch without retaining title text', () => {
    const expected = expectedCompactSidebarRows([{ key: 'one', kind: 'catalog', cls: 'nano' }])
    const actual = [{ key: 'sidebar:one', source: 'sidebar', id: 'one', type: 'catalog', hasContent: true, active: true, status: 'idle', group: '', depth: 0 }]
    const report = compareSidebarRows(expected, actual)
    expect(report.ok).toBe(false)
    expect(report.stateMismatch.map((row) => row.key)).toEqual(['sidebar:one'])
    expect(JSON.stringify(report)).not.toContain('Thread')
  })
})
