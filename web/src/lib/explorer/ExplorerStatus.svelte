<script>
  import { explorer, requestRefresh } from '../../explorer.svelte.js'

  // The catalog's health chip and its Refresh, lifted out of the Explorer's own
  // topbar so the expanded rail needs no extra band: they ride in the brand row,
  // which is present (and mostly empty) in both the compact and expanded states.
  const index = $derived(explorer.index)
  const scanning = $derived(index?.state === 'scanning' || index?.state === 'starting')
  const degraded = $derived(index?.state === 'degraded')
  const totalSources = $derived((index?.roots ?? []).reduce((n, r) => n + (r.sourceCount ?? 0), 0))
  const refreshBusy = $derived(explorer.refreshRequested || scanning)

  let rootsOpen = $state(false)
</script>

<div class="xstatus-slot">
  <!-- role=status: scanning→ready/degraded transitions and the source count
       announce politely; the label text changes with each state. -->
  <div class="xstatus" role="status">
    {#if scanning}
      <span class="status-dot idle" style="background: var(--orange)"></span>
      <span class="xstatus-label">scanning…</span>
    {:else if degraded}
      <button class="xstatus-btn" onclick={() => (rootsOpen = !rootsOpen)}>
        <span class="status-dot disconnected"></span>
        <span class="xstatus-label">degraded</span>
      </button>
    {:else if index}
      <span class="status-dot active"></span>
      <span class="xstatus-label">{totalSources} sources</span>
    {/if}
  </div>

  <button class="load-older xrefresh" disabled={refreshBusy} onclick={requestRefresh}>
    {refreshBusy ? 'Refreshing…' : 'Refresh'}
  </button>

  <!-- The per-root breakdown drops below the brand row rather than pushing the
       list down, so opening it never moves a thread. -->
  {#if degraded && rootsOpen}
    <div class="xroots xroots-pop">
      {#each index.roots ?? [] as r}
        <div class="xroot">
          <span class="t-provider {r.provider}">{r.provider}</span>
          <span class="xroot-state">{r.state}</span>
          <span class="xroot-count">{r.sourceCount ?? 0} sources</span>
          {#if r.error}<span class="xroot-error">{r.error}</span>{/if}
        </div>
      {/each}
    </div>
  {/if}
</div>
