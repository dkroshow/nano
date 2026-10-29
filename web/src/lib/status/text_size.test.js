import { describe, it, expect, beforeEach, vi } from 'vitest'
import { TEXT_SIZES, DEFAULT_TEXT_SIZE, applyTextSize, savedTextSizeIdx, applySavedTextSize } from './text_size.js'

let stored
let vars

beforeEach(() => {
  stored = new Map()
  vars = new Map()
  vi.stubGlobal('localStorage', {
    getItem: (k) => (stored.has(k) ? stored.get(k) : null),
    setItem: (k, v) => stored.set(k, String(v)),
  })
  vi.stubGlobal('document', {
    documentElement: { style: { setProperty: (k, v) => vars.set(k, v) } },
  })
})

describe('text size tiers', () => {
  it('matches the standard proportions table (labels and default)', () => {
    expect(TEXT_SIZES.map((s) => s.label)).toEqual(['A--', 'A-', 'A', 'A+'])
    expect(TEXT_SIZES[DEFAULT_TEXT_SIZE]).toMatchObject({ text: '15px', padV: '16px', padH: '18px', lineH: '1.7' })
  })

  it('applies every variable for a tier and persists the index', () => {
    applyTextSize(3)
    expect(vars.get('--text-size')).toBe('17px')
    expect(vars.get('--mono-size')).toBe('15px')
    expect(vars.get('--turn-pad-v')).toBe('18px')
    expect(vars.get('--turn-gap')).toBe('14px')
    expect(vars.get('--line-h')).toBe('1.8')
    expect(stored.get('nano-text-size')).toBe('3')
  })

  it('clamps invalid indices to the default and does not persist when asked', () => {
    expect(applyTextSize(99, { persist: false })).toBe(DEFAULT_TEXT_SIZE)
    expect(vars.get('--text-size')).toBe('15px')
    expect(stored.has('nano-text-size')).toBe(false)
  })

  it('savedTextSizeIdx falls back to default on absent or garbage values', () => {
    expect(savedTextSizeIdx()).toBe(DEFAULT_TEXT_SIZE)
    stored.set('nano-text-size', 'junk')
    expect(savedTextSizeIdx()).toBe(DEFAULT_TEXT_SIZE)
    stored.set('nano-text-size', '1')
    expect(savedTextSizeIdx()).toBe(1)
  })

  it('applySavedTextSize restores the persisted tier without rewriting it', () => {
    stored.set('nano-text-size', '0')
    expect(applySavedTextSize()).toBe(0)
    expect(vars.get('--text-size')).toBe('11px')
    expect(stored.get('nano-text-size')).toBe('0')
  })
})
