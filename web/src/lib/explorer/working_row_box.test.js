// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { render, cleanup } from '@testing-library/svelte'
import CompactThreadRow from './CompactThreadRow.svelte'
import ThreadRowItem from './ThreadRowItem.svelte'
import { projectSidebar } from './sidebar_projection.js'
import { applyView } from '../../explorer_local.js'

function catalogRow(runtime) {
  return {
    id: 'nth_1',
    provider: 'codex',
    nativeId: 'native-1',
    title: 'Box row',
    metadata: {},
    runtime,
  }
}

function renderRailRow(runtime) {
  const [row] = projectSidebar([catalogRow(runtime)], [])
  return render(CompactThreadRow, { props: { row } })
}

function renderExplorerRow(runtime) {
  return render(ThreadRowItem, { props: { item: catalogRow(runtime) } })
}

afterEach(cleanup)

const WORKING = { status: 'ready', attached: true, sessionId: 's1', turnActive: true, threadStatus: 'working', turnStartedAtMs: 1_800_000_000_000 }
const WAITING = { status: 'ready', attached: true, sessionId: 's1', turnActive: true, threadStatus: 'needs_you', waiting: true }
const IDLE = { status: 'ready', attached: true, sessionId: 's1', turnActive: false, threadStatus: 'idle' }

describe('the rail row working box', () => {
  it('boxes a row whose turn is in flight', () => {
    const { container } = renderRailRow(WORKING)
    expect(container.querySelector('.thread-row.working')).not.toBeNull()
  })

  it('does not box a waiting row — waiting is not working (D15)', () => {
    const { container } = renderRailRow(WAITING)
    expect(container.querySelector('.thread-row.working')).toBeNull()
  })

  it('does not box an idle attached row', () => {
    const { container } = renderRailRow(IDLE)
    expect(container.querySelector('.thread-row.working')).toBeNull()
  })

  // Compacting can take minutes with nothing else moving; the row says so
  // for exactly as long as the provider's own signals do.
  it('marks a working row while its provider compacts, and clears the mark when it ends', async () => {
    const { container, rerender } = renderRailRow({ ...WORKING, compactingSinceMs: 1_800_000_030_000 })
    expect(container.querySelector('.t-compacting')?.textContent).toBe('compacting')
    const [row] = projectSidebar([catalogRow(WORKING)], [])
    await rerender({ row })
    expect(container.querySelector('.t-compacting')).toBeNull()
    expect(container.querySelector('.thread-row.working')).not.toBeNull()
  })
})

describe('the Explorer row working box', () => {
  it('boxes a row whose turn is in flight', () => {
    const { container } = renderExplorerRow(WORKING)
    expect(container.querySelector('.xrow.working')).not.toBeNull()
  })

  it('does not box a waiting or idle row', () => {
    expect(renderExplorerRow(WAITING).container.querySelector('.xrow.working')).toBeNull()
    cleanup()
    expect(renderExplorerRow(IDLE).container.querySelector('.xrow.working')).toBeNull()
  })
})

describe('a running child does not box its parent', () => {
  const parent = {
    ...catalogRow({ status: 'ready', attached: true, sessionId: 'parent-session', turnActive: false, threadStatus: 'idle' }),
    id: 'nth_parent', nativeId: 'parent-native', title: 'Parent',
  }
  const child = {
    ...catalogRow({ status: 'ready', attached: true, sessionId: 'child-session', turnActive: true, threadStatus: 'working', turnStartedAtMs: 1_800_000_000_000 }),
    id: 'nth_child', nativeId: 'child-native', title: 'Child',
    invokerNativeThreadId: 'parent-native', invokerProvider: 'codex',
  }

  it('boxes only the child in the compact rail', () => {
    const arranged = applyView([child, parent], {
      projects: null, providers: null, models: null, efforts: null,
      createdRange: null, updatedRange: null, sort: 'liveness-updated', dir: 'desc',
    })
    const rows = projectSidebar(arranged, [])
    const parentView = render(CompactThreadRow, { props: { row: rows[0] } })
    const childView = render(CompactThreadRow, { props: { row: rows[1] } })

    expect(parentView.container.querySelector('.thread-row.working')).toBeNull()
    expect(childView.container.querySelector('.thread-row.working')).not.toBeNull()
  })

  it('boxes only the child in Thread Explorer', () => {
    const arranged = applyView([child, parent], {
      projects: null, providers: null, models: null, efforts: null,
      createdRange: null, updatedRange: null, sort: 'liveness-updated', dir: 'desc',
    })
    const parentView = render(ThreadRowItem, { props: { item: arranged[0] } })
    const childView = render(ThreadRowItem, { props: { item: arranged[1] } })

    expect(parentView.container.querySelector('.xrow.working')).toBeNull()
    expect(childView.container.querySelector('.xrow.working')).not.toBeNull()
  })
})
