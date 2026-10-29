// @vitest-environment jsdom
//
// The live working elapsed, as the operator actually sees it. These are
// component tests rather than state tests because every failure mode this work
// is guarding against is a RENDER decision: a row that ticks when it should be
// still, a row that shows a fabricated start, a parked dialog that keeps
// counting as if the agent were working.
import { describe, it, expect, afterEach, vi } from 'vitest'
import { render, cleanup } from '@testing-library/svelte'
import { tick } from 'svelte'
import CompactThreadRow from './CompactThreadRow.svelte'
import { projectSidebar } from './sidebar_projection.js'

const NOW = 1_800_000_000_000

function catalogRow(runtime) {
  return {
    id: 'nth_1',
    provider: 'codex',
    nativeId: 'native-1',
    title: 'Timer row',
    metadata: {},
    runtime,
  }
}

function renderRow(runtime) {
  const [row] = projectSidebar([catalogRow(runtime)], [])
  return render(CompactThreadRow, { props: { row } })
}

afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe('the rail row while a turn is in flight', () => {
  it('ticks the elapsed from the server anchor', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    const { container } = renderRow({
      status: 'ready', attached: true,
      sessionId: 's1',
      turnActive: true, threadStatus: 'working',
      turnStartedAtMs: NOW - 42_000,
    })
    expect(container.querySelector('.working-elapsed')?.textContent).toBe('42s')

    await vi.advanceTimersByTimeAsync(3_000)
    await tick()
    expect(container.querySelector('.working-elapsed')?.textContent).toBe('45s')
  })

  it('shows the tenth of a second in the first ten seconds', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    const { container } = renderRow({
      status: 'ready', attached: true,
      sessionId: 's1',
      turnActive: true, threadStatus: 'working',
      turnStartedAtMs: NOW - 2_000,
    })
    expect(container.querySelector('.working-elapsed')?.textContent).toBe('2.0s')

    await vi.advanceTimersByTimeAsync(500)
    await tick()
    expect(container.querySelector('.working-elapsed')?.textContent).toBe('2.5s')
  })

  // D2/D8. A re-attach that recovered no evidence must render the working dot
  // and NOTHING beside it — not "0.0s", which is a claim nano cannot make.
  it('renders the working dot with no timer when there is no anchor', () => {
    const { container } = renderRow({ status: 'ready', attached: true, sessionId: 's1', turnActive: true, threadStatus: 'working' })
    expect(container.querySelector('.t-dot')?.className).toContain('working')
    expect(container.querySelector('.working-elapsed')).toBeNull()
  })

  // D15. A parked dialog means the agent stopped; a clock still counting up
  // says the opposite.
  it('shows waiting instead of a ticking timer when a dialog is parked', () => {
    const { container } = renderRow({
      status: 'ready', attached: true,
      sessionId: 's1',
      turnActive: true, threadStatus: 'needs_you',
      turnStartedAtMs: NOW - 42_000,
      waiting: true,
    })
    expect(container.querySelector('.t-dot')?.className).toContain('waiting')
    expect(container.textContent).toContain('waiting')
    expect(container.querySelector('.working-elapsed')).toBeNull()
  })

  // An idle attached row keeps its last-updated age: nothing is running, so
  // there is nothing to time, and the timer must not have displaced the age
  // slot permanently.
  it('leaves an idle row showing its age', () => {
    const [row] = projectSidebar(
      [{ ...catalogRow({ status: 'ready', attached: true, sessionId: 's1' }), updatedAt: new Date(Date.now() - 120_000).toISOString() }],
      [],
    )
    const { container } = render(CompactThreadRow, { props: { row } })
    expect(container.querySelector('.working-elapsed')).toBeNull()
    expect(container.querySelector('.t-age')).not.toBeNull()
  })

  // A working row must not be paying for a wake it does not need, and an idle
  // one must schedule nothing at all (D6).
  it('schedules no timer for a row that is not working', () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    renderRow({ status: 'ready', attached: true, sessionId: 's1' })
    expect(vi.getTimerCount()).toBe(0)
  })
})

describe('the pending overlay', () => {
  // The spawn-moment row the catalog has not seen yet is built from a session
  // summary, and it has to carry the anchor too — otherwise the timer blinks
  // out for the second or two before the catalog agrees the thread exists.
  it('carries the anchor and the waiting flag from the session summary', () => {
    const [row] = projectSidebar([], [
      {
        id: 's1',
        provider: 'claude',
        threadId: 'native-9',
        status: 'ready',
        turnActive: true, threadStatus: 'working',
        turnStartedAtMs: NOW - 5_000,
      },
    ])
    expect(row.kind).toBe('pending')
    expect(row.runtime.turnStartedAtMs).toBe(NOW - 5_000)

    const { container } = render(CompactThreadRow, { props: { row } })
    expect(container.querySelector('.working-elapsed')?.textContent).toBeTruthy()
  })
})
