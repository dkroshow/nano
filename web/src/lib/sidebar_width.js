export const DEFAULT_SIDEBAR_WIDTH = 320
export const MIN_SIDEBAR_WIDTH = 280
export const MAX_SIDEBAR_WIDTH = 520
export const SIDEBAR_WIDTH_KEY = 'nano:sidebar-width'

const MIN_WORKSPACE_WIDTH = 320

export function sidebarWidthLimit(viewportWidth = globalThis.innerWidth) {
  const available = Number(viewportWidth) - MIN_WORKSPACE_WIDTH
  if (!Number.isFinite(available)) return MAX_SIDEBAR_WIDTH
  return Math.max(MIN_SIDEBAR_WIDTH, Math.min(MAX_SIDEBAR_WIDTH, available))
}

export function clampSidebarWidth(value, viewportWidth = globalThis.innerWidth) {
  const parsed = value == null || value === '' ? DEFAULT_SIDEBAR_WIDTH : Number(value)
  const width = Number.isFinite(parsed) ? parsed : DEFAULT_SIDEBAR_WIDTH
  return Math.round(Math.max(MIN_SIDEBAR_WIDTH, Math.min(sidebarWidthLimit(viewportWidth), width)))
}

export function loadSidebarWidth(storage = globalThis.localStorage, viewportWidth = globalThis.innerWidth) {
  try {
    return clampSidebarWidth(storage?.getItem(SIDEBAR_WIDTH_KEY), viewportWidth)
  } catch {
    return clampSidebarWidth(DEFAULT_SIDEBAR_WIDTH, viewportWidth)
  }
}

export function saveSidebarWidth(width, storage = globalThis.localStorage) {
  try {
    storage?.setItem(SIDEBAR_WIDTH_KEY, String(width))
  } catch {
    // Storage is a convenience; resizing remains usable without it.
  }
}
