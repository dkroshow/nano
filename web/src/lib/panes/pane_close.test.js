// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render } from '@testing-library/svelte'
import ThreadPane from './ThreadPane.svelte'
import { __resetPanesForTest, addPane, panes } from '../../panes.svelte.js'

beforeEach(() => {
  __resetPanesForTest()
  addPane()
  addPane()
})

afterEach(() => cleanup())

describe('pane close button', () => {
  it('removes the far-right pane from a three-pane workspace', async () => {
    const farRight = panes.items.at(-1)
    const { getByRole } = render(ThreadPane, { pane: farRight })

    await fireEvent.click(getByRole('button', { name: 'Close pane' }))

    expect(panes.items).toHaveLength(2)
    expect(panes.items.some((pane) => pane.id === farRight.id)).toBe(false)
    expect(panes.layout.flat()).not.toContain(farRight.id)
  })
})
