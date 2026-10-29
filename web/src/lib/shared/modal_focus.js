// Focus containment for blocking dialogs. Dependency-free and exported so
// other modals (e.g. the Thread Explorer detail card) can adopt it later.

const FOCUSABLE = 'a[href], button, input, select, textarea, [tabindex]'

function focusables(container) {
  return [...container.querySelectorAll(FOCUSABLE)].filter(
    (el) => !el.disabled && el.tabIndex >= 0 && el.offsetParent !== null
  )
}

// Keydown handler for Tab/Shift+Tab: keeps focus cycling inside `container`,
// wrapping at the edges and pulling focus back in if it escaped.
export function trapTab(container, e) {
  if (e.key !== 'Tab' || !container) return
  const els = focusables(container)
  if (!els.length) return
  const first = els[0]
  const last = els[els.length - 1]
  const active = document.activeElement
  if (!container.contains(active)) {
    e.preventDefault()
    ;(e.shiftKey ? last : first).focus()
  } else if (e.shiftKey && active === first) {
    e.preventDefault()
    last.focus()
  } else if (!e.shiftKey && active === last) {
    e.preventDefault()
    first.focus()
  }
}
