<script>
  let { dir = 'col' } = $props()
  let divider

  function startResize(event) {
    event.preventDefault()
    const previous = divider.previousElementSibling
    const next = divider.nextElementSibling
    if (!previous || !next) return

    divider.classList.add('active')
    const horizontal = dir === 'col'
    const start = horizontal ? event.clientX : event.clientY
    const previousRect = previous.getBoundingClientRect()
    const nextRect = next.getBoundingClientRect()
    const previousSize = horizontal ? previousRect.width : previousRect.height
    const nextSize = horizontal ? nextRect.width : nextRect.height
    previous.style.flex = 'none'
    next.style.flex = 'none'

    function move(moveEvent) {
      const delta = (horizontal ? moveEvent.clientX : moveEvent.clientY) - start
      const before = Math.max(120, previousSize + delta)
      const after = Math.max(120, nextSize - delta)
      if (horizontal) {
        previous.style.width = `${before}px`
        next.style.width = `${after}px`
      } else {
        previous.style.height = `${before}px`
        next.style.height = `${after}px`
      }
    }

    function end() {
      divider.classList.remove('active')
      document.removeEventListener('pointermove', move)
      document.removeEventListener('pointerup', end)
      const parent = previous.parentElement
      if (!parent) return
      const total = horizontal ? parent.clientWidth : parent.clientHeight
      if (!total) return
      const previousPixels = horizontal
        ? previous.getBoundingClientRect().width
        : previous.getBoundingClientRect().height
      const nextPixels = horizontal
        ? next.getBoundingClientRect().width
        : next.getBoundingClientRect().height
      if (horizontal) {
        previous.style.width = `${(previousPixels / total * 100).toFixed(2)}%`
        next.style.width = `${(nextPixels / total * 100).toFixed(2)}%`
      } else {
        previous.style.height = `${(previousPixels / total * 100).toFixed(2)}%`
        next.style.height = `${(nextPixels / total * 100).toFixed(2)}%`
      }
    }

    document.addEventListener('pointermove', move)
    document.addEventListener('pointerup', end)
  }
</script>

<div
  class="thread-pane-divider {dir}"
  role="separator"
  aria-orientation={dir === 'col' ? 'vertical' : 'horizontal'}
  bind:this={divider}
  onpointerdown={startResize}
></div>
