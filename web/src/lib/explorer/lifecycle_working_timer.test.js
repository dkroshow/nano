// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render } from '@testing-library/svelte'
import WorkingTimer from './WorkingTimer.svelte'

afterEach(cleanup)

describe('parent work display', () => {
  it('shows no work clock or work label for an idle parent with active children', () => {
    const { container } = render(WorkingTimer, {
      item: {
        subagentActivity: { active: 1 },
        // A stale pre-cutover payload must not restore the retired work axis.
        runtime: { attached: true, status: 'ready', turnActive: false, delegating: true },
      },
    })
    expect(container.textContent).toBe('')
  })
})

for (const status of ['starting', 'reconnecting', 'reconnect_parked', 'reconnect_failed', 'detached', 'exited']) {
  it(`renders no stale work or waiting label while ${status}`, () => {
    const { container } = render(WorkingTimer, {
      item: { runtime: { attached: true, status, turnActive: true, waiting: true, turnStartedAtMs: 1000 } },
    })
    expect(container.textContent).toBe('')
  })
}
