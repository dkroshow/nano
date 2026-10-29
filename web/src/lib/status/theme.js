
export const THEME_STORAGE_KEY = 'nano-theme'
export const DARK_THEME_COLOR = '#191a1b'
export const LIGHT_THEME_COLOR = '#f5f5f5'

export function savedTheme() {
  try {
    return localStorage.getItem(THEME_STORAGE_KEY) === 'light' ? 'light' : 'dark'
  } catch {
    return 'dark'
  }
}

export function applyTheme(next, { persist = true } = {}) {
  const theme = next === 'light' ? 'light' : 'dark'
  const isLight = theme === 'light'
  document.documentElement.classList.toggle('light', isLight)
  const meta = document.querySelector('meta[name="theme-color"]')
  if (meta) meta.content = isLight ? LIGHT_THEME_COLOR : DARK_THEME_COLOR
  if (persist) {
    try { localStorage.setItem(THEME_STORAGE_KEY, theme) } catch { /* preference remains page-local */ }
  }
  return theme
}

export function applySavedTheme() {
  return applyTheme(savedTheme(), { persist: false })
}
