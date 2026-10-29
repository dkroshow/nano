// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest'
import {
  DEFAULT_SIDEBAR_WIDTH,
  MAX_SIDEBAR_WIDTH,
  MIN_SIDEBAR_WIDTH,
  SIDEBAR_WIDTH_KEY,
  clampSidebarWidth,
  loadSidebarWidth,
  saveSidebarWidth,
} from './sidebar_width.js'

beforeEach(() => localStorage.clear())

describe('sidebar width preference', () => {
  it('defaults to the wider rail and clamps both stored and dragged values', () => {
    expect(loadSidebarWidth(localStorage, 1200)).toBe(DEFAULT_SIDEBAR_WIDTH)
    expect(clampSidebarWidth(100, 1200)).toBe(MIN_SIDEBAR_WIDTH)
    expect(clampSidebarWidth(900, 1200)).toBe(MAX_SIDEBAR_WIDTH)
    expect(clampSidebarWidth(500, 700)).toBe(380)
  })

  it('persists a valid pixel width', () => {
    saveSidebarWidth(416, localStorage)
    expect(localStorage.getItem(SIDEBAR_WIDTH_KEY)).toBe('416')
    expect(loadSidebarWidth(localStorage, 1200)).toBe(416)
  })
})
