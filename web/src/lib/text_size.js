
export const TEXT_SIZES = [
  { label: 'A--', text: '11px', code: '9.5px', mono: '9.5px', padV: '6px', padH: '8px', gap: '3px', radius: '10px', lineH: '1.4' },
  { label: 'A-', text: '13px', code: '11px', mono: '11px', padV: '8px', padH: '12px', gap: '6px', radius: '14px', lineH: '1.55' },
  { label: 'A', text: '15px', code: '13px', mono: '13px', padV: '16px', padH: '18px', gap: '12px', radius: '18px', lineH: '1.7' },
  { label: 'A+', text: '17px', code: '15px', mono: '15px', padV: '18px', padH: '20px', gap: '14px', radius: '20px', lineH: '1.8' },
]

export const DEFAULT_TEXT_SIZE = 2 // 'A' — matches the :root defaults

const STORAGE_KEY = 'nano-text-size'

export function savedTextSizeIdx() {
  const raw = localStorage.getItem(STORAGE_KEY)
  const idx = raw === null ? NaN : parseInt(raw, 10)
  return idx >= 0 && idx < TEXT_SIZES.length ? idx : DEFAULT_TEXT_SIZE
}

export function applyTextSize(idx, { persist = true } = {}) {
  if (!(idx >= 0 && idx < TEXT_SIZES.length)) idx = DEFAULT_TEXT_SIZE
  const s = TEXT_SIZES[idx]
  const r = document.documentElement.style
  r.setProperty('--text-size', s.text)
  r.setProperty('--code-size', s.code)
  r.setProperty('--mono-size', s.mono)
  r.setProperty('--turn-pad-v', s.padV)
  r.setProperty('--turn-pad-h', s.padH)
  r.setProperty('--turn-gap', s.gap)
  r.setProperty('--turn-radius', s.radius)
  r.setProperty('--line-h', s.lineH)
  if (persist) localStorage.setItem(STORAGE_KEY, String(idx))
  // The warm stream sizes its rows to the window in message lines; a larger
  // text size means fewer lines fit.
  globalThis.dispatchEvent?.(new Event('nano-text-size'))
  return idx
}

export function applySavedTextSize() {
  return applyTextSize(savedTextSizeIdx(), { persist: false })
}
