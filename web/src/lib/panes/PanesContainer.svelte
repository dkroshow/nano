<script>
  import { panes, normalizeLayout } from '../../panes.svelte.js'
  import { viewport } from './viewport.svelte.js'
  import PaneDivider from './PaneDivider.svelte'
  import ThreadPane from './ThreadPane.svelte'

  const layout = $derived(
    normalizeLayout().map((column) => (
      column.length === 1 ? { paneId: column[0] } : { stack: column, key: column.join('-') }
    )),
  )

  // A phone has room for exactly one pane. Splitting is a desktop act — the
  // dividers, the drop zones and the cross-pane drag all want a pointer — so
  // the mobile workspace shows the focused pane and nothing else. Pane state is
  // in-memory, so a phone load already starts at one pane; this covers the
  // cases that create a second (a rotation from desktop width, a Details open)
  // by rendering the pane the operator is actually in rather than a 2px column.
  const activePane = $derived(
    panes.items.find((pane) => pane.id === panes.activePaneId) ?? panes.items[0] ?? null,
  )
</script>

<div class="thread-panes">
  {#if viewport.mobile}
    {#if activePane}<ThreadPane pane={activePane} />{/if}
  {:else}
    {#each layout as column, index (column.paneId ?? `stack-${column.key}`)}
      {#if index > 0}<PaneDivider dir="col" />{/if}
      {#if column.paneId != null}
        {@const pane = panes.items.find((candidate) => candidate.id === column.paneId)}
        {#if pane}<ThreadPane {pane} />{/if}
      {:else}
        <div class="thread-pane-stack">
          {#each column.stack as paneId, row (paneId)}
            {#if row > 0}<PaneDivider dir="row" />{/if}
            {@const pane = panes.items.find((candidate) => candidate.id === paneId)}
            {#if pane}<ThreadPane {pane} />{/if}
          {/each}
        </div>
      {/if}
    {/each}
  {/if}
</div>
