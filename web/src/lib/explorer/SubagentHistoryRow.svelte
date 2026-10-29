<script>
  import { explorer, loadParentHistory } from '../../explorer.svelte.js'
  import { toggleHistoricalWorkerGroup } from './worker_disclosure.svelte.js'

  let { control, label, lazy = false } = $props()
  const parent = $derived(control.parent)
  const historyLoad = $derived(lazy ? explorer.historyScopes?.[parent.id] : null)
  const indent = $derived(`${18 + (control.depth ?? 1) * 14}px`)

  async function toggle(event) {
    event.stopPropagation()
    if (lazy && historyLoad?.state === 'error') {
      await loadParentHistory(parent)
      return
    }
    if (control.expanded) {
      toggleHistoricalWorkerGroup(parent)
      return
    }
    toggleHistoricalWorkerGroup(parent)
    if (lazy && parent.sidebar && historyLoad?.state !== 'ready') await loadParentHistory(parent)
  }
</script>

<button
  class="worker-history-toggle worker-history-row"
  style:--history-indent={indent}
  type="button"
  aria-expanded={control.expanded}
  aria-busy={historyLoad?.state === 'loading'}
  aria-label="{historyLoad?.state === 'error' ? 'Retry' : control.expanded ? 'Hide' : 'Show'} historical subagents of {label}"
  onclick={toggle}
>{historyLoad?.state === 'loading'
  ? `Loading historical (${control.count})`
  : historyLoad?.state === 'error'
    ? `Retry historical (${control.count})`
    : `${control.expanded ? 'Hide' : 'Show'} historical (${control.count})`}</button>
