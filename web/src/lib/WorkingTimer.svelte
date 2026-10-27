<script>
  import { nextTickDelayMs, tickingAnchor, workingIndicator } from './explorer_format.js'

  // The live working elapsed for one catalog row, rendered by both the compact
  // rail and the expanded Explorer from the same projection.
  //
  // The tick is the component's, not a global interval (D6): each row schedules
  // only the wake it needs, and a row that is not working — or is waiting, or
  // has no anchor — schedules nothing at all. The server owns the anchor and
  // never ships an elapsed (D4), so this is the only clock in the picture.
  let { item } = $props()

  const anchor = $derived(tickingAnchor(item))
  let now = $state(Date.now())
  const indicator = $derived(workingIndicator(item, now))

  $effect(() => {
    if (!anchor) return
    let timer
    const tick = () => {
      const at = Date.now()
      now = at
      timer = setTimeout(tick, nextTickDelayMs(at - anchor))
    }
    tick()
    return () => clearTimeout(timer)
  })
</script>

{#if indicator?.text}
  <span class="working-elapsed {indicator.state}" title={indicator.title}>{indicator.text}</span>
{/if}
